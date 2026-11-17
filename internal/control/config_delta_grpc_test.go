package control

import (
	"context"
	"net"
	"testing"
	"time"

	pb "github.com/nekopass/nekopass/internal/protocol"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

func TestConfigDeltaRealGRPC(t *testing.T) {
	p := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	server := New(p)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	pb.RegisterControlServer(g, &StreamServer{Server: server})
	go g.Serve(l)
	conn, err := grpc.NewClient(l.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); g.Stop(); l.Close() })
	client := pb.NewControlClient(conn)
	user, _ := testUser(t, p, 1000000, 10, false)
	token, instance := Secret(), Secret()
	var node, rule int64
	if err := p.QueryRow(ctx, `INSERT INTO nodes(name,token_hash,token) VALUES($1,$2,$3) RETURNING id`, Secret(), Hash(token), token).Scan(&node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = p.Exec(context.Background(), "DELETE FROM nodes WHERE id=$1", node) })
	testAuthorize(t, p, user, node)
	if err := p.QueryRow(ctx, `INSERT INTO rules(user_id,node_id,listen_port,target_host,target_port) VALUES($1,$2,32124,'example.com',443) RETURNING id`, user, node).Scan(&rule); err != nil {
		t.Fatal(err)
	}
	open := func() (pb.Control_ConnectClient, context.CancelFunc) {
		t.Helper()
		c, stop := context.WithCancel(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token))
		stream, err := client.Connect(c)
		if err != nil {
			stop()
			t.Fatal(err)
		}
		return stream, stop
	}
	stream, stop := open()
	defer stop()
	report := &pb.AgentMessage{InstanceId: instance, ProtocolVersion: pb.CurrentVersion, SupportsConfigDelta: true, RequestUsers: []int64{user}}
	exchange := func(stream pb.Control_ConnectClient) *pb.ControlMessage {
		t.Helper()
		if err := stream.Send(report); err != nil {
			t.Fatal(err)
		}
		out, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := exchange(stream)
	if first.ConfigUnchanged || first.ConfigToken == "" || first.Node == nil || len(first.Rules) != 1 || len(first.Users) != 1 || first.Users[0].Issued == 0 {
		t.Fatalf("invalid initial full message: %v", first)
	}
	report.RequestUsers = nil
	report.AppliedConfigToken = first.ConfigToken
	unchanged := exchange(stream)
	if !unchanged.ConfigUnchanged || unchanged.Node != nil || len(unchanged.Rules) != 0 || len(unchanged.Users) != 1 {
		t.Fatalf("invalid unchanged response: %v", unchanged)
	}
	report.AppliedConfigToken = Secret()
	wrongToken := exchange(stream)
	if wrongToken.ConfigUnchanged || wrongToken.Node == nil || wrongToken.ConfigToken != first.ConfigToken {
		t.Fatalf("unknown token did not receive a full configuration: %v", wrongToken)
	}
	report.AppliedConfigToken = wrongToken.ConfigToken
	// Diagnostics and the usage ACK must still travel on a static-free frame.
	taskID := Secret()
	results := make(chan diagnosticResult, 1)
	server.diagnostics.mu.Lock()
	server.diagnostics.tasks = map[string]*diagnosticPending{taskID: {nodeID: node, task: &pb.DiagnosticTask{TaskId: taskID, RuleId: rule}, result: results}}
	server.diagnostics.mu.Unlock()
	report.Usage = []*pb.Usage{{UserId: user, QuotaEpoch: first.Users[0].QuotaEpoch, Spent: 1, Traffic: 1, ChargedBytes: 1}}
	withTask := exchange(stream)
	if !withTask.ConfigUnchanged || len(withTask.AcknowledgedUsage) != 1 || len(withTask.DiagnosticTasks) != 1 || withTask.DiagnosticTasks[0].TaskId != taskID {
		t.Fatalf("delta lost usage ACK or diagnostic: %v", withTask)
	}
	report.DiagnosticResults = []*pb.DiagnosticResult{{TaskId: taskID, Status: "reachable", LatencyMs: 7}}
	resultFrame := exchange(stream)
	if !resultFrame.ConfigUnchanged || len(resultFrame.AcknowledgedUsage) != 1 {
		t.Fatalf("unexpected diagnostic-result response: %v", resultFrame)
	}
	select {
	case result := <-results:
		if result.status != "reachable" || result.latency != 7 {
			t.Fatalf("diagnostic result: %+v", result)
		}
	default:
		t.Fatal("diagnostic result was not delivered")
	}
	report.DiagnosticResults = nil
	if _, err := p.Exec(ctx, "UPDATE grants SET issued=issued-1 WHERE node_id=$1 AND user_id=$2 AND quota_epoch=$3", node, user, first.Users[0].QuotaEpoch); err != nil {
		t.Fatal(err)
	}
	quotaChanged := exchange(stream)
	if !quotaChanged.ConfigUnchanged || len(quotaChanged.Users) != 1 || quotaChanged.Users[0].Issued != first.Users[0].Issued-1 || len(quotaChanged.AcknowledgedUsage) != 1 {
		t.Fatalf("delta lost a changed quota policy or usage ACK: %v", quotaChanged)
	}
	if _, err := p.Exec(ctx, "UPDATE rules SET target_host='changed.example' WHERE id=$1", rule); err != nil {
		t.Fatal(err)
	}
	changed := exchange(stream)
	if changed.ConfigUnchanged || changed.ConfigToken == first.ConfigToken || changed.Node == nil || len(changed.Rules) != 1 || changed.Rules[0].TargetHost != "changed.example" {
		t.Fatalf("changed rule not delivered: %v", changed)
	}
	report.AppliedConfigToken = changed.ConfigToken
	if _, err := p.Exec(ctx, "UPDATE users SET resource_expires_at=now()+interval '1 second' WHERE id=$1", user); err != nil {
		t.Fatal(err)
	}
	_ = exchange(stream)
	time.Sleep(1500 * time.Millisecond)
	expired := exchange(stream)
	if expired.ConfigUnchanged || len(expired.Rules) != 0 || len(expired.Users) != 0 {
		t.Fatalf("expired authorization still delivered: %v", expired)
	}
	stop()
	// Wait until Connect has released the stream's per-node advisory lock.
	deadline := time.Now().Add(5 * time.Second)
	for {
		server.nodeLocks.mu.Lock()
		_, held := server.nodeLocks.owners[node]
		server.nodeLocks.mu.Unlock()
		if !held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stream lock was not released")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stream2, stop2 := open()
	defer stop2()
	report.AppliedConfigToken = expired.ConfigToken // A stale client token cannot skip the reconnect's first full config.
	reconnected := exchange(stream2)
	if reconnected.ConfigUnchanged || reconnected.Node == nil {
		t.Fatalf("reconnect did not receive a full configuration: %v", reconnected)
	}
	report.SupportsConfigDelta = false
	legacy := exchange(stream2)
	if legacy.ConfigUnchanged || legacy.ConfigToken != "" || legacy.Node == nil {
		t.Fatalf("legacy client did not receive a full configuration: %v", legacy)
	}
}
