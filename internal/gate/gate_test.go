package gate

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xsift/sift/internal/brain"
	"github.com/xsift/sift/internal/config"
	"github.com/xsift/sift/internal/policy"
	"github.com/xsift/sift/internal/storage"
)

func input(t *testing.T) Input {
	t.Helper()
	d := config.GateDefaults{ReviewPolicy: config.ReviewPolicyNever, RiskyReviewThreshold: 1, AutoMerge: true, ChecksPendingTimeout: time.Hour, FlakyRetryLimit: 1}
	cert := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	p, h, _, _, err := policy.Assemble(policy.Missing(), d, "feature", policy.CertificationProjection{TaskKind: "feature", CertificationVersion: cert, Certified: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := canonical(p)
	if digest(b) != h {
		t.Fatalf("policy hash mismatch got %s want %s bytes %s", digest(b), h, b)
	}
	return Input{SchemaVersion: 1, Identity: Identity{RunID: "r", ProjectID: "p", TaskKind: "feature", ChangeID: "42"}, Change: Change{State: "open", HeadSHA: "0123456789012345678901234567890123456789", BaseRef: "main", HeadRef: "sift/r", Mergeability: "mergeable", ReviewState: "approved", PathsComplete: true, ChangedPaths: []string{"cmd/a.go"}, FilesChanged: 1}, Checks: Checks{Conclusion: "success", ExternalURL: "https://ci.example/run"}, EffectivePolicy: p, EffectivePolicyHash: h, CertificationRulesVersion: cert, CertificationVersion: cert, Risk: Risk{RiskScore: 1}}
}
func TestEvaluateOrderingAndShadow(t *testing.T) {
	in := input(t)
	in.Change.State = "closed"
	in.Change.ChangedPaths = []string{".sift/policy.yaml"}
	in.Change.FilesChanged = 1
	v, e := Evaluate(in)
	if e != nil || v.Kind != "failed" || v.Code != "change_not_open" {
		t.Fatalf("closed must win: %#v %v", v, e)
	}
	for _, changedPath := range []string{".sift/policy.yaml", ".github/workflows/ci.yml", ".gitlab-ci.yml"} {
		in = input(t)
		in.Change.ChangedPaths = []string{changedPath}
		v, e = Evaluate(in)
		if e != nil || v.Code != "hard_guardrail" || ShadowDecision(v) != "block" {
			t.Fatalf("hard path %q: %#v %v", changedPath, v, e)
		}
	}
	in = input(t)
	v, e = Evaluate(in)
	if e != nil || v.Code != "merge" || v.ExpectedHeadSHA != in.Change.HeadSHA || ShadowDecision(v) != "allow" {
		t.Fatalf("merge: %#v %v", v, e)
	}
}
func TestInputHashCoversMutableGateInputsAndPathsIncompleteDoesNotHash(t *testing.T) {
	base := input(t)
	_, hash, err := CanonicalInput(base)
	if err != nil {
		t.Fatal(err)
	}
	vectors := []struct {
		name   string
		mutate func(*Input)
	}{
		{"checks", func(in *Input) { in.Checks.ExternalURL = "https://ci.example/other" }},
		{"review", func(in *Input) { in.Change.ReviewState = "not_approved" }},
		{"mergeability", func(in *Input) { in.Change.Mergeability = "unknown" }},
		{"risk value", func(in *Input) { in.Risk.RiskScore = 2 }},
		{"risk source", func(in *Input) {
			in.Risk.Source = Source{Kind: "fallback", Version: "T3/fallback/v1", Reason: "provider_disabled"}
		}},
		{"certification revision", func(in *Input) {
			in.CertificationVersion = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		}},
	}
	for _, tc := range vectors {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mutate(&in)
			if _, got, err := CanonicalInput(in); err != nil || got == hash {
				t.Fatalf("mutation hash=%q base=%q err=%v", got, hash, err)
			}
		})
	}
	base.Change.PathsComplete = false
	if _, _, err := CanonicalInput(base); err == nil {
		t.Fatal("incomplete paths accepted")
	}
}
func TestCacheKeysFullInputAndRecordsHits(t *testing.T) {
	var c Cache
	in := input(t)
	_, hit, _, err := c.EvaluateCached(in)
	if err != nil || hit {
		t.Fatalf("first evaluation hit=%v err=%v", hit, err)
	}
	_, hit, _, err = c.EvaluateCached(in)
	if err != nil || !hit {
		t.Fatalf("same input hit=%v err=%v", hit, err)
	}
	in.Change.Mergeability = "unknown"
	v, hit, _, err := c.EvaluateCached(in)
	if err != nil || hit || v.Code != "mergeability_unknown" {
		t.Fatalf("changed input hit=%v verdict=%#v err=%v", hit, v, err)
	}
}

