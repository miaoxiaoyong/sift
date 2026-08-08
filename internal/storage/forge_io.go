package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
