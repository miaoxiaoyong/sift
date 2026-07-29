package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// AttemptRaceCommand normalizes the four inputs which can race with a frozen
// attempt: wrapper started, recovery started, result evidence, and a human
// terminal decision.  All of them must use ResolveAttemptRace.
type AttemptResult struct {
	Agent        AgentIdentity
	ExitCode     *int
	Signal       string
	FinalHeadSHA string
	Digest       string
	FinishedAtMS int64
}

type AttemptRaceCommand struct {
	RunID              string
	AttemptNo          int
	ExpectedGeneration int
	ExpectedRunVersion int64 // zero accepts the version read in this transaction.
	FactKey            string
	NowMS              int64
	Agent              *AgentIdentity
	Result             *AttemptResult
	Reject             bool
}

const (
	AttemptRaceRunning              = "running"
	AttemptRaceSupersededByFact     = "superseded_by_fact"
	AttemptRaceSupersededByDecision = "superseded_by_decision"
	AttemptRaceDecisionApplied      = "decision_applied"
	AttemptRaceDuplicate            = "duplicate"
)

// ResolveAttemptRace is the sole linearization point for execution facts and
// attempt_resolution. It intentionally does not release isolation: a started
// fact proves the execution body exists, not that it has disappeared.
func (d *DB) ResolveAttemptRace(ctx context.Context, cmd AttemptRaceCommand) (string, error) {
	if cmd.Result != nil && (cmd.Agent == nil || cmd.Result.Agent != *cmd.Agent || cmd.Result.Digest == "" || cmd.Result.FinishedAtMS <= 0 || (cmd.Result.ExitCode == nil) == (cmd.Result.Signal == "") || (cmd.Result.ExitCode != nil && (*cmd.Result.ExitCode < 0 || *cmd.Result.ExitCode > 255))) {
		return "", ErrHandoffConflict
	}
	if cmd.RunID == "" || cmd.AttemptNo < 1 || cmd.ExpectedGeneration < 1 || cmd.FactKey == "" || cmd.NowMS <= 0 || (cmd.Agent == nil && !cmd.Reject) || (cmd.Agent != nil && cmd.Reject) || (cmd.Agent != nil && !validAgent(*cmd.Agent)) {
		return "", ErrHandoffConflict
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var phase, resolution, status string
	var generation int
	var version int64
	var pid, started sql.NullInt64
	var executable sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT a.phase,COALESCE(a.attempt_resolution,''),a.generation,r.status,r.version,a.agent_pid,a.agent_started_at_ms,a.agent_executable
		FROM attempts a JOIN runs r ON r.id=a.run_id WHERE a.run_id=? AND a.attempt_no=?`, cmd.RunID, cmd.AttemptNo).
		Scan(&phase, &resolution, &generation, &status, &version, &pid, &started, &executable)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrHandoffStale
	}
	if err != nil {
		return "", err
	}
	if generation != cmd.ExpectedGeneration || (cmd.ExpectedRunVersion != 0 && version != cmd.ExpectedRunVersion) {
		return "", ErrHandoffStale
	}
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT payload_json FROM events WHERE idempotency_key=?`, "attempt-race:"+cmd.FactKey).Scan(&existing)
	if err == nil {
		var receipt struct {
			Disposition string `json:"disposition"`
		}
		if json.Unmarshal([]byte(existing), &receipt) == nil && receipt.Disposition != "" {
			return receipt.Disposition, tx.Commit()
		}
		return AttemptRaceDuplicate, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	disposition := AttemptRaceRunning
	if cmd.Reject {
		if resolution != "" {
			disposition = AttemptRaceDecisionApplied
		} else {
			if _, err = tx.ExecContext(ctx, `UPDATE attempts SET attempt_resolution='reject',resolution_at_ms=?,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND generation=? AND attempt_resolution IS NULL`, cmd.NowMS, cmd.NowMS, cmd.RunID, cmd.AttemptNo, cmd.ExpectedGeneration); err != nil {
				return "", err
			}
			if RunStatus(status) != RunFailed {
				if err = d.transition(ctx, tx, cmd.RunID, version, DomainCommand{To: RunFailed, Source: SourceOperator, FailureReason: "human_reject", OccurredAtMS: cmd.NowMS}); err != nil {
					return "", err
				}
			}
			if err = closeStartupStallTx(ctx, tx, cmd.RunID, cmd.AttemptNo, "responded", cmd.NowMS); err != nil {
				return "", err
			}
			disposition = AttemptRaceDecisionApplied
		}
	} else if resolution != "" {
		// Preserve the only useful late fact (the exact identity) so the
		// termination port can terminate it, while never reviving the old Run.
		if !pid.Valid {
			if _, err = tx.ExecContext(ctx, `UPDATE attempts SET agent_pid=?,agent_started_at_ms=?,agent_executable=?,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND generation=?`, cmd.Agent.PID, cmd.Agent.StartedAtMS, cmd.Agent.Executable, cmd.NowMS, cmd.RunID, cmd.AttemptNo, cmd.ExpectedGeneration); err != nil {
				return "", err
			}
		} else if pid.Int64 != cmd.Agent.PID || started.Int64 != cmd.Agent.StartedAtMS || executable.String != cmd.Agent.Executable {
			return "", ErrHandoffConflict
		}
		disposition = AttemptRaceSupersededByDecision
	} else {
		if phase == "running" {
			if !pid.Valid || pid.Int64 != cmd.Agent.PID || started.Int64 != cmd.Agent.StartedAtMS || executable.String != cmd.Agent.Executable {
				return "", ErrHandoffConflict
			}
			disposition = AttemptRaceDuplicate
		} else if phase != "spawning" {
			return "", ErrHandoffConflict
		} else {
			if _, err = tx.ExecContext(ctx, `UPDATE attempts SET phase='running',agent_pid=?,agent_started_at_ms=?,agent_executable=?,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND generation=? AND phase='spawning'`, cmd.Agent.PID, cmd.Agent.StartedAtMS, cmd.Agent.Executable, cmd.NowMS, cmd.RunID, cmd.AttemptNo, cmd.ExpectedGeneration); err != nil {
				return "", err
			}
			if RunStatus(status) != RunRunning {
				if err = d.transition(ctx, tx, cmd.RunID, version, DomainCommand{To: RunRunning, Source: SourceAgent, Actor: "wrapper", OccurredAtMS: cmd.NowMS}); err != nil {
					return "", err
				}
			}
			if _, err = tx.ExecContext(ctx, `UPDATE attempt_claims SET started_confirmed_at_ms=COALESCE(started_confirmed_at_ms,?),updated_at_ms=? WHERE run_id=? AND attempt_no=?`, cmd.NowMS, cmd.NowMS, cmd.RunID, cmd.AttemptNo); err != nil {
				return "", err
			}
			if status == string(RunWaitingHuman) {
				if err = closeStartupStallTx(ctx, tx, cmd.RunID, cmd.AttemptNo, "superseded_by_fact", cmd.NowMS); err != nil {
					return "", err
				}
				disposition = AttemptRaceSupersededByFact
			}
		}
	}
	if cmd.Result != nil {
		if resolution == "" {
			if _, err = tx.ExecContext(ctx, `UPDATE attempts SET result_exit_code=?,result_signal=?,final_head_sha=?,result_digest=?,result_observed_at_ms=?,finished_at_ms=?,phase='finished',updated_at_ms=? WHERE run_id=? AND attempt_no=? AND generation=? AND phase IN ('spawning','running')`, nullableInt(cmd.Result.ExitCode), nullableString(cmd.Result.Signal), nullableString(cmd.Result.FinalHeadSHA), cmd.Result.Digest, cmd.Result.FinishedAtMS, cmd.Result.FinishedAtMS, cmd.NowMS, cmd.RunID, cmd.AttemptNo, cmd.ExpectedGeneration); err != nil {
				return "", err
			}
		} else {
			if _, err = tx.ExecContext(ctx, `UPDATE attempts SET result_exit_code=?,result_signal=?,final_head_sha=?,result_digest=?,result_observed_at_ms=?,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND generation=?`, nullableInt(cmd.Result.ExitCode), nullableString(cmd.Result.Signal), nullableString(cmd.Result.FinalHeadSHA), cmd.Result.Digest, cmd.Result.FinishedAtMS, cmd.NowMS, cmd.RunID, cmd.AttemptNo, cmd.ExpectedGeneration); err != nil {
				return "", err
			}
		}
	}
	payload, _ := json.Marshal(map[string]string{"disposition": disposition, "fact_key": cmd.FactKey})
	if _, err = tx.ExecContext(ctx, `INSERT INTO events(id,run_id,attempt_no,type,source,payload_schema_version,payload_json,idempotency_key,occurred_at_ms,recorded_at_ms) VALUES (?, ?, ?, 'attempt.race_resolved', 'agent', 1, ?, ?, ?, ?)`, newID(), cmd.RunID, cmd.AttemptNo, string(payload), "attempt-race:"+cmd.FactKey, cmd.NowMS, cmd.NowMS); err != nil {
		return "", fmt.Errorf("storage: record attempt race: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return disposition, nil
}

func nullableInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func closeStartupStallTx(ctx context.Context, tx *sql.Tx, runID string, attemptNo int, reason string, nowMS int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE interrupts SET status='closed',close_reason=?,closed_at_ms=?,updated_at_ms=? WHERE id=(SELECT id FROM interrupts WHERE run_id=? AND attempt_no=? AND reason='startup_stall' AND status='open' ORDER BY created_at_ms DESC LIMIT 1)`, reason, nowMS, nowMS, runID, attemptNo)
	return err
}
