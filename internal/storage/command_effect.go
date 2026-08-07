package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/miaoxiaoyong/sift/internal/command"
)

func (d *DB) applyCommandEffectTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1, c command.CompiledCommandV1, row *commandInterruptRow, nowMS int64, eventID, nextNonce string) error {
	if row == nil {
		return ErrRejectedStale
	}
	switch c.Action {
	case command.ActionReject:
		return d.commandRejectTx(ctx, tx, env, c, row, nowMS)
	case command.ActionHold:
		return d.commandHoldTx(ctx, tx, env, c, nowMS, nextNonce)
	case command.ActionAsk:
		return d.commandAskTx(ctx, tx, env, c, row, nowMS, eventID)
	case command.ActionApprove:
		return d.commandApproveTx(ctx, tx, env, c, row, nowMS, eventID)
	case command.ActionRetry:
		return d.commandRetryTx(ctx, tx, env, c, row, nowMS, eventID)
	}
	return ErrCommandEffectNotWired
}

func (d *DB) commandRejectTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1, c command.CompiledCommandV1, row *commandInterruptRow, nowMS int64) error {
	// reject: close/responded; Run -> failed(human_reject). The bound attempt is
	// marked attempt_resolution=reject (write-once); for startup_stall the
	// isolation is retained until the execution body is proven absent, which is
	// owned by the probe/race path.
	if err := d.transition(ctx, tx, row.RunID, row.RunVersion, DomainCommand{To: RunFailed, Source: SourceOperator, Actor: actorName(env), FailureReason: "human_reject", OccurredAtMS: nowMS}); err != nil {
		return err
	}
	if row.AttemptNo > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE attempts SET attempt_resolution='reject',resolution_at_ms=?,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND attempt_resolution IS NULL`, nowMS, nowMS, row.RunID, row.AttemptNo); err != nil {
			return err
		}
	}
	if _, err := recordHumanDecisionTx(ctx, tx, RecordHumanDecisionCmd{
		Action: DecisionReject, CommandEventID: env.EventKey, InterruptID: c.InterruptID,
		SemanticMaterial: c.RejectReason, NowMS: nowMS,
	}); err != nil {
		return err
	}
	return closeInterruptTx(ctx, tx, c.InterruptID, c.ExpectedInterruptVersion, c.Nonce, "responded", nowMS)
}

func (d *DB) commandHoldTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1, c command.CompiledCommandV1, nowMS int64, nextNonce string) error {
	expires := nowMS + c.HoldDurationMS
	res, err := tx.ExecContext(ctx, `UPDATE interrupts SET nonce=?,version=version+1,dispatch_state='held',held_reason='manual',delivery='held',expires_at_ms=?,next_dispatch_at_ms=NULL,nonce_issued_at_ms=?,updated_at_ms=? WHERE id=? AND status='open' AND version=? AND nonce=?`,
		nextNonce, expires, nowMS, nowMS, c.InterruptID, c.ExpectedInterruptVersion, c.Nonce)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrRejectedStale
	}
	if err := excludeStaleBatchMembersTx(ctx, tx, c.InterruptID, nowMS); err != nil {
		return err
	}
	_, err = recordHumanDecisionTx(ctx, tx, RecordHumanDecisionCmd{
		Action: DecisionHold, CommandEventID: env.EventKey, InterruptID: c.InterruptID, NowMS: nowMS,
	})
	return err
}

func (d *DB) commandAskTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1, c command.CompiledCommandV1, row *commandInterruptRow, nowMS int64, eventID string) error {
	if row == nil {
		return ErrRejectedStale
	}
	binding, err := loadInterruptEffectBindingTx(ctx, tx, c.InterruptID)
	if err != nil {
		return err
	}
	switch InterruptReason(row.Reason) {
	case InterruptAgentBlocked:
		// agent_blocked|ask full contract (command.md §4): insert Task Spec
		// snapshot sourced by the command event, close/responded, terminalize
		// the bound blocked attempt and create the next attempt/claim/launch.
		return d.commandAgentBlockedAskTx(ctx, tx, env, c, row, binding, nowMS, eventID)
	}
	// ask is exposed only by agent_blocked (compile rejects every other
	// reason's ask as rejected_option). A non-canonical ask that still reaches
	// here is not wired: stay honest rather than inventing a close-only effect.
	return ErrCommandEffectNotWired
}

// commandAgentBlockedAskTx wires the agent_blocked|ask row (command.md §4 /
// storage.md §5.1, §12.3). It writes HumanDecision(ask) + the unmodified
// SemanticMaterial, inserts an append-only Task Spec snapshot sourced by the
// command event and updates the Run's current pointer without overwriting the
// historical snapshot, closes the Interrupt responded, terminalizes the bound
// blocked attempt and spawns the next pending attempt/claim/launch from the
// clarification snapshot, then queues the Run. The clarification is task-layer
// only; it is never auto-promoted to project/global Context.
func (d *DB) commandAgentBlockedAskTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1, c command.CompiledCommandV1, row *commandInterruptRow, binding commandEffectBinding, nowMS int64, eventID string) error {
	if binding.RunID == "" || binding.AttemptNo < 1 || binding.Generation < 1 {
		return ErrCommandEffectNotWired
	}
	if _, err := recordHumanDecisionTx(ctx, tx, RecordHumanDecisionCmd{
		Action: DecisionAsk, CommandEventID: env.EventKey, InterruptID: c.InterruptID,
		SemanticMaterial: c.AskText, NowMS: nowMS,
	}); err != nil {
		return err
	}
	// Append-only Task Spec snapshot sourced by the command event; update the
	// Run's current pointer without overwriting the historical snapshot.
	snapshotID, err := insertClarificationTaskSpecTx(ctx, tx, row.RunID, eventID, c.AskText, nowMS)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET current_task_spec_id=?,updated_at_ms=? WHERE id=?`, snapshotID, nowMS, row.RunID); err != nil {
		return err
	}
	if err := closeInterruptTx(ctx, tx, c.InterruptID, c.ExpectedInterruptVersion, c.Nonce, "responded", nowMS); err != nil {
		return err
	}
	// Terminalize the bound blocked attempt and spawn the next pending
	// attempt/claim/launch from the clarification snapshot.
	if _, err := d.spawnNextAttemptTx(ctx, tx, row.RunID, binding.AttemptNo, binding.Generation, nowMS, snapshotID); err != nil {
		return err
	}
	return d.transition(ctx, tx, row.RunID, row.RunVersion, DomainCommand{To: RunQueued, Source: SourceOperator, Actor: actorName(env), OccurredAtMS: nowMS})
}

