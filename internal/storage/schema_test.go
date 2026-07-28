package storage

import (
	"strings"
	"testing"
)

// This file exercises the M1 schema contract of specs/storage.md directly
// through SQL: the tables and indexes exist, enum/nullability CHECKs hold,
// composite foreign keys are enforced, and the §13 triggers make append-only
// and write-once rules database facts rather than interface discipline.

func mustExec(t *testing.T, db *DB, query string, args ...any) {
	t.Helper()
	if _, err := db.db.Exec(query, args...); err != nil {
		t.Fatalf("exec failed: %v\nquery: %s", err, query)
	}
}

func mustFail(t *testing.T, db *DB, query string, args ...any) error {
	t.Helper()
	if _, err := db.db.Exec(query, args...); err != nil {
		return err
	}
	t.Fatalf("expected failure but exec succeeded:\n%s", query)
	return nil
}

// --- Row factories. Each inserts one minimally valid row. ---

func insertConfigSnapshot(t *testing.T, db *DB, id string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO config_snapshots
		(id, config_hash, schema_version, canonical_json, source_present, source_mtime_ms, loaded_at_ms, binary_version)
		VALUES (?, ?, 1, '{}', 1, NULL, ?, 'test-binary')`, id, "hash-"+id, testNow)
}

func insertDaemonBoot(t *testing.T, db *DB, id, cfgID string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO daemon_boots
		(id, config_snapshot_id, pid, binary_version, protocol_major, started_at_ms)
		VALUES (?, ?, 4242, 'test-binary', 1, ?)`, id, cfgID, testNow)
}

func insertProject(t *testing.T, db *DB, id, cfgID string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO projects
		(id, config_snapshot_id, forge_kind, forge_host, forge_project_key, repo_path,
		 enabled, health, isolation_reason, capabilities_json, capabilities_checked_at_ms,
		 created_at_ms, updated_at_ms)
		VALUES (?, ?, 'github', 'github.com', ?, ?, 1, 'active', NULL, '{}', NULL, ?, ?)`,
		id, cfgID, "org/repo-"+id, "/repo/"+id, testNow, testNow)
}

func insertManualRun(t *testing.T, db *DB, id, projID, cfgID string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO runs
		(id, source_kind, project_id, config_snapshot_id, forge_kind, forge_host, forge_project_key,
		 discussion_target_kind, discussion_target_id, discussion_target_url,
		 status, max_attempts, created_at_ms, updated_at_ms)
		VALUES (?, 'manual', ?, ?, 'github', 'github.com', ?, 'issue', 'manual-target',
		 'https://github.com/org/repo/issues/1', 'queued', 3, ?, ?)`,
		id, projID, cfgID, "org/repo-"+projID, testNow, testNow)
}

func insertForgeRun(t *testing.T, db *DB, id, projID, cfgID, issueID string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO runs
		(id, source_kind, project_id, config_snapshot_id, forge_kind, forge_host, forge_project_key,
		 issue_id, status, max_attempts, created_at_ms, updated_at_ms)
		VALUES (?, 'forge', ?, ?, 'github', 'github.com', ?, ?, 'queued', 3, ?, ?)`,
		id, projID, cfgID, "org/repo-"+projID, issueID, testNow, testNow)
}

func insertTaskSpec(t *testing.T, db *DB, id, runID string, version int) {
	t.Helper()
	mustExec(t, db, `INSERT INTO task_spec_snapshots
		(id, run_id, version, schema_version, canonical_json, content_digest, source_event_id, created_at_ms)
		VALUES (?, ?, ?, 1, '{}', ?, NULL, ?)`, id, runID, version, "digest-"+id, testNow)
}

func insertAttempt(t *testing.T, db *DB, runID string, no int, specID string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO attempts
		(run_id, attempt_no, phase, generation, backend, agent_id, task_spec_snapshot_id,
		 worktree_path, branch_name, base_ref, base_sha, created_at_ms, updated_at_ms)
		VALUES (?, ?, 'pending', 1, 'process', 'claude', ?, '/wt', 'sift/x', 'main', 'abc', ?, ?)`,
		runID, no, specID, testNow, testNow)
}

