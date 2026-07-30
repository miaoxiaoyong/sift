package storage

import (
	"context"
	"strconv"
	"sync"
	"testing"
)

// These tests are the spec-fixed acceptance vectors for the critical fuse and
// non-critical overage batching (interrupt.md §8.3, config.md §3.9,
// storage.md §6.3/§9.1/§9.3). They prove:
//   - the half-open window [created_at_ms, created_at_ms+window) counts exactly
//     at t+window-1ms and excludes t+window / t+window+1ms, on BOTH the
//     EmitInterrupt initial-critical path and the AdvanceInterrupt escalation
//     path;
//   - concurrent admission serializes so exactly limit Interrupts are admitted
//     and the rest fused, with one immutable admission each;
//   - a quota_batched Interrupt escalating to critical keeps charge=NULL whether
//     the fuse has room (critical_admitted) or is saturated (critical_fused),
//     while a charged high Interrupt escalating to critical reuses its charge;
//   - non-critical daily-quota CAS never borrows: at limit=1 one candidate is
//     quota_charged and the other quota_batched with NULL charge, at limit=2
//     both are charged and the counter never exceeds the limit.
//
// The fuse lives inside the sole emitters: initial critical via EmitInterrupt
// and escalation critical via AdvanceInterrupt both call admitCriticalTx in
// their CAS transaction; no caller can bypass the window check.

const (
	fuseWindowMS     = int64(600_000)                 // 10 minutes
	longFuseWindowMS = int64(7 * 24 * 60 * 60 * 1000) // 7 days
)

// criticalStartupCmd builds a startup_stall EmitInterruptCmd whose deterministic
// severity is already critical at first emission (base high + one promotion),
// so EmitInterrupt exercises the initial-critical admission path directly. It
// is the highest-base reason that needs no Gate calibration chain, only an
// attempt row. attemptNo makes the generation key unique, so a single Run may
// hold several distinct critical Interrupts (needed for the per-Run vectors).
func criticalStartupCmd(runID string, attemptNo int, now, window int64, total, perRun int) EmitInterruptCmd {
	no := attemptNo
	return EmitInterruptCmd{
		RunID: runID, ExpectedRunVersion: 1, AttemptNo: &no, Reason: InterruptStartupStall,
		Facts: map[string]string{
			"attempt_no": strconv.Itoa(attemptNo), "generation": "1",
			"diagnostic_cause": "termination_unconfirmed", "isolation_consequence": "worktree held",
			"recommended_action": "retry", "attempt_diagnostic_ref": "/attempt", "worktree_ref": "/worktree",
		},
		Generation: InterruptGeneration{AttemptNo: attemptNo, Generation: 1},
		GatePhase:  GateNone, GuardrailLevel: GuardrailNone,
		// startup_stall base is high; one promotion -> critical at first emit.
		EscalationCount: 1, MaxEscalations: 2,
		ExpiresAfterMS: 10, OnExpire: ExpireEscalate, OnMaxEscalations: ExpireHold,
		AttentionDailyQuota: interruptQuota(), Source: SourceRecovery, NowMS: now,
		CriticalWindowMS: window, CriticalTotalLimit: total, CriticalPerRunLimit: perRun,
		Channels: []InterruptChannel{{ID: "ops", Capabilities: []string{"text"}, Default: true}},
	}
}

