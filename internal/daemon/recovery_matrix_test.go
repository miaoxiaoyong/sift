package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
	runtimepkg "github.com/miaoxiaoyong/sift/internal/runtime"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// recoverySequenceInspector makes every observation explicit. In particular,
// it prevents a row from accidentally sharing the identity-mismatch fallback
// used by a different row.
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

type recoverySignals struct {
	pgids   []int
	signals []syscall.Signal
}

func (s *recoverySignals) SignalGroup(pgid int, signal syscall.Signal) error {
	s.pgids = append(s.pgids, pgid)
	s.signals = append(s.signals, signal)
	return nil
}

// TestRecoveryRowsBackendParameterized constructs the durable/file/process/
// session observation named by every non-human DESIGN §10.1 row. The backend
// only hosts the wrapper: every row must reach the same recovery projection on
// process and tmux, while R15/R16 additionally prove the tmux diagnostic is
// observational.
func TestRecoveryRowsBackendParameterized(t *testing.T) {
	for _, backend := range []string{"process", "tmux"} {
		for _, row := range recoveryRows() {
			t.Run(backend+"/"+row.name, func(t *testing.T) {
				db, raw, attempt, now := seedRecoveryCoordinator(t, row.phase, row.heartbeat(time.UnixMilli(10_000)))
				if _, err := raw.Exec(`UPDATE attempts SET backend=? WHERE run_id='run'`, backend); err != nil {
					t.Fatal(err)
				}
				if row.setup != nil {
					row.setup(t, raw)
				}
				attempt, err := db.RecoveryAttemptForRun(context.Background(), "run")
				if err != nil {
					t.Fatal(err)
				}
				root := t.TempDir()
				inspector := &recoverySequenceInspector{observations: row.observations(attempt)}
				signaler := &recoverySignals{}
				coordinator := &TerminationCoordinator{
					DB: db, ControlRoot: root,
					Terminator:           runtimepkg.Terminator{Inspector: inspector, Signaler: signaler, Sleep: func(context.Context, time.Duration) error { return nil }},
					Runtime:              config.Runtime{HeartbeatStaleAfter: time.Second, TerminationTermGrace: 0, TerminationKillGrace: 0, AbsenceRecheckCount: 1},
					ProcessGroupVerified: row.qualified,
					AttentionDailyQuota:  recoveryQuota(), Now: func() time.Time { return now },
				}
				if backend == "tmux" && row.session != "" {
					if _, err := raw.Exec(`UPDATE attempt_claims SET dispatch_id='dispatch',bootstrap_nonce_hash='nonce',run_token_hash='token' WHERE run_id='run'`); err != nil {
						t.Fatal(err)
					}
					coordinator.TmuxPath = tmuxObservationFixture(t, row.session)
					coordinator.TmuxSocketPath = filepath.Join(root, "tmux.sock")
				}
				row.files(t, root, attempt, now)

				boot, err := db.StartDaemonBoot(context.Background(), "hash-cfg", "test", 1, 123, now.UnixMilli())
				if err != nil {
					t.Fatal(err)
				}
				if err := coordinator.RecoverStartup(context.Background(), boot); err != nil {
					t.Fatal(err)
				}
				row.assert(t, raw, boot, attempt, inspector, signaler, backend)
			})
		}
	}
}

type recoveryRow struct {
	name         string
	phase        string
	session      string
	setup        func(*testing.T, *sql.DB)
	qualified    func(string) bool
	heartbeat    func(time.Time) int64
	observations func(storage.RecoveryAttempt) []runtimepkg.ProcessObservation
	files        func(*testing.T, string, storage.RecoveryAttempt, time.Time)
	assert       func(*testing.T, *sql.DB, string, storage.RecoveryAttempt, *recoverySequenceInspector, *recoverySignals, string)
}

