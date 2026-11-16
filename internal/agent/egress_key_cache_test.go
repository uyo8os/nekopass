package agent

import (
	"sync/atomic"
	"testing"

	pb "github.com/nekopass/nekopass/internal/protocol"
	"google.golang.org/protobuf/proto"
)

func TestPeerKeyCacheMatchesUncached(t *testing.T) {
	base := &pb.Rule{IngressNodeId: 1, EgressNodeId: 2, TunnelHost: "exit.example", TunnelPort: 443, TunnelToken: "secret", TunnelProtocol: "tls_h2", Tls: &pb.TLSClientConfig{ServerName: "exit.example", RootCa: "root"}}
	mutations := []func(*pb.Rule){
		func(*pb.Rule) {},
		func(r *pb.Rule) { r.IngressNodeId++ },
		func(r *pb.Rule) { r.EgressNodeId++ },
		func(r *pb.Rule) { r.TunnelHost += "2" },
		func(r *pb.Rule) { r.TunnelPort++ },
		func(r *pb.Rule) { r.TunnelToken += "2" },
		func(r *pb.Rule) { r.TunnelProtocol = "tls_h3" },
		func(r *pb.Rule) { r.Tls.ServerName += "2" },
		func(r *pb.Rule) { r.Tls.RootCa += "2" },
		func(r *pb.Rule) { r.Tls = nil },
		func(r *pb.Rule) { r.Tls = &pb.TLSClientConfig{} },
		func(r *pb.Rule) { r.Random = &pb.RandomConfig{} },
		func(r *pb.Rule) { r.Congestion = &pb.TunnelCongestion{} },
		func(r *pb.Rule) { r.Mux = &pb.MuxConfig{Enabled: true} },
	}
	cache := peerKeyCache{}
	for i, mutate := range mutations {
		r := proto.Clone(base).(*pb.Rule)
		mutate(r)
		for j := 0; j < 2; j++ {
			if got, want := cache.key(r), peerKey(r); got != want {
				t.Fatalf("mutation %d, call %d: got %q, want %q", i, j, got, want)
			}
		}
		// Revisit the original after each mutation to test replacement when
		// the same scalar peer identity has a different nested configuration.
		if got, want := cache.key(base), peerKey(base); got != want {
			t.Fatalf("original after mutation %d: got %q, want %q", i, got, want)
		}
	}
}

func TestGroupUpdateSharedKeysPreservesEachRuleGrant(t *testing.T) {
	var queued atomic.Int64
	m := newEgressManager(nil, &queued)
	defer m.close()
	peer := &pb.EgressPeer{NodeId: 2, Host: "exit.example", Port: 443, Token: "secret", Protocol: "tls_h2", Tls: &pb.TLSClientConfig{ServerName: "exit.example"}, Weight: 1}
	first := &pb.Rule{Id: 1, UserId: 1, QuotaEpoch: 10, IngressNodeId: 3, EgressGroupId: 4, Enabled: true, Protocol: "tcp", HealthIntervalSeconds: 60, EgressPeers: []*pb.EgressPeer{peer}}
	second := proto.Clone(first).(*pb.Rule)
	second.Id, second.UserId, second.QuotaEpoch = 2, 2, 20
	second.EgressPeers[0].Weight = 7
	second.EgressPeers[0].Standby = true
	third := proto.Clone(first).(*pb.Rule)
	third.Id, third.UserId, third.QuotaEpoch = 3, 3, 30
	third.EgressPeers[0].Tls.ServerName = "other.example"

	m.update([]*pb.Rule{first, second, third})
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.peers) != 2 {
		t.Fatalf("got %d distinct peer keys, want 2", len(m.peers))
	}
	for _, rule := range []*pb.Rule{first, second, third} {
		candidates := m.prepared[rule]
		if len(candidates) != 1 {
			t.Fatalf("rule %d has %d candidates", rule.Id, len(candidates))
		}
		c := candidates[0]
		p := rule.EgressPeers[0]
		if c.key != peerKey(peerRule(rule, p)) || c.rule.Id != rule.Id || c.rule.UserId != rule.UserId || c.rule.QuotaEpoch != rule.QuotaEpoch || c.weight != int64(p.Weight) || c.standby != p.Standby {
			t.Fatalf("rule %d inherited another rule's peer credentials or scheduling: %+v", rule.Id, c)
		}
	}
}
