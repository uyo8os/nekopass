ALTER TABLE nodes DROP CONSTRAINT nodes_port_min_check;
ALTER TABLE nodes ADD CONSTRAINT nodes_port_min_check CHECK(port_min BETWEEN 80 AND 65535);
ALTER TABLE rules DROP CONSTRAINT rules_listen_port_check;
ALTER TABLE rules ADD CONSTRAINT rules_listen_port_check CHECK(
 (tls_inbound_id IS NULL AND listen_port BETWEEN 80 AND 65535)
 OR (tls_inbound_id IS NOT NULL AND listen_port=0 AND protocol='tcp' AND jsonb_array_length(sni_names) BETWEEN 1 AND 32));
INSERT INTO schema_version VALUES(42);
