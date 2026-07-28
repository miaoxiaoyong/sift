package storage

import (
	"strings"
	"testing"
)

// CHECK constraint, foreign-key and uniqueness behavior required by
// specs/storage.md. Each subtest inserts its own fixture chain into a fresh
// database, so cases stay independent.

// seedRunChain creates cfg/project/run/task-spec and returns nothing; ids are
// fixed: cfg1, p1, r1, ts1.
func seedRunChain(t *testing.T, db *DB) {
	t.Helper()
	insertConfigSnapshot(t, db, "cfg1")
	insertProject(t, db, "p1", "cfg1")
	insertManualRun(t, db, "r1", "p1", "cfg1")
	insertTaskSpec(t, db, "ts1", "r1", 1)
}

func TestRunChecks(t *testing.T) {
	db, _ := openTestDB(t)
	insertConfigSnapshot(t, db, "cfg1")
	insertProject(t, db, "p1", "cfg1")

	// forge source must carry issue fields.
	mustFail(t, db, `INSERT INTO runs
		(id, source_kind, project_id, config_snapshot_id, forge_kind, forge_host, forge_project_key,
		 status, max_attempts, created_at_ms, updated_at_ms)
		VALUES ('rf', 'forge', 'p1', 'cfg1', 'github', 'github.com', 'org/repo-p1', 'queued', 3, 0, 0)`)
	// manual source must not carry issue fields.
	mustFail(t, db, `INSERT INTO runs
		(id, source_kind, project_id, config_snapshot_id, forge_kind, forge_host, forge_project_key,
		 issue_id, status, max_attempts, created_at_ms, updated_at_ms)
		VALUES ('rm', 'manual', 'p1', 'cfg1', 'github', 'github.com', 'org/repo-p1', '7', 'queued', 3, 0, 0)`)
	// unknown status / failure_reason values are rejected.
	mustFail(t, db, `INSERT INTO runs
		(id, source_kind, project_id, config_snapshot_id, forge_kind, forge_host, forge_project_key,
		 status, max_attempts, created_at_ms, updated_at_ms)
		VALUES ('rs', 'manual', 'p1', 'cfg1', 'github', 'github.com', 'org/repo-p1', 'paused', 3, 0, 0)`)

	insertManualRun(t, db, "r1", "p1", "cfg1")
	// done requires a change id.
	mustFail(t, db, `UPDATE runs SET status = 'done', completed_at_ms = 1 WHERE id = 'r1'`)
	// terminal status requires completed_at_ms; non-terminal forbids it.
	mustFail(t, db, `UPDATE runs SET status = 'failed', failure_reason = 'no_change' WHERE id = 'r1'`)
	mustFail(t, db, `UPDATE runs SET completed_at_ms = 1 WHERE id = 'r1'`)
	// failure_reason enum.
	mustFail(t, db, `UPDATE runs SET status = 'failed', failure_reason = 'mystery', completed_at_ms = 1 WHERE id = 'r1'`)
	// A legal terminal transition works.
	mustExec(t, db, `UPDATE runs SET status = 'failed', failure_reason = 'operator_kill',
		completed_at_ms = 1, version = 2 WHERE id = 'r1'`)
}

func TestProjectHealthChecks(t *testing.T) {
	db, _ := openTestDB(t)
	insertConfigSnapshot(t, db, "cfg1")
	insertProject(t, db, "p1", "cfg1")

	// isolated requires a known reason; free text is rejected.
	mustFail(t, db, `UPDATE projects SET health = 'isolated' WHERE id = 'p1'`)
	mustFail(t, db, `UPDATE projects SET health = 'isolated', isolation_reason = 'vibes' WHERE id = 'p1'`)
	mustExec(t, db, `UPDATE projects SET health = 'isolated', isolation_reason = 'repo_invalid' WHERE id = 'p1'`)
	// active forbids a reason.
	mustFail(t, db, `UPDATE projects SET health = 'active', isolation_reason = 'repo_invalid' WHERE id = 'p1'`)
}

