-- A group rule remains one logical rule. Its node_id is a compatibility
-- anchor; actual ingress nodes are derived from current group membership.
ALTER TABLE rules ADD COLUMN ingress_group_id bigint REFERENCES node_groups(id);
ALTER TABLE rules ADD CONSTRAINT rules_group_tls_inbound_check
 CHECK (ingress_group_id IS NULL OR tls_inbound_id IS NULL);

CREATE VIEW rule_ingress_nodes AS
 SELECT id AS rule_id,node_id FROM rules WHERE ingress_group_id IS NULL
 UNION ALL
 SELECT r.id,m.node_id FROM rules r JOIN node_group_members m ON m.group_id=r.ingress_group_id
 JOIN nodes n ON n.id=m.node_id AND n.ingress_enabled;

-- Append ingress_node_id so existing consumers can migrate independently.
CREATE OR REPLACE VIEW rule_outbound_routes AS
 SELECT r.id AS rule_id,n.id AS egress_node_id,1 AS weight,'active'::text AS role,i.node_id AS ingress_node_id
 FROM rules r JOIN rule_ingress_nodes i ON i.rule_id=r.id
 JOIN nodes n ON n.id=r.egress_node_id AND n.enabled AND n.tunnel_exit_enabled
 JOIN node_tunnel_links l ON l.ingress_node_id=i.node_id AND l.egress_node_id=n.id
 JOIN user_nodes a ON a.user_id=r.user_id AND a.node_id=n.id
 WHERE n.id<>i.node_id AND (r.protocol='udp' OR n.tunnel_protocol NOT IN ('plain_udp','dtls_udp','random_udp'))
 UNION ALL
 SELECT r.id,n.id,m.weight,m.role,i.node_id
 FROM rules r JOIN rule_ingress_nodes i ON i.rule_id=r.id
 JOIN user_node_groups ug ON ug.user_id=r.user_id AND ug.group_id=r.egress_group_id
 JOIN node_groups g ON g.id=ug.group_id AND g.enabled
 JOIN node_group_members m ON m.group_id=g.id AND m.role<>'disabled'
 JOIN nodes n ON n.id=m.node_id AND n.enabled AND n.tunnel_exit_enabled AND n.id<>i.node_id
 JOIN node_tunnel_links l ON l.ingress_node_id=i.node_id AND l.egress_node_id=n.id
 JOIN user_nodes a ON a.user_id=r.user_id AND a.node_id=n.id
 WHERE r.protocol='udp' OR n.tunnel_protocol NOT IN ('plain_udp','dtls_udp','random_udp');

CREATE FUNCTION refresh_ingress_rule_ports(rule_key bigint) RETURNS void LANGUAGE plpgsql AS $$
DECLARE r rules%ROWTYPE;
BEGIN
 PERFORM pg_advisory_xact_lock(734292);
 SELECT * INTO r FROM rules WHERE id=rule_key;
 IF NOT FOUND THEN RETURN; END IF;
 IF r.ingress_group_id IS NOT NULL AND EXISTS (
  SELECT 1 FROM rule_ingress_nodes i JOIN nodes n ON n.id=i.node_id
  WHERE i.rule_id=r.id AND (r.listen_port NOT BETWEEN n.port_min AND n.port_max
   OR (n.tunnel_exit_enabled AND n.tunnel_listen_port=r.listen_port AND
    (n.tunnel_protocol='tls_h3' OR
     (r.protocol='udp')=(n.tunnel_protocol IN ('plain_udp','dtls_udp','random_udp')))))
 ) THEN
  RAISE EXCEPTION '入口组成员的端口范围或隧道端口与规则冲突' USING ERRCODE='23514';
 END IF;
 DELETE FROM rule_ports WHERE rule_id=r.id;
 IF r.tls_inbound_id IS NULL THEN
  INSERT INTO rule_ports(node_id,protocol,listen_port,rule_id)
   SELECT node_id,r.protocol,r.listen_port,r.id FROM rule_ingress_nodes WHERE rule_id=r.id ORDER BY node_id;
 END IF;
 INSERT INTO rule_usage(node_id,rule_id,user_id)
  SELECT node_id,r.id,r.user_id FROM rule_ingress_nodes WHERE rule_id=r.id
  ON CONFLICT DO NOTHING;
END $$;

