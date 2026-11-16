package control

import (
	"context"
	"testing"

	pb "github.com/nekopass/nekopass/internal/protocol"
)

// Reports use cumulative counters and are repeated until acknowledged. An
// identical report must not update the grant tuple, but transitions closing an
// unlimited grant must not be mistaken for no-ops.
func TestHeartbeatUsageReplayDoesNotRewriteGrant(t *testing.T) {
	p := testDB(t)
	ctx := context.Background()
	user, _ := testUser(t, p, 1_000_000, 10, false)
	var node int64
	if err := p.QueryRow(ctx, "INSERT INTO nodes(name,token_hash) VALUES($1,$2) RETURNING id", Secret(), Secret()).Scan(&node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = p.Exec(ctx, "DELETE FROM nodes WHERE id=$1", node) })
	testAuthorize(t, p, user, node)
	s := &StreamServer{Server: New(p)}
	instance := Secret()
	if _, err := s.exchange(ctx, p, node, &pb.AgentMessage{InstanceId: instance, ProtocolVersion: 35, RequestUsers: []int64{user}}); err != nil {
		t.Fatal(err)
	}
	usage := &pb.Usage{UserId: user, Spent: 200, Traffic: 100, ChargedBytes: 100}
	report := &pb.AgentMessage{InstanceId: instance, ProtocolVersion: 35, Usage: []*pb.Usage{usage}}
	if _, err := s.exchange(ctx, p, node, report); err != nil {
		t.Fatal(err)
	}
	grantVersion := func() string {
		t.Helper()
		var version string
		if err := p.QueryRow(ctx, "SELECT xmin::text FROM grants WHERE node_id=$1 AND user_id=$2 AND quota_epoch=0", node, user).Scan(&version); err != nil {
			t.Fatal(err)
		}
		return version
	}
	before := grantVersion()
	for i := 0; i < 3; i++ {
		out, err := s.exchange(ctx, p, node, report)
		if err != nil {
			t.Fatal(err)
		}
		if len(out.AcknowledgedUsage) != 1 || out.AcknowledgedUsage[0].Traffic != usage.Traffic {
			t.Fatalf("usage replay not acknowledged: %+v", out.AcknowledgedUsage)
		}
	}
	if after := grantVersion(); after != before {
		t.Fatalf("identical reports rewrote grant: xmin %s -> %s", before, after)
	}
	if _, err := p.Exec(ctx, "UPDATE grants SET unlimited_open=true WHERE node_id=$1 AND user_id=$2 AND quota_epoch=0", node, user); err != nil {
		t.Fatal(err)
	}
	if _, err := s.exchange(ctx, p, node, report); err != nil {
		t.Fatal(err)
	}
	var open bool
	if err := p.QueryRow(ctx, "SELECT unlimited_open FROM grants WHERE node_id=$1 AND user_id=$2 AND quota_epoch=0", node, user).Scan(&open); err != nil || open {
		t.Fatalf("finite report failed to close unlimited grant: open=%v err=%v", open, err)
	}
}
