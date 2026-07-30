package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// M1 skeleton intake write port (storage.md §11: the Run-creation half of
// PersistIntakeDecision). It creates the forge Run, appends the
// intake.trigger_observed event and the forge receipt in one transaction. The
// full intake projection, CAS and clarification-generation protocol land in M2
// (PersistIntakeDecision); this M1 port carries exactly the spine facts the
// skeleton chain (WBS M1 §1.6) drives and no more.

// CreateForgeRunCmd carries the facts needed to create a forge Run from an
// accepted intake.
type CreateForgeRunCmd struct {
	RunID            string
	ProjectID        string
	ConfigSnapshotID string
	ForgeKind        string // github | gitlab
	ForgeHost        string
	ForgeProjectKey  string
	IssueID          string
	IssueURL         string
	IssueAuthor      string
	// TriggerLabelEventID is the forge id of the trusted trigger-label event;
	// it is the receipt idempotency anchor (UNIQUE project_id+forge_event_id).
	TriggerLabelEventID string
	// TriggerActor is the trusted allowlist actor that applied the trigger
	// label (PRD §9.2): the trigger is only driving when actor-resolved.
	TriggerActor string
	// TriggerObservedAtMS is when the trusted trigger label was observed; it is
	// the P50 start anchor (PRD §10.2 trigger→started).
	TriggerObservedAtMS int64
	CreatedAtMS         int64
}

