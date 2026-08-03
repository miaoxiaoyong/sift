package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// Handoff errors are deliberately distinct so the control-plane boundary can
// preserve the protocol's unauthorized/stale/conflict split.
var (
	ErrHandoffUnauthorized = errors.New("storage: handoff credential rejected")
	ErrHandoffStale        = errors.New("storage: handoff is stale")
	ErrHandoffConflict     = errors.New("storage: handoff conflict")
)

type WrapperIdentity struct {
	PID         int64
	StartedAtMS int64
	Executable  string
	PGID        int64
}
type AgentIdentity struct {
	PID         int64
	StartedAtMS int64
	Executable  string
}
type AcquireLaunchClaim struct {
	RunID, DispatchID, BootstrapNonce, InstanceID, Session string
	AttemptNo, Generation                                  int
	Wrapper                                                WrapperIdentity
	NowMS                                                  int64
}
type PermitSpawn struct {
	RunID, InstanceID, Session, Permit, ControlDigest, ControlNonceHash string
	AttemptNo, Generation                                               int
	Wrapper                                                             WrapperIdentity
	NowMS                                                               int64
}
type StartedClaim struct {
	RunID, InstanceID, Session, Permit, ControlDigest, ResultDigest string
	AttemptNo, Generation                                           int
	Agent                                                           AgentIdentity
	NowMS                                                           int64
}

func handoffHash(v string) string      { s := sha256.Sum256([]byte(v)); return hex.EncodeToString(s[:]) }
func validHandoffSecret(v string) bool { return len(v) == 64 && handoffHash(v) != "" && isLowerHex(v) }
func isLowerHex(v string) bool {
	_, err := hex.DecodeString(v)
	if err != nil {
		return false
	}
	for _, r := range v {
		if r >= 'A' && r <= 'F' {
			return false
		}
	}
	return true
}
func validWrapper(i WrapperIdentity) bool {
	return i.PID > 0 && i.StartedAtMS > 0 && i.PGID > 0 && i.Executable != ""
}
func validAgent(i AgentIdentity) bool { return i.PID > 0 && i.StartedAtMS > 0 && i.Executable != "" }

