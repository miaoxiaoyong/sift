package storage

import (
	"context"
	"strings"
	"testing"

	"github.com/miaoxiaoyong/sift/internal/config"
)

func TestRecordHumanDecisionSettlesBoundCalibrationAndProjectsCertification(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE runs SET kind='bug' WHERE id='run'`); err != nil {
		t.Fatal(err)
	}
	r := gateRecord(testNow)
	cmd := EmitInterruptCmd{RunID: "run", ExpectedRunVersion: 1, Reason: InterruptCodeReview, Facts: map[string]string{"change_ref": "https://forge.example/change/1", "head_sha": "abc", "review_requirement": "required", "recommended_action": "approve", "diff_ref": "https://forge.example/change/1/diff"}, Generation: InterruptGeneration{ChangeID: "change-01", HeadSHA: strings.Repeat("a", 40)}, GatePhase: GateNone, GuardrailLevel: GuardrailNone, AttentionDailyQuota: interruptQuota(), DayTimezone: "UTC", Source: SourceSystem, NowMS: testNow}
	recorded, interrupt, err := db.RecordGateEvaluationAndEmitInterrupt(ctx, r, cmd)
	if err != nil {
		t.Fatal(err)
	}
	cert := config.DefaultConfig().Certification
	cert.TotalSamplesMin, cert.NegativeSamplesMin = 1, 1
	out, err := db.RecordHumanDecision(ctx, RecordHumanDecisionCmd{Action: DecisionReject, CommandEventID: "event-1", InterruptID: interrupt.ID, NowMS: testNow + 1, Certification: cert})
	if err != nil {
		t.Fatal(err)
	}
	if out.CalibrationID != recorded.CalibrationID || out.CertificationVersion == "" {
		t.Fatalf("result = %#v", out)
	}
	projection, err := db.Certification(ctx, "bug")
	if err != nil {
		t.Fatal(err)
	}
	if projection.TotalSamples != 1 || projection.NegativeSamples != 1 || projection.LeakCount != 0 {
		t.Fatalf("projection = %#v", projection)
	}
	// The event identity is idempotent rather than appending another decision.
	again, err := db.RecordHumanDecision(ctx, RecordHumanDecisionCmd{Action: DecisionReject, CommandEventID: "event-1", InterruptID: interrupt.ID, NowMS: testNow + 2, Certification: cert})
	if err != nil || again.LedgerEntryID != out.LedgerEntryID {
		t.Fatalf("replay = %#v, %v", again, err)
	}
	assertCount(t, db, "ledger_entries", 2) // gate sample + human decision
}

func TestExternalMergeFactBindsExactGateAndSettlesIdempotently(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE runs SET kind='bug' WHERE id='run'`); err != nil {
		t.Fatal(err)
	}
	recorded, err := db.RecordGateEvaluation(ctx, gateRecord(testNow))
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"change_id":"42","head_sha":"` + strings.Repeat("b", 40) + `","state":"merged"}`)
	fact, err := db.AppendExternalMergeFact(ctx, EventCmd{RunID: "run", ProjectID: "project", Type: "forge_change_merged", Source: SourceForge, PayloadJSON: payload, IdempotencyKey: "fact", OccurredAtMS: testNow + 1, RecordedAtMS: testNow + 1}, strings.Repeat("b", 40), recorded.EvaluationID, recorded.CalibrationID)
	if err != nil {
		t.Fatal(err)
	}
	cert := config.DefaultConfig().Certification
	cert.TotalSamplesMin, cert.NegativeSamplesMin = 1, 1
	out, err := db.RecordHumanDecision(ctx, RecordHumanDecisionCmd{Action: DecisionManualMerge, ForgeFactEventID: fact, NowMS: testNow + 2, Certification: cert})
	if err != nil || out.CalibrationID != recorded.CalibrationID || out.CertificationVersion == "" {
		t.Fatalf("settlement = %#v, %v", out, err)
	}
	again, err := db.RecordHumanDecision(ctx, RecordHumanDecisionCmd{Action: DecisionManualMerge, ForgeFactEventID: fact, NowMS: testNow + 3, Certification: cert})
	if err != nil || again.LedgerEntryID != out.LedgerEntryID {
		t.Fatalf("replay = %#v, %v", again, err)
	}
}

func TestAppendExternalMergeFactRejectsPartialOrMismatchedBinding(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	recorded, err := db.RecordGateEvaluation(ctx, gateRecord(testNow))
	if err != nil {
		t.Fatal(err)
	}
	cmd := EventCmd{RunID: "run", ProjectID: "project", Type: "forge_change_merged", Source: SourceForge, PayloadJSON: []byte(`{}`), IdempotencyKey: "partial", OccurredAtMS: testNow + 1, RecordedAtMS: testNow + 1}
	if _, err := db.AppendExternalMergeFact(ctx, cmd, strings.Repeat("b", 40), "", recorded.CalibrationID); err == nil {
		t.Fatal("partial external binding accepted")
	}
	cmd.IdempotencyKey = "mismatch"
	if _, err := db.AppendExternalMergeFact(ctx, cmd, strings.Repeat("b", 40), "wrong-evaluation", recorded.CalibrationID); err == nil {
		t.Fatal("mismatched external binding accepted")
	}
}

func TestRecordHumanDecisionRejectsUnboundAndInconclusiveSettlement(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordHumanDecision(ctx, RecordHumanDecisionCmd{Action: DecisionReject, CommandEventID: "event", InterruptID: "missing", NowMS: testNow, Certification: config.DefaultConfig().Certification}); err == nil {
		t.Fatal("unbound decision was accepted")
	}
}