// insertClarificationTaskSpecTx inserts an append-only Task Spec snapshot
// sourced by the command event for an agent_blocked|ask clarification
// (storage.md §5.1). It never overwrites the historical snapshot: prior
// attempts keep the snapshot they started from. The canonical body is the
// task-layer clarification only; project/global Context is never promoted.
// Returns the new snapshot id.
func insertClarificationTaskSpecTx(ctx context.Context, tx *sql.Tx, runID, sourceEventID, clarification string, nowMS int64) (string, error) {
	body, err := canonicalJSON(map[string]any{"schema_version": 1, "clarification": clarification})
	if err != nil {
		return "", err
	}
	var maxVersion int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM task_spec_snapshots WHERE run_id=?`, runID).Scan(&maxVersion); err != nil {
		return "", err
	}
	id := newID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_spec_snapshots (id,run_id,version,schema_version,canonical_json,content_digest,source_event_id,created_at_ms) VALUES (?,?,?,?,?,?,?,?)`,
		id, runID, maxVersion+1, 1, string(body), sha256Hex(body), sourceEventID, nowMS); err != nil {
		return "", err
	}
	return id, nil
}

func (d *DB) commandApproveTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1, c command.CompiledCommandV1, row *commandInterruptRow, nowMS int64, eventID string) error {
	switch InterruptReason(row.Reason) {
	case InterruptDesignApproval:
		// approve: close/responded; waiting_human -> queued. The next pending
		// attempt/claim/launch is enqueued by the launch path on the queued Run.
		if err := d.transition(ctx, tx, row.RunID, row.RunVersion, DomainCommand{To: RunQueued, Source: SourceOperator, Actor: actorName(env), OccurredAtMS: nowMS}); err != nil {
			return err
		}
	case InterruptGuardrailViolation:
		// guardrail approve: consume the one-time exemption from the immutable
		// binding (command_effects row), close/responded, enqueue exactly one
		// Gate re-evaluation of that binding. Run stays waiting_human behind the
		// pending re-evaluation (storage.md §8.1).
		binding, err := loadInterruptEffectBindingTx(ctx, tx, c.InterruptID)
		if err != nil {
			return err
		}
		return d.commandGateReEvalTx(ctx, tx, env, c, row, nowMS, eventID, gateReEvalEffect{
			commandEffectKind: "one_time_exemption", decision: DecisionApprove,
		}, binding)
	case InterruptCodeReview:
		// code_review approve: insert one immutable human-review approval for
		// that binding, close/responded, enqueue exactly one Gate re-evaluation.
		binding, err := loadInterruptEffectBindingTx(ctx, tx, c.InterruptID)
		if err != nil {
			return err
		}
		return d.commandGateReEvalTx(ctx, tx, env, c, row, nowMS, eventID, gateReEvalEffect{
			commandEffectKind: "human_review_approval", decision: DecisionApprove,
		}, binding)
	default:
		// design_approval is the only approve-binding Run transition; every
		// other reason's approve remains honestly unwired until its successor
		// contract lands.
		return ErrCommandEffectNotWired
	}
	if _, err := recordHumanDecisionTx(ctx, tx, RecordHumanDecisionCmd{
		Action: DecisionApprove, CommandEventID: env.EventKey, InterruptID: c.InterruptID, NowMS: nowMS,
	}); err != nil {
		return err
	}
	return closeInterruptTx(ctx, tx, c.InterruptID, c.ExpectedInterruptVersion, c.Nonce, "responded", nowMS)
}

