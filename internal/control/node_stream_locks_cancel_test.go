package control

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pause a specific advisory-lock query before pgx executes it, so request
// cancellation occurs while acquisition is using the shared carrier.
type pausedAdvisoryTracer struct {
	target           atomic.Int64
	entered          chan struct{}
	resume           chan struct{}
	cancelConnectCtx context.Context
	cancelConnect    context.CancelFunc
}

func (t *pausedAdvisoryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if data.SQL == "SELECT pg_try_advisory_lock($1)" && len(data.Args) == 1 {
		if id, ok := data.Args[0].(int64); ok && id != 0 && id == t.target.Load() {
			close(t.entered)
			<-t.resume
		}
	}
	return ctx
}

func (*pausedAdvisoryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (*pausedAdvisoryTracer) TraceConnectStart(ctx context.Context, _ pgx.TraceConnectStartData) context.Context {
	return ctx
}

func (t *pausedAdvisoryTracer) TraceConnectEnd(ctx context.Context, data pgx.TraceConnectEndData) {
	if ctx == t.cancelConnectCtx && data.Err == nil {
		t.cancelConnect()
	}
}

func TestNodeLockCanceledFirstAcquireClosesCarrier(t *testing.T) {
	basePool := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	requestCtx, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	cfg := basePool.Config()
	cfg.ConnConfig.Tracer = &pausedAdvisoryTracer{cancelConnectCtx: requestCtx, cancelConnect: cancelRequest}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var nodeID int64
	if err := pool.QueryRow(ctx, `INSERT INTO nodes(name,token_hash,token) VALUES($1,$2,$3) RETURNING id`, Secret(), Secret(), Secret()).Scan(&nodeID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = basePool.Exec(context.Background(), "DELETE FROM nodes WHERE id=$1", nodeID) })
	manager := &nodeLockManager{}
	lock, err := manager.acquire(requestCtx, pool, nodeID)
	manager.mu.Lock()
	carrierRetained := manager.conn != nil
	manager.invalidateLocked()
	manager.mu.Unlock()
	if lock != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("first acquire returned lock=%v, err=%v", lock, err)
	}
	if carrierRetained {
		t.Fatal("first acquire retained an unused carrier after cancellation")
	}
}

func TestNodeLockCanceledAcquirePreservesCarrier(t *testing.T) {
	basePool := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tracer := &pausedAdvisoryTracer{entered: make(chan struct{}), resume: make(chan struct{})}
	cfg := basePool.Config()
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var existingID, canceledID int64
	for _, id := range []*int64{&existingID, &canceledID} {
		if err := pool.QueryRow(ctx, `INSERT INTO nodes(name,token_hash,token) VALUES($1,$2,$3) RETURNING id`, Secret(), Secret(), Secret()).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = basePool.Exec(context.Background(), "DELETE FROM nodes WHERE id=ANY($1)", []int64{existingID, canceledID})
	})
	manager := &nodeLockManager{}
	existing, err := manager.acquire(ctx, pool, existingID)
	if err != nil {
		t.Fatal(err)
	}
	defer existing.release()
	tracer.target.Store(-canceledID)
	requestCtx, cancelRequest := context.WithCancel(ctx)
	result := make(chan error, 1)
	go func() {
		lock, acquireErr := manager.acquire(requestCtx, pool, canceledID)
		if lock != nil {
			lock.release()
		}
		result <- acquireErr
	}()
	select {
	case <-tracer.entered:
	case <-ctx.Done():
		close(tracer.resume)
		t.Fatal("timed out waiting for advisory-lock query")
	}
	cancelRequest()
	tracer.target.Store(0)
	close(tracer.resume)
	select {
	case err = <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled acquire returned %v", err)
		}
	case <-ctx.Done():
		t.Fatal("canceled acquire did not finish")
	}
	select {
	case <-existing.done:
		t.Fatal("canceled acquire invalidated unrelated active node")
	default:
	}
	// The independently executed query may have acquired the canceled node's
	// lock; it must be released before another panel can take it over.
	other := &nodeLockManager{}
	takeover, err := other.acquire(ctx, pool, canceledID)
	if err != nil {
		t.Fatalf("canceled node lock leaked: %v", err)
	}
	takeover.release()
	if _, err := other.acquire(ctx, pool, existingID); !errors.Is(err, errNodeAlreadyConnected) {
		t.Fatalf("unrelated node lost its lock: %v", err)
	}
}
