package control

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	pb "github.com/nekopass/nekopass/internal/protocol"
)

func TestIngressGroupAdmissionDistributionAndMembership(t *testing.T) {
	f := newSecurityFixture(t)
	ctx := context.Background()
	var nodes []int64
	for i := 0; i < 3; i++ {
		var id int64
		if err := f.p.QueryRow(ctx, `INSERT INTO nodes(name,token_hash,port_min,port_max,allow_direct,protocol_version) VALUES($1,$2,24000,24001,$3,35) RETURNING id`, Secret(), Secret(), i != 2).Scan(&id); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, id)
		testAuthorize(t, f.p, f.user, id)
	}
	var group int64
	if err := f.p.QueryRow(ctx, `INSERT INTO node_groups(name) VALUES($1) RETURNING id`, Secret()).Scan(&group); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.p.Exec(ctx, `DELETE FROM rules WHERE ingress_group_id=$1 OR node_id=ANY($2)`, group, nodes)
		_, _ = f.p.Exec(ctx, `DELETE FROM user_node_groups WHERE group_id=$1`, group)
		_, _ = f.p.Exec(ctx, `DELETE FROM node_groups WHERE id=$1`, group)
		_, _ = f.p.Exec(ctx, `DELETE FROM rule_usage WHERE node_id=ANY($1)`, nodes)
		_, _ = f.p.Exec(ctx, `DELETE FROM grants WHERE node_id=ANY($1)`, nodes)
		_, _ = f.p.Exec(ctx, `DELETE FROM nodes WHERE id=ANY($1)`, nodes)
	})
	if _, err := f.p.Exec(ctx, `INSERT INTO node_group_members(group_id,node_id) VALUES($1,$2),($1,$3)`, group, nodes[0], nodes[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.Exec(ctx, `INSERT INTO user_node_groups(user_id,group_id) VALUES($1,$2)`, f.user, group); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.Exec(ctx, `UPDATE users SET max_rules=2 WHERE id=$1`, f.user); err != nil {
		t.Fatal(err)
	}
	occupied := RuleInput{NodeID: nodes[0], ListenPort: 24000, Targets: []string{"1.1.1.1:443"}, Enabled: true}
	securityStatus(t, f.request(f.user, "rules", "POST", occupied), 200)
	in := RuleInput{IngressGroupID: group, Targets: []string{"1.1.1.1:443"}, Enabled: true}
	securityStatus(t, f.request(f.other, "rules", "POST", in), 400)
	w := f.request(f.user, "rules", "POST", in)
	if w.Code != 200 {
		t.Fatalf("group rule: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var port, reserved, usage int
	if err := f.p.QueryRow(ctx, `SELECT listen_port,(SELECT count(*) FROM rule_ports WHERE rule_id=r.id),(SELECT count(*) FROM rule_usage WHERE rule_id=r.id) FROM rules r WHERE id=$1`, result.ID).Scan(&port, &reserved, &usage); err != nil {
		t.Fatal(err)
	}
	if port != 24001 || reserved != 2 || usage != 2 {
		t.Fatalf("port %d reservations %d usage %d", port, reserved, usage)
	}
	securityStatus(t, f.request(f.user, "rules", "POST", in), 400)
	balance := "round_robin"
	securityStatus(t, f.request(f.user, "rules/batch", "POST", BatchInput{Action: "edit", IDs: []int64{result.ID}, Patch: &RuleBatchPatch{Balance: &balance}}), 200)
	var storedGroup int64
	if err := f.p.QueryRow(ctx, `SELECT COALESCE(ingress_group_id,0) FROM rules WHERE id=$1`, result.ID).Scan(&storedGroup); err != nil || storedGroup != group {
		t.Fatal("batch lost group", err, storedGroup)
	}
	stream := &StreamServer{Server: f.s}
	instances := map[int64]string{}
	exchange := func(node int64) *pb.ControlMessage {
		t.Helper()
		if instances[node] == "" {
			instances[node] = Secret()
		}
		c, err := stream.exchange(ctx, f.p, node, &pb.AgentMessage{ProtocolVersion: 35, InstanceId: instances[node]})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	hasRule := func(c *pb.ControlMessage) bool {
		for _, r := range c.Rules {
			if r.Id == result.ID && r.Enabled {
				return true
			}
		}
		return false
	}
	if !hasRule(exchange(nodes[0])) || !hasRule(exchange(nodes[1])) || hasRule(exchange(nodes[2])) {
		t.Fatal("wrong initial ingress distribution")
	}
	// A disabled plan revokes user_nodes even if the explicit group grant remains.
	if _, err := f.p.Exec(ctx, `UPDATE plans SET enabled=false WHERE id=$1`, f.plan); err != nil {
		t.Fatal(err)
	}
	w = f.request(f.admin, fmt.Sprintf("admin/users/%d/rules", f.user), "GET", nil)
	securityStatus(t, w, 200)
	var listed []struct {
		ID           int64 `json:"id"`
		IngressNodes []struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"ingress_nodes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rule := range listed {
		if rule.ID != result.ID {
			continue
		}
		for _, node := range rule.IngressNodes {
			if node.ID == nodes[0] {
				found = true
				if node.Status != "unauthorized" {
					t.Fatalf("revoked member status = %q", node.Status)
				}
			}
		}
	}
	if !found {
		t.Fatal("group member missing from rules response")
	}
	if _, err := f.p.Exec(ctx, `UPDATE plans SET enabled=true WHERE id=$1`, f.plan); err != nil {
		t.Fatal(err)
	}
	groupInput := nodeGroupInput{Name: Secret(), Enabled: true, NodeIDs: nodes, Strategy: "round_robin", HealthIntervalSeconds: 5, HealthFailures: 3, HealthSuccesses: 2}
	securityStatus(t, f.request(f.admin, fmt.Sprintf("admin/node-groups/%d", group), "PUT", groupInput), 409)
	if _, err := f.p.Exec(ctx, `UPDATE nodes SET allow_direct=true WHERE id=$1`, nodes[2]); err != nil {
		t.Fatal(err)
	}
	w = f.request(f.admin, fmt.Sprintf("admin/node-groups/%d", group), "PUT", groupInput)
	if w.Code != 200 {
		t.Fatalf("add member: %d %s", w.Code, w.Body.String())
	}
	if !hasRule(exchange(nodes[2])) {
		t.Fatal("new member did not receive rule")
	}
	groupInput.NodeIDs = nodes[1:]
	securityStatus(t, f.request(f.admin, fmt.Sprintf("admin/node-groups/%d", group), "PUT", groupInput), 200)
	if hasRule(exchange(nodes[0])) {
		t.Fatal("removed member still receives group rule")
	}
	var oldUsage bool
	if err := f.p.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM rule_usage WHERE node_id=$1 AND rule_id=$2)`, nodes[0], result.ID).Scan(&oldUsage); err != nil || !oldUsage {
		t.Fatal("removed member usage was lost", err)
	}
	if _, err := f.p.Exec(ctx, `DELETE FROM user_node_groups WHERE user_id=$1 AND group_id=$2`, f.user, group); err != nil {
		t.Fatal(err)
	}
	if hasRule(exchange(nodes[1])) {
		t.Fatal("group rule remained active after group authorization revoked")
	}
}

func TestIngressGroupTunnelRoutesAreIsolatedAndRequireUpdatedExit(t *testing.T) {
	f := newSecurityFixture(t)
	ctx := context.Background()
	var entry [2]int64
	for i := range entry {
		if err := f.p.QueryRow(ctx, `INSERT INTO nodes(name,token_hash,protocol_version) VALUES($1,$2,37) RETURNING id`, Secret(), Secret()).Scan(&entry[i]); err != nil {
			t.Fatal(err)
		}
	}
	var exit, group int64
	if err := f.p.QueryRow(ctx, `INSERT INTO nodes(name,token_hash,tunnel_exit_enabled,tunnel_public_host,tunnel_listen_port,protocol_version) VALUES($1,$2,true,'exit.example.test',24443,37) RETURNING id`, Secret(), Secret()).Scan(&exit); err != nil {
		t.Fatal(err)
	}
	testAuthorize(t, f.p, f.user, exit)
	if err := f.p.QueryRow(ctx, `INSERT INTO node_groups(name) VALUES($1) RETURNING id`, Secret()).Scan(&group); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.p.Exec(ctx, `DELETE FROM rules WHERE ingress_group_id=$1`, group)
		_, _ = f.p.Exec(ctx, `DELETE FROM user_node_groups WHERE group_id=$1`, group)
		_, _ = f.p.Exec(ctx, `DELETE FROM node_groups WHERE id=$1`, group)
		_, _ = f.p.Exec(ctx, `DELETE FROM grants WHERE node_id=ANY($1)`, []int64{entry[0], entry[1], exit})
		_, _ = f.p.Exec(ctx, `DELETE FROM rule_usage WHERE node_id=ANY($1)`, []int64{entry[0], entry[1], exit})
		_, _ = f.p.Exec(ctx, `DELETE FROM nodes WHERE id=ANY($1)`, []int64{entry[0], entry[1], exit})
	})
	if _, err := f.p.Exec(ctx, `INSERT INTO node_group_members(group_id,node_id) VALUES($1,$2),($1,$3)`, group, entry[0], entry[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.Exec(ctx, `INSERT INTO user_node_groups(user_id,group_id) VALUES($1,$2)`, f.user, group); err != nil {
		t.Fatal(err)
	}
	for _, n := range entry {
		if _, err := f.p.Exec(ctx, `INSERT INTO node_tunnel_links(ingress_node_id,egress_node_id,token) VALUES($1,$2,$3)`, n, exit, Secret()); err != nil {
			t.Fatal(err)
		}
	}
	w := f.request(f.user, "rules", "POST", RuleInput{IngressGroupID: group, EgressNodeID: exit, Targets: []string{"1.1.1.1:443"}, Enabled: true})
	if w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	stream := &StreamServer{Server: f.s}
	instances := map[int64]string{}
	exchange := func(node int64, version int32) *pb.ControlMessage {
		t.Helper()
		if instances[node] == "" {
			instances[node] = Secret()
		}
		c, err := stream.exchange(ctx, f.p, node, &pb.AgentMessage{ProtocolVersion: version, InstanceId: instances[node]})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	for _, n := range entry {
		c := exchange(n, 37)
		if len(c.Rules) != 1 || c.Rules[0].Id != result.ID || !c.Rules[0].Enabled {
			t.Fatal("missing ingress rule", n, c.Rules)
		}
	}
	c := exchange(exit, 37)
	seen := map[int64]bool{}
	for _, r := range c.EgressRules {
		if r.RuleId == result.ID {
			seen[r.IngressNodeId] = true
		}
	}
	if !seen[entry[0]] || !seen[entry[1]] || len(seen) != 2 {
		t.Fatal("exit rules not isolated by source", c.EgressRules)
	}
	if _, err := f.p.Exec(ctx, `DELETE FROM node_tunnel_links WHERE ingress_node_id=$1 AND egress_node_id=$2`, entry[0], exit); err != nil {
		t.Fatal(err)
	}
	c = exchange(exit, 37)
	if len(c.EgressRules) != 1 || c.EgressRules[0].IngressNodeId != entry[1] {
		t.Fatal("removed route still allowed", c.EgressRules)
	}
	if c = exchange(entry[0], 37); len(c.Rules) != 1 || c.Rules[0].Enabled {
		t.Fatal("unlinked entry remained active", c.Rules)
	}
	c = exchange(exit, 36)
	if len(c.EgressRules) != 0 {
		t.Fatal("old exit received group ingress authorization", c.EgressRules)
	}
}
