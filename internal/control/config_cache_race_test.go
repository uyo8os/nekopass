package control

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pb "github.com/nekopass/nekopass/internal/protocol"
	"google.golang.org/protobuf/proto"
)

// configRaceConn commits an ACL update immediately after an identified SELECT
// has been read. The hook uses another pooled connection, so it models an
// administrator or another panel committing during a heartbeat transaction.
type configRaceConn struct {
	pool  *pgxpool.Pool
	point string
	hook  func(context.Context) error
	hit   bool
}

func (c *configRaceConn) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &configRaceTx{Tx: tx, owner: c}, nil
}

type configRaceTx struct {
	pgx.Tx
	owner    *configRaceConn
	versions int
}

func (t *configRaceTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	row := t.Tx.QueryRow(ctx, sql, args...)
	match := false
	if strings.Contains(sql, "FROM static_config_epoch") {
		t.versions++
		match = t.owner.point == "after-version" && t.versions == 1
	} else if strings.Contains(sql, "SELECT config,telegram_generation FROM site_settings") {
		match = t.owner.point == "after-settings"
	}
	if match && !t.owner.hit {
		t.owner.hit = true
		return &configRaceRow{Row: row, ctx: ctx, hook: t.owner.hook}
	}
	return row
}

type configRaceRow struct {
	pgx.Row
	ctx  context.Context
	hook func(context.Context) error
}

func (r *configRaceRow) Scan(dest ...any) error {
	if err := r.Row.Scan(dest...); err != nil {
		return err
	}
	return r.hook(r.ctx)
}

// A concurrent edit may commit after this exchange reads settings. This is
// not a linearizability guarantee: the in-flight response may still reflect
// that earlier read. It must never retain that mixed snapshot in the cache,
// and the next exchange must observe the committed settings.
func TestConfigCacheConcurrentACLChange(t *testing.T) {
	p := testDB(t)
	ctx := context.Background()
	var originalSettings []byte
	if err := p.QueryRow(ctx, "SELECT config FROM site_settings WHERE id=1").Scan(&originalSettings); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = p.Exec(context.Background(), "UPDATE site_settings SET config=$1 WHERE id=1", originalSettings)
	})
	var node int64
	if err := p.QueryRow(ctx, "INSERT INTO nodes(name,token_hash) VALUES($1,$2) RETURNING id", Secret(), Secret()).Scan(&node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = p.Exec(context.Background(), "DELETE FROM nodes WHERE id=$1", node) })
	server := &StreamServer{Server: New(p)}
	report := &pb.AgentMessage{InstanceId: Secret(), ProtocolVersion: pb.CurrentVersion, SupportsConfigDelta: true}
	for _, point := range []string{"after-version", "after-settings"} {
		t.Run(point, func(t *testing.T) {
			setACL := func(cidr string) error {
				_, err := p.Exec(ctx, "UPDATE site_settings SET config=jsonb_set(config,'{target_deny_cidrs}',to_jsonb(ARRAY[$1::text])) WHERE id=1", cidr)
				return err
			}
			if err := setACL("198.51.100.0/24"); err != nil {
				t.Fatal(err)
			}
			cache := &nodeStaticCache{budget: &server.Server.staticCacheBudget}
			defer cache.clear()
			exchange := func(conn interface {
				Begin(context.Context) (pgx.Tx, error)
			}) (*pb.ControlMessage, error) {
				return server.exchangeWithLock(ctx, conn, node, proto.Clone(report).(*pb.AgentMessage), "", nil, cache)
			}
			if _, err := exchange(p); err != nil {
				t.Fatal(err)
			}
			if cache.cached == nil {
				t.Fatal("warm cache was not populated")
			}
			injected := &configRaceConn{pool: p, point: point, hook: func(context.Context) error { return setACL("203.0.113.0/24") }}
			got, err := exchange(injected)
			if err != nil {
				t.Fatal(err)
			}
			if !injected.hit {
				t.Fatal("concurrent update hook did not run")
			}
			wantACL := "[203.0.113.0/24]"
			if point == "after-settings" {
				wantACL = "[198.51.100.0/24]"
			}
			if fmt.Sprint(got.Node.TargetDenyCidrs) != wantACL {
				t.Errorf("unexpected ACL for the exchange's read boundary: %v, want %s", got.Node.TargetDenyCidrs, wantACL)
			}
			if cache.cached != nil {
				t.Error("cached a snapshot built across the concurrent update")
			}
			next, err := exchange(p)
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := (&StreamServer{Server: New(p)}).exchange(ctx, p, node, proto.Clone(report).(*pb.AgentMessage))
			if err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(next, fresh) {
				t.Errorf("next heartbeat differs from full build\nnext: %v\nfresh: %v", next, fresh)
			}
		})
	}
}
