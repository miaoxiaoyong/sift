-- Persist closed GateReEvaluationInterruptV1 seam fields for HITL successors
-- (storage.md section 8.1) so generation-key replay verifies source_interrupt_id,
-- created_from_event_id, and facts byte-identical.
CREATE TABLE gate_re_eval_interrupt_seams (
  interrupt_id TEXT PRIMARY KEY REFERENCES interrupts(id),
  source_interrupt_id TEXT NOT NULL,
  created_from_event_id TEXT NOT NULL REFERENCES events(id),
  facts_canonical_json TEXT NOT NULL,
  facts_digest TEXT NOT NULL,
  created_at_ms INTEGER NOT NULL
);

CREATE TRIGGER gate_re_eval_interrupt_seams_no_update
BEFORE UPDATE ON gate_re_eval_interrupt_seams
BEGIN SELECT RAISE(ABORT,'gate re-eval interrupt seam is immutable'); END;

CREATE TRIGGER gate_re_eval_interrupt_seams_no_delete
BEFORE DELETE ON gate_re_eval_interrupt_seams
BEGIN SELECT RAISE(ABORT,'gate re-eval interrupt seam is immutable'); END;