func recoveryRows() []recoveryRow {
	live := func(a storage.RecoveryAttempt) []runtimepkg.ProcessObservation {
		return []runtimepkg.ProcessObservation{matchingObservation(a)}
	}
	absent := func(storage.RecoveryAttempt) []runtimepkg.ProcessObservation {
		return []runtimepkg.ProcessObservation{{Exists: false}}
	}
	residual := func(a storage.RecoveryAttempt) []runtimepkg.ProcessObservation {
		mismatch := matchingObservation(a)
		mismatch.Executable = "/dead-wrapper"
		// The wrapper identity has disappeared, but the durable group binding
		// remains observable and must be terminated before recovery can settle.
		return []runtimepkg.ProcessObservation{mismatch, matchingObservation(a), matchingObservation(a), matchingObservation(a)}
	}
	noFiles := func(*testing.T, string, storage.RecoveryAttempt, time.Time) {}
	control := func(t *testing.T, root string, a storage.RecoveryAttempt, _ time.Time) {
		writeRecoveryControl(t, root, a, 0)
	}
	withAgentControl := func(t *testing.T, root string, a storage.RecoveryAttempt, _ time.Time) {
		writeRecoveryControl(t, root, a, 11)
	}
	durableAgent := func(t *testing.T, raw *sql.DB) {
		t.Helper()
		if _, err := raw.Exec(`UPDATE attempts SET agent_pid=11,agent_started_at_ms=1001,agent_executable='/agent' WHERE run_id='run'`); err != nil {
			t.Fatal(err)
		}
	}
	return []recoveryRow{
		{
			name: "R01_pending_no_execution_body", phase: "pending", setup: clearRecoveryWrapper, heartbeat: func(time.Time) int64 { return 0 }, observations: absent, files: noFiles,
			assert: func(t *testing.T, raw *sql.DB, boot string, a storage.RecoveryAttempt, inspector *recoverySequenceInspector, signals *recoverySignals, _ string) {
				assertRedispatched(t, raw, boot, a, "no bootstrap/control", inspector.calls, 0)
			},
		},
		{
			name: "R02_pending_preacquire_matched_redispatch", phase: "pending", heartbeat: func(time.Time) int64 { return 0 }, observations: live, files: noFiles,
			assert: func(t *testing.T, raw *sql.DB, boot string, a storage.RecoveryAttempt, inspector *recoverySequenceInspector, signals *recoverySignals, _ string) {
				// R02 is deliberately not R01: a durable pre-acquire wrapper is
				// observed before fencing; it has no session/permit and may be
				// redispatched without signalling or waiting for it.
				assertRedispatched(t, raw, boot, a, "pre-acquire wrapper", inspector.calls, 1)
				if len(signals.signals) != 0 {
					t.Fatalf("pre-acquire owner signals=%v", signals.signals)
				}
			},
		},
		{
			name: "R02_pending_preacquire_identity_mismatch_freezes", phase: "pending", heartbeat: func(time.Time) int64 { return 0 }, observations: func(a storage.RecoveryAttempt) []runtimepkg.ProcessObservation {
				o := matchingObservation(a)
				o.Executable = "/reused-wrapper"
				return []runtimepkg.ProcessObservation{o}
			}, files: noFiles,
			assert: func(t *testing.T, raw *sql.DB, boot string, a storage.RecoveryAttempt, inspector *recoverySequenceInspector, signals *recoverySignals, _ string) {
				assertPendingPreacquireFrozen(t, raw, boot, a)
				assertNoSignals(t, signals)
				assertObserved(t, inspector, 1)
			},
		},
		{
			name: "R02_pending_preacquire_pid_pgid_reuse_freezes", phase: "pending", heartbeat: func(time.Time) int64 { return 0 }, observations: func(a storage.RecoveryAttempt) []runtimepkg.ProcessObservation {
				o := matchingObservation(a)
				o.PGID = 99
				return []runtimepkg.ProcessObservation{o}
			}, files: noFiles,
			assert: func(t *testing.T, raw *sql.DB, boot string, a storage.RecoveryAttempt, inspector *recoverySequenceInspector, signals *recoverySignals, _ string) {
				assertPendingPreacquireFrozen(t, raw, boot, a)
				assertNoSignals(t, signals)
				assertObserved(t, inspector, 1)
			},
		},
		{
			name: "R03_starting_owner_control_without_permit", phase: "starting", heartbeat: func(time.Time) int64 { return 0 }, observations: live, files: control,
			assert: func(t *testing.T, raw *sql.DB, boot string, _ storage.RecoveryAttempt, inspector *recoverySequenceInspector, signals *recoverySignals, _ string) {
				assertSupervised(t, raw, boot, "starting")
				assertNoSignals(t, signals)
				assertObserved(t, inspector, 1)
			},
		},
		{
			name: "R04_starting_owner_and_group_absent", phase: "starting", qualified: func(string) bool { return true }, heartbeat: func(time.Time) int64 { return 0 }, observations: absent, files: noFiles,
			assert: func(t *testing.T, raw *sql.DB, boot string, _ storage.RecoveryAttempt, inspector *recoverySequenceInspector, signals *recoverySignals, _ string) {
				assertOrphanedSuccessor(t, raw, boot)
				assertNoSignals(t, signals)
				assertObserved(t, inspector, 2)
			},
		},
		{
			name: "R05_spawning_owner_without_agent_identity", phase: "spawning", heartbeat: func(time.Time) int64 { return 0 }, observations: live, files: control,
			assert: func(t *testing.T, raw *sql.DB, boot string, _ storage.RecoveryAttempt, inspector *recoverySequenceInspector, signals *recoverySignals, _ string) {
				assertSupervised(t, raw, boot, "spawning")
				assertNoSignals(t, signals)
				assertObserved(t, inspector, 1)
			},
		},
		{
			name: "R06_spawning_durable_agent_identity_process_match", phase: "spawning", setup: durableAgent, heartbeat: func(time.Time) int64 { return 0 }, observations: func(a storage.RecoveryAttempt) []runtimepkg.ProcessObservation {
				return []runtimepkg.ProcessObservation{matchingAgentObservation(a)}
			}, files: withAgentControl,
			assert: func(t *testing.T, raw *sql.DB, boot string, _ storage.RecoveryAttempt, inspector *recoverySequenceInspector, signals *recoverySignals, _ string) {
				assertStartedRecovered(t, raw, boot)
				assertNoSignals(t, signals)
				assertObserved(t, inspector, 1)
			},
		},
		{
			name: "R07_spawning_durable_agent_identity_matching_result", phase: "spawning", setup: durableAgent, heartbeat: func(time.Time) int64 { return 0 }, observations: absent, files: recoveryResult(7),
			assert: func(t *testing.T, raw *sql.DB, boot string, _ storage.RecoveryAttempt, inspector *recoverySequenceInspector, signals *recoverySignals, _ string) {
				assertResultConsumed(t, raw, boot, 7)
				assertNoSignals(t, signals)
				assertObserved(t, inspector, 0)
			},
		},
		{
			name: "R08_spawning_durable_agent_identity_wrapper_live", phase: "spawning", setup: durableAgent, heartbeat: func(time.Time) int64 { return 0 }, observations: live, files: noFiles,
			assert: func(t *testing.T, raw *sql.DB, boot string, _ storage.RecoveryAttempt, inspector *recoverySequenceInspector, signals *recoverySignals, _ string) {
				assertSupervised(t, raw, boot, "spawning")
				assertNoSignals(t, signals)
				assertObserved(t, inspector, 1)
			},
		},
		{
			name: "R08_spawning_durable_agent_identity_wrapper_dead_verified_absence", phase: "spawning", qualified: func(string) bool { return true }, setup: durableAgent, heartbeat: func(time.Time) int64 { return 0 }, observations: absent, files: noFiles,
			assert: func(t *testing.T, raw *sql.DB, boot string, _ storage.RecoveryAttempt, inspector *recoverySequenceInspector, signals *recoverySignals, _ string) {
				assertOrphanedSuccessor(t, raw, boot)
				assertNoSignals(t, signals)
				assertObserved(t, inspector, 2)
			},
		},
		{
			name: "R09_spawning_group_disappearance_verified_absence", phase: "spawning", qualified: func(string) bool { return true }, heartbeat: func(time.Time) int64 { return 0 }, observations: func(a storage.RecoveryAttempt) []runtimepkg.ProcessObservation {
				mismatch := matchingObservation(a)
				mismatch.Executable = "/dead-wrapper"
				return []runtimepkg.ProcessObservation{mismatch, matchingObservation(a), {}}
			}, files: control,
			assert: func(t *testing.T, raw *sql.DB, boot string, _ storage.RecoveryAttempt, inspector *recoverySequenceInspector, signals *recoverySignals, _ string) {
				assertOrphanedSuccessor(t, raw, boot)
				assertTerm(t, signals)
				assertObserved(t, inspector, 3)
			},
		},
		{
			name: "R09_spawning_group_refusal_freezes", phase: "spawning", heartbeat: func(time.Time) int64 { return 0 }, observations: residual, files: control,
			assert: func(t *testing.T, raw *sql.DB, boot string, _ storage.RecoveryAttempt, inspector *recoverySequenceInspector, signals *recoverySignals, _ string) {
				assertFrozenRecovery(t, raw, boot, "termination_unconfirmed")
				assertTermKill(t, signals)
				assertObserved(t, inspector, 4)
			},
		},
		{
			name: "R10_spawning_verified_absence_orphans", phase: "spawning", qualified: func(string) bool { return true }, heartbeat: func(time.Time) int64 { return 0 }, observations: absent, files: noFiles,
			assert: func(t *testing.T, raw *sql.DB, boot string, _ storage.RecoveryAttempt, inspector *recoverySequenceInspector, signals *recoverySignals, _ string) {
				assertOrphanedSuccessor(t, raw, boot)
				assertNoSignals(t, signals)
				assertObserved(t, inspector, 2)
			},
		},
		{
			name: "R10_spawning_unverified_absence_freezes", phase: "spawning", heartbeat: func(time.Time) int64 { return 0 }, observations: absent, files: noFiles,
			assert: func(t *testing.T, raw *sql.DB, boot string, _ storage.RecoveryAttempt, inspector *recoverySequenceInspector, signals *recoverySignals, _ string) {
				assertFrozenRecovery(t, raw, boot, "process_group_unverified")
				assertNoSignals(t, signals)
				assertObserved(t, inspector, 2)
			},
		},
		{
			name: "R11_running_success_result", phase: "running", heartbeat: func(time.Time) int64 { return 0 }, observations: absent, files: recoveryResult(0),
			assert: func(t *testing.T, raw *sql.DB, boot string, _ storage.RecoveryAttempt, inspector *recoverySequenceInspector, signals *recoverySignals, _ string) {
				assertResultConsumed(t, raw, boot, 0)
				assertNoSignals(t, signals)
				assertObserved(t, inspector, 0)
			},
		},
		{
			name: "R12_running_failed_result", phase: "running", heartbeat: func(time.Time) int64 { return 0 }, observations: absent, files: recoveryResult(12),
			assert: func(t *testing.T, raw *sql.DB, boot string, _ storage.RecoveryAttempt, inspector *recoverySequenceInspector, signals *recoverySignals, _ string) {
				assertResultConsumed(t, raw, boot, 12)
				assertNoSignals(t, signals)
				assertObserved(t, inspector, 0)
			},
		},
		{
			name: "R13_running_fresh_heartbeat_owner_live", phase: "running", heartbeat: func(now time.Time) int64 { return now.UnixMilli() }, observations: live, files: control,
			assert: func(t *testing.T, raw *sql.DB, boot string, _ storage.RecoveryAttempt, inspector *recoverySequenceInspector, signals *recoverySignals, _ string) {
				assertSupervised(t, raw, boot, "running")
				assertNoSignals(t, signals)
				assertObserved(t, inspector, 1)
			},
		},
		{
			name: "R14_running_stale_heartbeat", phase: "running", heartbeat: func(now time.Time) int64 { return now.Add(-2 * time.Second).UnixMilli() }, observations: residual, files: control,
			assert: func(t *testing.T, raw *sql.DB, boot string, _ storage.RecoveryAttempt, inspector *recoverySequenceInspector, signals *recoverySignals, _ string) {
				assertFrozenRecovery(t, raw, boot, "termination_unconfirmed")
				assertTermKill(t, signals)
				assertObserved(t, inspector, 4)
			},
		},
		{
			name: "R15_running_session_present_verified_absence_fails", phase: "running", session: "present", qualified: func(string) bool { return true }, setup: exhaustRecoveryAttempts, heartbeat: func(now time.Time) int64 { return now.UnixMilli() }, observations: absent, files: noFiles,
			assert: func(t *testing.T, raw *sql.DB, boot string, _ storage.RecoveryAttempt, inspector *recoverySequenceInspector, signals *recoverySignals, backend string) {
				assertOrphanedFailure(t, raw, boot)
				assertNoSignals(t, signals)
				assertObserved(t, inspector, 2)
				if backend == "tmux" {
					assertSessionDiagnostic(t, raw, "backend_session_present_wrapper_absent")
				}
			},
		},
		{
			name: "R15_running_session_present_unverified_freezes", phase: "running", session: "present", heartbeat: func(now time.Time) int64 { return now.UnixMilli() }, observations: absent, files: noFiles,
			assert: func(t *testing.T, raw *sql.DB, boot string, _ storage.RecoveryAttempt, inspector *recoverySequenceInspector, signals *recoverySignals, backend string) {
				assertFrozenRecovery(t, raw, boot, "process_group_unverified")
				assertNoSignals(t, signals)
				assertObserved(t, inspector, 2)
				if backend == "tmux" {
					assertSessionDiagnostic(t, raw, "backend_session_present_wrapper_absent")
				}
			},
		},
		{
			name: "R16_running_wrapper_present_tmux_session_absent", phase: "running", session: "absent", heartbeat: func(now time.Time) int64 { return now.UnixMilli() }, observations: live, files: control,
			assert: func(t *testing.T, raw *sql.DB, boot string, _ storage.RecoveryAttempt, inspector *recoverySequenceInspector, signals *recoverySignals, backend string) {
				assertSupervised(t, raw, boot, "running")
				assertNoSignals(t, signals)
				if backend == "tmux" {
					assertSessionDiagnostic(t, raw, "backend_session_lost")
				}
			},
		},
	}
}