func TestAttemptChecks(t *testing.T) {
	db, _ := openTestDB(t)
	seedRunChain(t, db)

	// Wrapper identity is all-or-none.
	mustFail(t, db, `INSERT INTO attempts
		(run_id, attempt_no, phase, generation, backend, agent_id, task_spec_snapshot_id,
		 worktree_path, branch_name, base_ref, base_sha, wrapper_pid, created_at_ms, updated_at_ms)
		VALUES ('r1', 1, 'pending', 1, 'process', 'claude', 'ts1', '/wt', 'b', 'main', 'abc', 111, 0, 0)`)
	// Agent identity triple is all-or-none.
	mustFail(t, db, `INSERT INTO attempts
		(run_id, attempt_no, phase, generation, backend, agent_id, task_spec_snapshot_id,
		 worktree_path, branch_name, base_ref, base_sha, agent_pid, agent_started_at_ms, created_at_ms, updated_at_ms)
		VALUES ('r1', 1, 'pending', 1, 'process', 'claude', 'ts1', '/wt', 'b', 'main', 'abc', 222, 0, 0, 0)`)
	// running requires agent identity.
	mustFail(t, db, `INSERT INTO attempts
		(run_id, attempt_no, phase, generation, backend, agent_id, task_spec_snapshot_id,
		 worktree_path, branch_name, base_ref, base_sha, created_at_ms, updated_at_ms)
		VALUES ('r1', 1, 'running', 1, 'process', 'claude', 'ts1', '/wt', 'b', 'main', 'abc', 0, 0)`)
	// finished requires a result and exactly one of exit_code/signal.
	mustFail(t, db, `INSERT INTO attempts
		(run_id, attempt_no, phase, generation, backend, agent_id, task_spec_snapshot_id,
		 worktree_path, branch_name, base_ref, base_sha, result_exit_code, created_at_ms, updated_at_ms)
		VALUES ('r1', 1, 'finished', 1, 'process', 'claude', 'ts1', '/wt', 'b', 'main', 'abc', 0, 0, 0)`)
	mustFail(t, db, `INSERT INTO attempts
		(run_id, attempt_no, phase, generation, backend, agent_id, task_spec_snapshot_id,
		 worktree_path, branch_name, base_ref, base_sha, result_exit_code, result_signal,
		 result_digest, result_observed_at_ms, created_at_ms, updated_at_ms)
		VALUES ('r1', 1, 'finished', 1, 'process', 'claude', 'ts1', '/wt', 'b', 'main', 'abc', 0, 'SIGKILL', 'rd', 0, 0, 0)`)
	// attempt_resolution accepts only the two V0 values and moves with its timestamp.
	mustFail(t, db, `INSERT INTO attempts
		(run_id, attempt_no, phase, generation, backend, agent_id, task_spec_snapshot_id,
		 worktree_path, branch_name, base_ref, base_sha, attempt_resolution, resolution_at_ms,
		 created_at_ms, updated_at_ms)
		VALUES ('r1', 1, 'orphaned', 1, 'process', 'claude', 'ts1', '/wt', 'b', 'main', 'abc', 'abandon', 0, 0, 0)`)
	mustFail(t, db, `INSERT INTO attempts
		(run_id, attempt_no, phase, generation, backend, agent_id, task_spec_snapshot_id,
		 worktree_path, branch_name, base_ref, base_sha, attempt_resolution, created_at_ms, updated_at_ms)
		VALUES ('r1', 1, 'orphaned', 1, 'process', 'claude', 'ts1', '/wt', 'b', 'main', 'abc', 'reject', 0, 0)`)
	// frozen isolation requires reason + isolated_at, and no release timestamp.
	mustFail(t, db, `INSERT INTO attempts
		(run_id, attempt_no, phase, generation, backend, agent_id, task_spec_snapshot_id,
		 worktree_path, branch_name, base_ref, base_sha, isolation_state, isolation_reason,
		 created_at_ms, updated_at_ms)
		VALUES ('r1', 1, 'pending', 1, 'process', 'claude', 'ts1', '/wt', 'b', 'main', 'abc', 'frozen', 'startup_stall', 0, 0)`)

	// A full valid attempt finishes cleanly.
	insertAttempt(t, db, "r1", 1, "ts1")
	mustExec(t, db, `UPDATE attempts SET
		phase = 'finished',
		wrapper_pid = 10, wrapper_started_at_ms = 0, wrapper_executable = '/bin/w',
		wrapper_pgid = 10, wrapper_instance_id = 'wi1',
		agent_pid = 11, agent_started_at_ms = 0, agent_executable = '/bin/a',
		result_exit_code = 0, result_digest = 'rd', result_observed_at_ms = 1, finished_at_ms = 1
		WHERE run_id = 'r1' AND attempt_no = 1`)
}

