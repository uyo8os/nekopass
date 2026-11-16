package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	pb "github.com/nekopass/nekopass/internal/protocol"
)

// Diagnostic requests are ephemeral and bounded. They contain only identifiers;
// each agent resolves these against its own applied configuration.
const diagnosticMaxLinks = 128
const diagnosticMaxTasksPerNode = 32
const diagnosticTotalTimeout = 27 * time.Second
const diagnosticFirstPhaseTimeout = 14 * time.Second

type diagnosticRegistry struct {
	mu     sync.Mutex
	tasks  map[string]*diagnosticPending
	last   map[int64]time.Time
	active map[int64]bool
	count  int
}

type diagnosticPending struct {
	nodeID     int64
	task       *pb.DiagnosticTask
	dispatched bool
	result     chan diagnosticResult
}

type diagnosticResult struct {
	status  string
	latency int64
	reason  string
}
type diagnosticNode struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Address string `json:"address"`
}
type diagnosticLink struct {
	Source    string `json:"source"`
	Target    string `json:"target"`
	Status    string `json:"status"`
	LatencyMS *int64 `json:"latency_ms,omitempty"`
	Reason    string `json:"reason,omitempty"`
}
type diagnosticOutput struct {
	RuleID   int64            `json:"rule_id"`
	Protocol string           `json:"protocol"`
	Mode     string           `json:"mode"`
	Nodes    []diagnosticNode `json:"nodes"`
	Links    []diagnosticLink `json:"links"`
}
type diagnosticMachine struct {
	id                          int64
	name, address               string
	enabled, online, authorized bool
	version                     int32
	applied                     int64
	syncError                   string
}

func (m diagnosticMachine) unavailable(revision int64) string {
	switch {
	case !m.enabled:
		return "节点已禁用"
	case !m.authorized:
		return "节点未获授权"
	case !m.online:
		return "节点离线"
	case m.version < pb.DiagnosticVersion:
		return "请升级节点以使用诊断功能"
	case m.syncError != "" || m.applied < revision:
		return "节点配置未同步"
	default:
		return ""
	}
}

func (d *diagnosticRegistry) begin(user int64, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.last == nil {
		d.last = make(map[int64]time.Time)
		d.active = make(map[int64]bool)
		d.tasks = make(map[string]*diagnosticPending)
	}
	for k, t := range d.last {
		if now.Sub(t) > time.Minute {
			delete(d.last, k)
		}
	}
	if d.active[user] || d.count >= 4 || now.Sub(d.last[user]) < 10*time.Second {
		return false
	}
	d.active[user] = true
	d.last[user] = now
	d.count++
	return true
}
func (d *diagnosticRegistry) finish(user int64, ids []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, id := range ids {
		delete(d.tasks, id)
	}
	delete(d.active, user)
	d.count--
}

// Results are accepted only from the node assigned the corresponding task.
// Agent-provided reasons are mapped to fixed public text, never exposed raw.
func (s *Server) diagnosticExchange(nodeID int64, report *pb.AgentMessage, reply *pb.ControlMessage) {
	d := &s.diagnostics
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, result := range report.DiagnosticResults {
		if result == nil {
			continue
		}
		p := d.tasks[result.TaskId]
		if p == nil || p.nodeID != nodeID || !p.dispatched {
			continue
		}
		r := diagnosticResult{status: "not_checked", reason: "节点未完成检测"}
		switch result.Status {
		case "reachable":
			if result.LatencyMs >= 0 && result.LatencyMs <= 60000 {
				r = diagnosticResult{status: "reachable", latency: result.LatencyMs}
			}
		case "unreachable":
			r = diagnosticResult{status: "unreachable", reason: "连接失败"}
			if result.Reason == "timeout" {
				r.reason = "连接超时"
			}
		case "unknown":
			r = diagnosticResult{status: "unknown", reason: "无法确认目标是否可达"}
		case "not_checked":
			r = diagnosticResult{status: "not_checked", reason: "配置未同步或不支持此检测"}
			if result.Reason == "busy" {
				r.reason = "节点诊断繁忙"
			}
		}
		select {
		case p.result <- r:
		default:
		}
		delete(d.tasks, result.TaskId)
	}
	if report.ProtocolVersion < pb.DiagnosticVersion {
		return
	}
	for _, p := range d.tasks {
		if p.nodeID == nodeID && !p.dispatched && len(reply.DiagnosticTasks) < diagnosticMaxTasksPerNode {
			reply.DiagnosticTasks = append(reply.DiagnosticTasks, p.task)
			p.dispatched = true
		}
	}
}

