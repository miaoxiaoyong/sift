package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/miaoxiaoyong/sift/internal/config"
)

// canonicalMetricsSnapshot builds a config snapshot canonical JSON whose
// metrics weights are fully controlled by the test, matching the flat
// {"metrics":{...}} shape CanonicalJSON produces.
func canonicalMetricsSnapshot(t *testing.T, weights map[string]float64) []byte {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Metrics.DesignApproval = weights["design_approval"]
	cfg.Metrics.GuardrailViolation = weights["guardrail_violation"]
	cfg.Metrics.CodeReview = weights["code_review"]
	cfg.Metrics.AgentBlocked = weights["agent_blocked"]
	cfg.Metrics.MergeConflict = weights["merge_conflict"]
	cfg.Metrics.FailureReview = weights["failure_review"]
	cfg.Metrics.StartupStall = weights["startup_stall"]
	canonical, err := config.CanonicalJSON(cfg)
	if err != nil {
		t.Fatalf("canonical json: %v", err)
	}
	return canonical
}

// seedInterruptWithDelivery inserts a closed/delivered Interrupt backed by a
// real budget charge, plus a delivered forge_comment delivery projection. This
// is the minimal shape the weighted-attention metric joins over.
func seedInterruptWithDelivery(t *testing.T, db *DB, ctx context.Context, interruptID, runID, reason, severity string, nowMS int64) {
	t.Helper()
	chargeID := "chg-" + interruptID
	if _, err := db.ExecForTest(ctx, `INSERT INTO budget_entries(id,kind,scope,scope_id,bucket_start_ms,amount,reason,run_id,operation_key,created_at_ms) VALUES(?,'attention','run',?,?,1,?,?,?,?)`, chargeID, runID, nowMS, reason, runID, "op-"+interruptID, nowMS); err != nil {
		t.Fatalf("seed budget entry: %v", err)
	}
	if _, err := db.ExecForTest(ctx, `INSERT INTO interrupts(id,run_id,generation_key,reason,severity,headline,brief_markdown,options_json,min_modality,links_json,nonce,nonce_issued_at_ms,version,status,close_reason,closed_at_ms,dispatch_state,expires_at_ms,on_expire,escalation_count,max_escalations,charged_budget_entry_id,created_at_ms,updated_at_ms) VALUES(?,?,?,?,?,?,?,?,?,'[]',?, ?,1,'closed','responded',?, 'ready',?,? ,0,0,?,?,?)`,
		interruptID, runID, "gen-"+interruptID, reason, severity, "h", "b", "[]", "text", "n-"+interruptID, nowMS, nowMS, nowMS+1000, "hold", chargeID, nowMS, nowMS); err != nil {
		t.Fatalf("seed interrupt: %v", err)
	}
	if _, err := db.ExecForTest(ctx, `INSERT INTO interrupt_deliveries(id,interrupt_id,surface,priority,operation_key,state,attempt_count,created_at_ms,delivered_at_ms) VALUES(?,?,'forge_comment','normal',?,'delivered',1,?,?)`,
		"del-"+interruptID, interruptID, "op-"+interruptID, nowMS, nowMS); err != nil {
		t.Fatalf("seed delivery: %v", err)
	}
}

