package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	pb "github.com/nekopass/nekopass/internal/protocol"
)

func TestDiagnoseRuleAuthorizationAndOffline(t *testing.T) {
	f := newSecurityFixture(t)
	ctx := context.Background()
	var node int64
	if err := f.p.QueryRow(ctx, `INSERT INTO nodes(name,token_hash,allow_direct,public_address) VALUES($1,$2,true,'192.0.2.8') RETURNING id`, Secret(), Secret()).Scan(&node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.p.Exec(ctx, `DELETE FROM rules WHERE node_id=$1`, node)
		_, _ = f.p.Exec(ctx, `DELETE FROM nodes WHERE id=$1`, node)
	})
	testAuthorize(t, f.p, f.user, node)
	w := f.request(f.user, "rules", "POST", RuleInput{NodeID: node, Targets: []string{"example.com:443"}, Enabled: true})
	securityStatus(t, w, 200)
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("rules/%d/diagnose", created.ID)
	securityStatus(t, f.request(f.other, path, "POST", nil), 404)
	securityStatus(t, f.request(f.admin, path, "POST", nil), 404)
	securityStatus(t, f.request(f.other, fmt.Sprintf("admin/users/%d/%s", f.user, path), "POST", nil), 403)
	w = f.request(f.user, path, "POST", nil)
	securityStatus(t, w, 200)
	var result diagnosticOutput
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Mode != "direct" || len(result.Links) != 1 || result.Links[0].Status != "not_checked" || result.Links[0].Reason != "节点离线" {
		t.Fatalf("unexpected result: %+v", result)
	}
	securityStatus(t, f.request(f.user, path, "POST", nil), 429)
	f.s.diagnostics.mu.Lock()
	delete(f.s.diagnostics.last, f.user)
	f.s.diagnostics.mu.Unlock()
	securityStatus(t, f.request(f.admin, fmt.Sprintf("admin/users/%d/%s", f.user, path), "POST", nil), 200)
}