// AcquireLaunchClaim binds the sole wrapper instance and session, moves the
// attempt from pending to starting, and is idempotent only for the same tuple.
func (d *DB) AcquireLaunchClaim(ctx context.Context, c AcquireLaunchClaim) error {
	if c.RunID == "" || c.AttemptNo < 1 || c.Generation < 1 || !validHandoffSecret(c.BootstrapNonce) || !validHandoffSecret(c.Session) || !validWrapper(c.Wrapper) || c.InstanceID == "" || c.DispatchID == "" || c.NowMS <= 0 {
		return ErrHandoffConflict
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var phase string
	var generation int
	var dispatch, nonce, instance, session, executable sql.NullString
	var pid, started, pgid sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT a.phase,a.generation,c.dispatch_id,c.bootstrap_nonce_hash,c.wrapper_instance_id,c.wrapper_session_hash,a.wrapper_pid,a.wrapper_started_at_ms,a.wrapper_executable,a.wrapper_pgid FROM attempts a JOIN attempt_claims c ON c.run_id=a.run_id AND c.attempt_no=a.attempt_no WHERE a.run_id=? AND a.attempt_no=?`, c.RunID, c.AttemptNo).Scan(&phase, &generation, &dispatch, &nonce, &instance, &session, &pid, &started, &executable, &pgid)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrHandoffStale
	}
	if err != nil {
		return err
	}
	if generation != c.Generation || !dispatch.Valid || dispatch.String != c.DispatchID {
		return ErrHandoffStale
	}
	if !nonce.Valid || nonce.String != handoffHash(c.BootstrapNonce) {
		return ErrHandoffUnauthorized
	}
	if phase == "starting" && instance.Valid && instance.String == c.InstanceID && session.Valid && session.String == handoffHash(c.Session) && pid.Int64 == c.Wrapper.PID && started.Int64 == c.Wrapper.StartedAtMS && executable.String == c.Wrapper.Executable && pgid.Int64 == c.Wrapper.PGID {
		return tx.Commit()
	}
	if phase != "pending" {
		return ErrHandoffConflict
	}
	if instance.Valid || session.Valid {
		return ErrHandoffConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE attempts SET phase='starting',wrapper_pid=?,wrapper_started_at_ms=?,wrapper_executable=?,wrapper_pgid=?,wrapper_instance_id=?,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND phase='pending'`, c.Wrapper.PID, c.Wrapper.StartedAtMS, c.Wrapper.Executable, c.Wrapper.PGID, c.InstanceID, c.NowMS, c.RunID, c.AttemptNo); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE attempt_claims SET wrapper_instance_id=?,wrapper_session_hash=?,acquired_at_ms=?,updated_at_ms=? WHERE run_id=? AND attempt_no=?`, c.InstanceID, handoffHash(c.Session), c.NowMS, c.NowMS, c.RunID, c.AttemptNo); err != nil {
		return err
	}
	if err := appendHandoffEvent(ctx, tx, c.RunID, c.AttemptNo, "attempt.acquired", c.NowMS); err != nil {
		return err
	}
	// acquire is the operation's single completion point. The worker only
	// prepares/file-spawns; a wrapper that never acquires must remain visible to
	// recovery instead of being incorrectly marked launched by the worker.
	key := LaunchOperationKey(c.RunID, c.AttemptNo, c.Generation)
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_attempt_results(attempt_id,finished_at_ms,outcome) SELECT oa.id,?,'success' FROM outbox_attempts oa JOIN outbox_operations o ON o.id=oa.operation_id WHERE o.operation_key=? AND o.kind='launch_agent' AND o.state='executing' AND oa.attempt_no=o.attempt_count`, c.NowMS, key); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE outbox_operations SET state='succeeded',lease_owner=NULL,lease_expires_at_ms=NULL,completed_at_ms=?,updated_at_ms=? WHERE operation_key=? AND kind='launch_agent' AND state='executing'`, c.NowMS, c.NowMS, key); err != nil {
		return err
	}
	return tx.Commit()
}

// PermitSpawn is the sole transition which grants the irrevocable one-shot
// spawn permit. No later call can replace that permit or owner.
func (d *DB) PermitSpawn(ctx context.Context, c PermitSpawn) error {
	if c.RunID == "" || c.AttemptNo < 1 || c.Generation < 1 || !validHandoffSecret(c.Session) || !validHandoffSecret(c.Permit) || !validWrapper(c.Wrapper) || c.InstanceID == "" || !isLowerHex(c.ControlDigest) || len(c.ControlDigest) != 64 || !isLowerHex(c.ControlNonceHash) || len(c.ControlNonceHash) != 64 || c.NowMS <= 0 {
		return ErrHandoffConflict
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var phase string
	var gen int
	var instance, session, permit sql.NullString
	var pid, started, pgid sql.NullInt64
	var executable sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT a.phase,a.generation,c.wrapper_instance_id,c.wrapper_session_hash,c.spawn_permit_hash,a.wrapper_pid,a.wrapper_started_at_ms,a.wrapper_executable,a.wrapper_pgid FROM attempts a JOIN attempt_claims c ON c.run_id=a.run_id AND c.attempt_no=a.attempt_no WHERE a.run_id=? AND a.attempt_no=?`, c.RunID, c.AttemptNo).Scan(&phase, &gen, &instance, &session, &permit, &pid, &started, &executable, &pgid)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrHandoffStale
	}
	if err != nil {
		return err
	}
	if gen != c.Generation {
		return ErrHandoffStale
	}
	if !instance.Valid || instance.String != c.InstanceID || !session.Valid || session.String != handoffHash(c.Session) {
		return ErrHandoffUnauthorized
	}
	if pid.Int64 != c.Wrapper.PID || started.Int64 != c.Wrapper.StartedAtMS || executable.String != c.Wrapper.Executable || pgid.Int64 != c.Wrapper.PGID {
		return ErrHandoffConflict
	}
	if phase == "spawning" && permit.Valid && permit.String == handoffHash(c.Permit) {
		return tx.Commit()
	}
	if phase != "starting" || permit.Valid {
		return ErrHandoffConflict
	}
	// control digest is evidence supplied by the wrapper; filesystem validation
	// belongs to the control-plane file boundary, not this DB-only port.
	if _, err = tx.ExecContext(ctx, `UPDATE attempt_claims SET spawn_permit_hash=?,permit_issued_at_ms=?,updated_at_ms=? WHERE run_id=? AND attempt_no=?`, handoffHash(c.Permit), c.NowMS, c.NowMS, c.RunID, c.AttemptNo); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE attempts SET phase='spawning',control_nonce_hash=?,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND phase='starting'`, c.ControlNonceHash, c.NowMS, c.RunID, c.AttemptNo); err != nil {
		return err
	}
	if err := appendHandoffEvent(ctx, tx, c.RunID, c.AttemptNo, "attempt.spawn_permitted", c.NowMS); err != nil {
		return err
	}
	return tx.Commit()
}

// ConfirmStarted records verifiable Agent identity and advances the Run only
// after a previously issued permit is presented. A byte-for-byte replay is a
// no-op; a different fact can never overwrite the original identity.
func (d *DB) ConfirmStarted(ctx context.Context, c StartedClaim) (string, error) {
	if c.RunID == "" || c.AttemptNo < 1 || c.Generation < 1 || !validHandoffSecret(c.Session) || !validHandoffSecret(c.Permit) || !validAgent(c.Agent) || c.InstanceID == "" || len(c.ControlDigest) != 64 || !isLowerHex(c.ControlDigest) || c.NowMS <= 0 {
		return "", ErrHandoffConflict
	}
	var generation int
	var phase string
	var existingPID, existingStarted sql.NullInt64
	var existingExecutable, instance, session, permit sql.NullString
	err := d.db.QueryRowContext(ctx, `SELECT a.generation,a.phase,a.agent_pid,a.agent_started_at_ms,a.agent_executable,c.wrapper_instance_id,c.wrapper_session_hash,c.spawn_permit_hash FROM attempts a JOIN attempt_claims c ON c.run_id=a.run_id AND c.attempt_no=a.attempt_no WHERE a.run_id=? AND a.attempt_no=?`, c.RunID, c.AttemptNo).Scan(&generation, &phase, &existingPID, &existingStarted, &existingExecutable, &instance, &session, &permit)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrHandoffStale
	}
	if err != nil {
		return "", err
	}
	if generation != c.Generation {
		return "", ErrHandoffStale
	}
	if !instance.Valid || instance.String != c.InstanceID || !session.Valid || session.String != handoffHash(c.Session) || !permit.Valid || permit.String != handoffHash(c.Permit) {
		return "", ErrHandoffUnauthorized
	}
	if phase == "running" && existingPID.Valid && existingPID.Int64 == c.Agent.PID && existingStarted.Int64 == c.Agent.StartedAtMS && existingExecutable.String == c.Agent.Executable {
		return AttemptRaceDuplicate, nil
	}
	factKey := fmt.Sprintf("started:%s:%d:%d:%d:%d:%s", c.RunID, c.AttemptNo, c.Generation, c.Agent.PID, c.Agent.StartedAtMS, c.Agent.Executable)
	disposition, err := d.ResolveAttemptRace(ctx, AttemptRaceCommand{RunID: c.RunID, AttemptNo: c.AttemptNo, ExpectedGeneration: c.Generation, FactKey: factKey, NowMS: c.NowMS, Agent: &c.Agent})
	if err != nil {
		return "", err
	}
	return disposition, nil
}
func appendHandoffEvent(ctx context.Context, tx *sql.Tx, runID string, attemptNo int, typ string, now int64) error {
	p, _ := json.Marshal(map[string]any{"attempt_no": attemptNo})
	_, err := tx.ExecContext(ctx, `INSERT INTO events(id,run_id,attempt_no,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES (?, ?, ?, ?, 'agent', 1, ?, ?, ?)`, newID(), runID, attemptNo, typ, string(p), now, now)
	return err
}
func (d *DB) HandoffClaimHash(secret string) string { return handoffHash(secret) }

// AttemptRaceCommand normalizes the four inputs which can race with a frozen
// attempt: wrapper started, recovery started, result evidence, and a human
// terminal decision.  All of them must use ResolveAttemptRace.
type AttemptResult struct {
	Agent         AgentIdentity
	ExitCode      *int
	Signal        string
	FinalHeadSHA  string
	Digest        string
	FinishedAtMS  int64
	FailureReason string
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
			if _, err = tx.ExecContext(ctx, `UPDATE attempts SET phase='running',agent_pid=?,agent_started_at_ms=?,agent_executable=?,heartbeat_at_ms=?,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND generation=? AND phase='spawning'`, cmd.Agent.PID, cmd.Agent.StartedAtMS, cmd.Agent.Executable, cmd.NowMS, cmd.NowMS, cmd.RunID, cmd.AttemptNo, cmd.ExpectedGeneration); err != nil {
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
			if _, err = tx.ExecContext(ctx, `UPDATE attempts SET result_exit_code=?,result_signal=?,result_failure_reason=?,final_head_sha=?,result_digest=?,result_observed_at_ms=?,finished_at_ms=?,phase='finished',updated_at_ms=? WHERE run_id=? AND attempt_no=? AND generation=? AND phase IN ('spawning','running')`, nullableInt(cmd.Result.ExitCode), nullableString(cmd.Result.Signal), nullableString(cmd.Result.FailureReason), nullableString(cmd.Result.FinalHeadSHA), cmd.Result.Digest, cmd.Result.FinishedAtMS, cmd.Result.FinishedAtMS, cmd.NowMS, cmd.RunID, cmd.AttemptNo, cmd.ExpectedGeneration); err != nil {
				return "", err
			}
		} else {
			if _, err = tx.ExecContext(ctx, `UPDATE attempts SET result_exit_code=?,result_signal=?,result_failure_reason=?,final_head_sha=?,result_digest=?,result_observed_at_ms=?,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND generation=?`, nullableInt(cmd.Result.ExitCode), nullableString(cmd.Result.Signal), nullableString(cmd.Result.FailureReason), nullableString(cmd.Result.FinalHeadSHA), cmd.Result.Digest, cmd.Result.FinishedAtMS, cmd.NowMS, cmd.RunID, cmd.AttemptNo, cmd.ExpectedGeneration); err != nil {
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
	var interruptID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM interrupts WHERE run_id=? AND attempt_no=? AND reason='startup_stall' AND status='open' ORDER BY created_at_ms DESC LIMIT 1`, runID, attemptNo).Scan(&interruptID); err == sql.ErrNoRows {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE interrupts SET status='closed',close_reason=?,closed_at_ms=?,updated_at_ms=? WHERE id=? AND status='open'`, reason, nowMS, nowMS, interruptID); err != nil {
		return err
	}
	return excludeStaleBatchMembersTx(ctx, tx, interruptID, nowMS)
}