// seedProjectOnce seeds the single shared project; every run hangs off it.
func seedProjectOnce(t *testing.T, db *DB) {
	t.Helper()
	if err := db.SeedProjectForTest(context.Background(), "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
}

// seedRun adds a forge-sourced queued run with a distinct issue id.
func seedRun(t *testing.T, db *DB, run, issue string) {
	t.Helper()
	if err := db.SeedForgeRunForTest(context.Background(), run, "project", "cfg", issue, testNow); err != nil {
		t.Fatal(err)
	}
}

// seedAttempt adds a pending attempt with its own task spec, at generation 1.
// The spec is versioned by attempt number so a Run may hold several attempts.
func seedAttempt(t *testing.T, db *DB, run string, no int) {
	t.Helper()
	spec := "spec-" + run + "-" + strconv.Itoa(no)
	insertTaskSpec(t, db, spec, run, no)
	insertAttempt(t, db, run, no, spec)
}

// seedAttempts seeds count attempts on run, terminalizing all but the last so
// the single-live-attempt invariant holds while each keeps a distinct
// (attempt_no, generation) key for startup_stall emission.
func seedAttempts(t *testing.T, db *DB, run string, count int) {
	t.Helper()
	for no := 1; no <= count; no++ {
		seedAttempt(t, db, run, no)
		if no < count {
			mustExec(t, db, `UPDATE attempts SET phase='finished', result_digest='digest', result_observed_at_ms=?, result_exit_code=0 WHERE run_id=? AND attempt_no=?`, testNow, run, no)
		}
	}
}

// readCriticalAdmission returns the kind, scope (via batch) and charge ref for
// the single critical admission of an Interrupt, failing if there is not
// exactly one.
func readCriticalAdmission(t *testing.T, db *DB, interruptID string) (kind, scope, charge string) {
	t.Helper()
	var count int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM attention_admissions WHERE interrupt_id=? AND kind IN ('critical_admitted','critical_fused')`, interruptID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("critical admissions for %s = %d, want 1", interruptID, count)
	}
	if err := db.db.QueryRow(`SELECT kind,COALESCE(attention_charge_entry_id,'') FROM attention_admissions WHERE interrupt_id=? AND kind IN ('critical_admitted','critical_fused')`, interruptID).Scan(&kind, &charge); err != nil {
		t.Fatal(err)
	}
	if kind == "critical_fused" {
		if err := db.db.QueryRow(`SELECT b.scope FROM attention_batch_members m JOIN attention_batches b ON b.id=m.batch_id WHERE m.interrupt_id=? AND b.kind='critical_fuse'`, interruptID).Scan(&scope); err != nil {
			t.Fatal(err)
		}
	}
	return kind, scope, charge
}

// advanceInterruptAt reads the current version/nonce and advances the Interrupt
// by expiry at the injected time. AdvanceInterrupt is the single write port
// between a supervisor tick and the Interrupt row.
func advanceInterruptAt(t *testing.T, ctx context.Context, db *DB, id string, now int64) {
	t.Helper()
	var version int64
	var nonce string
	if err := db.db.QueryRow(`SELECT version,nonce FROM interrupts WHERE id=?`, id).Scan(&version, &nonce); err != nil {
		t.Fatal(err)
	}
	ok, err := db.AdvanceInterrupt(ctx, AdvanceInterruptCmd{InterruptID: id, ExpectedVersion: version, ExpectedNonce: nonce, Kind: AdvanceExpiry, NowMS: now})
	if err != nil || !ok {
		t.Fatalf("advance %s @%d = %v, %v", id, now, ok, err)
	}
}

// emitCritical emits a startup_stall Interrupt that is critical at first
// emission and returns it, failing on any error. It reads the current run
// version so several critical Interrupts may be emitted against one Run.
func emitCritical(t *testing.T, ctx context.Context, db *DB, runID string, attemptNo int, now, window int64, total, perRun int) Interrupt {
	t.Helper()
	var version int64
	if err := db.db.QueryRow(`SELECT version FROM runs WHERE id=?`, runID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	cmd := criticalStartupCmd(runID, attemptNo, now, window, total, perRun)
	cmd.ExpectedRunVersion = version
	in, err := db.EmitInterrupt(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// TestCriticalFuseEmitInterruptWindowBoundary proves the initial-critical path
// in EmitInterrupt enforces the half-open evidence window. With total_limit=1
// the first critical is admitted; a second critical emitted at t+window-1ms
// still sees the first counting and fuses, while one emitted at t+window (and
// t+window+1ms) admits because the first evidence has left the window.
func TestCriticalFuseEmitInterruptWindowBoundary(t *testing.T) {
	cases := []struct {
		name   string
		offset int64
		admit  bool
	}{
		{"window_minus_1ms_counts", fuseWindowMS - 1, false},
		{"window_excludes", fuseWindowMS, true},
		{"window_plus_1ms_excludes", fuseWindowMS + 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := openTestDB(t)
			ctx := context.Background()
			seedProjectOnce(t, db)
			seedRun(t, db, "a", "41")
			seedRun(t, db, "b", "42")
			seedAttempt(t, db, "a", 1)
			seedAttempt(t, db, "b", 1)
			const t0 = int64(1_700_000_000_000)
			anchor := emitCritical(t, ctx, db, "a", 1, t0, fuseWindowMS, 1, 1)
			if kind, _, _ := readCriticalAdmission(t, db, anchor.ID); kind != "critical_admitted" {
				t.Fatalf("anchor admission = %s, want critical_admitted", kind)
			}
			probe := emitCritical(t, ctx, db, "b", 1, t0+tc.offset, fuseWindowMS, 1, 1)
			kind, scope, charge := readCriticalAdmission(t, db, probe.ID)
			if tc.admit {
				if kind != "critical_admitted" {
					t.Fatalf("probe at t+%d admitted = %s, want critical_admitted (window should exclude anchor)", tc.offset, kind)
				}
			} else {
				if kind != "critical_fused" || scope != "global" {
					t.Fatalf("probe at t+%d = %s/%s, want critical_fused/global (window should still count anchor)", tc.offset, kind, scope)
				}
			}
			if charge != "" {
				t.Fatalf("initial-critical admission charge = %q, want empty (critical never charges daily quota)", charge)
			}
			assertCount(t, db, "budget_entries", 0)
		})
	}
}

// TestCriticalFuseAdvanceInterruptWindowBoundary proves the escalation path in
// AdvanceInterrupt enforces the same half-open window. The anchor escalates to
// critical at tCrit; a probe escalated at tCrit+window-1ms fuses, while one
// escalated at tCrit+window admits. Escalation from a charged high Interrupt
// reuses its original charge (never NULL, never a second entry).
func TestCriticalFuseAdvanceInterruptWindowBoundary(t *testing.T) {
	cases := []struct {
		name   string
		offset int64
		admit  bool
	}{
		{"window_minus_1ms_counts", fuseWindowMS - 1, false},
		{"window_excludes", fuseWindowMS, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := openTestDB(t)
			ctx := context.Background()
			seedProjectOnce(t, db)
			seedRun(t, db, "a", "41")
			seedRun(t, db, "b", "42")
			const t0 = int64(1_700_000_000_000)
			emit := func(run string, now int64) Interrupt {
				cmd := t6Command(now)
				cmd.RunID = run
				cmd.Generation.ChangeID = "change-" + run
				cmd.ExpiresAfterMS, cmd.OnExpire, cmd.OnMaxEscalations, cmd.MaxEscalations = 10, ExpireEscalate, ExpireHold, 3
				cmd.CriticalWindowMS, cmd.CriticalTotalLimit, cmd.CriticalPerRunLimit = fuseWindowMS, 1, 1
				at := now + 1
				cmd.BatchAtMS = &at
				cmd.Channels = []InterruptChannel{{ID: "ops", Capabilities: []string{"visual"}, Default: true}}
				in, err := emitTestInterrupt(t, ctx, db, cmd)
				if err != nil {
					t.Fatal(err)
				}
				return in
			}
			anchor := emit("a", t0)
			advanceInterruptAt(t, ctx, db, anchor.ID, t0+10) // normal -> high
			advanceInterruptAt(t, ctx, db, anchor.ID, t0+20) // high -> critical (admitted)
			const tCrit = t0 + 20
			if kind, _, _ := readCriticalAdmission(t, db, anchor.ID); kind != "critical_admitted" {
				t.Fatalf("anchor escalation = %s, want critical_admitted", kind)
			}
			probe := emit("b", t0)
			advanceInterruptAt(t, ctx, db, probe.ID, t0+10)           // normal -> high
			advanceInterruptAt(t, ctx, db, probe.ID, tCrit+tc.offset) // high -> critical
			kind, scope, charge := readCriticalAdmission(t, db, probe.ID)
			if tc.admit {
				if kind != "critical_admitted" {
					t.Fatalf("probe escalation at tCrit+%d = %s, want critical_admitted", tc.offset, kind)
				}
			} else {
				if kind != "critical_fused" || scope != "global" {
					t.Fatalf("probe escalation at tCrit+%d = %s/%s, want critical_fused/global", tc.offset, kind, scope)
				}
			}
			if charge == "" {
				t.Fatalf("escalation critical charge is empty; high->critical must reuse the original charge")
			}
			assertCount(t, db, "budget_entries", 2) // one quota_charged initial emit each; escalation reuses, never doubles
		})
	}
}

// TestCriticalFuseConcurrentAdmissionSerializes proves that N concurrent
// critical admissions against total_limit=K produce exactly K critical_admitted
// and N-K critical_fused, each Interrupt owning exactly one immutable
// admission. The single-writer pool serializes the CAS; the goroutine fan-out
// is -race clean.
func TestCriticalFuseConcurrentAdmissionSerializes(t *testing.T) {
	const total = 2
	const n = 6
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedProjectOnce(t, db)
	for i := 0; i < n; i++ {
		rid := "run-" + strconv.Itoa(i)
		seedRun(t, db, rid, strconv.Itoa(40+i))
		seedAttempt(t, db, rid, 1)
	}
	const t0 = int64(1_700_000_000_000)
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = db.EmitInterrupt(ctx, criticalStartupCmd("run-"+strconv.Itoa(i), 1, t0, fuseWindowMS, total, total))
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent emit failed: %v", err)
		}
	}
	var admitted, fused int
	rows, err := db.db.Query(`SELECT kind FROM attention_admissions WHERE kind IN ('critical_admitted','critical_fused')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		if k == "critical_admitted" {
			admitted++
		} else {
			fused++
		}
	}
	if admitted != total || fused != n-total {
		t.Fatalf("admitted/fused = %d/%d, want %d/%d", admitted, fused, total, n-total)
	}
	assertCount(t, db, "attention_admissions", n) // exactly one admission per Interrupt, no duplicates
	assertCount(t, db, "budget_entries", 0)       // critical never charges the daily quota counter
	// Re-pushing the same Interrupts returns the existing admission and never
	// re-occupies a slot.
	for i := 0; i < n; i++ {
		emitCritical(t, ctx, db, "run-"+strconv.Itoa(i), 1, t0, fuseWindowMS, total, total)
	}
	assertCount(t, db, "attention_admissions", n)
}

