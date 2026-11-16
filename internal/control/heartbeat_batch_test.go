package control

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	pb "github.com/nekopass/nekopass/internal/protocol"
)

type batchCountingTx struct {
	pgx.Tx
	queries, execs int
}

func TestHeartbeatBatchLegacyAccounting(t *testing.T) {
	p := testDB(t)
	ctx := context.Background()
	user, _ := testUser(t, p, 10000, 10, false)
	var node int64
	if err := p.QueryRow(ctx, `INSERT INTO nodes(name,token_hash) VALUES($1,$2) RETURNING id`, Secret(), Secret()).Scan(&node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = p.Exec(ctx, `DELETE FROM grants WHERE node_id=$1`, node)
		_, _ = p.Exec(ctx, `DELETE FROM nodes WHERE id=$1`, node)
	})
	if _, err := p.Exec(ctx, `INSERT INTO grants(node_id,user_id,issued) VALUES($1,$2,100)`, node, user); err != nil {
		t.Fatal(err)
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = applyUsageBatch(ctx, tx, node, 24, []*pb.Usage{{UserId: user, Spent: 100, Traffic: 80}}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var charged int64
	if err = p.QueryRow(ctx, `SELECT charged_bytes FROM grants WHERE node_id=$1 AND user_id=$2`, node, user).Scan(&charged); err != nil || charged != 80 {
		t.Fatalf("legacy charged=%d err=%v", charged, err)
	}
}

// Opt in because this exercises the protocol's full 10,000-user and
// 100,000-rule report limits and intentionally creates many database rows.
func TestHeartbeatBatchMaximumReports(t *testing.T) {
	if os.Getenv("NEKOPASS_BATCH_STRESS") != "1" {
		t.Skip("set NEKOPASS_BATCH_STRESS=1 for the protocol-limit stress test")
	}
	p := testDB(t)
	ctx := context.Background()
	user, _ := testUser(t, p, 1000000, 10, false)
	var node int64
	if err := p.QueryRow(ctx, `INSERT INTO nodes(name,token_hash) VALUES($1,$2) RETURNING id`, Secret(), Secret()).Scan(&node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = p.Exec(ctx, `DELETE FROM grants WHERE node_id=$1`, node)
		_, _ = p.Exec(ctx, `DELETE FROM rule_usage WHERE node_id=$1`, node)
		_, _ = p.Exec(ctx, `DELETE FROM nodes WHERE id=$1`, node)
	})
	if _, err := p.Exec(ctx, `INSERT INTO grants(node_id,user_id,quota_epoch,issued) SELECT $1,$2,n,100 FROM generate_series(0,9999) n`, node, user); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO rule_usage(node_id,rule_id,user_id) SELECT $1,n,$2 FROM generate_series(1,100000) n`, node, user); err != nil {
		t.Fatal(err)
	}
	usage := make([]*pb.Usage, 10000)
	rules := make([]*pb.RuleUsage, 100000)
	for i := range usage {
		usage[i] = &pb.Usage{UserId: user, QuotaEpoch: int64(i), Spent: 10, Traffic: 10, ChargedBytes: 10}
	}
	for i := range rules {
		rules[i] = &pb.RuleUsage{RuleId: int64(i + 1), Traffic: 10}
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	start := time.Now()
	if err = applyUsageBatch(ctx, tx, node, 35, usage); err != nil {
		t.Fatal(err)
	}
	if err = applyRuleUsageBatch(ctx, tx, node, rules); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("processed 10000 usage and 100000 rule reports in %s", time.Since(start))
	var count, sum int64
	if err = p.QueryRow(ctx, `SELECT count(*),sum(traffic_bytes) FROM rule_events WHERE node_id=$1`, node).Scan(&count, &sum); err != nil || count != 100000 || sum != 1000000 {
		t.Fatalf("rule events count=%d sum=%d err=%v", count, sum, err)
	}
	if err = p.QueryRow(ctx, `SELECT count(*),sum(traffic_bytes) FROM usage_events WHERE node_id=$1`, node).Scan(&count, &sum); err != nil || count != 10000 || sum != 100000 {
		t.Fatalf("usage events count=%d sum=%d err=%v", count, sum, err)
	}
}

func (tx *batchCountingTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	tx.queries++
	return tx.Tx.Query(ctx, sql, args...)
}
func (tx *batchCountingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tx.execs++
	return tx.Tx.Exec(ctx, sql, args...)
}

func TestHeartbeatBatchStatementCountDoesNotGrowWithReports(t *testing.T) {
	p := testDB(t)
	ctx := context.Background()
	user, _ := testUser(t, p, 10000, 10, false)
	var node int64
	if err := p.QueryRow(ctx, `INSERT INTO nodes(name,token_hash) VALUES($1,$2) RETURNING id`, Secret(), Secret()).Scan(&node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = p.Exec(ctx, `DELETE FROM grants WHERE node_id=$1`, node)
		_, _ = p.Exec(ctx, `DELETE FROM rule_usage WHERE node_id=$1`, node)
		_, _ = p.Exec(ctx, `DELETE FROM nodes WHERE id=$1`, node)
	})
	if _, err := p.Exec(ctx, `INSERT INTO grants(node_id,user_id,quota_epoch,issued) SELECT $1,$2,n,100 FROM generate_series(0,399) n`, node, user); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO rule_usage(node_id,rule_id,user_id) SELECT $1,n,$2 FROM generate_series(1,400) n`, node, user); err != nil {
		t.Fatal(err)
	}
	usage := make([]*pb.Usage, 400)
	rules := make([]*pb.RuleUsage, 400)
	for i := range usage {
		usage[i] = &pb.Usage{UserId: user, QuotaEpoch: int64(i), Spent: 10, Traffic: 10, ChargedBytes: 10}
		rules[i] = &pb.RuleUsage{RuleId: int64(i + 1), Traffic: 10}
	}
	inner, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Rollback(ctx)
	tx := &batchCountingTx{Tx: inner}
	if err = applyUsageBatch(ctx, tx, node, 35, usage); err != nil {
		t.Fatal(err)
	}
	if err = applyRuleUsageBatch(ctx, tx, node, rules); err != nil {
		t.Fatal(err)
	}
	if tx.queries != 2 || tx.execs != 4 {
		t.Fatalf("400 reports used %d queries and %d writes", tx.queries, tx.execs)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var sum int64
	if err = p.QueryRow(ctx, `SELECT sum(traffic_bytes) FROM usage_events WHERE node_id=$1`, node).Scan(&sum); err != nil || sum != 4000 {
		t.Fatalf("usage events sum=%d err=%v", sum, err)
	}
	if err = p.QueryRow(ctx, `SELECT sum(traffic_bytes) FROM rule_events WHERE node_id=$1`, node).Scan(&sum); err != nil || sum != 4000 {
		t.Fatalf("rule events sum=%d err=%v", sum, err)
	}
}

