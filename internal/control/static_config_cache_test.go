package control

import (
	"bytes"
	"sync"
	"testing"
	"time"

	pb "github.com/nekopass/nekopass/internal/protocol"
	"google.golang.org/protobuf/proto"
)

func TestStaticConfigCacheExpiresAndBoundsMemory(t *testing.T) {
	budget := &staticCacheBudget{}
	c := &nodeStaticCache{budget: budget}
	v := staticConfigVersion{epoch: "epoch", count: 2}
	now := time.Now()
	expiry := now.Add(time.Minute)
	wire, err := proto.Marshal(&pb.ControlMessage{Node: &pb.NodeConfig{Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	c.put(v, now, &expiry, 38, wire)
	first := c.get(v, now.Add(time.Second), 38)
	if first == nil || !first.Node.Enabled {
		t.Fatal("expected cache hit")
	}
	first.Node.Enabled = false
	if !c.get(v, now.Add(time.Second), 38).Node.Enabled {
		t.Fatal("caller mutated cached bytes")
	}
	if c.get(v, expiry, 38) != nil || budget.used != 0 {
		t.Fatal("expired entry retained")
	}
	c.put(v, now, nil, 38, wire)
	if c.get(v, now.Add(-time.Second), 38) != nil {
		t.Fatal("database clock rollback reused stale entry")
	}
	c.put(v, now, nil, 38, wire)
	if c.get(staticConfigVersion{epoch: "different"}, now, 38) != nil {
		t.Fatal("different database version reused entry")
	}
	c.put(v, now, nil, 38, bytes.Repeat([]byte{'x'}, maxStaticCacheEntryBytes+1))
	if c.cached != nil || budget.used != 0 {
		t.Fatal("oversized entry retained")
	}

	// Separate streams reserve from a single server budget concurrently.
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := &nodeStaticCache{budget: budget}
			c.put(v, now, nil, 38, bytes.Repeat([]byte{'x'}, maxStaticCacheEntryBytes))
			c.clear()
		}()
	}
	wg.Wait()
	if budget.used != 0 {
		t.Fatalf("cache budget leaked: %d", budget.used)
	}
}

func TestStaticConfigTokenIncludesDynamicNodeAndRules(t *testing.T) {
	base := &pb.ControlMessage{Node: &pb.NodeConfig{Enabled: true}, Rules: []*pb.Rule{{Id: 1, Enabled: true}}}
	a, err := staticConfigToken(base)
	if err != nil {
		t.Fatal(err)
	}
	changed := proto.Clone(base).(*pb.ControlMessage)
	changed.Rules[0].Enabled = false
	b, err := staticConfigToken(changed)
	if err != nil || a == b {
		t.Fatal("rule change not reflected in token", err)
	}
	changed = proto.Clone(base).(*pb.ControlMessage)
	changed.Node.Ddns = &pb.DDNSConfig{Enabled: true}
	b, err = staticConfigToken(changed)
	if err != nil || a == b {
		t.Fatal("dynamic node change not reflected in token", err)
	}
}