// TestCriticalFuseGlobalPreferredOverPerRun proves that when a candidate
// simultaneously exceeds both the global and per-Run limits it joins the global
// critical batch, and that a per-Run-only saturation joins the run batch.
func TestCriticalFuseGlobalPreferredOverPerRun(t *testing.T) {
	t.Run("both_saturated_goes_global", func(t *testing.T) {
		db, _ := openTestDB(t)
		ctx := context.Background()
		seedProjectOnce(t, db)
		seedRun(t, db, "a", "41")
		seedAttempts(t, db, "a", 2)
		emitCritical(t, ctx, db, "a", 1, testNow, fuseWindowMS, 1, 1)
		second := emitCritical(t, ctx, db, "a", 2, testNow+1, fuseWindowMS, 1, 1)
		kind, scope, _ := readCriticalAdmission(t, db, second.ID)
		if kind != "critical_fused" || scope != "global" {
			t.Fatalf("both-saturated = %s/%s, want critical_fused/global", kind, scope)
		}
	})
	t.Run("per_run_only_goes_run", func(t *testing.T) {
		db, _ := openTestDB(t)
		ctx := context.Background()
		seedProjectOnce(t, db)
		seedRun(t, db, "run-0", "41")
		seedRun(t, db, "run-1", "42")
		seedAttempts(t, db, "run-0", 2)
		seedAttempt(t, db, "run-1", 1)
		emitCritical(t, ctx, db, "run-0", 1, testNow, fuseWindowMS, 2, 1)
		second := emitCritical(t, ctx, db, "run-0", 2, testNow+1, fuseWindowMS, 2, 1)
		kind, scope, _ := readCriticalAdmission(t, db, second.ID)
		if kind != "critical_fused" || scope != "run" {
			t.Fatalf("per-run-only = %s/%s, want critical_fused/run", kind, scope)
		}
		// A different Run still finds a global slot, proving the run-scope fuse
		// did not consume a global slot it did not need.
		other := emitCritical(t, ctx, db, "run-1", 1, testNow+2, fuseWindowMS, 2, 1)
		if kind, _, _ := readCriticalAdmission(t, db, other.ID); kind != "critical_admitted" {
			t.Fatalf("other run = %s, want critical_admitted", kind)
		}
	})
}

