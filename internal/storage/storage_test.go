package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

const testNow = 1_700_000_000_000

// openTestDB opens a fresh database under t.TempDir() and registers cleanup.
func openTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sift-home", "sift.db")
	db, err := Open(context.Background(), OpenConfig{
		Path:          path,
		BinaryVersion: "test-binary",
		Now:           time.UnixMilli(testNow),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func TestOpenContract(t *testing.T) {
	db, path := openTestDB(t)
	ctx := context.Background()

	// Write pool pinned to a single connection (storage.md §1 invariant 3).
	if got := db.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1", got)
	}

	// §2 PRAGMA contract, verified on the live connection.
	intPragmas := map[string]int64{
		"foreign_keys":       1,
		"busy_timeout":       5000,
		"synchronous":        2, // FULL
		"temp_store":         2, // MEMORY
		"wal_autocheckpoint": 1000,
	}
	for name, want := range intPragmas {
		var got int64
		if err := db.db.QueryRowContext(ctx, "PRAGMA "+name).Scan(&got); err != nil {
			t.Fatalf("PRAGMA %s: %v", name, err)
		}
		if got != want {
			t.Errorf("PRAGMA %s = %d, want %d", name, got, want)
		}
	}
	var mode string
	if err := db.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}

	// File 0600, parent directory 0700 (storage.md §2).
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat db file: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("db file mode = %o, want 600", got)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat db dir: %v", err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("db dir mode = %o, want 700", got)
	}
}

func TestMigrationRecordedAndIdempotent(t *testing.T) {
	db, path := openTestDB(t)
	ctx := context.Background()

	version, err := db.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != 58 {
		t.Fatalf("SchemaVersion = %d, want 58", version)
	}

	embedded, err := loadEmbeddedMigrations()
	if err != nil {
		t.Fatalf("loadEmbeddedMigrations: %v", err)
	}
	var (
		name, checksum, binaryVersion string
		appliedAt                     int64
	)
	err = db.db.QueryRowContext(ctx,
		`SELECT name, checksum, applied_at_ms, binary_version FROM schema_migrations WHERE version = 1`,
	).Scan(&name, &checksum, &appliedAt, &binaryVersion)
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if name != embedded[0].name {
		t.Errorf("migration name = %q, want %q", name, embedded[0].name)
	}
	if checksum != embedded[0].checksum {
		t.Errorf("migration checksum = %q, want %q", checksum, embedded[0].checksum)
	}
	if appliedAt != testNow {
		t.Errorf("applied_at_ms = %d, want injected %d", appliedAt, testNow)
	}
	if binaryVersion != "test-binary" {
		t.Errorf("binary_version = %q, want test-binary", binaryVersion)
	}

	// Re-opening the same database is a no-op and must not fail.
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(ctx, OpenConfig{Path: path, BinaryVersion: "test-binary", Now: time.UnixMilli(testNow + 1)})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	var count int
	if err := reopened.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if count != 58 {
		t.Fatalf("schema_migrations rows = %d, want 58 after reopen", count)
	}
}

