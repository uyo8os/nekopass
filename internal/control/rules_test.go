package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRuleWorkflowAndOwnership(t *testing.T) {
	p := testDB(t)
	ctx := context.Background()
	s := New(p)
	h := s.Handler(http.NotFoundHandler())
	owner, _ := testUser(t, p, 1<<30, 3, false)
	other, _ := testUser(t, p, 1<<30, 3, false)
	var node int64
	if e := p.QueryRow(ctx, "INSERT INTO nodes(name,token_hash,port_min,port_max) VALUES($1,$2,20000,59999) RETURNING id", Secret(), Secret()).Scan(&node); e != nil {
		t.Fatal(e)
	}
	testAuthorize(t, p, owner, node)
	var e error
	token := Secret()
	_, e = p.Exec(ctx, "INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '1 hour')", Hash(token), owner)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		for _, q := range []string{"DELETE FROM sessions WHERE user_id=$1", "DELETE FROM rules WHERE user_id=$1", "DELETE FROM rule_groups WHERE user_id=$1", "DELETE FROM rule_usage WHERE user_id=$1", "DELETE FROM users WHERE id=$1"} {
			_, _ = p.Exec(ctx, q, owner)
			_, _ = p.Exec(ctx, q, other)
		}
		_, _ = p.Exec(ctx, "DELETE FROM nodes WHERE id=$1", node)
	}()
	req := func(path, method string, data any) *httptest.ResponseRecorder {
		body, _ := json.Marshal(data)
		r := httptest.NewRequest(method, "/api/v1/"+path, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: "nekopass_session_http", Value: token})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	getID := func(w *httptest.ResponseRecorder) int64 {
		t.Helper()
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		var v struct {
			ID int64 `json:"id"`
		}
		json.Unmarshal(w.Body.Bytes(), &v)
		return v.ID
	}
	gid := getID(req("rule-groups", "POST", map[string]any{"name": "my group", "user_id": other}))
	in := RuleInput{UserID: other, NodeID: node, Name: "round robin", GroupID: gid, Targets: []string{"127.0.0.1:1234", "[::1]:4321"}, Balance: "round_robin", Enabled: true, SpeedMbps: 0}
	id := getID(req("rules", "POST", in))
	var actualOwner int64
	var port int
	if e = p.QueryRow(ctx, "SELECT user_id,listen_port FROM rules WHERE id=$1", id).Scan(&actualOwner, &port); e != nil {
		t.Fatal(e)
	}
	if actualOwner != owner || port < 20000 || port > 59999 {
		t.Fatal("ownership or random port")
	}
	var foreignGroup, foreignRule int64
	p.QueryRow(ctx, "INSERT INTO rule_groups(user_id,name) VALUES($1,'private') RETURNING id", other).Scan(&foreignGroup)
	p.QueryRow(ctx, "INSERT INTO rules(user_id,node_id,listen_port,target_host,target_port) VALUES($1,$2,19999,'127.0.0.1',80) RETURNING id", other, node).Scan(&foreignRule)
	if w := req("rules", "GET", nil); w.Code != 200 {
		t.Fatalf("list rules: %d %s", w.Code, w.Body.String())
	} else {
		var listed []struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil || len(listed) != 1 || listed[0].ID != id {
			t.Fatalf("rule list exposed a foreign rule or omitted the owner's rule: %s (%v)", w.Body.String(), err)
		}
	}
	if w := req("rules/batch", "POST", BatchInput{Action: "delete", IDs: []int64{id, foreignRule}}); w.Code != 403 {
		t.Fatal("cross-user batch accepted", w.Code)
	}
	if w := req("rules/batch", "POST", BatchInput{Action: "group", IDs: []int64{id}, GroupID: foreignGroup}); w.Code != 400 {
		t.Fatal("cross-user group accepted")
	}
	in.GroupID = foreignGroup
	if w := req("rules", "POST", in); w.Code != 400 {
		t.Fatal("foreign group accepted")
	}
	in.GroupID = gid
	bad := in
	bad.Targets = []string{"invalid"}
	if w := req("rules/import", "POST", map[string]any{"rules": []RuleInput{in, bad}}); w.Code != 400 {
		t.Fatal("invalid import accepted")
	}
	var count int
	p.QueryRow(ctx, "SELECT count(*) FROM rules WHERE user_id=$1", owner).Scan(&count)
	if count != 1 {
		t.Fatal("partial import persisted")
	}
	if _, e = p.Exec(ctx, "UPDATE rule_usage SET traffic=1234 WHERE rule_id=$1", id); e != nil {
		t.Fatal(e)
	}
	if w := req("rules/batch", "POST", BatchInput{Action: "clear", IDs: []int64{id}}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var raw, baseline int64
	p.QueryRow(ctx, "SELECT traffic FROM rule_usage WHERE rule_id=$1", id).Scan(&raw)
	p.QueryRow(ctx, "SELECT traffic_baseline FROM rules WHERE id=$1", id).Scan(&baseline)
	if raw != 1234 || baseline != 1234 {
		t.Fatal("clear destroyed accounting")
	}
	if w := req("announcement", "PUT", map[string]string{"content": "unauthorized"}); w.Code != 403 {
		t.Fatal("announcement privilege escalation")
	}
	if w := req("rules/batch", "POST", BatchInput{Action: "disable", IDs: []int64{id}}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := req("rules/batch", "POST", BatchInput{Action: "enable", IDs: []int64{id}}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}

// The setting is scoped to this query's read-only transaction, even when the
// transaction fails or the HTTP request is canceled. Reusing one connection
// ensures this checks the actual pooled session, not another connection.
func TestRuleListJITIsTransactionLocal(t *testing.T) {
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
	var pid int
	if err = pool.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	s := &Server{Pool: pool}
	checkRestored := func() {
		t.Helper()
		var actualPID int
		var jit, collapse string
		if err := pool.QueryRow(ctx, "SELECT pg_backend_pid(), current_setting('jit'), current_setting('join_collapse_limit')").Scan(&actualPID, &jit, &collapse); err != nil {
			t.Fatal(err)
		}
		if actualPID != pid || jit != "on" || collapse != "8" {
			t.Fatalf("pooled session changed or settings leaked: pid=%d jit=%s join_collapse_limit=%s", actualPID, jit, collapse)
		}
	}
	for _, tc := range []string{"normal", "error", "canceled"} {
		t.Run(tc, func(t *testing.T) {
			requestCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			tx, err := s.beginRuleListTx(requestCtx)
			if err != nil {
				t.Fatal(err)
			}
			var jit, readOnly, collapse string
			if err = tx.QueryRow(requestCtx, "SELECT current_setting('jit'), current_setting('transaction_read_only'), current_setting('join_collapse_limit')").Scan(&jit, &readOnly, &collapse); err != nil {
				t.Fatal(err)
			}
			if jit != "off" || readOnly != "on" || collapse != "1" {
				t.Fatalf("jit=%s read_only=%s join_collapse_limit=%s", jit, readOnly, collapse)
			}
			if tc == "error" {
				if _, err = tx.Exec(requestCtx, "CREATE TEMP TABLE rule_list_write_forbidden (id int)"); err == nil {
					t.Fatal("read-only transaction allowed a write")
				}
			}
			if tc == "canceled" {
				cancel()
			}
			rollbackRuleListTx(tx)
			checkRestored()
		})
	}
}
