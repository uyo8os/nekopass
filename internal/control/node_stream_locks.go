package control

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var errNodeAlreadyConnected = errors.New("node already connected")
var errNodeLockLost = errors.New("node stream lock lost")

// nodeLockManager multiplexes session-level node locks over one PostgreSQL
// connection. Its mutex also protects pgx.Conn, which is not concurrency safe.
type nodeLockManager struct {
	mu     sync.Mutex
	conn   *pgx.Conn
	pid    uint32
	done   chan struct{}
	owners map[int64]string
}

type nodeStreamLock struct {
	manager    *nodeLockManager
	nodeID     int64
	generation string
	pid        uint32
	done       <-chan struct{}
}

// invalidateLocked tears down all stream owners together. Closing the carrier
// lets PostgreSQL release any locks even when an individual unlock failed.
func (m *nodeLockManager) invalidateLocked() {
	if m.conn == nil {
		return
	}
	conn := m.conn
	done := m.done
	m.conn, m.done, m.owners, m.pid = nil, nil, nil, 0
	close(done)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}

func (m *nodeLockManager) monitor(conn *pgx.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			m.mu.Lock()
			if m.conn != conn {
				m.mu.Unlock()
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err := conn.Ping(ctx)
			cancel()
			if err != nil {
				m.invalidateLocked()
			}
			m.mu.Unlock()
		}
	}
}

// acquire serializes with exchanges and other panel instances by locking the
// node row before attempting its original advisory lock. Generation changes
// are committed while that row is locked, before any new exchange can start.
func (m *nodeLockManager) acquire(ctx context.Context, pool *pgxpool.Pool, nodeID int64) (*nodeStreamLock, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var id int64
	if err = tx.QueryRow(ctx, "SELECT id FROM nodes WHERE id=$1 FOR UPDATE", nodeID).Scan(&id); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn == nil {
		conn, e := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
		if e != nil {
			return nil, e
		}
		m.conn, m.pid, m.done, m.owners = conn, conn.PgConn().PID(), make(chan struct{}), make(map[int64]string)
		go m.monitor(conn, m.done)
	}
	if _, exists := m.owners[nodeID]; exists {
		return nil, errNodeAlreadyConnected
	}
	var locked bool
	if err = m.conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", -nodeID).Scan(&locked); err != nil {
		m.invalidateLocked()
		return nil, err
	}
	if !locked {
		if len(m.owners) == 0 {
			m.invalidateLocked()
		}
		return nil, errNodeAlreadyConnected
	}
	generation := Secret()
	if _, err = tx.Exec(ctx, `INSERT INTO node_stream_sessions(node_id,generation,lock_pid) VALUES($1,$2,$3)
 ON CONFLICT(node_id) DO UPDATE SET generation=EXCLUDED.generation,lock_pid=EXCLUDED.lock_pid`, nodeID, generation, m.pid); err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		// If the unlock cannot be confirmed, discard the entire carrier.
		unlockCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		var unlocked bool
		unlockErr := m.conn.QueryRow(unlockCtx, "SELECT pg_advisory_unlock($1)", -nodeID).Scan(&unlocked)
		cancel()
		if unlockErr != nil || !unlocked || len(m.owners) == 0 {
			m.invalidateLocked()
		}
		return nil, err
	}
	m.owners[nodeID] = generation
	return &nodeStreamLock{manager: m, nodeID: nodeID, generation: generation, pid: m.pid, done: m.done}, nil
}

func (l *nodeStreamLock) release() {
	m := l.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn == nil || m.done != l.done || m.owners[l.nodeID] != l.generation {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var unlocked bool
	err := m.conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", -l.nodeID).Scan(&unlocked)
	if err != nil || !unlocked {
		m.invalidateLocked()
		return
	}
	delete(m.owners, l.nodeID)
	if len(m.owners) == 0 {
		m.invalidateLocked()
	}
}

// Caller must already hold nodes.id FOR UPDATE. Checking pg_locks and the
// generation while holding this row fences a stream after carrier loss: a new
// owner cannot register until this transaction finishes, and older owners
// cannot transact after that registration. The bigint lock key is represented
// by pg_locks as two unsigned 32-bit OIDs, with objsubid=1.
func (l *nodeStreamLock) validate(ctx context.Context, tx pgx.Tx) error {
	var valid bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM node_stream_sessions s JOIN pg_locks l ON l.pid=s.lock_pid
 WHERE s.node_id=$1 AND s.generation=$2 AND s.lock_pid=$3
 AND l.locktype='advisory' AND l.granted AND l.objsubid=1
 AND l.classid=(((-$1::bigint >> 32) & 4294967295)::oid)
 AND l.objid=(((-$1::bigint) & 4294967295)::oid)
 )`, l.nodeID, l.generation, l.pid).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return errNodeLockLost
	}
	return nil
}
