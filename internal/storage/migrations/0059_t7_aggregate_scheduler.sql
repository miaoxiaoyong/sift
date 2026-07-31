CREATE TABLE t7_replay_evidence (
    id                TEXT NOT NULL PRIMARY KEY,
    scope             TEXT NOT NULL CHECK (scope IN ('global', 'project')),
    project_id        TEXT REFERENCES projects (id),
    task_kind         TEXT NOT NULL CHECK (task_kind IN ('feature', 'bug', 'chore', 'docs', 'refactor', 'all')),
    window_start_ms   INTEGER NOT NULL CHECK (window_start_ms >= 0),
    window_end_ms     INTEGER NOT NULL CHECK (window_end_ms > window_start_ms),
    dataset_version   TEXT NOT NULL CHECK (length(dataset_version) BETWEEN 1 AND 128),
    gate_version      TEXT NOT NULL CHECK (length(gate_version) BETWEEN 1 AND 128),
    total_samples     INTEGER NOT NULL CHECK (total_samples >= 0),
    negative_samples  INTEGER NOT NULL CHECK (negative_samples BETWEEN 0 AND total_samples),
    leak_count        INTEGER NOT NULL CHECK (leak_count BETWEEN 0 AND negative_samples),
    false_block_count INTEGER NOT NULL CHECK (false_block_count BETWEEN 0 AND total_samples - negative_samples),
    evidence_id       TEXT NOT NULL UNIQUE,
    created_at_ms     INTEGER NOT NULL CHECK (created_at_ms >= window_end_ms),
    CHECK ((scope = 'global' AND project_id IS NULL)
        OR (scope = 'project' AND project_id IS NOT NULL))
);

CREATE UNIQUE INDEX t7_replay_evidence_window
ON t7_replay_evidence (scope, COALESCE(project_id, ''), task_kind, window_start_ms, window_end_ms);

CREATE TABLE t7_aggregate_completions (
    aggregate_key   TEXT NOT NULL PRIMARY KEY,
    replay_evidence_id TEXT NOT NULL UNIQUE REFERENCES t7_replay_evidence (id),
    logical_call_id TEXT NOT NULL UNIQUE REFERENCES brain_calls (id),
    outcome         TEXT NOT NULL CHECK (outcome IN ('valid', 'fallback')),
    completed_at_ms INTEGER NOT NULL CHECK (completed_at_ms >= 0)
);

-- A frozen aggregate window is one scheduler-owned logical T7 call. Keeping
-- this binding separate avoids changing the generic Brain trace contract,
-- which permits explicit replay calls with the same subject.
CREATE TABLE t7_aggregate_call_bindings (
    aggregate_key   TEXT NOT NULL PRIMARY KEY,
    logical_call_id TEXT NOT NULL UNIQUE REFERENCES brain_calls (id)
);

CREATE TRIGGER t7_replay_evidence_append_only_update
BEFORE UPDATE ON t7_replay_evidence FOR EACH ROW
BEGIN SELECT RAISE(ABORT, 'append-only table'); END;
CREATE TRIGGER t7_replay_evidence_append_only_delete
BEFORE DELETE ON t7_replay_evidence FOR EACH ROW
BEGIN SELECT RAISE(ABORT, 'append-only table'); END;
CREATE TRIGGER t7_aggregate_completions_append_only_update
BEFORE UPDATE ON t7_aggregate_completions FOR EACH ROW
BEGIN SELECT RAISE(ABORT, 'append-only table'); END;
CREATE TRIGGER t7_aggregate_completions_append_only_delete
BEFORE DELETE ON t7_aggregate_completions FOR EACH ROW
BEGIN SELECT RAISE(ABORT, 'append-only table'); END;
CREATE TRIGGER t7_aggregate_call_bindings_append_only_update
BEFORE UPDATE ON t7_aggregate_call_bindings FOR EACH ROW
BEGIN SELECT RAISE(ABORT, 'append-only table'); END;
CREATE TRIGGER t7_aggregate_call_bindings_append_only_delete
BEFORE DELETE ON t7_aggregate_call_bindings FOR EACH ROW
BEGIN SELECT RAISE(ABORT, 'append-only table'); END;