func TestInterruptChecks(t *testing.T) {
	db, _ := openTestDB(t)
	seedRunChain(t, db)
	insertBudgetEntry(t, db, "be1", "op-be1")

	// startup_stall may never auto-reject (PRD §4.3).
	mustFail(t, db, `INSERT INTO interrupts
		(id, run_id, generation_key, reason, severity, headline, brief_markdown, options_json,
		 min_modality, nonce, status, dispatch_state, expires_at_ms, on_expire, max_escalations,
		 charged_budget_entry_id, created_at_ms, updated_at_ms)
		VALUES ('i1', 'r1', 'gk1', 'startup_stall', 'high', 'h', 'b', '[]', 'text', 'n', 'open',
		 'ready', 1, 'auto_reject', 0, 'be1', 0, 0)`)
	// closed requires close fields; open forbids them.
	mustFail(t, db, `INSERT INTO interrupts
		(id, run_id, generation_key, reason, severity, headline, brief_markdown, options_json,
		 min_modality, nonce, status, dispatch_state, expires_at_ms, on_expire, max_escalations,
		 charged_budget_entry_id, created_at_ms, updated_at_ms)
		VALUES ('i2', 'r1', 'gk2', 'code_review', 'normal', 'h', 'b', '[]', 'visual', 'n', 'closed',
		 'ready', 1, 'hold', 0, 'be1', 0, 0)`)
	mustFail(t, db, `INSERT INTO interrupts
		(id, run_id, generation_key, reason, severity, headline, brief_markdown, options_json,
		 min_modality, nonce, status, dispatch_state, expires_at_ms, on_expire, max_escalations,
		 close_reason, charged_budget_entry_id, created_at_ms, updated_at_ms)
		VALUES ('i3', 'r1', 'gk3', 'code_review', 'normal', 'h', 'b', '[]', 'visual', 'n', 'open',
		 'ready', 1, 'hold', 0, 'responded', 'be1', 0, 0)`)
	// unknown reason and close_reason values are rejected.
	mustFail(t, db, `INSERT INTO interrupts
		(id, run_id, generation_key, reason, severity, headline, brief_markdown, options_json,
		 min_modality, nonce, status, dispatch_state, expires_at_ms, on_expire, max_escalations,
		 charged_budget_entry_id, created_at_ms, updated_at_ms)
		VALUES ('i4', 'r1', 'gk4', 'confusion', 'normal', 'h', 'b', '[]', 'text', 'n', 'open',
		 'ready', 1, 'hold', 0, 'be1', 0, 0)`)

	// The happy path and a legal close.
	insertInterrupt(t, db, "i5", "r1", "be1")
	mustExec(t, db, `UPDATE interrupts SET status = 'closed', close_reason = 'superseded_by_fact',
		closed_at_ms = 1, version = 2 WHERE id = 'i5'`)
}

func TestIntakeChecks(t *testing.T) {
	db, _ := openTestDB(t)
	seedRunChain(t, db)

	// consumed requires a linked run; other states forbid one.
	mustFail(t, db, `INSERT INTO intake_items
		(id, project_id, forge_kind, normalized_host, forge_project_key, issue_id, issue_url,
		 issue_digest, state, version, created_at_ms, updated_at_ms)
		VALUES ('ii1', 'p1', 'github', 'github.com', 'org/repo-p1', '42', 'u', 'd', 'consumed', 1, 0, 0)`)
	mustFail(t, db, `INSERT INTO intake_items
		(id, project_id, forge_kind, normalized_host, forge_project_key, issue_id, issue_url,
		 issue_digest, state, version, linked_run_id, created_at_ms, updated_at_ms)
		VALUES ('ii2', 'p1', 'github', 'github.com', 'org/repo-p1', '43', 'u', 'd', 'ready', 1, 'r1', 0, 0)`)
	// awaiting states require an assessment and generation >= 1.
	mustFail(t, db, `INSERT INTO intake_items
		(id, project_id, forge_kind, normalized_host, forge_project_key, issue_id, issue_url,
		 issue_digest, state, version, clarification_generation, created_at_ms, updated_at_ms)
		VALUES ('ii3', 'p1', 'github', 'github.com', 'org/repo-p1', '44', 'u', 'd',
		 'awaiting_clarification', 1, 1, 0, 0)`)
	// awaiting_duplicate_confirmation additionally requires the candidate run.
	mustFail(t, db, `INSERT INTO intake_items
		(id, project_id, forge_kind, normalized_host, forge_project_key, issue_id, issue_url,
		 issue_digest, state, version, latest_assessment_id, clarification_generation,
		 created_at_ms, updated_at_ms)
		VALUES ('ii4', 'p1', 'github', 'github.com', 'org/repo-p1', '45', 'u', 'd',
		 'awaiting_duplicate_confirmation', 1, 'x', 1, 0, 0)`)
}

