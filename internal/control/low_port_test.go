package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRuleListenPortBounds(t *testing.T) {
	for _, tc := range []struct {
		port  int
		valid bool
	}{{-1, false}, {0, true}, {79, false}, {80, true}, {443, true}, {65535, true}, {65536, false}} {
		t.Run(fmt.Sprint(tc.port), func(t *testing.T) {
			in := RuleInput{NodeID: 1, ListenPort: tc.port, Targets: []string{"example.com:443"}}
			if err := in.normalize(); (err == nil) != tc.valid {
				t.Fatalf("port %d: got error %v, valid=%v", tc.port, err, tc.valid)
			}
		})
	}
}

func TestLowPortNodeAndRules(t *testing.T) {
	p := testDB(t)
	ctx := context.Background()
	adminID, _ := testUser(t, p, 1<<30, 3, true)
	userID, _ := testUser(t, p, 1<<30, 3, false)
	sessions := make(map[int64]string)
	for _, id := range []int64{adminID, userID} {
		sessions[id] = Secret()
		if _, err := p.Exec(ctx, "INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '1 hour')", Hash(sessions[id]), id); err != nil {
			t.Fatal(err)
		}
	}
	h := New(p).Handler(http.NotFoundHandler())
	req := func(actor int64, path, method string, body any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		r := httptest.NewRequest(method, "/api/v1/"+path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: "nekopass_session_http", Value: sessions[actor]})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	form := NodeInput{Name: Secret(), Enabled: true, IngressEnabled: true, AllowDirect: true, TunnelProtocol: "plain_tcp", TunnelListenHost: "0.0.0.0", ListenHost: "0.0.0.0", PortMin: 79, PortMax: 80, MaxConnections: 1000, DialTimeoutSeconds: 8, IdleTimeoutSeconds: 300, ProbeIntervalSeconds: 5, DiskPath: "/"}
	if w := req(adminID, "admin/nodes", "POST", form); w.Code != http.StatusBadRequest {
		t.Fatalf("port 79 accepted: %d %s", w.Code, w.Body.String())
	}
	form.PortMin = 80
	w := req(adminID, "admin/nodes", "POST", form)
	if w.Code != http.StatusOK {
		t.Fatalf("create node: %d %s", w.Code, w.Body.String())
	}
	var node struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &node); err != nil || node.ID == 0 {
		t.Fatalf("invalid node response: %s (%v)", w.Body.String(), err)
	}
	t.Cleanup(func() {
		_, _ = p.Exec(ctx, "DELETE FROM rule_usage WHERE node_id=$1", node.ID)
		_, _ = p.Exec(ctx, "DELETE FROM grants WHERE node_id=$1", node.ID)
		_, _ = p.Exec(ctx, "DELETE FROM rules WHERE node_id=$1", node.ID)
		_, _ = p.Exec(ctx, "DELETE FROM nodes WHERE id=$1", node.ID)
	})
	testAuthorize(t, p, userID, node.ID)
	form.PortMin = 79
	if w := req(adminID, fmt.Sprintf("admin/nodes/%d", node.ID), "PUT", form); w.Code != http.StatusBadRequest {
		t.Fatalf("update to port 79 accepted: %d %s", w.Code, w.Body.String())
	}
	input := RuleInput{NodeID: node.ID, Protocol: "tcp", ListenPort: 80, Targets: []string{"example.com:443"}, Enabled: true}
	if w := req(userID, "rules", "POST", input); w.Code != http.StatusOK {
		t.Fatalf("explicit port 80: %d %s", w.Code, w.Body.String())
	}
	input.Protocol, input.ListenPort = "udp", 0
	w = req(userID, "rules", "POST", input)
	if w.Code != http.StatusOK {
		t.Fatalf("automatic port 80: %d %s", w.Code, w.Body.String())
	}
	var allocated int
	if err := p.QueryRow(ctx, "SELECT listen_port FROM rules WHERE node_id=$1 AND protocol='udp'", node.ID).Scan(&allocated); err != nil || allocated != 80 {
		t.Fatalf("automatic port = %d, error = %v", allocated, err)
	}
}
