-- M4 Ledger completion: immutable gate-sample FK, causal external bindings and
-- task-kind certification revisions. Existing databases contain only un-settled
-- M3 calibrations, so the added gate-sample column is populated by the M4 writer.
ALTER TABLE calibration_entries ADD COLUMN gate_sample_entry_id TEXT;
CREATE UNIQUE INDEX calibration_entries_gate_sample_entry_id
    ON calibration_entries (gate_sample_entry_id) WHERE gate_sample_entry_id IS NOT NULL;

ALTER TABLE ledger_entries ADD COLUMN features_digest TEXT;

CREATE TABLE external_decision_bindings (
    forge_fact_event_id TEXT NOT NULL PRIMARY KEY REFERENCES events (id),
    calibration_id TEXT NOT NULL UNIQUE REFERENCES calibration_entries (id),
    created_at_ms INTEGER NOT NULL
);

CREATE TABLE human_decision_receipts (
    idempotency_id TEXT NOT NULL PRIMARY KEY,
    ledger_entry_id TEXT NOT NULL REFERENCES ledger_entries (id),
    calibration_id TEXT REFERENCES calibration_entries (id)
);

CREATE TABLE certification_current (
    task_kind TEXT NOT NULL PRIMARY KEY CHECK (task_kind IN ('feature','bug','chore','docs','refactor')),
    certification_version TEXT NOT NULL,
    version INTEGER NOT NULL CHECK (version >= 1),
    updated_at_ms INTEGER NOT NULL,
    FOREIGN KEY (task_kind, certification_version) REFERENCES certifications (task_kind, certification_version)
);

ALTER TABLE certifications ADD COLUMN certification_rules_version TEXT NOT NULL DEFAULT '';
ALTER TABLE certifications ADD COLUMN window_start_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE certifications ADD COLUMN window_end_ms INTEGER NOT NULL DEFAULT 0;

DROP TRIGGER calibration_entries_decision_completion_only;
CREATE TRIGGER calibration_entries_decision_completion_only
BEFORE UPDATE ON calibration_entries FOR EACH ROW
WHEN NOT (OLD.human_decision IS NULL AND NEW.human_decision IN ('allow','block')
    AND OLD.decision_source IS NULL AND NEW.decision_source IN ('command','manual_merge','manual_close')
    AND OLD.decided_at_ms IS NULL AND NEW.decided_at_ms IS NOT NULL
    AND NEW.id IS OLD.id AND NEW.run_id IS OLD.run_id
    AND NEW.gate_evaluation_id IS OLD.gate_evaluation_id
    AND NEW.predicted_decision IS OLD.predicted_decision
    AND NEW.features_json IS OLD.features_json
    AND NEW.gate_sample_entry_id IS OLD.gate_sample_entry_id
    AND NEW.predicted_at_ms IS OLD.predicted_at_ms)
BEGIN SELECT RAISE(ABORT, 'calibration_entries allows only a one-time human decision completion'); END;

CREATE INDEX calibration_entries_settled ON calibration_entries (decided_at_ms, human_decision);