// TestIntakeAwaitingFlow exercises the circular intake_items <->
// intake_assessments composite FK: the deferred constraint lets a single
// transaction install the assessment and flip the item to an awaiting state.
func TestIntakeAwaitingFlow(t *testing.T) {
	db, _ := openTestDB(t)
	seedRunChain(t, db)
	insertBrainCallT1(t, db, "bc1", "p1", "s", 1)
	insertIntakeItem(t, db, "ii1", "p1", "42")
	insertIntakeAssessment(t, db, "ia1", "ii1", "bc1")

	mustExec(t, db, `UPDATE intake_items
		SET state = 'awaiting_clarification', latest_assessment_id = 'ia1',
			clarification_generation = 1, version = 2, updated_at_ms = 1
		WHERE id = 'ii1'`)
	// consumed requires the linked run.
	mustFail(t, db, `UPDATE intake_items SET state = 'consumed', version = 3 WHERE id = 'ii1'`)
	mustExec(t, db, `UPDATE intake_items SET state = 'consumed', linked_run_id = 'r1', version = 3 WHERE id = 'ii1'`)
}

func TestIntakeAssessmentMatrix(t *testing.T) {
	db, _ := openTestDB(t)
	seedRunChain(t, db)
	insertBrainCallT1(t, db, "bc1", "p1", "s", 1)
	insertIntakeItem(t, db, "ii1", "p1", "42")

	// ready: questions empty, no duplicate.
	mustFail(t, db, `INSERT INTO intake_assessments
		(id, intake_id, logical_call_id, disposition, questions_json, possible_duplicate_run_id,
		 rationale, created_at_ms)
		VALUES ('ia1', 'ii1', 'bc1', 'ready', '[{"q":"?"}]', NULL, 'r', 0)`)
	// needs_clarification: questions non-empty, no duplicate.
	mustFail(t, db, `INSERT INTO intake_assessments
		(id, intake_id, logical_call_id, disposition, questions_json, possible_duplicate_run_id,
		 rationale, created_at_ms)
		VALUES ('ia2', 'ii1', 'bc1', 'needs_clarification', '[]', NULL, 'r', 0)`)
	// possible_duplicate: duplicate set, questions empty.
	mustFail(t, db, `INSERT INTO intake_assessments
		(id, intake_id, logical_call_id, disposition, questions_json, possible_duplicate_run_id,
		 rationale, created_at_ms)
		VALUES ('ia3', 'ii1', 'bc1', 'possible_duplicate', '[]', NULL, 'r', 0)`)
	mustExec(t, db, `INSERT INTO intake_assessments
		(id, intake_id, logical_call_id, disposition, questions_json, possible_duplicate_run_id,
		 rationale, created_at_ms)
		VALUES ('ia4', 'ii1', 'bc1', 'possible_duplicate', '[]', 'r1', 'r', 0)`)
}

func TestBrainCallScopeChecks(t *testing.T) {
	db, _ := openTestDB(t)
	seedRunChain(t, db)

	// T1 is intake scope: project required, run/attempt forbidden.
	mustFail(t, db, `INSERT INTO brain_calls
		(id, scope, subject_key, project_id, run_id, touchpoint, call_seq, prompt_version,
		 output_schema_version, input_json, input_digest, status, started_at_ms)
		VALUES ('x1', 'intake', 's', NULL, NULL, 'T1', 1, 'pv', 1, '{}', 'd', 'running', 0)`)
	mustFail(t, db, `INSERT INTO brain_calls
		(id, scope, subject_key, project_id, run_id, touchpoint, call_seq, prompt_version,
		 output_schema_version, input_json, input_digest, status, started_at_ms)
		VALUES ('x2', 'intake', 's', 'p1', 'r1', 'T1', 1, 'pv', 1, '{}', 'd', 'running', 0)`)
	// T2 is run scope without attempt.
	mustFail(t, db, `INSERT INTO brain_calls
		(id, scope, subject_key, run_id, touchpoint, call_seq, prompt_version,
		 output_schema_version, input_json, input_digest, status, started_at_ms)
		VALUES ('x3', 'run', 's', NULL, 'T2', 1, 'pv', 1, '{}', 'd', 'running', 0)`)
	// T3–T6 are run scope; attempt optional.
	mustFail(t, db, `INSERT INTO brain_calls
		(id, scope, subject_key, run_id, touchpoint, call_seq, prompt_version,
		 output_schema_version, input_json, input_digest, status, started_at_ms)
		VALUES ('x4', 'aggregate', 's', NULL, 'T3', 1, 'pv', 1, '{}', 'd', 'running', 0)`)
	// T7 is aggregate scope: run/attempt forbidden.
	mustFail(t, db, `INSERT INTO brain_calls
		(id, scope, subject_key, run_id, touchpoint, call_seq, prompt_version,
		 output_schema_version, input_json, input_digest, status, started_at_ms)
		VALUES ('x5', 'aggregate', 's', 'r1', 'T7', 1, 'pv', 1, '{}', 'd', 'running', 0)`)
	// attempt_no requires run_id and a real attempt row.
	mustFail(t, db, `INSERT INTO brain_calls
		(id, scope, subject_key, run_id, attempt_no, touchpoint, call_seq, prompt_version,
		 output_schema_version, input_json, input_digest, status, started_at_ms)
		VALUES ('x6', 'run', 's', NULL, 1, 'T4', 1, 'pv', 1, '{}', 'd', 'running', 0)`)
	mustFail(t, db, `INSERT INTO brain_calls
		(id, scope, subject_key, run_id, attempt_no, touchpoint, call_seq, prompt_version,
		 output_schema_version, input_json, input_digest, status, started_at_ms)
		VALUES ('x7', 'run', 's', 'r1', 99, 'T4', 1, 'pv', 1, '{}', 'd', 'running', 0)`)

	// Legal: T1 without run, T2 without attempt, T7 aggregate.
	insertBrainCallT1(t, db, "ok1", "p1", "s", 1)
	insertBrainCallT2(t, db, "ok2", "r1", 1)
	mustExec(t, db, `INSERT INTO brain_calls
		(id, scope, subject_key, run_id, touchpoint, call_seq, prompt_version,
		 output_schema_version, input_json, input_digest, status, started_at_ms)
		VALUES ('ok3', 'aggregate', 'global', NULL, 'T7', 1, 'pv', 1, '{}', 'd', 'running', 0)`)
	// Legal: T4 bound to a real attempt of the same run.
	insertAttempt(t, db, "r1", 1, "ts1")
	mustExec(t, db, `INSERT INTO brain_calls
		(id, scope, subject_key, run_id, attempt_no, touchpoint, call_seq, prompt_version,
		 output_schema_version, input_json, input_digest, status, started_at_ms)
		VALUES ('ok4', 'run', 's', 'r1', 1, 'T4', 1, 'pv', 1, '{}', 'd', 'running', 0)`)
}

