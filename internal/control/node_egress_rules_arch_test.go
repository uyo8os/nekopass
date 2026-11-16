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

// Compare complete rows with the previous rule_outbound_routes-based queries:
// authorization also affects tunnel credentials, limits, and group candidates.
// Update the reference queries in testdata and these comparisons whenever that
// view's routing semantics change.
func TestNodeEgressRulesQueryEquivalent(t *testing.T) {
	f := newSecurityFixture(t)
	ctx := context.Background()
	var nodes, nodeGrants [4]int64
	for i := range nodes {
		if err := f.p.QueryRow(ctx, `INSERT INTO nodes(name,token_hash,ingress_enabled,allow_direct,tunnel_exit_enabled,tunnel_public_host,tunnel_listen_port,protocol_version) VALUES($1,$2,true,true,true,'example.test',24443,40) RETURNING id`, Secret(), Secret()).Scan(&nodes[i]); err != nil {
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
	for i, variant := range []struct {
		user       int64
		ig, en, eg any
		proto      string
		enabled    bool
	}{
		{f.user, nil, nil, nil, "tcp", true}, {f.user, nil, nodes[2], nil, "tcp", true},
		{f.user, nil, nil, egress, "udp", true}, {f.user, ingress, nodes[2], nil, "tcp", true},
		{f.user, ingress, nil, egress, "udp", true}, {f.user, ingress, nil, nil, "tcp", false},
		{f.other, nil, nil, nil, "tcp", true},
	} {
		_, err := f.p.Exec(ctx, `INSERT INTO rules(user_id,node_id,ingress_group_id,egress_node_id,egress_group_id,protocol,listen_port,target_host,target_port,enabled) VALUES($1,$2,$3,$4,$5,$6,$7,'1.1.1.1',443,$8)`, variant.user, nodes[0], variant.ig, variant.en, variant.eg, variant.proto, 31001+i, variant.enabled)
		if err != nil {
			t.Fatal(err)
		}
	}
	oldSQL, err := os.ReadFile("testdata/node_egress_rules_before_arch.sql")
	if err != nil {
		t.Fatal(err)
	}
	oldGroupSQL, err := os.ReadFile("testdata/group_candidates_before_arch.sql")
	if err != nil {
		t.Fatal(err)
	}
	var groupRuleIDs []int64
	ids, err := f.p.Query(ctx, `SELECT id FROM rules WHERE egress_group_id=$1 ORDER BY id`, egress)
	if err != nil {
		t.Fatal(err)
	}
	for ids.Next() {
		var id int64
		if err := ids.Scan(&id); err != nil {
			t.Fatal(err)
		}
		groupRuleIDs = append(groupRuleIDs, id)
	}
	if err := ids.Err(); err != nil {
		t.Fatal(err)
	}
	ids.Close()
	columns := make([]string, 7)
	for i := range columns {
		columns[i] = fmt.Sprintf("c%d", i+1)
	}
	collect := func(tx pgx.Tx, query string, node int64, version int32) []string {
		t.Helper()
		rows, err := tx.Query(ctx, `SELECT row_to_json(q)::text FROM (`+query+`) AS q(`+strings.Join(columns, ",")+`)`, node, version, pb.MultiIngressVersion)
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
	collectGroups := func(tx pgx.Tx, query string, node int64) []string {
		t.Helper()
		rows, err := tx.Query(ctx, `SELECT row_to_json(q)::text FROM (`+query+`) AS q(c1,c2,c3,c4,c5,c6,c7,c8)`, groupRuleIDs, node, pb.MultiIngressVersion)
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
	changes := []struct {
		name, sql string
		args      []any
	}{
		{"unchanged", "", nil},
		{"disabled ingress", `UPDATE nodes SET ingress_enabled=false WHERE id=$1`, []any{nodes[0]}},
		{"disabled source node", `UPDATE nodes SET enabled=false WHERE id=$1`, []any{nodes[0]}},
		{"ingress role disabled", `UPDATE node_group_members SET role='disabled' WHERE group_id=$1 AND node_id=$2`, []any{ingress, nodes[0]}},
		{"direct disabled", `UPDATE nodes SET allow_direct=false WHERE id=$1`, []any{nodes[0]}},
		{"exit disabled", `UPDATE nodes SET enabled=false WHERE id=$1`, []any{nodes[2]}},
		{"exit tunnel disabled", `UPDATE nodes SET tunnel_exit_enabled=false WHERE id=$1`, []any{nodes[2]}},
		{"exit role disabled", `UPDATE node_group_members SET role='disabled' WHERE group_id=$1 AND node_id=$2`, []any{egress, nodes[2]}},
		{"exit group disabled", `UPDATE node_groups SET enabled=false WHERE id=$1`, []any{egress}},
		{"exit group revoked", `DELETE FROM user_node_groups WHERE user_id=$1 AND group_id=$2`, []any{f.user, egress}},
		{"exit node revoked", `DELETE FROM user_node_groups WHERE user_id=$1 AND group_id=$2`, []any{f.user, nodeGrants[2]}},
		{"all exit grants revoked", `DELETE FROM user_node_groups WHERE user_id=$1 AND group_id=ANY($2)`, []any{f.user, []int64{egress, nodeGrants[2]}}},
		{"ingress node revoked", `DELETE FROM user_node_groups WHERE user_id=$1 AND group_id=$2`, []any{f.user, nodeGrants[0]}},
		{"all ingress grants revoked", `DELETE FROM user_node_groups WHERE user_id=$1 AND group_id=ANY($2)`, []any{f.user, []int64{ingress, nodeGrants[0]}}},
		{"ingress group revoked", `DELETE FROM user_node_groups WHERE user_id=$1 AND group_id=$2`, []any{f.user, ingress}},
		{"ingress group disabled", `UPDATE node_groups SET enabled=false WHERE id=$1`, []any{ingress}},
		{"ingress member removed", `DELETE FROM node_group_members WHERE group_id=$1 AND node_id=$2`, []any{ingress, nodes[1]}},
		{"ingress member added", `INSERT INTO node_group_members(group_id,node_id) VALUES($1,$2)`, []any{ingress, nodes[2]}},
		{"tunnel link revoked", `DELETE FROM node_tunnel_links WHERE ingress_node_id=$1 AND egress_node_id=$2`, []any{nodes[0], nodes[2]}},
		{"udp-only tunnel", `UPDATE nodes SET tunnel_protocol='plain_udp' WHERE id=$1`, []any{nodes[2]}},
		{"old exit", `UPDATE nodes SET protocol_version=1 WHERE id=$1`, []any{nodes[2]}},
		{"disabled plan", `UPDATE plans SET enabled=false WHERE id=$1`, []any{f.plan}},
		{"disabled account", `UPDATE users SET enabled=false WHERE id=$1`, []any{f.user}},
		{"expired account", `UPDATE users SET resource_expires_at=now()-interval '1 second',subscription_managed=false WHERE id=$1`, []any{f.user}},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			tx, err := f.p.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if change.sql != "" {
				if _, err := tx.Exec(ctx, change.sql, change.args...); err != nil {
					t.Fatal(err)
				}
			}
			for _, node := range nodes {
				beforeGroup := collectGroups(tx, string(oldGroupSQL), node)
				afterGroup := collectGroups(tx, groupCandidatesSQL, node)
				if !reflect.DeepEqual(beforeGroup, afterGroup) {
					t.Fatalf("group candidates node=%d:\nbefore=%v\nafter=%v", node, beforeGroup, afterGroup)
				}
				if change.name == "unchanged" && node == nodes[0] && len(beforeGroup) < 3 {
					t.Fatalf("fixture did not produce expected group candidates: %v", beforeGroup)
				}
				for _, version := range []int32{1, pb.MultiIngressVersion, pb.CurrentVersion} {
					before := collect(tx, string(oldSQL), node, version)
					after := collect(tx, nodeEgressRulesSQL, node, version)
					if !reflect.DeepEqual(before, after) {
						t.Fatalf("node=%d version=%d:\nbefore=%v\nafter=%v", node, version, before, after)
					}
					if change.name == "unchanged" && node == nodes[2] && version >= pb.MultiIngressVersion && len(before) < 3 {
						t.Fatalf("fixture did not produce expected rules: %v", before)
					}
				}
			}
		})
	}
}
