package storage

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestAdvanceInterruptEscalatesOnceAndRotatesNonce(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	insertTaskSpec(t, db, "spec", "run", 1)
	insertAttempt(t, db, "run", 1, "spec")
	mustExec(t, db, `INSERT INTO events(id,run_id,attempt_no,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES ('report-event','run',1,'project','report','agent',1,'{}',?,?)`, testNow, testNow)
	mustExec(t, db, `INSERT INTO report_receipts(id,run_id,attempt_no,report_key,report_kind,payload_digest,event_id,received_at_ms) VALUES ('report-1','run',1,'report-1','blocker','digest','report-event',?)`, testNow)
	batch := int64(testNow + 2)
	attempt := 1
	in, err := db.EmitInterrupt(ctx, EmitInterruptCmd{
		RunID: "run", ExpectedRunVersion: 1, AttemptNo: &attempt, Reason: InterruptAgentBlocked,
		Facts:      map[string]string{"blocker_summary": "blocked", "attempted_summary": "tried", "recommended_action": "ask", "agent_log_ref": "/log"},
		Generation: InterruptGeneration{AttemptNo: 1, Generation: 1, ReportID: "report-1"}, GatePhase: GateNone, GuardrailLevel: GuardrailNone,
		ExpiresAfterMS: 10, OnExpire: ExpireEscalate, OnMaxEscalations: ExpireHold, MaxEscalations: 1,
		AttentionDailyQuota: interruptQuota(), DayTimezone: "UTC", Source: SourceSystem, NowMS: testNow,
		Channels: []InterruptChannel{{ID: "voice", Capabilities: []string{"voice"}}}, BatchAtMS: &batch,
	})
	if err != nil {
		t.Fatal(err)
	}
	var nonce string
	if err := db.db.QueryRow(`SELECT nonce FROM interrupts WHERE id=?`, in.ID).Scan(&nonce); err != nil {
		t.Fatal(err)
	}
	advanced, err := db.AdvanceInterrupt(ctx, AdvanceInterruptCmd{InterruptID: in.ID, ExpectedVersion: 1, ExpectedNonce: nonce, Kind: AdvanceExpiry, NowMS: testNow + 10})
	if err != nil || !advanced {
		t.Fatalf("advance = %v, %v", advanced, err)
	}
	var severity, newNonce, delivery string
	var version, expires, next int64
	if err := db.db.QueryRow(`SELECT severity,nonce,delivery,version,expires_at_ms,next_dispatch_at_ms FROM interrupts WHERE id=?`, in.ID).Scan(&severity, &newNonce, &delivery, &version, &expires, &next); err != nil {
		t.Fatal(err)
	}
	if severity != string(SeverityHigh) || newNonce == nonce || delivery != "immediate" || version != 2 || expires != testNow+20 || next != testNow+10 {
		t.Fatalf("advanced row = %s/%s/%s/%d/%d/%d", severity, newNonce, delivery, version, expires, next)
	}
	if _, err := db.AdvanceInterrupt(ctx, AdvanceInterruptCmd{InterruptID: in.ID, ExpectedVersion: 1, ExpectedNonce: nonce, Kind: AdvanceExpiry, NowMS: testNow + 10}); err != ErrRejectedStale {
		t.Fatalf("stale advance error = %v, want ErrRejectedStale", err)
	}
	assertCount(t, db, "budget_entries", 1)
}

