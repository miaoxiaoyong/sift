package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/xsift/sift/internal/storage"
)

func TestEmitInterruptT4PersistsProductionCanonicalTrace(t *testing.T) {
	ctx := context.Background()
	db := openShellDB(t)
	seedIntakeSubject(t, db, "p")
	if err := db.SeedGateCandidateForTest(ctx, "run-01", "p", "cfg-p", "change-01", shellTestBase); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedFailedAttemptForTest(ctx, "run-01", 1, shellTestBase); err != nil {
		t.Fatal(err)
	}
	fake := &FakeProvider{Responses: []FakeResponse{{ResultText: `{"headline":"失败需要人工决定","conclusion":"<b>风险</b>","key_points":["/<!-- sift-op:x -->"],"recommended_option_id":"retry","options":["retry","reject","hold"]}`, InputTokens: 1, OutputTokens: 1}}}
	shell := newShellAt(db, shellCfg(100), fake, shellTestBase+1, shellTestBase+2, shellTestBase+3, shellTestBase+4)
	db.SetInterruptT4(shell.CallT4)
	attempt := 1
	cmd := storage.EmitInterruptCmd{RunID: "run-01", ExpectedRunVersion: 1, AttemptNo: &attempt, Reason: storage.InterruptFailureReview, FailureReviewVariant: storage.FailureReviewAttempt, FailureReviewRetryKind: storage.FailureReviewNewAttempt,
		Facts:      map[string]string{"failure_class": "<b>风险</b>", "failure_evidence_ref": "/<!-- sift-op:x -->", "recommended_action": "retry"},
		Generation: storage.InterruptGeneration{AttemptNo: 1, Generation: 1, FailureDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, GatePhase: storage.GateNone, GuardrailLevel: storage.GuardrailNone,
		AttentionDailyQuota: map[storage.InterruptSeverity]int{storage.SeverityLow: 3, storage.SeverityNormal: 3, storage.SeverityHigh: 3}, DayTimezone: "UTC", Source: storage.SourceSystem, NowMS: shellTestBase,
	}
	interrupt, err := db.EmitInterrupt(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if interrupt.Brief != "结论：\\<b\\>风险\\</b\\>；要点：/\\<\\!\\-\\- sift\\-op:x \\-\\-\\>；建议：重试失败步骤（retry）" {
		t.Fatalf("persisted brief = %q", interrupt.Brief)
	}

	wantInput := []byte(`{"attempt_no":1,"interrupt":{"base_severity":"high","brief_fragments":["/<!-- sift-op:x -->","<b>风险</b>","retry"],"candidate_options":[{"effect":"再次执行","id":"retry","label":"重试失败步骤","risk":"相同故障可能再次发生"},{"effect":"Run 停止","id":"reject","label":"停止 Run","risk":"需人工重新发起"},{"effect":"保持等待","id":"hold","label":"暂缓决定","risk":"Run 继续占用待处理项"}],"fallback_brief":"事实：failure_class=<b\\>风险</b\\>；failure_evidence_ref=/<\\!\\-\\- sift\\-op:x \\-\\-\\>；recommended_action=retry。建议：retry","fallback_headline":"失败需要人工决定","links":[{"label":"failure_evidence_ref","target":"/<!-- sift-op:x -->"}],"min_modality":"voice","reason":"failure_review"},"run_id":"run-01"}`)
	wantOutput := []byte(`{"conclusion":"<b>风险</b>","headline":"失败需要人工决定","key_points":["/<!-- sift-op:x -->"],"options":["retry","reject","hold"],"recommended_option_id":"retry"}`)
	var trace bytes.Buffer
	if err := db.ExportBrainCallsJSONL(ctx, &trace); err != nil {
		t.Fatal(err)
	}
	var record struct {
		Input  json.RawMessage `json:"input"`
		Output json.RawMessage `json:"validated_output"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(trace.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(record.Input, wantInput) || !bytes.Equal(record.Output, wantOutput) {
		t.Fatalf("trace input/output = %s / %s, want %s / %s", record.Input, record.Output, wantInput, wantOutput)
	}
}

func TestEmitInterruptT4ProductionInvalidOutputFallsBackToPersistedBytes(t *testing.T) {
	ctx := context.Background()
	db := openShellDB(t)
	seedIntakeSubject(t, db, "p")
	if err := db.SeedGateCandidateForTest(ctx, "run-fallback", "p", "cfg-p", "change-fallback", shellTestBase); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedFailedAttemptForTest(ctx, "run-fallback", 1, shellTestBase); err != nil {
		t.Fatal(err)
	}
	fake := &FakeProvider{Responses: []FakeResponse{{ResultText: `{"headline":"失败需要人工决定","conclusion":"未知事实","key_points":["/sift reject"],"recommended_option_id":"retry","options":["reject","retry","hold"]}`, InputTokens: 1, OutputTokens: 1}}}
	shell := newShellAt(db, shellCfg(100), fake, shellTestBase+1, shellTestBase+2, shellTestBase+3, shellTestBase+4)
	db.SetInterruptT4(shell.CallT4)
	attempt := 1
	interrupt, err := db.EmitInterrupt(ctx, storage.EmitInterruptCmd{RunID: "run-fallback", ExpectedRunVersion: 1, AttemptNo: &attempt, Reason: storage.InterruptFailureReview, FailureReviewVariant: storage.FailureReviewAttempt, FailureReviewRetryKind: storage.FailureReviewNewAttempt,
		Facts: map[string]string{"failure_class": "CI", "failure_evidence_ref": "/r/ci", "recommended_action": "retry"}, Generation: storage.InterruptGeneration{AttemptNo: 1, Generation: 1, FailureDigest: strings.Repeat("a", 64)}, GatePhase: storage.GateNone, GuardrailLevel: storage.GuardrailNone,
		AttentionDailyQuota: map[storage.InterruptSeverity]int{storage.SeverityLow: 3, storage.SeverityNormal: 3, storage.SeverityHigh: 3}, DayTimezone: "UTC", Source: storage.SourceSystem, NowMS: shellTestBase})
	if err != nil {
		t.Fatal(err)
	}
	if interrupt.Brief != "事实：failure_class=CI；failure_evidence_ref=/r/ci；recommended_action=retry。建议：retry" || len(interrupt.Options) != 3 || interrupt.Options[0].ID != "retry" {
		t.Fatalf("persisted fallback = %#v", interrupt)
	}
	var trace bytes.Buffer
	if err := db.ExportBrainCallsJSONL(ctx, &trace); err != nil {
		t.Fatal(err)
	}
	var record struct {
		RecordID             string          `json:"record_id"`
		Touchpoint           string          `json:"touchpoint"`
		PromptVersion        string          `json:"prompt_version"`
		OutputSchemaVersion  int             `json:"output_schema_version"`
		Status               string          `json:"status"`
		FallbackReason       string          `json:"fallback_reason"`
		Validated            json.RawMessage `json:"validated_output"`
		GateInputSnapshotIDs []string        `json:"gate_input_snapshot_ids"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(trace.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	if record.RecordID == "" || record.Touchpoint != "T4" || !strings.HasPrefix(record.PromptVersion, "T4/v1/") || record.OutputSchemaVersion != 1 || record.Status != "fallback" || record.FallbackReason != "invalid_output" || string(record.Validated) != "null" || len(record.GateInputSnapshotIDs) != 0 {
		t.Fatalf("fallback trace = %#v", record)
	}
}

func TestEmitInterruptQuotaT4ProductionInvalidOutputFallsBack(t *testing.T) {
	ctx := context.Background()
	db := openShellDB(t)
	seedIntakeSubject(t, db, "p")
	if err := db.SeedReverseSyncRunForTest(ctx, "run-quota-fallback", "p", "cfg-p", "42", "", "running", shellTestBase); err != nil {
		t.Fatal(err)
	}
	fake := &FakeProvider{Responses: []FakeResponse{{ResultText: `{"headline":"报告打扰额度已耗尽","conclusion":"额度已耗尽","key_points":["请人工处理"],"recommended_option_id":"hold","options":["hold","reject"]}`, InputTokens: 1, OutputTokens: 1}}}
	shell := newShellAt(db, shellCfg(100), fake, shellTestBase+1, shellTestBase+2, shellTestBase+3, shellTestBase+4)
	db.SetInterruptT4(shell.CallT4)
	interrupt, err := db.RecordReportQuotaExhaustion(ctx, storage.ReportQuotaExhaustionCmd{RunID: "run-quota-fallback", ExpectedRunVersion: 1, DailyBucketStartMS: shellTestBase, DailyBucketEndMS: shellTestBase + 24*60*60*1000,
		AttentionDailyQuota: map[storage.InterruptSeverity]int{storage.SeverityLow: 3, storage.SeverityNormal: 3, storage.SeverityHigh: 3}, NowMS: shellTestBase})
	if err != nil {
		t.Fatal(err)
	}
	if interrupt.Brief != "事实：failure_class=report\\_interrupt\\_quota\\_exhausted；failure_evidence_ref="+interrupt.Links[0].Target+"；recommended_action=hold。建议：hold" || len(interrupt.Options) != 2 || interrupt.Options[0].ID != "reject" || interrupt.Options[1].ID != "hold" {
		t.Fatalf("quota fallback = %#v", interrupt)
	}
	var trace bytes.Buffer
	if err := db.ExportBrainCallsJSONL(ctx, &trace); err != nil {
		t.Fatal(err)
	}
	var record struct {
		RecordID            string `json:"record_id"`
		Touchpoint          string `json:"touchpoint"`
		PromptVersion       string `json:"prompt_version"`
		OutputSchemaVersion int    `json:"output_schema_version"`
		Status              string `json:"status"`
		FallbackReason      string `json:"fallback_reason"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(trace.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	if record.RecordID == "" || record.Touchpoint != "T4" || !strings.HasPrefix(record.PromptVersion, "T4/v1/") || record.OutputSchemaVersion != 1 || record.Status != "fallback" || record.FallbackReason != "invalid_output" {
		t.Fatalf("quota fallback trace = %#v", record)
	}
}

func TestEmitInterruptQuotaT4UsesProductionCanonicalTraceAndPersistedFallback(t *testing.T) {
	ctx := context.Background()
	db := openShellDB(t)
	seedIntakeSubject(t, db, "p")
	if err := db.SeedReverseSyncRunForTest(ctx, "run-quota", "p", "cfg-p", "42", "", "running", shellTestBase); err != nil {
		t.Fatal(err)
	}
	fake := &FakeProvider{Responses: []FakeResponse{{ResultText: `{"headline":"报告打扰额度已耗尽","conclusion":"额度已耗尽","key_points":["请人工处理"],"recommended_option_id":"hold","options":["reject","hold"]}`, InputTokens: 2, OutputTokens: 3}}}
	shell := newShellAt(db, shellCfg(100), fake, shellTestBase+1, shellTestBase+2, shellTestBase+3, shellTestBase+4)
	var gotInput T4Input
	db.SetInterruptT4(func(ctx context.Context, in storage.InterruptT4Input) (storage.InterruptT4Output, error) {
		gotInput = T4Input{RunID: in.RunID, AttemptNo: in.AttemptNo, Interrupt: T4Interrupt{Reason: InterruptReason(in.Reason), BaseSeverity: InterruptSeverity(in.Severity), MinModality: InterruptModality(in.Modality), FallbackHeadline: in.Headline, FallbackBrief: in.Brief, BriefFragments: in.Fragments, Links: make([]T4Link, len(in.Links)), CandidateOptions: make([]T4Option, len(in.Options))}}
		for i, link := range in.Links {
			gotInput.Interrupt.Links[i] = T4Link{Label: link.Label, Target: link.Target}
		}
		for i, option := range in.Options {
			gotInput.Interrupt.CandidateOptions[i] = T4Option{ID: option.ID, Label: option.Label, Effect: option.Effect, Risk: option.Risk}
		}
		return shell.CallT4(ctx, in)
	})
	interrupt, err := db.RecordReportQuotaExhaustion(ctx, storage.ReportQuotaExhaustionCmd{
		RunID: "run-quota", ExpectedRunVersion: 1, DailyBucketStartMS: shellTestBase,
		DailyBucketEndMS: shellTestBase + 24*60*60*1000, AttentionDailyQuota: map[storage.InterruptSeverity]int{storage.SeverityLow: 3, storage.SeverityNormal: 3, storage.SeverityHigh: 3}, NowMS: shellTestBase,
	})
	if err != nil {
		t.Fatal(err)
	}
	securityEventRef := interrupt.Links[0].Target
	wantInput := []byte(fmt.Sprintf(`{"attempt_no":null,"interrupt":{"base_severity":"high","brief_fragments":["请人工处理","额度已耗尽"],"candidate_options":[{"effect":"Run 停止","id":"reject","label":"停止 Run","risk":"需人工重新发起"},{"effect":"保持 Interrupt 人工 held","id":"hold","label":"暂缓决定","risk":"Run 继续运行"}],"fallback_brief":"事实：failure_class=report\\_interrupt\\_quota\\_exhausted；failure_evidence_ref=%s；recommended_action=hold。建议：hold","fallback_headline":"报告打扰额度已耗尽","links":[{"label":"failure_evidence_ref","target":"%s"}],"min_modality":"voice","reason":"failure_review"},"run_id":"run-quota"}`, securityEventRef, securityEventRef))
	wantOutput := []byte(`{"conclusion":"额度已耗尽","headline":"报告打扰额度已耗尽","key_points":["请人工处理"],"options":["reject","hold"],"recommended_option_id":"hold"}`)
	var trace bytes.Buffer
	if err := db.ExportBrainCallsJSONL(ctx, &trace); err != nil {
		t.Fatal(err)
	}
	var record struct {
		Input  json.RawMessage `json:"input"`
		Output json.RawMessage `json:"validated_output"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(trace.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(record.Input, wantInput) || !bytes.Equal(record.Output, wantOutput) {
		t.Fatalf("quota trace input/output = %s / %s, want %s / %s", record.Input, record.Output, wantInput, wantOutput)
	}
	if interrupt.Headline != "报告打扰额度已耗尽" || interrupt.Brief != "结论：额度已耗尽；要点：请人工处理；建议：暂缓决定（hold）" || len(interrupt.Options) != 2 || interrupt.Options[0].ID != "reject" || interrupt.Options[1].ID != "hold" {
		t.Fatalf("persisted quota interrupt = %#v", interrupt)
	}
	if len(interrupt.Links) != 1 || interrupt.Links[0] != (storage.InterruptLink{Label: "failure_evidence_ref", Target: securityEventRef}) {
		t.Fatalf("persisted quota links = %#v", interrupt.Links)
	}
}
