package control

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	pb "github.com/nekopass/nekopass/internal/protocol"
	"github.com/nekopass/nekopass/internal/tunnelcrypto"
)

func TestPeerConfigurationBatchPreservesPerExitAndMissingKeyBehavior(t *testing.T) {
	p := testDB(t)
	ctx := context.Background()
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	create := func(tls TunnelTLSConfig, random TunnelRandomConfig) int64 {
		t.Helper()
		tlsData, err := json.Marshal(tls)
		if err != nil {
			t.Fatal(err)
		}
		randomData, err := json.Marshal(random)
		if err != nil {
			t.Fatal(err)
		}
		var id int64
		if err := tx.QueryRow(ctx, "INSERT INTO nodes(name,token_hash,tls_config,tls_ca,random_config) VALUES($1,$2,$3,$4,$5) RETURNING id", Secret(), Secret(), tlsData, tls.ServerName, randomData).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	entry := create(DefaultTunnelTLS(), defaultTunnelRandom())
	key, err := tunnelcrypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	firstTLS := DefaultTunnelTLS()
	firstTLS.ServerName = "first.example.test"
	firstTLS.PublicPort = 443
	secondTLS := DefaultTunnelTLS()
	secondTLS.ServerName = "second.example.test"
	secondTLS.PublicPort = 8443
	first := create(firstTLS, TunnelRandomConfig{Cipher: tunnelcrypto.AES256GCM, Key: key})
	second := create(secondTLS, defaultTunnelRandom())
	out := &pb.ControlMessage{Node: &pb.NodeConfig{TunnelProtocol: "plain_tcp"}, Rules: []*pb.Rule{
		{EgressNodeId: first, TunnelProtocol: "tls_tcp", TunnelPort: 1111, Enabled: true},
		{EgressNodeId: second, TunnelProtocol: "dtls_udp", TunnelPort: 2222, Enabled: true},
		{EgressNodeId: first, TunnelProtocol: "random_h2", TunnelPort: 3333, Enabled: true},
		{EgressNodeId: first, TunnelProtocol: "plain_tcp", TunnelPort: 4444, Enabled: true},
	}}
	if err = appendTLSControl(ctx, tx, entry, out); err != nil {
		t.Fatal(err)
	}
	if err = appendRandomControl(ctx, tx, entry, out); err != nil {
		t.Fatal(err)
	}
	if out.Rules[0].Tls.GetServerName() != firstTLS.ServerName || out.Rules[1].Tls.GetServerName() != secondTLS.ServerName || out.Rules[3].Tls != nil {
		t.Fatal("exit TLS settings or protocol isolation changed")
	}
	for i, want := range []int32{443, 8443, 443, 443} {
		if out.Rules[i].TunnelPort != want {
			t.Fatalf("rule %d port = %d, want %d", i, out.Rules[i].TunnelPort, want)
		}
	}
	if out.Rules[2].Random.GetKey() != key || out.Rules[2].Random.GetCipher() != tunnelcrypto.AES256GCM || !out.Rules[2].Enabled {
		t.Fatal("first exit random key not assigned")
	}
	if out.Rules[1].Enabled || out.Rules[1].Random != nil {
		t.Fatal("exit without key must remain disabled")
	}
	var pendingKey string
	if err = tx.QueryRow(ctx, "SELECT COALESCE(random_config->>'key','') FROM nodes WHERE id=$1", second).Scan(&pendingKey); err != nil || pendingKey != "" {
		t.Fatal("another node's key was initialized", err)
	}
	missing := &pb.ControlMessage{Node: &pb.NodeConfig{}, Rules: []*pb.Rule{{EgressNodeId: -1, TunnelProtocol: "random_tcp"}}}
	if err = appendTLSControl(ctx, tx, entry, missing); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing TLS exit: %v", err)
	}
	if err = appendRandomControl(ctx, tx, entry, missing); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing random exit: %v", err)
	}
}
