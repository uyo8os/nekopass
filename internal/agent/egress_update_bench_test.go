package agent

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	pb "github.com/nekopass/nekopass/internal/protocol"
)

// Repeated full snapshots with many rules sharing an exit group, as seen on a
// busy entry node. Each rule has separate peer messages as after decoding.
func BenchmarkEgressUpdateSharedGroup(b *testing.B) {
	var queued atomic.Int64
	m := newEgressManager(nil, &queued)
	defer m.close()
	rules := make([]*pb.Rule, 100)
	for i := range rules {
		r := &pb.Rule{Id: int64(i + 1), UserId: int64(i + 1), IngressNodeId: 1, EgressGroupId: 1, Enabled: true, Protocol: "tcp", HealthIntervalSeconds: 60}
		for n := 1; n <= 57; n++ {
			r.EgressPeers = append(r.EgressPeers, &pb.EgressPeer{NodeId: int64(n + 1), Host: fmt.Sprintf("exit-%d.example.com", n), Port: 443, Token: strings.Repeat("a", 64), Protocol: "tls_h2", Weight: 1, Tls: &pb.TLSClientConfig{ServerName: fmt.Sprintf("exit-%d.example.com", n), RootCa: strings.Repeat("certificate", 200)}})
		}
		rules[i] = r
	}
	m.update(rules)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.update(rules)
	}
}