// TestCriticalFuseQuotaBatchedToCritical proves the two fixed vectors for an
// Interrupt that entered as quota_batched (NULL charge) and later escalates to
// critical: with fuse room it writes critical_admitted with charge still NULL,
// and when saturated it writes critical_fused with charge still NULL. No new
// attention charge is fabricated in either case.
func TestCriticalFuseQuotaBatchedToCritical(t *testing.T) {
	const expiry = int64(72 * 60 * 60 * 1000) // 72h: the daily summary fits before expiry
	emitBatchedHigh := func(t *testing.T, ctx context.Context, db *DB, run string, now int64, total, perRun int) Interrupt {
		cmd := t6Command(now)
		cmd.RunID = run
		cmd.Generation.ChangeID = "change-" + run
		// GateMerge promotes code_review normal -> high; high quota=0 forces
		// quota_batched with a NULL charge at first emission.
		cmd.GatePhase = GateMerge
		cmd.AttentionDailyQuota = map[InterruptSeverity]int{SeverityLow: 3, SeverityNormal: 5, SeverityHigh: 0}
		cmd.ExpiresAfterMS, cmd.OnExpire, cmd.OnMaxEscalations, cmd.MaxEscalations = expiry, ExpireEscalate, ExpireHold, 3
		cmd.CriticalWindowMS, cmd.CriticalTotalLimit, cmd.CriticalPerRunLimit = longFuseWindowMS, total, perRun
		at := now + 1
		cmd.BatchAtMS = &at
		cmd.Channels = []InterruptChannel{{ID: "ops", Capabilities: []string{"visual"}, Default: true}}
		in, err := emitTestInterrupt(t, ctx, db, cmd)
		if err != nil {
			t.Fatal(err)
		}
		var kind, charge, state string
		if err := db.db.QueryRow(`SELECT a.kind,COALESCE(a.attention_charge_entry_id,''),i.dispatch_state FROM attention_admissions a JOIN interrupts i ON i.id=a.interrupt_id WHERE a.interrupt_id=?`, in.ID).Scan(&kind, &charge, &state); err != nil {
			t.Fatal(err)
		}
		if kind != "quota_batched" || charge != "" || state != "batched" {
			t.Fatalf("initial = %s/%q/%s, want quota_batched/empty/batched", kind, charge, state)
		}
		return in
	}

	t.Run("room_admits_with_null_charge", func(t *testing.T) {
		db, _ := openTestDB(t)
		ctx := context.Background()
		seedProjectOnce(t, db)
		seedRun(t, db, "a", "41")
		const t0 = int64(1_700_000_000_000)
		in := emitBatchedHigh(t, ctx, db, "a", t0, 1, 1)
		advanceInterruptAt(t, ctx, db, in.ID, t0+expiry) // high -> critical
		kind, _, charge := readCriticalAdmission(t, db, in.ID)
		if kind != "critical_admitted" {
			t.Fatalf("quota_batched->critical = %s, want critical_admitted", kind)
		}
		if charge != "" {
			t.Fatalf("critical charge = %q, want empty (initial was quota_batched, no charge to reuse)", charge)
		}
		assertCount(t, db, "budget_entries", 0) // no charge fabricated at any step
		// Re-advancing (old tick) returns the existing critical admission.
		advanceInterruptAt(t, ctx, db, in.ID, t0+2*expiry)
		assertCount(t, db, "attention_admissions", 2) // initial + critical, no duplicate
	})

	t.Run("saturated_fuses_with_null_charge", func(t *testing.T) {
		db, _ := openTestDB(t)
		ctx := context.Background()
		seedProjectOnce(t, db)
		seedRun(t, db, "run-0", "41")
		seedRun(t, db, "run-1", "42")
		seedAttempt(t, db, "run-0", 1)
		const t0 = int64(1_700_000_000_000)
		// Occupy the sole global critical slot with an initial critical within
		// the long window so it still counts at the later escalation tick.
		emitCritical(t, ctx, db, "run-0", 1, t0, longFuseWindowMS, 1, 1)
		in := emitBatchedHigh(t, ctx, db, "run-1", t0, 1, 1)
		advanceInterruptAt(t, ctx, db, in.ID, t0+expiry) // high -> critical (fused)
		kind, scope, charge := readCriticalAdmission(t, db, in.ID)
		if kind != "critical_fused" || scope != "global" {
			t.Fatalf("quota_batched->critical saturated = %s/%s, want critical_fused/global", kind, scope)
		}
		if charge != "" {
			t.Fatalf("fused critical charge = %q, want empty", charge)
		}
		assertCount(t, db, "budget_entries", 0)
	})
}