// seedProjectWithSnapshot inserts a config snapshot carrying a custom canonical
// JSON plus its project, without the duplicate SeedProjectForTest snapshot.
func seedProjectWithSnapshot(t *testing.T, db *DB, ctx context.Context, cfgID, projectID string, canonical []byte, nowMS int64) {
	t.Helper()
	if _, err := db.ExecForTest(ctx, `INSERT INTO config_snapshots(id,config_hash,schema_version,canonical_json,source_present,loaded_at_ms,binary_version) VALUES(?,?,1,?,1,?,'test-binary')`, cfgID, "hash-"+cfgID, string(canonical), nowMS); err != nil {
		t.Fatalf("seed config snapshot: %v", err)
	}
	if _, err := db.ExecForTest(ctx, `INSERT INTO projects(id,config_snapshot_id,forge_kind,forge_host,forge_project_key,repo_path,enabled,health,capabilities_json,created_at_ms,updated_at_ms) VALUES(?,?,'github','github.com',?,?,'1','active','{}',?,?)`,
		projectID, cfgID, "org/repo-"+projectID, "/repo/"+projectID, nowMS, nowMS); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

// seedDoneRun creates a done forge Run bound to a config snapshot + project.
func seedDoneRun(t *testing.T, db *DB, ctx context.Context, runID, projectID, cfgID, changeID string, gateBypassed bool, nowMS int64) {
	t.Helper()
	bypass := 0
	if gateBypassed {
		bypass = 1
	}
	if _, err := db.ExecForTest(ctx, `INSERT INTO runs(id,source_kind,project_id,config_snapshot_id,forge_kind,forge_host,forge_project_key,issue_id,change_id,status,gate_bypassed,max_attempts,created_at_ms,updated_at_ms,completed_at_ms) VALUES(?,'forge',?,?,'github','github.com','org/repo',?,?,'done',?,3,?,?,?)`,
		runID, projectID, cfgID, "issue-"+runID, changeID, bypass, nowMS, nowMS, nowMS); err != nil {
		t.Fatalf("seed done run: %v", err)
	}
}

// seedSiftMergeOutbox records a succeeded merge_change outbox operation, the
// causal identity of a Sift-initiated merge (IsSiftMerge).
func seedSiftMergeOutbox(t *testing.T, db *DB, ctx context.Context, runID string, nowMS int64) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"change_id": "change-" + runID, "gate_evaluation_id": "geval-" + runID, "expected_head_sha": "head-" + runID})
	if _, err := db.ExecForTest(ctx, `INSERT INTO outbox_operations(id,operation_key,kind,run_id,state,payload_schema_version,payload_json,payload_digest,next_attempt_at_ms,created_at_ms,updated_at_ms,completed_at_ms) VALUES(?,?,'merge_change',?,'succeeded',1,?, 'digest',?,?,?,?)`,
		"op-merge-"+runID, "merge:"+runID, runID, string(payload), nowMS, nowMS, nowMS, nowMS); err != nil {
		t.Fatalf("seed sift merge outbox: %v", err)
	}
}

// seedT2DispatchAttempt inserts one valid T2 Brain dispatch call (project-bound)
// and its provider attempt with known tokens. The composite
// (id,selected_attempt_no)→brain_attempts FK is deferred, so both rows must
// land in one transaction.
func seedT2DispatchAttempt(t *testing.T, db *DB, ctx context.Context, projectID, runID, callID, attemptID string, nowMS int64) {
	t.Helper()
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO brain_calls(id,scope,subject_key,project_id,run_id,touchpoint,call_seq,prompt_version,output_schema_version,input_json,input_digest,status,selected_attempt_no,validated_output_json,started_at_ms,finished_at_ms) VALUES(?, 'run', ?, ?, ?, 'T2', 1, 'pv', 1, '{}', 'd', 'valid', 1, '{}', ?, ?)`,
		callID, "run:"+runID, projectID, runID, nowMS, nowMS); err != nil {
		t.Fatalf("seed brain call: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO brain_attempts(id,logical_call_id,provider_attempt,outcome,request_digest,input_tokens,output_tokens,raw_output_digest,started_at_ms,finished_at_ms) VALUES(?, ?, 1, 'valid', 'rd', 100, 200, 'rod', ?, ?)`,
		attemptID, callID, nowMS, nowMS); err != nil {
		t.Fatalf("seed brain attempt: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit tx: %v", err)
	}
}

