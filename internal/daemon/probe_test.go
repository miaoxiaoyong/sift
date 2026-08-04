package daemon

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/miaoxiaoyong/sift/internal/command"
	"github.com/miaoxiaoyong/sift/internal/config"
	runtimepkg "github.com/miaoxiaoyong/sift/internal/runtime"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// These tests cover the startup_stall probe process-check worker (#810): the
// supervisor-tick coordinator that drives pending|running attempt_probes to the
// unique ApplyRetryProbeResult finalizer. It observes the bound attempt's
// wrapper process outside any transaction and commits exactly once.

const probeRunID = "0123456789abcdef0123456789abcdef"

// probeInspector is a scriptable ProcessInspector for the probe coordinator
// tests. It returns one observation per call, in order.
type probeInspector struct {
	observations []runtimepkg.ProcessObservation
	err          error
}

func (i *probeInspector) Observe(context.Context, runtimepkg.ProcessIdentity) (runtimepkg.ProcessObservation, error) {
	if i.err != nil {
		return runtimepkg.ProcessObservation{}, i.err
	}
	if len(i.observations) == 0 {
		return runtimepkg.ProcessObservation{}, nil
	}
	o := i.observations[0]
	i.observations = i.observations[1:]
	return o, nil
}

func probeIdentity() runtimepkg.ProcessIdentity {
	return runtimepkg.ProcessIdentity{PID: 700, PGID: 700, StartedAtMS: 5000, Executable: "/wrapper", ControlNonceHash: "nonce-hash"}
}

// seedProbeFixture seeds a Run, a frozen startup_stall attempt with a recorded
// wrapper identity, and a pending retry probe (the post-/sift-retry state). It
// returns the DB handle, a raw SQL handle for assertions, the interrupt id and
// the pending probe id.
func seedProbeFixture(t *testing.T, now time.Time) (*storage.DB, *sql.DB, string, string) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SeedProjectForTest(ctx, "cfg", "project", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, probeRunID, "project", "cfg", "42", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", db.Path())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close(); db.Close() })
	id := probeIdentity()
	for _, stmt := range []string{
		`INSERT INTO task_spec_snapshots(id,run_id,version,schema_version,canonical_json,content_digest,created_at_ms) VALUES ('task','` + probeRunID + `',1,1,'{}','digest',` + strconv.FormatInt(now.UnixMilli(), 10) + `)`,
		`INSERT INTO attempts(run_id,attempt_no,phase,generation,backend,agent_id,task_spec_snapshot_id,worktree_path,branch_name,base_ref,base_sha,wrapper_pid,wrapper_started_at_ms,wrapper_executable,wrapper_pgid,wrapper_instance_id,control_nonce_hash,created_at_ms,updated_at_ms) VALUES ('` + probeRunID + `',1,'pending',1,'process','agent','task','/work','branch','main','abc',` + strconv.FormatInt(int64(id.PID), 10) + `,` + strconv.FormatInt(id.StartedAtMS, 10) + `,'` + id.Executable + `',` + strconv.FormatInt(int64(id.PGID), 10) + `,'instance','` + id.ControlNonceHash + `',` + strconv.FormatInt(now.UnixMilli(), 10) + `,` + strconv.FormatInt(now.UnixMilli(), 10) + `)`,
		`INSERT INTO attempt_claims(run_id,attempt_no,generation,launch_operation_key,wrapper_instance_id,wrapper_session_hash,created_at_ms,updated_at_ms) VALUES ('` + probeRunID + `',1,1,'launch','instance','session',` + strconv.FormatInt(now.UnixMilli(), 10) + `,` + strconv.FormatInt(now.UnixMilli(), 10) + `)`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	attempt := 1
	in, err := db.EmitInterrupt(ctx, storage.EmitInterruptCmd{
		RunID: probeRunID, ExpectedRunVersion: 1, AttemptNo: &attempt, Reason: storage.InterruptStartupStall,
		Facts:      map[string]string{"attempt_no": "1", "generation": "1", "diagnostic_cause": "termination_unconfirmed", "isolation_consequence": "worktree held", "recommended_action": "retry", "attempt_diagnostic_ref": "/attempt", "worktree_ref": "/worktree"},
		Generation: storage.InterruptGeneration{AttemptNo: 1, Generation: 1}, GatePhase: storage.GateNone, GuardrailLevel: storage.GuardrailNone,
		ExpiresAfterMS: 10, OnExpire: storage.ExpireEscalate, OnMaxEscalations: storage.ExpireHold, MaxEscalations: 1,
		AttentionDailyQuota: recoveryQuota(), DayTimezone: "UTC", Source: storage.SourceRecovery, NowMS: now.UnixMilli(),
		Channels: []storage.InterruptChannel{{ID: "text", Capabilities: []string{"text"}}},
	})
	if err != nil {
		t.Fatalf("emit startup_stall: %v", err)
	}
	// EmitInterrupt froze isolation exactly like the production recovery path.
	var nonce string
	if err := raw.QueryRow(`SELECT nonce FROM interrupts WHERE id=?`, in.ID).Scan(&nonce); err != nil {
		t.Fatal(err)
	}
	env := probeRetryEnv(t, nonce)
	if _, err := db.ApplyCommandEvent(ctx, storage.ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: now.UnixMilli() + 5}); err != nil {
		t.Fatalf("retry request: %v", err)
	}
	var probeID string
	if err := raw.QueryRow(`SELECT id FROM attempt_probes WHERE interrupt_id=? AND state='pending'`, in.ID).Scan(&probeID); err != nil {
		t.Fatalf("locate pending probe: %v", err)
	}
	return db, raw, in.ID, probeID
}