func TestBrainAttemptChecks(t *testing.T) {
	db, _ := openTestDB(t)
	seedRunChain(t, db)
	insertBrainCallT1(t, db, "bc1", "p1", "s", 1)

	// provider_attempt 0 is fallback-only and carries no provider facts.
	mustFail(t, db, `INSERT INTO brain_attempts
		(id, logical_call_id, provider_attempt, outcome, request_digest, started_at_ms, finished_at_ms)
		VALUES ('ba1', 'bc1', 0, 'valid', 'd', 0, 0)`)
	mustFail(t, db, `INSERT INTO brain_attempts
		(id, logical_call_id, provider_attempt, outcome, request_digest, exit_code,
		 started_at_ms, finished_at_ms)
		VALUES ('ba2', 'bc1', 0, 'fallback', 'd', 1, 0, 0)`)
	// provider attempts are 1 or 2.
	mustFail(t, db, `INSERT INTO brain_attempts
		(id, logical_call_id, provider_attempt, outcome, request_digest, started_at_ms, finished_at_ms)
		VALUES ('ba3', 'bc1', 3, 'invalid_output', 'd', 0, 0)`)
	// provider_error requires a code; other outcomes forbid one.
	mustFail(t, db, `INSERT INTO brain_attempts
		(id, logical_call_id, provider_attempt, outcome, request_digest, started_at_ms, finished_at_ms)
		VALUES ('ba4', 'bc1', 1, 'provider_error', 'd', 0, 0)`)
	mustFail(t, db, `INSERT INTO brain_attempts
		(id, logical_call_id, provider_attempt, outcome, provider_error_code, request_digest,
		 started_at_ms, finished_at_ms)
		VALUES ('ba5', 'bc1', 1, 'invalid_output', 'timeout', 'd', 0, 0)`)
	// valid requires tokens and a raw output digest.
	mustFail(t, db, `INSERT INTO brain_attempts
		(id, logical_call_id, provider_attempt, outcome, request_digest, started_at_ms, finished_at_ms)
		VALUES ('ba6', 'bc1', 1, 'valid', 'd', 0, 0)`)
	// output_too_large implies the raw output was truncated.
	mustFail(t, db, `INSERT INTO brain_attempts
		(id, logical_call_id, provider_attempt, outcome, provider_error_code, request_digest,
		 raw_output_digest, raw_output_bytes, raw_output_truncated, started_at_ms, finished_at_ms)
		VALUES ('ba7', 'bc1', 1, 'provider_error', 'output_too_large', 'd', 'rod', 10, 0, 0, 0)`)

	insertBrainAttemptValid(t, db, "ba8", "bc1", 1)
	insertBrainAttemptFallback(t, db, "ba9", "bc1")
}