// TestV11GateBypassExcludedFromFalseRelease is the V11 metrics segment: a done
// Run merged by a human (gate_bypassed=1) must NOT enter the false-release
// denominator and MUST be counted in the gate-bypass rate (PRD §10.2).
func TestV11GateBypassExcludedFromFalseRelease(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	const now = testNow
	cfgID, projectID := "cfg-1", "proj-1"
	if err := db.SeedProjectForTest(ctx, cfgID, projectID, now); err != nil {
		t.Fatal(err)
	}

	// runManual: done via human manual merge → gate_bypassed=1, NO merge_change outbox.
	seedDoneRun(t, db, ctx, "runManual", projectID, cfgID, "changeManual", true, now)
	// runSift: done via a Sift-initiated merge → succeeded merge_change outbox.
	seedDoneRun(t, db, ctx, "runSift", projectID, cfgID, "changeSift", false, now)
	seedSiftMergeOutbox(t, db, ctx, "runSift", now)

	report, err := db.Metrics(ctx, MetricsQuery{})
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}

	// False-release denominator = Sift-initiated merges only = 1 (runSift).
	if report.FalseReleaseRate.Denominator != 1 {
		t.Fatalf("false-release denominator = %v, want 1 (gate_bypassed manual merge excluded)", report.FalseReleaseRate.Denominator)
	}
	if report.FalseReleaseRate.Rate != 0 {
		t.Fatalf("false-release rate = %v, want 0 (numerator fails closed until revert/fix follow-ups are written)", report.FalseReleaseRate.Rate)
	}

	// Gate-bypass rate: 1 gate_bypassed done / 2 done = 0.5.
	if report.GateBypassRate.Numerator != 1 {
		t.Fatalf("gate-bypass numerator = %v, want 1 (manual merge counted here)", report.GateBypassRate.Numerator)
	}
	if report.GateBypassRate.Denominator != 2 {
		t.Fatalf("gate-bypass denominator = %v, want 2 (all done)", report.GateBypassRate.Denominator)
	}
	if report.GateBypassRate.Rate != 0.5 {
		t.Fatalf("gate-bypass rate = %v, want 0.5", report.GateBypassRate.Rate)
	}

	// Merged changes = 2 (both done runs carry a change_id).
	if report.WeightedAttentionPerChange.MergedChanges != 2 {
		t.Fatalf("merged changes = %v, want 2", report.WeightedAttentionPerChange.MergedChanges)
	}
}

// TestMetricsWeightedAttentionUsesFrozenWeights verifies the north star sums
// each delivered metric identity's frozen reason weight exactly once, using the
// config snapshot frozen at Run creation.
func TestMetricsWeightedAttentionUsesFrozenWeights(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	const now = testNow
	// Snapshot with non-default weights: code_review=20, design_approval=4.
	canonical := canonicalMetricsSnapshot(t, map[string]float64{
		"design_approval": 4, "code_review": 20, "guardrail_violation": 5,
		"agent_blocked": 5, "merge_conflict": 3, "failure_review": 5, "startup_stall": 5,
	})
	seedProjectWithSnapshot(t, db, ctx, "cfg-w", "proj-w", canonical, now)
	// Two done runs.
	seedDoneRun(t, db, ctx, "runA", "proj-w", "cfg-w", "changeA", false, now)
	seedDoneRun(t, db, ctx, "runB", "proj-w", "cfg-w", "changeB", false, now)
	seedSiftMergeOutbox(t, db, ctx, "runA", now)
	seedSiftMergeOutbox(t, db, ctx, "runB", now)
	// Delivered interrupts: one code_review (20) on runA, one design_approval (4) on runB,
	// and a second delivered projection on runA's interrupt must NOT double-count.
	seedInterruptWithDelivery(t, db, ctx, "intA", "runA", "code_review", "normal", now)
	seedInterruptWithDelivery(t, db, ctx, "intB", "runB", "design_approval", "normal", now)
	// Duplicate delivery on the same interrupt identity → still counted once.
	if _, err := db.ExecForTest(ctx, `INSERT INTO interrupt_deliveries(id,interrupt_id,surface,priority,operation_key,state,attempt_count,created_at_ms,delivered_at_ms) VALUES(?,?,'forge_comment','normal',?,'delivered',1,?,?)`,
		"delA2", "intA", "op-intA-2", now, now); err != nil {
		t.Fatal(err)
	}

	report, err := db.Metrics(ctx, MetricsQuery{})
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	w := report.WeightedAttentionPerChange
	if w.DeliveredMetricIdentity != 2 {
		t.Fatalf("delivered metric identities = %v, want 2 (dedup by interrupt id)", w.DeliveredMetricIdentity)
	}
	// 20 (code_review) + 4 (design_approval) = 24, over 2 merged changes = 12.
	if w.WeightedMinutes != 24 {
		t.Fatalf("weighted minutes = %v, want 24", w.WeightedMinutes)
	}
	if w.PerMergedChange != 12 {
		t.Fatalf("per merged change = %v, want 12", w.PerMergedChange)
	}
}