func TestDailyBatchUpgradeNormalizesLegacyLimitsAndReopens(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sift.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE schema_migrations (version INTEGER NOT NULL PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at_ms INTEGER NOT NULL, binary_version TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.version > 43 {
			break
		}
		if err := applyOne(ctx, raw, migration, "old-binary", time.UnixMilli(testNow)); err != nil {
			t.Fatalf("apply %04d: %v", migration.version, err)
		}
	}
	if _, err := raw.Exec(`INSERT INTO config_snapshots(id,config_hash,schema_version,canonical_json,source_present,loaded_at_ms,binary_version) VALUES('cfg','hash',1,'{}',1,1,'old-binary'); INSERT INTO projects(id,config_snapshot_id,forge_kind,forge_host,forge_project_key,repo_path,enabled,health,created_at_ms,updated_at_ms) VALUES('project','cfg','github','github.com','owner/project','/repo',1,'active',1,1); INSERT INTO attention_batches(id,state,project_id,channel_id,channel_snapshot_json,forge_kind,forge_host,forge_project_key,target_kind,target_id,kind,delivery_id,scope,scope_id,due_at_ms,critical_window_ms,critical_total_limit,critical_per_run_limit,created_at_ms,updated_at_ms) VALUES('daily:legacy','collecting','project','ops','{}','github','github.com','owner/project','issue','42','daily_summary','daily:legacy:publish:1','day','UTC:1',1,900000,5,2,1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(ctx, OpenConfig{Path: path, BinaryVersion: "test-binary", Now: time.UnixMilli(testNow + 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var window, total, perRun sql.NullInt64
	if err := db.db.QueryRowContext(ctx, `SELECT critical_window_ms,critical_total_limit,critical_per_run_limit FROM attention_batches WHERE id='daily:legacy'`).Scan(&window, &total, &perRun); err != nil {
		t.Fatal(err)
	}
	if window.Valid || total.Valid || perRun.Valid {
		t.Fatalf("legacy daily limits = %#v/%#v/%#v, want NULL", window, total, perRun)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE attention_batches SET critical_window_ms=1 WHERE id='daily:legacy'`); err == nil {
		t.Fatal("collecting daily batch accepted critical authority")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, OpenConfig{Path: path, BinaryVersion: "test-binary", Now: time.UnixMilli(testNow + 2)})
	if err != nil {
		t.Fatalf("reopen upgraded database: %v", err)
	}
	defer reopened.Close()
}

func TestPopulated0020To0021UpgradePreservesForeignKeysAndRestarts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sift.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA foreign_keys=ON; CREATE TABLE schema_migrations (version INTEGER NOT NULL PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at_ms INTEGER NOT NULL, binary_version TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.version > 20 {
			break
		}
		if err := applyOne(ctx, raw, migration, "old-binary", time.UnixMilli(testNow)); err != nil {
			t.Fatalf("apply %04d: %v", migration.version, err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO config_snapshots(id,config_hash,schema_version,canonical_json,source_present,loaded_at_ms,binary_version) VALUES ('cfg','hash',1,'{}',1,1700000000000,'old')`,
		`INSERT INTO projects(id,config_snapshot_id,forge_kind,forge_host,forge_project_key,repo_path,enabled,health,isolation_reason,capabilities_json,created_at_ms,updated_at_ms) VALUES ('project','cfg','github','github.com','org/repo','/repo',1,'active',NULL,'{}',1700000000000,1700000000000)`,
		`INSERT INTO runs(id,source_kind,project_id,config_snapshot_id,forge_kind,forge_host,forge_project_key,issue_id,status,max_attempts,created_at_ms,updated_at_ms) VALUES ('run','forge','project','cfg','github','github.com','org/repo','42','queued',1,1700000000000,1700000000000)`,
		`INSERT INTO budget_entries(id,kind,scope,scope_id,bucket_start_ms,amount,reason,run_id,operation_key,created_at_ms) VALUES ('charge','attention','run','run',1700000000000,1,'interrupt','run','charge:interrupt',1700000000000)`,
		`INSERT INTO interrupts(id,run_id,generation_key,reason,severity,headline,brief_markdown,options_json,min_modality,links_json,nonce,status,dispatch_state,expires_at_ms,on_expire,max_escalations,charged_budget_entry_id,created_at_ms,updated_at_ms,expires_after_ms,on_max_escalations,base_severity,nonce_issued_at_ms,day_timezone,daily_summary_at,critical_window_ms,critical_total_limit,critical_per_run_limit) VALUES ('interrupt','run','generation','code_review','normal','review','brief','[]','text','[]','nonce','open','held',1700000000010,'hold',0,'charge',1700000000000,1700000000000,10,'hold','normal',1700000000000,'UTC','09:00',900000,5,2)`,
	} {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatalf("populate 0020 database: %v", err)
		}
	}
	binding := `{"arm":"code_review","change_id":"change","head_sha":"head","review_policy_snapshot_digest":"policy"}`
	sum := sha256.Sum256([]byte(binding))
	if _, err := raw.Exec(`INSERT INTO interrupt_command_effect_bindings(interrupt_id,reason,binding_schema_version,binding_json,binding_digest,created_at_ms) VALUES ('interrupt','code_review',1,?,?,1700000000000)`, binding, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("populate 0020 binding: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(ctx, OpenConfig{Path: path, BinaryVersion: "test-binary", Now: time.UnixMilli(testNow + 1)})
	if err != nil {
		t.Fatalf("upgrade populated 0020 database: %v", err)
	}
	var runID string
	if err := upgraded.db.QueryRow(`SELECT run_id FROM interrupts WHERE id='interrupt'`).Scan(&runID); err != nil || runID != "run" {
		t.Fatalf("upgraded interrupt = %q, %v", runID, err)
	}
	if err := upgraded.db.QueryRow(`PRAGMA foreign_key_check`).Scan(new(any)); err != sql.ErrNoRows {
		t.Fatalf("foreign_key_check = %v", err)
	}
	if err := upgraded.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(ctx, OpenConfig{Path: path, BinaryVersion: "test-binary", Now: time.UnixMilli(testNow + 2)}); err != nil {
		t.Fatalf("restart after populated upgrade: %v", err)
	} else {
		defer reopened.Close()
	}
}

func TestPopulated0035To0036FailureRollsBackBindingIndexAndMigrationRecord(t *testing.T) {
	ctx := context.Background()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "sift.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TABLE schema_migrations (version INTEGER NOT NULL PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at_ms INTEGER NOT NULL, binary_version TEXT NOT NULL);
		CREATE TABLE interrupt_command_effect_bindings (binding_digest TEXT NOT NULL);
		INSERT INTO interrupt_command_effect_bindings VALUES ('duplicate'), ('duplicate')`); err != nil {
		t.Fatal(err)
	}
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var migration36 migration
	for _, m := range migrations {
		if m.version == 36 {
			migration36 = m
			break
		}
	}
	if migration36.version != 36 {
		t.Fatal("0036 migration not embedded")
	}
	if err := applyOne(ctx, raw, migration36, "test-binary", time.UnixMilli(testNow)); err == nil {
		t.Fatal("duplicate binding digests accepted by 0036")
	}
	var count int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=36`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed migration recorded count=%d err=%v", count, err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='interrupt_command_effect_bindings_digest'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed migration left unique index count=%d err=%v", count, err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM interrupt_command_effect_bindings`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("failed migration changed populated rows count=%d err=%v", count, err)
	}
}

