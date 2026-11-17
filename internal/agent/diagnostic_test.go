package agent

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	pb "github.com/nekopass/nekopass/internal/protocol"
	"github.com/nekopass/nekopass/internal/tunnel"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestDiagnosticTargetPolicyAndAuthorization(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	e := &Engine{revision: 1}
	r := &pb.Rule{Id: 11, Enabled: true, Protocol: "tcp", Targets: []string{ln.Addr().String()}}
	c := &pb.ControlMessage{Revision: 1, Node: &pb.NodeConfig{NodeId: 2, Enabled: true}, Rules: []*pb.Rule{r}}
	task := &pb.DiagnosticTask{TaskId: "one", RuleId: 11, Kind: "target"}
	if got := e.diagnose(context.Background(), c, task); got.Status != "reachable" {
		t.Fatalf("reachable: %+v", got)
	}
	task.TargetIndex = 1
	if got := e.diagnose(context.Background(), c, task); got.Status != "not_checked" {
		t.Fatalf("bad target index: %+v", got)
	}
	task.TargetIndex = 0
	r.Protocol = "udp"
	if got := e.diagnose(context.Background(), c, task); got.Status != "unknown" || got.Reason != "udp_unconfirmed" {
		t.Fatalf("UDP: %+v", got)
	}
	r.Protocol = "tcp"
	// Resolve localhost through the ordinary resolver: network policy must be
	// enforced against the resolved address, not just the configured hostname.
	r.Targets[0] = net.JoinHostPort("localhost", strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))
	policy, err := targetPolicy(true, []string{"127.0.0.0/8", "::1/128"})
	if err != nil {
		t.Fatal(err)
	}
	e.targetPolicy.Store(policy)
	if got := e.diagnose(context.Background(), c, task); got.Status != "unreachable" {
		t.Fatalf("denied target: %+v", got)
	}
	task.RuleId = 12
	if got := e.diagnose(context.Background(), c, task); got.Status != "not_checked" {
		t.Fatalf("wrong rule: %+v", got)
	}
}

func TestDiagnosticExitGrantAndTunnel(t *testing.T) {
	for _, mode := range []string{"plain_tcp", "tls_h2", "plain_udp"} {
		t.Run(mode, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			exit, h2, exitConfig, rule := h2Fixture(t, ln.Addr().String())
			exitConfig.Node.TunnelProtocol = mode
			rule.TunnelProtocol = mode
			rule.Enabled = true
			rule.Protocol = "tcp"
			if tunnel.UDP(mode) {
				rule.Protocol = "udp"
				exitConfig.EgressRules[0].Protocol = "udp"
			}
			if err := exit.Apply(exitConfig); err != nil {
				t.Fatal(err)
			}
			assertTunnelTestApplied(t, exit)
			entry := &Engine{revision: 1, h2: h2}
			entryConfig := &pb.ControlMessage{Revision: 1, Node: &pb.NodeConfig{NodeId: 2, Enabled: true}, Rules: []*pb.Rule{rule}}
			task := &pb.DiagnosticTask{TaskId: "one", RuleId: rule.Id, Kind: "tunnel", EgressNodeId: 3}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if got := entry.diagnose(ctx, entryConfig, task); got.Status != "reachable" {
				t.Fatalf("tunnel: %+v", got)
			}
			task.EgressNodeId = 4
			if got := entry.diagnose(ctx, entryConfig, task); got.Status != "not_checked" {
				t.Fatalf("unauthorized peer: %+v", got)
			}
			task.Kind, task.EgressNodeId, task.IngressNodeId = "target", 3, 2
			got := exit.diagnose(ctx, exitConfig, task)
			if mode == "plain_udp" && got.Status != "unknown" || mode != "plain_udp" && got.Status != "reachable" {
				t.Fatalf("exit target: %+v", got)
			}
			task.IngressNodeId = 99
			if got := exit.diagnose(ctx, exitConfig, task); got.Status != "not_checked" {
				t.Fatalf("wrong grant: %+v", got)
			}
		})
	}
}

