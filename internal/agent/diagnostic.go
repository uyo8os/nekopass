package agent

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"
	"time"

	pb "github.com/nekopass/nekopass/internal/protocol"
	"github.com/nekopass/nekopass/internal/tunnel"
)

const diagnosticMaxTasks = 32
const diagnosticTimeout = 3 * time.Second

// A session owns its bounded asynchronous probes. Diagnostic I/O must never
// block the heartbeat and requests must never survive a stream reconnect.
type diagnosticSession struct {
	ctx    context.Context
	e      *Engine
	mu     sync.Mutex
	states map[string]*pb.DiagnosticResult
	slots  chan struct{}
}

func newDiagnosticSession(ctx context.Context, e *Engine) *diagnosticSession {
	return &diagnosticSession{ctx: ctx, e: e, states: make(map[string]*pb.DiagnosticResult), slots: make(chan struct{}, 8)}
}

func (s *diagnosticSession) schedule(config *pb.ControlMessage, tasks []*pb.DiagnosticTask) {
	if len(tasks) > diagnosticMaxTasks {
		tasks = tasks[:diagnosticMaxTasks]
	}
	for _, task := range tasks {
		if task == nil || task.TaskId == "" || len(task.TaskId) > 128 {
			continue
		}
		s.mu.Lock()
		if _, exists := s.states[task.TaskId]; exists {
			s.mu.Unlock()
			continue
		}
		pending := 0
		for _, state := range s.states {
			if state == nil {
				pending++
			}
		}
		if pending >= diagnosticMaxTasks {
			// Report saturation immediately instead of silently losing the task.
			if len(s.states) < diagnosticMaxTasks*2 {
				s.states[task.TaskId] = &pb.DiagnosticResult{TaskId: task.TaskId, Status: "not_checked", Reason: "busy"}
			}
			s.mu.Unlock()
			continue
		}
		s.states[task.TaskId] = nil
		s.mu.Unlock()
		go func(task *pb.DiagnosticTask) {
			select {
			case s.slots <- struct{}{}:
			case <-s.ctx.Done():
				return
			}
			defer func() { <-s.slots }()
			ctx, cancel := context.WithTimeout(s.ctx, diagnosticTimeout)
			defer cancel()
			result := s.e.diagnose(ctx, config, task)
			s.mu.Lock()
			s.states[task.TaskId] = result
			s.mu.Unlock()
		}(task)
	}
}

func (s *diagnosticSession) results() []*pb.DiagnosticResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	var results []*pb.DiagnosticResult
	for _, result := range s.states {
		if result != nil {
			results = append(results, result)
		}
	}
	return results
}
func (s *diagnosticSession) sent(results []*pb.DiagnosticResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, result := range results {
		delete(s.states, result.TaskId)
	}
}

// Resolve the task from the last successfully applied configuration only; the
// control message never supplies an arbitrary network address to this method.
func (e *Engine) diagnose(ctx context.Context, config *pb.ControlMessage, task *pb.DiagnosticTask) *pb.DiagnosticResult {
	result := &pb.DiagnosticResult{TaskId: task.TaskId, Status: "not_checked", Reason: "rule_unavailable"}
	e.mu.Lock()
	applied := !e.closed && e.syncError == "" && e.revision == config.Revision
	e.mu.Unlock()
	if !applied || config.Node == nil || !config.Node.Enabled {
		return result
	}
	var r *pb.Rule
	for _, candidate := range config.Rules {
		if candidate.Id == task.RuleId && candidate.Enabled {
			r = candidate
			break
		}
	}
	var target, network string
	switch task.Kind {
	case "target":
		if task.EgressNodeId == 0 && r != nil && r.EgressNodeId == 0 && r.EgressGroupId == 0 {
			network = r.Protocol
			targets := r.Targets
			if len(targets) == 0 && r.TargetHost != "" {
				targets = []string{net.JoinHostPort(r.TargetHost, strconv.Itoa(int(r.TargetPort)))}
			}
			if task.TargetIndex >= 0 && int(task.TargetIndex) < len(targets) {
				target = targets[task.TargetIndex]
			}
		} else if task.EgressNodeId == config.Node.NodeId && config.Node.TunnelExitEnabled {
			for _, grant := range config.EgressRules {
				if grant.RuleId == task.RuleId && grant.IngressNodeId == task.IngressNodeId && (grant.ExpiresUnix == 0 || time.Now().Unix() < grant.ExpiresUnix) && task.TargetIndex >= 0 && int(task.TargetIndex) < len(grant.Targets) {
					target, network = grant.Targets[task.TargetIndex], grant.Protocol
					break
				}
			}
		}
		if target == "" {
			return result
		}
		if network == "udp" {
			result.Status, result.Reason = "unknown", "udp_unconfirmed"
			return result
		}
		if network != "tcp" && network != "" {
			result.Reason = "unsupported"
			return result
		}
		start := time.Now()
		conn, err := e.dialTarget(ctx, "tcp", target)
		if err != nil {
			diagnosticError(result, ctx)
			return result
		}
		conn.Close()
		result.Status, result.Reason, result.LatencyMs = "reachable", "", time.Since(start).Milliseconds()
		return result
	case "tunnel":
		if r == nil || task.EgressNodeId <= 0 {
			return result
		}
		var peer *pb.Rule
		if r.EgressGroupId != 0 {
			for _, p := range r.EgressPeers {
				if p.NodeId == task.EgressNodeId {
					peer = peerRule(r, p)
					break
				}
			}
		} else if r.EgressNodeId == task.EgressNodeId {
			peer = r
		}
		if peer == nil {
			return result
		}
		start := time.Now()
		conn, err := dialEgress(ctx, ctx, peer, tunnelHealthTarget, peer.Protocol == "udp", e.h2, &e.udpQueued)
		if err == nil {
			defer conn.Close()
			stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
			defer stop()
			if tunnel.UDP(peer.TunnelProtocol) {
				if deadline, ok := ctx.Deadline(); ok {
					_ = conn.SetDeadline(deadline)
				}
				_, err = conn.Write([]byte{0})
				if err == nil {
					var b [1]byte
					var n int
					n, err = conn.Read(b[:])
					if err == nil && (n != 1 || b[0] != 0) {
						err = errors.New("invalid tunnel health response")
					}
				}
			}
		}
		if err != nil {
			diagnosticError(result, ctx)
			return result
		}
		result.Status, result.Reason, result.LatencyMs = "reachable", "", time.Since(start).Milliseconds()
		return result
	default:
		result.Reason = "unsupported"
		return result
	}
}

func diagnosticError(result *pb.DiagnosticResult, ctx context.Context) {
	result.Status, result.Reason = "unreachable", "connection_failed"
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.Reason = "timeout"
	}
}
