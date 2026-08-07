package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// MergeConflictDigest identifies the exact durable conflicting Change observation.
func MergeConflictDigest(changeID, headSHA string) string {
	body, _ := json.Marshal(struct {
		ChangeID     string `json:"change_id"`
		HeadSHA      string `json:"head_sha"`
		Mergeability string `json:"mergeability"`
	}{changeID, headSHA, "conflicting"})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func validateEffectBinding(ctx context.Context, tx *sql.Tx, interruptID, reason, runID string, schema int64, raw, digest string) (string, error) {
	var binding map[string]json.RawMessage
	if schema != 1 || json.Unmarshal([]byte(raw), &binding) != nil {
		return "", fmt.Errorf("%w: corrupt effect binding", ErrInterruptRejected)
	}
	canonical, err := canonicalJSON(binding)
	if err != nil || string(canonical) != raw {
		return "", fmt.Errorf("%w: non-canonical effect binding", ErrInterruptRejected)
	}
	sum := sha256.Sum256(canonical)
	if digest != hex.EncodeToString(sum[:]) {
		return "", fmt.Errorf("%w: effect binding digest mismatch", ErrInterruptRejected)
	}
	var arm string
	if json.Unmarshal(binding["arm"], &arm) != nil {
		return "", fmt.Errorf("%w: corrupt effect binding", ErrInterruptRejected)
	}
	if rawRun, ok := binding["run_id"]; ok {
		var boundRun string
		if json.Unmarshal(rawRun, &boundRun) != nil || boundRun != runID {
			return "", fmt.Errorf("%w: corrupt effect binding", ErrInterruptRejected)
		}
	} else if arm != "code_review" && arm != "merge_conflict" {
		return "", fmt.Errorf("%w: corrupt effect binding", ErrInterruptRejected)
	}
	fields := map[string][]string{
		"design_approval":             {"arm", "run_id", "task_spec_snapshot_id"},
		"guardrail_violation":         {"arm", "run_id", "head_sha", "rule_id", "matched_paths_digest"},
		"code_review":                 {"arm", "change_id", "head_sha", "review_policy_snapshot_digest"},
		"agent_blocked":               {"arm", "run_id", "attempt_no", "generation", "report_id"},
		"merge_conflict":              {"arm", "change_id", "head_sha", "conflict_digest"},
		"startup_stall":               {"arm", "run_id", "attempt_no", "generation"},
		"failure_review_attempt":      {"arm", "run_id", "attempt_no", "generation", "retry_kind", "change_id", "head_sha", "terminal_attempt_no", "terminal_generation"},
		"report_quota_failure_review": {"arm", "run_id", "daily_bucket_start_ms", "daily_bucket_end_ms", "security_event_id"},
	}
	expected, ok := fields[arm]
	if !ok || len(binding) != len(expected) {
		return "", fmt.Errorf("%w: invalid effect binding arm", ErrInterruptRejected)
	}
	for _, field := range expected {
		if _, ok := binding[field]; !ok {
			return "", fmt.Errorf("%w: incomplete effect binding", ErrInterruptRejected)
		}
	}
	if (arm == "failure_review_attempt" || arm == "report_quota_failure_review") != (reason == string(InterruptFailureReview)) || (arm != "failure_review_attempt" && arm != "report_quota_failure_review" && arm != reason) {
		return "", fmt.Errorf("%w: binding reason mismatch", ErrInterruptRejected)
	}
	text := func(name string) bool { var v string; return json.Unmarshal(binding[name], &v) == nil && v != "" }
	integer := func(name string) bool {
		var v int64
		return json.Unmarshal(binding[name], &v) == nil && v > 0
	}
	if arm != "code_review" && arm != "merge_conflict" && !text("run_id") {
		return "", fmt.Errorf("%w: invalid effect binding field", ErrInterruptRejected)
	}
	switch arm {
	case "design_approval":
		if !text("task_spec_snapshot_id") {
			return "", fmt.Errorf("%w: incomplete effect binding", ErrInterruptRejected)
		}
	case "guardrail_violation":
		if !text("head_sha") || !text("rule_id") || !text("matched_paths_digest") {
			return "", fmt.Errorf("%w: incomplete effect binding", ErrInterruptRejected)
		}
	case "code_review":
		if !text("change_id") || !text("head_sha") || !text("review_policy_snapshot_digest") {
			return "", fmt.Errorf("%w: incomplete effect binding", ErrInterruptRejected)
		}
	case "agent_blocked":
		if !integer("attempt_no") || !integer("generation") || !text("report_id") {
			return "", fmt.Errorf("%w: incomplete effect binding", ErrInterruptRejected)
		}
	case "merge_conflict":
		if !text("change_id") || !text("head_sha") || !text("conflict_digest") {
			return "", fmt.Errorf("%w: incomplete effect binding", ErrInterruptRejected)
		}
	case "startup_stall":
		if !integer("attempt_no") || !integer("generation") {
			return "", fmt.Errorf("%w: incomplete effect binding", ErrInterruptRejected)
		}
	case "report_quota_failure_review":
		if !integer("daily_bucket_start_ms") || !integer("daily_bucket_end_ms") || !text("security_event_id") {
			return "", fmt.Errorf("%w: incomplete effect binding", ErrInterruptRejected)
		}
	case "failure_review_attempt":
		var retry string
		if !integer("attempt_no") || !integer("generation") || json.Unmarshal(binding["retry_kind"], &retry) != nil {
			return "", fmt.Errorf("%w: incomplete effect binding", ErrInterruptRejected)
		}
		var change, head, terminalAttempt, terminalGeneration any
		if json.Unmarshal(binding["change_id"], &change) != nil || json.Unmarshal(binding["head_sha"], &head) != nil || json.Unmarshal(binding["terminal_attempt_no"], &terminalAttempt) != nil || json.Unmarshal(binding["terminal_generation"], &terminalGeneration) != nil {
			return "", fmt.Errorf("%w: incomplete effect binding", ErrInterruptRejected)
		}
		if retry == "gate_recheck" {
			if _, ok := change.(string); !ok {
				return "", fmt.Errorf("%w: invalid effect binding", ErrInterruptRejected)
			}
			if _, ok := head.(string); !ok || terminalAttempt != nil || terminalGeneration != nil {
				return "", fmt.Errorf("%w: invalid effect binding", ErrInterruptRejected)
			}
		} else if retry == "new_attempt" {
			var attempt, generation, terminalAttemptValue, terminalGenerationValue int64
			if change != nil || head != nil || json.Unmarshal(binding["attempt_no"], &attempt) != nil || json.Unmarshal(binding["generation"], &generation) != nil || json.Unmarshal(binding["terminal_attempt_no"], &terminalAttemptValue) != nil || json.Unmarshal(binding["terminal_generation"], &terminalGenerationValue) != nil || attempt != terminalAttemptValue || generation != terminalGenerationValue {
				return "", fmt.Errorf("%w: invalid effect binding", ErrInterruptRejected)
			}
		} else {
			return "", fmt.Errorf("%w: invalid effect binding", ErrInterruptRejected)
		}
	}
	if err := validateEffectBindingReferences(ctx, tx, arm, interruptID, runID, binding); err != nil {
		return "", err
	}
	return arm, nil
}

func validateEffectBindingReferences(ctx context.Context, tx *sql.Tx, arm, interruptID, runID string, binding map[string]json.RawMessage) error {
	textValue := func(name string) string {
		var value string
		_ = json.Unmarshal(binding[name], &value)
		return value
	}
	integerValue := func(name string) int64 {
		var value int64
		_ = json.Unmarshal(binding[name], &value)
		return value
	}
	var exists int
	var err error
	switch arm {
	case "design_approval":
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_spec_snapshots WHERE id=? AND run_id=?)`, textValue("task_spec_snapshot_id"), runID).Scan(&exists)
	case "code_review":
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM interrupts i JOIN runs r ON r.id=i.run_id
			JOIN calibration_entries c ON c.id=i.calibration_id
			JOIN gate_evaluations e ON e.id=c.gate_evaluation_id
			JOIN gate_input_snapshots s ON s.id=e.snapshot_id
			WHERE i.id=? AND r.id=? AND r.change_id=? AND r.change_head_sha=?
				AND s.head_sha=? AND s.effective_policy_hash=?)`, interruptID, runID, textValue("change_id"), textValue("head_sha"), textValue("head_sha"), textValue("review_policy_snapshot_digest")).Scan(&exists)
	case "merge_conflict":
		changeID, headSHA := textValue("change_id"), textValue("head_sha")
		if textValue("conflict_digest") != MergeConflictDigest(changeID, headSHA) {
			return fmt.Errorf("%w: invalid merge conflict digest", ErrInterruptRejected)
		}
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM interrupts i JOIN runs r ON r.id=i.run_id
			JOIN calibration_entries c ON c.id=i.calibration_id
			JOIN gate_evaluations e ON e.id=c.gate_evaluation_id
			JOIN gate_input_snapshots s ON s.id=e.snapshot_id
			WHERE i.id=? AND r.id=? AND r.change_id=? AND r.change_head_sha=?
				AND s.head_sha=?
                AND json_extract(s.canonical_json,'$.change.mergeability')='conflicting'
                AND json_extract(e.verdict_json,'$.mergeability')='conflicting')`, interruptID, runID, changeID, headSHA, headSHA).Scan(&exists)
	case "guardrail_violation":
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM interrupts i
			JOIN calibration_entries c ON c.id=i.calibration_id
			JOIN gate_evaluations e ON e.id=c.gate_evaluation_id
			JOIN gate_input_snapshots s ON s.id=e.snapshot_id
			WHERE i.id=? AND e.run_id=? AND s.head_sha=?
				AND json_extract(e.verdict_json,'$.rule_id')=?
				AND json_extract(e.verdict_json,'$.matched_paths_digest')=?)`, interruptID, runID,
			textValue("head_sha"), textValue("rule_id"), textValue("matched_paths_digest")).Scan(&exists)
	case "agent_blocked", "startup_stall", "failure_review_attempt":
		query := `SELECT EXISTS(SELECT 1 FROM attempts a JOIN interrupts i ON i.id=? WHERE a.run_id=? AND a.run_id=i.run_id AND a.attempt_no=? AND a.generation=?)`
		args := []any{interruptID, runID, integerValue("attempt_no"), integerValue("generation")}
		if arm == "failure_review_attempt" {
			var retry string
			_ = json.Unmarshal(binding["retry_kind"], &retry)
			if retry == "new_attempt" {
				query = `SELECT EXISTS(SELECT 1 FROM attempts a JOIN interrupts i ON i.id=? WHERE a.run_id=? AND a.run_id=i.run_id AND a.attempt_no=? AND a.generation=? AND phase='finished' AND ((result_exit_code IS NOT NULL AND result_exit_code<>0) OR result_signal IS NOT NULL))`
			} else if retry == "gate_recheck" {
				query = `SELECT EXISTS(SELECT 1 FROM runs r JOIN interrupts i ON i.id=? WHERE r.id=? AND r.id=i.run_id AND r.change_id=? AND r.change_head_sha=?)`
				args = []any{interruptID, runID, textValue("change_id"), textValue("head_sha")}
			}
		}
		err = tx.QueryRowContext(ctx, query, args...).Scan(&exists)
		if err == nil && exists == 1 && arm == "agent_blocked" {
			var report string
			_ = json.Unmarshal(binding["report_id"], &report)
			if report != "" {
				err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM report_receipts WHERE id=? AND run_id=? AND attempt_no=? AND report_kind='blocker')`, report, runID, integerValue("attempt_no")).Scan(&exists)
			}
		}
	case "report_quota_failure_review":
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM report_quota_exhaustions q JOIN events e ON e.id=q.security_event_id WHERE q.run_id=? AND q.daily_bucket_start_ms=? AND q.daily_bucket_end_ms=? AND q.security_event_id=? AND e.source='system')`, runID, integerValue("daily_bucket_start_ms"), integerValue("daily_bucket_end_ms"), textValue("security_event_id")).Scan(&exists)
	default:
		return nil
	}
	if err != nil || exists != 1 {
		return fmt.Errorf("%w: invalid effect binding reference", ErrInterruptRejected)
	}
	return nil
}
