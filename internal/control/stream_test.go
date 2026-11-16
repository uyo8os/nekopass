package control

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	pb "github.com/nekopass/nekopass/internal/protocol"
	"github.com/nekopass/nekopass/internal/store"
)

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("NEKOPASS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set NEKOPASS_TEST_DATABASE_URL to a dedicated test database")
	}
	config, parseErr := pgxpool.ParseConfig(url)
	if parseErr != nil || !strings.HasSuffix(config.ConnConfig.Database, "_test") {
		t.Fatal("refusing integration tests outside a dedicated database whose name ends in _test")
	}
	p, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	if err = store.Migrate(context.Background(), p); err != nil {
		t.Fatal(err)
	}

	// Existing integration fixtures intentionally use local targets and older
	// capabilities. Configure that environment explicitly; ACL tests replace it.
	var original []byte
	if err = p.QueryRow(context.Background(), "SELECT config FROM site_settings WHERE id=1").Scan(&original); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Exec(context.Background(), `UPDATE site_settings SET config=config||'{"target_deny_cidrs":[],"proxy_trusted_cidrs":["192.0.2.0/24"]}'::jsonb WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = p.Exec(context.Background(), "UPDATE site_settings SET config=$1 WHERE id=1", original) })
	return p
}
func TestConcurrentGrantsAndReplay(t *testing.T) {
	p := testDB(t)
	ctx := context.Background()
	s := &StreamServer{Server: New(p)}
	uid, _ := testUser(t, p, 1000000, 10, false)
	name := Secret()
	var err error
	var nodes [2]int64
	for i := range nodes {
		err = p.QueryRow(ctx, "INSERT INTO nodes(name,token_hash) VALUES($1,$2) RETURNING id", Secret(), Secret()).Scan(&nodes[i])
		if err != nil {
			t.Fatal(err)
		}
		testAuthorize(t, p, uid, nodes[i])
	}
	t.Cleanup(func() {
		for _, q := range []string{"DELETE FROM usage_events WHERE user_id=$1", "DELETE FROM grants WHERE user_id=$1", "DELETE FROM users WHERE id=$1"} {
			_, _ = p.Exec(ctx, q, uid)
		}
		for _, n := range nodes {
			_, _ = p.Exec(ctx, "DELETE FROM nodes WHERE id=$1", n)
		}
	})
	var wg sync.WaitGroup
	var issued [2]int64
	for i, n := range nodes {
		wg.Add(1)
		go func(i int, n int64) {
			defer wg.Done()
			c, e := p.Acquire(ctx)
			if e != nil {
				t.Error(e)
				return
			}
			defer c.Release()
			out, e := s.exchange(ctx, c, n, &pb.AgentMessage{InstanceId: name, RequestUsers: []int64{uid}})
			if e != nil {
				t.Error(e)
				return
			}
			issued[i] = out.Users[0].Issued
		}(i, n)
	}
	wg.Wait()
	if issued[0]+issued[1] != 1000000 {
		t.Fatalf("over/under allocated: %v", issued)
	}
	winner := nodes[0]
	if issued[1] > 0 {
		winner = nodes[1]
	}
	c, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	report := &pb.AgentMessage{InstanceId: name, Usage: []*pb.Usage{{UserId: uid, Spent: 1000000, Traffic: 500000}}}
	for i := 0; i < 3; i++ {
		if _, err = s.exchange(ctx, c, winner, report); err != nil {
			t.Fatal(err)
		}
	}
	var total int64
	err = p.QueryRow(ctx, "SELECT COALESCE(sum(traffic_bytes),0)::bigint FROM usage_events WHERE user_id=$1", uid).Scan(&total)
	if err != nil {
		t.Fatal(err)
	}
	if total != 500000 {
		t.Fatalf("replay billed %d", total)
	}
	report.Usage[0].Traffic = 499999
	if _, err = s.exchange(ctx, c, winner, report); err == nil {
		t.Fatal("accepted regressed state")
	}
	report.Usage[0].Traffic = 500000
	report.Usage[0].Spent = 1000001
	if _, err = s.exchange(ctx, c, winner, report); err == nil {
		t.Fatal("accepted overspent grant")
	}
	report.Usage[0].Spent = 1000000
	report.InstanceId = "different-instance-id"
	if _, err = s.exchange(ctx, c, winner, report); err == nil {
		t.Fatal("accepted replacement agent without state")
	}
}

// Record settings actually applied inside the heartbeat transaction while
// still executing against PostgreSQL, including the normal commit/error paths.
type tracedExchangeConn struct {
	pool     *pgxpool.Pool
	settings []string
}

type tracedExchangeTx struct {
	pgx.Tx
	owner *tracedExchangeConn
}

func (c *tracedExchangeConn) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &tracedExchangeTx{Tx: tx, owner: c}, nil
}

func (tx *tracedExchangeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.HasPrefix(sql, "SET LOCAL ") {
		tx.owner.settings = append(tx.owner.settings, sql)
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func TestExchangePlannerSettingsAreTransactionLocal(t *testing.T) {
	base := testDB(t)
	ctx := context.Background()
	config := base.Config().Copy()
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, setting := range []string{"SET jit = on", "SET join_collapse_limit = 8"} {
		if _, err = pool.Exec(ctx, setting); err != nil {
			t.Fatal(err)
		}
	}
	var node int64
	token := Secret()
	if err = base.QueryRow(ctx, "INSERT INTO nodes(name,token_hash) VALUES($1,$2) RETURNING id", Secret(), Hash(token)).Scan(&node); err != nil {
		t.Fatal(err)
	}
	defer base.Exec(ctx, "DELETE FROM nodes WHERE id=$1", node)
	s := &StreamServer{Server: New(pool)}
	c := &tracedExchangeConn{pool: pool}
	report := &pb.AgentMessage{InstanceId: Secret(), ProtocolVersion: 35}
	for _, expectedHash := range []string{Hash(token), Hash(Secret())} {
		c.settings = nil
		_, err = s.exchangeWithCredential(ctx, c, node, report, expectedHash)
		if (expectedHash == Hash(token)) != (err == nil) {
			t.Fatalf("unexpected exchange result: %v", err)
		}
		if len(c.settings) != 2 || c.settings[0] != "SET LOCAL jit = off" || c.settings[1] != "SET LOCAL join_collapse_limit = 1" {
			t.Fatalf("heartbeat did not configure local planner settings: %v", c.settings)
		}
		var jit, collapse string
		if err = pool.QueryRow(ctx, "SELECT current_setting('jit'), current_setting('join_collapse_limit')").Scan(&jit, &collapse); err != nil {
			t.Fatal(err)
		}
		if jit != "on" || collapse != "8" {
			t.Fatalf("exchange leaked planner settings: %s %s", jit, collapse)
		}
	}
}
