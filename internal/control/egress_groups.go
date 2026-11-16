package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	pb "github.com/nekopass/nekopass/internal/protocol"
)

type nodeGroupMember struct {
	NodeID int64  `json:"node_id"`
	Role   string `json:"role"`
	Weight int    `json:"weight"`
}
type nodeGroupInput struct {
	Name                  string            `json:"name"`
	GroupType             string            `json:"group_type"`
	ConnectionAddress     *string           `json:"connection_address"`
	Description           string            `json:"description"`
	Enabled               bool              `json:"enabled"`
	SortOrder             int               `json:"sort_order"`
	NodeIDs               []int64           `json:"node_ids"`
	Members               []nodeGroupMember `json:"members"`
	Strategy              string            `json:"strategy"`
	HealthIntervalSeconds int               `json:"health_interval_seconds"`
	HealthFailures        int               `json:"health_failures"`
	HealthSuccesses       int               `json:"health_successes"`
}

func (v *nodeGroupInput) normalize() error {
	if v.Strategy != "round_robin" && v.Strategy != "random" && v.Strategy != "least_connections" {
		return errors.New("节点组负载策略无效")
	}
	if v.HealthIntervalSeconds < 2 || v.HealthIntervalSeconds > 60 || v.HealthFailures < 1 || v.HealthFailures > 20 || v.HealthSuccesses < 1 || v.HealthSuccesses > 20 {
		return errors.New("健康检查参数无效")
	}
	ids := map[int64]bool{}
	for _, id := range v.NodeIDs {
		if id <= 0 || ids[id] {
			return errors.New("组内节点无效或重复")
		}
		ids[id] = true
	}
	seen := map[int64]bool{}
	for _, m := range v.Members {
		if !ids[m.NodeID] || seen[m.NodeID] || m.Weight < 1 || m.Weight > 1000 || m.Role != "active" && m.Role != "standby" && m.Role != "disabled" {
			return errors.New("节点职责或权重无效")
		}
		seen[m.NodeID] = true
	}
	return nil
}

// Only expose groups explicitly assigned to the account, without credentials.
func (s *Server) ruleEgressGroups(w http.ResponseWriter, r *http.Request) {
	u := ruleActor(r)
	rows, err := s.Pool.Query(r.Context(), `SELECT g.id,g.name,g.strategy,COALESCE((SELECT array_agg(m.node_id ORDER BY m.node_id) FROM node_group_members m JOIN user_nodes un ON un.node_id=m.node_id AND un.user_id=$1 JOIN nodes n ON n.id=m.node_id AND n.tunnel_exit_enabled WHERE m.group_id=g.id AND m.role<>'disabled'),'{}') AS node_ids FROM node_groups g JOIN user_node_groups ug ON ug.group_id=g.id AND ug.user_id=$1 WHERE g.enabled AND g.group_type IN ('legacy','egress') ORDER BY g.sort_order,g.id`, u.ID)
	s.sendRows(w, rows, err)
}

func (s *Server) ruleIngressGroups(w http.ResponseWriter, r *http.Request) {
	u := ruleActor(r)
	rows, err := s.Pool.Query(r.Context(), `SELECT g.id,g.name,g.connection_address,COALESCE((SELECT array_agg(n.id ORDER BY n.id) FROM node_group_members m JOIN nodes n ON n.id=m.node_id AND n.ingress_enabled WHERE m.group_id=g.id),'{}') AS node_ids,COALESCE((SELECT jsonb_agg(jsonb_build_object('id',n.id,'name',n.name,'enabled',n.enabled,'port_min',n.port_min,'port_max',n.port_max,'allow_direct',n.allow_direct,'deny_udp_ingress_creation',n.deny_udp_ingress_creation,'ingress_multiplier',to_char(n.ingress_rate::numeric/10000,'FM9990.0000')) ORDER BY n.id) FROM node_group_members m JOIN nodes n ON n.id=m.node_id AND n.ingress_enabled WHERE m.group_id=g.id),'[]') AS members FROM node_groups g JOIN user_node_groups ug ON ug.group_id=g.id AND ug.user_id=$1 WHERE g.enabled AND g.group_type IN ('legacy','ingress') ORDER BY g.sort_order,g.id`, u.ID)
	s.sendRows(w, rows, err)
}

// Keep the legacy node reference on a current member whenever one exists.
// Empty groups retain their last reference until another member is added.
func reanchorIngressRules(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `UPDATE rules r SET node_id=member.node_id FROM (SELECT ri.rule_id,min(ri.node_id) AS node_id FROM rule_ingress_nodes ri JOIN rules x ON x.id=ri.rule_id AND x.ingress_group_id IS NOT NULL GROUP BY ri.rule_id) member WHERE r.id=member.rule_id AND r.node_id<>member.node_id`)
	return err
}

