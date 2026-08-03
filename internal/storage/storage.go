// Package storage owns sift.db: the single SQLite database that carries
// Sift's current projections, attempt lifecycle, append-only event stream,
// intake projections, transactional outbox, budgets, Brain call/attempt
// records, Gate snapshots, calibration and Ledger data.
//
// The package implements the opening contract of specs/storage.md §2:
//
//   - modernc.org/sqlite, pure Go, CGO_ENABLED=0; no ORM, no Postgres
//     abstraction layer (DESIGN §7).
//   - The write pool is pinned to MaxOpenConns=1 so driver-internal
//     concurrency can never produce SQLITE_BUSY; busy_timeout is a safety
//     net, not the mechanism.
//   - Every connection runs with foreign_keys=ON, busy_timeout=5000,
//     synchronous=FULL, temp_store=MEMORY; the write connection additionally
//     runs journal_mode=WAL and wal_autocheckpoint=1000. Each critical
//     PRAGMA is verified after opening and any mismatch refuses startup.
//   - The database file is mode 0600 and its parent directory 0700.
//
// Forward-only migrations (specs/storage.md §3) run during Open. A database
// whose schema version is newer than this binary supports is refused; so is
// any checksum drift between an applied migration record and the embedded
// migration file.
//
// Time is injected by the caller (OpenConfig.Now); nothing in this package
// reads the system clock (storage invariant §1.7). Write families (the
// restricted ports of specs/storage.md §11) land on top of this foundation
// in later slices; this package deliberately exposes no business update or
// delete entry point.

package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	sqlite "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func init() {
	_ = sqlite.RegisterDeterministicScalarFunction("sift_sha256", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		var input []byte
		switch v := args[0].(type) {
		case string:
			input = []byte(v)
		case []byte:
			input = v
		default:
			return nil, fmt.Errorf("sift_sha256: invalid input")
		}
		sum := sha256.Sum256(input)
		return sum[:], nil
	})
}

// Opening contract values (specs/storage.md §2). These are verified after
// every open, not merely requested.
const (
	busyTimeoutMS     = 5000
	synchronousFull   = 2 // PRAGMA synchronous = FULL
	tempStoreMemory   = 2 // PRAGMA temp_store = MEMORY
	walAutocheckpoint = 1000
	journalModeWAL    = "wal"

	dbFileMode os.FileMode = 0o600
	dbDirMode  os.FileMode = 0o700
)

// OpenConfig carries the caller-supplied facts Open needs.
type OpenConfig struct {
	// Path is the sift.db file path, e.g. $SIFT_HOME/sift.db.
	Path string
	// BinaryVersion is recorded on every applied schema_migrations row.
	BinaryVersion string
	// Now stamps applied_at_ms for migrations applied during this Open.
	// Storage logic never reads the system clock (storage.md §1 invariant 7).
	Now time.Time
}

// DB is the verified, migrated write handle for sift.db. The embedded pool is
// pinned to a single connection.
type DB struct {
	db   *sql.DB
	path string

	wakeupMu           sync.RWMutex
	outboxWakeup       func()
	interruptT4        InterruptT4Caller
	interruptT6        InterruptT6Caller
	gateReEvalIntr     GateReEvalInterruptEmission
	channelPolicyMu    sync.RWMutex
	channelAlertAfter  int
	channelMaxAttempts int
}

// SetOutboxWakeup installs the post-commit wakeup hook used by the named
// SupervisorScheduler. It never runs inside a database transaction.
// SetChannelPolicy installs the frozen effective policy used by reclaim. It is
// configured once during daemon assembly; zero max attempts means retry forever.
func (d *DB) SetChannelPolicy(alertAfter, maxAttempts int) {
	d.channelPolicyMu.Lock()
	defer d.channelPolicyMu.Unlock()
	d.channelAlertAfter, d.channelMaxAttempts = alertAfter, maxAttempts
}
func (d *DB) channelPolicy() (int, int) {
	d.channelPolicyMu.RLock()
	defer d.channelPolicyMu.RUnlock()
	return d.channelAlertAfter, d.channelMaxAttempts
}

func (d *DB) SetOutboxWakeup(wakeup func()) {
	d.wakeupMu.Lock()
	defer d.wakeupMu.Unlock()
	d.outboxWakeup = wakeup
}

func (d *DB) wakeOutbox() {
	d.wakeupMu.RLock()
	wakeup := d.outboxWakeup
	d.wakeupMu.RUnlock()
	if wakeup != nil {
		wakeup()
	}
}

// SetInterruptT4 installs the single production T4 caller used by every
// EmitInterrupt path. The caller itself is always invoked before the write
// transaction.
func (d *DB) SetInterruptT4(caller InterruptT4Caller) {
	d.wakeupMu.Lock()
	defer d.wakeupMu.Unlock()
	d.interruptT4 = caller
}

func (d *DB) interruptT4Caller() InterruptT4Caller {
	d.wakeupMu.RLock()
	defer d.wakeupMu.RUnlock()
	return d.interruptT4
}

// SetInterruptT6 installs the single production T6 dispatch caller used by
// every EmitInterrupt path. Like T4 it runs before the write transaction. A
// per-call cmd.T6 override takes precedence so tests can pin one call.
func (d *DB) SetInterruptT6(caller InterruptT6Caller) {
	d.wakeupMu.Lock()
	defer d.wakeupMu.Unlock()
	d.interruptT6 = caller
}

