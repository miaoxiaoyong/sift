package storage

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/miaoxiaoyong/sift/internal/config"
	runtimepkg "github.com/miaoxiaoyong/sift/internal/runtime"
)

func TestQualificationDispatchBindsExactKeyOnce(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	if err := db.ActivateConfig(ctx, &config.Snapshot{Config: &config.Config{Version: 1}, Hash: "cfg", CanonicalJSON: []byte(`{"version":1}`)}, "test", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedLaunchRunForTest(ctx, "run", "project", "cfg", testNow, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	boot, err := db.StartDaemonBoot(ctx, "cfg", "test", 1, 1, testNow)
	if err != nil {
		t.Fatal(err)
	}
	attempts, operations, err := db.StartupRecoveryPending(ctx, boot)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range attempts {
		if err := db.ApplyStartupRecoveryAction(ctx, StartupRecoveryAction{BootID: boot, RunID: a.RunID, AttemptNo: a.AttemptNo, ExpectedGeneration: a.Generation, ObservationDigest: "a", Action: "supervise", NowMS: testNow + 1}); err != nil {
			t.Fatal(err)
		}
	}
	for _, o := range operations {
		if err := db.ApplyStartupRecoveryAction(ctx, StartupRecoveryAction{BootID: boot, OperationID: o.ID, ExpectedOperationVersion: o.Version, ObservationDigest: "o", Action: "converge_operation", NowMS: testNow + 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CompleteStartupRecovery(ctx, boot, testNow+1); err != nil {
		t.Fatal(err)
	}
	claim, err := db.ClaimLaunchOperation(ctx, boot, "worker", testNow+2, 100)
	if err != nil || claim == nil {
		t.Fatalf("claim=%#v err=%v", claim, err)
	}
	key := qualificationDispatchKey(t, "a")
	if _, err := db.PrepareLaunchDispatchWithQualification(ctx, *claim, "dispatch", strings.Repeat("a", 64), strings.Repeat("b", 64), key, testNow+2); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := db.db.QueryRow(`SELECT topology_qualification_key FROM attempts WHERE run_id='run' AND attempt_no=1`).Scan(&got); err != nil || got != key {
		t.Fatalf("bound key=%q err=%v want=%q", got, err, key)
	}
	if _, err := db.PrepareLaunchDispatchWithQualification(ctx, *claim, "other", strings.Repeat("a", 64), strings.Repeat("b", 64), qualificationDispatchKey(t, "b"), testNow+3); !errors.Is(err, ErrLaunchDispatchPrepared) {
		t.Fatalf("dispatch replay=%v, want prepared", err)
	}
}

func TestQualificationBinaryReplacementProducesNewKey(t *testing.T) {
	first, second := qualificationDispatchKey(t, "a"), qualificationDispatchKey(t, "b")
	if first == second {
		t.Fatalf("binary replacement retained qualification key %q", first)
	}
}

func qualificationDispatchKey(t *testing.T, binary string) string {
	t.Helper()
	q := runtimepkg.Qualification{MethodVersion: runtimepkg.TopologyMethodVersion, AgentID: "agent", AgentDefinitionHash: strings.Repeat("a", 64), ExecutablePath: "/resolved/agent", ExecutableSHA256: strings.Repeat(binary, 64), VersionOutputDigest: strings.Repeat("c", 64), GOOS: "linux", GOARCH: "amd64"}
	key, err := runtimepkg.QualificationKey(q)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