// TestAttentionQuotaConcurrentNoBorrowing proves the non-critical daily-quota
// CAS never borrows. At limit=1 two concurrent candidates yield exactly one
// quota_charged (with charge) and one quota_batched (NULL charge); at limit=2
// both are charged. The counter never exceeds the limit and the zero-row CAS
// is proven exhausted by the re-read, not assumed.
func TestAttentionQuotaConcurrentNoBorrowing(t *testing.T) {
	emitNormal := func(t *testing.T, ctx context.Context, db *DB, run string, now int64, quota int) (Interrupt, error) {
		cmd := t6Command(now)
		cmd.RunID = run
		cmd.Generation.ChangeID = "change-" + run
		cmd.AttentionDailyQuota = map[InterruptSeverity]int{SeverityLow: 3, SeverityNormal: quota, SeverityHigh: 5}
		cmd.Channels = []InterruptChannel{{ID: "ops", Capabilities: []string{"visual"}, Default: true}}
		return emitTestInterrupt(t, ctx, db, cmd)
	}
	checkCounter := func(t *testing.T, db *DB, severity string, want int64) {
		t.Helper()
		var consumed int64
		if err := db.db.QueryRow(`SELECT consumed_value FROM budget_counters WHERE kind='attention' AND scope='severity' AND scope_id=?`, severity).Scan(&consumed); err != nil {
			t.Fatal(err)
		}
		if consumed != want {
			t.Fatalf("normal consumed = %d, want %d (must never exceed limit)", consumed, want)
		}
	}

	t.Run("limit1_one_charged_one_batched", func(t *testing.T) {
		db, _ := openTestDB(t)
		ctx := context.Background()
		seedProjectOnce(t, db)
		seedRun(t, db, "run-0", "41")
		seedRun(t, db, "run-1", "42")
		const t0 = int64(1_700_000_000_000)
		var wg sync.WaitGroup
		ins := make([]Interrupt, 2)
		errs := make([]error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ins[i], errs[i] = emitNormal(t, ctx, db, "run-"+strconv.Itoa(i), t0, 1)
			}(i)
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatalf("concurrent emit failed: %v", err)
			}
		}
		var charged, batched int
		for _, in := range ins {
			var kind, charge string
			if err := db.db.QueryRow(`SELECT kind,COALESCE(attention_charge_entry_id,'') FROM attention_admissions WHERE interrupt_id=? AND admission_key=?`, in.ID, in.ID+":initial").Scan(&kind, &charge); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "quota_charged":
				charged++
				if charge == "" {
					t.Fatalf("quota_charged admission has NULL charge")
				}
			case "quota_batched":
				batched++
				if charge != "" {
					t.Fatalf("quota_batched admission has fabricated charge %q", charge)
				}
			default:
				t.Fatalf("unexpected admission kind %q", kind)
			}
		}
		if charged != 1 || batched != 1 {
			t.Fatalf("charged/batched = %d/%d, want 1/1", charged, batched)
		}
		checkCounter(t, db, "normal", 1) // never borrowed past the limit
	})

	t.Run("limit2_both_charged", func(t *testing.T) {
		db, _ := openTestDB(t)
		ctx := context.Background()
		seedProjectOnce(t, db)
		seedRun(t, db, "run-0", "41")
		seedRun(t, db, "run-1", "42")
		const t0 = int64(1_700_000_000_000)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = emitNormal(t, ctx, db, "run-"+strconv.Itoa(i), t0, 2)
			}(i)
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatalf("concurrent emit failed: %v", err)
			}
		}
		assertCount(t, db, "budget_entries", 2)
		checkCounter(t, db, "normal", 2)
	})
}
