package storage

import (
	"context"
	"database/sql"
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
	if version != 5 {
		t.Fatalf("SchemaVersion = %d, want 5", version)
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
	if count != 5 {
		t.Fatalf("schema_migrations rows = %d, want 5 after reopen", count)
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