func TestAdvanceInterruptRepeatedCriticalFuseSealsCurrentAuthority(t *testing.T) {
	db, path := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{"run", "run-2"} {
		if err := db.SeedForgeRunForTest(ctx, run, "project", "cfg", map[string]string{"run": "42", "run-2": "43"}[run], testNow); err != nil {
			t.Fatal(err)
		}
	}
	emit := func(run string, now int64) Interrupt {
		cmd := t6Command(now)
		cmd.RunID = run
		cmd.Generation.ChangeID = "change-" + run
		cmd.ExpiresAfterMS, cmd.OnExpire, cmd.OnMaxEscalations, cmd.MaxEscalations = 10, ExpireEscalate, ExpireHold, 3
		cmd.CriticalTotalLimit, cmd.CriticalPerRunLimit = 1, 10
		batchAt := now + 1
		cmd.BatchAtMS = &batchAt
		cmd.Channels = []InterruptChannel{{ID: "ops", Capabilities: []string{"visual"}, Default: true}}
		in, err := emitTestInterrupt(t, ctx, db, cmd)
		if err != nil {
			t.Fatal(err)
		}
		return in
	}
	advance := func(id string, version int64, nonce string, now int64) (int64, string) {
		ok, err := db.AdvanceInterrupt(ctx, AdvanceInterruptCmd{InterruptID: id, ExpectedVersion: version, ExpectedNonce: nonce, Kind: AdvanceExpiry, NowMS: now})
		if err != nil || !ok {
			var gotVersion, expires int64
			var gotNonce, state string
			_ = db.db.QueryRow(`SELECT version,nonce,expires_at_ms,dispatch_state FROM interrupts WHERE id=?`, id).Scan(&gotVersion, &gotNonce, &expires, &state)
			t.Fatalf("advance %s = %v, %v (got version=%d nonce=%s expires=%d state=%s)", id, ok, err, gotVersion, gotNonce, expires, state)
		}
		var gotVersion int64
		var gotNonce string
		if err := db.db.QueryRow(`SELECT version,nonce FROM interrupts WHERE id=?`, id).Scan(&gotVersion, &gotNonce); err != nil {
			t.Fatal(err)
		}
		return gotVersion, gotNonce
	}

	// The first Interrupt occupies the sole admitted-critical slot.
	admitted := emit("run", testNow)
	var nonce string
	if err := db.db.QueryRow(`SELECT nonce FROM interrupts WHERE id=?`, admitted.ID).Scan(&nonce); err != nil {
		t.Fatal(err)
	}
	version, nonce := advance(admitted.ID, 1, nonce, testNow+10)
	_, _ = advance(admitted.ID, version, nonce, testNow+20) // admitted critical

	// The second one fuses twice. The second fuse must refresh only the
	// collecting authority, so sealing uses its newest nonce/version.
	fused := emit("run-2", testNow+100)
	if err := db.db.QueryRow(`SELECT nonce FROM interrupts WHERE id=?`, fused.ID).Scan(&nonce); err != nil {
		t.Fatal(err)
	}
	version, nonce = advance(fused.ID, 1, nonce, testNow+110)       // normal → high
	version, nonce = advance(fused.ID, version, nonce, testNow+120) // high → fused critical
	version, nonce = advance(fused.ID, version, nonce, testNow+130) // repeated fused critical
	var batch, authorityNonce string
	var authorityVersion int64
	if err := db.db.QueryRow(`SELECT batch_id FROM attention_batch_members WHERE interrupt_id=?`, fused.ID).Scan(&batch); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT interrupt_version,nonce FROM attention_batch_member_authority WHERE batch_id=? AND interrupt_id=?`, batch, fused.ID).Scan(&authorityVersion, &authorityNonce); err != nil {
		t.Fatal(err)
	}
	if authorityVersion != version || authorityNonce != nonce {
		t.Fatalf("authority = %d/%s, want %d/%s", authorityVersion, authorityNonce, version, nonce)
	}
	if _, err := db.db.Exec(`UPDATE attention_batch_member_authority SET nonce='forged' WHERE batch_id=? AND interrupt_id=?`, batch, fused.ID); err == nil || !strings.Contains(err.Error(), "current open Interrupt") {
		t.Fatalf("forged collecting authority update = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(ctx, OpenConfig{Path: path, BinaryVersion: "test-binary", Now: time.UnixMilli(testNow)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.PrepareDueAttentionBatches(ctx, testNow+1_000_000); err != nil {
		t.Fatal(err)
	}
	var payload string
	if err := db.db.QueryRow(`SELECT payload_json FROM attention_batches WHERE id=?`, batch).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, `"interrupt_version":4`) || !strings.Contains(payload, `"nonce":"`+nonce+`"`) {
		t.Fatalf("sealed payload did not use current authority: %s", payload)
	}
}

func TestAdvanceInterruptRepeatedFusedMemberCloseCancelsAfterRestart(t *testing.T) {
	db, path := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{"run", "run-2"} {
		if err := db.SeedForgeRunForTest(ctx, run, "project", "cfg", map[string]string{"run": "42", "run-2": "43"}[run], testNow); err != nil {
			t.Fatal(err)
		}
	}
	emit := func(run string, now int64) Interrupt {
		cmd := t6Command(now)
		cmd.RunID, cmd.Generation.ChangeID = run, "change-"+run
		cmd.ExpiresAfterMS, cmd.OnExpire, cmd.OnMaxEscalations, cmd.MaxEscalations = 10, ExpireEscalate, ExpireAutoReject, 3
		cmd.CriticalTotalLimit, cmd.CriticalPerRunLimit = 1, 10
		batchAt := now + 1
		cmd.BatchAtMS = &batchAt
		cmd.Channels = []InterruptChannel{{ID: "ops", Capabilities: []string{"visual"}, Default: true}}
		in, err := emitTestInterrupt(t, ctx, db, cmd)
		if err != nil {
			t.Fatal(err)
		}
		return in
	}
	advance := func(id string, now int64) {
		var version int64
		var nonce string
		if err := db.db.QueryRow(`SELECT version,nonce FROM interrupts WHERE id=?`, id).Scan(&version, &nonce); err != nil {
			t.Fatal(err)
		}
		if ok, err := db.AdvanceInterrupt(ctx, AdvanceInterruptCmd{InterruptID: id, ExpectedVersion: version, ExpectedNonce: nonce, Kind: AdvanceExpiry, NowMS: now}); err != nil || !ok {
			t.Fatalf("advance %s = %v, %v", id, ok, err)
		}
	}
	admitted := emit("run", testNow)
	advance(admitted.ID, testNow+10)
	advance(admitted.ID, testNow+20)
	fused := emit("run-2", testNow+100)
	advance(fused.ID, testNow+110)
	advance(fused.ID, testNow+120)
	advance(fused.ID, testNow+130)
	advance(fused.ID, testNow+140)
	var batch, status string
	var excluded int
	if err := db.db.QueryRow(`SELECT batch_id FROM attention_batch_members WHERE interrupt_id=?`, fused.ID).Scan(&batch); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT status FROM interrupts WHERE id=?`, fused.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT count(*) FROM attention_batch_members WHERE batch_id=? AND interrupt_id=? AND excluded_at_ms=?`, batch, fused.ID, testNow+140).Scan(&excluded); err != nil {
		t.Fatal(err)
	}
	if status != "closed" || excluded != 1 {
		t.Fatalf("fused close status/excluded = %s/%d", status, excluded)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(ctx, OpenConfig{Path: path, BinaryVersion: "test-binary", Now: time.UnixMilli(testNow)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.PrepareDueAttentionBatches(ctx, testNow+1_000_000); err != nil {
		t.Fatal(err)
	}
	var state string
	var operations int
	if err := db.db.QueryRow(`SELECT state FROM attention_batches WHERE id=?`, batch).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT count(*) FROM outbox_operations WHERE kind='channel_publish'`).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if state != "cancelled" || operations != 0 {
		t.Fatalf("fused batch state/channel operations = %s/%d", state, operations)
	}
}

