package storage

import (
	"context"
	"testing"

	"github.com/miaoxiaoyong/sift/internal/config"
)

func TestLaunchClaimWaitsForCurrentBootRecoveryBarrier(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	snapshot := &config.Snapshot{Config: &config.Config{Version: 1}, Hash: "boot-config", CanonicalJSON: []byte(`{"version":1}`)}
	if err := db.ActivateConfig(ctx, snapshot, "test", testNow); err != nil {
		t.Fatal(err)
	}
	boot, err := db.StartDaemonBoot(ctx, snapshot.Hash, "test", 1, 123, testNow)
	if err != nil {
		t.Fatal(err)
	}
	op := Operation{Key: "launch:barrier", Kind: OperationLaunchAgent, Payload: []byte(`{"schema_version":1}`)}
	if _, err := db.EnqueueOperation(ctx, op, testNow); err != nil {
		t.Fatal(err)
	}
	if claim, err := db.ClaimOutboxOperation(ctx, "generic", testNow, 10); err != nil || claim != nil {
		t.Fatalf("generic claim = %#v, %v", claim, err)
	}
	if claim, err := db.ClaimLaunchOperation(ctx, boot, "launch", testNow, 10); err != nil || claim != nil {
		t.Fatalf("claim before recovery = %#v, %v", claim, err)
	}
	if err := db.CompleteStartupRecovery(ctx, boot, testNow+1); err != nil {
		t.Fatal(err)
	}
	claim, err := db.ClaimLaunchOperation(ctx, boot, "launch", testNow+1, 10)
	if err != nil || claim == nil || claim.Kind != OperationLaunchAgent {
		t.Fatalf("claim after recovery = %#v, %v", claim, err)
	}
	if err := db.CompleteOutboxAttempt(ctx, *claim, CompleteOutcome{State: OperationSucceeded, NowMS: testNow + 2}); err != nil {
		t.Fatal(err)
	}
	newBoot, err := db.StartDaemonBoot(ctx, snapshot.Hash, "test", 1, 124, testNow+3)
	if err != nil {
		t.Fatal(err)
	}
	if newBoot == boot {
		t.Fatal("restart reused boot identity")
	}
	if err := db.CompleteStartupRecovery(ctx, boot, testNow+4); err != ErrRejectedStale {
		t.Fatalf("second completion = %v, want stale", err)
	}
}
