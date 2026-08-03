package daemon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/controlplane"
	runtimepkg "github.com/miaoxiaoyong/sift/internal/runtime"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

type recoveryInspector struct{ observation runtimepkg.ProcessObservation }

func (i recoveryInspector) Observe(context.Context, runtimepkg.ProcessIdentity) (runtimepkg.ProcessObservation, error) {
	return i.observation, nil
}

type recoverySignaler struct{ calls int }

func (s *recoverySignaler) SignalGroup(int, syscall.Signal) error { s.calls++; return nil }

func seedRecoveryCoordinator(t *testing.T, phase string, heartbeat int64) (*storage.DB, *sql.DB, storage.RecoveryAttempt, time.Time) {
	t.Helper()
	ctx := context.Background()
	now := time.UnixMilli(10_000)
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SeedProjectForTest(ctx, "cfg", "project", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "1", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", db.Path())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close(); db.Close() })
	agentColumns, agentValues := "", ""
	if phase == "running" {
		agentColumns = ",agent_pid,agent_started_at_ms,agent_executable"
		agentValues = ",11,1001,'/agent'"
	}
	for _, statement := range []string{
		`INSERT INTO task_spec_snapshots(id,run_id,version,schema_version,canonical_json,content_digest,created_at_ms) VALUES ('task','run',1,1,'{}','digest',10000)`,
		`INSERT INTO attempts(run_id,attempt_no,phase,generation,backend,agent_id,task_spec_snapshot_id,worktree_path,branch_name,base_ref,base_sha,wrapper_pid,wrapper_started_at_ms,wrapper_executable,wrapper_pgid,wrapper_instance_id,control_nonce_hash,heartbeat_at_ms` + agentColumns + `,created_at_ms,updated_at_ms) VALUES ('run',1,'` + phase + `',1,'process','agent','task','/work','branch','main','abc',10,1000,'/wrapper',10,'instance','nonce',` + sqlNumber(heartbeat) + agentValues + `,10000,10000)`,
		`INSERT INTO attempt_claims(run_id,attempt_no,generation,launch_operation_key,wrapper_instance_id,wrapper_session_hash,created_at_ms,updated_at_ms) VALUES ('run',1,1,'launch','instance','session',10000,10000)`,
	} {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	attempts, err := db.RecoveryAttempts(ctx)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("recovery attempts = %#v, %v", attempts, err)
	}
	return db, raw, attempts[0], now
}

func sqlNumber(v int64) string { return strconv.FormatInt(v, 10) }

func TestProductionResultConsumerPreservesAgentLogRelayFailure(t *testing.T) {
	db, raw, _, now := seedRecoveryCoordinator(t, "running", 0)
	root := t.TempDir()
	resultPath := filepath.Join(root, "runs", "run", "attempts", "1", "result.json")
	if err := os.MkdirAll(filepath.Dir(resultPath), 0700); err != nil {
		t.Fatal(err)
	}
	result, err := json.Marshal(map[string]any{
		"schema_version": 1, "run_id": "run", "attempt_no": 1, "generation": 1,
		"wrapper_instance_id": "instance", "agent_identity": map[string]any{"pid": 11, "started_at_ms": 1001, "executable": "/agent"},
		"exit_code": 1, "signal": nil, "failure_reason": "agent_log_relay_failed", "finished_at_ms": now.UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, result, 0600); err != nil {
		t.Fatal(err)
	}
	attempt, err := db.RecoveryAttemptForRun(context.Background(), "run")
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &TerminationCoordinator{DB: db, ControlRoot: root, Now: func() time.Time { return now }}
	if disposition, err := coordinator.resolveLateFact(context.Background(), attempt); err != nil || disposition != storage.AttemptRaceDuplicate {
		t.Fatalf("late result disposition=%q err=%v", disposition, err)
	}
	var failureReason string
	if err := raw.QueryRow(`SELECT result_failure_reason FROM attempts WHERE run_id='run' AND attempt_no=1`).Scan(&failureReason); err != nil {
		t.Fatal(err)
	}
	if failureReason != "agent_log_relay_failed" {
		t.Fatalf("stored failure reason = %q", failureReason)
	}
}

func TestRecoverKeepsLiveStartingOwner(t *testing.T) {
	db, raw, attempt, now := seedRecoveryCoordinator(t, "starting", 0)
	signaler := &recoverySignaler{}
	coordinator := &TerminationCoordinator{DB: db, Terminator: runtimepkg.Terminator{Inspector: recoveryInspector{observation: runtimepkg.ProcessObservation{Exists: true, ProcessIdentity: runtimepkg.ProcessIdentity{PID: attempt.WrapperPID, StartedAtMS: attempt.WrapperStartedAtMS, Executable: attempt.WrapperExecutable, PGID: attempt.WrapperPGID, ControlNonceHash: attempt.ControlNonceHash}}}, Signaler: signaler}, Runtime: config.Runtime{HeartbeatStaleAfter: time.Second}, AttentionDailyQuota: recoveryQuota(), Now: func() time.Time { return now }}
	if err := coordinator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	var phase string
	if err := raw.QueryRow(`SELECT phase FROM attempts WHERE run_id='run'`).Scan(&phase); err != nil || phase != "starting" || signaler.calls != 0 {
		t.Fatalf("phase/signals = %q/%d, %v", phase, signaler.calls, err)
	}
}

func TestRecoverStartupClassifiesAttemptBeforeBootBarrier(t *testing.T) {
	db, _, attempt, now := seedRecoveryCoordinator(t, "starting", 0)
	boot, err := db.StartDaemonBoot(context.Background(), "hash-cfg", "test", 1, 123, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &TerminationCoordinator{DB: db, Terminator: runtimepkg.Terminator{Inspector: recoveryInspector{observation: runtimepkg.ProcessObservation{Exists: true, ProcessIdentity: runtimepkg.ProcessIdentity{PID: attempt.WrapperPID, StartedAtMS: attempt.WrapperStartedAtMS, Executable: attempt.WrapperExecutable, PGID: attempt.WrapperPGID, ControlNonceHash: attempt.ControlNonceHash}}}}, Runtime: config.Runtime{HeartbeatStaleAfter: time.Second}, AttentionDailyQuota: recoveryQuota(), Now: func() time.Time { return now }}
	if err := coordinator.RecoverStartup(context.Background(), boot); err != nil {
		t.Fatal(err)
	}
	attempts, operations, err := db.StartupRecoveryPending(context.Background(), boot)
	if err != nil || len(attempts) != 0 || len(operations) != 0 {
		t.Fatalf("pending recovery candidates = %#v %#v, %v", attempts, operations, err)
	}
	if err := db.CompleteStartupRecovery(context.Background(), boot, now.UnixMilli()+1); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverStartupFrozenCandidatesEmitStartupStallBeforeOpeningBarrier(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, root string)
	}{
		{
			name: "corrupt bootstrap",
			setup: func(t *testing.T, root string) {
				t.Helper()
				path := filepath.Join(root, "runs", "run", "attempts", "1", "bootstrap.json")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(`{`), 0600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "wide bootstrap permissions",
			setup: func(t *testing.T, root string) {
				t.Helper()
				path := filepath.Join(root, "runs", "run", "attempts", "1", "bootstrap.json")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(`{}`), 0644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "ambiguous control",
			setup: func(t *testing.T, root string) {
				t.Helper()
				path := filepath.Join(root, "runs", "run", "attempts", "1", "control.json")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, raw, _, now := seedRecoveryCoordinator(t, "pending", 0)
			root := t.TempDir()
			tc.setup(t, root)
			boot, err := db.StartDaemonBoot(context.Background(), "hash-cfg", "test", 1, 123, now.UnixMilli())
			if err != nil {
				t.Fatal(err)
			}
			coordinator := &TerminationCoordinator{DB: db, ControlRoot: root, AttentionDailyQuota: recoveryQuota(), Now: func() time.Time { return now }}
			if err := coordinator.RecoverStartup(context.Background(), boot); err != nil {
				t.Fatal(err)
			}

			var state, reason, status string
			var isolatedAt, completed sql.NullInt64
			if err := raw.QueryRow(`SELECT a.isolation_state,a.isolation_reason,a.isolated_at_ms,r.status,b.recovery_completed_at_ms FROM attempts a JOIN runs r ON r.id=a.run_id JOIN daemon_boots b ON b.id=? WHERE a.run_id='run' AND a.attempt_no=1`, boot).Scan(&state, &reason, &isolatedAt, &status, &completed); err != nil {
				t.Fatal(err)
			}
			if state != "frozen" || reason != "process_identity_unknown" || !isolatedAt.Valid || status != "waiting_human" || completed.Valid {
				t.Fatalf("pre-barrier projection = state=%q reason=%q isolated=%v run=%q completed=%v", state, reason, isolatedAt, status, completed)
			}
			for _, table := range []string{"interrupts", "budget_entries", "outbox_operations", "interrupt_deliveries"} {
				var count int
				if err := raw.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 1 {
					t.Fatalf("%s count = %d, %v", table, count, err)
				}
			}
			if err := db.CompleteStartupRecovery(context.Background(), boot, now.UnixMilli()+1); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecoverStartupRedispatchesPendingAttemptBeforeOpeningBarrier(t *testing.T) {
	db, raw, attempt, now := seedRecoveryCoordinator(t, "pending", 0)
	boot, err := db.StartDaemonBoot(context.Background(), "hash-cfg", "test", 1, 123, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &TerminationCoordinator{DB: db, Runtime: config.Runtime{HeartbeatStaleAfter: time.Second}, AttentionDailyQuota: recoveryQuota(), Now: func() time.Time { return now }}
	if err := coordinator.RecoverStartup(context.Background(), boot); err != nil {
		t.Fatal(err)
	}
	var generation int
	var operations int
	if err := raw.QueryRow(`SELECT generation FROM attempts WHERE run_id='run' AND attempt_no=1`).Scan(&generation); err != nil || generation != attempt.Generation+1 {
		t.Fatalf("generation = %d, %v", generation, err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM outbox_operations WHERE run_id='run' AND attempt_no=1 AND kind='launch_agent' AND state='pending'`).Scan(&operations); err != nil || operations != 1 {
		t.Fatalf("pending dispatches = %d, %v", operations, err)
	}
	if err := db.CompleteStartupRecovery(context.Background(), boot, now.UnixMilli()+1); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverStartupReusesValidatedPreparedBootstrap(t *testing.T) {
	db, raw, attempt, now := seedRecoveryCoordinator(t, "pending", 0)
	key := storage.LaunchOperationKey(attempt.RunID, attempt.AttemptNo, attempt.Generation)
	nonce, token, dispatch := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccccccccccccccc"
	if _, err := raw.Exec(`INSERT INTO outbox_operations(id,operation_key,kind,run_id,attempt_no,state,payload_schema_version,payload_json,payload_digest,next_attempt_at_ms,created_at_ms,updated_at_ms) VALUES ('op',?,'launch_agent','run',1,'pending',1,'{}','digest',10000,10000,10000)`, key); err != nil {
		t.Fatal(err)
	}
	hash := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	if _, err := raw.Exec(`UPDATE attempt_claims SET launch_operation_key=?,dispatch_id=?,bootstrap_nonce_hash=?,run_token_hash=? WHERE run_id='run' AND attempt_no=1`, key, dispatch, hash(nonce), hash(token)); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	runDir := filepath.Join(root, "runs", "run", "attempts", "1")
	if err := os.MkdirAll(runDir, 0700); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := json.Marshal(runtimepkg.Bootstrap{SchemaVersion: 2, ProtocolMajor: controlplane.ProtocolMajor, ProtocolMinor: controlplane.ProtocolMinor, DaemonVersion: controlplane.Version, WrapperVersion: controlplane.Version, RunID: "run", AttemptNo: 1, Generation: 1, DispatchID: dispatch, BootstrapNonce: nonce, RunToken: token})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "bootstrap.json"), bootstrap, 0600); err != nil {
		t.Fatal(err)
	}
	boot, err := db.StartDaemonBoot(context.Background(), "hash-cfg", "test", 1, 123, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &TerminationCoordinator{DB: db, ControlRoot: root, Now: func() time.Time { return now }}
	if err := coordinator.RecoverStartup(context.Background(), boot); err != nil {
		t.Fatal(err)
	}
	var generation int
	var state, gotDispatch string
	if err := raw.QueryRow(`SELECT a.generation,o.state,c.dispatch_id FROM attempts a JOIN attempt_claims c ON c.run_id=a.run_id AND c.attempt_no=a.attempt_no JOIN outbox_operations o ON o.id='op' WHERE a.run_id='run'`).Scan(&generation, &state, &gotDispatch); err != nil {
		t.Fatal(err)
	}
	if generation != 1 || state != "pending" || gotDispatch != dispatch {
		t.Fatalf("prepared recovery changed generation/state/dispatch = %d/%s/%s", generation, state, gotDispatch)
	}
	if err := db.CompleteStartupRecovery(context.Background(), boot, now.UnixMilli()+1); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverRoutesStaleHeartbeatThroughTermination(t *testing.T) {
	db, raw, attempt, now := seedRecoveryCoordinator(t, "running", nowMillis(time.UnixMilli(10_000).Add(-2*time.Second)))
	signaler := &recoverySignaler{}
	coordinator := &TerminationCoordinator{DB: db, Terminator: runtimepkg.Terminator{Inspector: recoveryInspector{observation: runtimepkg.ProcessObservation{Exists: true, ProcessIdentity: runtimepkg.ProcessIdentity{PID: attempt.WrapperPID, StartedAtMS: attempt.WrapperStartedAtMS, Executable: attempt.WrapperExecutable, PGID: attempt.WrapperPGID, ControlNonceHash: attempt.ControlNonceHash}}}, Signaler: signaler, Sleep: func(context.Context, time.Duration) error { return nil }}, Runtime: config.Runtime{HeartbeatStaleAfter: time.Second, TerminationTermGrace: 0, TerminationKillGrace: 0, AbsenceRecheckCount: 1}, AttentionDailyQuota: recoveryQuota(), Now: func() time.Time { return now }}
	if err := coordinator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := raw.QueryRow(`SELECT isolation_state FROM attempts WHERE run_id='run'`).Scan(&state); err != nil || state != "frozen" || signaler.calls != 2 {
		t.Fatalf("isolation/signals = %q/%d, %v", state, signaler.calls, err)
	}
}

func nowMillis(t time.Time) int64 { return t.UnixMilli() }

func recoveryQuota() map[storage.InterruptSeverity]int {
	return map[storage.InterruptSeverity]int{storage.SeverityLow: 1, storage.SeverityNormal: 1, storage.SeverityHigh: 1}
}