func (d *DB) interruptT6Caller() InterruptT6Caller {
	d.wakeupMu.RLock()
	defer d.wakeupMu.RUnlock()
	return d.interruptT6
}

// GateReEvalInterruptEmission carries attention/channel defaults for
// Interrupt emission inside CompleteGateReEvaluation (failed-arm and HITL arms).
type GateReEvalInterruptEmission struct {
	AttentionDailyQuota                     map[InterruptSeverity]int
	DayTimezone, DailySummaryAt             string
	MaxEscalations                          int
	CriticalWindowMS                        int64
	CriticalTotalLimit, CriticalPerRunLimit int
	Channels                                []InterruptChannel
}

// SetGateReEvalInterruptEmission installs production defaults for
// gate_re_evaluation Interrupt successors (storage.md §8.1).
func (d *DB) SetGateReEvalInterruptEmission(cfg GateReEvalInterruptEmission) {
	d.wakeupMu.Lock()
	defer d.wakeupMu.Unlock()
	d.gateReEvalIntr = cfg
}

func (d *DB) gateReEvalInterruptEmission() GateReEvalInterruptEmission {
	d.wakeupMu.RLock()
	defer d.wakeupMu.RUnlock()
	return d.gateReEvalIntr
}

// Path returns the database file path this handle opened.
func (d *DB) Path() string { return d.path }

// NewID returns a storage-shaped opaque identifier for callers assembling a
// restricted write-port command.
func NewID() string { return newID() }

// Close closes the underlying pool.
func (d *DB) Close() error { return d.db.Close() }

// SchemaVersion reports the highest applied migration version, or 0 for a
// database with no applied migrations.
func (d *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	if err := d.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, fmt.Errorf("storage: read schema version: %w", err)
	}
	return int(v.Int64), nil
}

// CheckReadOnly verifies that an existing database can be read without creating,
// migrating, or modifying it. It is used by offline diagnostics.
func CheckReadOnly(ctx context.Context, path string) error {
	if path == "" {
		return errors.New("storage: database path is required")
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("storage: stat %s: %w", path, err)
	}
	pool, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return fmt.Errorf("storage: open read-only %s: %w", path, err)
	}
	defer pool.Close()
	if err := pool.PingContext(ctx); err != nil {
		return fmt.Errorf("storage: read-only connect %s: %w", path, err)
	}
	var result string
	if err := pool.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("storage: quick_check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("storage: quick_check = %q", result)
	}
	return nil
}

// Open opens (creating if necessary) the database at cfg.Path, enforces file
// and directory permissions, sets and verifies the §2 PRAGMA contract, and
// applies pending forward migrations. It refuses to start when any critical
// PRAGMA does not take effect, when the database is newer than this binary,
// or when an applied migration record drifts from the embedded files.
func Open(ctx context.Context, cfg OpenConfig) (*DB, error) {
	if cfg.Path == "" {
		return nil, errors.New("storage: OpenConfig.Path is required")
	}
	if cfg.BinaryVersion == "" {
		return nil, errors.New("storage: OpenConfig.BinaryVersion is required")
	}
	if cfg.Now.IsZero() {
		return nil, errors.New("storage: OpenConfig.Now is required: time is injected, never read from the system clock")
	}

	dir := filepath.Dir(cfg.Path)
	if err := os.MkdirAll(dir, dbDirMode); err != nil {
		return nil, fmt.Errorf("storage: create database directory %s: %w", dir, err)
	}
	if err := os.Chmod(dir, dbDirMode); err != nil {
		return nil, fmt.Errorf("storage: enforce directory mode 0700 on %s: %w", dir, err)
	}

	params := url.Values{}
	for _, p := range []string{
		fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS),
		"foreign_keys(1)",
		"synchronous(FULL)",
		"temp_store(MEMORY)",
		"journal_mode(WAL)",
		fmt.Sprintf("wal_autocheckpoint(%d)", walAutocheckpoint),
	} {
		params.Add("_pragma", p)
	}
	// Every transaction opened through database/sql, including the migration
	// transactions, runs as BEGIN IMMEDIATE (specs/storage.md §3.1).
	params.Set("_txlock", "immediate")
	dsn := "file:" + cfg.Path + "?" + params.Encode()

	pool, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", cfg.Path, err)
	}
	// The write pool is pinned to 1 (DESIGN §7, storage.md §1 invariant 3).
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)

	fail := func(err error) (*DB, error) {
		_ = pool.Close()
		return nil, err
	}

	if err := pool.PingContext(ctx); err != nil {
		return fail(fmt.Errorf("storage: connect %s: %w", cfg.Path, err))
	}
	// journal_mode is persistent but set-and-verified on every startup, as
	// the §2 contract requires of the write connection.
	if _, err := pool.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
		return fail(fmt.Errorf("storage: set journal_mode=WAL: %w", err))
	}
	if err := verifyPragmas(ctx, pool); err != nil {
		return fail(err)
	}
	if err := os.Chmod(cfg.Path, dbFileMode); err != nil {
		return fail(fmt.Errorf("storage: enforce file mode 0600 on %s: %w", cfg.Path, err))
	}
	if err := applyMigrations(ctx, pool, cfg.BinaryVersion, cfg.Now); err != nil {
		return fail(err)
	}
	if err := ensureChannelSchema(ctx, pool); err != nil {
		return fail(fmt.Errorf("storage: ensure channel schema: %w", err))
	}
	return &DB{db: pool, path: cfg.Path}, nil
}

