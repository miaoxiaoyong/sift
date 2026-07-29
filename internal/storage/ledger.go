package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/miaoxiaoyong/sift/internal/config"
)

// HumanDecisionAction is the closed set of audited human actions.
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

// AppendExternalMergeFact atomically records a Forge merge fact and, only when
// exactly one prior binary Gate calibration exists for this Run/head, binds it.
// No temporal or "latest evaluation" heuristic is permitted.
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
	rows, err := tx.QueryContext(ctx, `SELECT c.id FROM calibration_entries c JOIN gate_evaluations e ON e.id=c.gate_evaluation_id JOIN gate_input_snapshots s ON s.id=e.snapshot_id WHERE c.run_id=? AND s.head_sha=? AND c.predicted_decision IN ('allow','block') ORDER BY c.id`, cmd.RunID, headSHA)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var calibrationID string
		if err := rows.Scan(&calibrationID); err != nil {
			return "", err
		}
		ids = append(ids, calibrationID)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(ids) == 1 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO external_decision_bindings (forge_fact_event_id,calibration_id,created_at_ms) VALUES (?,?,?)`, id, ids[0], cmd.RecordedAtMS); err != nil {
			return "", err
		}
	}
	return id, tx.Commit()
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
	if cmd.NowMS <= 0 || !validHumanAction(cmd.Action) {
		return HumanDecisionResult{}, errors.New("storage: invalid human decision")
	}
	isExternal := cmd.Action == DecisionManualMerge || cmd.Action == DecisionManualClose
	if isExternal != (cmd.ForgeFactEventID != "") || (!isExternal && (cmd.CommandEventID == "" || cmd.InterruptID == "")) {
		return HumanDecisionResult{}, errors.New("storage: invalid human decision identity")
	}
	if cmd.SemanticMaterial != "" && cmd.Action != DecisionReject && cmd.Action != DecisionAsk {
		return HumanDecisionResult{}, errors.New("storage: semantic material is only valid for reject or ask")
	}
	idempotency := cmd.CommandEventID
	if isExternal {
		idempotency = cmd.ForgeFactEventID
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return HumanDecisionResult{}, err
	}
	defer tx.Rollback()
	var existing HumanDecisionResult
	err = tx.QueryRowContext(ctx, `SELECT r.ledger_entry_id,COALESCE(r.calibration_id,'') FROM human_decision_receipts r WHERE r.idempotency_id=?`, idempotency).Scan(&existing.LedgerEntryID, &existing.CalibrationID)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return HumanDecisionResult{}, err
		}
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
			return HumanDecisionResult{}, errors.New("storage: interrupt has no calibration binding")
		}
		var nullableRun sql.NullString
		if eventErr := tx.QueryRowContext(ctx, `SELECT run_id FROM events WHERE id=?`, cmd.ForgeFactEventID).Scan(&nullableRun); eventErr != nil || !nullableRun.Valid {
			return HumanDecisionResult{}, errors.New("storage: external fact has no run identity")
		}
		calibrationID, shadow, runID = "", "inconclusive", nullableRun.String
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
	if err := tx.Commit(); err != nil {
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
	return json.Marshal(x)
}

func recomputeCertification(ctx context.Context, tx *sql.Tx, runID string, rules config.Certification, asOf int64) (string, error) {
	if rules.Window <= 0 {
		return "", errors.New("storage: certification window is required")
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
type CertificationProjection struct {
	TaskKind, CertificationVersion                            string
	Certified                                                 bool
	TotalSamples, NegativeSamples, LeakCount, FalseBlockCount int
	WindowStartMS, WindowEndMS                                int64
}

func (d *DB) Certification(ctx context.Context, kind string) (CertificationProjection, error) {
	var p CertificationProjection
	var certified int
	err := d.db.QueryRowContext(ctx, `SELECT c.task_kind,c.certification_version,c.certified,c.total_samples,c.negative_samples,c.leak_count,c.false_block_count,c.window_start_ms,c.window_end_ms FROM certification_current x JOIN certifications c ON c.task_kind=x.task_kind AND c.certification_version=x.certification_version WHERE x.task_kind=?`, kind).Scan(&p.TaskKind, &p.CertificationVersion, &certified, &p.TotalSamples, &p.NegativeSamples, &p.LeakCount, &p.FalseBlockCount, &p.WindowStartMS, &p.WindowEndMS)
	p.Certified = certified != 0
	return p, err
}