func TestDiagnosticProtocolRoundTrip(t *testing.T) {
	task := &pb.DiagnosticTask{TaskId: "id", RuleId: 21, Kind: "target", TargetIndex: 3, EgressNodeId: 4, IngressNodeId: 5}
	config := &pb.ControlMessage{Revision: 8, DiagnosticTasks: []*pb.DiagnosticTask{task}}
	data, err := proto.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var copy pb.ControlMessage
	if err := proto.Unmarshal(data, &copy); err != nil || !proto.Equal(config, &copy) {
		t.Fatalf("task roundtrip: %v", err)
	}
	report := &pb.AgentMessage{DiagnosticResults: []*pb.DiagnosticResult{{TaskId: "id", Status: "reachable", LatencyMs: 25, Reason: "test"}}}
	data, err = proto.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var copyReport pb.AgentMessage
	if err := proto.Unmarshal(data, &copyReport); err != nil || !proto.Equal(report, &copyReport) {
		t.Fatalf("result roundtrip: %v", err)
	}
}

func TestDiagnosticProtocolDescriptor(t *testing.T) {
	tests := []struct {
		message protoreflect.MessageDescriptor
		fields  map[protoreflect.Name]struct {
			number   protoreflect.FieldNumber
			kind     protoreflect.Kind
			repeated bool
		}
	}{
		{(&pb.DiagnosticTask{}).ProtoReflect().Descriptor(), map[protoreflect.Name]struct {
			number   protoreflect.FieldNumber
			kind     protoreflect.Kind
			repeated bool
		}{
			"task_id": {1, protoreflect.StringKind, false}, "rule_id": {2, protoreflect.Int64Kind, false}, "kind": {3, protoreflect.StringKind, false}, "target_index": {4, protoreflect.Int32Kind, false}, "egress_node_id": {5, protoreflect.Int64Kind, false}, "ingress_node_id": {6, protoreflect.Int64Kind, false},
		}},
		{(&pb.DiagnosticResult{}).ProtoReflect().Descriptor(), map[protoreflect.Name]struct {
			number   protoreflect.FieldNumber
			kind     protoreflect.Kind
			repeated bool
		}{
			"task_id": {1, protoreflect.StringKind, false}, "status": {2, protoreflect.StringKind, false}, "latency_ms": {3, protoreflect.Int64Kind, false}, "reason": {4, protoreflect.StringKind, false},
		}},
		{(&pb.AgentMessage{}).ProtoReflect().Descriptor(), map[protoreflect.Name]struct {
			number   protoreflect.FieldNumber
			kind     protoreflect.Kind
			repeated bool
		}{"diagnostic_results": {18, protoreflect.MessageKind, true}}},
		{(&pb.ControlMessage{}).ProtoReflect().Descriptor(), map[protoreflect.Name]struct {
			number   protoreflect.FieldNumber
			kind     protoreflect.Kind
			repeated bool
		}{"diagnostic_tasks": {11, protoreflect.MessageKind, true}}},
	}
	for _, tc := range tests {
		for name, want := range tc.fields {
			f := tc.message.Fields().ByName(name)
			if f == nil || f.Number() != want.number || f.Kind() != want.kind || f.IsList() != want.repeated {
				t.Fatalf("bad descriptor %s.%s: %v", tc.message.Name(), name, f)
			}
		}
	}
}

func TestDiagnosticQueueSaturation(t *testing.T) {
	s := newDiagnosticSession(context.Background(), &Engine{})
	for i := 0; i < diagnosticMaxTasks; i++ {
		s.states[fmt.Sprint(i)] = nil
	}
	s.schedule(&pb.ControlMessage{}, []*pb.DiagnosticTask{{TaskId: "overflow"}})
	results := s.results()
	if len(results) != 1 || results[0].TaskId != "overflow" || results[0].Status != "not_checked" || results[0].Reason != "busy" {
		t.Fatalf("overflow result: %+v", results)
	}
	s.sent(results)
	if len(s.results()) != 0 {
		t.Fatal("sent result remained queued")
	}
}
