package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	pb "github.com/nekopass/nekopass/internal/protocol"
	"github.com/nekopass/nekopass/internal/tunnel"
	"github.com/nekopass/nekopass/internal/tunnelcrypto"
)

type TunnelRandomConfig struct {
	Cipher string `json:"cipher"`
	Key    string `json:"key"`
}

func defaultTunnelRandom() TunnelRandomConfig {
	return TunnelRandomConfig{Cipher: tunnelcrypto.ChaCha20Poly1305}
}

func (v *TunnelRandomConfig) normalize(generate bool) error {
	if v.Cipher == "" {
		v.Cipher = tunnelcrypto.ChaCha20Poly1305
	}
	if !tunnelcrypto.ValidCipher(v.Cipher) {
		return errors.New("全随机加密算法无效")
	}
	v.Key = strings.TrimSpace(v.Key)
	if v.Key == "" {
		if generate {
			var err error
			v.Key, err = tunnelcrypto.GenerateKey()
			return err
		}
		return nil
	}
	key, err := tunnelcrypto.DecodeKey(v.Key)
	if err != nil {
		return errors.New("加密密钥须为 Base64 编码的 32 字节密钥；留空自动生成")
	}
	v.Key = base64.RawURLEncoding.EncodeToString(key)
	return nil
}

func prepareNodeRandom(ctx context.Context, tx pgx.Tx, id int64, input *NodeInput) error {
	cfg := defaultTunnelRandom()
	if input.Random != nil {
		cfg = *input.Random
	} else {
		var data []byte
		if err := tx.QueryRow(ctx, "SELECT random_config FROM nodes WHERE id=$1", id).Scan(&data); err != nil {
			return err
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return err
		}
	}
	if err := cfg.normalize(tunnel.Random(input.TunnelProtocol) || input.TunnelProtocol == "dtls_udp" || input.Random != nil); err != nil {
		return err
	}
	input.Random = &cfg
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "UPDATE nodes SET random_config=$2 WHERE id=$1", id, data)
	return err
}

func appendRandomControl(ctx context.Context, tx pgx.Tx, nodeID int64, out *pb.ControlMessage) error {
	// Retrieve only nodes whose protocol needs a random key, in one round trip.
	// Key generation remains local and happens lazily under the existing row lock.
	ids := make([]int64, 0)
	seen := make(map[int64]struct{})
	add := func(id int64) {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	if tunnel.Random(out.Node.TunnelProtocol) || out.Node.TunnelProtocol == "dtls_udp" {
		add(nodeID)
	}
	for _, rule := range out.Rules {
		if rule.EgressNodeId != 0 && (tunnel.Random(rule.TunnelProtocol) || rule.TunnelProtocol == "dtls_udp") {
			add(rule.EgressNodeId)
		}
	}
	configs := make(map[int64][]byte, len(ids))
	if len(ids) > 0 {
		rows, err := tx.Query(ctx, "SELECT id,random_config FROM nodes WHERE id=ANY($1::bigint[])", ids)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			var data []byte
			if err = rows.Scan(&id, &data); err != nil {
				rows.Close()
				return err
			}
			configs[id] = data
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	cache := map[int64]*pb.RandomConfig{}
	load := func(id int64) (*pb.RandomConfig, error) {
		if cfg, ok := cache[id]; ok {
			return cfg, nil
		}
		data, ok := configs[id]
		if !ok {
			return nil, pgx.ErrNoRows
		}
		cfg := defaultTunnelRandom()
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, err
		}
		if err := cfg.normalize(false); err != nil {
			return nil, err
		}
		if cfg.Key == "" {
			// The local node row is already locked by its control exchange. Never
			// initialize another node here: reciprocal ingress/exit pulls could
			// otherwise lock both rows in opposite order. Its own pull fills it.
			if id != nodeID {
				cache[id] = nil
				return nil, nil
			}
			// Existing DTLS installations also need an opaque outer envelope.
			// Initialize its node-level key once, preserving any concurrent winner.
			key, err := tunnelcrypto.GenerateKey()
			if err != nil {
				return nil, err
			}
			if _, err = tx.Exec(ctx, "UPDATE nodes SET random_config=jsonb_set(random_config,'{key}',to_jsonb($2::text)) WHERE id=$1 AND COALESCE(random_config->>'key','')=''", id, key); err != nil {
				return nil, err
			}
			if err = tx.QueryRow(ctx, "SELECT random_config FROM nodes WHERE id=$1", id).Scan(&data); err != nil {
				return nil, err
			}
			if err = json.Unmarshal(data, &cfg); err != nil {
				return nil, err
			}
			if err = cfg.normalize(false); err != nil {
				return nil, err
			}
			if cfg.Key == "" {
				return nil, errors.New("encrypted tunnel key not configured")
			}
		}
		p := &pb.RandomConfig{Cipher: cfg.Cipher, Key: cfg.Key}
		cache[id] = p
		return p, nil
	}
	var err error
	if tunnel.Random(out.Node.TunnelProtocol) || out.Node.TunnelProtocol == "dtls_udp" {
		if out.Node.Random, err = load(nodeID); err != nil {
			return err
		}
	}
	for _, rule := range out.Rules {
		if rule.EgressNodeId != 0 && (tunnel.Random(rule.TunnelProtocol) || rule.TunnelProtocol == "dtls_udp") {
			if rule.Random, err = load(rule.EgressNodeId); err != nil {
				return err
			}
			if rule.Random == nil {
				rule.Enabled = false
			}
		}
	}
	return nil
}