func TestEvaluateRecordAndEmitInterruptCommitsGateShadowAndHITLTogether(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: time.UnixMilli(1_700_000_000_000)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const now = int64(1_700_000_000_000)
	if err := db.SeedProjectForTest(ctx, "cfg", "p", now); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "r", "p", "cfg", "42", now); err != nil {
		t.Fatal(err)
	}
	in := input(t)
	in.Risk.Source = Source{Kind: "fallback", Version: "T3/fallback/v1", Reason: "provider_disabled"}
	in.EffectivePolicy.ReviewPolicy = config.ReviewPolicyAlways
	in.Change.ReviewState = "not_approved"
	policyJSON, err := canonical(in.EffectivePolicy)
	if err != nil {
		t.Fatal(err)
	}
	in.EffectivePolicyHash = digest(policyJSON)
	t4 := brain.NewShell(db, config.Brain{DailyTokenLimit: 100, MaxInputBytes: 1 << 20}, nil, func() time.Time { return time.UnixMilli(now) })
	db.SetInterruptT4(t4.CallT4)
	if err := db.SetRunChangeHeadForTest(ctx, "r", in.Identity.ChangeID, in.Change.HeadSHA); err != nil {
		t.Fatal(err)
	}
	cmd := storage.EmitInterruptCmd{RunID: "r", ExpectedRunVersion: 1, Reason: storage.InterruptCodeReview, Facts: map[string]string{"change_ref": "https://forge.example/change/42", "head_sha": in.Change.HeadSHA, "review_requirement": "required", "recommended_action": "approve", "diff_ref": "https://forge.example/change/42/diff"}, Generation: storage.InterruptGeneration{ChangeID: "42", HeadSHA: in.Change.HeadSHA}, GatePhase: storage.GateReview, GuardrailLevel: storage.GuardrailNone, AttentionDailyQuota: map[storage.InterruptSeverity]int{storage.SeverityLow: 3, storage.SeverityNormal: 5, storage.SeverityHigh: 5}, DayTimezone: "UTC", Source: storage.SourceSystem, NowMS: now}
	v, record, interrupt, err := EvaluateRecordAndEmitInterrupt(ctx, db, in, false, []byte(`{"schema_version":1}`), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if v.Code != "code_review" || record.CalibrationID == "" || interrupt.ID == "" || interrupt.Brief != "事实：change_ref=https://forge.example/change/42；head_sha=0123456789012345678901234567890123456789；review_requirement=required；recommended_action=approve；diff_ref=https://forge.example/change/42/diff。建议：approve" {
		t.Fatalf("verdict=%#v record=%#v interrupt=%#v", v, record, interrupt)
	}
	var trace bytes.Buffer
	if err := db.ExportBrainCallsJSONL(ctx, &trace); err != nil || !strings.Contains(trace.String(), `"touchpoint":"T4"`) || !strings.Contains(trace.String(), `"status":"fallback"`) || !strings.Contains(trace.String(), `"fallback_reason":"provider_disabled"`) {
		t.Fatalf("T4 fallback trace=%s err=%v", trace.String(), err)
	}
}