func matchingObservation(a storage.RecoveryAttempt) runtimepkg.ProcessObservation {
	return runtimepkg.ProcessObservation{Exists: true, ProcessIdentity: runtimepkg.ProcessIdentity{PID: a.WrapperPID, StartedAtMS: a.WrapperStartedAtMS, Executable: a.WrapperExecutable, PGID: a.WrapperPGID, ControlNonceHash: a.ControlNonceHash}}
}

func matchingAgentObservation(a storage.RecoveryAttempt) runtimepkg.ProcessObservation {
	return runtimepkg.ProcessObservation{Exists: true, ProcessIdentity: runtimepkg.ProcessIdentity{PID: a.AgentPID, StartedAtMS: a.AgentStartedAtMS, Executable: a.AgentExecutable}}
}

func clearRecoveryWrapper(t *testing.T, raw *sql.DB) {
	t.Helper()
	if _, err := raw.Exec(`UPDATE attempts SET wrapper_pid=NULL,wrapper_started_at_ms=NULL,wrapper_executable=NULL,wrapper_pgid=NULL,wrapper_instance_id=NULL,control_nonce_hash=NULL WHERE run_id='run'`); err != nil {
		t.Fatal(err)
	}
}

func exhaustRecoveryAttempts(t *testing.T, raw *sql.DB) {
	t.Helper()
	if _, err := raw.Exec(`UPDATE runs SET max_attempts=1 WHERE id='run'`); err != nil {
		t.Fatal(err)
	}
}

