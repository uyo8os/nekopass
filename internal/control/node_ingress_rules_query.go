package control

// nodeIngressRulesSQL selects the rules and effective settings for one ingress.
// The outer user_nodes join already verifies account/plan/expiry eligibility.
// Therefore route authorization only needs to check the user's enabled group
// membership; the selected exit node is also checked for enabled status.
// Keep these route checks in sync with rule_outbound_routes in migration043.sql.
const nodeIngressRulesSQL = `SELECT r.id,r.user_id,nn.listen_host,r.listen_port,r.target_host,r.target_port,(r.enabled AND (r.ingress_group_id IS NULL OR r.egress_node_id IS NULL OR en.protocol_version >= $2) AND CASE
 WHEN r.egress_node_id IS NULL AND r.egress_group_id IS NULL THEN nn.allow_direct
 WHEN r.egress_node_id IS NOT NULL THEN EXISTS (
  SELECT 1 FROM nodes dest
  JOIN node_tunnel_links route ON route.ingress_node_id=ri.node_id AND route.egress_node_id=dest.id
  WHERE dest.id=r.egress_node_id AND dest.enabled AND dest.tunnel_exit_enabled AND dest.id<>ri.node_id
   AND (r.protocol='udp' OR dest.tunnel_protocol NOT IN ('plain_udp','dtls_udp','random_udp'))
   AND EXISTS (SELECT 1 FROM user_node_groups permission
    JOIN node_groups permission_group ON permission_group.id=permission.group_id AND permission_group.enabled
    JOIN node_group_members permission_member ON permission_member.group_id=permission_group.id AND permission_member.node_id=dest.id
    WHERE permission.user_id=r.user_id)
 )
 ELSE EXISTS (
  SELECT 1 FROM user_node_groups permission
  JOIN node_groups exit_group ON exit_group.id=permission.group_id AND exit_group.enabled
  JOIN node_group_members member ON member.group_id=exit_group.id AND member.role<>'disabled'
  JOIN nodes dest ON dest.id=member.node_id AND dest.enabled AND dest.tunnel_exit_enabled AND dest.id<>ri.node_id
  JOIN node_tunnel_links route ON route.ingress_node_id=ri.node_id AND route.egress_node_id=dest.id
  WHERE permission.user_id=r.user_id AND permission.group_id=r.egress_group_id
   AND (r.protocol='udp' OR dest.tunnel_protocol NOT IN ('plain_udp','dtls_udp','random_udp'))
 ) END),r.targets,r.balance,
 COALESCE(LEAST(NULLIF(r.speed_mbps*125000,0),NULLIF(u.rule_speed_bps,0)),0),
 COALESCE(LEAST(NULLIF(r.ip_limit,0),NULLIF(u.rule_ip_limit,0)),0),
	 COALESCE(LEAST(NULLIF(r.connection_limit,0),NULLIF(u.rule_connection_limit,0)),0),r.proxy_accept,r.proxy_send,r.proxy_trusted_cidrs,COALESCE(r.egress_node_id,0),COALESCE(en.tunnel_public_host,''),COALESCE(en.tunnel_listen_port,0),COALESCE(l.token,''),COALESCE(en.tunnel_protocol,''),u.quota_epoch,r.protocol,COALESCE(r.tls_inbound_id,0),r.sni_names,r.tls_alpn,r.origin_tls,COALESCE(r.egress_group_id,0) FROM rules r JOIN rule_ingress_nodes ri ON ri.rule_id=r.id JOIN nodes nn ON nn.id=ri.node_id JOIN user_entitlements u ON u.id=r.user_id JOIN user_nodes n ON n.user_id=r.user_id AND n.node_id=ri.node_id LEFT JOIN node_groups ig ON ig.id=r.ingress_group_id LEFT JOIN user_node_groups iug ON iug.group_id=r.ingress_group_id AND iug.user_id=r.user_id LEFT JOIN nodes en ON en.id=r.egress_node_id AND en.enabled AND en.tunnel_exit_enabled LEFT JOIN node_tunnel_links l ON l.ingress_node_id=ri.node_id AND l.egress_node_id=r.egress_node_id WHERE ri.node_id=$1 AND nn.ingress_enabled AND (r.ingress_group_id IS NULL OR ig.enabled AND iug.user_id IS NOT NULL) ORDER BY r.id`
