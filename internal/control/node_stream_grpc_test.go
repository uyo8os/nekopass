package control

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync"
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

func TestNodeLockGRPCManyNodesAndRecovery(t *testing.T) {
	p := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	listen := func() (pb.ControlClient, *Server) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		s := New(p)
		g := grpc.NewServer()
		pb.RegisterControlServer(g, &StreamServer{Server: s})
		go g.Serve(l)
		cc, err := grpc.NewClient(l.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cc.Close(); g.Stop(); l.Close() })
		return pb.NewControlClient(cc), s
	}
	a, serverA := listen()
	b, _ := listen()
	const total = 58
	type node struct {
		id              int64
		token, instance string
		version         int32
	}
	nodes := make([]node, total)
	for i := range nodes {
		n := &nodes[i]
		n.token, n.instance = Secret(), Secret()
		n.version = pb.CurrentVersion
		if i == total-1 {
			n.version = 5
		} // Old supported Agent protocol.
		if err := p.QueryRow(ctx, `INSERT INTO nodes(name,token_hash,token) VALUES($1,$2,$3) RETURNING id`, Secret(), Hash(n.token), n.token).Scan(&n.id); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, n := range nodes {
			_, _ = p.Exec(context.Background(), "DELETE FROM nodes WHERE id=$1", n.id)
		}
	})
	open := func(client pb.ControlClient, n node) (pb.Control_ConnectClient, context.CancelFunc, error) {
		streamCtx, stop := context.WithCancel(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+n.token))
		stream, err := client.Connect(streamCtx)
		if err == nil {
			err = stream.Send(&pb.AgentMessage{ProtocolVersion: n.version, InstanceId: n.instance})
		}
		if err == nil {
			_, err = stream.Recv()
		}
		return stream, stop, err
	}
	streams := make([]pb.Control_ConnectClient, total)
	stops := make([]context.CancelFunc, total)
	var wg sync.WaitGroup
	errs := make(chan error, total)
	for i := range nodes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			streams[i], stops[i], err = open(a, nodes[i])
			if err != nil {
				errs <- fmt.Errorf("node %d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	defer func() {
		for _, stop := range stops {
			if stop != nil {
				stop()
			}
		}
	}()
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	serverA.nodeLocks.mu.Lock()
	pid, ownerCount := serverA.nodeLocks.pid, len(serverA.nodeLocks.owners)
	serverA.nodeLocks.mu.Unlock()
	if ownerCount != total {
		t.Fatalf("expected %d locks, got %d", total, ownerCount)
	}
	var lockBackends int
	if err := p.QueryRow(ctx, `SELECT count(DISTINCT pid) FROM pg_locks WHERE locktype='advisory' AND granted AND pid=$1`, pid).Scan(&lockBackends); err != nil {
		t.Fatal(err)
	}
	if lockBackends != 1 {
		t.Fatalf("expected one carrier backend, got %d", lockBackends)
	}
	var dbBackends int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND usename=current_user AND backend_type='client backend'`).Scan(&dbBackends); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d active node streams: %d PostgreSQL client backends (pool maximum %d + one carrier)", total, dbBackends, p.Config().MaxConns)
	if dbBackends > int(p.Config().MaxConns)+1 {
		t.Fatalf("unexpected per-node database connections: %d", dbBackends)
	}
	// Saturating the query pool delays, but must not deadlock, an exchange. The
	// lock carrier stays independent from the pool and can continue monitoring.
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
	exchangeResult := make(chan error, 1)
	go func() {
		err := streams[1].Send(&pb.AgentMessage{ProtocolVersion: pb.CurrentVersion, InstanceId: nodes[1].instance})
		if err == nil {
			_, err = streams[1].Recv()
		}
		exchangeResult <- err
	}()
	select {
	case err := <-exchangeResult:
		t.Fatalf("exchange succeeded with fully occupied pool: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	for _, c := range held {
		c.Release()
	}
	held = nil
	select {
	case err := <-exchangeResult:
		if err != nil {
			t.Fatalf("exchange after releasing pool: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("exchange deadlocked: %v", ctx.Err())
	}
	// Exercise concurrent heartbeats with more streams than pool connections.
	// The timing output characterizes this local run, not a scale SLA. Avoid a
	// hard latency assertion because shared CI machines can be heavily loaded.
	var latency []time.Duration
	for round := 0; round < 5; round++ {
		type sample struct {
			elapsed time.Duration
			err     error
		}
		samples := make(chan sample, total)
		for i := range streams {
			go func(i int) {
				started := time.Now()
				err := streams[i].Send(&pb.AgentMessage{ProtocolVersion: nodes[i].version, InstanceId: nodes[i].instance})
				if err == nil {
					_, err = streams[i].Recv()
				}
				samples <- sample{time.Since(started), err}
			}(i)
		}
		for range streams {
			s := <-samples
			if s.err != nil {
				t.Fatalf("round %d heartbeat: %v", round, s.err)
			}
			latency = append(latency, s.elapsed)
		}
	}
	sort.Slice(latency, func(i, j int) bool { return latency[i] < latency[j] })
	t.Logf("%d concurrent heartbeat samples: p50=%s p95=%s max=%s", len(latency), latency[len(latency)/2], latency[len(latency)*95/100], latency[len(latency)-1])
	_, stopDuplicate, err := open(b, nodes[0])
	stopDuplicate()
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("duplicate node across panels: %v", err)
	}
	// A carrier failure also cancels an exchange waiting for a saturated pool.
	for i := int32(0); i < p.Config().MaxConns; i++ {
		c, err := p.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	blocked := make(chan error, 1)
	go func() {
		err := streams[1].Send(&pb.AgentMessage{ProtocolVersion: nodes[1].version, InstanceId: nodes[1].instance})
		if err == nil {
			_, err = streams[1].Recv()
		}
		blocked <- err
	}()
	select {
	case err := <-blocked:
		t.Fatalf("exchange did not block on saturated pool: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if _, err = held[0].Exec(ctx, "SELECT pg_terminate_backend($1)", pid); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-blocked:
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("blocked exchange did not terminate after carrier loss: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("blocked exchange did not stop after carrier loss")
	}
	for _, c := range held {
		c.Release()
	}
	held = nil
	// Each idle server handler must return when its shared carrier is lost.
	for i, s := range streams {
		if i == 1 {
			continue
		}
		_, err = s.Recv()
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("node %d did not observe carrier failure: %v", i, err)
		}
	}
	recovered, stopRecovered, err := open(b, nodes[0])
	defer stopRecovered()
	if err != nil {
		t.Fatalf("reconnect after carrier loss: %v", err)
	}
	if err = recovered.Send(&pb.AgentMessage{ProtocolVersion: pb.CurrentVersion, InstanceId: nodes[0].instance}); err != nil {
		t.Fatal(err)
	}
	if _, err = recovered.Recv(); err != nil {
		t.Fatalf("second exchange after reconnect: %v", err)
	}
	if _, err = p.Exec(ctx, `UPDATE nodes SET token_hash=$2 WHERE id=$1`, nodes[0].id, Hash(Secret())); err != nil {
		t.Fatal(err)
	}
	if err = recovered.Send(&pb.AgentMessage{ProtocolVersion: pb.CurrentVersion, InstanceId: nodes[0].instance}); err != nil {
		t.Fatal(err)
	}
	if _, err = recovered.Recv(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("revoked token accepted: %v", err)
	}
}
