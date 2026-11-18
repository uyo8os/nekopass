package control

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pb "github.com/nekopass/nekopass/internal/protocol"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestNodeExchangeTimeoutReleasesLockAndStopsWork(t *testing.T) {
	p := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	token, instance := Secret(), Secret()
	var nodeID int64
	if err := p.QueryRow(ctx, `INSERT INTO nodes(name,token_hash,token) VALUES($1,$2,$3) RETURNING id`, Secret(), Hash(token), token).Scan(&nodeID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = p.Exec(context.Background(), `DELETE FROM nodes WHERE id=$1`, nodeID) })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := New(p)
	grpcServer := grpc.NewServer()
	pb.RegisterControlServer(grpcServer, &StreamServer{Server: server, exchangeTimeout: 800 * time.Millisecond})
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
	stream := open()
	initial := &pb.AgentMessage{ProtocolVersion: pb.CurrentVersion, InstanceId: instance, AppliedRevision: 1}
	if err := stream.Send(initial); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("initial exchange: %v", err)
	}

	// Occupy every pool connection after the stream is established. The next
	// transaction must wait for the pool while the independent lock is held.
	held := make([]*pgxpool.Conn, 0, p.Config().MaxConns)
	defer func() {
		for _, c := range held {
			c.Release()
		}
	}()
	for i := int32(0); i < p.Config().MaxConns; i++ {
		c, err := p.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	blocked := &pb.AgentMessage{ProtocolVersion: pb.CurrentVersion, InstanceId: instance, AppliedRevision: 999}
	if err := stream.Send(blocked); err != nil {
		t.Fatal(err)
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
	_, locked := server.nodeLocks.owners[nodeID]
	server.nodeLocks.mu.Unlock()
	if locked {
		t.Fatal("node lock still held after exchange timed out")
	}
	for _, c := range held {
		c.Release()
	}
	held = nil

	// A timed-out pool acquisition must not resume and apply the old report
	// once a connection becomes available, even if a new stream starts.
	var revision int64
	if err := p.QueryRow(ctx, `SELECT applied_revision FROM nodes WHERE id=$1`, nodeID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if revision != 1 {
		t.Fatalf("timed-out report was applied: revision=%d", revision)
	}
	reconnected := open()
	if err := reconnected.Send(&pb.AgentMessage{ProtocolVersion: pb.CurrentVersion, InstanceId: instance, AppliedRevision: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := reconnected.Recv(); err != nil {
		t.Fatalf("reconnect after exchange timeout: %v", err)
	}
	if err := p.QueryRow(ctx, `SELECT applied_revision FROM nodes WHERE id=$1`, nodeID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if revision != 2 {
		t.Fatalf("reconnected exchange did not apply: revision=%d", revision)
	}
}
