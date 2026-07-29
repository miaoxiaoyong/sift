package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/miaoxiaoyong/sift/internal/config"
)

func TestRecordBootstrapDigestIsIdempotentForSameDispatch(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	snapshot := &config.Snapshot{Config: &config.Config{Version: 1}, Hash: "launch-digest-config", CanonicalJSON: []byte(`{"version":1}`)}
	if err := db.ActivateConfig(ctx, snapshot, "test", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedLaunchRunForTest(ctx, "run", "project", "cfg", testNow, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	boot, err := db.StartDaemonBoot(ctx, "hash-cfg", "test", 1, 123, testNow)
	if err != nil {
		t.Fatal(err)
	}
	attempts, operations, err := db.StartupRecoveryPending(ctx, boot)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range attempts {
		if err := db.ApplyStartupRecoveryAction(ctx, StartupRecoveryAction{BootID: boot, RunID: attempt.RunID, AttemptNo: attempt.AttemptNo, ExpectedGeneration: attempt.Generation, ObservationDigest: "pending", Action: "supervise", NowMS: testNow + 1}); err != nil {
			t.Fatal(err)
		}
	}
	for _, operation := range operations {
		if err := db.ApplyStartupRecoveryAction(ctx, StartupRecoveryAction{BootID: boot, OperationID: operation.ID, ExpectedOperationVersion: operation.Version, ObservationDigest: "launch", Action: "converge_operation", NowMS: testNow + 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CompleteStartupRecovery(ctx, boot, testNow+1); err != nil {
		t.Fatal(err)
	}
	claim, err := db.ClaimLaunchOperation(ctx, boot, "worker", testNow+2, 100)
	if err != nil || claim == nil {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	dispatch, err := db.PrepareLaunchDispatch(ctx, *claim, "dispatch", secret('a'), secret('b'), testNow+2)
	if err != nil {
		t.Fatal(err)
	}
	first := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := db.RecordBootstrapDigest(ctx, *claim, dispatch.DispatchID, first, testNow+3); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordBootstrapDigest(ctx, *claim, dispatch.DispatchID, first, testNow+4); err != nil {
		t.Fatalf("same digest replay: %v", err)
	}
	if err := db.RecordBootstrapDigest(ctx, *claim, dispatch.DispatchID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", testNow+5); !errors.Is(err, ErrRejectedStaleWorker) {
		t.Fatalf("different digest = %v, want stale worker", err)
	}
}