// gateReEvalEffect describes the optional immutable command_effects fact and
// the HumanDecision action for a Gate re-evaluation command. guardrail and
// code_review approve insert an effect fact; failure_review gate_recheck and
// merge_conflict retry do not.
type gateReEvalEffect struct {
	commandEffectKind string // "" | "one_time_exemption" | "human_review_approval"
	decision          HumanDecisionAction
}

// commandRetryTx wires the non-startup retry effects (command.md §4). It never
// guesses a Change or attempt outside the immutable binding: the binding arm
// selects between a Gate re-evaluation successor (failure_review gate_recheck,
// merge_conflict) and an attempt-terminalization + next-attempt successor
// (failure_review new_attempt, agent_blocked).
func (d *DB) commandRetryTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1, c command.CompiledCommandV1, row *commandInterruptRow, nowMS int64, eventID string) error {
	binding, err := loadInterruptEffectBindingTx(ctx, tx, c.InterruptID)
	if err != nil {
		return err
	}
	switch InterruptReason(row.Reason) {
	case InterruptFailureReview:
		if binding.Arm != "failure_review_attempt" {
			// report_quota_failure_review exposes only reject|hold; a retry never
			// reaches this port. Keep it honest rather than guessing a successor.
			return ErrCommandEffectNotWired
		}
		if binding.RetryKind == string(FailureReviewGateRecheck) {
			return d.commandGateReEvalTx(ctx, tx, env, c, row, nowMS, eventID, gateReEvalEffect{decision: DecisionRetry}, binding)
		}
		return d.commandNewAttemptRetryTx(ctx, tx, env, c, row, binding, nowMS)
	case InterruptAgentBlocked:
		// agent_blocked retry terminalizes the bound blocked attempt and creates
		// the next attempt/claim/launch without a Task Spec change (the ask
		// contract's snapshot stays out of scope).
		return d.commandNewAttemptRetryTx(ctx, tx, env, c, row, binding, nowMS)
	case InterruptMergeConflict:
		return d.commandGateReEvalTx(ctx, tx, env, c, row, nowMS, eventID, gateReEvalEffect{decision: DecisionRetry}, binding)
	}
	return ErrCommandEffectNotWired
}

