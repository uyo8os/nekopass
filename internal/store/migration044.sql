-- Existing groups retain both ingress and egress behaviour until explicitly converted.
ALTER TABLE node_groups ADD COLUMN group_type text NOT NULL DEFAULT 'legacy' CHECK(group_type IN ('legacy','ingress','egress'));
ALTER TABLE node_groups ADD COLUMN connection_address text NOT NULL DEFAULT '';
INSERT INTO schema_version VALUES(44);