func TestOutboxChecks(t *testing.T) {
	db, _ := openTestDB(t)
	seedRunChain(t, db)

	// executing requires a complete lease; a half lease is rejected anywhere.
	mustFail(t, db, `INSERT INTO outbox_operations
		(id, operation_key, kind, state, payload_schema_version, payload_json, payload_digest,
		 next_attempt_at_ms, created_at_ms, updated_at_ms)
		VALUES ('o1', 'k1', 'forge_comment', 'executing', 1, '{}', 'd', 0, 0, 0)`)
	mustFail(t, db, `INSERT INTO outbox_operations
		(id, operation_key, kind, state, payload_schema_version, payload_json, payload_digest,
		 lease_owner, next_attempt_at_ms, created_at_ms, updated_at_ms)
		VALUES ('o2', 'k2', 'forge_comment', 'pending', 1, '{}', 'd', 'w', 0, 0, 0)`)
	// attempt_no requires run_id.
	mustFail(t, db, `INSERT INTO outbox_operations
		(id, operation_key, kind, attempt_no, state, payload_schema_version, payload_json,
		 payload_digest, next_attempt_at_ms, created_at_ms, updated_at_ms)
		VALUES ('o3', 'k3', 'launch_agent', 1, 'pending', 1, '{}', 'd', 0, 0, 0)`)

	insertOutboxOperation(t, db, "o4", "k4")
	mustExec(t, db, `UPDATE outbox_operations SET state = 'executing', lease_owner = 'w1',
		lease_expires_at_ms = 5, attempt_count = 1 WHERE id = 'o4'`)
}

func TestClaimAndProbeChecks(t *testing.T) {
	db, _ := openTestDB(t)
	seedRunChain(t, db)
	insertAttempt(t, db, "r1", 1, "ts1")

	// dispatch fields move as a group.
	mustFail(t, db, `INSERT INTO attempt_claims
		(run_id, attempt_no, generation, launch_operation_key, dispatch_id, created_at_ms, updated_at_ms)
		VALUES ('r1', 1, 1, 'k1', 'd1', 0, 0)`)
	insertAttemptClaim(t, db, "r1", 1, "k2")

	// probe evidence moves as a group.
	insertEvent(t, db, "e1")
	insertBudgetEntry(t, db, "be1", "op-be1")
	insertInterrupt(t, db, "i1", "r1", "be1")
	mustFail(t, db, `INSERT INTO attempt_probes
		(id, run_id, attempt_no, interrupt_id, state, expected_run_version, expected_generation,
		 requested_by_event_id, absence_evidence_json, created_at_ms)
		VALUES ('p1', 'r1', 1, 'i1', 'succeeded', 1, 1, 'e1', '{}', 0)`)
	insertAttemptProbe(t, db, "p2", "r1", 1, "i1", "e1")
}

func TestForeignKeysEnforced(t *testing.T) {
	db, _ := openTestDB(t)
	seedRunChain(t, db)

	// attempt for a run that does not exist.
	mustFail(t, db, `INSERT INTO attempts
		(run_id, attempt_no, phase, generation, backend, agent_id, task_spec_snapshot_id,
		 worktree_path, branch_name, base_ref, base_sha, created_at_ms, updated_at_ms)
		VALUES ('ghost', 1, 'pending', 1, 'process', 'claude', 'ts1', '/wt', 'b', 'main', 'abc', 0, 0)`)
	// event referencing a missing run / project.
	mustFail(t, db, `INSERT INTO events
		(id, run_id, type, source, payload_schema_version, payload_json, occurred_at_ms, recorded_at_ms)
		VALUES ('e1', 'ghost', 't', 'system', 1, '{}', 0, 0)`)
	mustFail(t, db, `INSERT INTO events
		(id, project_id, type, source, payload_schema_version, payload_json, occurred_at_ms, recorded_at_ms)
		VALUES ('e2', 'ghost', 't', 'system', 1, '{}', 0, 0)`)
	// task spec composite FK: the snapshot must belong to the same run.
	insertManualRun(t, db, "r2", "p1", "cfg1")
	mustFail(t, db, `INSERT INTO attempts
		(run_id, attempt_no, phase, generation, backend, agent_id, task_spec_snapshot_id,
		 worktree_path, branch_name, base_ref, base_sha, created_at_ms, updated_at_ms)
		VALUES ('r2', 1, 'pending', 1, 'process', 'claude', 'ts1', '/wt', 'b', 'main', 'abc', 0, 0)`)
	// brain call referencing a missing gate snapshot.
	insertBrainCallT1(t, db, "bc1", "p1", "s", 1)
	mustFail(t, db, `INSERT INTO brain_calls
		(id, scope, subject_key, project_id, touchpoint, call_seq, prompt_version,
		 output_schema_version, input_json, input_digest, status, gate_input_snapshot_id, started_at_ms)
		VALUES ('bc2', 'intake', 's', 'p1', 'T1', 2, 'pv', 1, '{}', 'd', 'running', 'ghost', 0)`)
	// brain attempt referencing a missing call.
	mustFail(t, db, `INSERT INTO brain_attempts
		(id, logical_call_id, provider_attempt, outcome, request_digest, started_at_ms, finished_at_ms)
		VALUES ('ba1', 'ghost', 1, 'invalid_output', 'd', 0, 0)`)
}