// TestMetricsGateConfusionAndHITL verifies the Gate miss/false-block rates come
// from settled calibration samples and the HITL rate counts Runs with ≥1
// Interrupt.
func TestMetricsGateConfusionAndHITL(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	const now = testNow
	if err := db.SeedProjectForTest(ctx, "cfg-c", "proj-c", now); err != nil {
		t.Fatal(err)
	}
	// Three runs; two carry an Interrupt (HITL).
	for _, runID := range []string{"r1", "r2", "r3"} {
		seedDoneRun(t, db, ctx, runID, "proj-c", "cfg-c", "change-"+runID, false, now)
	}
	seedInterruptWithDelivery(t, db, ctx, "i1", "r1", "code_review", "normal", now)
	seedInterruptWithDelivery(t, db, ctx, "i2", "r2", "agent_blocked", "normal", now)

	// Calibration samples (ledger.md §4.1): predicted vs human. Each needs a
	// gate input snapshot + evaluation to satisfy the FK chain.
	seedCalibration := func(id, runID, predicted, human string) {
		t.Helper()
		if _, err := db.ExecForTest(ctx, `INSERT INTO gate_input_snapshots(id,gate_input_hash,schema_version,canonical_json,head_sha,effective_policy_hash,certification_version,risk_source_version,created_at_ms) VALUES(?, ?,1,'{}','sha','ph','cv','rv',?)`, "snap-"+id, "hash-"+id, now); err != nil {
			t.Fatalf("seed gate snapshot: %v", err)
		}
		if _, err := db.ExecForTest(ctx, `INSERT INTO gate_evaluations(id,run_id,snapshot_id,gate_version,verdict_json,verdict_digest,cache_hit,created_at_ms) VALUES(?,?,?,'gv','{}',?,0,?)`, "geval-"+id, runID, "snap-"+id, "vd-"+id, now); err != nil {
			t.Fatalf("seed gate eval: %v", err)
		}
		if _, err := db.ExecForTest(ctx, `INSERT INTO calibration_entries(id,run_id,gate_evaluation_id,predicted_decision,human_decision,decision_source,gate_bypassed,features_json,predicted_at_ms,decided_at_ms) VALUES(?,?,?, ?,?, 'manual_merge',0,'{}',?,?)`,
			id, runID, "geval-"+id, predicted, human, now, now); err != nil {
			t.Fatalf("seed calibration: %v", err)
		}
	}
	// 2 leaks (allow predicted, human blocked), 1 false-block (block predicted, human allowed).
	// negative samples = 3 (all human=block below), positive = 1.
	seedCalibration("cal1", "r1", "allow", "block")
	seedCalibration("cal2", "r1", "allow", "block")
	seedCalibration("cal3", "r1", "block", "allow")
	seedCalibration("cal4", "r1", "allow", "block")

	report, err := db.Metrics(ctx, MetricsQuery{})
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	// Gate miss = leak(3) / negative(3) = 1.0
	if report.GateMissRate.Numerator != 3 || report.GateMissRate.Denominator != 3 || report.GateMissRate.Rate != 1 {
		t.Fatalf("gate miss = %+v, want 3/3=1.0", report.GateMissRate)
	}
	// False-block = fblock(1) / positive(1) = 1.0
	if report.GateFalseBlockRate.Numerator != 1 || report.GateFalseBlockRate.Denominator != 1 || report.GateFalseBlockRate.Rate != 1 {
		t.Fatalf("gate false-block = %+v, want 1/1=1.0", report.GateFalseBlockRate)
	}
	// HITL = 2 runs with interrupts / 3 total.
	if report.HITLRate.Numerator != 2 || report.HITLRate.Denominator != 3 {
		t.Fatalf("hitl rate = %+v, want 2/3", report.HITLRate)
	}
}

