package daemon

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/miaoxiaoyong/sift/internal/config"
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
