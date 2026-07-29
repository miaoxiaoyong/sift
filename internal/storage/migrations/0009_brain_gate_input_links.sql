-- M4 replay links: a terminal T3/T5 call can contribute to multiple immutable
-- Gate snapshots. Do not use the legacy nullable brain_calls snapshot column.
CREATE TABLE brain_gate_input_links (
    logical_call_id       TEXT NOT NULL REFERENCES brain_calls (id),
    gate_input_snapshot_id TEXT NOT NULL REFERENCES gate_input_snapshots (id),
    touchpoint            TEXT NOT NULL CHECK (touchpoint IN ('T3', 'T5')),
    created_at_ms         INTEGER NOT NULL,
    PRIMARY KEY (logical_call_id, gate_input_snapshot_id)
);

CREATE INDEX brain_gate_input_links_snapshot ON brain_gate_input_links (gate_input_snapshot_id, logical_call_id);
