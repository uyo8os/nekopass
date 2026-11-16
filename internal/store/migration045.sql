-- NULL preserves the existing ID order for older and newly created nodes.
-- Once reordered, new nodes still follow the sorted nodes.
ALTER TABLE nodes ADD COLUMN sort_order integer;
INSERT INTO schema_version VALUES(45);
