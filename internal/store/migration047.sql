-- A committed configuration transaction appends an event. Do not use a
-- sequence high-water mark: concurrent transactions may commit out of order.
-- Consumers compare epoch, count and event fingerprints in a *single*
-- statement/snapshot. Random event IDs distinguish branches after a restore.
CREATE TABLE static_config_epoch (
    id integer PRIMARY KEY CHECK (id=1),
    epoch text NOT NULL
);
INSERT INTO static_config_epoch VALUES
    (1, gen_random_uuid()::text);
CREATE TABLE static_config_events (id uuid NOT NULL DEFAULT gen_random_uuid());

CREATE FUNCTION static_config_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO static_config_events DEFAULT VALUES;
    RETURN NULL;
END
$$;

CREATE FUNCTION static_config_inserted() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM new_rows) THEN
        INSERT INTO static_config_events DEFAULT VALUES;
    END IF;
    RETURN NULL;
END
$$;
CREATE FUNCTION static_config_updated() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM new_rows) THEN
        INSERT INTO static_config_events DEFAULT VALUES;
    END IF;
    RETURN NULL;
END
$$;
CREATE FUNCTION static_config_deleted() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM old_rows) THEN
        INSERT INTO static_config_events DEFAULT VALUES;
    END IF;
    RETURN NULL;
END
$$;

-- Node reports write these telemetry columns every second. In particular,
-- protocol_version is included below because it affects other nodes' routes;
-- merely repeating the same version must not invalidate every cached config.
-- Keep this list in sync when adding fields used in node configurations.
CREATE FUNCTION static_config_node_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.enabled, OLD.listen_host, OLD.port_min, OLD.port_max,
           OLD.max_connections, OLD.dial_timeout_seconds, OLD.idle_timeout_seconds,
           OLD.probe_interval_seconds, OLD.disk_path, OLD.network_interfaces,
           OLD.allow_direct, OLD.ingress_enabled, OLD.tunnel_exit_enabled,
           OLD.tunnel_protocol, OLD.tunnel_listen_host, OLD.tunnel_listen_port,
           OLD.tunnel_public_host, OLD.public_address, OLD.protocol_version,
           OLD.tls_config, OLD.tls_certificate, OLD.tls_private_key, OLD.tls_ca,
           OLD.tls_inbound_only, OLD.udp_idle_timeout_seconds, OLD.ingress_rate,
           OLD.egress_rate, OLD.random_config, OLD.bandwidth_up_mbps,
           OLD.bandwidth_down_mbps, OLD.brutal_enabled, OLD.client_tcp_brutal_enabled,
           OLD.mux_config)
       IS DISTINCT FROM
       ROW(NEW.enabled, NEW.listen_host, NEW.port_min, NEW.port_max,
           NEW.max_connections, NEW.dial_timeout_seconds, NEW.idle_timeout_seconds,
           NEW.probe_interval_seconds, NEW.disk_path, NEW.network_interfaces,
           NEW.allow_direct, NEW.ingress_enabled, NEW.tunnel_exit_enabled,
           NEW.tunnel_protocol, NEW.tunnel_listen_host, NEW.tunnel_listen_port,
           NEW.tunnel_public_host, NEW.public_address, NEW.protocol_version,
           NEW.tls_config, NEW.tls_certificate, NEW.tls_private_key, NEW.tls_ca,
           NEW.tls_inbound_only, NEW.udp_idle_timeout_seconds, NEW.ingress_rate,
           NEW.egress_rate, NEW.random_config, NEW.bandwidth_up_mbps,
           NEW.bandwidth_down_mbps, NEW.brutal_enabled, NEW.client_tcp_brutal_enabled,
           NEW.mux_config) THEN
        INSERT INTO static_config_events DEFAULT VALUES;
    END IF;
    RETURN NULL;
END
$$;

CREATE TRIGGER static_config_node_update
    AFTER UPDATE ON nodes FOR EACH ROW EXECUTE FUNCTION static_config_node_changed();
-- Statement-level triggers avoid one event per affected row. These tables
-- contain no high-frequency heartbeat accounting or node telemetry.
DO $$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
      'nodes',
      'rules', 'node_groups', 'node_group_members', 'user_node_groups',
      'users', 'plans', 'node_tunnel_links', 'site_settings',
      'node_tls_inbounds', 'node_acme_challenges'
    ] LOOP
        EXECUTE format('CREATE TRIGGER static_config_inserted AFTER INSERT ON %I REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION static_config_inserted()', table_name);
        EXECUTE format('CREATE TRIGGER static_config_deleted AFTER DELETE ON %I REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION static_config_deleted()', table_name);
        EXECUTE format('CREATE TRIGGER static_config_truncated AFTER TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION static_config_changed()', table_name);
        IF table_name <> 'nodes' THEN
            EXECUTE format('CREATE TRIGGER static_config_updated AFTER UPDATE ON %I REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION static_config_updated()', table_name);
        END IF;
    END LOOP;
END
$$;

-- Use DELETE, not TRUNCATE: TRUNCATE is not MVCC-safe for older snapshots.
-- Inserts invisible to DELETE's snapshot survive and become visible normally,
-- even when their transactions commit after the epoch flip. No exclusive
-- table lock is taken: a compactor must never block heartbeat readers while
-- waiting for an inserter that may itself be waiting on a node row lock.
-- Invoke explicitly as occasional maintenance, never on every heartbeat.
CREATE FUNCTION compact_static_config_events() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    IF NOT pg_try_advisory_xact_lock(734291, 47) THEN
        RETURN;
    END IF;
    DELETE FROM static_config_events;
    UPDATE static_config_epoch
       SET epoch = gen_random_uuid()::text
     WHERE id=1;
END
$$;

INSERT INTO schema_version(version) VALUES (47);