func writeRecoveryControl(t *testing.T, root string, a storage.RecoveryAttempt, agentPID int64) {
	t.Helper()
	path := filepath.Join(root, "runs", a.RunID, "attempts", "1", "control.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"wrapper_pid": a.WrapperPID}
	if agentPID != 0 {
		body["agent_identity"] = map[string]any{"pid": agentPID, "started_at_ms": int64(1001), "executable": "/agent"}
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func recoveryResult(exit int) func(*testing.T, string, storage.RecoveryAttempt, time.Time) {
	return func(t *testing.T, root string, a storage.RecoveryAttempt, now time.Time) {
		t.Helper()
		path := filepath.Join(root, "runs", a.RunID, "attempts", "1", "result.json")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(map[string]any{"schema_version": 1, "run_id": a.RunID, "attempt_no": a.AttemptNo, "generation": a.Generation, "wrapper_instance_id": "instance", "agent_identity": map[string]any{"pid": 11, "started_at_ms": 1001, "executable": "/agent"}, "exit_code": exit, "finished_at_ms": now.UnixMilli(), "digest": "result-" + string(rune('a'+exit%20))})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertRedispatched(t *testing.T, raw *sql.DB, boot string, a storage.RecoveryAttempt, label string, observed, wantObserved int) {
	t.Helper()
	var generation, launches int
	var action string
	if err := raw.QueryRow(`SELECT generation FROM attempts WHERE run_id='run'`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM outbox_operations WHERE run_id='run' AND kind='launch_agent' AND state='pending'`).Scan(&launches); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT action FROM startup_recovery_actions WHERE boot_id=? AND candidate_key='attempt:run:1'`, boot).Scan(&action); err != nil {
		t.Fatal(err)
	}
	if generation != a.Generation+1 || launches != 1 || action != "redispatch" || observed != wantObserved {
		t.Fatalf("%s recovery generation=%d launches=%d action=%s observed=%d", label, generation, launches, action, observed)
	}
}

func assertSupervised(t *testing.T, raw *sql.DB, boot, phase string) {
	t.Helper()
	var gotPhase, action string
	if err := raw.QueryRow(`SELECT phase FROM attempts WHERE run_id='run' AND attempt_no=1`).Scan(&gotPhase); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT action FROM startup_recovery_actions WHERE boot_id=? AND candidate_key='attempt:run:1'`, boot).Scan(&action); err != nil {
		t.Fatal(err)
	}
	if gotPhase != phase || action != "supervise" {
		t.Fatalf("supervision phase/action=%s/%s, want %s/supervise", gotPhase, action, phase)
	}
}

func assertStartedRecovered(t *testing.T, raw *sql.DB, boot string) {
	t.Helper()
	var phase, status, action string
	var agent int
	if err := raw.QueryRow(`SELECT a.phase,r.status,a.agent_pid FROM attempts a JOIN runs r ON r.id=a.run_id WHERE a.run_id='run'`).Scan(&phase, &status, &agent); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT action FROM startup_recovery_actions WHERE boot_id=? AND candidate_key='attempt:run:1'`, boot).Scan(&action); err != nil {
		t.Fatal(err)
	}
	if phase != "running" || status != "running" || agent != 11 || action != "supervise" {
		t.Fatalf("started recovery=%s/%s agent=%d action=%s", phase, status, agent, action)
	}
}

func assertResultConsumed(t *testing.T, raw *sql.DB, _ string, exit int) {
	t.Helper()
	var phase string
	var gotExit int
	if err := raw.QueryRow(`SELECT phase,result_exit_code FROM attempts WHERE run_id='run' AND attempt_no=1`).Scan(&phase, &gotExit); err != nil {
		t.Fatal(err)
	}
	// Result consumption itself removes the attempt from the boot candidate set;
	// no synthetic supervision receipt may stand in for the durable result.
	if phase != "finished" || gotExit != exit {
		t.Fatalf("result recovery phase/exit=%s/%d", phase, gotExit)
	}
}

func assertOrphanedSuccessor(t *testing.T, raw *sql.DB, _ string) {
	t.Helper()
	var phase string
	var attempts int
	if err := raw.QueryRow(`SELECT phase FROM attempts WHERE run_id='run' AND attempt_no=1`).Scan(&phase); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM attempts WHERE run_id='run'`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	// Absence atomically orphans the current attempt and creates the successor;
	// there is no startup receipt for a candidate that no longer exists.
	if phase != "orphaned" || attempts != 2 {
		t.Fatalf("absence recovery phase/attempts=%s/%d", phase, attempts)
	}
}

func assertOrphanedFailure(t *testing.T, raw *sql.DB, _ string) {
	t.Helper()
	var phase, status, worktree string
	var successors int
	if err := raw.QueryRow(`SELECT a.phase,r.status,a.worktree_path FROM attempts a JOIN runs r ON r.id=a.run_id WHERE a.run_id='run' AND a.attempt_no=1`).Scan(&phase, &status, &worktree); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM attempts WHERE run_id='run' AND attempt_no>1`).Scan(&successors); err != nil {
		t.Fatal(err)
	}
	if phase != "orphaned" || status != "failed" || worktree != "/work" || successors != 0 {
		t.Fatalf("orphaned failure phase/status/worktree/successors=%s/%s/%s/%d", phase, status, worktree, successors)
	}
}

func assertFrozenRecovery(t *testing.T, raw *sql.DB, boot, reason string) {
	t.Helper()
	assertSingleFrozenStartupStall(t, raw, boot, reason)
	var action string
	if err := raw.QueryRow(`SELECT action FROM startup_recovery_actions WHERE boot_id=? AND candidate_key='attempt:run:1'`, boot).Scan(&action); err != nil {
		t.Fatal(err)
	}
	if action != "frozen" {
		t.Fatalf("recovery action=%s, want frozen", action)
	}
}

func assertNoSignals(t *testing.T, s *recoverySignals) {
	t.Helper()
	if len(s.signals) != 0 {
		t.Fatalf("unexpected signals=%v pgids=%v", s.signals, s.pgids)
	}
}
func assertTerm(t *testing.T, s *recoverySignals) {
	t.Helper()
	if len(s.signals) != 1 || s.signals[0] != syscall.SIGTERM || s.pgids[0] != 10 {
		t.Fatalf("signals=%v pgids=%v, want TERM to known pgid 10", s.signals, s.pgids)
	}
}

func assertTermKill(t *testing.T, s *recoverySignals) {
	t.Helper()
	if len(s.signals) != 2 || s.signals[0] != syscall.SIGTERM || s.signals[1] != syscall.SIGKILL || s.pgids[0] != 10 || s.pgids[1] != 10 {
		t.Fatalf("signals=%v pgids=%v, want TERM+KILL to known pgid 10", s.signals, s.pgids)
	}
}
func assertObserved(t *testing.T, i *recoverySequenceInspector, want int) {
	t.Helper()
	if i.calls != want {
		t.Fatalf("observations=%d, want %d", i.calls, want)
	}
}

func assertPendingPreacquireFrozen(t *testing.T, raw *sql.DB, boot string, a storage.RecoveryAttempt) {
	t.Helper()
	assertSingleFrozenStartupStall(t, raw, boot, "process_identity_unknown")
	var generation, launches int
	var action string
	if err := raw.QueryRow(`SELECT action FROM startup_recovery_actions WHERE boot_id=? AND candidate_key='attempt:run:1'`, boot).Scan(&action); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT generation FROM attempts WHERE run_id='run' AND attempt_no=1`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM outbox_operations WHERE run_id='run' AND kind='launch_agent'`).Scan(&launches); err != nil {
		t.Fatal(err)
	}
	if action != "frozen" || generation != a.Generation || launches != 0 {
		t.Fatalf("preacquire freeze action/generation/launches=%s/%d/%d, want frozen/%d/0", action, generation, launches, a.Generation)
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
	var interrupts, open, charges, successors int
	if err := raw.QueryRow(`SELECT count(*),sum(status='open') FROM interrupts WHERE run_id='run' AND reason='startup_stall'`).Scan(&interrupts, &open); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM budget_entries WHERE run_id='run'`).Scan(&charges); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM attempts WHERE run_id='run' AND attempt_no>1`).Scan(&successors); err != nil {
		t.Fatal(err)
	}
	if interrupts != 1 || open != 1 || charges != 1 || successors != 0 {
		t.Fatalf("startup_stall interrupts/open/charges/successors=%d/%d/%d/%d", interrupts, open, charges, successors)
	}
}

// TestRecoveryFailClosedVectors keeps the four safety inputs independent. A
// vector never inherits a signal count or qualification result from another.
func TestRecoveryFailClosedVectors(t *testing.T) {
	for _, backend := range []string{"process", "tmux"} {
		for _, vector := range []struct {
			name         string
			observations func(storage.RecoveryAttempt) []runtimepkg.ProcessObservation
			qualified    func(*storage.DB, *sql.DB, storage.RecoveryAttempt) string
			reason       string
			wantSignals  int
		}{
			{"identity_mismatch", func(a storage.RecoveryAttempt) []runtimepkg.ProcessObservation {
				o := matchingObservation(a)
				o.Executable = "/reused"
				return []runtimepkg.ProcessObservation{o}
			}, nil, "process_identity_unknown", 0},
			{"pid_pgid_reuse", func(a storage.RecoveryAttempt) []runtimepkg.ProcessObservation {
				o := matchingObservation(a)
				o.PGID = 99
				return []runtimepkg.ProcessObservation{o}
			}, nil, "process_identity_unknown", 0},
			{"bounded_group_refusal", func(a storage.RecoveryAttempt) []runtimepkg.ProcessObservation {
				return []runtimepkg.ProcessObservation{matchingObservation(a), matchingObservation(a), matchingObservation(a)}
			}, nil, "termination_unconfirmed", 2},
			{"detached_descendant_unverified", func(storage.RecoveryAttempt) []runtimepkg.ProcessObservation {
				return []runtimepkg.ProcessObservation{{Exists: false}}
			}, func(db *storage.DB, raw *sql.DB, a storage.RecoveryAttempt) string {
				q := detachedQualification(t, runtimepkg.QualificationEvidence{Status: runtimepkg.ProcessGroupUnverified, Reason: "detached_descendant"})
				if err := db.RecordTopologyQualification(context.Background(), q); err != nil {
					t.Fatal(err)
				}
				if _, err := raw.Exec(`UPDATE attempts SET topology_qualification_key=? WHERE run_id='run'`, q.QualificationKey); err != nil {
					t.Fatal(err)
				}
				return q.QualificationKey
			}, "process_group_unverified", 0},
		} {
			t.Run(backend+"/"+vector.name, func(t *testing.T) {
				db, raw, attempt, now := seedRecoveryCoordinator(t, "running", 0)
				if _, err := raw.Exec(`UPDATE attempts SET backend=? WHERE run_id='run'`, backend); err != nil {
					t.Fatal(err)
				}
				key := ""
				if vector.qualified != nil {
					key = vector.qualified(db, raw, attempt)
				}
				signals := &recoverySignals{}
				c := &TerminationCoordinator{DB: db, Terminator: runtimepkg.Terminator{Inspector: &recoverySequenceInspector{observations: vector.observations(attempt)}, Signaler: signals, Sleep: func(context.Context, time.Duration) error { return nil }}, Runtime: config.Runtime{HeartbeatStaleAfter: time.Second, TerminationTermGrace: 0, TerminationKillGrace: 0, AbsenceRecheckCount: 1}, ProcessGroupQualified: func(got string) bool { return key != "" && got != key }, AttentionDailyQuota: recoveryQuota(), Now: func() time.Time { return now }}
				if err := c.Recover(context.Background()); err != nil {
					t.Fatal(err)
				}
				assertSingleFrozenStartupStall(t, raw, "", vector.reason)
				if len(signals.signals) != vector.wantSignals {
					t.Fatalf("signals=%v, want %d", signals.signals, vector.wantSignals)
				}
				if vector.wantSignals == 0 {
					assertNoSignals(t, signals)
				} else {
					assertTermKill(t, signals)
				}
				if vector.name == "detached_descendant_unverified" {
					var status string
					if err := raw.QueryRow(`SELECT status FROM agent_topology_qualifications WHERE qualification_key=?`, key).Scan(&status); err != nil || status != "process-group-unverified" {
						t.Fatalf("detached durable status=%q err=%v", status, err)
					}
				}
			})
		}
	}
}