// verifyPragmas refuses startup when any critical PRAGMA failed to take
// effect on the write connection (specs/storage.md §2).
func verifyPragmas(ctx context.Context, db *sql.DB) error {
	intChecks := []struct {
		name string
		want int64
	}{
		{"foreign_keys", 1},
		{"busy_timeout", busyTimeoutMS},
		{"synchronous", synchronousFull},
		{"temp_store", tempStoreMemory},
		{"wal_autocheckpoint", walAutocheckpoint},
	}
	for _, c := range intChecks {
		var got int64
		if err := db.QueryRowContext(ctx, "PRAGMA "+c.name).Scan(&got); err != nil {
			return fmt.Errorf("storage: verify PRAGMA %s: %w", c.name, err)
		}
		if got != c.want {
			return fmt.Errorf("storage: PRAGMA %s = %d, want %d: refusing to start", c.name, got, c.want)
		}
	}
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return fmt.Errorf("storage: verify PRAGMA journal_mode: %w", err)
	}
	if !strings.EqualFold(mode, journalModeWAL) {
		return fmt.Errorf("storage: journal_mode = %q, want %q: refusing to start", mode, journalModeWAL)
	}
	return nil
}

// StartDaemonBoot records a new daemon lifetime with its recovery barrier
// closed. A boot never reuses a prior completion marker: every restart must
// reclassify recovery candidates before it can dispatch an agent.
func (d *DB) StartDaemonBoot(ctx context.Context, configHash, binaryVersion string, protocolMajor, pid int, nowMS int64) (string, error) {
	if configHash == "" || binaryVersion == "" || protocolMajor < 1 || pid < 1 || nowMS <= 0 {
		return "", errors.New("storage: invalid daemon boot")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var configID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM config_snapshots WHERE config_hash=?`, configHash).Scan(&configID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("storage: daemon boot config snapshot: %w", err)
		}
		return "", err
	}
	id := newID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO daemon_boots
		(id,config_snapshot_id,pid,binary_version,protocol_major,started_at_ms)
		VALUES (?,?,?,?,?,?)`, id, configID, pid, binaryVersion, protocolMajor, nowMS); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// CompleteStartupRecovery opens this boot's launch-agent claim barrier. It is
// deliberately separate from recovery writes so a crash before this commit
// leaves the next boot closed as well.
func (d *DB) CompleteStartupRecovery(ctx context.Context, bootID string, nowMS int64) error {
	if bootID == "" || nowMS <= 0 {
		return errors.New("storage: invalid startup recovery completion")
	}
	// The final query is part of the barrier write, not a best-effort check in
	// the coordinator. A crash or a newly visible candidate cannot open the
	// launch gate until it has a boot-scoped classification receipt.
	result, err := d.db.ExecContext(ctx, `UPDATE daemon_boots
		SET recovery_completed_at_ms=?
		WHERE id=? AND recovery_completed_at_ms IS NULL
		AND NOT EXISTS (
			SELECT 1 FROM attempts a WHERE a.phase NOT IN ('finished','orphaned')
			AND NOT EXISTS (SELECT 1 FROM startup_recovery_actions s WHERE s.boot_id=?
				AND s.candidate_key='attempt:' || a.run_id || ':' || a.attempt_no)
		)
		AND NOT EXISTS (
			SELECT 1 FROM outbox_operations o WHERE o.kind='launch_agent' AND o.state NOT IN ('succeeded','failed','stale','conflict')
			AND NOT EXISTS (SELECT 1 FROM startup_recovery_actions s WHERE s.boot_id=?
				AND s.candidate_key='operation:' || o.id)
		)`, nowMS, bootID, bootID, bootID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrRejectedStale
	}
	return nil
}

type HookBaseline struct {
	ProjectID string
	Digest    string
}

// HookBaselineSnapshot is the immutable observation supplied by the
// application layer. Storage persists it but never performs filesystem IO.
type HookBaselineSnapshot struct {
	GitConfigDigest      string
	CoreHooksPathValue   *string
	EffectiveHooksPath   string
	HooksDirectoryDigest string
	Digest               string
}

type RecordHookBaselineCmd struct {
	ProjectID       string
	Snapshot        HookBaselineSnapshot
	ExpectedDigest  string
	SourceRunID     string
	SourceAttemptNo *int
	CapturedAtMS    int64
}

type HookRecheckReceipt struct {
	RunID     string
	AttemptNo int
	ProjectID string
}

// PendingHookRechecks returns terminal attempts whose audit receipt survived
// completion but has not yet been inspected.
func (d *DB) PendingHookRechecks(ctx context.Context) ([]HookRecheckReceipt, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT run_id,attempt_no,project_id FROM hook_recheck_receipts WHERE state='pending' ORDER BY created_at_ms,run_id,attempt_no`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HookRecheckReceipt
	for rows.Next() {
		var receipt HookRecheckReceipt
		if err := rows.Scan(&receipt.RunID, &receipt.AttemptNo, &receipt.ProjectID); err != nil {
			return nil, err
		}
		out = append(out, receipt)
	}
	return out, rows.Err()
}

// CompleteHookRecheck makes an audit result durable. It is idempotent so a
// replay after an already-recorded diagnostic never creates a second result.
func (d *DB) CompleteHookRecheck(ctx context.Context, runID string, attemptNo int, nowMS int64) error {
	if runID == "" || attemptNo < 1 || nowMS <= 0 {
		return errors.New("storage: invalid hook recheck receipt")
	}
	_, err := d.db.ExecContext(ctx, `UPDATE hook_recheck_receipts SET state='completed',completed_at_ms=COALESCE(completed_at_ms,?) WHERE run_id=? AND attempt_no=?`, nowMS, runID, attemptNo)
	return err
}

// RecordHookDiagnostic is the audit-only failure path for capture/persist
// observations. Its stable receipt key makes crash replay byte-stable.
func (d *DB) RecordHookDiagnostic(ctx context.Context, projectID, runID string, attemptNo int, kind, detail string, nowMS int64) error {
	if projectID == "" || kind == "" || nowMS <= 0 || (runID == "") != (attemptNo == 0) {
		return errors.New("storage: invalid hook diagnostic")
	}
	payload, _ := json.Marshal(map[string]any{"project_id": projectID, "run_id": runID, "attempt_no": attemptNo, "detail": detail})
	var attempt any
	if attemptNo != 0 {
		attempt = attemptNo
	}
	_, err := d.db.ExecContext(ctx, `INSERT INTO events(id,project_id,run_id,attempt_no,type,source,payload_schema_version,payload_json,idempotency_key,occurred_at_ms,recorded_at_ms)
		VALUES(?,?,?,?,?,'system',1,?,?,?,?) ON CONFLICT(idempotency_key) DO NOTHING`, newID(), projectID, nullable(runID), attempt, kind, string(payload), "hook-diagnostic:"+kind+":"+projectID+":"+runID+":"+strconv.Itoa(attemptNo), nowMS, nowMS)
	return err
}

// HookProjectForRun resolves the immutable project/repository identity used
// by completion-time hook rechecks.
func (d *DB) HookProjectForRun(ctx context.Context, runID string) (string, string, error) {
	var projectID, repo string
	err := d.db.QueryRowContext(ctx, `SELECT r.project_id,p.repo_path FROM runs r JOIN projects p ON p.id=r.project_id WHERE r.id=?`, runID).Scan(&projectID, &repo)
	return projectID, repo, err
}

// RecordHookBaseline installs the trusted initial baseline, or rechecks the
// current baseline. A changed observation is recorded as an audit event and
// deliberately does not replace the trusted baseline.
func (d *DB) RecordHookBaseline(ctx context.Context, cmd RecordHookBaselineCmd) error {
	if cmd.ProjectID == "" || cmd.Snapshot.Digest == "" || cmd.Snapshot.GitConfigDigest == "" || cmd.Snapshot.EffectiveHooksPath == "" || cmd.Snapshot.HooksDirectoryDigest == "" || cmd.CapturedAtMS <= 0 {
		return errors.New("storage: invalid hook baseline")
	}
	if (cmd.SourceRunID == "") != (cmd.SourceAttemptNo == nil) {
		return errors.New("storage: hook baseline source must be paired")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old string
	var exists bool
	err = tx.QueryRowContext(ctx, `SELECT baseline_digest FROM project_hook_baselines WHERE project_id=?`, cmd.ProjectID).Scan(&old)
	if err == sql.ErrNoRows {
		exists = false
	} else if err != nil {
		return err
	} else {
		exists = true
	}
	if cmd.ExpectedDigest != "" && ((!exists) || old != cmd.ExpectedDigest) {
		return ErrRejectedStale
	}
	if !exists && cmd.SourceRunID != "" {
		payload, _ := json.Marshal(map[string]any{"project_id": cmd.ProjectID, "observed_digest": cmd.Snapshot.Digest, "source_run_id": cmd.SourceRunID, "source_attempt_no": cmd.SourceAttemptNo})
		_, err = tx.ExecContext(ctx, `INSERT INTO events(id,project_id,type,source,payload_schema_version,payload_json,idempotency_key,occurred_at_ms,recorded_at_ms) VALUES(?,?, 'hooks_baseline_missing','system',1,?,?,?,?) ON CONFLICT(idempotency_key) DO NOTHING`, newID(), cmd.ProjectID, string(payload), "hooks-baseline-missing:"+cmd.ProjectID, cmd.CapturedAtMS, cmd.CapturedAtMS)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	if !exists {
		var completed int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM attempts a JOIN runs r ON r.id=a.run_id WHERE r.project_id=?`, cmd.ProjectID).Scan(&completed); err != nil {
			return err
		}
		if completed != 0 {
			payload, _ := json.Marshal(map[string]any{"project_id": cmd.ProjectID, "observed_digest": cmd.Snapshot.Digest})
			_, err = tx.ExecContext(ctx, `INSERT INTO events(id,project_id,type,source,payload_schema_version,payload_json,idempotency_key,occurred_at_ms,recorded_at_ms) VALUES(?,?, 'hooks_baseline_activation_missing','system',1,?,?,?,?) ON CONFLICT(idempotency_key) DO NOTHING`, newID(), cmd.ProjectID, string(payload), "hooks-baseline-activation-missing:"+cmd.ProjectID, cmd.CapturedAtMS, cmd.CapturedAtMS)
			if err != nil {
				return err
			}
			return tx.Commit()
		}
	}
	if exists {
		if old != cmd.Snapshot.Digest {
			payload, _ := json.Marshal(map[string]any{"project_id": cmd.ProjectID, "baseline_digest": old, "observed_digest": cmd.Snapshot.Digest, "git_config_digest": cmd.Snapshot.GitConfigDigest, "core_hooks_path": cmd.Snapshot.CoreHooksPathValue, "effective_hooks_path": cmd.Snapshot.EffectiveHooksPath, "hooks_directory_digest": cmd.Snapshot.HooksDirectoryDigest, "source_run_id": cmd.SourceRunID, "source_attempt_no": cmd.SourceAttemptNo})
			_, err = tx.ExecContext(ctx, `INSERT INTO events(id,project_id,type,source,payload_schema_version,payload_json,idempotency_key,occurred_at_ms,recorded_at_ms) VALUES(?,?, 'hooks_drift_detected','system',1,?,?,?,?) ON CONFLICT(idempotency_key) DO NOTHING`, newID(), cmd.ProjectID, string(payload), "hooks-drift:"+cmd.ProjectID+":"+cmd.Snapshot.Digest, cmd.CapturedAtMS, cmd.CapturedAtMS)
			if err != nil {
				return err
			}
			return tx.Commit()
		}
		_, err = tx.ExecContext(ctx, `UPDATE project_hook_baselines SET updated_at_ms=? WHERE project_id=?`, cmd.CapturedAtMS, cmd.ProjectID)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO project_hook_baselines(project_id,git_config_digest,core_hooks_path_value,effective_hooks_path,hooks_directory_digest,baseline_digest,source_run_id,source_attempt_no,captured_at_ms,updated_at_ms) VALUES(?,?,?,?,?,?,?,?,?,?)`, cmd.ProjectID, cmd.Snapshot.GitConfigDigest, cmd.Snapshot.CoreHooksPathValue, cmd.Snapshot.EffectiveHooksPath, cmd.Snapshot.HooksDirectoryDigest, cmd.Snapshot.Digest, nullable(cmd.SourceRunID), cmd.SourceAttemptNo, cmd.CapturedAtMS, cmd.CapturedAtMS)
	if err != nil {
		return err
	}
	return tx.Commit()
}

type DoctorAttempt struct {
	RunID          string
	AttemptNo      int
	Phase          string
	IsolationState string
	WorktreePath   string
	AgentID        string
}

// ReadDoctorState reads only diagnostic projections from an existing database.
// It never creates, migrates, or mutates the database.
func ReadDoctorState(ctx context.Context, path string) ([]HookBaseline, []DoctorAttempt, error) {
	pool, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, nil, fmt.Errorf("storage: open doctor database: %w", err)
	}
	defer pool.Close()
	hooks := []HookBaseline{}
	rows, err := pool.QueryContext(ctx, `SELECT project_id,baseline_digest FROM project_hook_baselines`)
	if err != nil {
		return nil, nil, fmt.Errorf("storage: read hook baselines: %w", err)
	}
	for rows.Next() {
		var h HookBaseline
		if err := rows.Scan(&h.ProjectID, &h.Digest); err != nil {
			rows.Close()
			return nil, nil, err
		}
		hooks = append(hooks, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	rows.Close()
	attempts := []DoctorAttempt{}
	rows, err = pool.QueryContext(ctx, `SELECT run_id,attempt_no,phase,isolation_state,worktree_path,agent_id FROM attempts WHERE isolation_state='frozen' OR phase NOT IN ('finished','orphaned') ORDER BY run_id,attempt_no`)
	if err != nil {
		return nil, nil, fmt.Errorf("storage: read attempt state: %w", err)
	}
	for rows.Next() {
		var a DoctorAttempt
		if err := rows.Scan(&a.RunID, &a.AttemptNo, &a.Phase, &a.IsolationState, &a.WorktreePath, &a.AgentID); err != nil {
			rows.Close()
			return nil, nil, err
		}
		attempts = append(attempts, a)
	}
	return hooks, attempts, rows.Err()
}

// Migration refusal sentinels (specs/storage.md §3.1). Callers match with
// errors.Is; the wrapped message carries the versions involved.
var (
	// ErrDatabaseTooNew means the database's highest applied version exceeds
	// what this binary embeds. Downgrades are never attempted.
	ErrDatabaseTooNew = errors.New("storage: database schema is newer than this binary supports")
	// ErrMigrationMismatch means an applied migration record (name or
	// checksum) disagrees with the embedded file of the same version, or an
	// applied version has no embedded counterpart.
	ErrMigrationMismatch = errors.New("storage: applied migration does not match embedded migration file")
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

var migrationFileRE = regexp.MustCompile(`^([0-9]{4})_([a-z0-9_]+)\.sql$`)

// foreignKeyRebuilds lists migrations that rebuild a table. They run with
// foreign_keys temporarily disabled (PRAGMA is only honored outside a tx).
var foreignKeyRebuilds = map[int]bool{21: true, 54: true, 57: true}

// migration is one embedded forward migration file.
type migration struct {
	version  int
	name     string // file name without extension, e.g. "0001_initial_schema"
	body     string
	checksum string // lowercase hex SHA-256 of the file bytes
}

// loadEmbeddedMigrations reads, validates and sorts the embedded migration
// files. Versions must be positive and unique.
func loadEmbeddedMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("storage: read embedded migrations: %w", err)
	}
	if len(entries) == 0 {
		return nil, errors.New("storage: no embedded migrations found")
	}
	ms := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		match := migrationFileRE.FindStringSubmatch(e.Name())
		if match == nil {
			return nil, fmt.Errorf("storage: migration file %q does not match NNNN_name.sql", e.Name())
		}
		version, err := strconv.Atoi(match[1])
		if err != nil || version < 1 {
			return nil, fmt.Errorf("storage: migration file %q has a non-positive version", e.Name())
		}
		body, err := fs.ReadFile(migrationsFS, "migrations/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("storage: read migration %q: %w", e.Name(), err)
		}
		sum := sha256.Sum256(body)
		ms = append(ms, migration{
			version:  version,
			name:     match[1] + "_" + match[2],
			body:     string(body),
			checksum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i := 1; i < len(ms); i++ {
		if ms[i].version == ms[i-1].version {
			return nil, fmt.Errorf("storage: duplicate embedded migration version %04d", ms[i].version)
		}
	}
	return ms, nil
}

// applyMigrations creates schema_migrations if needed, verifies every applied
// record against the embedded files, refuses a newer database, and applies
// each pending migration in its own BEGIN IMMEDIATE transaction (the pool's
// _txlock=immediate) together with its schema_migrations row.
func applyMigrations(ctx context.Context, db *sql.DB, binaryVersion string, now time.Time) error {
	ms, err := loadEmbeddedMigrations()
	if err != nil {
		return err
	}

	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER NOT NULL PRIMARY KEY CHECK (version >= 1),
		name TEXT NOT NULL,
		checksum TEXT NOT NULL,
		applied_at_ms INTEGER NOT NULL,
		binary_version TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("storage: create schema_migrations: %w", err)
	}

	type appliedRecord struct{ name, checksum string }
	applied := map[int]appliedRecord{}
	maxApplied := 0
	rows, err := db.QueryContext(ctx, `SELECT version, name, checksum FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("storage: read schema_migrations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v int
		var rec appliedRecord
		if err := rows.Scan(&v, &rec.name, &rec.checksum); err != nil {
			return fmt.Errorf("storage: scan schema_migrations: %w", err)
		}
		applied[v] = rec
		if v > maxApplied {
			maxApplied = v
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("storage: read schema_migrations: %w", err)
	}

	maxEmbedded := ms[len(ms)-1].version
	if maxApplied > maxEmbedded {
		return fmt.Errorf("%w: database is at version %d, binary supports %d",
			ErrDatabaseTooNew, maxApplied, maxEmbedded)
	}

	// Applied records must form an exact prefix of the embedded migrations:
	// same version, same name, same checksum, no gaps.
	for _, m := range ms {
		rec, ok := applied[m.version]
		if m.version > maxApplied {
			break
		}
		if !ok {
			return fmt.Errorf("%w: version %04d is embedded but not applied (gap in migration history)",
				ErrMigrationMismatch, m.version)
		}
		if rec.name != m.name {
			return fmt.Errorf("%w: version %04d applied as %q, embedded as %q",
				ErrMigrationMismatch, m.version, rec.name, m.name)
		}
		if rec.checksum != m.checksum {
			return fmt.Errorf("%w: version %04d (%s) checksum drifted", ErrMigrationMismatch, m.version, m.name)
		}
	}

	for _, m := range ms {
		if m.version <= maxApplied {
			continue
		}
		if err := applyOne(ctx, db, m, binaryVersion, now); err != nil {
			return err
		}
	}
	return nil
}

// applyOne applies a single migration and records it, atomically. The
// migration file itself contains no transaction control; the pool's
// _txlock=immediate makes BeginTx a BEGIN IMMEDIATE.
func applyOne(ctx context.Context, db *sql.DB, m migration, binaryVersion string, now time.Time) error {
	// These migrations rebuild a table (SQLite CHECK constraints are
	// immutable). SQLite only honors foreign_keys outside a transaction, so
	// execute the otherwise transactional migration with FK enforcement
	// temporarily disabled. This preserves FK definitions on the replacement
	// table and lets populated databases advance; foreign_key_check runs after.
	// 0021 rebuilds interrupts; 0054 rebuilds outbox_operations to add the
	// gate_re_evaluation outbox kind (storage.md §8.1); 0057 rebuilds it again
	// to add the rerun_checks kind and creates check_rerun_consumptions
	// (storage.md §8.1 retry_checks successor + §8.2).
	foreignKeysDisabled := false
	if foreignKeyRebuilds[m.version] {
		if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
			return fmt.Errorf("storage: disable foreign keys for migration %s: %w", m.name, err)
		}
		foreignKeysDisabled = true
		defer func() {
			_, _ = db.ExecContext(context.Background(), `PRAGMA foreign_keys=ON`)
		}()
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: begin migration %s: %w", m.name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.body); err != nil {
		return fmt.Errorf("storage: apply migration %s: %w", m.name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, checksum, applied_at_ms, binary_version)
		 VALUES (?, ?, ?, ?, ?)`,
		m.version, m.name, m.checksum, now.UnixMilli(), binaryVersion,
	); err != nil {
		return fmt.Errorf("storage: record migration %s: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit migration %s: %w", m.name, err)
	}
	if foreignKeysDisabled {
		if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
			return fmt.Errorf("storage: re-enable foreign keys for migration %s: %w", m.name, err)
		}
		var violation string
		if err := db.QueryRowContext(ctx, `PRAGMA foreign_key_check`).Scan(&violation); err != sql.ErrNoRows {
			if err != nil {
				return fmt.Errorf("storage: check foreign keys after migration %s: %w", m.name, err)
			}
			return fmt.Errorf("storage: foreign key violation after migration %s: %s", m.name, violation)
		}
	}
	return nil
}

// RecordHandoffSecurityEvent preserves rejected wrapper handoffs without
// retaining credentials. A stale generation and a competing wrapper are both
// security-relevant: the rejection is the fencing boundary that prevented a
// second owner from obtaining spawn authority.
func (d *DB) RecordHandoffSecurityEvent(ctx context.Context, runID string, attemptNo int, method, disposition string, nowMS int64) error {
	if runID == "" || attemptNo < 1 || method == "" || disposition == "" || nowMS <= 0 {
		return nil
	}
	payload, _ := json.Marshal(map[string]string{"method": method, "disposition": disposition})
	_, err := d.db.ExecContext(ctx, `INSERT INTO events
		(id,run_id,attempt_no,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms)
		SELECT ?,?,?, 'security.handoff_rejected','agent',1,?,?,?
		WHERE EXISTS (SELECT 1 FROM attempts WHERE run_id=? AND attempt_no=?)`,
		newID(), runID, attemptNo, string(payload), nowMS, nowMS, runID, attemptNo)
	return err
}

// Cross-package integration-test seeds. These are NOT domain write ports
// (§11): they exist so tests in other packages (brain shell, control plane)
// can satisfy foreign keys without raw SQL access. Production code must not
// call them.

// ExecForTest runs an arbitrary statement against the underlying handle. It is
// the escape hatch for cross-package tests that need bespoke fixture rows not
// covered by a named Seed helper; it is never a production write port.
func (d *DB) ExecForTest(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.db.ExecContext(ctx, query, args...)
}

// QueryRowForTest returns one row from the underlying handle. It is a
// read-only escape hatch for cross-package tests; production reads use the
// named projections.
func (d *DB) QueryRowForTest(ctx context.Context, query string, args ...any) *sql.Row {
	return d.db.QueryRowContext(ctx, query, args...)
}

// AdvanceAttachIdentityForTest atomically simulates recovery superseding an
// active attach binding. It is only for cross-package attach race fixtures.
func (d *DB) AdvanceAttachIdentityForTest(ctx context.Context, runID string, attemptNo, generation int, dispatchID string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE attempts SET generation=? WHERE run_id=? AND attempt_no=?`, generation, runID, attemptNo); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE attempt_claims SET generation=?, dispatch_id=? WHERE run_id=? AND attempt_no=?`, generation, dispatchID, runID, attemptNo); err != nil {
		return err
	}
	return tx.Commit()
}

// SeedProjectForTest inserts a config snapshot and project with minimal
// valid rows.
func (d *DB) SeedProjectForTest(ctx context.Context, cfgID, projectID string, nowMS int64) error {
	if _, err := d.db.ExecContext(ctx, `INSERT INTO config_snapshots
		(id, config_hash, schema_version, canonical_json, source_present, source_mtime_ms, loaded_at_ms, binary_version)
		VALUES (?, ?, 1, '{}', 1, NULL, ?, 'test-binary')`, cfgID, "hash-"+cfgID, nowMS); err != nil {
		return fmt.Errorf("storage: seed config snapshot: %w", err)
	}
	if _, err := d.db.ExecContext(ctx, `INSERT INTO projects
		(id, config_snapshot_id, forge_kind, forge_host, forge_project_key, repo_path,
		 enabled, health, isolation_reason, capabilities_json, capabilities_checked_at_ms,
		 created_at_ms, updated_at_ms)
		VALUES (?, ?, 'github', 'github.com', ?, ?, 1, 'active', NULL, '{}', NULL, ?, ?)`,
		projectID, cfgID, "org/repo-"+projectID, "/repo/"+projectID, nowMS, nowMS); err != nil {
		return fmt.Errorf("storage: seed project: %w", err)
	}
	return nil
}

// SeedForgeRunForTest inserts a forge-sourced queued run with minimal valid
// fields.
func (d *DB) SeedForgeRunForTest(ctx context.Context, runID, projectID, cfgID, issueID string, nowMS int64) error {
	return d.SeedReverseSyncRunForTest(ctx, runID, projectID, cfgID, issueID, "", "queued", nowMS)
}

// SeedLaunchRunForTest creates the smallest fully assigned pending launch used
// by end-to-end runtime tests. It deliberately remains a test-only seed: the
// production assignment and transition ports are still the only domain writes.
func (d *DB) SeedLaunchRunForTest(ctx context.Context, runID, projectID, cfgID string, nowMS int64, worktree string) error {
	if err := d.SeedForgeRunForTest(ctx, runID, projectID, cfgID, "issue-1", nowMS); err != nil {
		return err
	}
	taskID := "task-" + runID
	if _, err := d.db.ExecContext(ctx, `INSERT INTO task_spec_snapshots
		(id, run_id, version, schema_version, canonical_json, content_digest, created_at_ms)
		VALUES (?, ?, 1, 1, '{"title":"crash-suite"}', 'task-digest', ?)
	`, taskID, runID, nowMS); err != nil {
		return fmt.Errorf("storage: seed launch task: %w", err)
	}
	if _, err := d.db.ExecContext(ctx, `UPDATE runs SET kind='bug', agent_id='agent', current_task_spec_id=?, version=2, updated_at_ms=? WHERE id=?`, taskID, nowMS, runID); err != nil {
		return fmt.Errorf("storage: seed launch assignment: %w", err)
	}
	key := LaunchOperationKey(runID, 1, 1)
	if _, err := d.db.ExecContext(ctx, `INSERT INTO attempts
		(run_id, attempt_no, phase, generation, backend, agent_id, task_spec_snapshot_id,
		 worktree_path, branch_name, base_ref, base_sha, isolation_state, created_at_ms, updated_at_ms)
		VALUES (?, 1, 'pending', 1, 'process', 'agent', ?, ?, 'main', 'main', 'base', 'none', ?, ?)`, runID, taskID, worktree, nowMS, nowMS); err != nil {
		return fmt.Errorf("storage: seed launch attempt: %w", err)
	}
	if _, err := d.db.ExecContext(ctx, `INSERT INTO attempt_claims
		(run_id, attempt_no, generation, launch_operation_key, created_at_ms, updated_at_ms)
		VALUES (?, 1, 1, ?, ?, ?)`, runID, key, nowMS, nowMS); err != nil {
		return fmt.Errorf("storage: seed launch claim: %w", err)
	}
	if _, err := d.EnqueueOperation(ctx, Operation{Key: key, Kind: OperationLaunchAgent, RunID: runID, AttemptNo: intPtr(1), Payload: []byte(`{"schema_version":1}`)}, nowMS); err != nil {
		return fmt.Errorf("storage: seed launch operation: %w", err)
	}
	return nil
}

// SeedAttachRunForTest creates a fully dispatched active attempt for attach
// observer tests. It is test-only; production dispatch still flows through the
// launch worker's claim and prepare ports.
func (d *DB) SeedAttachRunForTest(ctx context.Context, runID, projectID, cfgID, backend string, nowMS int64, worktree string) error {
	if backend != "process" && backend != "tmux" {
		return fmt.Errorf("storage: invalid attach test backend %q", backend)
	}
	if err := d.SeedLaunchRunForTest(ctx, runID, projectID, cfgID, nowMS, worktree); err != nil {
		return err
	}
	if _, err := d.db.ExecContext(ctx, `UPDATE attempts SET backend=? WHERE run_id=? AND attempt_no=1`, backend, runID); err != nil {
		return fmt.Errorf("storage: seed attach backend: %w", err)
	}
	if _, err := d.db.ExecContext(ctx, `UPDATE attempt_claims
		SET dispatch_id='dispatch-' || ?, bootstrap_nonce_hash=x'01', run_token_hash=x'02'
		WHERE run_id=? AND attempt_no=1`, runID, runID); err != nil {
		return fmt.Errorf("storage: seed attach dispatch: %w", err)
	}
	return nil
}

// SeedGateCandidateForTest creates the persisted Change and attempt identity
// required by the production Gate reconciler.
func (d *DB) SeedGateCandidateForTest(ctx context.Context, runID, projectID, cfgID, changeID string, nowMS int64) error {
	if err := d.SeedLaunchRunForTest(ctx, runID, projectID, cfgID, nowMS, "/work"); err != nil {
		return err
	}
	if _, err := d.db.ExecContext(ctx, `UPDATE runs SET kind='feature', change_id=?, version=1 WHERE id=?`, changeID, runID); err != nil {
		return fmt.Errorf("storage: seed Gate run: %w", err)
	}
	return nil
}

// SeedFailedAttemptForTest gives cross-package tests the failed attempt binding
// required by failure_review's new_attempt arm.
func (d *DB) SeedFailedAttemptForTest(ctx context.Context, runID string, attemptNo int, nowMS int64) error {
	_, err := d.db.ExecContext(ctx, `UPDATE attempts SET phase='finished',result_exit_code=1,result_digest='failed',result_observed_at_ms=?,finished_at_ms=?,updated_at_ms=? WHERE run_id=? AND attempt_no=?`, nowMS, nowMS, nowMS, runID, attemptNo)
	return err
}

// SetRunChangeHeadForTest completes the immutable Change identity used by Gate fixtures.
func (d *DB) SetRunChangeHeadForTest(ctx context.Context, runID, changeID, headSHA string) error {
	_, err := d.db.ExecContext(ctx, `UPDATE runs SET change_id=?, change_head_sha=? WHERE id=?`, changeID, headSHA, runID)
	return err
}

// SeedCertificationForTest installs a current certified projection for Gate tests.
func (d *DB) SeedCertificationForTest(ctx context.Context, kind, version string, nowMS int64) error {
	if _, err := d.db.ExecContext(ctx, `INSERT INTO certifications
		(task_kind,certification_version,total_samples,negative_samples,leak_count,false_block_count,certified,evidence_digest,updated_at_ms,certification_rules_version,window_start_ms,window_end_ms)
		VALUES (?,?,0,0,0,0,1,'test',?,?,0,?)`, kind, version, nowMS, version, nowMS); err != nil {
		return fmt.Errorf("storage: seed certification: %w", err)
	}
	if _, err := d.db.ExecContext(ctx, `INSERT INTO certification_current(task_kind,certification_version,version,updated_at_ms) VALUES(?,?,1,?)`, kind, version, nowMS); err != nil {
		return fmt.Errorf("storage: seed current certification: %w", err)
	}
	return nil
}

// SeedReverseSyncRunForTest inserts an active forge run with the remote
// identity needed by the reverse-sync integration tests.
func (d *DB) SeedReverseSyncRunForTest(ctx context.Context, runID, projectID, cfgID, issueID, changeID, status string, nowMS int64) error {
	if _, err := d.db.ExecContext(ctx, `INSERT INTO runs
		(id, source_kind, project_id, config_snapshot_id, forge_kind, forge_host, forge_project_key,
		 issue_id, change_id, status, max_attempts, created_at_ms, updated_at_ms)
		VALUES (?, 'forge', ?, ?, 'github', 'github.com', ?, ?, NULLIF(?, ''), ?, 3, ?, ?)`,
		runID, projectID, cfgID, "org/repo-"+projectID, issueID, changeID, status, nowMS, nowMS); err != nil {
		return fmt.Errorf("storage: seed reverse-sync run: %w", err)
	}
	return nil
}