// A newly added ingress must satisfy every rule already assigned to its group.
// The stream also validates routes, so later permission changes fail closed.
func validateNewIngressMember(ctx context.Context, tx pgx.Tx, groupID, nodeID int64) error {
	var ruleID int64
	err := tx.QueryRow(ctx, `SELECT r.id FROM rules r JOIN nodes n ON n.id=$2 AND n.ingress_enabled AND n.enabled WHERE r.ingress_group_id=$1 AND (r.protocol='udp' AND n.deny_udp_ingress_creation OR r.egress_node_id IS NULL AND r.egress_group_id IS NULL AND NOT n.allow_direct OR (r.egress_node_id IS NOT NULL OR r.egress_group_id IS NOT NULL) AND EXISTS(SELECT 1 FROM user_nodes un WHERE un.user_id=r.user_id AND un.node_id=n.id) AND NOT EXISTS(SELECT 1 FROM rule_outbound_routes o WHERE o.rule_id=r.id AND o.ingress_node_id=n.id)) ORDER BY r.id LIMIT 1`, groupID, nodeID).Scan(&ruleID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("节点 %d 不符合入口组已有规则 %d 的协议或出口配置，请先调整节点与出口关联", nodeID, ruleID)
}

type groupRuleDraft struct {
	rule    *pb.Rule
	peer    *pb.Rule
	weight  int32
	standby bool
}

// Reuse the single-exit TLS/capability checks for every candidate. Draft rules
// are removed before the control message leaves the server.
func appendGroupDrafts(ctx context.Context, tx pgx.Tx, out *pb.ControlMessage) ([]groupRuleDraft, error) {
	rules := map[int64]*pb.Rule{}
	var ruleIDs, groupIDs []int64
	for _, r := range out.Rules {
		if r.EgressGroupId != 0 {
			rules[r.Id] = r
			ruleIDs = append(ruleIDs, r.Id)
			groupIDs = append(groupIDs, r.EgressGroupId)
		}
	}
	if len(ruleIDs) == 0 {
		return nil, nil
	}
	type groupPolicy struct {
		strategy                      string
		interval, failures, successes int32
	}
	groups := map[int64]groupPolicy{}
	rows, err := tx.Query(ctx, `SELECT id,strategy,health_interval_seconds,health_failures,health_successes FROM node_groups WHERE id=ANY($1::bigint[])`, groupIDs)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var v groupPolicy
		if err = rows.Scan(&id, &v.strategy, &v.interval, &v.failures, &v.successes); err != nil {
			rows.Close()
			return nil, err
		}
		groups[id] = v
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, r := range rules {
		g := groups[r.EgressGroupId]
		r.EgressStrategy = g.strategy
		r.HealthIntervalSeconds = g.interval
		r.HealthFailures = g.failures
		r.HealthSuccesses = g.successes
	}
	rows, err = tx.Query(ctx, `SELECT o.rule_id,n.id,n.tunnel_public_host,n.tunnel_listen_port,l.token,n.tunnel_protocol,o.weight,o.role='standby' FROM rule_outbound_routes o JOIN rules r ON r.id=o.rule_id JOIN nodes n ON n.id=o.egress_node_id JOIN node_tunnel_links l ON l.ingress_node_id=o.ingress_node_id AND l.egress_node_id=n.id WHERE o.rule_id=ANY($1::bigint[]) AND o.ingress_node_id=$2 AND n.protocol_version>=24 AND (r.ingress_group_id IS NULL OR n.protocol_version >= $3) ORDER BY o.rule_id,n.id`, ruleIDs, out.Node.NodeId, pb.MultiIngressVersion)
	if err != nil {
		return nil, err
	}
	var drafts []groupRuleDraft
	for rows.Next() {
		var id int64
		var candidate pb.EgressPeer
		if err = rows.Scan(&id, &candidate.NodeId, &candidate.Host, &candidate.Port, &candidate.Token, &candidate.Protocol, &candidate.Weight, &candidate.Standby); err != nil {
			rows.Close()
			return nil, err
		}
		r := rules[id]
		if r == nil || candidate.Host == "" || candidate.Port == 0 || candidate.Token == "" {
			continue
		}
		// The draft is used only to resolve transport/capability settings. Do
		// not duplicate targets, SNI lists or origin certificates per exit.
		p := &pb.Rule{Id: r.Id, UserId: r.UserId, IngressNodeId: r.IngressNodeId, QuotaEpoch: r.QuotaEpoch, Protocol: r.Protocol, Enabled: r.Enabled}
		p.EgressNodeId = candidate.NodeId
		p.TunnelHost = candidate.Host
		p.TunnelPort = candidate.Port
		p.TunnelToken = candidate.Token
		p.TunnelProtocol = candidate.Protocol
		drafts = append(drafts, groupRuleDraft{rule: r, peer: p, weight: candidate.Weight, standby: candidate.Standby})
		out.Rules = append(out.Rules, p)
	}
	err = rows.Err()
	rows.Close()
	return drafts, err
}

func finishGroupDrafts(out *pb.ControlMessage, drafts []groupRuleDraft, version int32, originalCount int) {
	for _, d := range drafts {
		p := d.peer
		if p.Enabled {
			d.rule.EgressPeers = append(d.rule.EgressPeers, &pb.EgressPeer{NodeId: p.EgressNodeId, Host: p.TunnelHost, Port: p.TunnelPort, Token: p.TunnelToken, Protocol: p.TunnelProtocol, Tls: p.Tls, Random: p.Random, Congestion: p.Congestion, Mux: p.Mux, Weight: d.weight, Standby: d.standby})
		}
	}
	if len(out.Rules) > originalCount {
		clear(out.Rules[originalCount:])
		// A large candidate matrix must not remain in the backing array of
		// the small final rule list while the node receives its configuration.
		out.Rules = append([]*pb.Rule(nil), out.Rules[:originalCount]...)
	}
	for _, r := range out.Rules {
		if r.EgressGroupId != 0 && (version < 24 || len(r.EgressPeers) == 0) {
			r.Enabled = false
		}
	}
}
