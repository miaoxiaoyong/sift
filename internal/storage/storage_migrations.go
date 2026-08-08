package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"time"
)

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
