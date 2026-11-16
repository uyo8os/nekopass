-- Identifies the current holder of each node's advisory lock. Rows persist
-- across disconnects; the lock and a matching generation are both required.
CREATE TABLE node_stream_sessions (
    node_id bigint PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    generation text NOT NULL,
    lock_pid integer NOT NULL
);
INSERT INTO schema_version(version) VALUES (46);