// commandGateReEvalTx is the shared core for guardrail/code_review approve and
// failure_review gate_recheck / merge_conflict retry. It optionally inserts one
// immutable command_effects fact from the frozen binding, records the human
// decision, closes the Interrupt responded and enqueues exactly one Gate
// re-evaluation of the frozen head. The Run stays waiting_human behind the
// pending re-evaluation (storage.md §8.1).
func (d *DB) commandGateReEvalTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1, c command.CompiledCommandV1, row *commandInterruptRow, nowMS int64, eventID string, eff gateReEvalEffect, binding commandEffectBinding) error {
	if eff.commandEffectKind != "" {
		if err := insertCommandEffectTx(ctx, tx, c.InterruptID, eventID, row.RunID, eff.commandEffectKind, binding, nowMS); err != nil {
			return err
		}
	}
	if _, err := recordHumanDecisionTx(ctx, tx, RecordHumanDecisionCmd{
		Action: eff.decision, CommandEventID: env.EventKey, InterruptID: c.InterruptID, NowMS: nowMS,
	}); err != nil {
		return err
	}
	if err := closeInterruptTx(ctx, tx, c.InterruptID, c.ExpectedInterruptVersion, c.Nonce, "responded", nowMS); err != nil {
		return err
	}
	snap, err := loadInterruptGateSnapshotTx(ctx, tx, c.InterruptID)
	if err != nil {
		return err
	}
	return enqueueGateReEvaluationTx(ctx, tx, c.InterruptID, eventID, row, snap, binding, nowMS)
}

// commandNewAttemptRetryTx terminalizes the bound attempt and spawns the next
// pending attempt/claim/launch, then closes the Interrupt responded and queues
// the Run. It mirrors the retry-after-absence spawn without the absence proof:
// the human retry decision is recorded by the Ledger, not by attempt_resolution
// (storage.md §1.10 leaves that enum to reject|retry_after_absence).
func (d *DB) commandNewAttemptRetryTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1, c command.CompiledCommandV1, row *commandInterruptRow, binding commandEffectBinding, nowMS int64) error {
	if binding.RunID == "" || binding.AttemptNo < 1 || binding.Generation < 1 {
		return ErrCommandEffectNotWired
	}
	if _, err := recordHumanDecisionTx(ctx, tx, RecordHumanDecisionCmd{
		Action: DecisionRetry, CommandEventID: env.EventKey, InterruptID: c.InterruptID, NowMS: nowMS,
	}); err != nil {
		return err
	}
	if err := closeInterruptTx(ctx, tx, c.InterruptID, c.ExpectedInterruptVersion, c.Nonce, "responded", nowMS); err != nil {
		return err
	}
	if _, err := d.spawnNextAttemptTx(ctx, tx, row.RunID, binding.AttemptNo, binding.Generation, nowMS, ""); err != nil {
		return err
	}
	return d.transition(ctx, tx, row.RunID, row.RunVersion, DomainCommand{To: RunQueued, Source: SourceOperator, Actor: actorName(env), OccurredAtMS: nowMS})
}

