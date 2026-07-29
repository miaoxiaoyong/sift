package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	RunID, InstanceID, Session, Permit, ControlDigest string
	AttemptNo, Generation                             int
	Wrapper                                           WrapperIdentity
	NowMS                                             int64
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
	return tx.Commit()
}

// PermitSpawn is the sole transition which grants the irrevocable one-shot
// spawn permit. No later call can replace that permit or owner.
func (d *DB) PermitSpawn(ctx context.Context, c PermitSpawn) error {
	if c.RunID == "" || c.AttemptNo < 1 || c.Generation < 1 || !validHandoffSecret(c.Session) || !validHandoffSecret(c.Permit) || !validWrapper(c.Wrapper) || c.InstanceID == "" || !isLowerHex(c.ControlDigest) || len(c.ControlDigest) != 64 || c.NowMS <= 0 {
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
	if _, err = tx.ExecContext(ctx, `UPDATE attempts SET phase='spawning',updated_at_ms=? WHERE run_id=? AND attempt_no=? AND phase='starting'`, c.NowMS, c.RunID, c.AttemptNo); err != nil {
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
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var phase, status string
	var gen int
	var version int64
	var instance, session, permit sql.NullString
	var pid, started sql.NullInt64
	var executable sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT a.phase,a.generation,r.status,r.version,c.wrapper_instance_id,c.wrapper_session_hash,c.spawn_permit_hash,a.agent_pid,a.agent_started_at_ms,a.agent_executable FROM attempts a JOIN runs r ON r.id=a.run_id JOIN attempt_claims c ON c.run_id=a.run_id AND c.attempt_no=a.attempt_no WHERE a.run_id=? AND a.attempt_no=?`, c.RunID, c.AttemptNo).Scan(&phase, &gen, &status, &version, &instance, &session, &permit, &pid, &started, &executable)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrHandoffStale
	}
	if err != nil {
		return "", err
	}
	if gen != c.Generation {
		return "", ErrHandoffStale
	}
	if !instance.Valid || instance.String != c.InstanceID || !session.Valid || session.String != handoffHash(c.Session) || !permit.Valid || permit.String != handoffHash(c.Permit) {
		return "", ErrHandoffUnauthorized
	}
	if phase == "running" && pid.Valid && pid.Int64 == c.Agent.PID && started.Int64 == c.Agent.StartedAtMS && executable.String == c.Agent.Executable {
		return "duplicate", tx.Commit()
	}
	if phase != "spawning" {
		return "", ErrHandoffConflict
	}
	if status != "queued" && status != "waiting_human" {
		return "", ErrHandoffConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE attempts SET phase='running',agent_pid=?,agent_started_at_ms=?,agent_executable=?,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND phase='spawning'`, c.Agent.PID, c.Agent.StartedAtMS, c.Agent.Executable, c.NowMS, c.RunID, c.AttemptNo); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE attempt_claims SET started_confirmed_at_ms=?,updated_at_ms=? WHERE run_id=? AND attempt_no=?`, c.NowMS, c.NowMS, c.RunID, c.AttemptNo); err != nil {
		return "", err
	}
	if err = d.transition(ctx, tx, c.RunID, version, DomainCommand{To: RunRunning, Source: SourceAgent, Actor: "wrapper", OccurredAtMS: c.NowMS}); err != nil {
		return "", err
	}
	if err = appendHandoffEvent(ctx, tx, c.RunID, c.AttemptNo, "attempt.started", c.NowMS); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	if status == "waiting_human" {
		return "superseded_by_fact", nil
	}
	return "running", nil
}
func appendHandoffEvent(ctx context.Context, tx *sql.Tx, runID string, attemptNo int, typ string, now int64) error {
	p, _ := json.Marshal(map[string]any{"attempt_no": attemptNo})
	_, err := tx.ExecContext(ctx, `INSERT INTO events(id,run_id,attempt_no,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES (?, ?, ?, ?, 'agent', 1, ?, ?, ?)`, newID(), runID, attemptNo, typ, string(p), now, now)
	return err
}
func (d *DB) HandoffClaimHash(secret string) string { return handoffHash(secret) }