func TestGateT4NormalAndInvalidFallbackPreserveEmissionIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, wantBrief string
		out             storage.InterruptT4Output
	}{
		{"normal", "结论：required；要点：required；建议：批准审阅（approve）", storage.InterruptT4Output{Headline: "变更等待代码审阅", Conclusion: "required", KeyPoints: []string{"required"}, Options: []string{"approve", "reject", "hold"}, RecommendedOptionID: "approve"}},
		{"option reorder fallback", "事实：change_ref=https://forge.example/change/42；head_sha=0123456789012345678901234567890123456789；review_requirement=required；recommended_action=approve；diff_ref=https://forge.example/change/42/diff。建议：approve", storage.InterruptT4Output{Headline: "变更等待代码审阅", Conclusion: "required", KeyPoints: []string{"required"}, Options: []string{"reject", "approve", "hold"}, RecommendedOptionID: "approve"}},
		{"unknown fragment fallback", "事实：change_ref=https://forge.example/change/42；head_sha=0123456789012345678901234567890123456789；review_requirement=required；recommended_action=approve；diff_ref=https://forge.example/change/42/diff。建议：approve", storage.InterruptT4Output{Headline: "变更等待代码审阅", Conclusion: "unfrozen", KeyPoints: []string{"required"}, Options: []string{"approve", "reject", "hold"}, RecommendedOptionID: "approve"}},
		{"unknown option fallback", "事实：change_ref=https://forge.example/change/42；head_sha=0123456789012345678901234567890123456789；review_requirement=required；recommended_action=approve；diff_ref=https://forge.example/change/42/diff。建议：approve", storage.InterruptT4Output{Headline: "变更等待代码审阅", Conclusion: "required", KeyPoints: []string{"required"}, Options: []string{"approve", "reject", "hold"}, RecommendedOptionID: "retry"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			const now = int64(1_700_000_000_000)
			db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: time.UnixMilli(now)})
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.SeedProjectForTest(ctx, "cfg", "p", now); err != nil {
				t.Fatal(err)
			}
			if err := db.SeedForgeRunForTest(ctx, "r", "p", "cfg", "42", now); err != nil {
				t.Fatal(err)
			}
			db.SetInterruptT4(func(context.Context, storage.InterruptT4Input) (storage.InterruptT4Output, error) { return tc.out, nil })
			in := input(t)
			in.Risk.Source = Source{Kind: "fallback", Version: "T3/fallback/v1", Reason: "provider_disabled"}
			in.EffectivePolicy.ReviewPolicy = config.ReviewPolicyAlways
			in.Change.ReviewState = "not_approved"
			policyJSON, err := canonical(in.EffectivePolicy)
			if err != nil {
				t.Fatal(err)
			}
			in.EffectivePolicyHash = digest(policyJSON)
			if err := db.SetRunChangeHeadForTest(ctx, "r", in.Identity.ChangeID, in.Change.HeadSHA); err != nil {
				t.Fatal(err)
			}
			cmd := storage.EmitInterruptCmd{RunID: "r", ExpectedRunVersion: 1, Reason: storage.InterruptCodeReview, Facts: map[string]string{"change_ref": "https://forge.example/change/42", "head_sha": in.Change.HeadSHA, "review_requirement": "required", "recommended_action": "approve", "diff_ref": "https://forge.example/change/42/diff"}, Generation: storage.InterruptGeneration{ChangeID: "42", HeadSHA: in.Change.HeadSHA}, GatePhase: storage.GateReview, GuardrailLevel: storage.GuardrailNone, AttentionDailyQuota: map[storage.InterruptSeverity]int{storage.SeverityLow: 3, storage.SeverityNormal: 5, storage.SeverityHigh: 5}, DayTimezone: "UTC", Source: storage.SourceSystem, NowMS: now}
			_, record, got, err := EvaluateRecordAndEmitInterrupt(ctx, db, in, false, []byte(`{"schema_version":1}`), cmd)
			if err != nil {
				t.Fatal(err)
			}
			if got.Brief != tc.wantBrief || got.Severity != storage.SeverityNormal || got.GenerationKey == "" || record.CalibrationID == "" {
				t.Fatalf("record=%#v interrupt=%#v", record, got)
			}
		})
	}
}

func TestSoftExemptionIsBoundToPaths(t *testing.T) {
	in := input(t)
	in.EffectivePolicy.ProtectedPaths.Soft = []string{"docs/**"} // policy changes require frozen hash refresh
	b, _ := canonical(in.EffectivePolicy)
	in.EffectivePolicyHash = digest(b)
	in.Change.ChangedPaths = []string{"docs/a.md"}
	d := pathsDigest(in.Change.ChangedPaths)
	in.OneTimeExemptions = []Exemption{{RunID: "r", HeadSHA: in.Change.HeadSHA, RuleID: "soft:docs/**", MatchedPathsDigest: d}}
	v, e := Evaluate(in)
	if e != nil || v.Code != "merge" {
		t.Fatalf("valid exemption: %#v %v", v, e)
	}
	in.Change.ChangedPaths = []string{"docs/b.md"}
	v, e = Evaluate(in)
	if e != nil || v.Code != "guardrail_violation" {
		t.Fatalf("exemption reused for changed paths: %#v %v", v, e)
	}
}
