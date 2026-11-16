package control

// groupCandidatesSQL implements the group branch of rule_outbound_routes for
// a single ingress. Group assignment itself grants access to every enabled
// member, so joining user_nodes would repeatedly expand the same memberships.
// Keep this in sync with the view and its differential authorization test.
const groupCandidatesSQL = `SELECT r.id,n.id,n.tunnel_public_host,n.tunnel_listen_port,l.token,n.tunnel_protocol,m.weight,m.role='standby'
FROM rules r
JOIN rule_ingress_nodes i ON i.rule_id=r.id AND i.node_id=$2
JOIN user_entitlements u ON u.id=r.user_id AND u.enabled AND (u.expires_at IS NULL OR u.expires_at>now())
JOIN user_node_groups ug ON ug.user_id=r.user_id AND ug.group_id=r.egress_group_id
JOIN node_groups g ON g.id=ug.group_id AND g.enabled
JOIN node_group_members m ON m.group_id=g.id AND m.role<>'disabled'
JOIN nodes n ON n.id=m.node_id AND n.enabled AND n.tunnel_exit_enabled AND n.id<>i.node_id
JOIN node_tunnel_links l ON l.ingress_node_id=i.node_id AND l.egress_node_id=n.id
WHERE r.id=ANY($1::bigint[]) AND n.protocol_version>=24
 AND (r.ingress_group_id IS NULL OR n.protocol_version >= $3)
 AND (r.protocol='udp' OR n.tunnel_protocol NOT IN ('plain_udp','dtls_udp','random_udp'))
ORDER BY r.id,n.id`
