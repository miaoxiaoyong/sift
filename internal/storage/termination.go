package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// TerminationSource distinguishes the three callers which share controlled
// termination but have different safe outcomes after absence is proved.
type TerminationSource string

const (
	TerminationRecovery TerminationSource = "recovery"
	TerminationRetry    TerminationSource = "retry"
	TerminationKill     TerminationSource = "kill"
)

type RecordTerminationObservationCmd struct {
	RunID, Evidence, DiagnosticRef string
	AttemptNo                      int
	ExpectedRunVersion             int64
	ExpectedGeneration             int
	Source                         TerminationSource
	Absent                         bool
	DiagnosticCause                string
	NowMS                          int64
	AttentionDailyQuota            map[InterruptSeverity]int
	DayTimezone                    string
	DailySummaryAt                 string
	CriticalWindowMS               int64
	CriticalTotalLimit             int
	CriticalPerRunLimit            int
	Channels                       []InterruptChannel
}

// RecordTerminationObservation is the persistence half of the shared
// identity→signal→absence protocol. Runtime performs OS observation and
// signalling outside a transaction; this method records only the resulting
// fact. Absence releases isolation and chooses an outcome by source. Anything
// else freezes the attempt and emits the sole startup_stall interrupt.
func (d *DB) RecordTerminationObservation(ctx context.Context, cmd RecordTerminationObservationCmd) (Run, error) {
	if cmd.RunID == "" || cmd.AttemptNo < 1 || cmd.ExpectedRunVersion < 1 || cmd.ExpectedGeneration < 1 || cmd.NowMS <= 0 || (cmd.Source != TerminationRecovery && cmd.Source != TerminationRetry && cmd.Source != TerminationKill) {
		return Run{}, errors.New("storage: invalid termination observation")
	}
	if !cmd.Absent {
		if cmd.DiagnosticCause != "process_identity_unknown" && cmd.DiagnosticCause != "termination_unconfirmed" && cmd.DiagnosticCause != "process_group_unverified" {
			return Run{}, errors.New("storage: invalid termination diagnostic cause")
		}
		var worktree string
		if err := d.db.QueryRowContext(ctx, `SELECT worktree_path FROM attempts WHERE run_id=? AND attempt_no=? AND generation=?`, cmd.RunID, cmd.AttemptNo, cmd.ExpectedGeneration).Scan(&worktree); err != nil {
			return Run{}, err
		}
		attempt := cmd.AttemptNo
		_, err := d.EmitInterrupt(ctx, EmitInterruptCmd{
			RunID: cmd.RunID, ExpectedRunVersion: cmd.ExpectedRunVersion, AttemptNo: &attempt, Reason: InterruptStartupStall,
			Facts:      map[string]string{"attempt_no": fmt.Sprint(attempt), "generation": fmt.Sprint(cmd.ExpectedGeneration), "diagnostic_cause": cmd.DiagnosticCause, "isolation_consequence": "worktree 保持隔离", "recommended_action": "retry", "attempt_diagnostic_ref": nonEmptyRef(cmd.DiagnosticRef, worktree), "worktree_ref": worktree},
			Generation: InterruptGeneration{AttemptNo: attempt, Generation: cmd.ExpectedGeneration}, GatePhase: GateNone, GuardrailLevel: GuardrailNone,
			AttentionDailyQuota: cmd.AttentionDailyQuota, DayTimezone: cmd.DayTimezone, DailySummaryAt: cmd.DailySummaryAt, CriticalWindowMS: cmd.CriticalWindowMS, CriticalTotalLimit: cmd.CriticalTotalLimit, CriticalPerRunLimit: cmd.CriticalPerRunLimit, Channels: cmd.Channels, Source: terminationEventSource(cmd.Source), NowMS: cmd.NowMS,
		})
		if err != nil {
			return Run{}, err
		}
		return d.Run(ctx, cmd.RunID)
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	var status string
	var version int64
	var maxAttempts, retryCount int
	if err := tx.QueryRowContext(ctx, `SELECT status,version,max_attempts,retry_count FROM runs WHERE id=?`, cmd.RunID).Scan(&status, &version, &maxAttempts, &retryCount); err != nil {
		return Run{}, err
	}
	if version != cmd.ExpectedRunVersion {
		return Run{}, ErrRejectedStale
	}
	var phase, isolation string
	if err := tx.QueryRowContext(ctx, `SELECT phase,isolation_state FROM attempts WHERE run_id=? AND attempt_no=? AND generation=?`, cmd.RunID, cmd.AttemptNo, cmd.ExpectedGeneration).Scan(&phase, &isolation); err != nil {
		return Run{}, err
	}
	evidence, _ := json.Marshal(map[string]string{"evidence": cmd.Evidence, "source": string(cmd.Source)})
	eventID := newID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (id,run_id,attempt_no,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES (?,?,?,'termination.absence_confirmed',?,1,?,?,?)`, eventID, cmd.RunID, cmd.AttemptNo, terminationEventSource(cmd.Source), string(evidence), cmd.NowMS, cmd.NowMS); err != nil {
		return Run{}, err
	}
	if cmd.Source == TerminationRetry {
		if _, err := tx.ExecContext(ctx, `UPDATE attempts SET attempt_resolution='retry_after_absence',resolution_at_ms=?,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND generation=? AND attempt_resolution IS NULL`, cmd.NowMS, cmd.NowMS, cmd.RunID, cmd.AttemptNo, cmd.ExpectedGeneration); err != nil {
			return Run{}, err
		}
	}
	if isolation == "frozen" {
		if _, err := tx.ExecContext(ctx, `UPDATE attempts SET isolation_state='none',isolation_released_at_ms=?,isolation_release_event_id=?,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND generation=? AND isolation_state='frozen'`, cmd.NowMS, eventID, cmd.NowMS, cmd.RunID, cmd.AttemptNo, cmd.ExpectedGeneration); err != nil {
			return Run{}, err
		}
	}
	if phase != "finished" && phase != "orphaned" {
		if _, err := tx.ExecContext(ctx, `UPDATE attempts SET phase='orphaned',finished_at_ms=?,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND generation=?`, cmd.NowMS, cmd.NowMS, cmd.RunID, cmd.AttemptNo, cmd.ExpectedGeneration); err != nil {
			return Run{}, err
		}
	}
	// A confirmed-absent attempt cannot retain a dispatchable launch operation.
	// Normally claim.acquire already completed it; this CAS also closes the
	// pre-acquire crash/retry path before a successor is enqueued.
	if _, err := tx.ExecContext(ctx, `UPDATE outbox_operations SET state='stale',lease_owner=NULL,lease_expires_at_ms=NULL,completed_at_ms=?,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND kind='launch_agent' AND state IN ('pending','executing')`, cmd.NowMS, cmd.NowMS, cmd.RunID, cmd.AttemptNo); err != nil {
		return Run{}, err
	}
	if cmd.Source == TerminationKill || retryCount+1 >= maxAttempts {
		reason := "operator_kill"
		if cmd.Source != TerminationKill {
			reason = "attempts_exhausted"
		}
		if err := d.transition(ctx, tx, cmd.RunID, version, DomainCommand{To: RunFailed, Source: terminationEventSource(cmd.Source), FailureReason: reason, OccurredAtMS: cmd.NowMS}); err != nil {
			return Run{}, err
		}
	} else {
		newNo := cmd.AttemptNo + 1
		if _, err := tx.ExecContext(ctx, `INSERT INTO attempts (run_id,attempt_no,phase,generation,backend,agent_id,task_spec_snapshot_id,worktree_path,branch_name,base_ref,base_sha,isolation_state,created_at_ms,updated_at_ms) SELECT run_id,?,'pending',1,backend,agent_id,task_spec_snapshot_id,worktree_path,branch_name,base_ref,base_sha,'none',?,? FROM attempts WHERE run_id=? AND attempt_no=?`, newNo, cmd.NowMS, cmd.NowMS, cmd.RunID, cmd.AttemptNo); err != nil {
			return Run{}, err
		}
		key := LaunchOperationKey(cmd.RunID, newNo, 1)
		if _, err := tx.ExecContext(ctx, `INSERT INTO attempt_claims (run_id,attempt_no,generation,launch_operation_key,created_at_ms,updated_at_ms) VALUES (?,?,1,?,?,?)`, cmd.RunID, newNo, key, cmd.NowMS, cmd.NowMS); err != nil {
			return Run{}, err
		}
		payload := []byte(`{"schema_version":1}`)
		if err := insertOperation(ctx, tx, Operation{Key: key, Kind: OperationLaunchAgent, Payload: payload, RunID: cmd.RunID, AttemptNo: intPtr(newNo)}, cmd.RunID, eventID, cmd.NowMS); err != nil {
			return Run{}, err
		}
		if RunStatus(status) != RunWaitingHuman && RunStatus(status) != RunFailed {
			if err := d.transition(ctx, tx, cmd.RunID, version, DomainCommand{To: RunWaitingHuman, Source: terminationEventSource(cmd.Source), OccurredAtMS: cmd.NowMS}); err != nil {
				return Run{}, err
			}
			version++
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET retry_count=retry_count+1 WHERE id=? AND version=?`, cmd.RunID, version); err != nil {
			return Run{}, err
		}
		if err := d.transition(ctx, tx, cmd.RunID, version, DomainCommand{To: RunQueued, Source: terminationEventSource(cmd.Source), OccurredAtMS: cmd.NowMS}); err != nil {
			return Run{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Run{}, err
	}
	d.wakeOutbox()
	return d.Run(ctx, cmd.RunID)
}

func terminationEventSource(s TerminationSource) EventSource {
	if s == TerminationRecovery {
		return SourceRecovery
	}
	return SourceOperator
}
func nonEmptyRef(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}
func intPtr(v int) *int { return &v }