func TestNewerDatabaseRefused(t *testing.T) {
	db, path := openTestDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Simulate a database written by a newer binary.
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO schema_migrations (version, name, checksum, applied_at_ms, binary_version)
		VALUES (999, '9999_future', 'deadbeef', 0, 'future-binary')`); err != nil {
		t.Fatalf("insert future migration: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	_, err = Open(context.Background(), OpenConfig{Path: path, BinaryVersion: "test-binary", Now: time.UnixMilli(testNow)})
	if !errors.Is(err, ErrDatabaseTooNew) {
		t.Fatalf("Open on newer database: got %v, want ErrDatabaseTooNew", err)
	}
}

func TestChecksumMismatchRefused(t *testing.T) {
	db, path := openTestDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := raw.Exec(`UPDATE schema_migrations SET checksum = 'tampered' WHERE version = 1`); err != nil {
		t.Fatalf("tamper checksum: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	_, err = Open(context.Background(), OpenConfig{Path: path, BinaryVersion: "test-binary", Now: time.UnixMilli(testNow)})
	if !errors.Is(err, ErrMigrationMismatch) {
		t.Fatalf("Open on checksum drift: got %v, want ErrMigrationMismatch", err)
	}
}

func TestOpenRejectsIncompleteConfig(t *testing.T) {
	dir := t.TempDir()
	cases := []OpenConfig{
		{Path: "", BinaryVersion: "v", Now: time.UnixMilli(testNow)},
		{Path: filepath.Join(dir, "a.db"), BinaryVersion: "", Now: time.UnixMilli(testNow)},
		{Path: filepath.Join(dir, "b.db"), BinaryVersion: "v"},
	}
	for i, cfg := range cases {
		if _, err := Open(context.Background(), cfg); err == nil {
			t.Errorf("case %d: Open succeeded with incomplete config %+v", i, cfg)
		}
	}
}