// spawnNextAttemptTx moves the bound attempt out of the live set (so the
// single-live-phase index admits its successor) and creates the next pending
// attempt, its claim and its launch operation from the bound attempt's frozen
// assignment. It is the shared terminalize+spawn helper for human retry and
// agent_blocked ask; the caller owns the Run transition and the human-decision
// Ledger entry. taskSpecSnapshotID is empty for retry (the new attempt reuses
// the bound attempt's frozen snapshot) or the clarification snapshot id for ask
// (the new attempt starts from the command-event-sourced Task Spec).
func (d *DB) spawnNextAttemptTx(ctx context.Context, tx *sql.Tx, runID string, boundAttemptNo, boundGeneration int, nowMS int64, taskSpecSnapshotID string) (int, error) {
	if boundAttemptNo < 1 || boundGeneration < 1 {
		return 0, ErrRejectedStale
	}
	if _, err := tx.ExecContext(ctx, `UPDATE attempts SET phase='orphaned',updated_at_ms=? WHERE run_id=? AND attempt_no=? AND phase IN ('pending','starting','spawning','running')`, nowMS, runID, boundAttemptNo); err != nil {
		return 0, err
	}
	newNo := boundAttemptNo + 1
	if taskSpecSnapshotID == "" {
		// Retry: the new attempt reuses the bound attempt's frozen snapshot
		// (no Task Spec change).
		if _, err := tx.ExecContext(ctx, `INSERT INTO attempts (run_id,attempt_no,phase,generation,backend,agent_id,task_spec_snapshot_id,worktree_path,branch_name,base_ref,base_sha,isolation_state,created_at_ms,updated_at_ms) SELECT run_id,?,'pending',1,backend,agent_id,task_spec_snapshot_id,worktree_path,branch_name,base_ref,base_sha,'none',?,? FROM attempts WHERE run_id=? AND attempt_no=?`, newNo, nowMS, nowMS, runID, boundAttemptNo); err != nil {
			return 0, err
		}
	} else {
		// Ask: the new attempt starts from the clarification snapshot.
		if _, err := tx.ExecContext(ctx, `INSERT INTO attempts (run_id,attempt_no,phase,generation,backend,agent_id,task_spec_snapshot_id,worktree_path,branch_name,base_ref,base_sha,isolation_state,created_at_ms,updated_at_ms) SELECT run_id,?,'pending',1,backend,agent_id,?,worktree_path,branch_name,base_ref,base_sha,'none',?,? FROM attempts WHERE run_id=? AND attempt_no=?`, newNo, taskSpecSnapshotID, nowMS, nowMS, runID, boundAttemptNo); err != nil {
			return 0, err
		}
	}
	key := LaunchOperationKey(runID, newNo, 1)
	if _, err := tx.ExecContext(ctx, `INSERT INTO attempt_claims (run_id,attempt_no,generation,launch_operation_key,created_at_ms,updated_at_ms) VALUES (?,?,1,?,?,?)`, runID, newNo, key, nowMS, nowMS); err != nil {
		return 0, err
	}
	if err := insertOperation(ctx, tx, Operation{Key: key, Kind: OperationLaunchAgent, Payload: []byte(`{"schema_version":1}`), RunID: runID, AttemptNo: intPtr(newNo)}, runID, "", nowMS); err != nil {
		return 0, err
	}
	return newNo, nil
}

// commandEffectBinding is the parsed immutable effect binding for the current
// Interrupt (storage.md §6.4). Only the fields consumed by Command effects are
// projected; the frozen binding_json/digest are retained for the Gate
// re-evaluation payload.
type commandEffectBinding struct {
	Arm                        string `json:"arm"`
	RunID                      string `json:"run_id"`
	AttemptNo                  int    `json:"attempt_no"`
	Generation                 int    `json:"generation"`
	ChangeID                   string `json:"change_id"`
	HeadSHA                    string `json:"head_sha"`
	RuleID                     string `json:"rule_id"`
	MatchedPathsDigest         string `json:"matched_paths_digest"`
	ReviewPolicySnapshotDigest string `json:"review_policy_snapshot_digest"`
	ConflictDigest             string `json:"conflict_digest"`
	RetryKind                  string `json:"retry_kind"`
	JSON                       []byte
	Digest                     string
}