// TestMetricsAttentionQuota verifies the consumption reads the latest persisted
// daily bucket per severity.
func TestMetricsAttentionQuota(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	// Two buckets for 'normal'; the later one is current.
	if _, err := db.ExecForTest(ctx, `INSERT INTO budget_counters(kind,scope,scope_id,bucket_start_ms,bucket_end_ms,limit_value,consumed_value,version,updated_at_ms) VALUES('attention','severity','normal',100,200,5,2,1,100)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(ctx, `INSERT INTO budget_counters(kind,scope,scope_id,bucket_start_ms,bucket_end_ms,limit_value,consumed_value,version,updated_at_ms) VALUES('attention','severity','normal',200,300,5,4,1,200)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(ctx, `INSERT INTO budget_counters(kind,scope,scope_id,bucket_start_ms,bucket_end_ms,limit_value,consumed_value,version,updated_at_ms) VALUES('attention','severity','high',200,300,5,1,1,200)`); err != nil {
		t.Fatal(err)
	}
	report, err := db.Metrics(ctx, MetricsQuery{})
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	var normal, high *QuotaConsumption
	for i := range report.AttentionQuotaConsumption {
		switch report.AttentionQuotaConsumption[i].Severity {
		case "normal":
			normal = &report.AttentionQuotaConsumption[i]
		case "high":
			high = &report.AttentionQuotaConsumption[i]
		}
	}
	if normal == nil || normal.Consumed != 4 || normal.Limit != 5 || normal.Rate != 0.8 {
		t.Fatalf("normal quota = %+v, want consumed=4 limit=5 rate=0.8", normal)
	}
	if high == nil || high.Consumed != 1 || high.Rate != 0.2 {
		t.Fatalf("high quota = %+v, want consumed=1 rate=0.2", high)
	}
}

// TestMetricsLLMCostAndDispatch verifies token totals and the structural
// dispatch-accuracy note.
func TestMetricsLLMCostAndDispatch(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	const now = testNow
	if err := db.SeedProjectForTest(ctx, "cfg-l", "proj-l", now); err != nil {
		t.Fatal(err)
	}
	seedDoneRun(t, db, ctx, "rl", "proj-l", "cfg-l", "changeL", false, now)
	// One valid T2 call + one valid provider attempt with known tokens.
	// One valid T2 call + one valid provider attempt with known tokens. The
	// composite (call,selected_attempt_no)→brain_attempts FK is deferred, so both
	// rows must land in one transaction.
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO brain_calls(id,scope,subject_key,run_id,touchpoint,call_seq,prompt_version,output_schema_version,input_json,input_digest,status,selected_attempt_no,validated_output_json,started_at_ms,finished_at_ms) VALUES('bc1','run','run:rl','rl','T2',1,'pv',1,'{}','d','valid',1,'{}',?,?)`, now, now); err != nil {
		t.Fatalf("seed brain call: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO brain_attempts(id,logical_call_id,provider_attempt,outcome,request_digest,input_tokens,output_tokens,raw_output_digest,started_at_ms,finished_at_ms) VALUES('ba1','bc1',1,'valid','rd',100,200,'rod',?,?)`, now, now); err != nil {
		t.Fatalf("seed brain attempt: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	report, err := db.Metrics(ctx, MetricsQuery{})
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if report.LLMCostPerMergedChange.InputTokens != 100 || report.LLMCostPerMergedChange.OutputTokens != 200 {
		t.Fatalf("llm tokens = %+v, want in=100 out=200", report.LLMCostPerMergedChange)
	}
	if report.LLMCostPerMergedChange.MergedChanges != 1 || report.LLMCostPerMergedChange.PerMergedChangeTokens != 300 {
		t.Fatalf("llm per change = %+v, want 300/1", report.LLMCostPerMergedChange)
	}
	// Dispatch accuracy denominator = 1 T2 assignment, structural rate 1.0.
	if report.DispatchAccuracy.Denominator != 1 || report.DispatchAccuracy.Rate != 1 {
		t.Fatalf("dispatch accuracy = %+v, want denominator=1 rate=1 (structural)", report.DispatchAccuracy)
	}
}

// TestMetricsProjectScoped verifies MetricsQuery{ProjectID} scopes every series
// that supports it and — the round-1 P0 regression — does not raise the
// dual-WHERE SQL error in weightedAttention. All nine series must return.
func TestMetricsProjectScoped(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	const now = testNow

	// Project A: two done runs (one gate-bypassed), one Sift merge, one
	// delivered code_review interrupt, and one valid T2 dispatch on runA1.
	canonicalA := canonicalMetricsSnapshot(t, map[string]float64{
		"design_approval": 4, "code_review": 10, "guardrail_violation": 5,
		"agent_blocked": 5, "merge_conflict": 3, "failure_review": 5, "startup_stall": 5,
	})
	seedProjectWithSnapshot(t, db, ctx, "cfgA", "projA", canonicalA, now)
	seedDoneRun(t, db, ctx, "runA1", "projA", "cfgA", "changeA1", false, now)
	seedDoneRun(t, db, ctx, "runA2", "projA", "cfgA", "changeA2", true, now)
	seedSiftMergeOutbox(t, db, ctx, "runA1", now)
	seedInterruptWithDelivery(t, db, ctx, "intA1", "runA1", "code_review", "normal", now)
	seedT2DispatchAttempt(t, db, ctx, "projA", "runA1", "bcA1", "baA1", now)

	// Project B: one done run with its own Sift merge, delivered interrupt and
	// dispatch, to prove the scoped numbers below are real scoping, not global.
	if err := db.SeedProjectForTest(ctx, "cfgB", "projB", now); err != nil {
		t.Fatal(err)
	}
	seedDoneRun(t, db, ctx, "runB1", "projB", "cfgB", "changeB1", false, now)
	seedSiftMergeOutbox(t, db, ctx, "runB1", now)
	seedInterruptWithDelivery(t, db, ctx, "intB1", "runB1", "design_approval", "normal", now)
	seedT2DispatchAttempt(t, db, ctx, "projB", "runB1", "bcB1", "baB1", now)

	// Project-scoped report for A: no SQL error, nine series populated.
	scoped, err := db.Metrics(ctx, MetricsQuery{ProjectID: "projA"})
	if err != nil {
		t.Fatalf("project-scoped Metrics: %v", err)
	}

	// Weighted attention: only projA's code_review(10) interrupt, 2 merged → 5.
	if scoped.WeightedAttentionPerChange.MergedChanges != 2 {
		t.Fatalf("scoped merged changes = %v, want 2", scoped.WeightedAttentionPerChange.MergedChanges)
	}
	if scoped.WeightedAttentionPerChange.WeightedMinutes != 10 {
		t.Fatalf("scoped weighted minutes = %v, want 10 (projA code_review only)", scoped.WeightedAttentionPerChange.WeightedMinutes)
	}
	if scoped.WeightedAttentionPerChange.DeliveredMetricIdentity != 1 {
		t.Fatalf("scoped delivered identities = %v, want 1", scoped.WeightedAttentionPerChange.DeliveredMetricIdentity)
	}
	if scoped.WeightedAttentionPerChange.PerMergedChange != 5 {
		t.Fatalf("scoped per merged change = %v, want 5", scoped.WeightedAttentionPerChange.PerMergedChange)
	}

	// False-release denominator scoped to projA Sift merges = 1.
	if scoped.FalseReleaseRate.Denominator != 1 {
		t.Fatalf("scoped false-release denominator = %v, want 1", scoped.FalseReleaseRate.Denominator)
	}

	// Gate bypass: 1 gate_bypassed / 2 done in projA = 0.5.
	if scoped.GateBypassRate.Numerator != 1 || scoped.GateBypassRate.Denominator != 2 || scoped.GateBypassRate.Rate != 0.5 {
		t.Fatalf("scoped gate bypass = %+v, want 1/2=0.5", scoped.GateBypassRate)
	}

	// HITL: 1 run with an interrupt / 2 runs in projA = 0.5.
	if scoped.HITLRate.Numerator != 1 || scoped.HITLRate.Denominator != 2 || scoped.HITLRate.Rate != 0.5 {
		t.Fatalf("scoped hitl = %+v, want 1/2=0.5", scoped.HITLRate)
	}

	// Dispatch accuracy scoped to projA T2 calls = 1.
	if scoped.DispatchAccuracy.Denominator != 1 || scoped.DispatchAccuracy.Rate != 1 {
		t.Fatalf("scoped dispatch = %+v, want denominator=1 rate=1", scoped.DispatchAccuracy)
	}

	// Gate confusion / attention quota are intentionally not project-scoped and
	// llm cost token sum is not yet scoped (tracked as P2-2); they must still
	// return without error. The llm merged-changes denominator IS scoped.
	if scoped.GateMissRate.Coverage == "" || scoped.GateFalseBlockRate.Coverage == "" {
		t.Fatalf("unscoped series lost coverage notes: %+v / %+v", scoped.GateMissRate, scoped.GateFalseBlockRate)
	}
	if scoped.LLMCostPerMergedChange.MergedChanges != 2 {
		t.Fatalf("llm merged changes = %v, want 2 (denominator is scoped even though token sum is not)", scoped.LLMCostPerMergedChange.MergedChanges)
	}

	// Global report sees both projects: 3 merged, 2 Sift merges, 2 delivered
	// identities — proving the scoped numbers above are real scoping.
	global, err := db.Metrics(ctx, MetricsQuery{})
	if err != nil {
		t.Fatalf("global Metrics: %v", err)
	}
	if global.WeightedAttentionPerChange.MergedChanges != 3 {
		t.Fatalf("global merged changes = %v, want 3", global.WeightedAttentionPerChange.MergedChanges)
	}
	if global.FalseReleaseRate.Denominator != 2 {
		t.Fatalf("global false-release denominator = %v, want 2", global.FalseReleaseRate.Denominator)
	}
	if global.WeightedAttentionPerChange.DeliveredMetricIdentity != 2 {
		t.Fatalf("global delivered identities = %v, want 2", global.WeightedAttentionPerChange.DeliveredMetricIdentity)
	}
}

// TestTriggerStartedLatencyDistribution verifies the P50 anchors are read from
// persisted events and the distribution is computed over both-anchored runs.
func TestTriggerStartedLatencyDistribution(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	const now = testNow
	if err := db.SeedProjectForTest(ctx, "cfg-lat", "proj-lat", now); err != nil {
		t.Fatal(err)
	}
	// Seed two runs with trigger→started deltas of 10s and 30s.
	for i, runID := range []string{"lat1", "lat2"} {
		delta := int64(10000 * (i + 1))
		observed := now + int64(i)*1000
		started := observed + delta
		if _, err := db.ExecForTest(ctx, `INSERT INTO runs(id,source_kind,project_id,config_snapshot_id,forge_kind,forge_host,forge_project_key,issue_id,status,max_attempts,created_at_ms,updated_at_ms) VALUES(?,'forge','proj-lat','cfg-lat','github','github.com','org/repo',?,'running',3,?,?)`,
			runID, "issue-"+runID, observed, observed); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecForTest(ctx, `INSERT INTO events(id,run_id,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES(?,?,'proj-lat','intake.trigger_observed','forge',1,'{}',?,?)`,
			"ev-trig-"+runID, runID, observed, observed); err != nil {
			t.Fatal(err)
		}
		payload, _ := json.Marshal(map[string]any{"to": "running"})
		if _, err := db.ExecForTest(ctx, `INSERT INTO events(id,run_id,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES(?,?,'proj-lat','run.transitioned','agent',1,?,?,?)`,
			"ev-run-"+runID, runID, string(payload), started, started); err != nil {
			t.Fatal(err)
		}
	}
	// A non-running transition must not be mistaken for the start anchor.
	payloadDone, _ := json.Marshal(map[string]any{"to": "done"})
	if _, err := db.ExecForTest(ctx, `INSERT INTO events(id,run_id,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES('ev-done-lat1','lat1','proj-lat','run.transitioned','forge',1,?,?,?)`,
		string(payloadDone), now+999999, now+999999); err != nil {
		t.Fatal(err)
	}

	dist, err := db.TriggerStartedLatency(ctx, MetricsQuery{})
	if err != nil {
		t.Fatalf("latency: %v", err)
	}
	if dist.Count != 2 {
		t.Fatalf("count = %v, want 2", dist.Count)
	}
	// Sorted: 10s, 20s → min 10000, p50 (nearest-rank rank=1)=10000, p90 (rank=2)=20000, max 20000.
	if dist.MinMS != 10000 || dist.P50MS != 10000 || dist.P90MS != 20000 || dist.MaxMS != 20000 {
		t.Fatalf("distribution = %+v, want min=10000 p50=10000 p90=20000 max=20000", dist)
	}
}

// TestTriggerStartedLatencyZeroAllowed verifies a run whose agent started at the
// same instant the trigger was observed (LatencyMS==0) is included rather than
// dropped by an off-by-one guard. Regression for the round-1 P1 fix.
func TestTriggerStartedLatencyZeroAllowed(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	const now = testNow
	if err := db.SeedProjectForTest(ctx, "cfg-z", "proj-z", now); err != nil {
		t.Fatal(err)
	}
	// Zero-latency run: agent started at the same ms the trigger was observed.
	if _, err := db.ExecForTest(ctx, `INSERT INTO runs(id,source_kind,project_id,config_snapshot_id,forge_kind,forge_host,forge_project_key,issue_id,status,max_attempts,created_at_ms,updated_at_ms) VALUES(?,'forge','proj-z','cfg-z','github','github.com','org/repo',?,'running',3,?,?)`,
		"runZero", "issue-runZero", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(ctx, `INSERT INTO events(id,run_id,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES(?,?,'proj-z','intake.trigger_observed','forge',1,'{}',?,?)`,
		"ev-trig-zero", "runZero", now, now); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"to": "running"})
	if _, err := db.ExecForTest(ctx, `INSERT INTO events(id,run_id,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES(?,?,'proj-z','run.transitioned','agent',1,?,?,?)`,
		"ev-run-zero", "runZero", string(payload), now, now); err != nil {
		t.Fatal(err)
	}

	dist, err := db.TriggerStartedLatency(ctx, MetricsQuery{})
	if err != nil {
		t.Fatalf("latency: %v", err)
	}
	if dist.Count != 1 {
		t.Fatalf("count = %v, want 1 (zero-latency sample must be included)", dist.Count)
	}
	if len(dist.Samples) != 1 || dist.Samples[0].LatencyMS != 0 {
		t.Fatalf("samples = %+v, want one sample with LatencyMS=0", dist.Samples)
	}
	if dist.MinMS != 0 || dist.P50MS != 0 || dist.P90MS != 0 || dist.MaxMS != 0 {
		t.Fatalf("distribution = %+v, want all zero for a single zero-latency sample", dist)
	}
}

// TestMetricsEmptyIsHonest verifies a fresh database reports all nine series
// without error and with zero denominators rather than invented numbers.
func TestMetricsEmptyIsHonest(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	report, err := db.Metrics(ctx, MetricsQuery{})
	if err != nil {
		t.Fatalf("Metrics on empty db: %v", err)
	}
	if report.WeightedAttentionPerChange.WeightedMinutes != 0 || report.WeightedAttentionPerChange.MergedChanges != 0 {
		t.Fatalf("empty weighted attention = %+v, want zeros", report.WeightedAttentionPerChange)
	}
	if report.FalseReleaseRate.Denominator != 0 || report.GateBypassRate.Denominator != 0 {
		t.Fatalf("empty denominators = %+v / %+v, want zeros", report.FalseReleaseRate, report.GateBypassRate)
	}
	// Coverage notes must be non-empty for the series V0 cannot fully populate.
	if report.FalseReleaseRate.Coverage == "" || report.DispatchAccuracy.Coverage == "" {
		t.Fatalf("missing coverage notes: %+v / %+v", report.FalseReleaseRate, report.DispatchAccuracy)
	}
	if err := verifyJSONSerializable(report); err != nil {
		t.Fatalf("report not json-serializable: %v", err)
	}
}

func TestMetricsForgeAPIQuotaConsumption(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg-api-metrics", "proj-api-metrics", testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ChargeForgeAPICall(ctx, ChargeForgeAPICallCmd{
		ProjectID: "proj-api-metrics", CallAttemptKey: "api-metrics-1", NowMS: testNow,
		Limit: 10, WarningRatio: .8,
	}); err != nil {
		t.Fatalf("charge forge api: %v", err)
	}
	report, err := db.Metrics(ctx, MetricsQuery{
		NowMS: testNow, ForgeAPIHourlyLimit: 10, ForgeAPIWarningRatio: .8,
	})
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if len(report.ForgeAPIQuotaConsumption) != 1 {
		t.Fatalf("forge api quotas = %+v, want one project", report.ForgeAPIQuotaConsumption)
	}
	got := report.ForgeAPIQuotaConsumption[0]
	if got.ProjectID != "proj-api-metrics" || got.Consumed != 1 || got.Limit != 10 || got.Unit != "calls" {
		t.Fatalf("forge api quota = %+v, want proj-api-metrics 1/10 calls", got)
	}
}

func verifyJSONSerializable(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return fmt.Errorf("empty serialization")
	}
	return nil
}