func TestUniqueness(t *testing.T) {
	db, _ := openTestDB(t)
	seedRunChain(t, db)
	insertAttempt(t, db, "r1", 1, "ts1")
	insertEvent(t, db, "e1")

	// Intake idempotency key: unique only when issue_id is set.
	insertForgeRun(t, db, "rf1", "p1", "cfg1", "42")
	mustFail(t, db, `INSERT INTO runs
		(id, source_kind, project_id, config_snapshot_id, forge_kind, forge_host, forge_project_key,
		 issue_id, status, max_attempts, created_at_ms, updated_at_ms)
		VALUES ('rf2', 'forge', 'p1', 'cfg1', 'github', 'github.com', 'org/repo-p1', '42', 'queued', 3, 0, 0)`)
	insertManualRun(t, db, "rm1", "p1", "cfg1") // manual runs share forge key but no issue: allowed

	// At most one live attempt per run.
	mustFail(t, db, `INSERT INTO attempts
		(run_id, attempt_no, phase, generation, backend, agent_id, task_spec_snapshot_id,
		 worktree_path, branch_name, base_ref, base_sha, created_at_ms, updated_at_ms)
		VALUES ('r1', 2, 'spawning', 1, 'process', 'claude', 'ts1', '/wt2', 'b', 'main', 'abc', 0, 0)`)
	mustExec(t, db, `UPDATE attempts SET phase = 'finished', result_exit_code = 0,
		result_digest = 'rd', result_observed_at_ms = 1, finished_at_ms = 1
		WHERE run_id = 'r1' AND attempt_no = 1`)
	insertAttempt(t, db, "r1", 2, "ts1")

	// At most one live probe per interrupt.
	insertBudgetEntry(t, db, "be1", "op-be1")
	insertInterrupt(t, db, "i1", "r1", "be1")
	insertAttemptProbe(t, db, "pr1", "r1", 2, "i1", "e1")
	mustFail(t, db, `INSERT INTO attempt_probes
		(id, run_id, attempt_no, interrupt_id, state, expected_run_version, expected_generation,
		 requested_by_event_id, created_at_ms)
		VALUES ('pr2', 'r1', 2, 'i1', 'running', 1, 1, 'e1', 0)`)
	mustExec(t, db, `UPDATE attempt_probes SET state = 'superseded', finished_at_ms = 1 WHERE id = 'pr1'`)
	insertAttemptProbe(t, db, "pr3", "r1", 2, "i1", "e1")

	// events.idempotency_key, outbox/budget operation keys.
	mustExec(t, db, `INSERT INTO events
		(id, type, source, payload_schema_version, payload_json, idempotency_key, occurred_at_ms, recorded_at_ms)
		VALUES ('e2', 't', 'system', 1, '{}', 'ik1', 0, 0)`)
	mustFail(t, db, `INSERT INTO events
		(id, type, source, payload_schema_version, payload_json, idempotency_key, occurred_at_ms, recorded_at_ms)
		VALUES ('e3', 't', 'system', 1, '{}', 'ik1', 0, 0)`)
	insertOutboxOperation(t, db, "o1", "ok1")
	mustFail(t, db, `INSERT INTO outbox_operations
		(id, operation_key, kind, state, payload_schema_version, payload_json, payload_digest,
		 next_attempt_at_ms, created_at_ms, updated_at_ms)
		VALUES ('o2', 'ok1', 'forge_comment', 'pending', 1, '{}', 'd', 0, 0, 0)`)
	mustFail(t, db, `INSERT INTO budget_entries
		(id, kind, scope, scope_id, bucket_start_ms, amount, reason, operation_key, created_at_ms)
		VALUES ('be2', 'token', 'global', 'global', 0, 1, 'r', 'op-be1', 0)`)

	// forge receipts dedupe on (project, forge_event_id).
	insertForgeReceipt(t, db, "fr1", "p1", "fe1")
	mustFail(t, db, `INSERT INTO forge_event_receipts
		(id, project_id, forge_event_id, event_kind, target_kind, target_id, raw_digest,
		 disposition, observed_at_ms)
		VALUES ('fr2', 'p1', 'fe1', 'issue', 'issue', '1', 'rd', 'accepted', 0)`)

	// report receipts dedupe on (run, attempt, report_key).
	insertReportReceipt(t, db, "rr1", "r1", 2, "e1", "rk1")
	mustFail(t, db, `INSERT INTO report_receipts
		(id, run_id, attempt_no, report_key, report_kind, payload_digest, event_id, received_at_ms)
		VALUES ('rr2', 'r1', 2, 'rk1', 'goal', 'pd2', 'e1', 0)`)

	// brain call identity is (scope, subject_key, touchpoint, call_seq);
	// attempts dedupe on (logical_call_id, provider_attempt).
	insertBrainCallT1(t, db, "bc1", "p1", "s", 1)
	mustFail(t, db, `INSERT INTO brain_calls
		(id, scope, subject_key, project_id, touchpoint, call_seq, prompt_version,
		 output_schema_version, input_json, input_digest, status, started_at_ms)
		VALUES ('bc2', 'intake', 's', 'p1', 'T1', 1, 'pv', 1, '{}', 'd', 'running', 0)`)
	insertBrainAttemptValid(t, db, "ba1", "bc1", 1)
	mustFail(t, db, `INSERT INTO brain_attempts
		(id, logical_call_id, provider_attempt, outcome, request_digest, input_tokens, output_tokens,
		 raw_output_digest, started_at_ms, finished_at_ms)
		VALUES ('ba2', 'bc1', 1, 'valid', 'd', 1, 1, 'rod', 0, 0)`)

	// project forge key and enabled repo_path.
	mustFail(t, db, `INSERT INTO projects
		(id, config_snapshot_id, forge_kind, forge_host, forge_project_key, repo_path, enabled,
		 health, capabilities_json, created_at_ms, updated_at_ms)
		VALUES ('p2', 'cfg1', 'github', 'github.com', 'org/repo-p1', '/repo/other', 1, 'active', '{}', 0, 0)`)
	mustFail(t, db, `INSERT INTO projects
		(id, config_snapshot_id, forge_kind, forge_host, forge_project_key, repo_path, enabled,
		 health, capabilities_json, created_at_ms, updated_at_ms)
		VALUES ('p3', 'cfg1', 'gitlab', 'gitlab.com', 'org/repo-p3', '/repo/p1', 1, 'active', '{}', 0, 0)`)
	mustExec(t, db, `INSERT INTO projects
		(id, config_snapshot_id, forge_kind, forge_host, forge_project_key, repo_path, enabled,
		 health, capabilities_json, created_at_ms, updated_at_ms)
		VALUES ('p4', 'cfg1', 'gitlab', 'gitlab.com', 'org/repo-p4', '/repo/p1', 0, 'active', '{}', 0, 0)`)
}