func TestHeartbeatBatchCumulativeReports(t *testing.T) {
	p := testDB(t)
	ctx := context.Background()
	u1, _ := testUser(t, p, 10000, 10, false)
	u2, _ := testUser(t, p, 10000, 10, false)
	var node int64
	if err := p.QueryRow(ctx, `INSERT INTO nodes(name,token_hash) VALUES($1,$2) RETURNING id`, Secret(), Secret()).Scan(&node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = p.Exec(ctx, `DELETE FROM grants WHERE node_id=$1`, node)
		_, _ = p.Exec(ctx, `DELETE FROM rule_usage WHERE node_id=$1`, node)
		_, _ = p.Exec(ctx, `DELETE FROM nodes WHERE id=$1`, node)
	})
	if _, err := p.Exec(ctx, `INSERT INTO grants(node_id,user_id,issued,unlimited_open) VALUES($1,$2,1000,true),($1,$3,1000,false)`, node, u1, u2); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO rule_usage(node_id,rule_id,user_id) VALUES($1,1001,$2),($1,1002,$3)`, node, u1, u2); err != nil {
		t.Fatal(err)
	}
	usage := []*pb.Usage{
		{UserId: u1, Spent: 100, Traffic: 90, ChargedBytes: 80, QuotaUnlimited: true},
		{UserId: u2, Spent: 40, Traffic: 30, ChargedBytes: 30},
		{UserId: u1, Spent: 200, Traffic: 170, ChargedBytes: 150, QuotaUnlimited: false},
		{UserId: 999999999},
	}
	ruleUsage := []*pb.RuleUsage{{RuleId: 1001, Traffic: 10}, {RuleId: 1002, Traffic: 30}, {RuleId: 1001, Traffic: 8}, {RuleId: 1001, Traffic: 25}, {RuleId: 999999999, Traffic: 99}}
	for i := 0; i < 2; i++ {
		if i == 1 {
			usage = []*pb.Usage{usage[2], usage[1], usage[3]}
		}
		tx, err := p.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = applyUsageBatch(ctx, tx, node, 35, usage); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err = applyRuleUsageBatch(ctx, tx, node, ruleUsage); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var spent, traffic, charged int64
	var open bool
	if err := p.QueryRow(ctx, `SELECT spent,traffic,charged_bytes,unlimited_open FROM grants WHERE node_id=$1 AND user_id=$2`, node, u1).Scan(&spent, &traffic, &charged, &open); err != nil {
		t.Fatal(err)
	}
	if spent != 200 || traffic != 170 || charged != 150 || open {
		t.Fatalf("grant=(%d,%d,%d,%v)", spent, traffic, charged, open)
	}
	var events, bytes int64
	if err := p.QueryRow(ctx, `SELECT count(*),sum(traffic_bytes) FROM usage_events WHERE node_id=$1 AND user_id=$2`, node, u1).Scan(&events, &bytes); err != nil || events != 2 || bytes != 170 {
		t.Fatalf("usage events=(%d,%d), err=%v", events, bytes, err)
	}
	if err := p.QueryRow(ctx, `SELECT count(*),sum(traffic_bytes) FROM rule_events WHERE node_id=$1 AND rule_id=1001`, node).Scan(&events, &bytes); err != nil || events != 2 || bytes != 25 {
		t.Fatalf("rule events=(%d,%d), err=%v", events, bytes, err)
	}
	// A valid report followed by an invalid report must not partially commit.
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = applyUsageBatch(ctx, tx, node, 35, []*pb.Usage{{UserId: u1, Spent: 250, Traffic: 200, ChargedBytes: 180}, {UserId: u2, Spent: 1001, Traffic: 30, ChargedBytes: 30}})
	_ = tx.Rollback(ctx)
	if err == nil {
		t.Fatal("accepted overdrawn batch")
	}
	if err = p.QueryRow(ctx, `SELECT spent FROM grants WHERE node_id=$1 AND user_id=$2`, node, u1).Scan(&spent); err != nil || spent != 200 {
		t.Fatalf("invalid batch changed grant: %d, err=%v", spent, err)
	}
	// A later invalid rule report rolls back an otherwise valid usage update.
	tx, err = p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = applyUsageBatch(ctx, tx, node, 35, []*pb.Usage{{UserId: u1, Spent: 250, Traffic: 200, ChargedBytes: 180}}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	err = applyRuleUsageBatch(ctx, tx, node, []*pb.RuleUsage{{RuleId: 1001, Traffic: -1}})
	_ = tx.Rollback(ctx)
	if err == nil {
		t.Fatal("accepted negative rule traffic")
	}
	if err = p.QueryRow(ctx, `SELECT spent FROM grants WHERE node_id=$1 AND user_id=$2`, node, u1).Scan(&spent); err != nil || spent != 200 {
		t.Fatalf("invalid rule report changed grant: %d, err=%v", spent, err)
	}
	// Unlimited traffic cannot be added after the finite report closed it.
	tx, err = p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = applyUsageBatch(ctx, tx, node, 35, []*pb.Usage{{UserId: u1, Spent: 200, Traffic: 170, ChargedBytes: 150, UnlimitedSpent: 1}})
	_ = tx.Rollback(ctx)
	if err == nil {
		t.Fatal("accepted unlimited spending after grant closed")
	}
}
