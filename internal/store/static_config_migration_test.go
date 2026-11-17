package store

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type staticVersion struct {
	epoch         string
	count, h1, h2 int64
}

func TestStaticConfigTransactionalEvents(t *testing.T) {
	dsn := os.Getenv("NEKOPASS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires a dedicated PostgreSQL test database")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || !strings.HasSuffix(cfg.ConnConfig.Database, "_test") {
		t.Fatal("refusing test outside a dedicated test database")
	}
	ctx := context.Background()
	cfg.MaxConns = 8
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	if err := Migrate(ctx, p); err != nil {
		t.Fatal(err)
	}
	version := func(db interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	}) staticVersion {
		t.Helper()
		var v staticVersion
		err := db.QueryRow(ctx, `SELECT e.epoch,x.n,x.h1,x.h2
FROM static_config_epoch e CROSS JOIN
 (SELECT count(*) n,coalesce(bit_xor(hashtextextended(id::text,0)),0) h1,
         coalesce(bit_xor(hashtextextended(id::text,1)),0) h2
  FROM static_config_events) x WHERE e.id=1`).Scan(&v.epoch, &v.count, &v.h1, &v.h2)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if _, err := p.Exec(ctx, `INSERT INTO nodes(name,token_hash) VALUES('static-config-node-a','static-config-token-a'),('static-config-node-b','static-config-token-b')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Exec(ctx, "DELETE FROM nodes WHERE name LIKE 'static-config-node-%'") })
	var a, b int64
	if err := p.QueryRow(ctx, `SELECT min(id),max(id) FROM nodes WHERE name LIKE 'static-config-node-%'`).Scan(&a, &b); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "SELECT compact_static_config_events()"); err != nil {
		t.Fatal(err)
	}
	initial := version(p)
	if initial.count != 0 {
		t.Fatalf("unexpected compacted count: %+v", initial)
	}
	if _, err := p.Exec(ctx, `UPDATE nodes SET last_seen=now(),active_connections=3,protocol_version=protocol_version WHERE id=$1`, a); err != nil {
		t.Fatal(err)
	}
	if got := version(p); got != initial {
		t.Fatalf("heartbeat invalidated static config: %+v", got)
	}
	txA, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback(ctx)
	txB, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer txB.Rollback(ctx)
	if _, err = txA.Exec(ctx, `UPDATE nodes SET protocol_version=protocol_version+1 WHERE id=$1`, a); err != nil {
		t.Fatal(err)
	}
	if _, err = txB.Exec(ctx, `UPDATE nodes SET enabled=NOT enabled WHERE id=$1`, b); err != nil {
		t.Fatal(err)
	}
	if got := version(p); got != initial {
		t.Fatalf("uncommitted events visible: %+v", got)
	}
	if err = txB.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	first := version(p)
	if first.count != 1 {
		t.Fatalf("first commit missing: %+v", first)
	}
	if err = txA.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	second := version(p)
	if second.count != 2 || second == first {
		t.Fatalf("late commit missing: %+v", second)
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE nodes SET protocol_version=protocol_version+1 WHERE id=$1`, a); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := version(p); got != second {
		t.Fatalf("rollback changed version: %+v", got)
	}
	old, err := p.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer old.Rollback(ctx)
	var ignored int
	if err = old.QueryRow(ctx, "SELECT 1").Scan(&ignored); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Exec(ctx, "SELECT compact_static_config_events()"); err != nil {
		t.Fatal(err)
	}
	if got := version(old); got != second {
		t.Fatalf("old snapshot saw a mixed compaction: %+v versus %+v", got, second)
	}
	current := version(p)
	if current.count != 0 || current.epoch == second.epoch {
		t.Fatalf("compaction did not change generation: %+v", current)
	}
	if err := old.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// Empty maintenance statements must not invalidate all node snapshots.
	for _, table := range []string{
		"nodes", "rules", "node_groups", "node_group_members", "user_node_groups",
		"users", "plans", "node_tunnel_links", "site_settings",
		"node_tls_inbounds", "node_acme_challenges",
	} {
		before := version(p)
		if _, err := p.Exec(ctx, "DELETE FROM "+table+" WHERE false"); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		after := version(p)
		if after != before {
			t.Fatalf("%s empty DELETE emitted an event: before %+v, after %+v", table, before, after)
		}
	}
	before := version(p)
	if _, err := p.Exec(ctx, "UPDATE site_settings SET config=config WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if after := version(p); after.count != before.count+1 {
		t.Fatalf("direct configuration update did not emit event: before %+v, after %+v", before, after)
	}

	// The compactor must not hold an exclusive event-table lock while a node
	// transaction is in flight. An insert before its DELETE, and a different
	// insert after DELETE but before commit, must both survive a reverse commit.
	if _, err := p.Exec(ctx, "SELECT compact_static_config_events()"); err != nil {
		t.Fatal(err)
	}
	base := version(p)
	oldSnapshot, err := p.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer oldSnapshot.Rollback(ctx)
	if got := version(oldSnapshot); got != base {
		t.Fatalf("initial snapshot: %+v", got)
	}
	pending, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Rollback(ctx)
	if _, err = pending.Exec(ctx, "UPDATE nodes SET protocol_version=protocol_version+1 WHERE id=$1", a); err != nil {
		t.Fatal(err)
	}
	compact, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer compact.Rollback(ctx)
	if _, err = compact.Exec(ctx, "SELECT compact_static_config_events()"); err != nil {
		t.Fatal(err)
	}
	later, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer later.Rollback(ctx)
	if _, err = later.Exec(ctx, "UPDATE nodes SET protocol_version=protocol_version+1 WHERE id=$1", b); err != nil {
		t.Fatal(err)
	}
	if err = later.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := version(p); got.epoch != base.epoch || got.count != 1 {
		t.Fatalf("concurrent event before compactor commit: %+v", got)
	}
	if err = compact.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	afterCompact := version(p)
	if afterCompact.epoch == base.epoch || afterCompact.count != 1 {
		t.Fatalf("concurrent event lost by compaction: %+v", afterCompact)
	}
	if got := version(oldSnapshot); got != base {
		t.Fatalf("RR snapshot observed mixed compaction: %+v", got)
	}
	if err = pending.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := version(p); got.epoch != afterCompact.epoch || got.count != 2 || got == afterCompact {
		t.Fatalf("late event lost after compaction: %+v", got)
	}
}
