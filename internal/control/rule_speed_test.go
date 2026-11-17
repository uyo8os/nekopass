package control

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	pb "github.com/nekopass/nekopass/internal/protocol"
)

func TestRuleSpeedConfigurationAndNodeDelivery(t *testing.T) {
	f := newSecurityFixture(t)
	ctx := context.Background()
	var node int64
	if err := f.p.QueryRow(ctx, "INSERT INTO nodes(name,token_hash) VALUES($1,$2) RETURNING id", Secret(), Hash(Secret())).Scan(&node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range []string{"DELETE FROM grants WHERE node_id=$1", "DELETE FROM rule_usage WHERE node_id=$1", "DELETE FROM rules WHERE node_id=$1", "DELETE FROM nodes WHERE id=$1"} {
			_, _ = f.p.Exec(ctx, q, node)
		}
	})
	testAuthorize(t, f.p, f.user, node)
	if _, err := f.p.Exec(ctx, "UPDATE users SET rule_speed_bps=$2 WHERE id=$1", f.user, 20*125000); err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"node_id": node, "targets": []string{"127.0.0.1:8080"}, "enabled": true, "speed_mbps": 10}
	w := f.request(f.user, "rules", "POST", input)
	securityStatus(t, w, 200)
	var result struct{ ID int64 }
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	id := result.ID
	path := fmt.Sprintf("rules/%d", id)
	stream := &StreamServer{Server: f.s}
	report := &pb.AgentMessage{ProtocolVersion: pb.CurrentVersion, InstanceId: Secret()}
	check := func(configured, effective int64) {
		t.Helper()
		list := f.request(f.user, "rules", "GET", nil)
		securityStatus(t, list, 200)
		var rows []struct {
			ID         int64 `json:"id"`
			Configured int64 `json:"configured_speed_mbps"`
			Effective  int64 `json:"speed_mbps"`
		}
		if err := json.Unmarshal(list.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, row := range rows {
			if row.ID == id {
				found = true
				if row.Configured != configured || row.Effective != effective {
					t.Fatalf("list: got configured=%d effective=%d, want %d/%d", row.Configured, row.Effective, configured, effective)
				}
			}
		}
		if !found {
			t.Fatal("rule missing from list")
		}
		out, err := stream.exchange(ctx, f.p, node, report)
		if err != nil {
			t.Fatal(err)
		}
		for _, rule := range out.Rules {
			if rule.Id == id {
				if rule.SpeedBps != effective*125000 {
					t.Fatalf("node speed=%d, want %d", rule.SpeedBps, effective*125000)
				}
				return
			}
		}
		t.Fatal("rule missing from node configuration")
	}
	check(10, 10)
	delete(input, "speed_mbps")
	securityStatus(t, f.request(f.user, path, "PUT", input), 200)
	check(10, 10) // Old clients omitting speed retain the configured limit.
	input["speed_mbps"] = 30
	securityStatus(t, f.request(f.user, path, "PUT", input), 200)
	check(30, 20) // The user's limit still caps the rule.
	securityStatus(t, f.request(f.user, "rules/batch", "POST", map[string]any{"action": "edit", "ids": []int64{id}, "patch": map[string]any{"balance": "round_robin"}}), 200)
	check(30, 20) // Batch edits preserve the rule-specific value.
	input["speed_mbps"] = 0
	securityStatus(t, f.request(f.user, path, "PUT", input), 200)
	check(0, 20)
	if _, err := f.p.Exec(ctx, "UPDATE users SET rule_speed_bps=0 WHERE id=$1", f.user); err != nil {
		t.Fatal(err)
	}
	check(0, 0)
	input["speed_mbps"] = 100001
	securityStatus(t, f.request(f.user, path, "PUT", input), 400)
	input["speed_mbps"] = 7
	securityStatus(t, f.request(f.user, "rules/import", "POST", map[string]any{"rules": []any{input}}), 200)
	var count int
	if err := f.p.QueryRow(ctx, "SELECT count(*) FROM rules WHERE user_id=$1 AND speed_mbps=7", f.user).Scan(&count); err != nil || count != 1 {
		t.Fatalf("import did not preserve speed: count=%d err=%v", count, err)
	}
}
