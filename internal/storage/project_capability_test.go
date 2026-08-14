package storage

import (
	"context"
	"testing"
	"time"

	"github.com/xsift/sift/internal/forge"
)

func TestAutoMergeCapabilityPersistsFailClosedAcrossRestart(t *testing.T) {
	ctx := context.Background()
	db, path := openTestDB(t)
	if err := db.SeedProjectForTest(ctx, "cfg-cap", "project-cap", testNow); err != nil {
		t.Fatal(err)
	}
	ref := forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo-project-cap"}
	if enabled, err := db.AutoMergeEnabled(ctx, ref); err != nil || enabled {
		t.Fatalf("unproven capability = %v, %v; want false, nil", enabled, err)
	}
	if err := db.UpdateProjectAutoMergeCapability(ctx, "project-cap", true, "startup proof", testNow+1); err != nil {
		t.Fatal(err)
	}
	if enabled, err := db.AutoMergeEnabled(ctx, ref); err != nil || !enabled {
		t.Fatalf("proven capability = %v, %v; want true, nil", enabled, err)
	}
	if err := db.UpdateProjectAutoMergeCapability(ctx, "project-cap", false, "probe ambiguous", testNow+2); err != nil {
		t.Fatal(err)
	}
	var checks int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM events WHERE project_id='project-cap' AND type='project.capability_checked'`).Scan(&checks); err != nil || checks != 2 {
		t.Fatalf("capability audit events = %d, %v; want 2, nil", checks, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, OpenConfig{Path: path, BinaryVersion: "test-binary", Now: time.UnixMilli(testNow + 3)})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if enabled, err := reopened.AutoMergeEnabled(ctx, ref); err != nil || enabled {
		t.Fatalf("restart must retain disabled capability = %v, %v", enabled, err)
	}
}