func TestDiagnoseRuleDispatchesOnlySavedTargets(t *testing.T) {
	f := newSecurityFixture(t)
	ctx := context.Background()
	var node int64
	if err := f.p.QueryRow(ctx, `INSERT INTO nodes(name,token_hash,allow_direct,public_address,protocol_version,last_seen,applied_revision) VALUES($1,$2,true,'192.0.2.8',$3,now(),9223372036854775807) RETURNING id`, Secret(), Secret(), pb.DiagnosticVersion).Scan(&node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.p.Exec(ctx, `DELETE FROM rules WHERE node_id=$1`, node)
		_, _ = f.p.Exec(ctx, `DELETE FROM nodes WHERE id=$1`, node)
	})
	testAuthorize(t, f.p, f.user, node)
	w := f.request(f.user, "rules", "POST", RuleInput{NodeID: node, Targets: []string{"example.com:443"}, Enabled: true})
	securityStatus(t, w, 200)
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() { response <- f.request(f.user, fmt.Sprintf("rules/%d/diagnose", created.ID), "POST", nil) }()
	var msg *pb.ControlMessage
	deadline := time.After(5 * time.Second)
	for {
		msg = &pb.ControlMessage{}
		f.s.diagnosticExchange(node, &pb.AgentMessage{ProtocolVersion: pb.DiagnosticVersion}, msg)
		if len(msg.DiagnosticTasks) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("task not dispatched")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if len(msg.DiagnosticTasks) != 1 || msg.DiagnosticTasks[0].RuleId != created.ID || msg.DiagnosticTasks[0].Kind != "target" || msg.DiagnosticTasks[0].TargetIndex != 0 {
		t.Fatalf("unexpected task: %+v", msg.DiagnosticTasks)
	}
	f.s.diagnosticExchange(node, &pb.AgentMessage{ProtocolVersion: pb.DiagnosticVersion, DiagnosticResults: []*pb.DiagnosticResult{{TaskId: msg.DiagnosticTasks[0].TaskId, Status: "reachable", LatencyMs: 25}}}, &pb.ControlMessage{})
	select {
	case got := <-response:
		securityStatus(t, got, 200)
		var result diagnosticOutput
		if err := json.Unmarshal(got.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Links) != 1 || result.Links[0].Status != "reachable" || result.Links[0].LatencyMS == nil || *result.Links[0].LatencyMS != 25 {
			t.Fatalf("unexpected result: %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("diagnosis did not complete")
	}
}

func TestDiagnoseRelayAndMissingRoute(t *testing.T) {
	f := newSecurityFixture(t)
	ctx := context.Background()
	var entry, exit int64
	if err := f.p.QueryRow(ctx, `INSERT INTO nodes(name,token_hash,protocol_version,last_seen,applied_revision) VALUES($1,$2,$3,now(),9223372036854775807) RETURNING id`, Secret(), Secret(), pb.DiagnosticVersion).Scan(&entry); err != nil {
		t.Fatal(err)
	}
	if err := f.p.QueryRow(ctx, `INSERT INTO nodes(name,token_hash,tunnel_exit_enabled,tunnel_listen_port,tunnel_public_host,protocol_version,last_seen,applied_revision) VALUES($1,$2,true,32134,'127.0.0.1',$3,now(),9223372036854775807) RETURNING id`, Secret(), Secret(), pb.DiagnosticVersion).Scan(&exit); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.p.Exec(ctx, `DELETE FROM rules WHERE node_id=$1`, entry)
		_, _ = f.p.Exec(ctx, `DELETE FROM nodes WHERE id=ANY($1::bigint[])`, []int64{entry, exit})
	})
	testAuthorize(t, f.p, f.user, entry)
	testAuthorize(t, f.p, f.user, exit)
	if _, err := f.p.Exec(ctx, `INSERT INTO node_tunnel_links(ingress_node_id,egress_node_id,token) VALUES($1,$2,$3)`, entry, exit, Secret()); err != nil {
		t.Fatal(err)
	}
	w := f.request(f.user, "rules", "POST", RuleInput{NodeID: entry, EgressNodeID: exit, Targets: []string{"example.com:443"}, Enabled: true})
	securityStatus(t, w, 200)
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("rules/%d/diagnose", created.ID)
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() { response <- f.request(f.user, path, "POST", nil) }()
	for _, node := range []int64{entry, exit} {
		deadline := time.After(5 * time.Second)
		for {
			msg := &pb.ControlMessage{}
			f.s.diagnosticExchange(node, &pb.AgentMessage{ProtocolVersion: pb.DiagnosticVersion}, msg)
			if len(msg.DiagnosticTasks) > 0 {
				task := msg.DiagnosticTasks[0]
				if task.EgressNodeId != exit || (node == entry && task.Kind != "tunnel") || (node == exit && (task.Kind != "target" || task.IngressNodeId != entry)) {
					t.Fatalf("unexpected node %d task: %+v", node, task)
				}
				f.s.diagnosticExchange(node, &pb.AgentMessage{ProtocolVersion: pb.DiagnosticVersion, DiagnosticResults: []*pb.DiagnosticResult{{TaskId: task.TaskId, Status: "reachable", LatencyMs: 7}}}, &pb.ControlMessage{})
				break
			}
			select {
			case <-deadline:
				t.Fatal("relay task not dispatched")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	select {
	case got := <-response:
		securityStatus(t, got, 200)
		var result diagnosticOutput
		if err := json.Unmarshal(got.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Mode != "relay" || len(result.Links) != 2 || result.Links[0].Status != "reachable" || result.Links[1].Status != "reachable" {
			t.Fatalf("unexpected relay result: %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("relay diagnosis did not complete")
	}
	if _, err := f.p.Exec(ctx, `DELETE FROM node_tunnel_links WHERE ingress_node_id=$1 AND egress_node_id=$2`, entry, exit); err != nil {
		t.Fatal(err)
	}
	f.s.diagnostics.mu.Lock()
	delete(f.s.diagnostics.last, f.user)
	f.s.diagnostics.mu.Unlock()
	w = f.request(f.user, path, "POST", nil)
	securityStatus(t, w, 200)
	var result diagnosticOutput
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Mode != "relay" || len(result.Links) != 2 || result.Links[0].Status != "not_checked" || result.Links[1].Status != "not_checked" {
		t.Fatalf("missing route must not look like direct forwarding: %+v", result)
	}
}

func TestDiagnoseSharedExitWaitsForReachableIngress(t *testing.T) {
	f := newSecurityFixture(t)
	ctx := context.Background()
	var entries []int64
	for i := 0; i < 4; i++ {
		var id int64
		if err := f.p.QueryRow(ctx, `INSERT INTO nodes(name,token_hash,protocol_version,last_seen,applied_revision) VALUES($1,$2,$3,now(),9223372036854775807) RETURNING id`, Secret(), Secret(), pb.DiagnosticVersion).Scan(&id); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, id)
	}
	var exit, group int64
	if err := f.p.QueryRow(ctx, `INSERT INTO nodes(name,token_hash,tunnel_exit_enabled,tunnel_listen_port,tunnel_public_host,protocol_version,last_seen,applied_revision) VALUES($1,$2,true,32134,'127.0.0.1',$3,now(),9223372036854775807) RETURNING id`, Secret(), Secret(), pb.DiagnosticVersion).Scan(&exit); err != nil {
		t.Fatal(err)
	}
	if err := f.p.QueryRow(ctx, `INSERT INTO node_groups(name) VALUES($1) RETURNING id`, Secret()).Scan(&group); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.p.Exec(ctx, `DELETE FROM rules WHERE ingress_group_id=$1`, group)
		_, _ = f.p.Exec(ctx, `DELETE FROM user_node_groups WHERE group_id=$1`, group)
		_, _ = f.p.Exec(ctx, `DELETE FROM node_groups WHERE id=$1`, group)
		all := append(append([]int64{}, entries...), exit)
		_, _ = f.p.Exec(ctx, `DELETE FROM nodes WHERE id=ANY($1::bigint[])`, all)
	})
	for _, id := range append(append([]int64{}, entries...), exit) {
		testAuthorize(t, f.p, f.user, id)
	}
	for _, id := range entries {
		if _, err := f.p.Exec(ctx, `INSERT INTO node_group_members(group_id,node_id) VALUES($1,$2)`, group, id); err != nil {
			t.Fatal(err)
		}
		if _, err := f.p.Exec(ctx, `INSERT INTO node_tunnel_links(ingress_node_id,egress_node_id,token) VALUES($1,$2,$3)`, id, exit, Secret()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.p.Exec(ctx, `INSERT INTO user_node_groups(user_id,group_id) VALUES($1,$2)`, f.user, group); err != nil {
		t.Fatal(err)
	}
	w := f.request(f.user, "rules", "POST", RuleInput{IngressGroupID: group, EgressNodeID: exit, Targets: []string{"example.com:443"}, Enabled: true})
	securityStatus(t, w, 200)
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.Exec(ctx, `DELETE FROM node_tunnel_links WHERE ingress_node_id=$1 AND egress_node_id=$2`, entries[3], exit); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.Exec(ctx, `UPDATE nodes SET last_seen=NULL WHERE id=$1`, entries[2]); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("rules/%d/diagnose", created.ID)
	for _, success := range []bool{true, false} {
		f.s.diagnostics.mu.Lock()
		delete(f.s.diagnostics.last, f.user)
		f.s.diagnostics.mu.Unlock()
		response := make(chan *httptest.ResponseRecorder, 1)
		go func() { response <- f.request(f.user, path, "POST", nil) }()
		for i, node := range entries[:2] {
			end := time.After(5 * time.Second)
			for {
				msg := &pb.ControlMessage{}
				f.s.diagnosticExchange(node, &pb.AgentMessage{ProtocolVersion: pb.DiagnosticVersion}, msg)
				if len(msg.DiagnosticTasks) > 0 {
					// No exit task may exist before all relevant first-stage results.
					probe := &pb.ControlMessage{}
					f.s.diagnosticExchange(exit, &pb.AgentMessage{ProtocolVersion: pb.DiagnosticVersion}, probe)
					if len(probe.DiagnosticTasks) != 0 {
						t.Fatal("exit task dispatched before ingress completed")
					}
					status := "unreachable"
					if i == 1 && success {
						status = "reachable"
					}
					f.s.diagnosticExchange(node, &pb.AgentMessage{ProtocolVersion: pb.DiagnosticVersion, DiagnosticResults: []*pb.DiagnosticResult{{TaskId: msg.DiagnosticTasks[0].TaskId, Status: status, LatencyMs: 3}}}, &pb.ControlMessage{})
					break
				}
				select {
				case <-end:
					t.Fatal("ingress task missing")
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		if success {
			end := time.After(5 * time.Second)
			for {
				msg := &pb.ControlMessage{}
				f.s.diagnosticExchange(exit, &pb.AgentMessage{ProtocolVersion: pb.DiagnosticVersion}, msg)
				if len(msg.DiagnosticTasks) > 0 {
					task := msg.DiagnosticTasks[0]
					if task.IngressNodeId != entries[1] {
						t.Fatalf("used non-working ingress: %d", task.IngressNodeId)
					}
					f.s.diagnosticExchange(exit, &pb.AgentMessage{ProtocolVersion: pb.DiagnosticVersion, DiagnosticResults: []*pb.DiagnosticResult{{TaskId: task.TaskId, Status: "reachable", LatencyMs: 5}}}, &pb.ControlMessage{})
					break
				}
				select {
				case <-end:
					t.Fatal("exit task missing")
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		select {
		case got := <-response:
			securityStatus(t, got, 200)
			var result diagnosticOutput
			if err := json.Unmarshal(got.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			var target, missing, offline *diagnosticLink
			for i := range result.Links {
				l := &result.Links[i]
				if l.Source == fmt.Sprintf("out:%d", exit) {
					target = l
				}
				if l.Source == fmt.Sprintf("in:%d", entries[3]) {
					missing = l
				}
				if l.Source == fmt.Sprintf("in:%d", entries[2]) {
					offline = l
				}
			}
			if missing == nil || missing.Target != "out:unavailable" || missing.Status != "not_checked" || offline == nil || offline.Reason != "节点离线" || target == nil {
				t.Fatalf("missing/offline route lost: %+v", result.Links)
			}
			if success && target.Status != "reachable" || !success && (target.Status != "not_checked" || target.Reason != "入口到该出口的链路未成功，未检测后续目标") {
				t.Fatalf("unexpected target: %+v", target)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("shared exit diagnosis did not complete")
		}
		if !success {
			msg := &pb.ControlMessage{}
			f.s.diagnosticExchange(exit, &pb.AgentMessage{ProtocolVersion: pb.DiagnosticVersion}, msg)
			if len(msg.DiagnosticTasks) != 0 {
				t.Fatal("exit task dispatched with no reachable ingress")
			}
		}
	}
}

func TestDiagnosticRegistryAssignsResultsOnlyToTheirNode(t *testing.T) {
	s := &Server{}
	d := &s.diagnostics
	now := time.Now()
	if !d.begin(1, now) || d.begin(1, now) {
		t.Fatal("same user must not run concurrent diagnostics")
	}
	defer d.finish(1, []string{"task"})
	ch := make(chan diagnosticResult, 1)
	d.tasks["task"] = &diagnosticPending{nodeID: 10, task: &pb.DiagnosticTask{TaskId: "task"}, result: ch}
	message := &pb.ControlMessage{}
	s.diagnosticExchange(11, &pb.AgentMessage{ProtocolVersion: pb.DiagnosticVersion, DiagnosticResults: []*pb.DiagnosticResult{{TaskId: "task", Status: "reachable"}}}, message)
	select {
	case <-ch:
		t.Fatal("accepted result from wrong node")
	default:
	}
	message = &pb.ControlMessage{}
	s.diagnosticExchange(10, &pb.AgentMessage{ProtocolVersion: pb.DiagnosticVersion}, message)
	if len(message.DiagnosticTasks) != 1 {
		t.Fatalf("expected one task, got %d", len(message.DiagnosticTasks))
	}
	s.diagnosticExchange(10, &pb.AgentMessage{ProtocolVersion: pb.DiagnosticVersion, DiagnosticResults: []*pb.DiagnosticResult{{TaskId: "task", Status: "unreachable", Reason: "private network details"}}}, &pb.ControlMessage{})
	select {
	case result := <-ch:
		if result.status != "unreachable" || result.reason != "连接失败" {
			t.Fatalf("unexpected result: %+v", result)
		}
	default:
		t.Fatal("missing result")
	}
}

func TestDiagnosticPhaseDeadlineKeepsCompletedResultsAndCleansTasks(t *testing.T) {
	s := &Server{}
	if !s.diagnostics.begin(1, time.Now()) {
		t.Fatal("begin")
	}
	var ids []string
	defer func() { s.diagnostics.finish(1, ids) }()
	out := diagnosticOutput{Links: []diagnosticLink{{Status: "not_checked"}, {Status: "not_checked"}}}
	first := make(chan diagnosticResult, 1)
	second := make(chan diagnosticResult, 1)
	second <- diagnosticResult{status: "reachable", latency: 42}
	work := []diagnosticWork{{index: 0, nodeID: 1, task: &pb.DiagnosticTask{TaskId: "slow"}, result: first}, {index: 1, nodeID: 1, task: &pb.DiagnosticTask{TaskId: "ready"}, result: second}}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	s.runDiagnosticPhase(ctx, &out, work, &ids)
	if time.Since(start) > time.Second {
		t.Fatal("phase deadline was not honored")
	}
	if out.Links[0].Reason != "节点未在规定时间内返回结果" || out.Links[1].Status != "reachable" || out.Links[1].LatencyMS == nil || *out.Links[1].LatencyMS != 42 {
		t.Fatalf("deadline discarded completed result: %+v", out.Links)
	}
	s.diagnostics.mu.Lock()
	pending := len(s.diagnostics.tasks)
	s.diagnostics.mu.Unlock()
	if pending != 0 {
		t.Fatalf("timed-out tasks leaked: %d", pending)
	}
}

func TestDiagnosticOfflineAndOldAgentsAreNotProbed(t *testing.T) {
	revision := int64(10)
	m := diagnosticMachine{enabled: true, authorized: true, online: true, version: pb.DiagnosticVersion, applied: revision}
	if m.unavailable(revision) != "" {
		t.Fatal("ready node marked unavailable")
	}
	m.version--
	if m.unavailable(revision) != "请升级节点以使用诊断功能" {
		t.Fatal("old node must request upgrade")
	}
	m.version++
	m.online = false
	if m.unavailable(revision) != "节点离线" {
		t.Fatal("offline node should not be probed")
	}
	m.online = true
	m.applied--
	if m.unavailable(revision) != "节点配置未同步" {
		t.Fatal("unsynced node should not be probed")
	}
}
