package storage

import (
	"context"
	"testing"
)

func TestT7ReplayEvidenceIsFrozenAndIdempotent(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg-t7", "project/alpha", testNow); err != nil {
		t.Fatal(err)
	}
	cmd := AppendT7ReplayEvidenceCmd{
		Scope: "project", ProjectID: "project/alpha", TaskKind: "bug",
		WindowStartMS: 100, WindowEndMS: 200,
		DatasetVersion: "dataset/v1", GateVersion: "gate/v1",
		TotalSamples: 10, NegativeSamples: 2, LeakCount: 1, FalseBlockCount: 1,
		CreatedAtMS: 200,
	}
	first, err := db.AppendT7ReplayEvidence(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	again, err := db.AppendT7ReplayEvidence(ctx, cmd)
	if err != nil || again != first {
		t.Fatalf("replay evidence replay id=%s want=%s err=%v", again, first, err)
	}
	cmd.DatasetVersion = "dataset/v2"
	if _, err := db.AppendT7ReplayEvidence(ctx, cmd); err == nil {
		t.Fatal("conflicting replay evidence replaced frozen window")
	}
	if err := mustFail(t, db, `UPDATE t7_replay_evidence SET gate_version='gate/v2' WHERE id=?`, first); err == nil {
		t.Fatal("T7 replay evidence update succeeded")
	}
}

func TestPendingT7AggregateUsesExactScopeAndFrozenWindow(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg-t7", "project/alpha", testNow); err != nil {
		t.Fatal(err)
	}
	version := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	mustExec(t, db, `INSERT INTO certifications(task_kind,certification_version,total_samples,negative_samples,leak_count,false_block_count,certified,evidence_digest,updated_at_ms,certification_rules_version,window_start_ms,window_end_ms) VALUES('bug',?,1,1,0,0,1,?,?,?,?,?)`, version, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", 200, "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", 100, 200)
	mustExec(t, db, `INSERT INTO certification_current(task_kind,certification_version,version,updated_at_ms) VALUES('bug',?,1,200)`, version)
	for _, cmd := range []AppendT7ReplayEvidenceCmd{
		{Scope: "global", TaskKind: "bug", WindowStartMS: 100, WindowEndMS: 200},
		{Scope: "project", ProjectID: "project/alpha", TaskKind: "all", WindowStartMS: 200, WindowEndMS: 300},
	} {
		cmd.DatasetVersion, cmd.GateVersion, cmd.CreatedAtMS = "dataset/v1", "gate/v1", 300
		if _, err := db.AppendT7ReplayEvidence(ctx, cmd); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := db.PendingT7Aggregates(ctx, 300, 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending=%d err=%v", len(pending), err)
	}
	if pending[0].AggregateKey != "aggregate:v1:global:bug:100:200" || pending[0].ProjectID != "" {
		t.Fatalf("global identity=%+v", pending[0])
	}
	if pending[1].AggregateKey != "aggregate:v1:project:cHJvamVjdC9hbHBoYQ:all:200:300" || pending[1].ProjectID != "project/alpha" {
		t.Fatalf("project identity=%+v", pending[1])
	}
}