func insertAttemptClaim(t *testing.T, db *DB, runID string, no int, opKey string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO attempt_claims
		(run_id, attempt_no, generation, launch_operation_key, created_at_ms, updated_at_ms)
		VALUES (?, ?, 1, ?, ?, ?)`, runID, no, opKey, testNow, testNow)
}

func insertEvent(t *testing.T, db *DB, id string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO events
		(id, run_id, attempt_no, project_id, type, source, actor, payload_schema_version,
		 payload_json, idempotency_key, occurred_at_ms, recorded_at_ms)
		VALUES (?, NULL, NULL, NULL, 'test.event', 'system', NULL, 1, '{}', NULL, ?, ?)`,
		id, testNow, testNow)
}

func insertForgeReceipt(t *testing.T, db *DB, id, projID, forgeEventID string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO forge_event_receipts
		(id, project_id, forge_event_id, event_kind, target_kind, target_id, actor,
		 raw_digest, disposition, domain_event_id, observed_at_ms)
		VALUES (?, ?, ?, 'issue_comment', 'issue', '1', 'alice', 'rd', 'accepted', NULL, ?)`,
		id, projID, forgeEventID, testNow)
}

func insertBudgetEntry(t *testing.T, db *DB, id, opKey string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO budget_entries
		(id, kind, scope, scope_id, bucket_start_ms, amount, reason, run_id, operation_key, created_at_ms)
		VALUES (?, 'attention', 'severity', 'normal', ?, 1, 'test', NULL, ?, ?)`,
		id, testNow, opKey, testNow)
}

func insertInterrupt(t *testing.T, db *DB, id, runID, entryID string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO interrupts
		(id, run_id, attempt_no, generation_key, reason, severity, headline, brief_markdown,
		 options_json, min_modality, links_json, nonce, version, status, dispatch_state,
		 expires_at_ms, on_expire, escalation_count, max_escalations, close_reason, closed_at_ms,
		 charged_budget_entry_id, created_at_ms, updated_at_ms)
		VALUES (?, ?, NULL, ?, 'code_review', 'normal', 'review change', 'brief',
		 '[{"id":"approve"}]', 'visual', '[]', 'nonce-1', 1, 'open', 'ready',
		 ?, 'hold', 0, 3, NULL, NULL, ?, ?, ?)`,
		id, runID, "gk-"+id, testNow+3_600_000, entryID, testNow, testNow)
}

func insertInterruptDelivery(t *testing.T, db *DB, id, interruptID, opKey string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO interrupt_deliveries
		(id, interrupt_id, surface, priority, operation_key, state, attempt_count, created_at_ms)
		VALUES (?, ?, 'channel', 'normal', ?, 'pending', 0, ?)`, id, interruptID, opKey, testNow)
}

func insertAttemptProbe(t *testing.T, db *DB, id, runID string, no int, interruptID, eventID string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO attempt_probes
		(id, run_id, attempt_no, interrupt_id, state, expected_run_version, expected_generation,
		 requested_by_event_id, created_at_ms)
		VALUES (?, ?, ?, ?, 'pending', 1, 1, ?, ?)`, id, runID, no, interruptID, eventID, testNow)
}

func insertOutboxOperation(t *testing.T, db *DB, id, opKey string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO outbox_operations
		(id, operation_key, kind, state, payload_schema_version, payload_json, payload_digest,
		 attempt_count, next_attempt_at_ms, created_at_ms, updated_at_ms)
		VALUES (?, ?, 'forge_comment', 'pending', 1, '{}', ?, 0, ?, ?, ?)`,
		id, opKey, "pd-"+id, testNow, testNow, testNow)
}

func insertOutboxAttempt(t *testing.T, db *DB, id, opID string, no int) {
	t.Helper()
	mustExec(t, db, `INSERT INTO outbox_attempts (id, operation_id, attempt_no, worker_id, started_at_ms)
		VALUES (?, ?, ?, 'worker-1', ?)`, id, opID, no, testNow)
}

func insertOutboxAttemptResult(t *testing.T, db *DB, attemptID string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO outbox_attempt_results (attempt_id, finished_at_ms, outcome)
		VALUES (?, ?, 'success')`, attemptID, testNow)
}

func insertGateSnapshot(t *testing.T, db *DB, id, hash string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO gate_input_snapshots
		(id, gate_input_hash, schema_version, canonical_json, head_sha, effective_policy_hash,
		 certification_version, risk_source_version, created_at_ms)
		VALUES (?, ?, 1, '{}', 'sha', 'policy-hash', 'cv1', 'rv1', ?)`, id, hash, testNow)
}

