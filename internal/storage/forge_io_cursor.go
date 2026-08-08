package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

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
