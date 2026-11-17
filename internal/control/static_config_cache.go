package control

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pb "github.com/nekopass/nekopass/internal/protocol"
	"google.golang.org/protobuf/proto"
)

// A cache belongs to one authenticated stream. The budget is shared by all
// streams on this server so a large deployment cannot retain unbounded snapshots.
const maxStaticCacheBytes = 8 << 20
const maxStaticCacheEntryBytes = 512 << 10

type staticCacheBudget struct {
	mu          sync.Mutex
	used        int
	lastCompact time.Time
}

type staticConfigVersion struct {
	epoch string
	count int64
	hash1 int64
	hash2 int64
}

type nodeStaticCache struct {
	budget    *staticCacheBudget
	version   staticConfigVersion
	builtAt   time.Time
	expiresAt *time.Time
	versionID int32
	cached    *staticCacheEntry
	bytes     int
}

type staticCacheEntry struct{ data []byte }

func readStaticConfigVersion(ctx context.Context, tx pgx.Tx) (staticConfigVersion, time.Time, error) {
	var version staticConfigVersion
	var now time.Time
	err := tx.QueryRow(ctx, `SELECT e.epoch, x.n, x.h1, x.h2, statement_timestamp()
 FROM static_config_epoch e CROSS JOIN (
  SELECT count(*) AS n, coalesce(bit_xor(hashtextextended(id::text,0)),0) AS h1,
   coalesce(bit_xor(hashtextextended(id::text,1)),0) AS h2 FROM static_config_events
 ) x WHERE e.id=1`).Scan(&version.epoch, &version.count, &version.hash1, &version.hash2, &now)
	return version, now, err
}

// Both authorization and ACME challenges can change just because time passes.
// Read their next boundary using the database clock, not the panel clock.
func staticConfigNextExpiry(ctx context.Context, tx pgx.Tx) (*time.Time, error) {
	var expiry *time.Time
	err := tx.QueryRow(ctx, `SELECT LEAST(
 (SELECT min(expires_at) FROM user_entitlements WHERE expires_at>now()),
 (SELECT min(expires_at) FROM node_acme_challenges WHERE expires_at>now())
 )`).Scan(&expiry)
	return expiry, err
}

func (c *nodeStaticCache) get(v staticConfigVersion, now time.Time, protocol int32) *pb.ControlMessage {
	if c == nil || c.cached == nil {
		return nil
	}
	if c.version != v || c.versionID != protocol || now.Before(c.builtAt) || c.expiresAt != nil && !now.Before(*c.expiresAt) {
		c.clear()
		return nil
	}
	out := &pb.ControlMessage{}
	if err := proto.Unmarshal(c.cached.data, out); err != nil {
		c.clear()
		return nil
	}
	return out
}

// Event compaction is rare and asynchronous. It never runs inside the node's
// accounting transaction, and the database function rotates the epoch in the
// same transaction as deletion. Timeout bounds contention with config edits.
func (b *staticCacheBudget) maybeCompact(pool *pgxpool.Pool, count int64) {
	if pool == nil || count < 512 {
		return
	}
	b.mu.Lock()
	if time.Since(b.lastCompact) < time.Minute {
		b.mu.Unlock()
		return
	}
	b.lastCompact = time.Now()
	b.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, "SELECT compact_static_config_events()"); err != nil {
			slog.Warn("cannot compact static configuration events", "error", err)
		}
	}()
}

// RunStaticConfigMaintenance bounds change history even if every connected
// agent uses the legacy protocol (or no agents are currently connected).
func (s *Server) RunStaticConfigMaintenance(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var count int64
			if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM static_config_events").Scan(&count); err != nil {
				if ctx.Err() == nil {
					slog.Warn("cannot inspect static configuration events", "error", err)
				}
				continue
			}
			s.staticCacheBudget.maybeCompact(s.Pool, count)
		}
	}
}

func (c *nodeStaticCache) clear() {
	if c == nil || c.cached == nil {
		return
	}
	c.budget.mu.Lock()
	c.budget.used -= c.bytes
	c.budget.mu.Unlock()
	c.cached, c.expiresAt, c.bytes = nil, nil, 0
}

func (c *nodeStaticCache) put(v staticConfigVersion, builtAt time.Time, expiry *time.Time, protocol int32, data []byte) {
	if c == nil {
		return
	}
	size := len(data)
	c.clear()
	if size > maxStaticCacheEntryBytes {
		return
	}
	c.budget.mu.Lock()
	if c.budget.used+size > maxStaticCacheBytes {
		c.budget.mu.Unlock()
		return
	}
	c.budget.used += size
	c.budget.mu.Unlock()
	c.version, c.builtAt, c.expiresAt, c.versionID, c.bytes = v, builtAt, expiry, protocol, size
	c.cached = &staticCacheEntry{data: data}
}

// These fields are the output of configuration construction before metering
// modifies the rules. User policies, acknowledgements and commands are dynamic.
func staticSnapshot(out *pb.ControlMessage) *pb.ControlMessage {
	return &pb.ControlMessage{Node: out.Node, Rules: out.Rules, TunnelLinks: out.TunnelLinks, EgressRules: out.EgressRules}
}

// Token the final configuration after dynamic metering/DDNS changes. Framing
// each message avoids concatenation ambiguity without allocating the entire
// potentially large snapshot as a single wire buffer.
func staticConfigToken(out *pb.ControlMessage) (string, error) {
	h := sha256.New()
	var length [binary.MaxVarintLen64]byte
	var buf []byte
	write := func(m proto.Message) error {
		var err error
		buf, err = (proto.MarshalOptions{Deterministic: true}).MarshalAppend(buf[:0], m)
		if err != nil {
			return err
		}
		n := binary.PutUvarint(length[:], uint64(len(buf)))
		_, _ = h.Write(length[:n])
		_, _ = h.Write(buf)
		return nil
	}
	writeCount := func(n int) {
		k := binary.PutUvarint(length[:], uint64(n))
		_, _ = h.Write(length[:k])
	}
	if err := write(out.Node); err != nil {
		return "", err
	}
	writeCount(len(out.Rules))
	for _, v := range out.Rules {
		if err := write(v); err != nil {
			return "", err
		}
	}
	writeCount(len(out.TunnelLinks))
	for _, v := range out.TunnelLinks {
		if err := write(v); err != nil {
			return "", err
		}
	}
	writeCount(len(out.EgressRules))
	for _, v := range out.EgressRules {
		if err := write(v); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