func probeRetryEnv(t *testing.T, nonce string) command.CommandEventEnvelopeV1 {
	t.Helper()
	key, err := command.RecomputeEventKey("project", command.SourceForgeComment, "c1")
	if err != nil {
		t.Fatal(err)
	}
	actor := "alice"
	return command.CommandEventEnvelopeV1{
		SchemaVersion: 1, EventKey: key, ProjectID: "project", Source: command.SourceForgeComment,
		RemoteEventID: "c1", Target: command.CommandTarget{Kind: command.TargetIssue, ID: "42"},
		Actor: &actor, RawDigest: strings.Repeat("a", 64), OccurredAtMS: 1006,
		Comment: &command.CommandComment{ID: "c1", Body: "/sift retry " + probeRunID + " " + nonce},
	}
}

func newProbeCoordinator(db *storage.DB, inspector runtimepkg.ProcessInspector, now time.Time) *ProbeProcessCheckCoordinator {
	return &ProbeProcessCheckCoordinator{
		DB: db, Inspector: inspector, Runtime: config.Runtime{AbsenceRecheckCount: 2, AbsenceRecheckInterval: 0}, Now: func() time.Time { return now },
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
}

// TestProbeTickObserveAbsentDrivesSuccessArm proves the success path: a pending
// probe whose wrapper process is observed absent is driven through
// ApplyRetryProbeResult's success arm — Run waiting_human -> queued, Interrupt
// closed/responded, isolation released, one applied ack, probe succeeded with
// absence evidence (ADR-013).
func TestProbeTickObserveAbsentDrivesSuccessArm(t *testing.T) {
	now := time.UnixMilli(20_000)
	db, raw, interruptID, probeID := seedProbeFixture(t, now)
	inspector := &probeInspector{observations: []runtimepkg.ProcessObservation{{Exists: false}}}
	coordinator := newProbeCoordinator(db, inspector, now)

	if err := coordinator.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	var state, evidence string
	var finishedMS int64
	if err := raw.QueryRow(`SELECT state,COALESCE(absence_evidence_json,''),COALESCE(finished_at_ms,0) FROM attempt_probes WHERE id=?`, probeID).Scan(&state, &evidence, &finishedMS); err != nil {
		t.Fatal(err)
	}
	if state != "succeeded" || evidence == "" {
		t.Fatalf("probe = state=%s evidence=%q, want succeeded with evidence", state, evidence)
	}
	var status, closeReason string
	if err := raw.QueryRow(`SELECT status,COALESCE(close_reason,'') FROM interrupts WHERE id=?`, interruptID).Scan(&status, &closeReason); err != nil {
		t.Fatal(err)
	}
	if status != "closed" || closeReason != "responded" {
		t.Fatalf("interrupt = %s/%s, want closed/responded", status, closeReason)
	}
	var runStatus string
	if err := raw.QueryRow(`SELECT status FROM runs WHERE id=?`, probeRunID).Scan(&runStatus); err != nil {
		t.Fatal(err)
	}
	if runStatus != "queued" {
		t.Fatalf("run status = %s, want queued (ADR-013 retry_after_absence)", runStatus)
	}
	var attempts, claims, launches int
	if err := raw.QueryRow(`SELECT count(*) FROM attempts WHERE run_id=? AND attempt_no>1`, probeRunID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM attempt_claims WHERE run_id=? AND attempt_no>1`, probeRunID).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM outbox_operations WHERE run_id=? AND kind='launch_agent'`, probeRunID).Scan(&launches); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || claims != 1 || launches != 1 {
		t.Fatalf("success successor attempts/claims/launches=%d/%d/%d", attempts, claims, launches)
	}
	var isolation string
	if err := raw.QueryRow(`SELECT isolation_state FROM attempts WHERE run_id=? AND attempt_no=1`, probeRunID).Scan(&isolation); err != nil {
		t.Fatal(err)
	}
	if isolation != "none" {
		t.Fatalf("isolation_state = %s, want none (released)", isolation)
	}
	// No signal was sent: the process was already absent.
	if len(inspector.observations) != 0 {
		t.Fatalf("absent observation rechecked unexpectedly: %d left", len(inspector.observations))
	}
}

// TestProbeTickObservePresentDrivesFailureArm proves the failure path: a probe
// whose wrapper process is still present (and identity matches across the
// bounded recheck) finalizes absence_unconfirmed, keeps the Interrupt open, and
// leaves isolation frozen. No signal is ever sent.
func TestProbeTickObservePresentDrivesFailureArm(t *testing.T) {
	now := time.UnixMilli(20_000)
	db, raw, interruptID, probeID := seedProbeFixture(t, now)
	var nonce string
	if err := raw.QueryRow(`SELECT nonce FROM interrupts WHERE id=?`, interruptID).Scan(&nonce); err != nil {
		t.Fatal(err)
	}
	id := probeIdentity()
	inspector := &probeInspector{observations: []runtimepkg.ProcessObservation{
		{Exists: true, ProcessIdentity: runtimepkg.ProcessIdentity{PID: id.PID, PGID: id.PGID, StartedAtMS: id.StartedAtMS, Executable: id.Executable, ControlNonceHash: id.ControlNonceHash}},
		{Exists: true, ProcessIdentity: runtimepkg.ProcessIdentity{PID: id.PID, PGID: id.PGID, StartedAtMS: id.StartedAtMS, Executable: id.Executable, ControlNonceHash: id.ControlNonceHash}},
	}}
	coordinator := newProbeCoordinator(db, inspector, now)

	if err := coordinator.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	var state string
	if err := raw.QueryRow(`SELECT state FROM attempt_probes WHERE id=?`, probeID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "failed" {
		t.Fatalf("probe state = %s, want failed (absence_unconfirmed)", state)
	}
	var status, dispatch, closeReason, nextNonce string
	var escalation int
	if err := raw.QueryRow(`SELECT status,dispatch_state,COALESCE(close_reason,''),nonce,escalation_count FROM interrupts WHERE id=?`, interruptID).Scan(&status, &dispatch, &closeReason, &nextNonce, &escalation); err != nil {
		t.Fatal(err)
	}
	if status != "open" || closeReason != "" || dispatch != "batched" || escalation != 0 || nextNonce == nonce {
		t.Fatalf("interrupt = status=%s dispatch=%s close=%s escalation=%d nonce_rotated=%v", status, dispatch, closeReason, escalation, nextNonce != nonce)
	}
	var resolution string
	if err := raw.QueryRow(`SELECT COALESCE(attempt_resolution,'') FROM attempts WHERE run_id=? AND attempt_no=1`, probeRunID).Scan(&resolution); err != nil {
		t.Fatal(err)
	}
	if resolution != "" {
		t.Fatalf("probe failure wrote resolution marker %q", resolution)
	}
	var version int64
	if err := raw.QueryRow(`SELECT version FROM interrupts WHERE id=?`, interruptID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AdvanceInterrupt(context.Background(), storage.AdvanceInterruptCmd{InterruptID: interruptID, ExpectedVersion: version, ExpectedNonce: nextNonce, Kind: storage.AdvanceExpiry, NowMS: now.UnixMilli() + 20}); err != nil {
		t.Fatal(err)
	}
	var heldReason string
	if err := raw.QueryRow(`SELECT version,nonce FROM interrupts WHERE id=?`, interruptID).Scan(&version, &nextNonce); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AdvanceInterrupt(context.Background(), storage.AdvanceInterruptCmd{InterruptID: interruptID, ExpectedVersion: version, ExpectedNonce: nextNonce, Kind: storage.AdvanceExpiry, NowMS: now.UnixMilli() + 30}); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT status,dispatch_state,escalation_count,COALESCE(held_reason,'') FROM interrupts WHERE id=?`, interruptID).Scan(&status, &dispatch, &escalation, &heldReason); err != nil {
		t.Fatal(err)
	}
	if status != "open" || heldReason != "max_escalations" || escalation != 1 {
		t.Fatalf("cap projection=%s/%s/%d/%s", status, dispatch, escalation, heldReason)
	}
	var isolation string
	if err := raw.QueryRow(`SELECT isolation_state FROM attempts WHERE run_id=? AND attempt_no=1`, probeRunID).Scan(&isolation); err != nil {
		t.Fatal(err)
	}
	if isolation != "frozen" {
		t.Fatalf("isolation_state = %s, want frozen (retained)", isolation)
	}
}

// TestProbeTickIdentityMismatchFailsClosed proves a changed identity (PID reuse)
// is never treated as absence: the probe finalizes absence_unconfirmed.
func TestProbeTickIdentityMismatchFailsClosed(t *testing.T) {
	now := time.UnixMilli(20_000)
	db, raw, _, probeID := seedProbeFixture(t, now)
	id := probeIdentity()
	inspector := &probeInspector{observations: []runtimepkg.ProcessObservation{
		{Exists: true, ProcessIdentity: runtimepkg.ProcessIdentity{PID: id.PID, PGID: id.PGID, StartedAtMS: id.StartedAtMS + 1, Executable: id.Executable, ControlNonceHash: id.ControlNonceHash}},
	}}
	coordinator := newProbeCoordinator(db, inspector, now)

	if err := coordinator.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	var state string
	if err := raw.QueryRow(`SELECT state FROM attempt_probes WHERE id=?`, probeID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "failed" {
		t.Fatalf("probe state = %s, want failed (identity mismatch fails closed)", state)
	}
}

// TestProbeTickMissingIdentityFailsClosed proves a probe with no recorded
// wrapper identity cannot be observed and finalizes absence_unconfirmed.
func TestProbeTickMissingIdentityFailsClosed(t *testing.T) {
	now := time.UnixMilli(20_000)
	db, raw, _, probeID := seedProbeFixture(t, now)
	// Clear the wrapper identity block (all-NULL, satisfying the CHECK
	// constraint) to model a process_identity_unknown startup_stall.
	if _, err := raw.Exec(`UPDATE attempts SET wrapper_pid=NULL,wrapper_started_at_ms=NULL,wrapper_executable=NULL,wrapper_pgid=NULL,wrapper_instance_id=NULL,control_nonce_hash=NULL WHERE run_id=? AND attempt_no=1`, probeRunID); err != nil {
		t.Fatal(err)
	}
	inspector := &probeInspector{observations: []runtimepkg.ProcessObservation{{Exists: false}}}
	coordinator := newProbeCoordinator(db, inspector, now)

	if err := coordinator.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	var state string
	if err := raw.QueryRow(`SELECT state FROM attempt_probes WHERE id=?`, probeID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "failed" {
		t.Fatalf("probe state = %s, want failed (no identity -> unconfirmed)", state)
	}
	// The inspector must never have been consulted without an identity.
	if len(inspector.observations) != 1 {
		t.Fatalf("inspector consulted without identity: %d observations left", len(inspector.observations))
	}
}

// TestProbeTickCrashReplayFinalizesAtMostOnce proves idempotency: a probe left
// running by a crashed tick is re-scanned, re-observed and finalized exactly
// once. A second Tick is a stale no-op (the finalizer's CAS rejects it).
func TestProbeTickCrashReplayFinalizesAtMostOnce(t *testing.T) {
	now := time.UnixMilli(20_000)
	db, raw, interruptID, probeID := seedProbeFixture(t, now)
	// Simulate a crash mid-observation: mark the probe running, never finalized.
	if _, err := raw.Exec(`UPDATE attempt_probes SET state='running',started_at_ms=? WHERE id=?`, now.UnixMilli(), probeID); err != nil {
		t.Fatal(err)
	}
	inspector := &probeInspector{observations: []runtimepkg.ProcessObservation{{Exists: false}}}
	coordinator := newProbeCoordinator(db, inspector, now)

	if err := coordinator.Tick(context.Background()); err != nil {
		t.Fatalf("Tick (replay): %v", err)
	}
	var state string
	if err := raw.QueryRow(`SELECT state FROM attempt_probes WHERE id=?`, probeID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "succeeded" {
		t.Fatalf("probe state after replay = %s, want succeeded", state)
	}

	// A second Tick re-scans nothing (the probe is now succeeded) and must not
	// double-finalize: no second ack, no second isolation release.
	if err := coordinator.Tick(context.Background()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	var acks int
	if err := raw.QueryRow(`SELECT count(*) FROM outbox_operations WHERE kind='command_ack' AND interrupt_id=?`, interruptID).Scan(&acks); err != nil {
		t.Fatal(err)
	}
	if acks != 1 {
		t.Fatalf("command_ack operations = %d, want 1 (at-most-once finalizer)", acks)
	}
}

// TestProbeTickStaleFinalizerIsSwallowed proves a probe whose Interrupt was
// already closed (e.g. by a late fact winning the race) does not error the
// tick: the finalizer returns ErrRejectedStale and the worker swallows it.
func TestProbeTickStaleFinalizerIsSwallowed(t *testing.T) {
	now := time.UnixMilli(20_000)
	db, raw, interruptID, _ := seedProbeFixture(t, now)
	// A late fact already closed the Interrupt; the probe is still pending.
	if _, err := raw.Exec(`UPDATE interrupts SET status='closed',close_reason='superseded_by_fact',closed_at_ms=?,updated_at_ms=? WHERE id=?`, now.UnixMilli(), now.UnixMilli(), interruptID); err != nil {
		t.Fatal(err)
	}
	inspector := &probeInspector{observations: []runtimepkg.ProcessObservation{{Exists: false}}}
	coordinator := newProbeCoordinator(db, inspector, now)

	if err := coordinator.Tick(context.Background()); err != nil {
		t.Fatalf("Tick with stale Interrupt: %v (stale must be swallowed)", err)
	}
	// The probe stays pending|running (not finalized): the closed Interrupt
	// rejected the finalizer, and the worker must not force a transition.
	var state string
	if err := raw.QueryRow(`SELECT state FROM attempt_probes WHERE interrupt_id=?`, interruptID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "running" {
		t.Fatalf("probe state = %s, want running (stale finalizer left it untouched)", state)
	}
}