CREATE OR REPLACE FUNCTION reserve_rule_ports() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 PERFORM refresh_ingress_rule_ports(NEW.id);
 RETURN NEW;
END $$;
DROP TRIGGER reserve_rule_ports ON rules;
CREATE TRIGGER reserve_rule_ports AFTER INSERT OR UPDATE OF node_id,listen_port,protocol,tls_inbound_id,ingress_group_id ON rules
 FOR EACH ROW EXECUTE FUNCTION reserve_rule_ports();

-- Group changes are transactional: a conflicting member is not admitted.
CREATE FUNCTION refresh_group_ingress_ports() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE group_key bigint; rule_key bigint;
BEGIN
 PERFORM pg_advisory_xact_lock(734292);
 group_key := CASE WHEN TG_OP='DELETE' THEN OLD.group_id ELSE NEW.group_id END;
 FOR rule_key IN SELECT id FROM rules WHERE ingress_group_id=group_key ORDER BY id LOOP
  PERFORM refresh_ingress_rule_ports(rule_key);
 END LOOP;
 IF EXISTS(SELECT 1 FROM rules WHERE ingress_group_id=group_key) THEN
  UPDATE revision SET value=value+1 WHERE id=1;
  UPDATE rules SET config_revision=(SELECT value FROM revision WHERE id=1) WHERE ingress_group_id=group_key;
 END IF;
 IF TG_OP='UPDATE' AND OLD.group_id<>NEW.group_id THEN
  FOR rule_key IN SELECT id FROM rules WHERE ingress_group_id=OLD.group_id ORDER BY id LOOP
   PERFORM refresh_ingress_rule_ports(rule_key);
  END LOOP;
  IF EXISTS(SELECT 1 FROM rules WHERE ingress_group_id=OLD.group_id) THEN
   UPDATE revision SET value=value+1 WHERE id=1;
   UPDATE rules SET config_revision=(SELECT value FROM revision WHERE id=1) WHERE ingress_group_id=OLD.group_id;
  END IF;
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
END $$;
CREATE TRIGGER refresh_group_ingress_ports AFTER INSERT OR DELETE OR UPDATE OF node_id,group_id ON node_group_members
 FOR EACH ROW EXECUTE FUNCTION refresh_group_ingress_ports();

CREATE FUNCTION refresh_node_ingress_ports() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE rule_key bigint;
BEGIN
 IF NEW.ingress_enabled IS DISTINCT FROM OLD.ingress_enabled OR NEW.enabled IS DISTINCT FROM OLD.enabled THEN
  PERFORM pg_advisory_xact_lock(734292);
  IF NEW.ingress_enabled IS DISTINCT FROM OLD.ingress_enabled THEN
   FOR rule_key IN SELECT r.id FROM rules r JOIN node_group_members m ON m.group_id=r.ingress_group_id WHERE m.node_id=NEW.id ORDER BY r.id LOOP
    PERFORM refresh_ingress_rule_ports(rule_key);
   END LOOP;
  END IF;
  IF EXISTS(SELECT 1 FROM rules r JOIN node_group_members m ON m.group_id=r.ingress_group_id WHERE m.node_id=NEW.id) THEN
   UPDATE revision SET value=value+1 WHERE id=1;
   UPDATE rules SET config_revision=(SELECT value FROM revision WHERE id=1)
    WHERE ingress_group_id IN (SELECT group_id FROM node_group_members WHERE node_id=NEW.id);
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER refresh_node_ingress_ports AFTER UPDATE OF ingress_enabled,enabled ON nodes
 FOR EACH ROW EXECUTE FUNCTION refresh_node_ingress_ports();

CREATE FUNCTION refresh_ingress_group_enabled() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.enabled IS DISTINCT FROM OLD.enabled THEN
  PERFORM pg_advisory_xact_lock(734292);
  IF EXISTS(SELECT 1 FROM rules WHERE ingress_group_id=NEW.id) THEN
   UPDATE revision SET value=value+1 WHERE id=1;
   UPDATE rules SET config_revision=(SELECT value FROM revision WHERE id=1) WHERE ingress_group_id=NEW.id;
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER refresh_ingress_group_enabled AFTER UPDATE OF enabled ON node_groups
 FOR EACH ROW EXECUTE FUNCTION refresh_ingress_group_enabled();

UPDATE revision SET value=value+1;
INSERT INTO schema_version VALUES(43);