func TestSupervisorInterruptTickDispatches(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	at := int64(testNow + 1)
	cmd := t6Command(testNow)
	cmd.BatchAtMS, cmd.Channels = &at, []InterruptChannel{{ID: "ops", Capabilities: []string{"visual"}}}
	in, err := emitTestInterrupt(t, ctx, db, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SupervisorInterruptTick(ctx, at); err != nil {
		t.Fatal(err)
	}
	var state string
	var next any
	if err := db.db.QueryRow(`SELECT dispatch_state,next_dispatch_at_ms FROM interrupts WHERE id=?`, in.ID).Scan(&state, &next); err != nil {
		t.Fatal(err)
	}
	if state != "batched" || next != nil {
		t.Fatalf("dispatch state = %q next=%v", state, next)
	}
}

func TestQuotaExhaustionCreatesBatchedInterruptWithoutCharge(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	cmd := t6Command(testNow)
	cmd.AttentionDailyQuota = map[InterruptSeverity]int{SeverityLow: 0, SeverityNormal: 0, SeverityHigh: 1}
	cmd.DailySummaryAt = "09:00"
	cmd.Channels = []InterruptChannel{{ID: "ops", Capabilities: []string{"visual"}}}
	got, err := emitTestInterrupt(t, ctx, db, cmd)
	if err != nil {
		t.Fatal(err)
	}
	var kind, charge, state string
	if err := db.db.QueryRow(`SELECT a.kind,COALESCE(a.attention_charge_entry_id,''),i.dispatch_state FROM attention_admissions a JOIN interrupts i ON i.id=a.interrupt_id WHERE a.interrupt_id=?`, got.ID).Scan(&kind, &charge, &state); err != nil {
		t.Fatal(err)
	}
	if kind != "quota_batched" || charge != "" || state != "batched" {
		t.Fatalf("admission=%s charge=%q state=%s", kind, charge, state)
	}
	assertCount(t, db, "budget_entries", 0)
	assertCount(t, db, "attention_batch_members", 1)
}

func TestAdvanceInterruptStartupStallAtLimitHoldsRatherThanAutoRejecting(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	insertTaskSpec(t, db, "spec", "run", 1)
	insertAttempt(t, db, "run", 1, "spec")
	attempt := 1
	in, err := db.EmitInterrupt(ctx, EmitInterruptCmd{
		RunID: "run", ExpectedRunVersion: 1, AttemptNo: &attempt, Reason: InterruptStartupStall,
		Facts:      map[string]string{"attempt_no": "1", "generation": "1", "diagnostic_cause": "termination_unconfirmed", "isolation_consequence": "worktree held", "recommended_action": "retry", "attempt_diagnostic_ref": "/attempt", "worktree_ref": "/worktree"},
		Generation: InterruptGeneration{AttemptNo: 1, Generation: 1}, GatePhase: GateNone, GuardrailLevel: GuardrailNone,
		ExpiresAfterMS: 10, OnExpire: ExpireEscalate, OnMaxEscalations: ExpireAutoReject, MaxEscalations: 0,
		AttentionDailyQuota: interruptQuota(), Source: SourceRecovery, NowMS: testNow,
	})
	if err == nil || in.ID != "" {
		t.Fatalf("startup_stall auto-reject policy must be rejected, got %#v, %v", in, err)
	}
	// A legitimate frozen policy still maps the exhausted startup_stall to hold.
	in, err = db.EmitInterrupt(ctx, EmitInterruptCmd{
		RunID: "run", ExpectedRunVersion: 1, AttemptNo: &attempt, Reason: InterruptStartupStall,
		Facts:      map[string]string{"attempt_no": "1", "generation": "1", "diagnostic_cause": "termination_unconfirmed", "isolation_consequence": "worktree held", "recommended_action": "retry", "attempt_diagnostic_ref": "/attempt", "worktree_ref": "/worktree"},
		Generation: InterruptGeneration{AttemptNo: 1, Generation: 1}, GatePhase: GateNone, GuardrailLevel: GuardrailNone,
		ExpiresAfterMS: 10, OnExpire: ExpireEscalate, OnMaxEscalations: ExpireHold, MaxEscalations: 0,
		AttentionDailyQuota: interruptQuota(), Source: SourceRecovery, NowMS: testNow,
		Channels: []InterruptChannel{{ID: "text", Capabilities: []string{"text"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var nonce string
	if err := db.db.QueryRow(`SELECT nonce FROM interrupts WHERE id=?`, in.ID).Scan(&nonce); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AdvanceInterrupt(ctx, AdvanceInterruptCmd{InterruptID: in.ID, ExpectedVersion: 1, ExpectedNonce: nonce, Kind: AdvanceExpiry, NowMS: testNow + 10}); err != nil {
		t.Fatal(err)
	}
	var status, held string
	if err := db.db.QueryRow(`SELECT status,held_reason FROM interrupts WHERE id=?`, in.ID).Scan(&status, &held); err != nil {
		t.Fatal(err)
	}
	if status != "open" || held != "max_escalations" {
		t.Fatalf("startup stall = %s/%s", status, held)
	}
}

func TestNextDailySummaryAtSkipsTheCurrentOccurrence(t *testing.T) {
	const at = int64(1785286800000)
	got, ok := NextDailySummaryAt(at, "Asia/Shanghai", "09:00")
	if !ok || got <= at {
		t.Fatalf("next summary at instant = %d, %v", got, ok)
	}
	oneMSLater, ok := NextDailySummaryAt(at+1, "Asia/Shanghai", "09:00")
	if !ok || oneMSLater != got {
		t.Fatalf("next summary after instant = %d, %v; want %d", oneMSLater, ok, got)
	}
}

func TestChannelRendererIncludesCanonicalCommands(t *testing.T) {
	rendered, commands, err := renderChannelInterrupt("标题", "说明", `[{"label":"log","target":"/log"}]`, `[{"id":"hold","label":"暂缓","effect":"等待","risk":"延迟"}]`, "run-1", "nonce-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || commands[0] != "/sift hold run-1 nonce-1 1h" {
		t.Fatalf("commands=%q rendered=%q", commands, rendered)
	}
	if !strings.Contains(rendered, "log: /log") || !strings.Contains(rendered, commands[0]) {
		t.Fatalf("incomplete renderer: %q", rendered)
	}
}

// TestSupervisorInterruptTickExpiryHoldRoutesHold proves the I4 supervisor
// tick scans the expiry predicate and only calls AdvanceInterrupt on the
// resulting rows. An on_expire=hold Interrupt flips to held/expiry in the
// tick, with version bumped but nonce preserved and no second charge.
func TestSupervisorInterruptTickExpiryHoldRoutesHold(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	cmd := t6Command(testNow)
	cmd.ExpiresAfterMS, cmd.OnExpire, cmd.OnMaxEscalations, cmd.MaxEscalations = 10, ExpireHold, ExpireHold, 1
	cmd.Channels = []InterruptChannel{{ID: "ops", Capabilities: []string{"visual"}}}
	cmd.T6 = func(context.Context, InterruptT6Input) (InterruptT6Output, error) {
		return InterruptT6Output{Delivery: "immediate", ChannelID: "ops"}, nil
	}
	in, err := emitTestInterrupt(t, ctx, db, cmd)
	if err != nil {
		t.Fatal(err)
	}
	var initialNonce string
	if err := db.db.QueryRow(`SELECT nonce FROM interrupts WHERE id=?`, in.ID).Scan(&initialNonce); err != nil {
		t.Fatal(err)
	}
	if err := db.SupervisorInterruptTick(ctx, testNow+10); err != nil {
		t.Fatal(err)
	}
	assertAdvanceOutcome(t, readAdvanceOutcome(t, db, in.ID), advanceOutcome{
		status: "open", dispatchState: "held", delivery: "held", severity: "normal",
		held: "expiry", closeReason: "",
		version: 2, escalation: 0, expiresAt: testNow + 10, nextDispatch: sql.NullInt64{},
		admissions: 1, charges: 1, channelOps: 1, members: 0, authority: 0,
	}, initialNonce, false)
	var event string
	if err := db.db.QueryRow(`SELECT type FROM events WHERE payload_json LIKE ? ORDER BY occurred_at_ms DESC LIMIT 1`, `%"interrupt_id":"`+in.ID+`"%`).Scan(&event); err != nil {
		t.Fatal(err)
	}
	if event != "interrupt.expired" {
		t.Fatalf("advance event = %q, want interrupt.expired", event)
	}
}

// TestSupervisorInterruptTickExpiryAutoRejectClosesRunAndInterrupt covers the
// expiry → auto_reject path through the I4 tick. The Interrupt must close
// with the right close reason while the Run transitions to failed.
func TestSupervisorInterruptTickExpiryAutoRejectClosesRunAndInterrupt(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	cmd := t6Command(testNow)
	cmd.ExpiresAfterMS, cmd.OnExpire, cmd.OnMaxEscalations, cmd.MaxEscalations = 10, ExpireAutoReject, ExpireAutoReject, 1
	cmd.Channels = []InterruptChannel{{ID: "ops", Capabilities: []string{"visual"}}}
	cmd.T6 = func(context.Context, InterruptT6Input) (InterruptT6Output, error) {
		return InterruptT6Output{Delivery: "immediate", ChannelID: "ops"}, nil
	}
	in, err := emitTestInterrupt(t, ctx, db, cmd)
	if err != nil {
		t.Fatal(err)
	}
	var initialNonce string
	if err := db.db.QueryRow(`SELECT nonce FROM interrupts WHERE id=?`, in.ID).Scan(&initialNonce); err != nil {
		t.Fatal(err)
	}
	if err := db.SupervisorInterruptTick(ctx, testNow+10); err != nil {
		t.Fatal(err)
	}
	assertAdvanceOutcome(t, readAdvanceOutcome(t, db, in.ID), advanceOutcome{
		status: "closed", dispatchState: "batched", delivery: "immediate", severity: "normal",
		held: "", closeReason: "expired_auto_reject",
		version: 2, escalation: 0, expiresAt: testNow + 10, nextDispatch: sql.NullInt64{},
		admissions: 1, charges: 1, channelOps: 1, members: 0, authority: 0,
	}, initialNonce, false)
	var status, failureReason string
	if err := db.db.QueryRow(`SELECT status, COALESCE(failure_reason,'') FROM runs WHERE id=?`, "run").Scan(&status, &failureReason); err != nil {
		t.Fatal(err)
	}
	if status != string(RunFailed) || failureReason != "hitl_expired" {
		t.Fatalf("run = %s/%s, want failed/hitl_expired", status, failureReason)
	}
}

// TestSupervisorInterruptTickEscalatesThenRedelivers exercises the full
// expires → next_dispatch cycle through two supervisor ticks. The first tick
// sees expires_at_ms<=now, escalates the Interrupt (version+1, new nonce),
// and re-freezes next_dispatch_at_ms to the next frozen summary. The second
// tick sees next_dispatch_at_ms<=now and seals the Interrupt into the daily
// batch with the bumped authority. Only AdvanceInterrupt runs between the two
// ticks; no second advance path is used.
func TestSupervisorInterruptTickEscalatesThenRedelivers(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	const expiry = int64(48 * 60 * 60 * 1000)
	initialBatchAt := int64(testNow + 60*60*1000) // one hour after emit, well before expiry.
	cmd := t6Command(testNow)
	cmd.ExpiresAfterMS, cmd.OnExpire, cmd.OnMaxEscalations, cmd.MaxEscalations = expiry, ExpireEscalate, ExpireHold, 1
	cmd.BatchAtMS = &initialBatchAt
	cmd.Channels = []InterruptChannel{{ID: "ops", Capabilities: []string{"visual"}}}
	cmd.T6 = func(context.Context, InterruptT6Input) (InterruptT6Output, error) {
		return InterruptT6Output{Delivery: "batch", ChannelID: "ops", SuggestedDowngrade: true}, nil
	}
	in, err := emitTestInterrupt(t, ctx, db, cmd)
	if err != nil {
		t.Fatal(err)
	}
	var initialNonce string
	if err := db.db.QueryRow(`SELECT nonce FROM interrupts WHERE id=?`, in.ID).Scan(&initialNonce); err != nil {
		t.Fatal(err)
	}
	if err := db.SupervisorInterruptTick(ctx, testNow+expiry); err != nil {
		t.Fatal(err)
	}
	var newNonce string
	var newDispatch sql.NullInt64
	var version int64
	if err := db.db.QueryRow(`SELECT nonce,version,next_dispatch_at_ms FROM interrupts WHERE id=?`, in.ID).Scan(&newNonce, &version, &newDispatch); err != nil {
		t.Fatal(err)
	}
	if newNonce == initialNonce || version != 2 || !newDispatch.Valid || newDispatch.Int64 <= testNow+expiry {
		t.Fatalf("post-escalation = nonce=%s version=%d due=%v", newNonce, version, newDispatch)
	}
	if err := db.SupervisorInterruptTick(ctx, newDispatch.Int64); err != nil {
		t.Fatal(err)
	}
	assertAdvanceOutcome(t, readAdvanceOutcome(t, db, in.ID), advanceOutcome{
		status: "open", dispatchState: "batched", delivery: "batch", severity: "normal",
		held: "", closeReason: "",
		version: 3, escalation: 1, expiresAt: testNow + 2*expiry, nextDispatch: sql.NullInt64{},
		admissions: 1, charges: 1, channelOps: 0, members: 1, authority: 1,
	}, newNonce, false)
	var batchID string
	var memberNonce string
	var memberVersion int64
	if err := db.db.QueryRow(`SELECT batch_id,nonce,interrupt_version FROM attention_batch_members WHERE interrupt_id=?`, in.ID).Scan(&batchID, &memberNonce, &memberVersion); err != nil {
		t.Fatal(err)
	}
	if memberVersion != 3 || memberNonce != newNonce {
		t.Fatalf("batch member = %s/%d/%s, want escalated authority", batchID, memberVersion, memberNonce)
	}
}

// TestSupervisorInterruptTickProcessesExpiryAndDispatchInOneCall proves the
// I4 tick scans both predicates in a single sweep and routes each candidate
// through AdvanceInterrupt with the right kind. One Interrupt reaches its
// expiry (AdvanceExpiry → hold), another reaches its frozen next_dispatch
// (AdvanceDispatch → batched). Only AdvanceInterrupt runs in the tick; no
// extra charges, members, or operations are created beyond the per-path
// expectations.
func TestSupervisorInterruptTickProcessesExpiryAndDispatchInOneCall(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run2", "project", "cfg", "43", testNow); err != nil {
		t.Fatal(err)
	}
	// First Interrupt: immediate delivery so the dispatch scan skips it; its
	// expiry predicate is the only one that fires at T.
	holdCmd := t6Command(testNow)
	holdCmd.RunID = "run"
	holdCmd.ExpiresAfterMS, holdCmd.OnExpire, holdCmd.OnMaxEscalations, holdCmd.MaxEscalations = 100, ExpireHold, ExpireHold, 1
	holdCmd.Channels = []InterruptChannel{{ID: "ops", Capabilities: []string{"visual"}}}
	holdCmd.T6 = func(context.Context, InterruptT6Input) (InterruptT6Output, error) {
		return InterruptT6Output{Delivery: "immediate", ChannelID: "ops"}, nil
	}
	holdInterrupt, err := emitTestInterrupt(t, ctx, db, holdCmd)
	if err != nil {
		t.Fatal(err)
	}
	// Second Interrupt: batched delivery with a frozen next_dispatch at T-1 so
	// the dispatch scan matches at T and only the dispatch path fires.
	batchAt := int64(testNow + 50)
	dispatchCmd := t6Command(testNow)
	dispatchCmd.RunID = "run2"
	dispatchCmd.Generation.ChangeID = "change-02"
	dispatchCmd.ExpiresAfterMS, dispatchCmd.OnExpire, dispatchCmd.OnMaxEscalations, dispatchCmd.MaxEscalations = 72*60*60*1000, ExpireEscalate, ExpireHold, 1
	dispatchCmd.BatchAtMS = &batchAt
	dispatchCmd.Channels = []InterruptChannel{{ID: "ops", Capabilities: []string{"visual"}}}
	dispatchCmd.T6 = func(context.Context, InterruptT6Input) (InterruptT6Output, error) {
		return InterruptT6Output{Delivery: "batch", ChannelID: "ops", SuggestedDowngrade: true}, nil
	}
	dispatchInterrupt, err := emitTestInterrupt(t, ctx, db, dispatchCmd)
	if err != nil {
		t.Fatal(err)
	}
	now := int64(testNow + 100)
	if err := db.SupervisorInterruptTick(ctx, now); err != nil {
		t.Fatal(err)
	}
	var holdState, holdHeld string
	var holdVersion int64
	if err := db.db.QueryRow(`SELECT dispatch_state, COALESCE(held_reason,''), version FROM interrupts WHERE id=?`, holdInterrupt.ID).Scan(&holdState, &holdHeld, &holdVersion); err != nil {
		t.Fatal(err)
	}
	if holdState != "held" || holdHeld != "expiry" || holdVersion != 2 {
		t.Fatalf("expiry outcome = %s/%s version=%d, want held/expiry/2", holdState, holdHeld, holdVersion)
	}
	var dispatchState string
	var dispatchVersion int64
	if err := db.db.QueryRow(`SELECT dispatch_state, version FROM interrupts WHERE id=?`, dispatchInterrupt.ID).Scan(&dispatchState, &dispatchVersion); err != nil {
		t.Fatal(err)
	}
	if dispatchState != "batched" || dispatchVersion != 2 {
		t.Fatalf("dispatch outcome = %s version=%d, want batched/2", dispatchState, dispatchVersion)
	}
	assertCount(t, db, "outbox_operations", 4) // 2 forge_comment + 1 immediate channel publish + 1 sealed daily batch publish
	var expiryCount, dispatchedCount int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM events WHERE type='interrupt.expired'`).Scan(&expiryCount); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM events WHERE type='interrupt.dispatched'`).Scan(&dispatchedCount); err != nil {
		t.Fatal(err)
	}
	if expiryCount != 1 || dispatchedCount != 1 {
		t.Fatalf("events = expired:%d dispatched:%d, want 1/1", expiryCount, dispatchedCount)
	}
}
