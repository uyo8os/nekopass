package protocol

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type pathTestServer struct{ UnimplementedControlServer }

func (pathTestServer) Connect(stream grpc.BidiStreamingServer[AgentMessage, ControlMessage]) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	return stream.Send(&ControlMessage{})
}

func TestControlServicePath(t *testing.T) {
	const newPath = "/apacrerx.v1.Control/Connect"
	const oldPath = "/nekopass.v1.Control/Connect"
	if Control_Connect_FullMethodName != newPath {
		t.Fatalf("generated client path = %q, want %q", Control_Connect_FullMethodName, newPath)
	}

	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	RegisterControlServer(srv, pathTestServer{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop(); _ = lis.Close() })

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := NewControlClient(conn).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&AgentMessage{}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("new path failed: %v", err)
	}

	oldStream, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := oldStream.RecvMsg(&ControlMessage{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("old path error = %v, want Unimplemented", err)
	}
}
