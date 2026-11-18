package control

import (
	"context"
	"net"
	"testing"
	"time"

	pb "github.com/nekopass/nekopass/internal/protocol"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestNodeExchangeTimeoutAfterTransactionBegins(t *testing.T) {
	pool := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	token, instance := Secret(), Secret()
	var nodeID int64
	if err := pool.QueryRow(ctx, `INSERT INTO nodes(name,token_hash,token) VALUES($1,$2,$3) RETURNING id`, Secret(), Hash(token), token).Scan(&nodeID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM nodes WHERE id=$1`, nodeID) })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := New(pool)
	grpcServer := grpc.NewServer()
	pb.RegisterControlServer(grpcServer, &StreamServer{Server: server, exchangeTimeout: 4 * time.Second})
	go grpcServer.Serve(listener)
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		grpcServer.Stop()
		listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); grpcServer.Stop(); listener.Close() })
	client := pb.NewControlClient(conn)
	open := func() pb.Control_ConnectClient {
		t.Helper()
		stream, err := client.Connect(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token))
		if err != nil {
			t.Fatal(err)
		}
		return stream
	}
	report := func(revision int64) *pb.AgentMessage {
		return &pb.AgentMessage{ProtocolVersion: pb.CurrentVersion, InstanceId: instance, AppliedRevision: revision}
	}
	stream := open()
	if err := stream.Send(report(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("initial exchange: %v", err)
	}

	// Hold the node row after the stream acquires its advisory lock. The next
	// exchange can obtain a pool connection and start a transaction but cannot
	// pass its SELECT FOR UPDATE until this transaction releases the row.
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	var blockerPID int32
	if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	var lockedID int64
	if err := blocker.QueryRow(ctx, `SELECT id FROM nodes WHERE id=$1 FOR UPDATE`, nodeID).Scan(&lockedID); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(report(999)); err != nil {
		t.Fatal(err)
	}

	// Verify the blocked backend is waiting on this exact transaction, rather
	// than merely assuming the stream goroutine has begun its SQL work.
	observed := false
	waitUntil := time.Now().Add(3 * time.Second)
	for time.Now().Before(waitUntil) {
		if err := pool.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM pg_stat_activity
 WHERE wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid))
 AND query LIKE 'SELECT instance_id,token_hash,protocol_version FROM nodes%'
)`, blockerPID).Scan(&observed); err != nil {
			t.Fatal(err)
		}
		if observed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !observed {
		t.Fatal("exchange never entered a transaction blocked on the node row")
	}
	result := make(chan error, 1)
	go func() { _, err := stream.Recv(); result <- err }()
	select {
	case err := <-result:
		if status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("blocked exchange returned %v, want deadline exceeded", err)
		}
	case <-ctx.Done():
		t.Fatal("blocked exchange did not time out")
	}
	server.nodeLocks.mu.Lock()
	_, stillLocked := server.nodeLocks.owners[nodeID]
	server.nodeLocks.mu.Unlock()
	if stillLocked {
		t.Fatal("node advisory lock remained held after the transaction timed out")
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err := pool.QueryRow(ctx, `SELECT applied_revision FROM nodes WHERE id=$1`, nodeID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if revision != 1 {
		t.Fatalf("timed-out transaction applied stale report: revision=%d", revision)
	}
	reconnected := open()
	if err := reconnected.Send(report(2)); err != nil {
		t.Fatal(err)
	}
	if _, err := reconnected.Recv(); err != nil {
		t.Fatalf("reconnect after transaction timeout: %v", err)
	}
	// Give any incorrectly surviving worker an opportunity to resume after the
	// row is released, and ensure it cannot overwrite the new stream's report.
	time.Sleep(200 * time.Millisecond)
	if err := pool.QueryRow(ctx, `SELECT applied_revision FROM nodes WHERE id=$1`, nodeID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if revision != 2 {
		t.Fatalf("new stream revision was overwritten: revision=%d", revision)
	}
}
