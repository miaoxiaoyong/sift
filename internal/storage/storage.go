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
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sqlite "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"
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