// CreateForgeRun creates a forge Run from an accepted intake and records the
// trigger-observed event + forge receipt atomically. The Run is created in the
// queued status; SetInitialTaskSpec later writes the T2 kind/agent/task spec.
func (d *DB) CreateForgeRun(ctx context.Context, cmd CreateForgeRunCmd) (Run, error) {
	if cmd.RunID == "" || cmd.ProjectID == "" || cmd.ConfigSnapshotID == "" {
		return Run{}, errors.New("storage: create forge run requires run/project/config ids")
	}
	if cmd.ForgeKind != "github" && cmd.ForgeKind != "gitlab" {
		return Run{}, fmt.Errorf("storage: create forge run kind %q invalid", cmd.ForgeKind)
	}
	if cmd.ForgeHost == "" || cmd.ForgeProjectKey == "" || cmd.IssueID == "" {
		return Run{}, errors.New("storage: create forge run requires forge host/project/issue")
	}
	if cmd.TriggerLabelEventID == "" || cmd.TriggerActor == "" || cmd.TriggerObservedAtMS <= 0 || cmd.CreatedAtMS <= 0 {
		return Run{}, errors.New("storage: create forge run requires trigger event id, actor and timestamps")
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, fmt.Errorf("storage: begin create forge run: %w", err)
	}
	defer tx.Rollback()

	// Intake idempotency: a forge Run for this issue already exists.
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT id FROM runs WHERE forge_kind=? AND forge_host=? AND forge_project_key=? AND issue_id=?`,
		cmd.ForgeKind, cmd.ForgeHost, cmd.ForgeProjectKey, cmd.IssueID).Scan(&existing)
	switch {
	case err == nil:
		// Idempotent: return the existing Run without re-emitting events.
		if err := tx.Rollback(); err != nil {
			return Run{}, err
		}
		return d.Run(ctx, existing)
	case errors.Is(err, sql.ErrNoRows):
	default:
		return Run{}, fmt.Errorf("storage: check existing forge run: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO runs
		(id, source_kind, project_id, config_snapshot_id, forge_kind, forge_host, forge_project_key,
		 issue_id, issue_url, issue_author, status, max_attempts, created_at_ms, updated_at_ms)
		VALUES (?, 'forge', ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?)`,
		cmd.RunID, cmd.ProjectID, cmd.ConfigSnapshotID, cmd.ForgeKind, cmd.ForgeHost, cmd.ForgeProjectKey,
		cmd.IssueID, nullable(cmd.IssueURL), nullable(cmd.IssueAuthor), 3, cmd.CreatedAtMS, cmd.CreatedAtMS); err != nil {
		return Run{}, fmt.Errorf("storage: insert forge run: %w", err)
	}

	// P50 start anchor event: the trusted trigger label was observed (PRD §10.2).
	payload, _ := json.Marshal(map[string]any{
		"forge_event_id": cmd.TriggerLabelEventID,
		"actor":          cmd.TriggerActor,
		"label":          "sift",
	})
	triggerEventID := newID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO events
		(id, run_id, project_id, type, source, actor, payload_schema_version, payload_json, occurred_at_ms, recorded_at_ms)
		VALUES (?, ?, ?, 'intake.trigger_observed', 'forge', ?, 1, ?, ?, ?)`,
		triggerEventID, cmd.RunID, cmd.ProjectID, cmd.TriggerActor, string(payload),
		cmd.TriggerObservedAtMS, cmd.CreatedAtMS); err != nil {
		return Run{}, fmt.Errorf("storage: insert trigger observed event: %w", err)
	}

	// Forge receipt anchors intake idempotency against replay of the label event.
	if _, err := tx.ExecContext(ctx, `INSERT INTO forge_event_receipts
		(id, project_id, forge_event_id, event_kind, target_kind, target_id, actor,
		 raw_digest, disposition, domain_event_id, observed_at_ms)
		VALUES (?, ?, ?, 'issue_label', 'issue', ?, ?, ?, 'accepted', ?, ?)`,
		newID(), cmd.ProjectID, cmd.TriggerLabelEventID, cmd.IssueID, cmd.TriggerActor,
		cmd.TriggerLabelEventID, triggerEventID, cmd.TriggerObservedAtMS); err != nil {
		return Run{}, fmt.Errorf("storage: insert forge receipt: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Run{}, fmt.Errorf("storage: commit create forge run: %w", err)
	}
	return d.Run(ctx, cmd.RunID)
}

// IntakeItemInput is the durable, platform-neutral result of one forge poll.
type IntakeItemInput struct {
	IssueID, IssueURL, IssueDigest string
	ForgeKind, Host, ProjectKey    string
	EventID, EventKind, TargetKind string
	Actor                          string
	ForceHITLBeforeStart           bool
	ObservedAtMS                   int64
	RawDigest                      string
}

type PersistIntakeBatchCmd struct {
	ProjectID    string
	Stream       string
	Cursor       string
	PollMode     string
	NextPollAtMS int64
	NowMS        int64
	Items        []IntakeItemInput
}

type ForgeEventReceipt struct {
	Actor string
}

// IntakeReplyOperation is the durable clarification operation used to identify
// the generation that a reply may belong to. The marker is on the outbound
// clarification comment, not on the operator reply.
type IntakeReplyOperation struct {
	Key      string
	Payload  json.RawMessage
	Evidence json.RawMessage
}

func (d *DB) ForgeEventReceipt(ctx context.Context, projectID, eventID string) (ForgeEventReceipt, error) {
	var receipt ForgeEventReceipt
	err := d.db.QueryRowContext(ctx, `SELECT COALESCE(actor,'') FROM forge_event_receipts WHERE project_id=? AND forge_event_id=?`, projectID, eventID).Scan(&receipt.Actor)
	return receipt, err
}

type IntakeCursor struct {
	ProjectID    string
	Stream       string
	Cursor       string
	PollMode     string
	NextPollAtMS int64
}

type ReplyState struct {
	ProjectID  string
	IssueID    string
	Cursor     string
	Generation int
	MarkerAtMS int64
}

func (d *DB) ReplyState(ctx context.Context, projectID, issueID string) (ReplyState, error) {
	state := ReplyState{ProjectID: projectID, IssueID: issueID}
	var cursor sql.NullString
	err := d.db.QueryRowContext(ctx, `SELECT cursor,generation,marker_at_ms FROM forge_reply_state WHERE project_id=? AND issue_id=?`, projectID, issueID).Scan(&cursor, &state.Generation, &state.MarkerAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return ReplyState{}, err
	}
	state.Cursor = cursor.String
	return state, nil
}

// SaveReplyGenerationContext records the last marker independently of the
// observation cursor. This makes a marker durable before a crash can advance
// the cursor past the page containing it.
func (d *DB) SaveReplyGenerationContext(ctx context.Context, projectID, issueID string, generation int, markerAtMS, nowMS int64) error {
	if projectID == "" || issueID == "" || generation < 1 || markerAtMS < 0 || nowMS <= 0 {
		return errors.New("storage: incomplete reply generation context")
	}
	_, err := d.db.ExecContext(ctx, `INSERT INTO forge_reply_state(project_id,issue_id,generation,marker_at_ms,updated_at_ms) VALUES(?,?,?, ?,?) ON CONFLICT(project_id,issue_id) DO UPDATE SET generation=CASE WHEN excluded.generation > forge_reply_state.generation OR (excluded.generation = forge_reply_state.generation AND excluded.marker_at_ms >= forge_reply_state.marker_at_ms) THEN excluded.generation ELSE forge_reply_state.generation END, marker_at_ms=CASE WHEN excluded.generation > forge_reply_state.generation OR (excluded.generation = forge_reply_state.generation AND excluded.marker_at_ms >= forge_reply_state.marker_at_ms) THEN excluded.marker_at_ms ELSE forge_reply_state.marker_at_ms END, updated_at_ms=excluded.updated_at_ms`, projectID, issueID, generation, markerAtMS, nowMS)
	return err
}

func (d *DB) SaveReplyCursor(ctx context.Context, projectID, issueID, cursor string, nowMS int64) error {
	if projectID == "" || issueID == "" || cursor == "" || nowMS <= 0 {
		return errors.New("storage: incomplete reply cursor")
	}
	_, err := d.db.ExecContext(ctx, `INSERT INTO forge_reply_state(project_id,issue_id,cursor,updated_at_ms) VALUES(?,?,?,?) ON CONFLICT(project_id,issue_id) DO UPDATE SET cursor=excluded.cursor,updated_at_ms=excluded.updated_at_ms`, projectID, issueID, cursor, nowMS)
	return err
}

func (d *DB) IntakeReplyOperations(ctx context.Context, intakeID string) ([]IntakeReplyOperation, error) {
	if intakeID == "" {
		return nil, errors.New("storage: invalid intake id")
	}
	rows, err := d.db.QueryContext(ctx, `SELECT operation_key,payload_json,COALESCE(remote_evidence_json,'') FROM outbox_operations WHERE operation_key LIKE 'comment:intake-%:' || ? || ':%' ORDER BY operation_key`, intakeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IntakeReplyOperation
	for rows.Next() {
		var op IntakeReplyOperation
		var payload, evidence string
		if err := rows.Scan(&op.Key, &payload, &evidence); err != nil {
			return nil, err
		}
		op.Payload = json.RawMessage(payload)
		op.Evidence = json.RawMessage(evidence)
		out = append(out, op)
	}
	return out, rows.Err()
}

// SaveForgeCursor advances a per-target observation cursor. It is deliberately
// separate from PersistIntakeBatch because comment receipts are committed one
// at a time and a crash must replay them safely.
func (d *DB) SaveForgeCursor(ctx context.Context, projectID, stream, cursor string, nowMS int64) error {
	if projectID == "" || stream == "" || cursor == "" || nowMS <= 0 {
		return errors.New("storage: incomplete forge cursor")
	}
	_, err := d.db.ExecContext(ctx, `INSERT INTO forge_cursors(project_id,stream,cursor,poll_mode,next_poll_at_ms,updated_at_ms) VALUES(?,?,?,'active',?,?) ON CONFLICT(project_id,stream) DO UPDATE SET cursor=excluded.cursor,updated_at_ms=excluded.updated_at_ms`, projectID, stream, cursor, nowMS, nowMS)
	return err
}

func (d *DB) IntakeCursor(ctx context.Context, projectID, stream string) (IntakeCursor, error) {
	var c IntakeCursor
	c.ProjectID, c.Stream = projectID, stream
	var cursor sql.NullString
	err := d.db.QueryRowContext(ctx, `SELECT cursor,poll_mode,next_poll_at_ms FROM forge_cursors WHERE project_id=? AND stream=?`, projectID, stream).Scan(&cursor, &c.PollMode, &c.NextPollAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return IntakeCursor{}, err
	}
	c.Cursor = cursor.String
	return c, nil
}

// SetProjectHealth is idempotent: repeated capability failures do not create
// repeated isolation events or alerts, and isolation is scoped to this project.
func (d *DB) SetProjectHealth(ctx context.Context, projectID, reason string, nowMS int64) error {
	if projectID == "" || reason == "" || nowMS <= 0 {
		return errors.New("storage: invalid project health update")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE projects SET health='isolated',isolation_reason=?,updated_at_ms=? WHERE id=? AND health<>'isolated'`, reason, nowMS, projectID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 1 {
		payload, _ := json.Marshal(map[string]any{"project_id": projectID, "reason": reason})
		if _, err = tx.ExecContext(ctx, `INSERT INTO events(id,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES(?,?, 'project.isolated','recovery',1,?,?,?)`, newID(), projectID, string(payload), nowMS, nowMS); err != nil {
			return err
		}
		alertPayload, _ := json.Marshal(map[string]any{"purpose": "project_isolated", "project_id": projectID, "reason": reason})
		if err = insertOperation(ctx, tx, Operation{Key: AlertOperationKey("project_isolated", "project:"+projectID, 1), Kind: OperationForgeAlert, Payload: alertPayload}, "", "", nowMS); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ProjectIsolated reports whether a project has been quarantined by a runtime
// auth/capability failure. Pollers skip isolated projects so a bad credential
// is neither re-probed nor re-alerted every tick (WBS §2.3: alert once, no
// hammering).
func (d *DB) ProjectIsolated(ctx context.Context, projectID string) (bool, error) {
	var health string
	if err := d.db.QueryRowContext(ctx, `SELECT health FROM projects WHERE id=?`, projectID).Scan(&health); err != nil {
		return false, err
	}
	return health == "isolated", nil
}

type PendingIntake struct {
	ID, ProjectID, ForgeKind, Host, ProjectKey, IssueID, IssueURL, IssueDigest string
	Version                                                                    int
	Generation                                                                 int
	State                                                                      string
	ForceHITLBeforeStart                                                       bool
}

// AwaitingIntakes returns the current reply targets for one project.
func (d *DB) AwaitingIntakes(ctx context.Context, projectID string) ([]PendingIntake, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT id,project_id,forge_kind,normalized_host,forge_project_key,issue_id,issue_url,issue_digest,version,clarification_generation,state,force_hitl_before_start FROM intake_items WHERE project_id=? AND state IN ('awaiting_clarification','awaiting_duplicate_confirmation')`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingIntake
	for rows.Next() {
		var x PendingIntake
		if err := rows.Scan(&x.ID, &x.ProjectID, &x.ForgeKind, &x.Host, &x.ProjectKey, &x.IssueID, &x.IssueURL, &x.IssueDigest, &x.Version, &x.Generation, &x.State, &x.ForceHITLBeforeStart); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (d *DB) FindPendingIntake(ctx context.Context, projectID, issueID string) (PendingIntake, error) {
	var x PendingIntake
	err := d.db.QueryRowContext(ctx, `SELECT id,project_id,forge_kind,normalized_host,forge_project_key,issue_id,issue_url,issue_digest,version,clarification_generation,state,force_hitl_before_start FROM intake_items WHERE project_id=? AND issue_id=? AND state='pending_evaluation'`, projectID, issueID).Scan(&x.ID, &x.ProjectID, &x.ForgeKind, &x.Host, &x.ProjectKey, &x.IssueID, &x.IssueURL, &x.IssueDigest, &x.Version, &x.Generation, &x.State, &x.ForceHITLBeforeStart)
	return x, err
}

func (d *DB) PendingIntake(ctx context.Context, limit int) ([]PendingIntake, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.db.QueryContext(ctx, `SELECT id,project_id,forge_kind,normalized_host,forge_project_key,issue_id,issue_url,issue_digest,version,clarification_generation,state,force_hitl_before_start FROM intake_items WHERE state='pending_evaluation' ORDER BY created_at_ms,id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingIntake
	for rows.Next() {
		var x PendingIntake
		if err := rows.Scan(&x.ID, &x.ProjectID, &x.ForgeKind, &x.Host, &x.ProjectKey, &x.IssueID, &x.IssueURL, &x.IssueDigest, &x.Version, &x.Generation, &x.State, &x.ForceHITLBeforeStart); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// PersistIntakeBatch writes receipts and pending intake projections before
// advancing the cursor. Replaying a forge page is consequently harmless, and
// a failure leaves the cursor at its previous value.
func (d *DB) PersistIntakeBatch(ctx context.Context, cmd PersistIntakeBatchCmd) error {
	if cmd.ProjectID == "" || cmd.Stream == "" || cmd.NowMS <= 0 {
		return errors.New("storage: intake batch requires project, stream and timestamp")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, item := range cmd.Items {
		if item.IssueID == "" || item.EventID == "" || item.IssueDigest == "" || item.RawDigest == "" || item.ObservedAtMS <= 0 {
			return errors.New("storage: intake item is missing identity or receipt facts")
		}
		var receiptID string
		err = tx.QueryRowContext(ctx, `SELECT id FROM forge_event_receipts WHERE project_id=? AND forge_event_id=?`, cmd.ProjectID, item.EventID).Scan(&receiptID)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		intakeID := newID()
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT id FROM intake_items WHERE forge_kind=? AND normalized_host=? AND forge_project_key=? AND issue_id=?`, item.ForgeKind, item.Host, item.ProjectKey, item.IssueID).Scan(&existing)
		if errors.Is(err, sql.ErrNoRows) {
			if _, err = tx.ExecContext(ctx, `INSERT INTO intake_items
				(id,project_id,forge_kind,normalized_host,forge_project_key,issue_id,issue_url,issue_digest,force_hitl_before_start,state,version,created_at_ms,updated_at_ms)
				VALUES (?,?,?,?,?,?,?,?,?,'pending_evaluation',1,?,?)`, intakeID, cmd.ProjectID, item.ForgeKind, item.Host, item.ProjectKey, item.IssueID, item.IssueURL, item.IssueDigest, boolInt(item.ForceHITLBeforeStart), cmd.NowMS, cmd.NowMS); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			intakeID = existing
			if item.ForceHITLBeforeStart {
				if _, err = tx.ExecContext(ctx, `UPDATE intake_items SET force_hitl_before_start=1,updated_at_ms=? WHERE id=?`, cmd.NowMS, intakeID); err != nil {
					return err
				}
			}
		}
		domainEventID := newID()
		payload, _ := json.Marshal(map[string]any{"forge_event_id": item.EventID, "event_kind": item.EventKind, "target_id": item.IssueID})
		if _, err = tx.ExecContext(ctx, `INSERT INTO events (id,project_id,type,source,actor,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES (?,?, 'intake.issue_observed','forge',?,1,?,?,?)`, domainEventID, cmd.ProjectID, nullable(item.Actor), string(payload), item.ObservedAtMS, cmd.NowMS); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO forge_event_receipts (id,project_id,forge_event_id,event_kind,target_kind,target_id,actor,raw_digest,disposition,domain_event_id,observed_at_ms) VALUES (?,?,?,?,?,?,?,?, 'accepted',?,?)`, newID(), cmd.ProjectID, item.EventID, item.EventKind, valueOr(item.TargetKind, "issue"), item.IssueID, nullable(item.Actor), item.RawDigest, domainEventID, item.ObservedAtMS); err != nil {
			return err
		}
	}
	mode := cmd.PollMode
	if mode == "" {
		mode = "active"
	}
	next := cmd.NextPollAtMS
	if next == 0 {
		next = cmd.NowMS
	}
	if mode != "idle" && mode != "active" && mode != "interrupt" && mode != "slow" {
		return errors.New("storage: invalid intake poll mode")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO forge_cursors(project_id,stream,cursor,poll_mode,next_poll_at_ms,updated_at_ms) VALUES(?,?,?,?,?,?) ON CONFLICT(project_id,stream) DO UPDATE SET cursor=excluded.cursor,poll_mode=excluded.poll_mode,next_poll_at_ms=excluded.next_poll_at_ms,updated_at_ms=excluded.updated_at_ms`, cmd.ProjectID, cmd.Stream, nullable(cmd.Cursor), mode, next, cmd.NowMS); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return nil
}

type IntakeDecisionCmd struct {
	IntakeID, AssessmentID, LogicalCallID string
	ExpectedVersion                       int
	Disposition                           string
	QuestionsJSON                         string
	PossibleDuplicateRunID                *string
	Rationale                             string
	NowMS                                 int64
	RunID                                 string
}

// PersistIntakeDecision is the single transaction that records a T1 result,
// advances the intake CAS, and creates the required clarification operation or
// forge Run. It deliberately does not perform Forge or provider I/O.
func (d *DB) PersistIntakeDecision(ctx context.Context, cmd IntakeDecisionCmd) error {
	if cmd.IntakeID == "" || cmd.AssessmentID == "" || cmd.LogicalCallID == "" || cmd.ExpectedVersion < 1 || cmd.NowMS <= 0 {
		return errors.New("storage: incomplete intake decision")
	}
	if cmd.Disposition != "ready" && cmd.Disposition != "needs_clarification" && cmd.Disposition != "possible_duplicate" {
		return errors.New("storage: invalid intake disposition")
	}
	if cmd.QuestionsJSON == "" {
		cmd.QuestionsJSON = "[]"
	}
	if !json.Valid([]byte(cmd.QuestionsJSON)) {
		return errors.New("storage: invalid intake questions JSON")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var projectID, kind, host, projectKey, issueID, issueURL, state string
	var version, generation, forceHITL int
	err = tx.QueryRowContext(ctx, `SELECT project_id,forge_kind,normalized_host,forge_project_key,issue_id,issue_url,state,version,clarification_generation,force_hitl_before_start FROM intake_items WHERE id=?`, cmd.IntakeID).Scan(&projectID, &kind, &host, &projectKey, &issueID, &issueURL, &state, &version, &generation, &forceHITL)
	if err != nil {
		return err
	}
	if version != cmd.ExpectedVersion {
		return ErrRejectedStale
	}
	if state != "pending_evaluation" && state != "evaluating" {
		return fmt.Errorf("storage: intake state %q cannot accept decision", state)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO intake_assessments(id,intake_id,logical_call_id,disposition,questions_json,possible_duplicate_run_id,rationale,created_at_ms) VALUES(?,?,?,?,?,?,?,?)`, cmd.AssessmentID, cmd.IntakeID, cmd.LogicalCallID, cmd.Disposition, cmd.QuestionsJSON, nullableStringPtr(cmd.PossibleDuplicateRunID), cmd.Rationale, cmd.NowMS); err != nil {
		return err
	}
	newState := "ready"
	newGeneration := generation
	if cmd.Disposition == "needs_clarification" {
		newState = "awaiting_clarification"
		newGeneration++
	}
	if cmd.Disposition == "possible_duplicate" {
		newState = "awaiting_duplicate_confirmation"
		newGeneration++
	}
	if cmd.Disposition == "ready" {
		if cmd.RunID == "" {
			cmd.RunID = newID()
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO runs(id,source_kind,project_id,config_snapshot_id,forge_kind,forge_host,forge_project_key,issue_id,issue_url,status,hitl_before_start,max_attempts,created_at_ms,updated_at_ms) SELECT ?, 'forge',project_id,(SELECT config_snapshot_id FROM projects WHERE id=project_id),forge_kind,normalized_host,forge_project_key,issue_id,issue_url,'queued',?,3,?,? FROM intake_items WHERE id=?`, cmd.RunID, forceHITL, cmd.NowMS, cmd.NowMS, cmd.IntakeID); err != nil {
			return err
		}
		newState = "consumed"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE intake_items SET state=?,version=version+1,latest_assessment_id=?,linked_run_id=?,duplicate_candidate_run_id=?,clarification_generation=?,updated_at_ms=? WHERE id=? AND version=?`, newState, cmd.AssessmentID, nullable(cmd.RunID), nullableStringPtr(cmd.PossibleDuplicateRunID), newGeneration, cmd.NowMS, cmd.IntakeID, cmd.ExpectedVersion); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"intake_id": cmd.IntakeID, "disposition": cmd.Disposition, "generation": newGeneration})
	if _, err = tx.ExecContext(ctx, `INSERT INTO events(id,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES(?,?, 'intake.decision','system',1,?,?,?)`, newID(), projectID, string(payload), cmd.NowMS, cmd.NowMS); err != nil {
		return err
	}
	if cmd.Disposition != "ready" {
		purpose := "intake-clarification"
		if cmd.Disposition == "possible_duplicate" {
			purpose = "intake-duplicate-confirmation"
		}
		body, _ := json.Marshal(map[string]any{"project_id": projectID, "forge_kind": kind, "forge_host": host, "forge_project_key": projectKey, "target_kind": "issue", "target_id": issueID, "purpose": purpose, "intake_id": cmd.IntakeID, "generation": newGeneration, "markdown": cmd.Rationale})
		op := Operation{Key: CommentOperationKey(purpose, cmd.IntakeID, newGeneration), Kind: OperationForgeComment, Payload: body}
		if err = insertOperation(ctx, tx, op, "", "", cmd.NowMS); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func nullableStringPtr(p *string) any {
	if p == nil || *p == "" {
		return nil
	}
	return *p
}
func valueOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// IntakeReplyCmd is a trusted, already-authenticated forge comment. The
// generation is carried by the comment operation payload, never inferred from
// its natural-language body.
type IntakeReplyCmd struct {
	IntakeID, EventID, Actor, RawDigest string
	Generation                          int
	Accept                              bool
	ObservedAtMS, NowMS                 int64
}

// ApplyIntakeReply records every reply. A reply from an old generation is
// intentionally audit-only; it cannot move the current projection.
func (d *DB) ApplyIntakeReply(ctx context.Context, cmd IntakeReplyCmd) error {
	if cmd.IntakeID == "" || cmd.EventID == "" || cmd.Actor == "" || cmd.Generation < 1 || cmd.ObservedAtMS <= 0 || cmd.NowMS <= 0 {
		return errors.New("storage: incomplete intake reply")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var projectID, state string
	var generation, version int
	if err = tx.QueryRowContext(ctx, `SELECT project_id,state,version,clarification_generation FROM intake_items WHERE id=?`, cmd.IntakeID).Scan(&projectID, &state, &version, &generation); err != nil {
		return err
	}
	var existingReceipt string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM forge_event_receipts WHERE project_id=? AND forge_event_id=?`, projectID, cmd.EventID).Scan(&existingReceipt); err == nil {
		return tx.Commit()
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"intake_id": cmd.IntakeID, "generation": cmd.Generation, "accepted": cmd.Accept, "forge_event_id": cmd.EventID})
	eventType := "intake.reply_ignored"
	if cmd.Generation == generation && cmd.Accept && (state == "awaiting_clarification" || state == "awaiting_duplicate_confirmation") {
		eventType = "intake.reply_accepted"
		if _, err = tx.ExecContext(ctx, `UPDATE intake_items SET state='pending_evaluation',version=version+1,updated_at_ms=? WHERE id=? AND version=?`, cmd.NowMS, cmd.IntakeID, version); err != nil {
			return err
		}
	}
	eventID := newID()
	if _, err = tx.ExecContext(ctx, `INSERT INTO events(id,project_id,type,source,actor,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES(?,?,?,'forge',?,1,?,?,?)`, eventID, projectID, eventType, cmd.Actor, string(payload), cmd.ObservedAtMS, cmd.NowMS); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO forge_event_receipts(id,project_id,forge_event_id,event_kind,target_kind,target_id,actor,raw_digest,disposition,domain_event_id,observed_at_ms) VALUES(?,?,?,?,'issue',?,?,?, 'accepted',?,?)`, newID(), projectID, cmd.EventID, "issue_comment", cmd.IntakeID, cmd.Actor, cmd.RawDigest, eventID, cmd.ObservedAtMS); err != nil {
		return err
	}
	return tx.Commit()
}

// ReportSubmitCmd is the daemon-side production port for report.submit. The
// token is checked against the launch claim; callers never receive database access.
type ReportSubmitCmd struct {
	Token      string
	RunID      string
	AttemptNo  int
	Generation int
	ReportKey  string
	Kind       string
	Payload    map[string]any
	NowMS      int64
}
type ReportResult struct {
	Disposition string `json:"disposition"`
	ReceiptID   string `json:"receipt_id"`
	EventID     string `json:"event_id"`
}

// RetryPolicy is the closed not_ready backoff derived from the binding Run's
// frozen config snapshot (report.md §4). The CLI consumes it verbatim and
// recomputes each delay from these exact integer fields.
type RetryPolicy struct {
	InitialDelayMS   int `json:"initial_delay_ms"`
	MultiplierMicros int `json:"multiplier_micros"`
	MaxDelayMS       int `json:"max_delay_ms"`
	TotalTimeoutMS   int `json:"total_timeout_ms"`
}

// ReportNotReadyError signals a legal spawning window: the attempt is bound to
// the presented run token but claim.started has not linearized yet. It carries
// the closed retry_policy so the CLI never reads config.yaml.
type ReportNotReadyError struct {
	RetryPolicy RetryPolicy
}

func (e *ReportNotReadyError) Error() string { return "report: not ready" }
func (e *ReportNotReadyError) Unwrap() error { return ErrReportNotReady }

// Typed report errors let the control-plane gateway map each outcome to the
// closed error code set (control-plane.md §3.4) without parsing strings.
var (
	ErrReportInvalid         = errors.New("report: invalid")
	ErrReportUnauthorized    = errors.New("report: unauthorized")
	ErrReportStale           = errors.New("report: stale")
	ErrReportConflict        = errors.New("report: conflict")
	ErrReportRateLimited     = errors.New("report: rate limit exceeded")
	ErrReportQuotaExhausted  = errors.New("report: report_interrupt_quota_exhausted")
	ErrReportPayloadTooLarge = errors.New("report: payload too large")
	ErrReportNotReady        = errors.New("report: not ready")
)

func (d *DB) RecordReport(ctx context.Context, cmd ReportSubmitCmd) (ReportResult, error) {
	if cmd.RunID == "" || cmd.AttemptNo < 1 || cmd.Generation < 1 || cmd.NowMS <= 0 || len(cmd.ReportKey) != 32 || !lowerHex(cmd.ReportKey) || cmd.Token == "" {
		return ReportResult{}, fmt.Errorf("%w: request", ErrReportInvalid)
	}
	if cmd.Kind != "progress" && cmd.Kind != "goal" && cmd.Kind != "blocker" && cmd.Kind != "completed" {
		return ReportResult{}, fmt.Errorf("%w: kind", ErrReportInvalid)
	}
	if err := validateReportPayload(cmd.Kind, cmd.Payload); err != nil {
		return ReportResult{}, err
	}
	wrapped := map[string]any{"kind": cmd.Kind, "payload": cmd.Payload}
	canonical, _ := json.Marshal(wrapped)
	sum := sha256.Sum256(canonical)
	digest := hex.EncodeToString(sum[:])
	payloadCanonical, _ := json.Marshal(cmd.Payload)
	// The binding pre-check authorizes the run token, generation and attempt
	// phase before any write transaction opens. not_ready in particular must
	// not consume a rate token or occupy a report key, so it returns here.
	binding, err := d.checkReportBinding(ctx, cmd)
	if err != nil {
		return ReportResult{}, err
	}
	if len(payloadCanonical) > binding.cfg.Report.MaxPayloadBytes {
		return ReportResult{}, ErrReportPayloadTooLarge
	}
	if cmd.Kind == "blocker" {
		return d.recordBlockerReport(ctx, cmd, digest, binding)
	}
	return d.recordSimpleReport(ctx, cmd, digest, binding)
}

// recordSimpleReport writes a non-blocker (progress/goal/completed) report. It
// never writes runs.status, a Report charge, or an Interrupt; only the
// append-only event and its immutable receipt share one transaction with the
// rate-token CAS (report.md §5.1, §7).
func (d *DB) recordSimpleReport(ctx context.Context, cmd ReportSubmitCmd, digest string, binding reportBinding) (ReportResult, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return ReportResult{}, err
	}
	defer tx.Rollback()
	projectID, snapshotID, runVersion, err := assertReportBindingTx(ctx, tx, cmd)
	if err != nil {
		return ReportResult{}, err
	}
	_ = runVersion
	if dup, kind := lookupReportDuplicateTx(ctx, tx, cmd, digest, binding.cfg.Report.DedupeWindow); kind != dedupeNone {
		if kind == dedupeConflict {
			return ReportResult{}, ErrReportConflict
		}
		return dup, tx.Commit()
	}
	if err := consumeReportTokenTx(ctx, tx, cmd, snapshotID); err != nil {
		return ReportResult{}, err
	}
	eventID, receiptID := newID(), newID()
	payload, _ := json.Marshal(map[string]any{"report_key": cmd.ReportKey, "payload_digest": digest, "generation": cmd.Generation, "report": cmd.Payload})
	if _, err = tx.ExecContext(ctx, `INSERT INTO events(id,run_id,attempt_no,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES(?,?,?,?,?, 'agent',1,?,?,?)`, eventID, cmd.RunID, cmd.AttemptNo, projectID, "report."+cmd.Kind, string(payload), cmd.NowMS, cmd.NowMS); err != nil {
		return ReportResult{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO report_receipts(id,run_id,attempt_no,report_key,report_kind,payload_digest,event_id,report_interrupt_charge_entry_id,received_at_ms) VALUES(?,?,?,?,?,?,?,?,?)`, receiptID, cmd.RunID, cmd.AttemptNo, cmd.ReportKey, cmd.Kind, digest, eventID, nullable(""), cmd.NowMS); err != nil {
		return ReportResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return ReportResult{}, err
	}
	return ReportResult{"accepted", receiptID, eventID}, nil
}

// recordBlockerReport makes the Report receipt/event, rate token, child quota,
// and agent_blocked Interrupt one SQLite transaction. EmitInterrupt invokes
// before only after T4/T6 have completed and rolls this transaction back on
// any structural or publish failure.
func (d *DB) recordBlockerReport(ctx context.Context, cmd ReportSubmitCmd, digest string, binding reportBinding) (ReportResult, error) {
	var result ReportResult
	var exhausted bool
	cfg := binding.cfg
	runVersion := binding.RunVersion
	n := cmd.AttemptNo
	receiptID := newID()
	var batchAt *int64
	if at, ok := nextSummary(cmd.NowMS, timezoneOrUTC(cfg.Attention.DayTimezone), summaryOrDefault(cfg.Attention.DailySummaryAt)); ok {
		batchAt = &at
	}
	facts := map[string]string{"blocker_summary": cmd.Payload["blocker_summary"].(string), "attempted_summary": cmd.Payload["attempted_summary"].(string), "recommended_action": cmd.Payload["recommended_action"].(string), "agent_log_ref": strings.TrimRight(binding.Worktree, "/") + "/agent.log"}
	err := func() error {
		_, emitErr := d.emitReportInterruptHooks(ctx, EmitInterruptCmd{RunID: cmd.RunID, ExpectedRunVersion: runVersion, AttemptNo: &n, Reason: InterruptAgentBlocked, Facts: facts, Generation: InterruptGeneration{AttemptNo: cmd.AttemptNo, Generation: cmd.Generation, ReportID: receiptID}, GatePhase: GateNone, GuardrailLevel: GuardrailNone, MaxEscalations: cfg.Attention.MaxEscalations, AttentionDailyQuota: map[InterruptSeverity]int{SeverityLow: cfg.Attention.DailyQuota.Low, SeverityNormal: cfg.Attention.DailyQuota.Normal, SeverityHigh: cfg.Attention.DailyQuota.High}, DayTimezone: cfg.Attention.DayTimezone, DailySummaryAt: cfg.Attention.DailySummaryAt, CriticalWindowMS: cfg.Attention.CriticalFuse.Window, CriticalTotalLimit: cfg.Attention.CriticalFuse.TotalLimit, CriticalPerRunLimit: cfg.Attention.CriticalFuse.PerRunLimit, Channels: reportChannels(cfg), BatchAtMS: batchAt, Source: SourceAgent, NowMS: cmd.NowMS}, func(tx *sql.Tx) error {
			projectID, snapshotID, _, err := assertReportBindingTx(ctx, tx, cmd)
			if err != nil {
				return err
			}
			if dup, kind := lookupReportDuplicateTx(ctx, tx, cmd, digest, cfg.Report.DedupeWindow); kind != dedupeNone {
				if kind == dedupeConflict {
					return ErrReportConflict
				}
				result = dup
				return errReportDuplicate
			}
			if err := consumeReportTokenTx(ctx, tx, cmd, snapshotID); err != nil {
				return err
			}
			start, end, err := reportDayBucket(cmd.NowMS, cfg.Attention.DayTimezone)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO budget_counters(kind,scope,scope_id,bucket_start_ms,bucket_end_ms,limit_value,consumed_value,version,updated_at_ms) VALUES('report','run',?,?,?, ?,0,1,?) ON CONFLICT DO NOTHING`, cmd.RunID, start, end, cfg.Report.InterruptsPerRunDailyQuota, cmd.NowMS); err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `UPDATE budget_counters SET consumed_value=consumed_value+1,version=version+1,updated_at_ms=? WHERE kind='report' AND scope='run' AND scope_id=? AND bucket_start_ms=? AND consumed_value<limit_value`, cmd.NowMS, cmd.RunID, start)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				exhausted = true
				return errReportQuotaExhausted
			}
			eventID, chargeID := newID(), newID()
			payload, _ := json.Marshal(map[string]any{"report_key": cmd.ReportKey, "payload_digest": digest, "generation": cmd.Generation, "report": cmd.Payload})
			if _, err = tx.ExecContext(ctx, `INSERT INTO budget_entries(id,kind,scope,scope_id,bucket_start_ms,amount,reason,run_id,operation_key,created_at_ms) VALUES(?,'report','run',?,?,1,'report_agent_blocked',?,?,?)`, chargeID, cmd.RunID, start, cmd.RunID, "report-interrupt-quota:"+receiptID, cmd.NowMS); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO events(id,run_id,attempt_no,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES(?,?,?,?,?, 'agent',1,?,?,?)`, eventID, cmd.RunID, cmd.AttemptNo, projectID, "report.blocker", string(payload), cmd.NowMS, cmd.NowMS); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO report_receipts(id,run_id,attempt_no,report_key,report_kind,payload_digest,event_id,report_interrupt_charge_entry_id,received_at_ms) VALUES(?,?,?,?,?,?,?,?,?)`, receiptID, cmd.RunID, cmd.AttemptNo, cmd.ReportKey, cmd.Kind, digest, eventID, chargeID, cmd.NowMS); err != nil {
				return err
			}
			result = ReportResult{"accepted", receiptID, eventID}
			return nil
		}, func(tx *sql.Tx, in Interrupt) error {
			_, err := tx.ExecContext(ctx, `UPDATE report_receipts SET direct_interrupt_id=? WHERE id=? AND direct_interrupt_id IS NULL`, in.ID, receiptID)
			return err
		})
		return emitErr
	}()
	if errors.Is(err, errReportDuplicate) {
		return result, nil
	}
	if errors.Is(err, errReportQuotaExhausted) && exhausted {
		start, end, bucketErr := reportDayBucket(cmd.NowMS, cfg.Attention.DayTimezone)
		if bucketErr != nil {
			return ReportResult{}, bucketErr
		}
		if bucketErr = d.commitReportQuotaExhaustion(ctx, cmd, cfg, runVersion, start, end); bucketErr != nil {
			return ReportResult{}, bucketErr
		}
		_, emitErr := d.RecordReportQuotaExhaustion(ctx, reportQuotaCmd(cmd, runVersion, start, end, cfg))
		if emitErr != nil && !errors.Is(emitErr, ErrInterruptRejected) {
			return ReportResult{}, emitErr
		}
		return ReportResult{}, ErrReportQuotaExhausted
	}
	if err != nil {
		return ReportResult{}, err
	}
	return result, nil
}

var errReportDuplicate = errors.New("report duplicate")
var errReportQuotaExhausted = errors.New("report quota exhausted")

func (d *DB) commitReportQuotaExhaustion(ctx context.Context, cmd ReportSubmitCmd, cfg reportRuntimeConfig, runVersion, start, end int64) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var phase, tokenHash, snapshotID string
	var generation int
	if err := tx.QueryRowContext(ctx, `SELECT a.phase,a.generation,c.run_token_hash,r.config_snapshot_id FROM attempts a JOIN attempt_claims c USING(run_id,attempt_no) JOIN runs r ON r.id=a.run_id WHERE a.run_id=? AND a.attempt_no=?`, cmd.RunID, cmd.AttemptNo).Scan(&phase, &generation, &tokenHash, &snapshotID); err != nil || phase != "running" || generation != cmd.Generation || tokenHash != handoffHash(cmd.Token) {
		return ErrReportUnauthorized
	}
	// The exhaustion row is the rate-token linearization point. Replays and
	// later blockers reuse it without consuming another token.
	var existing string
	if err := tx.QueryRowContext(ctx, `SELECT security_event_id FROM report_quota_exhaustions WHERE run_id=? AND daily_bucket_start_ms=?`, cmd.RunID, start).Scan(&existing); err == nil {
		return tx.Commit()
	} else if err != sql.ErrNoRows {
		return err
	}
	if err := consumeReportTokenTx(ctx, tx, cmd, snapshotID); err != nil {
		return err
	}
	var projectID string
	if err := tx.QueryRowContext(ctx, `SELECT project_id FROM runs WHERE id=? AND version=?`, cmd.RunID, runVersion).Scan(&projectID); err != nil {
		return err
	}
	eventID := newID()
	digest := reportQuotaFailureDigest(cmd.RunID, start, end)
	key, err := interruptGenerationKey(cmd.RunID, InterruptFailureReview, InterruptGeneration{ReportDailyBucketStartMS: start, ReportDailyBucketEndMS: end, FailureDigest: digest})
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"daily_bucket_end_ms": end, "daily_bucket_start_ms": start, "failure_class": "report_interrupt_quota_exhausted", "failure_digest": digest, "generation_key": key})
	if _, err = tx.ExecContext(ctx, `INSERT INTO events(id,run_id,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES(?,?,?,'security.report_quota_exhausted','system',1,?,?,?)`, eventID, cmd.RunID, projectID, string(payload), cmd.NowMS, cmd.NowMS); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO report_quota_exhaustions(run_id,daily_bucket_start_ms,daily_bucket_end_ms,security_event_id,failure_digest,generation_key,created_at_ms) VALUES(?,?,?,?,?,?,?)`, cmd.RunID, start, end, eventID, digest, key, cmd.NowMS); err != nil {
		// Another writer may have linearized this day after our initial read.
		// Roll back the tentative token/event, then reuse the durable winner.
		_ = tx.Rollback()
		var winner string
		if lookupErr := d.db.QueryRowContext(ctx, `SELECT security_event_id FROM report_quota_exhaustions WHERE run_id=? AND daily_bucket_start_ms=?`, cmd.RunID, start).Scan(&winner); lookupErr == nil {
			return nil
		}
		return err
	}
	return tx.Commit()
}

func consumeReportTokenTx(ctx context.Context, tx *sql.Tx, cmd ReportSubmitCmd, snapshotID string) error {
	var capacity, available, numerator, period, remainder, last int64
	scope := "run:" + cmd.RunID + ":attempt:" + fmt.Sprint(cmd.AttemptNo)
	err := tx.QueryRowContext(ctx, `SELECT capacity_units,available_units,refill_numerator,refill_period_ms,refill_remainder,last_refill_at_ms FROM rate_limit_buckets WHERE kind='report' AND scope_id=?`, scope).Scan(&capacity, &available, &numerator, &period, &remainder, &last)
	if err == sql.ErrNoRows {
		var raw string
		if err = tx.QueryRowContext(ctx, `SELECT canonical_json FROM config_snapshots WHERE id=?`, snapshotID).Scan(&raw); err != nil {
			return err
		}
		var c struct {
			Report struct {
				EventsPerMinute int `json:"events_per_minute"`
				Burst           int `json:"burst"`
			} `json:"report"`
		}
		if json.Unmarshal([]byte(raw), &c) != nil || c.Report.Burst < 1 || c.Report.EventsPerMinute < 1 {
			return errors.New("report: invalid snapshot")
		}
		capacity, available, numerator, period, last = int64(c.Report.Burst), int64(c.Report.Burst), int64(c.Report.EventsPerMinute), 60000, cmd.NowMS
		if _, err = tx.ExecContext(ctx, `INSERT INTO rate_limit_buckets(kind,scope_id,capacity_units,available_units,refill_numerator,refill_period_ms,refill_remainder,last_refill_at_ms,version) VALUES('report',?,?,?,?,?,?,?,1)`, scope, capacity, available, numerator, period, 0, last); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if cmd.NowMS > last {
		add := (cmd.NowMS - last) * numerator / period
		if add > 0 {
			available += add
			if available > capacity {
				available = capacity
			}
			last = cmd.NowMS
		}
	}
	if available < 1 {
		return ErrReportRateLimited
	}
	available--
	_, err = tx.ExecContext(ctx, `UPDATE rate_limit_buckets SET available_units=?,last_refill_at_ms=?,version=version+1 WHERE kind='report' AND scope_id=?`, available, last, scope)
	return err
}

type reportRuntimeChannel struct {
	ID           string   `json:"id"`
	Enabled      bool     `json:"enabled"`
	Type         string   `json:"type"`
	TargetRef    string   `json:"target_ref"`
	Capabilities []string `json:"capabilities"`
	Renderer     string   `json:"renderer"`
	Default      bool     `json:"default"`
}

type reportRuntimeConfig struct {
	Runtime struct {
		RetryMultiplier float64 `json:"retry_multiplier"`
	} `json:"runtime"`
	Attention struct {
		DayTimezone string `json:"day_timezone"`
		DailyQuota  struct {
			Low    int `json:"low"`
			Normal int `json:"normal"`
			High   int `json:"high"`
		} `json:"daily_quota"`
		MaxEscalations int `json:"max_escalations"`
		CriticalFuse   struct {
			Window      int64 `json:"window"`
			TotalLimit  int   `json:"total_limit"`
			PerRunLimit int   `json:"per_run_limit"`
		} `json:"critical_fuse"`
		DailySummaryAt string                 `json:"daily_summary_at"`
		Channels       []reportRuntimeChannel `json:"channels"`
	} `json:"attention"`
	Report struct {
		EventsPerMinute            int           `json:"events_per_minute"`
		Burst                      int           `json:"burst"`
		DedupeWindow               time.Duration `json:"dedupe_window"`
		MaxPayloadBytes            int           `json:"max_payload_bytes"`
		NotReadyInitialDelay       time.Duration `json:"not_ready_initial_delay"`
		NotReadyMaxDelay           time.Duration `json:"not_ready_max_delay"`
		NotReadyTotalTimeout       time.Duration `json:"not_ready_total_timeout"`
		InterruptsPerRunDailyQuota int           `json:"interrupts_per_run_daily_quota"`
	} `json:"report"`
}

func reportDayBucket(nowMS int64, zone string) (int64, int64, error) {
	loc, err := time.LoadLocation(timezoneOrUTC(zone))
	if err != nil {
		return 0, 0, fmt.Errorf("report: invalid day timezone: %w", err)
	}
	now := time.UnixMilli(nowMS).In(loc)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	return start.UnixMilli(), start.AddDate(0, 0, 1).UnixMilli(), nil
}

func reportQuotaCmd(cmd ReportSubmitCmd, version, start, end int64, cfg reportRuntimeConfig) ReportQuotaExhaustionCmd {
	return ReportQuotaExhaustionCmd{RunID: cmd.RunID, ExpectedRunVersion: version, DailyBucketStartMS: start, DailyBucketEndMS: end, AttentionDailyQuota: map[InterruptSeverity]int{SeverityLow: cfg.Attention.DailyQuota.Low, SeverityNormal: cfg.Attention.DailyQuota.Normal, SeverityHigh: cfg.Attention.DailyQuota.High}, DayTimezone: cfg.Attention.DayTimezone, DailySummaryAt: cfg.Attention.DailySummaryAt, CriticalWindowMS: cfg.Attention.CriticalFuse.Window, CriticalTotalLimit: cfg.Attention.CriticalFuse.TotalLimit, CriticalPerRunLimit: cfg.Attention.CriticalFuse.PerRunLimit, Channels: reportChannels(cfg), NowMS: cmd.NowMS}
}

func reportChannels(cfg reportRuntimeConfig) []InterruptChannel {
	channels := make([]InterruptChannel, 0, len(cfg.Attention.Channels))
	for _, c := range cfg.Attention.Channels {
		channels = append(channels, InterruptChannel{ID: c.ID, Type: c.Type, TargetRef: c.TargetRef, Capabilities: c.Capabilities, Renderer: c.Renderer, Default: c.Default, Isolated: !c.Enabled})
	}
	return channels
}

func validateReportPayload(kind string, p map[string]any) error {
	if p == nil {
		return fmt.Errorf("%w: payload object", ErrReportInvalid)
	}
	want := map[string]string{"progress": "message", "goal": "goal", "blocker": "blocker_summary,attempted_summary,recommended_action", "completed": "summary"}[kind]
	keys := strings.Split(want, ",")
	if len(p) != len(keys) {
		return fmt.Errorf("%w: payload not closed", ErrReportInvalid)
	}
	for _, k := range keys {
		v, ok := p[k].(string)
		if !ok || v == "" {
			return fmt.Errorf("%w: payload field %q", ErrReportInvalid, k)
		}
		// report.md §3: reject empty strings, NUL and every Unicode Cc control
		// code point so the event is safe to project onto a single-line timeline.
		for _, r := range v {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("%w: payload field %q", ErrReportInvalid, k)
			}
		}
	}
	return nil
}

// reportBinding is the frozen attempt/claim/run state plus the Run's resolved
// config snapshot, read once before any write transaction opens. Every Report
// outcome derives its authorization, phase and policy values from this struct.
type reportBinding struct {
	Phase      string
	Generation int
	TokenHash  string
	ProjectID  string
	SnapshotID string
	RunVersion int64
	Worktree   string
	cfg        reportRuntimeConfig
}

// checkReportBinding authorizes the run token, generation and attempt phase in
// a single read. It is the not_ready fast path: a legal spawning window returns
// ReportNotReadyError before any transaction or rate token is touched.
func (d *DB) checkReportBinding(ctx context.Context, cmd ReportSubmitCmd) (reportBinding, error) {
	var b reportBinding
	err := d.db.QueryRowContext(ctx, `SELECT a.phase,a.generation,c.run_token_hash,r.project_id,r.config_snapshot_id,r.version,a.worktree_path FROM attempts a JOIN attempt_claims c USING(run_id,attempt_no) JOIN runs r ON r.id=a.run_id WHERE a.run_id=? AND a.attempt_no=?`, cmd.RunID, cmd.AttemptNo).Scan(&b.Phase, &b.Generation, &b.TokenHash, &b.ProjectID, &b.SnapshotID, &b.RunVersion, &b.Worktree)
	if err != nil {
		return reportBinding{}, ErrReportUnauthorized
	}
	if b.TokenHash != handoffHash(cmd.Token) {
		return reportBinding{}, ErrReportUnauthorized
	}
	if b.Generation != cmd.Generation {
		return reportBinding{}, ErrReportStale
	}
	raw, err := reportSnapshotJSON(ctx, d.db, b.SnapshotID)
	if err != nil {
		return reportBinding{}, err
	}
	if err := json.Unmarshal(raw, &b.cfg); err != nil {
		return reportBinding{}, errors.New("report: invalid snapshot")
	}
	if b.cfg.Report.MaxPayloadBytes < 1 || b.cfg.Report.EventsPerMinute < 1 || b.cfg.Report.Burst < 1 || b.cfg.Report.InterruptsPerRunDailyQuota < 1 {
		return reportBinding{}, errors.New("report: invalid snapshot")
	}
	switch b.Phase {
	case "running":
		return b, nil
	case "spawning":
		policy, perr := reportRetryPolicy(b.cfg)
		if perr != nil {
			return reportBinding{}, perr
		}
		return reportBinding{}, &ReportNotReadyError{RetryPolicy: policy}
	default:
		// pending/starting/finished/orphaned: phase already passed or not reached.
		return reportBinding{}, ErrReportConflict
	}
}

// assertReportBindingTx is the authoritative in-transaction re-check of the
// binding established by checkReportBinding. It guards against concurrent
// state mutation between the read-only pre-check and the write transaction.
func assertReportBindingTx(ctx context.Context, tx *sql.Tx, cmd ReportSubmitCmd) (string, string, int64, error) {
	var phase string
	var generation int
	var tokenHash, projectID, snapshotID string
	var runVersion int64
	err := tx.QueryRowContext(ctx, `SELECT a.phase,a.generation,c.run_token_hash,r.project_id,r.config_snapshot_id,r.version FROM attempts a JOIN attempt_claims c USING(run_id,attempt_no) JOIN runs r ON r.id=a.run_id WHERE a.run_id=? AND a.attempt_no=?`, cmd.RunID, cmd.AttemptNo).Scan(&phase, &generation, &tokenHash, &projectID, &snapshotID, &runVersion)
	if err != nil {
		return "", "", 0, ErrReportUnauthorized
	}
	if tokenHash != handoffHash(cmd.Token) {
		return "", "", 0, ErrReportUnauthorized
	}
	if generation != cmd.Generation {
		return "", "", 0, ErrReportStale
	}
	if phase != "running" {
		return "", "", 0, ErrReportConflict
	}
	return projectID, snapshotID, runVersion, nil
}

// dedupeKind classifies a two-layer dedupe lookup (report.md §5.2).
const (
	dedupeNone = iota
	dedupeDuplicate
	dedupeConflict
)

// lookupReportDuplicateTx applies both dedupe layers before any token or
// quota is consumed. Layer 1 is the idempotency key (run, attempt, report_key):
// same digest returns the original receipt as a duplicate, a different digest
// is a permanent conflict. Layer 2 is the semantic window: a new key with the
// same (kind, digest) accepted inside dedupe_window also returns the original.
func lookupReportDuplicateTx(ctx context.Context, tx *sql.Tx, cmd ReportSubmitCmd, digest string, dedupeWindow time.Duration) (ReportResult, int) {
	var id, oldDigest, oldEvent string
	err := tx.QueryRowContext(ctx, `SELECT id,payload_digest,event_id FROM report_receipts WHERE run_id=? AND attempt_no=? AND report_key=?`, cmd.RunID, cmd.AttemptNo, cmd.ReportKey).Scan(&id, &oldDigest, &oldEvent)
	if err == nil {
		if oldDigest == digest {
			return ReportResult{Disposition: "duplicate", ReceiptID: id, EventID: oldEvent}, dedupeDuplicate
		}
		return ReportResult{}, dedupeConflict
	}
	if err != sql.ErrNoRows {
		return ReportResult{}, dedupeConflict
	}
	if dedupeWindow > 0 {
		cutoff := cmd.NowMS - dedupeWindow.Milliseconds()
		err = tx.QueryRowContext(ctx, `SELECT id,event_id FROM report_receipts WHERE run_id=? AND attempt_no=? AND report_kind=? AND payload_digest=? AND received_at_ms>=? ORDER BY received_at_ms ASC LIMIT 1`, cmd.RunID, cmd.AttemptNo, cmd.Kind, digest, cutoff).Scan(&id, &oldEvent)
		if err == nil {
			return ReportResult{Disposition: "duplicate", ReceiptID: id, EventID: oldEvent}, dedupeDuplicate
		}
	}
	return ReportResult{}, dedupeNone
}

// reportRetryPolicy derives the closed not_ready policy from the Run's frozen
// config snapshot. The config loader already rejected unrepresentable values,
// so this only fail-closes on a malformed or tampered snapshot.
func reportRetryPolicy(cfg reportRuntimeConfig) (RetryPolicy, error) {
	initial := int(cfg.Report.NotReadyInitialDelay / time.Millisecond)
	maxDelay := int(cfg.Report.NotReadyMaxDelay / time.Millisecond)
	total := int(cfg.Report.NotReadyTotalTimeout / time.Millisecond)
	micros := int(math.Round(cfg.Runtime.RetryMultiplier * 1000000))
	if initial < 1 || maxDelay < initial || total < maxDelay || micros < 1000000 || micros > 10000000 {
		return RetryPolicy{}, errors.New("report: invalid retry policy")
	}
	return RetryPolicy{InitialDelayMS: initial, MultiplierMicros: micros, MaxDelayMS: maxDelay, TotalTimeoutMS: total}, nil
}

func reportSnapshotJSON(ctx context.Context, db *sql.DB, snapshotID string) ([]byte, error) {
	var raw string
	if err := db.QueryRowContext(ctx, `SELECT canonical_json FROM config_snapshots WHERE id=?`, snapshotID).Scan(&raw); err != nil {
		return nil, err
	}
	return []byte(raw), nil
}

// ReportQuotaExhaustionCmd is the committed Report quota fact that may produce
// the Report-only failure_review Interrupt. Callers invoke it only after the
// Report rate-token transaction has consumed its token.
type ReportQuotaExhaustionCmd struct {
	RunID                                   string
	ExpectedRunVersion                      int64
	DailyBucketStartMS                      int64
	DailyBucketEndMS                        int64
	AttentionDailyQuota                     map[InterruptSeverity]int
	DayTimezone                             string
	CriticalWindowMS                        int64
	CriticalTotalLimit, CriticalPerRunLimit int
	DailySummaryAt                          string
	Channels                                []InterruptChannel
	NowMS                                   int64
}

// RecordReportQuotaExhaustion is the production owner for the Report quota
// exhaustion fact. It commits the system security event and its unique
// exhaustion identity before attempting the best-effort EmitInterrupt step.
// A structural emission rejection never rolls back the durable quota fact.
func (d *DB) RecordReportQuotaExhaustion(ctx context.Context, cmd ReportQuotaExhaustionCmd) (Interrupt, error) {
	if cmd.RunID == "" || cmd.ExpectedRunVersion < 1 || cmd.DailyBucketStartMS <= 0 || cmd.DailyBucketEndMS <= cmd.DailyBucketStartMS || cmd.NowMS <= 0 {
		return Interrupt{}, fmt.Errorf("%w: invalid report quota exhaustion", ErrInterruptRejected)
	}
	digest := reportQuotaFailureDigest(cmd.RunID, cmd.DailyBucketStartMS, cmd.DailyBucketEndMS)
	generation := InterruptGeneration{ReportDailyBucketStartMS: cmd.DailyBucketStartMS, ReportDailyBucketEndMS: cmd.DailyBucketEndMS, FailureDigest: digest}
	key, err := interruptGenerationKey(cmd.RunID, InterruptFailureReview, generation)
	if err != nil {
		return Interrupt{}, err
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return Interrupt{}, err
	}
	defer tx.Rollback()
	var eventID string
	if err := tx.QueryRowContext(ctx, `SELECT security_event_id FROM report_quota_exhaustions WHERE run_id=? AND daily_bucket_start_ms=?`, cmd.RunID, cmd.DailyBucketStartMS).Scan(&eventID); err != nil && err != sql.ErrNoRows {
		return Interrupt{}, err
	} else if err == sql.ErrNoRows {
		var status string
		var version int64
		var projectID string
		if err := tx.QueryRowContext(ctx, `SELECT status,version,project_id FROM runs WHERE id=?`, cmd.RunID).Scan(&status, &version, &projectID); err != nil {
			return Interrupt{}, err
		}
		if RunStatus(status) != RunRunning || version != cmd.ExpectedRunVersion {
			return Interrupt{}, ErrRejectedStale
		}
		eventID = newID()
		payload, _ := json.Marshal(map[string]any{"daily_bucket_end_ms": cmd.DailyBucketEndMS, "daily_bucket_start_ms": cmd.DailyBucketStartMS, "failure_class": "report_interrupt_quota_exhausted", "failure_digest": digest, "generation_key": key})
		if _, err := tx.ExecContext(ctx, `INSERT INTO events(id,run_id,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES(?,?,?,'security.report_quota_exhausted','system',1,?,?,?)`, eventID, cmd.RunID, projectID, string(payload), cmd.NowMS, cmd.NowMS); err != nil {
			return Interrupt{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO report_quota_exhaustions(run_id,daily_bucket_start_ms,daily_bucket_end_ms,security_event_id,failure_digest,generation_key,created_at_ms) VALUES(?,?,?,?,?,?,?)`, cmd.RunID, cmd.DailyBucketStartMS, cmd.DailyBucketEndMS, eventID, digest, key, cmd.NowMS); err != nil {
			return Interrupt{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Interrupt{}, err
	}

	generation.SecurityEventID = eventID
	emit := EmitInterruptCmd{
		RunID: cmd.RunID, ExpectedRunVersion: cmd.ExpectedRunVersion,
		Reason: InterruptFailureReview, FailureReviewVariant: FailureReviewReportQuota,
		Facts:      map[string]string{"failure_class": "report_interrupt_quota_exhausted", "failure_evidence_ref": "sift://event/" + eventID, "recommended_action": "hold"},
		Generation: generation, GatePhase: GateNone, GuardrailLevel: GuardrailNone,
		AttentionDailyQuota: cmd.AttentionDailyQuota, DayTimezone: cmd.DayTimezone,
		CriticalWindowMS: cmd.CriticalWindowMS, CriticalTotalLimit: cmd.CriticalTotalLimit, CriticalPerRunLimit: cmd.CriticalPerRunLimit,
		DailySummaryAt: cmd.DailySummaryAt, Channels: cmd.Channels,
		Source: SourceSystem, NowMS: cmd.NowMS,
	}
	if cmd.DailySummaryAt != "" {
		if batchAt, ok := NextDailySummaryAt(cmd.NowMS, cmd.DayTimezone, cmd.DailySummaryAt); ok {
			emit.BatchAtMS = &batchAt
		}
	}
	interrupt, err := d.EmitInterrupt(ctx, emit)
	if errors.Is(err, ErrInterruptRejected) {
		if diagnosticErr := d.recordReportEmissionDiagnostic(ctx, cmd.RunID, eventID, key, cmd.NowMS); diagnosticErr != nil {
			return Interrupt{}, diagnosticErr
		}
	}
	return interrupt, err
}

