package agent

import (
	"context"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/nekopass/nekopass/internal/protocol"
	"google.golang.org/protobuf/proto"
)

func TestMultiIngressPolicyIsolationAndRevocation(t *testing.T) {
	first := &pb.EgressRule{RuleId: 11, IngressNodeId: 2, UserId: 5, QuotaEpoch: 3, Targets: []string{"one.example:80"}, Protocol: "tcp"}
	second := &pb.EgressRule{RuleId: 11, IngressNodeId: 4, UserId: 6, QuotaEpoch: 7, Targets: []string{"two.example:53"}, Protocol: "udp"}
	config := &pb.ControlMessage{EgressRules: []*pb.EgressRule{first, second}}
	policy := policyOf(config)
	if !tunnelRuleAllowed(policy, 11, 2, 5, 3, "one.example:80") || !tunnelRuleAllowed(policy, 11, 4, 6, 7, "two.example:53") {
		t.Fatal("one of the two ingress authorizations was lost")
	}
	if tunnelRuleAllowed(policy, 11, 2, 6, 7, "two.example:53") || tunnelRuleAllowed(policy, 11, 4, 5, 3, "one.example:80") {
		t.Fatal("authorization crossed ingress boundaries")
	}
	tcp := h2Open{Rule: 11, Ingress: 2, User: 5, Epoch: 3, Target: "one.example:80", Network: "tcp"}
	udp := h2Open{Rule: 11, Ingress: 4, User: 6, Epoch: 7, Target: "two.example:53", Network: "udp"}
	if !h2RuleAllowed(policy, tcp) || !h2RuleAllowed(policy, udp) {
		t.Fatal("TCP or UDP authorization was lost")
	}
	tcp.Network, udp.Network = "udp", "tcp"
	if h2RuleAllowed(policy, tcp) || h2RuleAllowed(policy, udp) {
		t.Fatal("per-ingress protocol restriction was not enforced")
	}
	config.EgressRules = []*pb.EgressRule{second}
	policy = policyOf(config)
	if tunnelRuleAllowed(policy, 11, 2, 5, 3, "one.example:80") || !tunnelRuleAllowed(policy, 11, 4, 6, 7, "two.example:53") {
		t.Fatal("revoking one ingress changed another ingress authorization")
	}
}

func TestMultiIngressH2ForwardingAndRevocation(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		for {
			conn, err := target.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	engine, transport, config, first := h2Fixture(t, target.Addr().String())
	second := proto.Clone(first).(*pb.Rule)
	second.IngressNodeId, second.TunnelToken = 4, strings.Repeat("b", 64)
	config.TunnelLinks = append(config.TunnelLinks, &pb.TunnelLink{IngressNodeId: 4, Token: second.TunnelToken})
	secondGrant := proto.Clone(config.EgressRules[0]).(*pb.EgressRule)
	secondGrant.IngressNodeId = 4
	config.EgressRules = append(config.EgressRules, secondGrant)
	if err := engine.Apply(config); err != nil {
		t.Fatal(err)
	}
	assertTunnelTestApplied(t, engine)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	check := func(rule *pb.Rule, allowed bool) {
		t.Helper()
		conn, err := transport.Dial(ctx, ctx, rule, target.Addr().String())
		if !allowed {
			if err == nil {
				conn.Close()
				t.Fatal("revoked ingress connected")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err = io.WriteString(conn, "ok"); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 2)
		if _, err = io.ReadFull(conn, buf); err != nil || string(buf) != "ok" {
			t.Fatal("forwarding failed", err)
		}
	}
	check(first, true)
	check(second, true)
	config.EgressRules = []*pb.EgressRule{secondGrant}
	if err := engine.Apply(config); err != nil {
		t.Fatal(err)
	}
	assertTunnelTestApplied(t, engine)
	check(first, false)
	check(second, true)
}

func TestMultiIngressH3UDPForwardingAndRevocation(t *testing.T) {
	target := randomUDPEcho(t)
	engine, transport, config, first := h3Fixture(t, target)
	first.Protocol = "udp"
	config.EgressRules[0].Protocol = "udp"
	second := proto.Clone(first).(*pb.Rule)
	second.IngressNodeId, second.TunnelToken = 4, strings.Repeat("b", 64)
	config.TunnelLinks = append(config.TunnelLinks, &pb.TunnelLink{IngressNodeId: 4, Token: second.TunnelToken})
	secondGrant := proto.Clone(config.EgressRules[0]).(*pb.EgressRule)
	secondGrant.IngressNodeId = 4
	config.EgressRules = append(config.EgressRules, secondGrant)
	if err := engine.Apply(config); err != nil {
		t.Fatal(err)
	}
	assertTunnelTestApplied(t, engine)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	check := func(rule *pb.Rule, allowed bool) {
		t.Helper()
		var queued atomic.Int64
		conn, err := transport.h3.Dial(ctx, ctx, rule, target, true, &queued)
		if !allowed {
			if err == nil {
				conn.Close()
				t.Fatal("revoked ingress connected")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err = conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err = conn.Write([]byte("ok")); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 8)
		n, err := conn.Read(buf)
		if err != nil || string(buf[:n]) != "ok" {
			t.Fatal("UDP forwarding failed", err)
		}
	}
	check(first, true)
	check(second, true)
	config.EgressRules = []*pb.EgressRule{secondGrant}
	if err := engine.Apply(config); err != nil {
		t.Fatal(err)
	}
	assertTunnelTestApplied(t, engine)
	check(first, false)
	check(second, true)
}

func TestMultiIngressPlainTCPForwardingAndRevocation(t *testing.T) {
	target, _ := inboundEcho(t, false)
	engine, _, config, first := h2Fixture(t, target)
	config.Node.TunnelProtocol = "plain_tcp"
	first.TunnelProtocol = "plain_tcp"
	second := proto.Clone(first).(*pb.Rule)
	second.IngressNodeId, second.TunnelToken = 4, strings.Repeat("b", 64)
	config.TunnelLinks = append(config.TunnelLinks, &pb.TunnelLink{IngressNodeId: 4, Token: second.TunnelToken})
	secondGrant := proto.Clone(config.EgressRules[0]).(*pb.EgressRule)
	secondGrant.IngressNodeId = 4
	config.EgressRules = append(config.EgressRules, secondGrant)
	if err := engine.Apply(config); err != nil {
		t.Fatal(err)
	}
	assertTunnelTestApplied(t, engine)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	check := func(rule *pb.Rule, allowed bool) {
		t.Helper()
		conn, err := dialTunnel(ctx, rule, target)
		if !allowed {
			if err == nil {
				conn.Close()
				t.Fatal("revoked ingress connected")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err = io.WriteString(conn, "ok"); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 2)
		if _, err = io.ReadFull(conn, buf); err != nil || string(buf) != "ok" {
			t.Fatal("forwarding failed", err)
		}
	}
	check(first, true)
	check(second, true)
	config.EgressRules = []*pb.EgressRule{secondGrant}
	if err := engine.Apply(config); err != nil {
		t.Fatal(err)
	}
	assertTunnelTestApplied(t, engine)
	check(first, false)
	check(second, true)
}
