package control

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestNodeLockCarrierAndFencing(t *testing.T) {
	p := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const n = 24
	ids := make([]int64, n)
	for i := range ids {
		if err := p.QueryRow(ctx, `INSERT INTO nodes(name, token_hash, token) VALUES($1,$2,$3) RETURNING id`, Secret(), Secret(), Secret()).Scan(&ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = p.Exec(context.Background(), "DELETE FROM nodes WHERE id=$1", id)
		}
	})
	a, b := &nodeLockManager{}, &nodeLockManager{}
	locks := make([]*nodeStreamLock, n)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			locks[i], err = a.acquire(ctx, p, ids[i])
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	defer func() {
		for _, l := range locks {
			if l != nil {
				l.release()
			}
		}
	}()
	for _, l := range locks {
		if l.pid != locks[0].pid {
			t.Fatalf("different carrier PIDs: %d and %d", l.pid, locks[0].pid)
		}
		tx, err := p.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "SELECT id FROM nodes WHERE id=$1 FOR UPDATE", l.nodeID); err != nil {
			t.Fatal(err)
		}
		if err = l.validate(ctx, tx); err != nil {
			t.Fatalf("valid lock: %v", err)
		}
		_ = tx.Rollback(ctx)
	}
	if _, err := a.acquire(ctx, p, ids[0]); !errors.Is(err, errNodeAlreadyConnected) {
		t.Fatalf("same panel duplicate: %v", err)
	}
	if _, err := b.acquire(ctx, p, ids[0]); !errors.Is(err, errNodeAlreadyConnected) {
		t.Fatalf("other panel duplicate: %v", err)
	}

	// Releasing a stale object must never unlock its replacement.
	old := locks[0]
	old.release()
	newLock, err := a.acquire(ctx, p, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	locks[0] = newLock
	old.release()
	if _, err = b.acquire(ctx, p, ids[0]); !errors.Is(err, errNodeAlreadyConnected) {
		t.Fatalf("stale release removed lock: %v", err)
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "SELECT id FROM nodes WHERE id=$1 FOR UPDATE", old.nodeID); err != nil {
		t.Fatal(err)
	}
	if err = old.validate(ctx, tx); !errors.Is(err, errNodeLockLost) {
		t.Fatalf("stale generation accepted: %v", err)
	}
	_ = tx.Rollback(ctx)

	// Carrier loss invalidates every stream and allows another panel to take over.
	if _, err = p.Exec(ctx, "SELECT pg_terminate_backend($1)", newLock.pid); err != nil {
		t.Fatal(err)
	}
	select {
	case <-newLock.done:
	case <-ctx.Done():
		t.Fatal("carrier death not detected")
	}
	for i, l := range locks {
		select {
		case <-l.done:
		default:
			t.Fatalf("stream %d not invalidated", i)
		}
	}
	replacement, err := b.acquire(ctx, p, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.release()
	newLock.release()
	if _, err = a.acquire(ctx, p, ids[0]); !errors.Is(err, errNodeAlreadyConnected) {
		t.Fatalf("dead carrier released new owner: %v", err)
	}
	tx, err = p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "SELECT id FROM nodes WHERE id=$1 FOR UPDATE", old.nodeID); err != nil {
		t.Fatal(err)
	}
	if err = newLock.validate(ctx, tx); !errors.Is(err, errNodeLockLost) {
		t.Fatalf("dead carrier generation accepted: %v", err)
	}
	if err = replacement.validate(ctx, tx); err != nil {
		t.Fatalf("replacement invalid: %v", err)
	}
	_ = tx.Rollback(ctx)
}

func TestNodeLockTakeoverWaitsForPreviousTransaction(t *testing.T) {
	p := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var id int64
	if err := p.QueryRow(ctx, `INSERT INTO nodes(name,token_hash,token) VALUES($1,$2,$3) RETURNING id`, Secret(), Secret(), Secret()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = p.Exec(context.Background(), "DELETE FROM nodes WHERE id=$1", id) })
	a, b := &nodeLockManager{}, &nodeLockManager{}
	old, err := a.acquire(ctx, p, id)
	if err != nil {
		t.Fatal(err)
	}
	defer old.release()
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, "SELECT id FROM nodes WHERE id=$1 FOR UPDATE", id); err != nil {
		t.Fatal(err)
	}
	if err = old.validate(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Exec(ctx, "SELECT pg_terminate_backend($1)", old.pid); err != nil {
		t.Fatal(err)
	}
	select {
	case <-old.done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	type result struct {
		lock *nodeStreamLock
		err  error
	}
	resultCh := make(chan result, 1)
	go func() { l, e := b.acquire(ctx, p, id); resultCh <- result{l, e} }()
	select {
	case r := <-resultCh:
		if r.lock != nil {
			r.lock.release()
		}
		t.Fatalf("takeover before previous commit: %v", r.err)
	case <-time.After(250 * time.Millisecond):
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-resultCh:
		if r.err != nil {
			t.Fatal(r.err)
		}
		defer r.lock.release()
	case <-ctx.Done():
		t.Fatal(fmt.Errorf("takeover stuck: %w", ctx.Err()))
	}
}