func insertGateEvaluation(t *testing.T, db *DB, id, runID, snapID string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO gate_evaluations
		(id, run_id, snapshot_id, gate_version, verdict_json, verdict_digest, cache_hit, created_at_ms)
		VALUES (?, ?, ?, 'gv1', '{}', ?, 0, ?)`, id, runID, snapID, "vd-"+id, testNow)
}

func insertGateCache(t *testing.T, db *DB, hash, version, snapID string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO gate_cache
		(gate_input_hash, gate_version, snapshot_id, verdict_json, verdict_digest, created_at_ms)
		VALUES (?, ?, ?, '{}', 'vd', ?)`, hash, version, snapID, testNow)
}

func insertCalibration(t *testing.T, db *DB, id, runID, evalID string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO calibration_entries
		(id, run_id, gate_evaluation_id, predicted_decision, gate_bypassed, features_json, predicted_at_ms)
		VALUES (?, ?, ?, 'approve', 0, '{}', ?)`, id, runID, evalID, testNow)
}

func insertLedgerEntry(t *testing.T, db *DB, id, runID string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO ledger_entries
		(id, run_id, interrupt_id, entry_kind, features_schema_version, features_json, created_at_ms)
		VALUES (?, ?, NULL, 'gate_sample', 1, '{}', ?)`, id, runID, testNow)
}

func insertBrainCallT1(t *testing.T, db *DB, id, projID, subject string, seq int) {
	t.Helper()
	mustExec(t, db, `INSERT INTO brain_calls
		(id, scope, subject_key, project_id, run_id, attempt_no, touchpoint, call_seq,
		 prompt_version, output_schema_version, input_json, input_digest, status, started_at_ms)
		VALUES (?, 'intake', ?, ?, NULL, NULL, 'T1', ?, 'pv1', 1, '{}', 'digest-' || ?, 'running', ?)`,
		id, subject, projID, seq, id, testNow)
}

func insertBrainCallT2(t *testing.T, db *DB, id, runID string, seq int) {
	t.Helper()
	mustExec(t, db, `INSERT INTO brain_calls
		(id, scope, subject_key, project_id, run_id, attempt_no, touchpoint, call_seq,
		 prompt_version, output_schema_version, input_json, input_digest, status, started_at_ms)
		VALUES (?, 'run', 'run:'||?, NULL, ?, NULL, 'T2', ?, 'pv1', 1, '{}', 'digest-' || ?, 'running', ?)`,
		id, runID, runID, seq, id, testNow)
}

func insertBrainAttemptFallback(t *testing.T, db *DB, id, callID string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO brain_attempts
		(id, logical_call_id, provider_attempt, outcome, provider_error_code, request_digest,
		 raw_output_truncated, stderr_truncated, started_at_ms, finished_at_ms)
		VALUES (?, ?, 0, 'fallback', NULL, 'digest', 0, 0, ?, ?)`, id, callID, testNow, testNow)
}

func insertBrainAttemptValid(t *testing.T, db *DB, id, callID string, n int) {
	t.Helper()
	mustExec(t, db, `INSERT INTO brain_attempts
		(id, logical_call_id, provider_attempt, outcome, provider_error_code, request_digest,
		 raw_output_text, raw_output_digest, raw_output_bytes, raw_output_truncated,
		 stderr_summary, stderr_truncated, exit_code, input_tokens, output_tokens,
		 started_at_ms, finished_at_ms)
		VALUES (?, ?, ?, 'valid', NULL, 'digest', 'out', 'rod', 3, 0, NULL, 0, 0, 10, 5, ?, ?)`,
		id, callID, n, testNow, testNow)
}

func insertIntakeItem(t *testing.T, db *DB, id, projID, issueID string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO intake_items
		(id, project_id, forge_kind, normalized_host, forge_project_key, issue_id, issue_url,
		 issue_digest, state, version, clarification_generation, created_at_ms, updated_at_ms)
		VALUES (?, ?, 'github', 'github.com', ?, ?, 'https://github.com/x', 'digest', 'pending_evaluation', 1, 0, ?, ?)`,
		id, projID, "org/repo-"+projID, issueID, testNow, testNow)
}