// TestIsolationIndependentOfRunTerminal: a Run reaching its terminal state
// does not disturb a frozen attempt, and isolation is released explicitly.
func TestIsolationIndependentOfRunTerminal(t *testing.T) {
	db, _ := openTestDB(t)
	seedRunChain(t, db)
	insertAttempt(t, db, "r1", 1, "ts1")
	mustExec(t, db, `UPDATE attempts SET isolation_state = 'frozen', isolation_reason = 'startup_stall',
		isolated_at_ms = 0 WHERE run_id = 'r1' AND attempt_no = 1`)
	// Run terminal transition leaves the frozen attempt untouched.
	mustExec(t, db, `UPDATE runs SET status = 'failed', failure_reason = 'operator_kill',
		completed_at_ms = 1, version = 2 WHERE id = 'r1'`)
	var iso string
	if err := db.db.QueryRow(`SELECT isolation_state FROM attempts WHERE run_id = 'r1' AND attempt_no = 1`).Scan(&iso); err != nil {
		t.Fatalf("read isolation: %v", err)
	}
	if iso != "frozen" {
		t.Fatalf("isolation_state = %q after Run terminal, want frozen", iso)
	}
	// Explicit release records the evidence event and retains the frozen facts.
	insertEvent(t, db, "isolation-released")
	mustExec(t, db, `UPDATE attempts SET isolation_state = 'none', isolation_released_at_ms = 2,
		isolation_release_event_id = 'isolation-released'
		WHERE run_id = 'r1' AND attempt_no = 1`)
}

func TestAppendOnlyErrorMentionsTable(t *testing.T) {
	db, _ := openTestDB(t)
	insertEvent(t, db, "e1")
	err := mustFail(t, db, `DELETE FROM events WHERE id = 'e1'`)
	if !strings.Contains(err.Error(), "append-only table") {
		t.Fatalf("error = %v, want append-only trigger message", err)
	}
}
