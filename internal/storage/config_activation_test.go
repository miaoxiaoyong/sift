package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
)

func TestActivateConfigProjectsOnEmptyDB(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1234)
	db, err := Open(ctx, OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	snapshot := &config.Snapshot{
		Config: &config.Config{Version: 1, Projects: []config.Project{
			{ID: "enabled", Repo: "/repo/enabled", Enabled: true, Forge: config.ForgeRef{Kind: config.ForgeKindGitHub, Host: "github.com", Project: "org/repo"}},
			{ID: "disabled", Repo: "/repo/disabled", Enabled: false, Forge: config.ForgeRef{Kind: config.ForgeKindGitLab, Host: "gitlab.com", Project: "org/repo"}},
		}},
		Hash:          "hash-activation",
		CanonicalJSON: []byte(`{"version":1}`),
	}
	if err := db.ActivateConfig(ctx, snapshot, "test", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}

	var snapshots, projects, enabled int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM config_snapshots`).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&projects); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM projects WHERE enabled=1`).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if snapshots != 1 || projects != 2 || enabled != 1 {
		t.Fatalf("activation counts: snapshots=%d projects=%d enabled=%d", snapshots, projects, enabled)
	}

	// Re-activating the same fingerprint is idempotent for the immutable
	// snapshot and refreshes the current project projection in one transaction.
	if err := db.ActivateConfig(ctx, snapshot, "test", now.Add(time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM config_snapshots`).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if snapshots != 1 {
		t.Fatalf("same config hash created %d snapshots, want 1", snapshots)
	}
}