// loadInterruptEffectBindingTx reads the immutable one-to-one effect binding
// frozen at emission. Command never reconstructs an arm from the current Run,
// Change, attempt or Forge state.
func loadInterruptEffectBindingTx(ctx context.Context, tx *sql.Tx, interruptID string) (commandEffectBinding, error) {
	var b commandEffectBinding
	var bindingJSON string
	err := tx.QueryRowContext(ctx, `SELECT binding_json,binding_digest FROM interrupt_command_effect_bindings WHERE interrupt_id=?`, interruptID).Scan(&bindingJSON, &b.Digest)
	if errors.Is(err, sql.ErrNoRows) {
		return commandEffectBinding{}, ErrCommandEffectNotWired
	}
	if err != nil {
		return commandEffectBinding{}, err
	}
	b.JSON = []byte(bindingJSON)
	if err := json.Unmarshal(b.JSON, &b); err != nil {
		return commandEffectBinding{}, fmt.Errorf("storage: corrupt command effect binding: %w", err)
	}
	return b, nil
}

// interruptGateSnapshot is the frozen previous Gate evaluation identity sourced
// through the Interrupt's calibration. The Gate re-evaluation operation payload
// carries this frozen source identity (storage.md §8.1); Complete alone
// allocates the re-observed successor snapshot.
type interruptGateSnapshot struct {
	SnapshotID, InputHash, GateVersion string
	ChangeID, HeadSHA                  string
}

// loadInterruptGateSnapshotTx resolves the frozen Gate input/evaluation for the
// Interrupt's calibration. It is the sole source of the re-evaluation's
// gate_input_snapshot_id/gate_input_hash/gate_version and exact head.
func loadInterruptGateSnapshotTx(ctx context.Context, tx *sql.Tx, interruptID string) (interruptGateSnapshot, error) {
	var s interruptGateSnapshot
	err := tx.QueryRowContext(ctx, `SELECT s.id,s.gate_input_hash,e.gate_version,json_extract(s.canonical_json,'$.identity.change_id'),s.head_sha
		FROM interrupts i
		JOIN calibration_entries c ON c.id=i.calibration_id
		JOIN gate_evaluations e ON e.id=c.gate_evaluation_id
		JOIN gate_input_snapshots s ON s.id=e.snapshot_id
		WHERE i.id=?`, interruptID).Scan(&s.SnapshotID, &s.InputHash, &s.GateVersion, &s.ChangeID, &s.HeadSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return interruptGateSnapshot{}, ErrCommandEffectNotWired
	}
	if err != nil {
		return interruptGateSnapshot{}, err
	}
	return s, nil
}

// enqueueGateReEvaluationTx inserts exactly one pending gate_re_evaluation
// outbox operation (storage.md §8.1) built only from the frozen source
// Interrupt, its Gate snapshot and the immutable effect binding. The operation
// key is keyed by the source Interrupt and the exact head, so it can never be
// reconstructed from the current Change.
func enqueueGateReEvaluationTx(ctx context.Context, tx *sql.Tx, sourceInterruptID, sourceCommandEventID string, row *commandInterruptRow, snap interruptGateSnapshot, binding commandEffectBinding, nowMS int64) error {
	opKey := GateReEvaluationOperationKey(sourceInterruptID, snap.HeadSHA)
	payload, err := canonicalJSON(map[string]any{
		"source_interrupt_id":     sourceInterruptID,
		"source_command_event_id": sourceCommandEventID,
		"source_run_version":      row.RunVersion,
		"run_id":                  row.RunID,
		"attempt_no":              row.AttemptNo,
		"generation":              row.Generation,
		"change_id":               snap.ChangeID,
		"head_sha":                snap.HeadSHA,
		"gate_input_snapshot_id":  snap.SnapshotID,
		"gate_input_hash":         snap.InputHash,
		"gate_version":            snap.GateVersion,
		"effect_binding_digest":   binding.Digest,
		"operation_key":           opKey,
	})
	if err != nil {
		return err
	}
	return insertOperation(ctx, tx, Operation{
		Key: opKey, Kind: OperationGateReEvaluation, Payload: payload,
		RunID: row.RunID, AttemptNo: intPtr(row.AttemptNo), InterruptID: sourceInterruptID,
	}, row.RunID, sourceCommandEventID, nowMS)
}

