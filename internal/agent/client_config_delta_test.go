package agent

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/nekopass/nekopass/internal/protocol"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

func TestStreamConfigDeltaMaterialize(t *testing.T) {
	cache := &streamConfigCache{}
	full := &pb.ControlMessage{ConfigToken: "first", Revision: 1, Node: &pb.NodeConfig{NodeId: 9}, Rules: []*pb.Rule{{Id: 3}}, TunnelLinks: []*pb.TunnelLink{{IngressNodeId: 5}}, EgressRules: []*pb.EgressRule{{RuleId: 7}}, Users: []*pb.UserPolicy{{Id: 2, Issued: 100}}}
	message, token, static, err := cache.materialize(full)
	if err != nil || token != "first" || static == nil || message.ConfigToken != "" {
		t.Fatalf("full: %v %q %#v", err, token, static)
	}
	// The caller must only commit the cache after Apply; without it a delta is rejected.
	if _, _, _, err := cache.materialize(&pb.ControlMessage{ConfigToken: "first", ConfigUnchanged: true}); err == nil {
		t.Fatal("accepted delta before full configuration was applied")
	}
	cache.token, cache.static = token, static
	message.Node.NodeId = 99 // the cache cannot be mutated through a prior message
	wire := &pb.ControlMessage{ConfigToken: "first", ConfigUnchanged: true, Revision: 2, Users: []*pb.UserPolicy{{Id: 2, Issued: 200}}, AcknowledgedUsage: []*pb.Usage{{UserId: 2, Spent: 5}}, StateRestore: &pb.StateRestore{InstanceId: "new"}, Update: &pb.UpdateRequest{Generation: 2}, DiagnosticTasks: []*pb.DiagnosticTask{{TaskId: "test"}}, ControlEndpoint: "new.example:443"}
	encoded, err := proto.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var decoded pb.ControlMessage
	if err := proto.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	got, token, static, err := cache.materialize(&decoded)
	if err != nil || token != "first" || static != cache.static {
		t.Fatalf("delta: %v", err)
	}
	if got.Node.GetNodeId() != 9 || len(got.Rules) != 1 || len(got.TunnelLinks) != 1 || len(got.EgressRules) != 1 {
		t.Fatalf("static fields not restored: %v", got)
	}
	if got.Revision != 2 || got.Users[0].Issued != 200 || got.AcknowledgedUsage[0].Spent != 5 || got.StateRestore.GetInstanceId() != "new" || got.Update.GetGeneration() != 2 || got.DiagnosticTasks[0].TaskId != "test" || got.ControlEndpoint != "new.example:443" {
		t.Fatalf("dynamic fields lost: %v", got)
	}
	if got.ConfigUnchanged || got.ConfigToken != "" {
		t.Fatal("stream metadata leaked into persisted configuration")
	}
	got.Rules[0].Id = 88
	next, _, _, err := cache.materialize(&pb.ControlMessage{ConfigToken: "first", ConfigUnchanged: true})
	if err != nil || next.Rules[0].Id != 3 {
		t.Fatal("cache mutated via delta", err)
	}
}

type deltaTestServer struct {
	pb.UnimplementedControlServer
	reports   chan *pb.AgentMessage
	responses chan *pb.ControlMessage
}

