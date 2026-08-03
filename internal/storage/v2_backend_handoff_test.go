package storage

import (
	"context"
	"errors"
	"testing"
)

// TestV2HandoffReplayIsBackendParameterized keeps the durable handoff cases
// identical for both hosts. Backend is an attempt fact only; lease, acquire,
// permit and started idempotency must not fork by host.
func TestV2HandoffReplayIsBackendParameterized(t *testing.T) {
	for _, backend := range []string{"process", "tmux"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			db, _ := openTestDB(t)
			if err := db.SeedProjectForTest(ctx, "cfg", "project", 1000); err != nil {
				t.Fatal(err)
			}
			if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "1", 1000); err != nil {
				t.Fatal(err)
			}
			mustExec(t, db, `INSERT INTO task_spec_snapshots(id,run_id,version,schema_version,canonical_json,content_digest,created_at_ms) VALUES ('task','run',1,1,'{}','digest',1000)`)
			mustExec(t, db, `INSERT INTO attempts(run_id,attempt_no,phase,generation,backend,agent_id,task_spec_snapshot_id,worktree_path,branch_name,base_ref,base_sha,created_at_ms,updated_at_ms) VALUES ('run',1,'pending',1,?,'agent','task','/work','branch','main','abc',1000,1000)`, backend)
			mustExec(t, db, `INSERT INTO attempt_claims(run_id,attempt_no,generation,launch_operation_key,dispatch_id,bootstrap_nonce_hash,run_token_hash,created_at_ms,updated_at_ms) VALUES ('run',1,1,'launch','dispatch',?,?,1000,1000)`, handoffHash(secret('a')), handoffHash(secret('b')))
			wrapper := WrapperIdentity{PID: 10, StartedAtMS: 1001, Executable: "/wrapper", PGID: 10}
			acquire := AcquireLaunchClaim{RunID: "run", AttemptNo: 1, Generation: 1, DispatchID: "dispatch", BootstrapNonce: secret('a'), InstanceID: "instance", Session: secret('c'), Wrapper: wrapper, NowMS: 1002}
			if err := db.AcquireLaunchClaim(ctx, acquire); err != nil {
				t.Fatal(err)
			}
			// Simulate a lost acquire response: the same wrapper must converge,
			// while a concurrent wrapper cannot become a second owner.
			if err := db.AcquireLaunchClaim(ctx, acquire); err != nil {
				t.Fatalf("acquire replay: %v", err)
			}
			second := acquire
			second.InstanceID, second.Session = "second-instance", secret('g')
			if err := db.AcquireLaunchClaim(ctx, second); !errors.Is(err, ErrHandoffConflict) {
				t.Fatalf("same-generation second wrapper = %v, want conflict", err)
			}
			if owners := countRows(t, db, "attempts WHERE run_id='run' AND wrapper_instance_id IS NOT NULL"); owners != 1 {
				t.Fatalf("same-generation owners = %d, want 1", owners)
			}
			permit := PermitSpawn{RunID: "run", AttemptNo: 1, Generation: 1, InstanceID: "instance", Session: secret('c'), Permit: secret('d'), ControlDigest: secret('e'), ControlNonceHash: secret('f'), Wrapper: wrapper, NowMS: 1003}
			if err := db.PermitSpawn(ctx, permit); err != nil {
				t.Fatal(err)
			}
			if err := db.PermitSpawn(ctx, permit); err != nil {
				t.Fatalf("permit replay: %v", err)
			}
			disposition, err := db.ConfirmStarted(ctx, StartedClaim{RunID: "run", AttemptNo: 1, Generation: 1, InstanceID: "instance", Session: secret('c'), Permit: secret('d'), ControlDigest: secret('e'), Agent: AgentIdentity{PID: 11, StartedAtMS: 1004, Executable: "/agent"}, NowMS: 1004})
			if err != nil || disposition != "running" {
				t.Fatalf("started: %q, %v", disposition, err)
			}
			disposition, err = db.ConfirmStarted(ctx, StartedClaim{RunID: "run", AttemptNo: 1, Generation: 1, InstanceID: "instance", Session: secret('c'), Permit: secret('d'), ControlDigest: secret('e'), Agent: AgentIdentity{PID: 11, StartedAtMS: 1004, Executable: "/agent"}, NowMS: 1005})
			if err != nil || disposition != "duplicate" {
				t.Fatalf("started replay: %q, %v", disposition, err)
			}
			// A sleeping old generation is rejected at every handoff verb, not
			// only permit. This is deliberately checked after started so no
			// later response can overwrite the current owner or identity.
			oldAcquire := acquire
			oldAcquire.Generation = 2
			if err := db.AcquireLaunchClaim(ctx, oldAcquire); !errors.Is(err, ErrHandoffStale) {
				t.Fatalf("old acquire = %v, want stale", err)
			}
			stale := permit
			stale.Generation = 2
			if err := db.PermitSpawn(ctx, stale); !errors.Is(err, ErrHandoffStale) {
				t.Fatalf("old permit = %v, want stale", err)
			}
			oldStarted := StartedClaim{RunID: "run", AttemptNo: 1, Generation: 2, InstanceID: "instance", Session: secret('c'), Permit: secret('d'), ControlDigest: secret('e'), Agent: AgentIdentity{PID: 11, StartedAtMS: 1004, Executable: "/agent"}, NowMS: 1006}
			if _, err := db.ConfirmStarted(ctx, oldStarted); !errors.Is(err, ErrHandoffStale) {
				t.Fatalf("old started = %v, want stale", err)
			}
		})
	}
}
