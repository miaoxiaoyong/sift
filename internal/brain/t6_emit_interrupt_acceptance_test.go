package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xsift/sift/internal/storage"
)

func TestEmitInterruptT6ProductionSeamPersistsCanonicalTrace(t *testing.T) {
	ctx := context.Background()
	db := openShellDB(t)
	seedIntakeSubject(t, db, "p")
	if err := db.SeedGateCandidateForTest(ctx, "run-t6", "p", "cfg-p", "change-t6", shellTestBase); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedFailedAttemptForTest(ctx, "run-t6", 1, shellTestBase); err != nil {
		t.Fatal(err)
	}
	// Provider returns a valid suggestion: downgrade high->normal and defer to
	// the daily batch. Channel "voice" is the sole compatible candidate.
	fake := &FakeProvider{Responses: []FakeResponse{{ResultText: `{"delivery":"batch","channel_id":"voice","suggested_downgrade":true,"rationale":"defer to daily summary"}`, InputTokens: 1, OutputTokens: 1}}}
	shell := newShellAt(db, shellCfg(100), fake, shellTestBase+1, shellTestBase+2, shellTestBase+3, shellTestBase+4)
	db.SetInterruptT6(shell.CallT6)

	batchAt := shellTestBase + 3600*1000
	attempt := 1
	cmd := storage.EmitInterruptCmd{RunID: "run-t6", ExpectedRunVersion: 1, AttemptNo: &attempt, Reason: storage.InterruptFailureReview, FailureReviewVariant: storage.FailureReviewAttempt, FailureReviewRetryKind: storage.FailureReviewNewAttempt,
		Facts:      map[string]string{"failure_class": "CI", "failure_evidence_ref": "/r/ci", "recommended_action": "retry"},
		Generation: storage.InterruptGeneration{AttemptNo: 1, Generation: 1, FailureDigest: strings.Repeat("a", 64)}, GatePhase: storage.GateNone, GuardrailLevel: storage.GuardrailNone,
		AttentionDailyQuota: map[storage.InterruptSeverity]int{storage.SeverityLow: 3, storage.SeverityNormal: 3, storage.SeverityHigh: 3}, DayTimezone: "UTC", Source: storage.SourceSystem, NowMS: shellTestBase,
		Channels: []storage.InterruptChannel{{ID: "voice", Capabilities: []string{"voice"}, Default: true}}, BatchAtMS: &batchAt,
	}
	interrupt, err := db.EmitInterrupt(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	// Deterministic acceptor applied the one-level downgrade (high->normal) and
	// the batch delivery; no second emit/charge path was taken.
	if interrupt.Severity != storage.SeverityNormal || !interrupt.SuggestedDowngrade || interrupt.ChannelID != "voice" || interrupt.Delivery != "batch" || interrupt.NextDispatchAtMS == nil || *interrupt.NextDispatchAtMS != batchAt {
		t.Fatalf("persisted dispatch = %#v", interrupt)
	}

	var trace bytes.Buffer
	if err := db.ExportBrainCallsJSONL(ctx, &trace); err != nil {
		t.Fatal(err)
	}
	var record struct {
		RecordID            string          `json:"record_id"`
		Touchpoint          string          `json:"touchpoint"`
		PromptVersion       string          `json:"prompt_version"`
		OutputSchemaVersion int             `json:"output_schema_version"`
		Status              string          `json:"status"`
		FallbackReason      *string         `json:"fallback_reason"`
		Input               json.RawMessage `json:"input"`
		Output              json.RawMessage `json:"validated_output"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(trace.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	if record.RecordID == "" || record.Touchpoint != "T6" || !strings.HasPrefix(record.PromptVersion, "T6/v1/") || record.OutputSchemaVersion != 1 || record.Status != "valid" || record.FallbackReason != nil {
		t.Fatalf("T6 trace header = %+v", record)
	}
	var in T6Input
	if err := json.Unmarshal(record.Input, &in); err != nil {
		t.Fatalf("decode T6 input: %v", err)
	}
	if in.RunID != "run-t6" || in.Candidate.Severity != "high" || in.Candidate.MinModality != "voice" || in.Candidate.DefaultChannelID != "voice" || len(in.Candidate.ChannelCandidates) != 1 || in.Candidate.ChannelCandidates[0] != "voice" {
		t.Fatalf("T6 frozen input = %+v", in)
	}
	var out T6Output
	if err := json.Unmarshal(record.Output, &out); err != nil || out.Delivery == nil || *out.Delivery != "batch" || *out.ChannelID != "voice" || !*out.SuggestedDowngrade {
		t.Fatalf("T6 validated output = %s", record.Output)
	}
}

func TestEmitInterruptT6ProductionSeamInvalidFallsBack(t *testing.T) {
	ctx := context.Background()
	db := openShellDB(t)
	seedIntakeSubject(t, db, "p")
	if err := db.SeedGateCandidateForTest(ctx, "run-t6-fb", "p", "cfg-p", "change-t6-fb", shellTestBase); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedFailedAttemptForTest(ctx, "run-t6-fb", 1, shellTestBase); err != nil {
		t.Fatal(err)
	}
	// "pager" is not in the frozen channel candidates, so the closed contract
	// rejects the output and the shell converges to the T6 fallback.
	fake := &FakeProvider{Responses: []FakeResponse{{ResultText: `{"delivery":"batch","channel_id":"pager","suggested_downgrade":true,"rationale":"defer"}`, InputTokens: 1, OutputTokens: 1}}}
	shell := newShellAt(db, shellCfg(100), fake, shellTestBase+1, shellTestBase+2, shellTestBase+3, shellTestBase+4)
	db.SetInterruptT6(shell.CallT6)

	attempt := 1
	cmd := storage.EmitInterruptCmd{RunID: "run-t6-fb", ExpectedRunVersion: 1, AttemptNo: &attempt, Reason: storage.InterruptFailureReview, FailureReviewVariant: storage.FailureReviewAttempt, FailureReviewRetryKind: storage.FailureReviewNewAttempt,
		Facts:      map[string]string{"failure_class": "CI", "failure_evidence_ref": "/r/ci", "recommended_action": "retry"},
		Generation: storage.InterruptGeneration{AttemptNo: 1, Generation: 1, FailureDigest: strings.Repeat("a", 64)}, GatePhase: storage.GateNone, GuardrailLevel: storage.GuardrailNone,
		AttentionDailyQuota: map[storage.InterruptSeverity]int{storage.SeverityLow: 3, storage.SeverityNormal: 3, storage.SeverityHigh: 3}, DayTimezone: "UTC", Source: storage.SourceSystem, NowMS: shellTestBase,
		Channels: []storage.InterruptChannel{{ID: "voice", Capabilities: []string{"voice"}, Default: true}},
	}
	interrupt, err := db.EmitInterrupt(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	// Fallback dispatch: high severity stays high and forces immediate.
	if interrupt.Severity != storage.SeverityHigh || interrupt.SuggestedDowngrade || interrupt.ChannelID != "voice" || interrupt.Delivery != "immediate" || interrupt.NextDispatchAtMS == nil || *interrupt.NextDispatchAtMS != shellTestBase {
		t.Fatalf("fallback dispatch = %#v", interrupt)
	}

	var trace bytes.Buffer
	if err := db.ExportBrainCallsJSONL(ctx, &trace); err != nil {
		t.Fatal(err)
	}
	var record struct {
		Touchpoint     string  `json:"touchpoint"`
		PromptVersion  string  `json:"prompt_version"`
		Status         string  `json:"status"`
		FallbackReason *string `json:"fallback_reason"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(trace.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	if record.Touchpoint != "T6" || !strings.HasPrefix(record.PromptVersion, "T6/v1/") || record.Status != "fallback" || record.FallbackReason == nil {
		t.Fatalf("fallback trace = %+v", record)
	}
}
