package control

import (
	"context"
	"testing"
	"time"

	pb "github.com/nekopass/nekopass/internal/protocol"
	"google.golang.org/protobuf/proto"
)

// A second, cold server is a reference implementation of a full configuration
// build against the same database. Comparing it with a previously warmed
// server exercises changes made outside that server, as in a panel cluster.
func TestConfigCacheCrossServerDifferential(t *testing.T) {
	p := testDB(t)
	ctx := context.Background()
	user, plan := testUser(t, p, -1, 10, false)
	var ingress, exit int64
	for _, dst := range []*int64{&ingress, &exit} {
		if err := p.QueryRow(ctx, `INSERT INTO nodes(name,token_hash,protocol_version,ingress_enabled,tunnel_exit_enabled,tunnel_public_host,tunnel_listen_port) VALUES($1,$2,$3,true,true,'exit.example',24443) RETURNING id`, Secret(), Secret(), pb.CurrentVersion).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _, _ = p.Exec(ctx, "DELETE FROM nodes WHERE id=ANY($1::bigint[])", []int64{ingress, exit}) })
	group := testAuthorize(t, p, user, ingress)
	exitGroup := testAuthorize(t, p, user, exit)
	if _, err := p.Exec(ctx, `INSERT INTO node_tunnel_links(ingress_node_id,egress_node_id,token) VALUES($1,$2,'test-tunnel-token')`, ingress, exit); err != nil {
		t.Fatal(err)
	}
	var rule int64
	if err := p.QueryRow(ctx, `INSERT INTO rules(user_id,node_id,egress_node_id,listen_port,target_host,target_port) VALUES($1,$2,$3,32123,'example.com',443) RETURNING id`, user, ingress, exit).Scan(&rule); err != nil {
		t.Fatal(err)
	}
	stable := Secret()
	report := &pb.AgentMessage{InstanceId: stable, ProtocolVersion: pb.CurrentVersion, SupportsConfigDelta: true}
	warm := []*StreamServer{{Server: New(p)}, {Server: New(p)}}
	caches := []*nodeStaticCache{{budget: &warm[0].Server.staticCacheBudget}, {budget: &warm[1].Server.staticCacheBudget}}
	exitCaches := []*nodeStaticCache{{budget: &warm[0].Server.staticCacheBudget}, {budget: &warm[1].Server.staticCacheBudget}}
	t.Cleanup(func() {
		for _, cache := range caches {
			cache.clear()
		}
		for _, cache := range exitCaches {
			cache.clear()
		}
	})
	check := func(label string) {
		t.Helper()
		fresh := &StreamServer{Server: New(p)}
		want, err := fresh.exchange(ctx, p, ingress, proto.Clone(report).(*pb.AgentMessage))
		if err != nil {
			t.Fatalf("%s: fresh exchange: %v", label, err)
		}
		exitReport := proto.Clone(report).(*pb.AgentMessage)
		if err := p.QueryRow(ctx, "SELECT protocol_version FROM nodes WHERE id=$1", exit).Scan(&exitReport.ProtocolVersion); err != nil {
			t.Fatal(err)
		}
		exitWant, err := fresh.exchange(ctx, p, exit, exitReport)
		if err != nil {
			t.Fatalf("%s: fresh exit exchange: %v", label, err)
		}
		for i, server := range warm {
			got, err := server.exchangeWithLock(ctx, p, ingress, proto.Clone(report).(*pb.AgentMessage), "", nil, caches[i])
			if err != nil {
				t.Fatalf("%s: warm exchange %d: %v", label, i, err)
			}
			if caches[i].cached == nil {
				t.Fatalf("%s: server %d did not retain a snapshot", label, i)
			}
			if !proto.Equal(got, want) {
				t.Fatalf("%s: warm server %d and fresh snapshots differ\nwarm: %v\nfresh: %v", label, i, got, want)
			}
			exitGot, err := server.exchangeWithLock(ctx, p, exit, proto.Clone(exitReport).(*pb.AgentMessage), "", nil, exitCaches[i])
			if err != nil {
				t.Fatalf("%s: warm exit exchange %d: %v", label, i, err)
			}
			if exitCaches[i].cached == nil || !proto.Equal(exitGot, exitWant) {
				t.Fatalf("%s: warm exit server %d differs or has no cache\nwarm: %v\nfresh: %v", label, i, exitGot, exitWant)
			}
		}
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := p.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	check("initial")
	oldSnapshots := [2]any{caches[0].cached, caches[1].cached}
	check("unchanged")
	for i := range caches {
		if caches[i].cached != oldSnapshots[i] {
			t.Fatalf("unchanged config was rebuilt on server %d", i)
		}
	}
	report.ProtocolVersion = pb.CurrentVersion - 1
	check("local protocol downgrade")
	report.ProtocolVersion = pb.CurrentVersion
	check("local protocol upgrade")
	exec("UPDATE users SET quota_bytes=1000000 WHERE id=$1", user)
	report.RequestUsers = []int64{user}
	check("finite grant request")
	report.RequestUsers = nil
	oldSnapshots = [2]any{caches[0].cached, caches[1].cached}
	exec("UPDATE grants SET issued=issued-1 WHERE node_id=$1 AND user_id=$2 AND issued>0", ingress, user)
	check("dynamic issued quota without static invalidation")
	for i := range caches {
		if caches[i].cached != oldSnapshots[i] {
			t.Fatalf("grant-only change unnecessarily rebuilt server %d snapshot", i)
		}
	}
	exec("UPDATE users SET quota_bytes=-1 WHERE id=$1", user)
	check("return to unlimited quota")
	exec("UPDATE rules SET target_host='other.example' WHERE id=$1", rule)
	check("rule change")
	exec("UPDATE nodes SET tunnel_public_host='new-exit.example' WHERE id=$1", exit)
	check("peer address change")
	exec("UPDATE node_tunnel_links SET token='new-test-token' WHERE ingress_node_id=$1 AND egress_node_id=$2", ingress, exit)
	check("tunnel secret change")
	exec("UPDATE nodes SET protocol_version=30 WHERE id=$1", exit)
	check("peer protocol downgrade")
	exec("UPDATE nodes SET protocol_version=$2 WHERE id=$1", exit, pb.CurrentVersion)
	check("peer protocol upgrade")
	exec("UPDATE rules SET egress_node_id=NULL,egress_group_id=$2 WHERE id=$1", rule, exitGroup)
	check("group exit route")
	exec("UPDATE node_group_members SET weight=3 WHERE group_id=$1 AND node_id=$2", exitGroup, exit)
	check("exit group weight")
	exec("UPDATE node_group_members SET role='disabled' WHERE group_id=$1 AND node_id=$2", exitGroup, exit)
	check("disabled exit group member")
	exec("UPDATE node_group_members SET role='active' WHERE group_id=$1 AND node_id=$2", exitGroup, exit)
	check("restored exit group member")
	exec("UPDATE rules SET egress_node_id=$2,egress_group_id=NULL WHERE id=$1", rule, exit)
	check("restored single exit route")
	exec("UPDATE users SET rule_speed_bps=1250000,quota_epoch=quota_epoch+1 WHERE id=$1", user)
	check("user limits and quota epoch")
	exec("UPDATE plans SET enabled=false WHERE id=$1", plan)
	check("disabled plan")
	exec("UPDATE plans SET enabled=true WHERE id=$1", plan)
	check("reenabled plan")
	exec("UPDATE node_groups SET enabled=false WHERE id=$1", group)
	check("revoked ingress group")
	exec("UPDATE node_groups SET enabled=true WHERE id=$1", group)
	check("restored ingress group")
	exec("DELETE FROM user_node_groups WHERE user_id=$1 AND group_id=$2", user, group)
	check("removed group permission")
	exec("INSERT INTO user_node_groups(user_id,group_id) VALUES($1,$2)", user, group)
	check("restored group permission")
	exec("UPDATE node_group_members SET weight=2 WHERE group_id=$1 AND node_id=$2", group, ingress)
	check("group member change")
	exec("UPDATE site_settings SET config=jsonb_set(config,'{proxy_trusted_cidrs}','[\"192.0.2.0/25\"]'::jsonb) WHERE id=1")
	check("global security policy")
	exec("UPDATE nodes SET enabled=false WHERE id=$1", exit)
	check("disabled peer")
	exec("UPDATE nodes SET enabled=true WHERE id=$1", exit)
	check("reenabled peer")
	oldSnapshots = [2]any{caches[0].cached, caches[1].cached}
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "UPDATE rules SET target_host='rolled-back.example' WHERE id=$1", rule); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	check("rolled back rule change")
	for i := range caches {
		if caches[i].cached != oldSnapshots[i] {
			t.Fatalf("rolled back transaction invalidated server %d snapshot", i)
		}
	}
	// A challenge can expire without a DELETE from the worker.
	exec("UPDATE nodes SET tunnel_protocol='tls_tcp' WHERE id=$1", ingress)
	exec("INSERT INTO node_acme_challenges(node_id,domain,token,key_authorization,revision,expires_at) VALUES($1,'acme.example','token','key',1,now()+interval '1 second')", ingress)
	check("before challenge expiry")
	time.Sleep(1500 * time.Millisecond)
	check("after challenge expiry without writes")
	exec("DELETE FROM node_acme_challenges WHERE node_id=$1", ingress)
	check("removed ACME challenge")
	// The credential check must run even if a static snapshot is cached.
	oldToken, newToken := Secret(), Secret()
	exec("UPDATE nodes SET token_hash=$2 WHERE id=$1", ingress, Hash(oldToken))
	check("node token change")
	exec("UPDATE nodes SET token_hash=$2 WHERE id=$1", ingress, Hash(newToken))
	for i, server := range warm {
		if _, err := server.exchangeWithLock(ctx, p, ingress, proto.Clone(report).(*pb.AgentMessage), Hash(oldToken), nil, caches[i]); err == nil {
			t.Fatalf("server %d accepted revoked token on cached stream", i)
		}
	}
	// With no database write at expiry, the cached rule must still disappear.
	exec("UPDATE users SET resource_expires_at=now()+interval '1 second' WHERE id=$1", user)
	check("before expiry")
	time.Sleep(1500 * time.Millisecond)
	check("after expiry without writes")
}
