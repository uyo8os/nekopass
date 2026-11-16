package control

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	pb "github.com/nekopass/nekopass/internal/protocol"
)

// Keep these checks and the route definitions in ruleListSQL and migration043.sql
// synchronized: status reporting must agree with the routes actually deployed.
func TestRuleListRouteAndResponseEquivalent(t *testing.T) {
	f := newSecurityFixture(t)
	ctx := context.Background()
	var nodes, nodeGrants [4]int64
	for i := range nodes {
		err := f.p.QueryRow(ctx, `INSERT INTO nodes(name,token_hash,ingress_enabled,tunnel_exit_enabled,tunnel_public_host,tunnel_listen_port,protocol_version,last_seen,applied_revision) VALUES($1,$2,true,true,'example.test',24443,40,now(),1000000) RETURNING id`, Secret(), Secret()).Scan(&nodes[i])
		if err != nil {
			t.Fatal(err)
		}
		nodeGrants[i] = testAuthorize(t, f.p, f.user, nodes[i])
	}
	var ingress, egress int64
	for _, group := range []*int64{&ingress, &egress} {
		if err := f.p.QueryRow(ctx, `INSERT INTO node_groups(name) VALUES($1) RETURNING id`, Secret()).Scan(group); err != nil {
			t.Fatal(err)
		}
		if _, err := f.p.Exec(ctx, `INSERT INTO user_node_groups(user_id,group_id) VALUES($1,$2)`, f.user, *group); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = f.p.Exec(ctx, `DELETE FROM rules WHERE node_id=ANY($1)`, nodes[:])
		_, _ = f.p.Exec(ctx, `DELETE FROM user_node_groups WHERE group_id=ANY($1)`, []int64{ingress, egress})
		_, _ = f.p.Exec(ctx, `DELETE FROM node_groups WHERE id=ANY($1)`, []int64{ingress, egress})
		_, _ = f.p.Exec(ctx, `DELETE FROM rule_usage WHERE node_id=ANY($1)`, nodes[:])
		_, _ = f.p.Exec(ctx, `DELETE FROM nodes WHERE id=ANY($1)`, nodes[:])
	})
	if _, err := f.p.Exec(ctx, `INSERT INTO node_group_members(group_id,node_id,role) VALUES($1,$3,'active'),($1,$4,'active'),($2,$5,'active'),($2,$6,'standby')`, ingress, egress, nodes[0], nodes[1], nodes[2], nodes[3]); err != nil {
		t.Fatal(err)
	}
	for _, entry := range nodes[:2] {
		for _, exit := range nodes[2:] {
			if _, err := f.p.Exec(ctx, `INSERT INTO node_tunnel_links(ingress_node_id,egress_node_id,token) VALUES($1,$2,$3)`, entry, exit, Secret()); err != nil {
				t.Fatal(err)
			}
		}
	}
	ids := make([]int64, 0, 5)
	for i, variant := range []struct {
		ig, en, eg any
		proto      string
	}{
		{nil, nil, nil, "tcp"}, {nil, nodes[2], nil, "tcp"}, {nil, nil, egress, "udp"}, {ingress, nodes[2], nil, "tcp"}, {ingress, nil, egress, "udp"},
	} {
		var id int64
		err := f.p.QueryRow(ctx, `INSERT INTO rules(user_id,node_id,ingress_group_id,egress_node_id,egress_group_id,protocol,listen_port,target_host,target_port,enabled) VALUES($1,$2,$3,$4,$5,$6,$7,'1.1.1.1',443,true) RETURNING id`, f.user, nodes[0], variant.ig, variant.en, variant.eg, variant.proto, 30001+i).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	var foreign int64
	if err := f.p.QueryRow(ctx, `INSERT INTO rules(user_id,node_id,listen_port,target_host,target_port) VALUES($1,$2,30100,'1.1.1.1',443) RETURNING id`, f.other, nodes[0]).Scan(&foreign); err != nil {
		t.Fatal(err)
	}
	ids = append(ids, foreign)
	legacy, err := os.ReadFile("testdata/rule_list_before_arch.sql")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(ruleListSQL, "WITH list_routes AS MATERIALIZED (")
	if start < 0 {
		t.Fatal("could not locate route CTE")
	}
	end := strings.Index(ruleListSQL[start:], "\n  )\n  SELECT COALESCE(")
	if end < 0 {
		t.Fatal("could not locate route definition")
	}
	routeCTE := ruleListSQL[start:start+end] + "\n  )\n"
	actualRoutes := `SELECT q.rule_id,q.ingress_node_id,q.egress_node_id FROM rules r CROSS JOIN LATERAL (` + routeCTE + `SELECT rule_id,ingress_node_id,egress_node_id FROM list_routes) q WHERE r.id=ANY($1) ORDER BY 1,2,3`
	collect := func(tx pgx.Tx, query string, args ...any) []string {
		t.Helper()
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			out = append(out, line)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	compare := func(label string) {
		t.Helper()
		tx, err := f.p.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, "SET LOCAL jit=off; SET LOCAL join_collapse_limit=1; SET LOCAL from_collapse_limit=1"); err != nil {
			t.Fatal(err)
		}
		before := collect(tx, `SELECT row_to_json(q)::text FROM (SELECT rule_id,ingress_node_id,egress_node_id FROM rule_outbound_routes WHERE rule_id=ANY($1) ORDER BY 1,2,3) q`, ids)
		after := collect(tx, `SELECT row_to_json(q)::text FROM (`+actualRoutes+`) q`, ids)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("%s routes differ: before=%v after=%v", label, before, after)
		}
		for _, admin := range []bool{false, true} {
			args := []any{admin, f.user, false, false, pb.EncryptedTunnelVersion, pb.MultiIngressVersion}
			b := collect(tx, `SELECT row_to_json(q)::text FROM (`+string(legacy)+`) q`, args...)
			a := collect(tx, `SELECT row_to_json(q)::text FROM (`+ruleListSQL+`) q`, args...)
			if !reflect.DeepEqual(b, a) {
				t.Fatalf("%s admin=%v responses differ:\nbefore=%v\nafter=%v", label, admin, b, a)
			}
			if !admin && len(a) != len(ids)-1 {
				t.Fatalf("%s user saw %d rules, want %d", label, len(a), len(ids)-1)
			}
		}
	}
	compare("initial")
	changes := []struct {
		name, sql string
		args      []any
	}{
		{"disabled ingress", `UPDATE nodes SET enabled=false WHERE id=$1`, []any{nodes[1]}},
		{"reenabled ingress", `UPDATE nodes SET enabled=true WHERE id=$1`, []any{nodes[1]}},
		{"offline and failed", `UPDATE nodes SET last_seen=now()-interval '1 hour',sync_error='failed' WHERE id=$1`, []any{nodes[2]}},
		{"disabled group member", `UPDATE node_group_members SET role='disabled' WHERE group_id=$1 AND node_id=$2`, []any{egress, nodes[3]}},
		{"udp-only tunnel", `UPDATE nodes SET tunnel_protocol='plain_udp' WHERE id=$1`, []any{nodes[2]}},
		{"link revoked", `DELETE FROM node_tunnel_links WHERE ingress_node_id=$1 AND egress_node_id=$2`, []any{nodes[0], nodes[2]}},
		{"exit group disabled", `UPDATE node_groups SET enabled=false WHERE id=$1`, []any{egress}},
		{"exit group enabled", `UPDATE node_groups SET enabled=true WHERE id=$1`, []any{egress}},
		{"exit group member removed", `DELETE FROM node_group_members WHERE group_id=$1 AND node_id=$2`, []any{egress, nodes[3]}},
		{"group authorization revoked", `DELETE FROM user_node_groups WHERE user_id=$1 AND group_id=$2`, []any{f.user, egress}},
		{"node authorization revoked", `DELETE FROM user_node_groups WHERE user_id=$1 AND group_id=$2`, []any{f.user, nodeGrants[2]}},
		{"ingress authorization revoked", `DELETE FROM user_node_groups WHERE user_id=$1 AND group_id=$2`, []any{f.user, ingress}},
		{"disabled plan", `UPDATE plans SET enabled=false WHERE id=$1`, []any{f.plan}},
	}
	for _, change := range changes {
		if _, err := f.p.Exec(ctx, change.sql, change.args...); err != nil {
			t.Fatal(fmt.Errorf("%s: %w", change.name, err))
		}
		compare(change.name)
	}
}
