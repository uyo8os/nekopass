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

// A live HTTP/2 transport can still contain a stream that has stopped sending
// reports. Such a stream must not reserve its node lock indefinitely, nor
// should its timeout disrupt another node on the same lock carrier.
func TestNodeIdleStreamReleasesLock(t *testing.T) {
	p := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := New(p)
	grpcServer := grpc.NewServer()
	pb.RegisterControlServer(grpcServer, &StreamServer{Server: server, idleTimeout: 2 * time.Second})
	go grpcServer.Serve(listener)
	t.Cleanup(func() { grpcServer.Stop(); listener.Close() })
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	client := pb.NewControlClient(conn)

	type node struct {
		id              int64
		token, instance string
	}
	nodes := make([]node, 2)
	for i := range nodes {
		n := &nodes[i]
		n.token, n.instance = Secret(), Secret()
		if err := p.QueryRow(ctx, `INSERT INTO nodes(name,token_hash,token) VALUES($1,$2,$3) RETURNING id`, Secret(), Hash(n.token), n.token).Scan(&n.id); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = p.Exec(context.Background(), "DELETE FROM nodes WHERE id=$1", n.id) })
	}
	open := func(n node) (pb.Control_ConnectClient, context.CancelFunc, error) {
		streamCtx, stop := context.WithCancel(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+n.token))
		stream, err := client.Connect(streamCtx)
		if err == nil {
			err = stream.Send(&pb.AgentMessage{ProtocolVersion: pb.CurrentVersion, InstanceId: n.instance})
		}
		if err == nil {
			_, err = stream.Recv()
		}
		return stream, stop, err
	}

	idle, stopIdle, err := open(nodes[0])
	defer stopIdle()
	if err != nil {
		t.Fatalf("open node that will become idle: %v", err)
	}
	active, stopActive, err := open(nodes[1])
	defer stopActive()
	if err != nil {
		t.Fatalf("open active node: %v", err)
	}

	// Keep a second node exchanging heartbeats through the idle node's timeout.
	heartbeatCtx, stopHeartbeats := context.WithCancel(ctx)
	heartbeatsDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				heartbeatsDone <- nil
				return
			case <-ticker.C:
				if err := active.Send(&pb.AgentMessage{ProtocolVersion: pb.CurrentVersion, InstanceId: nodes[1].instance}); err != nil {
					heartbeatsDone <- err
					return
				}
				if _, err := active.Recv(); err != nil {
					heartbeatsDone <- err
					return
				}
			}
		}
	}()
	defer stopHeartbeats()

	// The original stream is still connected: a duplicate must first be fenced.
	_, stopDuplicate, err := open(nodes[0])
	stopDuplicate()
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("duplicate connection before timeout: got %v, want AlreadyExists", err)
	}
	// Do not cancel the old stream. Let the server identify the silent stream
	// and release its advisory lock despite the live transport.
	_, err = idle.Recv()
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("idle stream: got %v, want DeadlineExceeded", err)
	}

	recovered, stopRecovered, err := open(nodes[0])
	defer stopRecovered()
	if err != nil {
		t.Fatalf("reconnect after idle stream expires: %v", err)
	}
	if err = recovered.Send(&pb.AgentMessage{ProtocolVersion: pb.CurrentVersion, InstanceId: nodes[0].instance}); err != nil {
		t.Fatalf("send after recovery: %v", err)
	}
	if _, err = recovered.Recv(); err != nil {
		t.Fatalf("exchange after recovery: %v", err)
	}
	stopHeartbeats()
	select {
	case err = <-heartbeatsDone:
		if err != nil {
			t.Fatalf("unrelated active node interrupted: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("active node did not stop after recovery")
	}
}
