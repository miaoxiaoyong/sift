package storage

import (
	"bytes"
	"context"
	"errors"

	"database/sql"
	"encoding/json"
	"github.com/miaoxiaoyong/sift/internal/config"
)

type HumanDecisionAction string

const (
	DecisionApprove     HumanDecisionAction = "approve"
	DecisionReject      HumanDecisionAction = "reject"
	DecisionRetry       HumanDecisionAction = "retry"
	DecisionHold        HumanDecisionAction = "hold"
	DecisionAsk         HumanDecisionAction = "ask"
	DecisionManualMerge HumanDecisionAction = "manual_merge"
	DecisionManualClose HumanDecisionAction = "manual_close"
)

// RecordHumanDecisionCmd intentionally has no calibration ID: it is resolved
// exclusively through the immutable interrupt or Forge-fact binding.
type RecordHumanDecisionCmd struct {
	Action                                        HumanDecisionAction
	CommandEventID, InterruptID, ForgeFactEventID string
	SemanticMaterial                              string
	NowMS                                         int64
	Certification                                 config.Certification
}

type HumanDecisionResult struct{ LedgerEntryID, CalibrationID, CertificationVersion string }

// AppendExternalMergeFact records the authoritative Forge merge observation.
// Its fact must not depend on a Gate or Ledger binding: pre-Gate, missing, and
// ambiguous bindings remain auditable facts but cannot settle a calibration.
func (d *DB) AppendExternalMergeFact(ctx context.Context, cmd EventCmd, headSHA string) (string, error) {
	if cmd.Type != "forge_change_merged" || cmd.RunID == "" || cmd.ProjectID == "" || headSHA == "" || !validSource(cmd.Source) || !json.Valid(cmd.PayloadJSON) || cmd.OccurredAtMS <= 0 || cmd.RecordedAtMS < cmd.OccurredAtMS {
		return "", errors.New("storage: invalid external merge fact")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var id string
	if cmd.IdempotencyKey != "" {
		err = tx.QueryRowContext(ctx, `SELECT id FROM events WHERE idempotency_key=?`, cmd.IdempotencyKey).Scan(&id)
		if err == nil {
			return id, tx.Commit()
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	id = newID()
	if _, err = tx.ExecContext(ctx, `INSERT INTO events (id,run_id,project_id,type,source,payload_schema_version,payload_json,idempotency_key,occurred_at_ms,recorded_at_ms) VALUES (?,?,?,? ,?,1,?,?,?,?)`, id, cmd.RunID, cmd.ProjectID, cmd.Type, cmd.Source, string(cmd.PayloadJSON), nullable(cmd.IdempotencyKey), cmd.OccurredAtMS, cmd.RecordedAtMS); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// BindExternalMergeFact attaches a previously observed fact to the exact Gate
// identity that was frozen with a waiting-human Interrupt. Callers must not
// infer this identity from mutable Run or Forge state.
func (d *DB) BindExternalMergeFact(ctx context.Context, forgeFactEventID, gateEvaluationID, calibrationID string, nowMS int64) error {
	if forgeFactEventID == "" || gateEvaluationID == "" || calibrationID == "" || nowMS <= 0 {
		return errors.New("storage: invalid external merge binding")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var factRunID, calibrationRunID string
	var valid bool
	if err := tx.QueryRowContext(ctx, `SELECT run_id FROM events WHERE id=? AND type='forge_change_merged'`, forgeFactEventID).Scan(&factRunID); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT run_id, predicted_decision IN ('allow','block','inconclusive') FROM calibration_entries WHERE id=? AND gate_evaluation_id=?`, calibrationID, gateEvaluationID).Scan(&calibrationRunID, &valid); err != nil {
		return err
	}
	if !valid || factRunID != calibrationRunID {
		return errors.New("storage: external merge binding is not an exact Gate calibration for this run")
	}
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT calibration_id FROM external_decision_bindings WHERE forge_fact_event_id=?`, forgeFactEventID).Scan(&existing)
	if err == nil {
		if existing != calibrationID {
			return errors.New("storage: external decision binding conflict")
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO external_decision_bindings (forge_fact_event_id,calibration_id,created_at_ms) VALUES (?,?,?)`, forgeFactEventID, calibrationID, nowMS); err != nil {
		return err
	}
	return tx.Commit()
}

// WaitingHumanGateBinding returns the one Gate identity that was atomically
// frozen with the currently displayed HITL Interrupt. It is causal state, not
// a heuristic lookup over Gate history.
func (d *DB) WaitingHumanGateBinding(ctx context.Context, runID string) (gateEvaluationID, calibrationID string, err error) {
	if runID == "" {
		return "", "", errors.New("storage: waiting-human gate binding requires run")
	}
	rows, err := d.db.QueryContext(ctx, `SELECT c.gate_evaluation_id,c.id
		FROM runs r
		JOIN interrupts i ON i.run_id=r.id
		JOIN calibration_entries c ON c.id=i.calibration_id
		WHERE r.id=? AND r.status='waiting_human' AND i.status='open'
		ORDER BY i.created_at_ms,i.id`, runID)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	for rows.Next() {
		var evaluationID, candidateCalibrationID string
		if err := rows.Scan(&evaluationID, &candidateCalibrationID); err != nil {
			return "", "", err
		}
		if calibrationID != "" {
			return "", "", errors.New("storage: ambiguous waiting-human gate binding")
		}
		gateEvaluationID, calibrationID = evaluationID, candidateCalibrationID
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	if gateEvaluationID == "" || calibrationID == "" {
		return "", "", errors.New("storage: waiting-human run has no Gate binding")
	}
	return gateEvaluationID, calibrationID, nil
}

func (d *DB) BindExternalDecision(ctx context.Context, forgeFactEventID, calibrationID string, nowMS int64) error {
	if forgeFactEventID == "" || calibrationID == "" || nowMS <= 0 {
		return errors.New("storage: invalid external decision binding")
	}
	var existing string
	err := d.db.QueryRowContext(ctx, `SELECT calibration_id FROM external_decision_bindings WHERE forge_fact_event_id=?`, forgeFactEventID).Scan(&existing)
	if err == nil {
		if existing == calibrationID {
			return nil
		}
		return errors.New("storage: external decision binding conflict")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = d.db.ExecContext(ctx, `INSERT INTO external_decision_bindings (forge_fact_event_id,calibration_id,created_at_ms) VALUES (?,?,?)`, forgeFactEventID, calibrationID, nowMS)
	return err
}

// RecordHumanDecision is the only Ledger settlement port for commands and
// externally observed manual merge/close facts. It never guesses a calibration.
func (d *DB) RecordHumanDecision(ctx context.Context, cmd RecordHumanDecisionCmd) (HumanDecisionResult, error) {
	if err := validateHumanDecision(cmd); err != nil {
		return HumanDecisionResult{}, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return HumanDecisionResult{}, err
	}
	defer tx.Rollback()
	result, err := recordHumanDecisionTx(ctx, tx, cmd)
	if err != nil {
		return HumanDecisionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return HumanDecisionResult{}, err
	}
	return result, nil
}

// validateHumanDecision enforces the closed input contract shared by the
// public port and the in-transaction command path.
func validateHumanDecision(cmd RecordHumanDecisionCmd) error {
	if cmd.NowMS <= 0 || !validHumanAction(cmd.Action) {
		return errors.New("storage: invalid human decision")
	}
	isExternal := cmd.Action == DecisionManualMerge || cmd.Action == DecisionManualClose
	if isExternal != (cmd.ForgeFactEventID != "") || (!isExternal && (cmd.CommandEventID == "" || cmd.InterruptID == "")) {
		return errors.New("storage: invalid human decision identity")
	}
	if cmd.SemanticMaterial != "" && cmd.Action != DecisionReject && cmd.Action != DecisionAsk {
		return errors.New("storage: semantic material is only valid for reject or ask")
	}
	return nil
}

// recordHumanDecisionTx is the single in-transaction Ledger settlement core.
// ApplyCommandEvent calls it inside its own transaction so command, Run
// transition, outbox and Ledger remain all-or-nothing; there is no second
// Ledger path. The caller owns the transaction (begin/commit/rollback).
func recordHumanDecisionTx(ctx context.Context, tx *sql.Tx, cmd RecordHumanDecisionCmd) (HumanDecisionResult, error) {
	isExternal := cmd.Action == DecisionManualMerge || cmd.Action == DecisionManualClose
	idempotency := cmd.CommandEventID
	if isExternal {
		idempotency = cmd.ForgeFactEventID
	}
	var existing HumanDecisionResult
	err := tx.QueryRowContext(ctx, `SELECT r.ledger_entry_id,COALESCE(r.calibration_id,'') FROM human_decision_receipts r WHERE r.idempotency_id=?`, idempotency).Scan(&existing.LedgerEntryID, &existing.CalibrationID)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return HumanDecisionResult{}, err
	}

	var calibrationID, shadow, runID string
	if isExternal {
		err = tx.QueryRowContext(ctx, `SELECT b.calibration_id,c.predicted_decision,c.run_id FROM external_decision_bindings b JOIN calibration_entries c ON c.id=b.calibration_id WHERE b.forge_fact_event_id=?`, cmd.ForgeFactEventID).Scan(&calibrationID, &shadow, &runID)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT i.calibration_id,c.predicted_decision,c.run_id FROM interrupts i JOIN calibration_entries c ON c.id=i.calibration_id WHERE i.id=?`, cmd.InterruptID).Scan(&calibrationID, &shadow, &runID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		// An unbound external fact is still audit evidence. Its event supplies
		// the Run identity, but cannot settle (or fabricate) a calibration.
		if !isExternal {
			// A command interrupt without a Gate calibration (for example a
			// pre-start design_approval or a startup_stall) still records its
			// human decision: it settles no calibration, but the Ledger entry,
			// semantic material and receipt are written in this transaction.
			var nullableCal sql.NullString
			if calErr := tx.QueryRowContext(ctx, `SELECT COALESCE(calibration_id,''),run_id FROM interrupts WHERE id=?`, cmd.InterruptID).Scan(&nullableCal, &runID); calErr != nil {
				return HumanDecisionResult{}, errors.New("storage: interrupt has no calibration binding")
			}
			calibrationID, shadow = "", "inconclusive"
		} else {
			var nullableRun sql.NullString
			if eventErr := tx.QueryRowContext(ctx, `SELECT run_id FROM events WHERE id=?`, cmd.ForgeFactEventID).Scan(&nullableRun); eventErr != nil || !nullableRun.Valid {
				return HumanDecisionResult{}, errors.New("storage: external fact has no run identity")
			}
			calibrationID, shadow, runID = "", "inconclusive", nullableRun.String
		}
	} else if err != nil {
		return HumanDecisionResult{}, err
	}
	decision := decisionFor(cmd.Action)
	if calibrationID == "" || shadow == "inconclusive" {
		decision = ""
	}
	if decision != "" {
		var prior sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT human_decision FROM calibration_entries WHERE id=?`, calibrationID).Scan(&prior); err != nil {
			return HumanDecisionResult{}, err
		}
		if prior.Valid {
			return HumanDecisionResult{}, errors.New("storage: calibration already settled")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE calibration_entries SET human_decision=?,decision_source=?,decided_at_ms=?,gate_bypassed=? WHERE id=? AND human_decision IS NULL`, decision, decisionSource(cmd.Action), cmd.NowMS, gateBoolInt(cmd.Action == DecisionManualMerge), calibrationID); err != nil {
			return HumanDecisionResult{}, err
		}
	}
	features, err := canonicalHumanDecision(cmd, calibrationID, decision)
	if err != nil {
		return HumanDecisionResult{}, err
	}
	entryID := newID()
	digest := sha256Hex(features)
	if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_entries (id,run_id,interrupt_id,entry_kind,features_schema_version,features_json,features_digest,natural_language,created_at_ms) VALUES (?,?,?,?,1,?,?,?,?)`, entryID, nullable(runID), nullable(cmd.InterruptID), "human_decision", string(features), digest, nullable(cmd.SemanticMaterial), cmd.NowMS); err != nil {
		return HumanDecisionResult{}, err
	}
	if cmd.SemanticMaterial != "" {
		if err := appendSemanticMaterial(ctx, tx, cmd, runID); err != nil {
			return HumanDecisionResult{}, err
		}
	}
	result := HumanDecisionResult{LedgerEntryID: entryID, CalibrationID: calibrationID}
	if decision != "" {
		// The decision at cmd.NowMS belongs to the projection written by this
		// transaction; use the next millisecond as the half-open window's as_of.
		result.CertificationVersion, err = recomputeCertification(ctx, tx, runID, cmd.Certification, cmd.NowMS+1)
		if err != nil {
			return HumanDecisionResult{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO human_decision_receipts (idempotency_id,ledger_entry_id,calibration_id) VALUES (?,?,?)`, idempotency, entryID, nullable(calibrationID)); err != nil {
		return HumanDecisionResult{}, err
	}
	return result, nil
}

func validHumanAction(a HumanDecisionAction) bool {
	switch a {
	case DecisionApprove, DecisionReject, DecisionRetry, DecisionHold, DecisionAsk, DecisionManualMerge, DecisionManualClose:
		return true
	}
	return false
}
func decisionFor(a HumanDecisionAction) string {
	switch a {
	case DecisionApprove, DecisionManualMerge:
		return "allow"
	case DecisionReject, DecisionManualClose:
		return "block"
	}
	return ""
}
func decisionSource(a HumanDecisionAction) string {
	if a == DecisionManualMerge {
		return "manual_merge"
	}
	if a == DecisionManualClose {
		return "manual_close"
	}
	return "command"
}

func appendSemanticMaterial(ctx context.Context, tx *sql.Tx, cmd RecordHumanDecisionCmd, runID string) error {
	b, err := canonicalJSON(map[string]any{"schema_version": 1, "command_event_id": cmd.CommandEventID, "interrupt_id": cmd.InterruptID, "material_kind": map[bool]string{true: "reject_reason", false: "ask_text"}[cmd.Action == DecisionReject], "text": cmd.SemanticMaterial})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO ledger_entries (id,run_id,interrupt_id,entry_kind,features_schema_version,features_json,features_digest,natural_language,created_at_ms) VALUES (?,?,?,?,1,?,?,?,?)`, newID(), nullable(runID), nullable(cmd.InterruptID), "semantic_material", string(b), sha256Hex(b), cmd.SemanticMaterial, cmd.NowMS)
	return err
}

func canonicalHumanDecision(cmd RecordHumanDecisionCmd, calibrationID, decision string) ([]byte, error) {
	return canonicalJSON(map[string]any{"schema_version": 1, "action": cmd.Action, "calibration_decision": nullable(decision), "calibration_id": nullable(calibrationID), "interrupt_id": nullable(cmd.InterruptID), "command_event_id": nullable(cmd.CommandEventID), "decision_source": decisionSource(cmd.Action), "gate_bypassed": cmd.Action == DecisionManualMerge, "response_interval_ms": nil})
}
func canonicalJSON(v any) ([]byte, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	var x any
	if e = json.Unmarshal(b, &x); e != nil {
		return nil, e
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if e = encoder.Encode(x); e != nil {
		return nil, e
	}
	return bytes.TrimSuffix(out.Bytes(), []byte{'\n'}), nil
}

func recomputeCertification(ctx context.Context, tx *sql.Tx, runID string, rules config.Certification, asOf int64) (string, error) {
	if rules.Window <= 0 {
		// Certification tracking is optional: when the Run's config defines no
		// certification window, the human decision still settles the calibration
		// and Ledger entry, but no certification projection is recomputed.
		return "", nil
	}
	var kind string
	if err := tx.QueryRowContext(ctx, `SELECT kind FROM runs WHERE id=?`, runID).Scan(&kind); err != nil {
		return "", err
	}
	start := asOf - rules.Window.Milliseconds()
	rows, err := tx.QueryContext(ctx, `SELECT c.id,c.predicted_decision,c.human_decision,c.decided_at_ms FROM calibration_entries c JOIN runs r ON r.id=c.run_id WHERE r.kind=? AND c.human_decision IS NOT NULL AND c.predicted_decision IN ('allow','block') AND c.decided_at_ms>=? AND c.decided_at_ms<? ORDER BY c.id`, kind, start, asOf)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	type sample struct {
		ID, P, H string
		At       int64
	}
	var ss []sample
	total, negative, leaks, falseBlocks := 0, 0, 0, 0
	for rows.Next() {
		var s sample
		if err := rows.Scan(&s.ID, &s.P, &s.H, &s.At); err != nil {
			return "", err
		}
		ss = append(ss, s)
		total++
		if s.H == "block" {
			negative++
		}
		if s.P == "allow" && s.H == "block" {
			leaks++
		}
		if s.P == "block" && s.H == "allow" {
			falseBlocks++
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	rv, err := config.CertificationRulesVersion(rules)
	if err != nil {
		return "", err
	}
	evidence, err := canonicalJSON(map[string]any{"samples": ss, "total_samples": total, "negative_samples": negative, "leak_count": leaks, "false_block_count": falseBlocks, "window_start_ms": start, "window_end_ms": asOf, "certification_rules_version": rv})
	if err != nil {
		return "", err
	}
	ed := sha256Hex(evidence)
	version := sha256Hex([]byte(kind + "\x00" + rv + "\x00" + ed))
	certified := negative > 0 && total >= rules.TotalSamplesMin && negative >= rules.NegativeSamplesMin && float64(leaks)/float64(negative) <= rules.LeakRateMax && float64(falseBlocks)/float64(total) <= rules.FalseBlockRateMax
	_, err = tx.ExecContext(ctx, `INSERT INTO certifications (task_kind,certification_version,total_samples,negative_samples,leak_count,false_block_count,certified,evidence_digest,updated_at_ms,certification_rules_version,window_start_ms,window_end_ms) VALUES (?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(task_kind,certification_version) DO NOTHING`, kind, version, total, negative, leaks, falseBlocks, gateBoolInt(certified), ed, asOf, rv, start, asOf)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO certification_current (task_kind,certification_version,version,updated_at_ms) VALUES (?,?,1,?) ON CONFLICT(task_kind) DO UPDATE SET certification_version=excluded.certification_version,version=certification_current.version+1,updated_at_ms=excluded.updated_at_ms`, kind, version, asOf)
	return version, err
}

// CertificationProjection returns only the permitted category-level view.