func insertIntakeAssessment(t *testing.T, db *DB, id, intakeID, callID string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO intake_assessments
		(id, intake_id, logical_call_id, disposition, questions_json, possible_duplicate_run_id,
		 rationale, created_at_ms)
		VALUES (?, ?, ?, 'ready', '[]', NULL, 'looks actionable', ?)`, id, intakeID, callID, testNow)
}

func insertReportReceipt(t *testing.T, db *DB, id, runID string, no int, eventID, key string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO report_receipts
		(id, run_id, attempt_no, report_key, report_kind, payload_digest, event_id, received_at_ms)
		VALUES (?, ?, ?, ?, 'progress', 'pd', ?, ?)`, id, runID, no, key, eventID, testNow)
}

func insertHookBaseline(t *testing.T, db *DB, projID string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO project_hook_baselines
		(project_id, git_config_digest, core_hooks_path_value, effective_hooks_path,
		 hooks_directory_digest, baseline_digest, source_run_id, source_attempt_no,
		 captured_at_ms, updated_at_ms)
		VALUES (?, 'gd', NULL, '/repo/.git/hooks', 'hdd', 'bd', NULL, NULL, ?, ?)`,
		projID, testNow, testNow)
}

func insertForgeCursor(t *testing.T, db *DB, projID, stream string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO forge_cursors
		(project_id, stream, cursor, etag, since_ms, poll_mode, next_poll_at_ms, updated_at_ms)
		VALUES (?, ?, NULL, NULL, NULL, 'active', ?, ?)`, projID, stream, testNow, testNow)
}

// --- Schema inventory ---

func TestSchemaInventory(t *testing.T) {
	db, _ := openTestDB(t)

	wantTables := []string{
		"schema_migrations",
		"config_snapshots", "daemon_boots", "projects", "project_hook_baselines",
		"task_spec_snapshots", "runs", "attempts", "attempt_claims", "attempt_probes",
		"interrupts", "interrupt_deliveries",
		"events", "forge_cursors", "forge_reply_state", "forge_event_receipts", "report_receipts",
		"intake_items", "intake_assessments",
		"outbox_operations", "outbox_attempts", "outbox_attempt_results",
		"budget_counters", "rate_limit_buckets", "budget_entries",
		"brain_call_counters", "brain_calls", "brain_attempts",
		"gate_input_snapshots", "gate_evaluations", "gate_cache",
		"calibration_entries", "certifications", "ledger_entries",
	}
	assertObjectsExist(t, db, "table", wantTables)

	// §15 M1 index minimum set (explicit indexes only; unique constraints
	// create their own implicit indexes).
	wantIndexes := []string{
		"projects_enabled_repo_path", "runs_intake_idempotency",
		"attempts_single_live_phase", "attempt_probes_one_live_per_interrupt",
		"task_spec_snapshots_run_version", "runs_status_updated", "runs_project_status",
		"runs_change_id", "project_hook_baselines_updated", "attempts_phase_updated",
		"attempts_run_attempt_desc", "interrupts_status_expires", "interrupts_run_status",
		"events_run_seq", "events_project_seq", "outbox_operations_state_next",
		"outbox_operations_lease_expiry", "budget_entries_kind_created_run",
		"forge_cursors_next_poll", "forge_reply_state_updated", "brain_calls_run_attempt_touchpoint",
		"intake_items_state_updated", "gate_evaluations_run_created", "ledger_entries_run_created",
	}
	assertObjectsExist(t, db, "index", wantIndexes)

	// §13 triggers.
	appendOnly := []string{
		"config_snapshots", "task_spec_snapshots", "events", "forge_event_receipts",
		"report_receipts", "outbox_attempts", "outbox_attempt_results", "budget_entries",
		"brain_attempts", "intake_assessments", "gate_input_snapshots", "gate_evaluations",
		"gate_cache", "ledger_entries",
	}
	wantTriggers := []string{
		"daemon_boots_stop_completion_only", "daemon_boots_append_only_delete",
		"calibration_entries_decision_completion_only", "calibration_entries_append_only_delete",
		"brain_calls_finalize_only", "brain_calls_append_only_delete",
		"outbox_operations_payload_immutable", "attempts_resolution_write_once",
		"attempt_claims_permit_irreplaceable", "interrupts_identity_immutable",
	}
	for _, table := range appendOnly {
		wantTriggers = append(wantTriggers, table+"_append_only_update", table+"_append_only_delete")
	}
	assertObjectsExist(t, db, "trigger", wantTriggers)
}