type diagnosticWork struct {
	index  int
	nodeID int64
	task   *pb.DiagnosticTask
	result chan diagnosticResult
}

// Each phase has a bounded deadline. Remove unanswered tasks immediately after
// the phase, so a later heartbeat cannot launch a stale probe.
func (s *Server) runDiagnosticPhase(ctx context.Context, out *diagnosticOutput, work []diagnosticWork, taskIDs *[]string) {
	if ctx.Err() != nil {
		for _, item := range work {
			out.Links[item.index].Reason = "节点未在规定时间内返回结果"
		}
		return
	}
	s.diagnostics.mu.Lock()
	for _, item := range work {
		s.diagnostics.tasks[item.task.TaskId] = &diagnosticPending{nodeID: item.nodeID, task: item.task, result: item.result}
		*taskIDs = append(*taskIDs, item.task.TaskId)
	}
	s.diagnostics.mu.Unlock()
	for _, item := range work {
		apply := func(result diagnosticResult) {
			l := &out.Links[item.index]
			l.Status, l.Reason = result.status, result.reason
			if result.status == "reachable" {
				value := result.latency
				l.LatencyMS = &value
			}
		}
		select {
		case result := <-item.result:
			apply(result)
		case <-ctx.Done():
			// Both the deadline and a result may already be ready. Do not
			// discard a completed result simply because select chose timeout.
			select {
			case result := <-item.result:
				apply(result)
			default:
				out.Links[item.index].Reason = "节点未在规定时间内返回结果"
			}
		}
	}
	s.diagnostics.mu.Lock()
	for _, item := range work {
		delete(s.diagnostics.tasks, item.task.TaskId)
	}
	s.diagnostics.mu.Unlock()
}

