package daemon

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
	runtimepkg "github.com/miaoxiaoyong/sift/internal/runtime"
)

// TestRecoveryRowsBackendParameterized names every non-human recovery phase.
// Backend selection is deliberately only an attempt fact: the recovery
// verdict is made from wrapper/control/process evidence for both hosts.
type recoverySequenceInspector struct {
	observations []runtimepkg.ProcessObservation
	calls        int
}

func (i *recoverySequenceInspector) Observe(context.Context, runtimepkg.ProcessIdentity) (runtimepkg.ProcessObservation, error) {
	observation := i.observations[len(i.observations)-1]
	if i.calls < len(i.observations) {
		observation = i.observations[i.calls]
	}
	i.calls++
	return observation, nil
}

func TestRecoveryRowsBackendParameterized(t *testing.T) {
	rows := []struct {
		name  string
		phase string
		want  string
	}{
		{"R01_pending_no_execution_body", "pending", "redispatch"},
		{"R02_pending_prepared_without_control", "pending", "redispatch"},
		{"R03_starting_owner_unknown", "starting", "freeze"},
		{"R04_starting_owner_absent", "starting", "freeze"},
		{"R05_spawning_owner_unknown", "spawning", "freeze"},
		{"R06_spawning_started_not_committed", "spawning", "freeze"},
		{"R07_spawning_result_missing", "spawning", "freeze"},
		{"R08_spawning_process_evidence_unknown", "spawning", "freeze"},
		{"R09_spawning_group_residual", "spawning", "freeze"},
		{"R10_spawning_no_identity", "spawning", "freeze"},
		{"R11_running_success_evidence_missing", "running", "freeze"},
		{"R12_running_failed_evidence_missing", "running", "freeze"},
		{"R13_running_fresh_identity_unknown", "running", "freeze"},
		{"R14_running_stale_heartbeat", "running", "freeze"},
		{"R15_running_tmux_session_present_owner_absent", "running", "freeze"},
		{"R16_running_owner_present_tmux_session_absent", "running", "freeze"},
	}
	for _, backend := range []string{"process", "tmux"} {
		for _, row := range rows {
			t.Run(backend+"/"+row.name, func(t *testing.T) {
				db, raw, attempt, now := seedRecoveryCoordinator(t, row.phase, 0)
				if _, err := raw.Exec(`UPDATE attempts SET backend=? WHERE run_id='run'`, backend); err != nil {
					t.Fatal(err)
				}
				boot, err := db.StartDaemonBoot(context.Background(), "hash-cfg", "test", 1, 123, now.UnixMilli())
				if err != nil {
					t.Fatal(err)
				}
				if row.phase == "pending" {
					c := &TerminationCoordinator{DB: db, Runtime: config.Runtime{HeartbeatStaleAfter: time.Second}, AttentionDailyQuota: recoveryQuota(), Now: func() time.Time { return now }}
					if err := c.RecoverStartup(context.Background(), boot); err != nil {
						t.Fatal(err)
					}
					var generation, dispatches int
					if err := raw.QueryRow(`SELECT generation FROM attempts WHERE run_id='run'`).Scan(&generation); err != nil {
						t.Fatal(err)
					}
					if err := raw.QueryRow(`SELECT count(*) FROM outbox_operations WHERE run_id='run' AND kind='launch_agent' AND state='pending'`).Scan(&dispatches); err != nil {
						t.Fatal(err)
					}
					if generation != attempt.Generation+1 || dispatches != 1 || row.want != "redispatch" {
						t.Fatalf("pending recovery generation=%d dispatches=%d", generation, dispatches)
					}
					return
				}

				// A mismatching observation represents PID/PGID reuse or otherwise
				// untrusted identity. It must not reach the signaler.
				c := &TerminationCoordinator{
					DB: db, Terminator: runtimepkg.Terminator{Inspector: recoveryInspector{observation: runtimepkg.ProcessObservation{Exists: true, ProcessIdentity: runtimepkg.ProcessIdentity{PID: 999, StartedAtMS: 999, Executable: "/other", PGID: 999, ControlNonceHash: "other"}}}, Signaler: &recoverySignaler{}},
					Runtime: config.Runtime{HeartbeatStaleAfter: time.Second, AbsenceRecheckCount: 1}, AttentionDailyQuota: recoveryQuota(), Now: func() time.Time { return now },
				}
				if err := c.RecoverStartup(context.Background(), boot); err != nil {
					t.Fatal(err)
				}
				assertSingleFrozenStartupStall(t, raw, boot, "process_identity_unknown")
			})
		}
	}
}