func (s *deltaTestServer) Connect(stream grpc.BidiStreamingServer[pb.AgentMessage, pb.ControlMessage]) error {
	for {
		report, err := stream.Recv()
		if err != nil {
			return err
		}
		select {
		case s.reports <- report:
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
		select {
		case response := <-s.responses:
			if err := stream.Send(response); err != nil {
				return err
			}
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}

func TestStreamConfigDeltaRealSessionRetriesAndAppliesPolicy(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := int32(busy.Addr().(*net.TCPAddr).Port)
	state, err := OpenState(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	engine, err := NewEngine(state, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	peer := &deltaTestServer{reports: make(chan *pb.AgentMessage, 1), responses: make(chan *pb.ControlMessage, 1)}
	pb.RegisterControlServer(server, peer)
	go server.Serve(lis)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///buffered", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runSession(ctx, engine, pb.NewControlClient(conn), "test-token", "local") }()
	receive := func() *pb.AgentMessage {
		t.Helper()
		select {
		case report := <-peer.reports:
			return report
		case <-ctx.Done():
			t.Fatal("timed out waiting for report")
			return nil
		}
	}
	respond := func(message *pb.ControlMessage) {
		t.Helper()
		select {
		case peer.responses <- message:
		case <-ctx.Done():
			t.Fatal("timed out sending response")
		}
	}
	first := receive()
	if !first.SupportsConfigDelta || first.AppliedConfigToken != "" {
		t.Fatalf("initial negotiation: %v", first)
	}
	full := &pb.ControlMessage{ConfigToken: "one", Revision: 1, Node: &pb.NodeConfig{NodeId: 5, Enabled: true}, Users: []*pb.UserPolicy{{Id: 8, Enabled: true, Issued: 100}}, Rules: []*pb.Rule{{Id: 1, UserId: 8, Enabled: true, Protocol: "tcp", ListenHost: "127.0.0.1", ListenPort: port, Targets: []string{"127.0.0.1:9"}}}}
	respond(proto.Clone(full).(*pb.ControlMessage))
	failed := receive()
	if failed.Error == "" || failed.AppliedConfigToken != "" {
		t.Fatalf("listener error incorrectly acknowledged: %v", failed)
	}
	if err := busy.Close(); err != nil {
		t.Fatal(err)
	}
	respond(proto.Clone(full).(*pb.ControlMessage))
	healthy := receive()
	if healthy.Error != "" || healthy.AppliedConfigToken != "one" {
		t.Fatalf("listener retry failed: %v", healthy)
	}
	respond(&pb.ControlMessage{ConfigToken: "one", ConfigUnchanged: true, Revision: 2, Users: []*pb.UserPolicy{{Id: 8, Enabled: false, Issued: 100}}})
	receive()
	saved, err := state.Config()
	if err != nil || saved.Node.GetNodeId() != 5 || len(saved.Rules) != 1 || saved.Users[0].Enabled || saved.Revision != 2 || saved.ConfigUnchanged {
		t.Fatalf("incorrect saved configuration: %v %v", saved, err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second * 2):
		t.Fatal("stream did not stop")
	}
}

func TestStreamConfigDeltaRejectsInvalidFramesAndReconnect(t *testing.T) {
	cache := &streamConfigCache{token: "known", static: &pb.ControlMessage{Node: &pb.NodeConfig{NodeId: 1}}}
	if cache.appliedToken("listener unavailable") != "" || cache.appliedToken("") != "known" {
		t.Fatal("failed local application must request a full retry")
	}
	for _, frame := range []*pb.ControlMessage{
		{ConfigUnchanged: true},
		{ConfigUnchanged: true, ConfigToken: "other"},
		{ConfigUnchanged: true, ConfigToken: "known", Node: &pb.NodeConfig{}},
		{ConfigUnchanged: true, ConfigToken: "known", Rules: []*pb.Rule{{}}},
		{ConfigUnchanged: true, ConfigToken: "known", TunnelLinks: []*pb.TunnelLink{{}}},
		{ConfigUnchanged: true, ConfigToken: "known", EgressRules: []*pb.EgressRule{{}}},
	} {
		if _, _, _, err := cache.materialize(frame); err == nil {
			t.Fatalf("accepted invalid frame: %v", frame)
		}
	}
	// A new stream has no cache even if the preceding stream used this token.
	if _, _, _, err := (&streamConfigCache{}).materialize(&pb.ControlMessage{ConfigUnchanged: true, ConfigToken: "known"}); err == nil {
		t.Fatal("accepted delta after reconnect")
	}
	// Old panels send only full frames; they must remain valid and clear negotiation.
	_, token, static, err := cache.materialize(&pb.ControlMessage{Node: &pb.NodeConfig{NodeId: 2}})
	if err != nil || token != "" || static != nil {
		t.Fatal("old panel compatibility", err)
	}
}

func TestStreamConfigDeltaAppliesDynamicPolicyAndPersistsFullConfig(t *testing.T) {
	state, err := OpenState(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	engine, err := NewEngine(state, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	cache := &streamConfigCache{}
	full := &pb.ControlMessage{ConfigToken: "v1", Node: &pb.NodeConfig{NodeId: 5}, Users: []*pb.UserPolicy{{Id: 8, Enabled: true, Issued: 100}}}
	config, token, static, err := cache.materialize(full)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(config); err != nil {
		t.Fatal(err)
	}
	cache.token, cache.static = token, static
	if !engine.users[8].valid() {
		t.Fatal("initial policy disabled")
	}
	delta := &pb.ControlMessage{ConfigUnchanged: true, ConfigToken: "v1", Users: []*pb.UserPolicy{{Id: 8, Enabled: false, Issued: 100}}}
	config, _, _, err = cache.materialize(delta)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(config); err != nil {
		t.Fatal(err)
	}
	if engine.users[8].valid() {
		t.Fatal("revoked policy remained active")
	}
	saved, err := state.Config()
	if err != nil {
		t.Fatal(err)
	}
	if saved.Node.GetNodeId() != 5 || saved.Users[0].Enabled || saved.ConfigToken != "" || saved.ConfigUnchanged {
		t.Fatalf("invalid persisted configuration: %v", saved)
	}
}