// insertCommandEffectTx inserts the immutable one-time exemption or
// human-review approval fact consumed by the next Gate snapshot (storage.md
// §6.4). The binding identity guarantees the run/head/rule/path or
// change/head/policy digest; Command never derives these from mutable state.
func insertCommandEffectTx(ctx context.Context, tx *sql.Tx, interruptID, eventID, runID, kind string, binding commandEffectBinding, nowMS int64) error {
	var changeID, headSHA, ruleID, matchedPathsDigest, reviewPolicySnapshotDigest string
	switch kind {
	case "one_time_exemption":
		headSHA, ruleID, matchedPathsDigest = binding.HeadSHA, binding.RuleID, binding.MatchedPathsDigest
	case "human_review_approval":
		changeID, headSHA, reviewPolicySnapshotDigest = binding.ChangeID, binding.HeadSHA, binding.ReviewPolicySnapshotDigest
	default:
		return fmt.Errorf("storage: invalid command effect kind %q", kind)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO command_effects (id,interrupt_id,event_id,effect_kind,run_id,change_id,head_sha,rule_id,matched_paths_digest,review_policy_snapshot_digest,created_at_ms) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		newID(), interruptID, eventID, kind, runID, nullable(changeID), nullable(headSHA), nullable(ruleID), nullable(matchedPathsDigest), nullable(reviewPolicySnapshotDigest), nowMS)
	return err
}

type GateCommandExemption struct {
	RunID, HeadSHA, RuleID, MatchedPathsDigest string
}

type GateCommandEffects struct {
	Exemptions     []GateCommandExemption
	ReviewApproved bool
}

// GateCommandEffectsForInput returns only immutable effects bound to the exact
// Gate identity being assembled. Historical heads and policy snapshots cannot
// satisfy the next Gate.
func (d *DB) GateCommandEffectsForInput(ctx context.Context, runID, changeID, headSHA, reviewPolicySnapshotDigest string) (GateCommandEffects, error) {
	if runID == "" || changeID == "" || headSHA == "" || reviewPolicySnapshotDigest == "" {
		return GateCommandEffects{}, errors.New("storage: incomplete Gate command-effect identity")
	}
	rows, err := d.db.QueryContext(ctx, `SELECT run_id,head_sha,rule_id,matched_paths_digest FROM command_effects
		WHERE effect_kind='one_time_exemption' AND run_id=? AND head_sha=? ORDER BY rule_id,matched_paths_digest,id`, runID, headSHA)
	if err != nil {
		return GateCommandEffects{}, err
	}
	defer rows.Close()
	var out GateCommandEffects
	for rows.Next() {
		var exemption GateCommandExemption
		if err := rows.Scan(&exemption.RunID, &exemption.HeadSHA, &exemption.RuleID, &exemption.MatchedPathsDigest); err != nil {
			return GateCommandEffects{}, err
		}
		out.Exemptions = append(out.Exemptions, exemption)
	}
	if err := rows.Err(); err != nil {
		return GateCommandEffects{}, err
	}
	var approved int
	err = d.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM command_effects WHERE effect_kind='human_review_approval' AND run_id=? AND change_id=? AND head_sha=? AND review_policy_snapshot_digest=?)`, runID, changeID, headSHA, reviewPolicySnapshotDigest).Scan(&approved)
	if err != nil {
		return GateCommandEffects{}, err
	}
	out.ReviewApproved = approved != 0
	return out, nil
}

// startupStallRetryRequestTx is the startup_stall retry request (§5). It does
// not close the Interrupt, create an attempt, release isolation, write a
// resolution or ack. It CAS-rotates the nonce, writes the initial retry event
// + pending outcome relation and a pending probe (referencing that event), and
// records the retry HumanDecision. Returns nextNonce and initialEventID.