func assertObjectsExist(t *testing.T, db *DB, kind string, names []string) {
	t.Helper()
	for _, name := range names {
		var found string
		err := db.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = ? AND name = ?`, kind, name,
		).Scan(&found)
		if err != nil {
			t.Errorf("%s %q missing from schema: %v", kind, name, err)
		}
	}
}

// --- Happy path: a full dependency chain of valid rows is accepted ---

func TestValidRowChainAccepted(t *testing.T) {
	db, _ := openTestDB(t)

	insertConfigSnapshot(t, db, "cfg1")
	insertDaemonBoot(t, db, "boot1", "cfg1")
	insertProject(t, db, "p1", "cfg1")
	insertHookBaseline(t, db, "p1")
	insertForgeCursor(t, db, "p1", "issues")
	insertForgeReceipt(t, db, "fr1", "p1", "fe1")
	insertManualRun(t, db, "r1", "p1", "cfg1")
	insertForgeRun(t, db, "r2", "p1", "cfg1", "42")
	insertTaskSpec(t, db, "ts1", "r1", 1)
	mustExec(t, db, `UPDATE runs SET current_task_spec_id = 'ts1' WHERE id = 'r1'`)
	insertAttempt(t, db, "r1", 1, "ts1")
	insertAttemptClaim(t, db, "r1", 1, "launch-op-1")
	insertEvent(t, db, "e1")
	insertBudgetEntry(t, db, "be1", "op-be1")
	insertInterrupt(t, db, "i1", "r1", "be1")
	insertInterruptDelivery(t, db, "d1", "i1", "op-d1")
	insertAttemptProbe(t, db, "probe1", "r1", 1, "i1", "e1")
	insertOutboxOperation(t, db, "o1", "op-o1")
	insertOutboxAttempt(t, db, "oa1", "o1", 1)
	insertOutboxAttemptResult(t, db, "oa1")
	insertReportReceipt(t, db, "rr1", "r1", 1, "e1", "rk1")
	insertBrainCallT1(t, db, "bc1", "p1", "intake:github.com:org/repo-p1:42", 1)
	insertBrainCallT2(t, db, "bc2", "r1", 1)
	insertBrainAttemptFallback(t, db, "ba1", "bc1")
	insertBrainAttemptValid(t, db, "ba2", "bc1", 1)
	insertIntakeItem(t, db, "ii1", "p1", "42")
	insertIntakeAssessment(t, db, "ia1", "ii1", "bc1")
	insertGateSnapshot(t, db, "gs1", "hash-gs1")
	insertGateEvaluation(t, db, "ge1", "r1", "gs1")
	insertGateCache(t, db, "hash-gs1", "gv1", "gs1")
	insertCalibration(t, db, "ce1", "r1", "ge1")
	insertLedgerEntry(t, db, "le1", "r1")
	mustExec(t, db, `INSERT INTO budget_counters
		(kind, scope, scope_id, bucket_start_ms, bucket_end_ms, limit_value, consumed_value, version, updated_at_ms)
		VALUES ('token', 'global', 'global', 0, 86400000, 1000, 0, 1, ?)`, testNow)
	mustExec(t, db, `INSERT INTO rate_limit_buckets
		(kind, scope_id, capacity_units, available_units, refill_numerator, refill_period_ms,
		 refill_remainder, last_refill_at_ms, version)
		VALUES ('report', 'run:r1:attempt:1', 4, 4, 10, 60000, 0, ?, 1)`, testNow)
	mustExec(t, db, `INSERT INTO brain_call_counters
		(scope, subject_key, touchpoint, next_call_seq, version, updated_at_ms)
		VALUES ('intake', 'intake:github.com:org/repo-p1:42', 'T1', 2, 1, ?)`, testNow)
	mustExec(t, db, `INSERT INTO certifications
		(task_kind, certification_version, total_samples, negative_samples, leak_count,
		 false_block_count, certified, evidence_digest, updated_at_ms)
		VALUES ('bugfix', 'cv1', 0, 0, 0, 0, 0, 'ed', ?)`, testNow)
}

// --- §13 append-only tables reject UPDATE and DELETE at the database level ---

func TestAppendOnlyTablesRejectUpdateAndDelete(t *testing.T) {
	db, _ := openTestDB(t)

	// Shared fixture chain for tables with foreign keys.
	insertConfigSnapshot(t, db, "cfg1")
	insertProject(t, db, "p1", "cfg1")
	insertManualRun(t, db, "r1", "p1", "cfg1")
	insertTaskSpec(t, db, "ts1", "r1", 1)
	insertAttempt(t, db, "r1", 1, "ts1")
	insertEvent(t, db, "e1")

	cases := []struct {
		name  string
		seed  func(t *testing.T)
		table string
		key   string
	}{
		{"config_snapshots", func(t *testing.T) { /* seeded above */ },
			"config_snapshots", "id = 'cfg1'"},
		{"task_spec_snapshots", func(t *testing.T) { /* seeded above */ },
			"task_spec_snapshots", "id = 'ts1'"},
		{"events", func(t *testing.T) { /* seeded above */ },
			"events", "id = 'e1'"},
		{"forge_event_receipts", func(t *testing.T) { insertForgeReceipt(t, db, "fr1", "p1", "fe1") },
			"forge_event_receipts", "id = 'fr1'"},
		{"report_receipts", func(t *testing.T) { insertReportReceipt(t, db, "rr1", "r1", 1, "e1", "rk1") },
			"report_receipts", "id = 'rr1'"},
		{"outbox_attempts", func(t *testing.T) {
			insertOutboxOperation(t, db, "o1", "op-o1")
			insertOutboxAttempt(t, db, "oa1", "o1", 1)
		}, "outbox_attempts", "id = 'oa1'"},
		{"outbox_attempt_results", func(t *testing.T) { insertOutboxAttemptResult(t, db, "oa1") },
			"outbox_attempt_results", "attempt_id = 'oa1'"},
		{"budget_entries", func(t *testing.T) { insertBudgetEntry(t, db, "be1", "op-be1") },
			"budget_entries", "id = 'be1'"},
		{"brain_attempts", func(t *testing.T) {
			insertBrainCallT1(t, db, "bc1", "p1", "s", 1)
			insertBrainAttemptFallback(t, db, "ba1", "bc1")
		}, "brain_attempts", "id = 'ba1'"},
		{"intake_assessments", func(t *testing.T) {
			insertIntakeItem(t, db, "ii1", "p1", "42")
			insertIntakeAssessment(t, db, "ia1", "ii1", "bc1")
		}, "intake_assessments", "id = 'ia1'"},
		{"gate_input_snapshots", func(t *testing.T) { insertGateSnapshot(t, db, "gs1", "h1") },
			"gate_input_snapshots", "id = 'gs1'"},
		{"gate_evaluations", func(t *testing.T) { insertGateEvaluation(t, db, "ge1", "r1", "gs1") },
			"gate_evaluations", "id = 'ge1'"},
		{"gate_cache", func(t *testing.T) { insertGateCache(t, db, "h1", "gv1", "gs1") },
			"gate_cache", "gate_input_hash = 'h1'"},
		{"ledger_entries", func(t *testing.T) { insertLedgerEntry(t, db, "le1", "r1") },
			"ledger_entries", "id = 'le1'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.seed(t)
			err := mustFail(t, db, `UPDATE `+c.name+` SET rowid = rowid WHERE `+c.key)
			if !strings.Contains(err.Error(), "append-only table") {
				t.Errorf("UPDATE error = %v, want append-only trigger", err)
			}
			err = mustFail(t, db, `DELETE FROM `+c.name+` WHERE `+c.key)
			if !strings.Contains(err.Error(), "append-only table") {
				t.Errorf("DELETE error = %v, want append-only trigger", err)
			}
		})
	}
}

// --- §13 one-time completion exceptions ---

func TestDaemonBootStopCompletion(t *testing.T) {
	db, _ := openTestDB(t)
	insertConfigSnapshot(t, db, "cfg1")
	insertDaemonBoot(t, db, "boot1", "cfg1")
	insertDaemonBoot(t, db, "boot2", "cfg1")

	// The one-time stop completion is the only permitted update.
	mustExec(t, db, `UPDATE daemon_boots SET stopped_at_ms = ?, stop_reason = 'shutdown' WHERE id = 'boot1'`, testNow+1000)
	// Already completed: no further writes.
	mustFail(t, db, `UPDATE daemon_boots SET stop_reason = 'other' WHERE id = 'boot1'`)
	// Any other column is frozen even while stop fields are still NULL.
	mustFail(t, db, `UPDATE daemon_boots SET pid = 9999 WHERE id = 'boot2'`)
	mustFail(t, db, `UPDATE daemon_boots SET started_at_ms = 0 WHERE id = 'boot2'`)
	mustFail(t, db, `DELETE FROM daemon_boots WHERE id = 'boot2'`)
}

func TestCalibrationDecisionCompletion(t *testing.T) {
	db, _ := openTestDB(t)
	insertConfigSnapshot(t, db, "cfg1")
	insertProject(t, db, "p1", "cfg1")
	insertManualRun(t, db, "r1", "p1", "cfg1")
	insertGateSnapshot(t, db, "gs1", "h1")
	insertGateEvaluation(t, db, "ge1", "r1", "gs1")
	insertCalibration(t, db, "ce1", "r1", "ge1")
	insertCalibration(t, db, "ce2", "r1", "ge1")

	// One-time completion of the human decision group.
	mustExec(t, db, `UPDATE calibration_entries
		SET human_decision = 'reject', decision_source = 'command', decided_at_ms = ?
		WHERE id = 'ce1'`, testNow+1000)
	// Completed entries cannot be revised.
	mustFail(t, db, `UPDATE calibration_entries SET human_decision = 'approve' WHERE id = 'ce1'`)
	// Predicted fields are frozen; partial completion is rejected by CHECK.
	mustFail(t, db, `UPDATE calibration_entries SET predicted_decision = 'reject' WHERE id = 'ce2'`)
	mustFail(t, db, `UPDATE calibration_entries SET human_decision = 'approve' WHERE id = 'ce2'`)
	mustFail(t, db, `DELETE FROM calibration_entries WHERE id = 'ce2'`)
}

func TestBrainCallSingleFinalize(t *testing.T) {
	db, _ := openTestDB(t)
	insertConfigSnapshot(t, db, "cfg1")
	insertProject(t, db, "p1", "cfg1")
	insertBrainCallT1(t, db, "bc1", "p1", "s", 1)
	insertBrainAttemptValid(t, db, "ba1", "bc1", 1)

	// running -> valid finalize referencing this call's valid attempt.
	mustExec(t, db, `UPDATE brain_calls
		SET status = 'valid', selected_attempt_no = 1, validated_output_json = '{}', finished_at_ms = ?
		WHERE id = 'bc1'`, testNow+1000)
	// Finalized is terminal: no second finalize.
	mustFail(t, db, `UPDATE brain_calls SET status = 'fallback', fallback_reason = 'x' WHERE id = 'bc1'`)
	// Identity and input columns can never change.
	mustFail(t, db, `UPDATE brain_calls SET input_json = '{"x":1}' WHERE id = 'bc1'`)
	mustFail(t, db, `UPDATE brain_calls SET call_seq = 2 WHERE id = 'bc1'`)
	// DELETE is forbidden outright.
	mustFail(t, db, `DELETE FROM brain_calls WHERE id = 'bc1'`)

	// A running call rejects identity edits and malformed finalizes.
	insertBrainCallT1(t, db, "bc2", "p1", "s2", 1)
	mustFail(t, db, `UPDATE brain_calls SET subject_key = 'other' WHERE id = 'bc2'`)
	mustFail(t, db, `UPDATE brain_calls SET status = 'valid', selected_attempt_no = 1,
		validated_output_json = '{}', finished_at_ms = 0 WHERE id = 'bc2'`) // no such attempt: FK
	mustFail(t, db, `UPDATE brain_calls SET status = 'fallback' WHERE id = 'bc2'`) // CHECK: reason required

	// Fallback finalize without any provider attempt (synthesized attempt 0).
	insertBrainAttemptFallback(t, db, "ba2", "bc2")
	mustExec(t, db, `UPDATE brain_calls
		SET status = 'fallback', fallback_reason = 'provider_unavailable', finished_at_ms = ?
		WHERE id = 'bc2'`, testNow+1000)
}

// --- §13 column-level immutability on mutable projections ---

func TestOutboxPayloadImmutable(t *testing.T) {
	db, _ := openTestDB(t)
	insertOutboxOperation(t, db, "o1", "op-o1")

	// Execution fields move freely...
	mustExec(t, db, `UPDATE outbox_operations
		SET state = 'executing', lease_owner = 'w1', lease_expires_at_ms = ?, attempt_count = 1, updated_at_ms = ?
		WHERE id = 'o1'`, testNow+5000, testNow+1)
	// ...payload never does.
	mustFail(t, db, `UPDATE outbox_operations SET payload_json = '{"x":1}' WHERE id = 'o1'`)
	mustFail(t, db, `UPDATE outbox_operations SET payload_digest = 'other' WHERE id = 'o1'`)
	mustFail(t, db, `UPDATE outbox_operations SET payload_schema_version = 2 WHERE id = 'o1'`)
}

func TestAttemptResolutionWriteOnce(t *testing.T) {
	db, _ := openTestDB(t)
	insertConfigSnapshot(t, db, "cfg1")
	insertProject(t, db, "p1", "cfg1")
	insertManualRun(t, db, "r1", "p1", "cfg1")
	insertTaskSpec(t, db, "ts1", "r1", 1)
	insertAttempt(t, db, "r1", 1, "ts1")

	// Written once with its timestamp.
	mustExec(t, db, `UPDATE attempts
		SET phase = 'orphaned', attempt_resolution = 'retry_after_absence', resolution_at_ms = ?
		WHERE run_id = 'r1' AND attempt_no = 1`, testNow+1000)
	// Never revised, never cleared.
	mustFail(t, db, `UPDATE attempts SET attempt_resolution = 'reject' WHERE run_id = 'r1' AND attempt_no = 1`)
	mustFail(t, db, `UPDATE attempts SET resolution_at_ms = NULL WHERE run_id = 'r1' AND attempt_no = 1`)
	// Unrelated columns still move (isolation release is independent).
	mustExec(t, db, `UPDATE attempts SET isolation_state = 'frozen', isolation_reason = 'startup_stall',
		isolated_at_ms = ? WHERE run_id = 'r1' AND attempt_no = 1`, testNow+1000)
}

func TestAttemptClaimPermitIrreplaceable(t *testing.T) {
	db, _ := openTestDB(t)
	insertConfigSnapshot(t, db, "cfg1")
	insertProject(t, db, "p1", "cfg1")
	insertManualRun(t, db, "r1", "p1", "cfg1")
	insertTaskSpec(t, db, "ts1", "r1", 1)
	insertAttempt(t, db, "r1", 1, "ts1")
	insertAttemptClaim(t, db, "r1", 1, "launch-op-1")

	mustExec(t, db, `UPDATE attempt_claims SET spawn_permit_hash = 'h1', permit_issued_at_ms = ?
		WHERE run_id = 'r1' AND attempt_no = 1`, testNow)
	mustFail(t, db, `UPDATE attempt_claims SET spawn_permit_hash = 'h2' WHERE run_id = 'r1' AND attempt_no = 1`)
	mustFail(t, db, `UPDATE attempt_claims SET spawn_permit_hash = NULL WHERE run_id = 'r1' AND attempt_no = 1`)
	// Dispatch preparation fields remain writable.
	mustExec(t, db, `UPDATE attempt_claims
		SET dispatch_id = 'd1', bootstrap_nonce_hash = 'bnh', run_token_hash = 'rth'
		WHERE run_id = 'r1' AND attempt_no = 1`)
}

func TestInterruptIdentityImmutable(t *testing.T) {
	db, _ := openTestDB(t)
	insertConfigSnapshot(t, db, "cfg1")
	insertProject(t, db, "p1", "cfg1")
	insertManualRun(t, db, "r1", "p1", "cfg1")
	insertBudgetEntry(t, db, "be1", "op-be1")
	insertInterrupt(t, db, "i1", "r1", "be1")

	// Escalation rotates nonce and bumps version — allowed.
	mustExec(t, db, `UPDATE interrupts SET version = 2, nonce = 'nonce-2', escalation_count = 1,
		updated_at_ms = ? WHERE id = 'i1'`, testNow+1)
	// generation_key and charged entry are creation-time facts.
	mustFail(t, db, `UPDATE interrupts SET generation_key = 'other' WHERE id = 'i1'`)
	mustFail(t, db, `UPDATE interrupts SET charged_budget_entry_id = 'other' WHERE id = 'i1'`)
}
