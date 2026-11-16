package control

// nodeEgressRulesSQL starts from the requested exit and checks grants with
// EXISTS instead of expanding every permitted user-node combination in the
// rule_outbound_routes view. Keep it synchronized with that view and the
// differential authorization tests when route semantics change.
const nodeEgressRulesSQL = `WITH exit_rules AS (
 SELECT r.id,true AS single_exit FROM rules r WHERE r.egress_node_id=$1
 UNION ALL
 SELECT r.id,false AS single_exit
 FROM node_group_members m
 JOIN node_groups g ON g.id=m.group_id AND g.enabled
 JOIN rules r ON r.egress_group_id=g.id
 JOIN user_node_groups ug ON ug.user_id=r.user_id AND ug.group_id=g.id
 WHERE m.node_id=$1 AND m.role<>'disabled'
)
SELECT r.id,i.node_id,r.targets,r.user_id,u.quota_epoch,
 COALESCE(extract(epoch FROM LEAST(u.expires_at,u.next_reset_at))::bigint,0),r.protocol
FROM exit_rules er
JOIN rules r ON r.id=er.id
JOIN user_entitlements u ON u.id=r.user_id AND u.enabled AND (u.expires_at IS NULL OR u.expires_at>now())
JOIN nodes exit ON exit.id=$1 AND exit.enabled AND exit.tunnel_exit_enabled
JOIN rule_ingress_nodes i ON i.rule_id=r.id
JOIN nodes source ON source.id=i.node_id AND source.enabled AND source.ingress_enabled
JOIN node_tunnel_links l ON l.ingress_node_id=i.node_id AND l.egress_node_id=exit.id
LEFT JOIN node_groups ig ON ig.id=r.ingress_group_id
LEFT JOIN user_node_groups iug ON iug.group_id=r.ingress_group_id AND iug.user_id=r.user_id
WHERE r.enabled AND i.node_id<>exit.id
 AND (r.protocol='udp' OR exit.tunnel_protocol NOT IN ('plain_udp','dtls_udp','random_udp'))
 AND (r.ingress_group_id IS NULL OR ig.enabled AND iug.user_id IS NOT NULL AND $2::integer >= $3::integer)
 AND EXISTS (SELECT 1 FROM user_node_groups a JOIN node_groups g ON g.id=a.group_id AND g.enabled
             JOIN node_group_members m ON m.group_id=g.id AND m.node_id=i.node_id WHERE a.user_id=r.user_id)
 AND (NOT er.single_exit OR EXISTS (SELECT 1 FROM user_node_groups a JOIN node_groups g ON g.id=a.group_id AND g.enabled
             JOIN node_group_members m ON m.group_id=g.id AND m.node_id=exit.id WHERE a.user_id=r.user_id))
ORDER BY r.id,i.node_id`