func assertSingleFrozenStartupStall(t *testing.T, raw *sql.DB, boot, wantReason string) {
	t.Helper()
	var isolation, reason, status string
	if err := raw.QueryRow(`SELECT a.isolation_state,a.isolation_reason,r.status FROM attempts a JOIN runs r ON r.id=a.run_id WHERE a.run_id='run'`).Scan(&isolation, &reason, &status); err != nil {
		t.Fatal(err)
	}
	if isolation != "frozen" || reason != wantReason || status != "waiting_human" {
		t.Fatalf("recovery projection = %s/%s/%s", isolation, reason, status)
	}
	var interrupts int
	if err := raw.QueryRow(`SELECT count(*) FROM interrupts WHERE run_id='run' AND reason='startup_stall'`).Scan(&interrupts); err != nil {
		t.Fatal(err)
	}
	if interrupts != 1 {
		t.Fatalf("startup_stall interrupts=%d, want 1", interrupts)
	}
}

func TestRecoveryFailsClosedOnGroupRefusalAndDetachedDescendant(t *testing.T) {
	for _, backend := range []string{"process", "tmux"} {
		t.Run(backend, func(t *testing.T) {
			db, raw, attempt, now := seedRecoveryCoordinator(t, "running", 0)
			if _, err := raw.Exec(`UPDATE attempts SET backend=?,topology_qualification_key='exact-key' WHERE run_id='run'`, backend); err != nil {
				t.Fatal(err)
			}
			signaler := &recoverySignaler{}
			c := &TerminationCoordinator{DB: db, Terminator: runtimepkg.Terminator{Inspector: &recoverySequenceInspector{observations: []runtimepkg.ProcessObservation{{Exists: true, ProcessIdentity: runtimepkg.ProcessIdentity{PID: 999, StartedAtMS: 999, Executable: "/other", PGID: 999, ControlNonceHash: "other"}}, {Exists: true, ProcessIdentity: runtimepkg.ProcessIdentity{PID: attempt.WrapperPID, StartedAtMS: attempt.WrapperStartedAtMS, Executable: attempt.WrapperExecutable, PGID: attempt.WrapperPGID, ControlNonceHash: attempt.ControlNonceHash}}, {Exists: true, ProcessIdentity: runtimepkg.ProcessIdentity{PID: attempt.WrapperPID, StartedAtMS: attempt.WrapperStartedAtMS, Executable: attempt.WrapperExecutable, PGID: attempt.WrapperPGID, ControlNonceHash: attempt.ControlNonceHash}}, {Exists: true, ProcessIdentity: runtimepkg.ProcessIdentity{PID: attempt.WrapperPID, StartedAtMS: attempt.WrapperStartedAtMS, Executable: attempt.WrapperExecutable, PGID: attempt.WrapperPGID, ControlNonceHash: attempt.ControlNonceHash}}, {Exists: true, ProcessIdentity: runtimepkg.ProcessIdentity{PID: attempt.WrapperPID, StartedAtMS: attempt.WrapperStartedAtMS, Executable: attempt.WrapperExecutable, PGID: attempt.WrapperPGID, ControlNonceHash: attempt.ControlNonceHash}}, {Exists: true, ProcessIdentity: runtimepkg.ProcessIdentity{PID: attempt.WrapperPID, StartedAtMS: attempt.WrapperStartedAtMS, Executable: attempt.WrapperExecutable, PGID: attempt.WrapperPGID, ControlNonceHash: attempt.ControlNonceHash}}, {Exists: false}}}, Signaler: signaler}, Runtime: config.Runtime{HeartbeatStaleAfter: time.Second, TerminationTermGrace: 0, TerminationKillGrace: 0, AbsenceRecheckCount: 2}, ProcessGroupQualified: func(string) bool { return false }, AttentionDailyQuota: recoveryQuota(), Now: func() time.Time { return now }}
			if err := c.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertSingleFrozenStartupStall(t, raw, "", "termination_unconfirmed")
			if signaler.calls != 2 {
				t.Fatalf("known owner signal calls=%d, want TERM+KILL", signaler.calls)
			}
		})
	}
}
