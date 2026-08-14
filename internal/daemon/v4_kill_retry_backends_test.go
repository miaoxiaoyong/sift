package daemon

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/xsift/sift/internal/config"
	runtimepkg "github.com/xsift/sift/internal/runtime"
	"github.com/xsift/sift/internal/storage"
)

// TestV4KillRetryBackends closes the X10 dual-backend/并发 evidence gap
// (docs/testing/runtime-matrix.md X10). The same table/harness drives both
// `process` and `tmux` through two production entry points:
//
//   - `TerminationCoordinator.Operator(retry=true|false)` for kill / retry after
//     qualified absence, and
//   - `ProbeProcessCheckCoordinator.Tick` for retry-via-probe, which is the
//     production seam wired in #810.
//
// The matrix asserts:
//
//   - kill: attempts=1, successors=0 (no successor regardless of qualified or
//     unverified absence).
//   - retry: only after qualified absence (Operator path) or a probe observing
//     the wrapper absent (Tick path); then attempts/claims/launch_agent
//     successor each = 1.
//   - concurrent / replay: running Tick twice (or Operator + Tick together)
//     must not increment the projected counts.
func TestV4KillRetryBackends(t *testing.T) {
	for _, backend := range []string{"process", "tmux"} {
		t.Run(backend+"/kill_never_creates_successor", func(t *testing.T) {
			db, raw, attempt, now := seedRecoveryCoordinator(t, "running", 0)
			if _, err := raw.Exec(`UPDATE attempts SET backend=? WHERE run_id='run'`, backend); err != nil {
				t.Fatal(err)
			}
			signaler := &recoverySignals{}
			liveObs := runtimepkg.ProcessObservation{Exists: true, ProcessIdentity: runtimepkg.ProcessIdentity{PID: attempt.WrapperPID, StartedAtMS: attempt.WrapperStartedAtMS, Executable: attempt.WrapperExecutable, PGID: attempt.WrapperPGID, ControlNonceHash: attempt.ControlNonceHash}}
			coordinator := &TerminationCoordinator{
				DB: db,
				Terminator: runtimepkg.Terminator{
					Inspector: recoveryInspector{observation: liveObs},
					Signaler:  signaler,
					Sleep:     func(context.Context, time.Duration) error { return nil },
				},
				Runtime:              config.Runtime{HeartbeatStaleAfter: time.Second, TerminationTermGrace: 0, TerminationKillGrace: 0, AbsenceRecheckCount: 1},
				AttentionDailyQuota:  recoveryQuota(),
				ProcessGroupVerified: func(string) bool { return true }, // qualified but wrapper is alive
				Now:                  func() time.Time { return now },
			}
			if err := coordinator.Operator(context.Background(), "run", attempt.RunVersion, false); err != nil {
				t.Fatalf("Operator kill: %v", err)
			}
			assertKillSuccessorAbsence(t, raw, "run")
		})

		t.Run(backend+"/retry_after_qualified_absence", func(t *testing.T) {
			db, raw, attempt, now := seedRecoveryCoordinator(t, "running", 0)
			if _, err := raw.Exec(`UPDATE attempts SET backend=? WHERE run_id='run'`, backend); err != nil {
				t.Fatal(err)
			}
			signaler := &recoverySignals{}
			coordinator := &TerminationCoordinator{
				DB: db,
				Terminator: runtimepkg.Terminator{
					Inspector: recoveryInspector{observation: runtimepkg.ProcessObservation{Exists: false}},
					Signaler:  signaler,
					Sleep:     func(context.Context, time.Duration) error { return nil },
				},
				Runtime:              config.Runtime{HeartbeatStaleAfter: time.Second, TerminationTermGrace: 0, TerminationKillGrace: 0, AbsenceRecheckCount: 1},
				AttentionDailyQuota:  recoveryQuota(),
				ProcessGroupVerified: func(string) bool { return true }, // qualified absence path
				Now:                  func() time.Time { return now },
			}
			if err := coordinator.Operator(context.Background(), "run", attempt.RunVersion, true); err != nil {
				t.Fatalf("Operator retry: %v", err)
			}
			assertRetrySuccessorCreated(t, raw, "run", 1, 1, 1)
		})

		t.Run(backend+"/retry_after_probe_absence", func(t *testing.T) {
			// seedProbeFixture creates a startup_stall Interrupt, applies the
			// retry command (probe_in_progress) and leaves a pending probe.
			db, raw, interruptID, probeID := seedProbeFixture(t, time.UnixMilli(20_000))
			if _, err := raw.Exec(`UPDATE attempts SET backend=? WHERE run_id=?`, backend, probeRunID); err != nil {
				t.Fatal(err)
			}
			inspector := &probeInspector{observations: []runtimepkg.ProcessObservation{{Exists: false}}}
			coordinator := newProbeCoordinator(db, inspector, time.UnixMilli(20_000))
			if err := coordinator.Tick(context.Background()); err != nil {
				t.Fatalf("Tick: %v", err)
			}
			// Exactly one successor attempt/claim/launch.
			assertRetrySuccessorCreated(t, raw, probeRunID, 1, 1, 1)
			// Probe state is succeeded and the Interrupt is closed/responded.
			var state, absenceEvidence string
			if err := raw.QueryRow(`SELECT state,COALESCE(absence_evidence_json,'') FROM attempt_probes WHERE id=?`, probeID).Scan(&state, &absenceEvidence); err != nil {
				t.Fatal(err)
			}
			if state != "succeeded" || absenceEvidence == "" {
				t.Fatalf("probe = state=%s evidence=%q, want succeeded with evidence", state, absenceEvidence)
			}
			var status, closeReason string
			if err := raw.QueryRow(`SELECT status,COALESCE(close_reason,'') FROM interrupts WHERE id=?`, interruptID).Scan(&status, &closeReason); err != nil {
				t.Fatal(err)
			}
			if status != "closed" || closeReason != "responded" {
				t.Fatalf("interrupt = %s/%s, want closed/responded", status, closeReason)
			}
		})

		t.Run(backend+"/concurrent_replay", func(t *testing.T) {
			db, raw, _, probeID := seedProbeFixture(t, time.UnixMilli(20_000))
			if _, err := raw.Exec(`UPDATE attempts SET backend=? WHERE run_id=?`, backend, probeRunID); err != nil {
				t.Fatal(err)
			}
			// Two parallel ticks, each with one absent observation queued. Each
			// tick advances the same probe; the first commits succeeded, the
			// second sees an empty pending|running scan and is a no-op. Counts
			// must reflect exactly one finalization.
			inspector := &probeInspector{observations: []runtimepkg.ProcessObservation{{Exists: false}}}
			coordinator := newProbeCoordinator(db, inspector, time.UnixMilli(20_000))
			start := make(chan struct{})
			errs := make(chan error, 2)
			var wg sync.WaitGroup
			wg.Add(2)
			for i := 0; i < 2; i++ {
				go func() { defer wg.Done(); <-start; errs <- coordinator.Tick(context.Background()) }()
			}
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("Tick: %v", err)
				}
			}
			assertRetrySuccessorCreated(t, raw, probeRunID, 1, 1, 1)
			var acks int
			if err := raw.QueryRow(`SELECT count(*) FROM outbox_operations WHERE kind='command_ack'`).Scan(&acks); err != nil {
				t.Fatal(err)
			}
			if acks != 1 {
				t.Fatalf("command_ack = %d, want exactly 1 (replay must not double-ack)", acks)
			}
			var state string
			if err := raw.QueryRow(`SELECT state FROM attempt_probes WHERE id=?`, probeID).Scan(&state); err != nil || state != "succeeded" {
				t.Fatalf("probe after concurrent = %s, want succeeded", state)
			}
		})
	}
}

