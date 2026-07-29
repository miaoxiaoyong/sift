-- Gate HITL rows carry the immutable calibration binding created in the same
-- transaction as their snapshot/evaluation/ledger sample.
ALTER TABLE interrupts ADD COLUMN calibration_id TEXT REFERENCES calibration_entries (id);
CREATE UNIQUE INDEX interrupts_calibration_id ON interrupts (calibration_id) WHERE calibration_id IS NOT NULL;
