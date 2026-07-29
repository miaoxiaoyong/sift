package storage

import (
	"context"
	"errors"
	"testing"
)

func TestHandoffPermitReplayAndStartedEvidence(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	const now = int64(1000)
	if err := db.SeedProjectForTest(ctx, "cfg", "project", now); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "1", now); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO task_spec_snapshots(id,run_id,version,schema_version,canonical_json,content_digest,created_at_ms) VALUES ('task','run',1,1,'{}','digest',1000)`)
	mustExec(t, db, `INSERT INTO attempts(run_id,attempt_no,phase,generation,backend,agent_id,task_spec_snapshot_id,worktree_path,branch_name,base_ref,base_sha,created_at_ms,updated_at_ms) VALUES ('run',1,'pending',1,'process','agent','task','/work','branch','main','abc',1000,1000)`)
	mustExec(t, db, `INSERT INTO attempt_claims(run_id,attempt_no,generation,launch_operation_key,dispatch_id,bootstrap_nonce_hash,run_token_hash,created_at_ms,updated_at_ms) VALUES ('run',1,1,'launch','dispatch',?,?,1000,1000)`, handoffHash(secret('a')), handoffHash(secret('b')))
	wrapper := WrapperIdentity{PID: 10, StartedAtMS: 1001, Executable: "/wrapper", PGID: 10}
	if err := db.AcquireLaunchClaim(ctx, AcquireLaunchClaim{RunID: "run", AttemptNo: 1, Generation: 1, DispatchID: "dispatch", BootstrapNonce: secret('a'), InstanceID: "instance", Session: secret('c'), Wrapper: wrapper, NowMS: 1002}); err != nil {
		t.Fatal(err)
	}
	permit := PermitSpawn{RunID: "run", AttemptNo: 1, Generation: 1, InstanceID: "instance", Session: secret('c'), Permit: secret('d'), ControlDigest: secret('e'), Wrapper: wrapper, NowMS: 1003}
	wrongWrapper := permit
	wrongWrapper.Wrapper.PID = 99
	if err := db.PermitSpawn(ctx, wrongWrapper); !errors.Is(err, ErrHandoffConflict) {
		t.Fatalf("different wrapper identity = %v", err)
	}
	wrongGeneration := permit
	wrongGeneration.Generation = 2
	if err := db.PermitSpawn(ctx, wrongGeneration); !errors.Is(err, ErrHandoffStale) {
		t.Fatalf("old generation = %v", err)
	}
	if err := db.PermitSpawn(ctx, permit); err != nil {
		t.Fatal(err)
	}
	if err := db.PermitSpawn(ctx, permit); err != nil {
		t.Fatalf("same permit replay: %v", err)
	}
	permit.Permit = secret('f')
	if err := db.PermitSpawn(ctx, permit); !errors.Is(err, ErrHandoffConflict) {
		t.Fatalf("replacement permit = %v", err)
	}
	disposition, err := db.ConfirmStarted(ctx, StartedClaim{RunID: "run", AttemptNo: 1, Generation: 1, InstanceID: "instance", Session: secret('c'), Permit: secret('d'), ControlDigest: secret('e'), Agent: AgentIdentity{PID: 11, StartedAtMS: 1004, Executable: "/agent"}, NowMS: 1004})
	if err != nil || disposition != "running" {
		t.Fatalf("started = %q, %v", disposition, err)
	}
	disposition, err = db.ConfirmStarted(ctx, StartedClaim{RunID: "run", AttemptNo: 1, Generation: 1, InstanceID: "instance", Session: secret('c'), Permit: secret('d'), ControlDigest: secret('e'), Agent: AgentIdentity{PID: 11, StartedAtMS: 1004, Executable: "/agent"}, NowMS: 1005})
	if err != nil || disposition != "duplicate" {
		t.Fatalf("started replay = %q, %v", disposition, err)
	}
	run, err := db.Run(ctx, "run")
	if err != nil || run.Status != RunRunning {
		t.Fatalf("run = %+v, %v", run, err)
	}
}
func secret(c byte) string { return string(make([]byte, 64))[:0] + repeat(c, 64) }
func repeat(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