func (s *Server) diagnoseRule(w http.ResponseWriter, r *http.Request) {
	id, ok := resourceID(w, r)
	if !ok {
		return
	}
	actor := ruleActor(r)
	ctx := r.Context()
	var protocol string
	var enabled bool
	var revision int64
	var egressNode, egressGroup int64
	var rawTargets []byte
	err := s.Pool.QueryRow(ctx, `SELECT protocol,enabled,config_revision,COALESCE(egress_node_id,0),COALESCE(egress_group_id,0),targets FROM rules WHERE id=$1 AND user_id=$2`, id, actor.ID).Scan(&protocol, &enabled, &revision, &egressNode, &egressGroup, &rawTargets)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, 404, "规则不存在")
		return
	}
	if err != nil {
		s.dbError(w, err)
		return
	}
	var targets []string
	if json.Unmarshal(rawTargets, &targets) != nil || len(targets) == 0 || len(targets) > 32 {
		fail(w, 409, "规则目标配置无效")
		return
	}
	if !s.diagnostics.begin(actor.ID, time.Now()) {
		fail(w, 429, "诊断过于频繁，请稍后再试")
		return
	}
	var taskIDs []string
	defer func() { s.diagnostics.finish(actor.ID, taskIDs) }()

	rows, err := s.Pool.Query(ctx, `SELECT n.id,n.name,n.public_address,n.enabled,COALESCE(n.last_seen>now()-interval '12 seconds',false),n.protocol_version,n.applied_revision,n.sync_error,EXISTS(SELECT 1 FROM user_nodes un WHERE un.user_id=r.user_id AND un.node_id=n.id) AND n.ingress_enabled AND (r.egress_node_id IS NOT NULL OR r.egress_group_id IS NOT NULL OR n.allow_direct) AND (r.ingress_group_id IS NULL OR EXISTS(SELECT 1 FROM node_groups g JOIN user_node_groups ug ON ug.group_id=g.id AND ug.user_id=r.user_id WHERE g.id=r.ingress_group_id AND g.enabled)) FROM rule_ingress_nodes ri JOIN rules r ON r.id=ri.rule_id JOIN nodes n ON n.id=ri.node_id WHERE ri.rule_id=$1 ORDER BY n.sort_order NULLS LAST,n.id`, id)
	if err != nil {
		s.dbError(w, err)
		return
	}
	var ingress []diagnosticMachine
	for rows.Next() {
		var n diagnosticMachine
		if err = rows.Scan(&n.id, &n.name, &n.address, &n.enabled, &n.online, &n.version, &n.applied, &n.syncError, &n.authorized); err != nil {
			break
		}
		ingress = append(ingress, n)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		s.dbError(w, err)
		return
	}
	if len(ingress) > diagnosticMaxLinks {
		fail(w, 409, "规则入口节点过多，无法一次诊断")
		return
	}

	// rule_outbound_routes applies the current membership, grant and tunnel link
	// restrictions; do not infer nonexistent group cross-product edges.
	type route struct {
		in  int64
		out diagnosticMachine
	}
	var routes []route
	if egressNode != 0 || egressGroup != 0 {
		rows, err = s.Pool.Query(ctx, `SELECT o.ingress_node_id,n.id,n.name,n.public_address,n.enabled,COALESCE(n.last_seen>now()-interval '12 seconds',false),n.protocol_version,n.applied_revision,n.sync_error FROM rule_outbound_routes o JOIN nodes n ON n.id=o.egress_node_id WHERE o.rule_id=$1 ORDER BY o.ingress_node_id,n.sort_order NULLS LAST,n.id`, id)
		if err != nil {
			s.dbError(w, err)
			return
		}
		for rows.Next() {
			var x route
			x.out.authorized = true
			if err = rows.Scan(&x.in, &x.out.id, &x.out.name, &x.out.address, &x.out.enabled, &x.out.online, &x.out.version, &x.out.applied, &x.out.syncError); err != nil {
				break
			}
			routes = append(routes, x)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			s.dbError(w, err)
			return
		}
	}
	out := diagnosticOutput{RuleID: id, Protocol: protocol, Mode: "direct", Nodes: []diagnosticNode{}, Links: []diagnosticLink{}}
	if egressNode != 0 || egressGroup != 0 {
		out.Mode = "relay"
	}
	machines := map[int64]diagnosticMachine{}
	for _, n := range ingress {
		machines[n.id] = n
		out.Nodes = append(out.Nodes, diagnosticNode{ID: fmt.Sprintf("in:%d", n.id), Kind: "ingress", Name: n.name, Address: n.address})
	}
	exits := map[int64]diagnosticMachine{}
	routedIngress := map[int64]bool{}
	for _, x := range routes {
		exits[x.out.id] = x.out
		routedIngress[x.in] = true
	}
	var unrouted []diagnosticMachine
	if out.Mode == "relay" {
		for _, n := range ingress {
			if !routedIngress[n.id] {
				unrouted = append(unrouted, n)
			}
		}
	}
	var exitIDs []int64
	for k := range exits {
		exitIDs = append(exitIDs, k)
	}
	sort.Slice(exitIDs, func(i, j int) bool { return exitIDs[i] < exitIDs[j] })
	for _, k := range exitIDs {
		n := exits[k]
		out.Nodes = append(out.Nodes, diagnosticNode{ID: fmt.Sprintf("out:%d", k), Kind: "egress", Name: n.name, Address: n.address})
	}
	if out.Mode == "relay" && (len(exitIDs) == 0 || len(unrouted) > 0) {
		out.Nodes = append(out.Nodes, diagnosticNode{ID: "out:unavailable", Kind: "egress", Name: "出口不可用"})
	}
	for i, t := range targets {
		out.Nodes = append(out.Nodes, diagnosticNode{ID: fmt.Sprintf("target:%d", i), Kind: "target", Name: fmt.Sprintf("目标 %d", i+1), Address: t})
	}
	var work []diagnosticWork
	firstWorkCount := 0
	// Remember exact route edges; a shared exit is eligible when any one of
	// its actually configured ingress-to-exit edges was reachable.
	type routeEdge struct {
		ingress, egress int64
		index           int
	}
	var routeEdges []routeEdge
	downstream := map[int64][]int{}
	perNode := map[int64]int{}
	add := func(source, target string, m diagnosticMachine, task *pb.DiagnosticTask) {
		l := diagnosticLink{Source: source, Target: target, Status: "not_checked"}
		reason := m.unavailable(revision)
		if !enabled {
			reason = "规则已停用"
		}
		if reason == "" && perNode[m.id] >= diagnosticMaxTasksPerNode {
			reason = "检测数量超过单节点上限"
		}
		if reason == "" {
			task.TaskId = Secret()
			perNode[m.id]++
			work = append(work, diagnosticWork{index: len(out.Links), nodeID: m.id, task: task, result: make(chan diagnosticResult, 1)})
		} else {
			l.Reason = reason
		}
		out.Links = append(out.Links, l)
	}
	if egressNode == 0 && egressGroup == 0 {
		if len(ingress)*len(targets) > diagnosticMaxLinks {
			fail(w, 409, "规则链路过多，无法一次诊断")
			return
		}
		for _, n := range ingress {
			for i := range targets {
				add(fmt.Sprintf("in:%d", n.id), fmt.Sprintf("target:%d", i), n, &pb.DiagnosticTask{RuleId: id, Kind: "target", TargetIndex: int32(i)})
			}
		}
	} else {
		linkCount := len(routes) + len(exitIDs)*len(targets) + len(unrouted)
		if len(exitIDs) == 0 {
			linkCount += len(targets)
		}
		if linkCount > diagnosticMaxLinks {
			fail(w, 409, "规则链路过多，无法一次诊断")
			return
		}
		for _, n := range unrouted {
			out.Links = append(out.Links, diagnosticLink{Source: fmt.Sprintf("in:%d", n.id), Target: "out:unavailable", Status: "not_checked", Reason: "无可用的出口路由"})
		}
		if len(exitIDs) == 0 {
			for i := range targets {
				out.Links = append(out.Links, diagnosticLink{Source: "out:unavailable", Target: fmt.Sprintf("target:%d", i), Status: "not_checked", Reason: "无可用的出口路由"})
			}
		}
		for _, x := range routes {
			if n, ok := machines[x.in]; ok {
				routeEdges = append(routeEdges, routeEdge{ingress: x.in, egress: x.out.id, index: len(out.Links)})
				add(fmt.Sprintf("in:%d", x.in), fmt.Sprintf("out:%d", x.out.id), n, &pb.DiagnosticTask{RuleId: id, Kind: "tunnel", EgressNodeId: x.out.id})
			}
		}
		firstWorkCount = len(work)
		for _, k := range exitIDs {
			n := exits[k]
			for i := range targets {
				downstream[k] = append(downstream[k], len(out.Links))
				add(fmt.Sprintf("out:%d", k), fmt.Sprintf("target:%d", i), n, &pb.DiagnosticTask{RuleId: id, Kind: "target", EgressNodeId: k, TargetIndex: int32(i)})
			}
		}
	}
	deadline, cancel := context.WithTimeout(ctx, diagnosticTotalTimeout)
	defer cancel()
	if out.Mode == "direct" {
		s.runDiagnosticPhase(deadline, &out, work, &taskIDs)
	} else {
		first, finishFirst := context.WithTimeout(deadline, diagnosticFirstPhaseTimeout)
		s.runDiagnosticPhase(first, &out, work[:firstWorkCount], &taskIDs)
		finishFirst()
		// Do not schedule downstream targets if the request was cancelled.
		if ctx.Err() != nil {
			return
		}
		via := map[int64]int64{}
		for _, edge := range routeEdges {
			if out.Links[edge.index].Status == "reachable" && via[edge.egress] == 0 {
				via[edge.egress] = edge.ingress
			}
		}
		for k, indexes := range downstream {
			if via[k] == 0 {
				for _, index := range indexes {
					out.Links[index].Reason = "入口到该出口的链路未成功，未检测后续目标"
				}
			}
		}
		var second []diagnosticWork
		for _, item := range work[firstWorkCount:] {
			if ingress := via[item.task.EgressNodeId]; ingress != 0 {
				item.task.IngressNodeId = ingress
				second = append(second, item)
			}
		}
		s.runDiagnosticPhase(deadline, &out, second, &taskIDs)
	}
	if ctx.Err() != nil {
		return
	}
	var currentRevision int64
	err = s.Pool.QueryRow(ctx, `SELECT config_revision FROM rules WHERE id=$1 AND user_id=$2`, id, actor.ID).Scan(&currentRevision)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && currentRevision != revision {
		fail(w, 409, "规则配置已变化，请重新诊断")
		return
	}
	if err != nil {
		s.dbError(w, err)
		return
	}
	writeJSON(w, 200, out)
}
