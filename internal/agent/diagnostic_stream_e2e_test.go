package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nekopass/nekopass/internal/control"
	pb "github.com/nekopass/nekopass/internal/protocol"
	"github.com/nekopass/nekopass/internal/store"
	"google.golang.org/grpc"
)

// This covers the real HTTP request -> StreamServer.Connect -> runSession ->
// target probe -> next heartbeat -> HTTP response path using an isolated DB.
func TestDiagnosticRealAgentControlStream(t *testing.T) {
	dsn := os.Getenv("NEKOPASS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires isolated test database")
	}
	parsed, err := pgxpool.ParseConfig(dsn)
	if err != nil || !strings.HasSuffix(parsed.ConnConfig.Database, "_test") {
		t.Fatal("requires *_test database")
	}
	ctx := context.Background()
	pool, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE site_settings SET config=config||'{"target_deny_cidrs":[]}'::jsonb WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	var plan, user, node, group int64
	if err := pool.QueryRow(ctx, `INSERT INTO plans(name,speed_bps,quota_bytes,max_rules,max_connections) VALUES($1,62500000,1048576,20,1000) RETURNING id`, control.Secret()).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO users(username,password_hash,plan_id) VALUES($1,'unused',$2) RETURNING id`, control.Secret(), plan).Scan(&user); err != nil {
		t.Fatal(err)
	}
	key := control.Secret()
	if err := pool.QueryRow(ctx, `INSERT INTO nodes(name,token_hash,token,allow_direct,public_address) VALUES($1,$2,$3,true,'127.0.0.1') RETURNING id`, control.Secret(), control.Hash(key), key).Scan(&node); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO node_groups(name) VALUES($1) RETURNING id`, control.Secret()).Scan(&group); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{`INSERT INTO node_group_members(group_id,node_id) VALUES($1,$2)`, `INSERT INTO plan_node_groups(group_id,plan_id) VALUES($1,$3)`, `INSERT INTO user_node_groups(group_id,user_id) VALUES($1,$4)`} {
		// Supply only the placeholders used in each statement.
		var args []any
		switch {
		case strings.Contains(statement, "$4"):
			statement = strings.ReplaceAll(statement, "$4", "$2")
			args = []any{group, user}
		case strings.Contains(statement, "$3"):
			statement = strings.ReplaceAll(statement, "$3", "$2")
			args = []any{group, plan}
		default:
			args = []any{group, node}
		}
		if _, err := pool.Exec(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	session := control.Secret()
	if _, err := pool.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '1 hour')`, control.Hash(session), user); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM sessions WHERE user_id=$1`, `DELETE FROM grants WHERE user_id=$1`, `DELETE FROM rule_usage WHERE user_id=$1`, `DELETE FROM rules WHERE user_id=$1`, `DELETE FROM user_node_groups WHERE user_id=$1`, `DELETE FROM users WHERE id=$1`} {
			_, _ = pool.Exec(context.Background(), q, user)
		}
		_, _ = pool.Exec(context.Background(), `DELETE FROM plan_node_groups WHERE group_id=$1`, group)
		_, _ = pool.Exec(context.Background(), `DELETE FROM node_groups WHERE id=$1`, group)
		_, _ = pool.Exec(context.Background(), `DELETE FROM nodes WHERE id=$1`, node)
		_, _ = pool.Exec(context.Background(), `DELETE FROM plans WHERE id=$1`, plan)
	})
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unreachable := closed.Addr().String()
	closed.Close()
	s := control.New(pool)
	h := s.Handler(http.NotFoundHandler())
	request := func(path, method string, value any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(value)
		r := httptest.NewRequest(method, "http://panel.test/api/v1/"+path, bytes.NewReader(b))
		r.RemoteAddr = "198.51.100.1:1234"
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: "nekopass_session_http", Value: session})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := request("rules", "POST", control.RuleInput{NodeID: node, Name: "diagnostic e2e", Protocol: "tcp", Enabled: true, Targets: []string{target.Addr().String(), unreachable}})
	if w.Code != 200 {
		t.Fatalf("create rule: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	listen, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	pb.RegisterControlServer(grpcServer, &control.StreamServer{Server: s})
	go grpcServer.Serve(listen)
	defer grpcServer.Stop()
	state, err := OpenState(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	engine, err := NewEngine(state, 100)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- Run(runCtx, engine, "http://"+listen.Addr().String(), key) }()
	defer func() { cancel(); <-done; engine.Close() }()
	deadline := time.Now().Add(12 * time.Second)
	for {
		var ready bool
		err := pool.QueryRow(ctx, `SELECT n.last_seen IS NOT NULL AND n.protocol_version >= $3 AND n.applied_revision >= r.config_revision AND n.sync_error='' FROM nodes n JOIN rules r ON r.id=$2 WHERE n.id=$1`, node, created.ID, pb.DiagnosticVersion).Scan(&ready)
		if err != nil {
			t.Fatal(err)
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent failed to synchronize rule")
		}
		time.Sleep(50 * time.Millisecond)
	}
	w = request(fmt.Sprintf("rules/%d/diagnose", created.ID), "POST", nil)
	if w.Code != 200 {
		t.Fatalf("diagnose: %d %s", w.Code, w.Body.String())
	}
	var output struct {
		Links []struct {
			Status  string `json:"status"`
			Latency *int64 `json:"latency_ms"`
		} `json:"links"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if len(output.Links) != 2 || output.Links[0].Status != "reachable" || output.Links[0].Latency == nil || output.Links[1].Status != "unreachable" {
		t.Fatalf("real agent results: %s", w.Body.String())
	}
	saved, err := state.Config()
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.DiagnosticTasks) != 0 {
		t.Fatal("ephemeral diagnostic task persisted")
	}
	var traffic int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE((SELECT traffic FROM rule_usage WHERE rule_id=$1),0)`, created.ID).Scan(&traffic); err != nil || traffic != 0 {
		t.Fatalf("diagnostic charged traffic: %d %v", traffic, err)
	}
}
