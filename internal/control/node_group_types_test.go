package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func TestNodeGroupTypesAndConnectionAddress(t *testing.T) {
	f := newSecurityFixture(t)
	ctx := context.Background()
	var entry, exit int64
	if err := f.p.QueryRow(ctx, `INSERT INTO nodes(name,token_hash,protocol_version) VALUES($1,$2,37) RETURNING id`, Secret(), Secret()).Scan(&entry); err != nil {
		t.Fatal(err)
	}
	if err := f.p.QueryRow(ctx, `INSERT INTO nodes(name,token_hash,ingress_enabled,tunnel_exit_enabled,tunnel_public_host,tunnel_listen_port,protocol_version) VALUES($1,$2,false,true,'exit.example.test',24443,37) RETURNING id`, Secret(), Secret()).Scan(&exit); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.p.Exec(ctx, `DELETE FROM rule_usage WHERE node_id IN ($1,$2)`, entry, exit)
		_, _ = f.p.Exec(ctx, `DELETE FROM grants WHERE node_id IN ($1,$2)`, entry, exit)
		_, _ = f.p.Exec(ctx, `DELETE FROM nodes WHERE id IN ($1,$2)`, entry, exit)
	})
	testAuthorize(t, f.p, f.user, entry)
	testAuthorize(t, f.p, f.user, exit)
	if _, err := f.p.Exec(ctx, `INSERT INTO node_tunnel_links(ingress_node_id,egress_node_id,token) VALUES($1,$2,$3)`, entry, exit, Secret()); err != nil {
		t.Fatal(err)
	}
	address := "ingress.example.test"
	makeGroup := func(kind string, ids []int64, addr *string) (int64, nodeGroupInput) {
		t.Helper()
		v := nodeGroupInput{Name: Secret(), GroupType: kind, ConnectionAddress: addr, Enabled: true, NodeIDs: ids, Strategy: "round_robin", HealthIntervalSeconds: 5, HealthFailures: 3, HealthSuccesses: 2}
		w := f.request(f.admin, "admin/node-groups", "POST", v)
		if w.Code != 200 {
			t.Fatalf("create %s group: %d %s", kind, w.Code, w.Body.String())
		}
		var response struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.ID == 0 {
			t.Fatalf("invalid group response: %s %v", w.Body.String(), err)
		}
		id := response.ID
		t.Cleanup(func() {
			_, _ = f.p.Exec(ctx, `DELETE FROM rule_usage WHERE rule_id IN (SELECT id FROM rules WHERE ingress_group_id=$1 OR egress_group_id=$1)`, id)
			_, _ = f.p.Exec(ctx, `DELETE FROM rules WHERE ingress_group_id=$1 OR egress_group_id=$1`, id)
			_, _ = f.p.Exec(ctx, `DELETE FROM user_node_groups WHERE group_id=$1`, id)
			_, _ = f.p.Exec(ctx, `DELETE FROM node_groups WHERE id=$1`, id)
		})
		if _, err := f.p.Exec(ctx, `INSERT INTO user_node_groups(user_id,group_id) VALUES($1,$2)`, f.user, id); err != nil {
			t.Fatal(err)
		}
		return id, v
	}
	entryGroup, entryForm := makeGroup("ingress", []int64{entry}, &address)
	exitGroup, exitForm := makeGroup("egress", []int64{exit}, nil)
	legacyGroup, legacyForm := makeGroup("", []int64{entry, exit}, nil)
	var typ, storedAddress string
	if err := f.p.QueryRow(ctx, `SELECT group_type,connection_address FROM node_groups WHERE id=$1`, legacyGroup).Scan(&typ, &storedAddress); err != nil || typ != "legacy" || storedAddress != "" {
		t.Fatalf("legacy defaults: %q %q %v", typ, storedAddress, err)
	}
	for _, tc := range []struct{ path, contains, excludes string }{
		{"rule-ingress-groups", entryForm.Name, exitForm.Name},
		{"rule-egress-groups", exitForm.Name, entryForm.Name},
	} {
		w := f.request(f.user, tc.path, "GET", nil)
		if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(tc.contains)) || bytes.Contains(w.Body.Bytes(), []byte(tc.excludes)) || !bytes.Contains(w.Body.Bytes(), []byte(legacyForm.Name)) {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	invalid := entryForm
	for _, raw := range []string{"", "https://example.test", "example.test:443", "[::1]:80"} {
		invalid.ConnectionAddress = &raw
		securityStatus(t, f.request(f.admin, "admin/node-groups", "POST", invalid), 400)
	}
	for _, raw := range []string{"127.0.0.1", "2001:db8::1", "valid.example.test"} {
		invalid.ConnectionAddress = &raw
		securityStatus(t, f.request(f.admin, fmt.Sprintf("admin/node-groups/%d", entryGroup), "PUT", invalid), 200)
	}
	securityStatus(t, f.request(f.admin, fmt.Sprintf("admin/node-groups/%d", entryGroup), "PUT", entryForm), 200)
	input := RuleInput{IngressGroupID: entryGroup, Targets: []string{"1.1.1.1:443"}, Enabled: true}
	securityStatus(t, f.request(f.user, "rules", "POST", input), 200)
	input.IngressGroupID = exitGroup
	securityStatus(t, f.request(f.user, "rules", "POST", input), 400)
	input.IngressGroupID, input.NodeID, input.EgressGroupID = 0, entry, entryGroup
	securityStatus(t, f.request(f.user, "rules", "POST", input), 400)
	input.EgressGroupID = exitGroup
	securityStatus(t, f.request(f.user, "rules", "POST", input), 200)
	entryForm.GroupType = "egress"
	securityStatus(t, f.request(f.admin, fmt.Sprintf("admin/node-groups/%d", entryGroup), "PUT", entryForm), 409)
	exitForm.GroupType, exitForm.ConnectionAddress = "ingress", &address
	securityStatus(t, f.request(f.admin, fmt.Sprintf("admin/node-groups/%d", exitGroup), "PUT", exitForm), 409)
	w := f.request(f.user, "rules", "GET", nil)
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"ingress_connection_address":"ingress.example.test"`)) {
		t.Fatalf("rule response missing address: %d %s", w.Code, w.Body.String())
	}
}