// recordReportEmissionDiagnostic records the post-exhaustion structural
// rejection under the same generation key as its best-effort emission.
func (d *DB) recordReportEmissionDiagnostic(ctx context.Context, runID, securityEventID, generationKey string, nowMS int64) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing string
	if err := tx.QueryRowContext(ctx, `SELECT event_id FROM report_emission_diagnostics WHERE generation_key=?`, generationKey).Scan(&existing); err == nil {
		return tx.Commit()
	} else if err != sql.ErrNoRows {
		return err
	}
	var projectID string
	if err := tx.QueryRowContext(ctx, `SELECT project_id FROM runs WHERE id=?`, runID).Scan(&projectID); err != nil {
		return err
	}
	eventID := newID()
	payload, _ := json.Marshal(map[string]string{"disposition": "structural_rejected", "generation_key": generationKey, "security_event_id": securityEventID})
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(id,run_id,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES(?,?,?,'security.report_interrupt_rejected','system',1,?,?,?)`, eventID, runID, projectID, string(payload), nowMS, nowMS); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO report_emission_diagnostics(generation_key,run_id,security_event_id,event_id,disposition,created_at_ms) VALUES(?,?,?,?, 'structural_rejected',?)`, generationKey, runID, securityEventID, eventID, nowMS); err != nil {
		if lookupErr := tx.QueryRowContext(ctx, `SELECT event_id FROM report_emission_diagnostics WHERE generation_key=?`, generationKey).Scan(&existing); lookupErr == nil {
			return tx.Commit()
		}
		return err
	}
	return tx.Commit()
}

func reportQuotaFailureDigest(runID string, start, end int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf(`{"daily_bucket_end_ms":%d,"daily_bucket_start_ms":%d,"failure_class":"report_interrupt_quota_exhausted","recommended_action":"hold","run_id":%q}`, end, start, runID)))
	return hex.EncodeToString(sum[:])
}
