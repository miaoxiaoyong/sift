package storage

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func gateRecord(now int64) GateEvaluationRecord {
	return GateEvaluationRecord{
		RunID: "run", GateInputHash: strings.Repeat("a", 64), GateVersion: "gate/v1", SnapshotSchemaVersion: 1,
		SnapshotJSON: []byte(`{"schema_version":1}`), VerdictJSON: []byte(`{"schema_version":1,"kind":"hitl"}`),
		HeadSHA: strings.Repeat("b", 40), EffectivePolicyHash: strings.Repeat("c", 64), CertificationVersion: strings.Repeat("d", 64), RiskSourceVersion: "T3/fallback/v1",
		VerdictDigest: strings.Repeat("e", 64), ShadowDecision: "block", FeaturesJSON: []byte(`{"schema_version":1}`), NowMS: now,
	}
}

func TestRecordGateEvaluationAlwaysAppendsCalibration(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	r := gateRecord(testNow)
	if _, err := db.RecordGateEvaluation(ctx, r); err != nil {
		t.Fatal(err)
	}
	r.CacheHit, r.NowMS = true, testNow+1
	if _, err := db.RecordGateEvaluation(ctx, r); err != nil {
		t.Fatal(err)
	}
	assertCount(t, db, "gate_input_snapshots", 1)
	assertCount(t, db, "gate_evaluations", 2)
	assertCount(t, db, "calibration_entries", 2)
	assertCount(t, db, "ledger_entries", 2)
}

func TestExportReplayJSONLUsesFrozenGateAndLinkedBrainTrace(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	call, err := db.ReserveBrainCall(ctx, ReserveBrainCallCmd{Scope: BrainScopeRun, SubjectKey: "run:run", RunID: "run", Touchpoint: "T3", PromptVersion: "T3/v1", OutputSchemaVersion: 1, InputJSON: []byte(`{"run_id":"run"}`), InputDigest: testCallIDDigest, StartedAtMS: testNow})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordBrainAttempt(ctx, BrainAttemptCmd{CallID: call.ID, ProviderAttempt: 0, Outcome: BrainAttemptFallback, RequestDigest: testCallIDDigest, StartedAtMS: testNow, FinishedAtMS: testNow, TokenLimit: 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.FinalizeBrainCall(ctx, FinalizeBrainCallCmd{CallID: call.ID, Status: BrainCallFallback, FallbackReason: "provider_disabled", FinishedAtMS: testNow}); err != nil {
		t.Fatal(err)
	}
	r := gateRecord(testNow)
	r.BrainInputLinks = []GateBrainInputLink{{LogicalCallID: call.ID, Touchpoint: "T3"}}
	if _, err := db.RecordGateEvaluation(ctx, r); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := db.ExportReplayJSONL(ctx, &out); err != nil {
		t.Fatal(err)
	}
	var gateRecord, brainRecord map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record["record_type"] == "gate" {
			gateRecord = record
		} else if record["record_type"] == "brain_call" {
			brainRecord = record
		}
	}
	if gateRecord == nil || gateRecord["input"].(map[string]any)["schema_version"] != float64(1) {
		t.Fatalf("frozen gate record = %v", gateRecord)
	}
	if brainRecord == nil || brainRecord["schema_version"] != float64(2) {
		t.Fatalf("linked brain record = %v", brainRecord)
	}
	ids := brainRecord["gate_input_snapshot_ids"].([]any)
	if len(ids) != 1 {
		t.Fatalf("linked snapshot IDs = %v", ids)
	}
}

func TestGateHITLIsAtomicWithCalibration(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	cmd := EmitInterruptCmd{RunID: "run", ExpectedRunVersion: 1, Reason: InterruptCodeReview, Facts: map[string]string{"change_ref": "https://forge.example/change/1", "head_sha": "abc", "review_requirement": "required", "recommended_action": "approve", "diff_ref": "https://forge.example/change/1/diff"}, Generation: InterruptGeneration{ChangeID: "change-01", HeadSHA: "0123456789012345678901234567890123456789"}, GatePhase: GateNone, GuardrailLevel: GuardrailNone, AttentionDailyQuota: interruptQuota(), DayTimezone: "UTC", Source: SourceSystem, NowMS: testNow}
	r, in, err := db.RecordGateEvaluationAndEmitInterrupt(ctx, gateRecord(testNow), cmd)
	if err != nil {
		t.Fatal(err)
	}
	var bound string
	if err := db.db.QueryRowContext(ctx, `SELECT calibration_id FROM interrupts WHERE id=?`, in.ID).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if bound != r.CalibrationID {
		t.Fatalf("calibration binding = %q, want %q", bound, r.CalibrationID)
	}
	assertCount(t, db, "gate_evaluations", 1)
	assertCount(t, db, "calibration_entries", 1)
	assertCount(t, db, "ledger_entries", 1)
}