// assertKillSuccessorAbsence is the canonical X10 kill projection: exactly
// one attempt row and zero successors regardless of presence/absence evidence.
func assertKillSuccessorAbsence(t *testing.T, raw *sql.DB, runID string) {
	t.Helper()
	var attempts, successors int
	if err := raw.QueryRow(`SELECT count(*) FROM attempts WHERE run_id=?`, runID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM attempts WHERE run_id=? AND attempt_no>1`, runID).Scan(&successors); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || successors != 0 {
		t.Fatalf("kill projection attempts/successors = %d/%d, want 1/0", attempts, successors)
	}
	var launches int
	if err := raw.QueryRow(`SELECT count(*) FROM outbox_operations WHERE run_id=? AND kind='launch_agent'`, runID).Scan(&launches); err != nil {
		t.Fatal(err)
	}
	if launches != 0 {
		t.Fatalf("kill projection launch_agent = %d, want 0", launches)
	}
}

// assertRetrySuccessorCreated is the canonical X10 retry projection: the
// retry-after-absence path produces exactly one successor attempt, claim, and
// launch_agent outbox operation. The original attempt is orphaned with
// retry_after_absence resolution and the Run is queued. The wantAttempts /
// wantClaims / wantLaunches counters are bound to attempt_no>1 (the successor
// row), not to the total count, so callers pass 1 for a single successor.
func assertRetrySuccessorCreated(t *testing.T, raw *sql.DB, runID string, wantAttempts, wantClaims, wantLaunches int) {
	t.Helper()
	var attempts, claims, launches int
	if err := raw.QueryRow(`SELECT count(*) FROM attempts WHERE run_id=? AND attempt_no>1`, runID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM attempt_claims WHERE run_id=? AND attempt_no>1`, runID).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM outbox_operations WHERE run_id=? AND kind='launch_agent' AND attempt_no>1`, runID).Scan(&launches); err != nil {
		t.Fatal(err)
	}
	if wantAttempts != 0 && attempts != wantAttempts {
		t.Fatalf("retry successors attempts = %d, want %d", attempts, wantAttempts)
	}
	if wantClaims != 0 && claims != wantClaims {
		t.Fatalf("retry claims (successors) = %d, want %d", claims, wantClaims)
	}
	if wantLaunches != 0 && launches != wantLaunches {
		t.Fatalf("retry launch_agent (successors) = %d, want %d", launches, wantLaunches)
	}
	// The first attempt must be orphaned (phase) with retry_after_absence
	// resolution. The Run must be queued (Operator retry path) or queued
	// (probe success path).
	var oldPhase, oldResolution string
	if err := raw.QueryRow(`SELECT phase,COALESCE(attempt_resolution,'') FROM attempts WHERE run_id=? AND attempt_no=1`, runID).Scan(&oldPhase, &oldResolution); err != nil {
		t.Fatal(err)
	}
	if oldPhase != "orphaned" || oldResolution != "retry_after_absence" {
		t.Fatalf("original attempt phase/resolution = %s/%s, want orphaned/retry_after_absence", oldPhase, oldResolution)
	}
	var runStatus string
	if err := raw.QueryRow(`SELECT status FROM runs WHERE id=?`, runID).Scan(&runStatus); err != nil {
		t.Fatal(err)
	}
	if runStatus != string(storage.RunQueued) {
		t.Fatalf("run status = %s, want queued", runStatus)
	}
	var successorPhase string
	if err := raw.QueryRow(`SELECT phase FROM attempts WHERE run_id=? AND attempt_no=2`, runID).Scan(&successorPhase); err != nil {
		t.Fatal(err)
	}
	if successorPhase != "pending" {
		t.Fatalf("successor phase = %s, want pending", successorPhase)
	}
}
