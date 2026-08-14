package gate

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xsift/sift/internal/brain"
	"github.com/xsift/sift/internal/config"
	"github.com/xsift/sift/internal/storage"
)

// A7 firewall at the Gate layer (brain.md §13.3 / §15.4, WBS §5.1 T7).
// Gate.Evaluate is a pure function of its frozen Input, and the Input struct
// carries no T7 trace, proposal draft, Ledger semantic material or
// calibration proposal. These tests prove that seeding real T7 output (a
// pending proposal draft) and historical Ledger data into the same store
// cannot relax a single Gate verdict, change its frozen digest, or suppress
// the single HITL Interrupt for that verdict.

const a7GateAggregateKey = "aggregate:v1:global:all:1:2"

// hitlInput returns a frozen Gate Input whose verdict is the code_review HITL
// (ReviewPolicyAlways + not approved), the exact verdict A7 must protect.
func hitlInput(t *testing.T) Input {
	t.Helper()
	in := input(t)
	in.Risk.Source = Source{Kind: "fallback", Version: "T3/fallback/v1", Reason: "provider_disabled"}
	in.EffectivePolicy.ReviewPolicy = config.ReviewPolicyAlways
	in.Change.ReviewState = "not_approved"
	policyJSON, err := canonical(in.EffectivePolicy)
	if err != nil {
		t.Fatal(err)
	}
	in.EffectivePolicyHash = digest(policyJSON)
	return in
}

// seedPendingT7Draft drives the real Brain shell to a terminal valid T7 call
// and persists the inert pending_human_approval draft in the same store, so
// the Gate verdict path runs with a concrete proposal present.
func seedPendingT7Draft(t *testing.T, ctx context.Context, db *storage.DB, now int64) {
	t.Helper()
	d := strings.Repeat("a", 64)
	t7Input, err := brain.BuildT7Input(brain.T7Input{
		AggregateKey: a7GateAggregateKey, Window: brain.T7Window{StartMS: 1, EndMS: 2},
		Categories: []brain.T7CategoryEvidence{{EvidenceID: "cat", TaskKind: brain.TaskBug, CertificationVersion: d,
			EvidenceSummary: brain.T7EvidenceSummary{WindowStartMS: 1, WindowEndMS: 2, CertificationRulesVersion: d, EvidenceDigest: d}}},
		ReplaySummary:    brain.T7ReplaySummary{EvidenceID: "replay", DatasetVersion: "v1", GateVersion: "gate/v1"},
		SemanticMaterial: []brain.T7SemanticMaterial{},
		AllCategoryKinds: []brain.TaskKind{brain.TaskBug},
	})
	if err != nil {
		t.Fatalf("BuildT7Input: %v", err)
	}
	provider := &fakeT7Provider{result: `{"proposal_kind":"policy","target_scope":"global","title":"Loosen review","body":"draft only","evidence_entry_ids":["cat"],"requires_human_approval":true}`}
	shell := brain.NewShell(db, config.Brain{Executable: "fake-cli", Args: []string{"-p"}, Protocol: config.BrainProtocolClaudeJSONv1, DailyTokenLimit: 100, CallTimeout: time.Minute, SchemaRetries: 1, MaxInputBytes: 1 << 20, MaxRawOutputBytes: 1 << 20}, provider, func() time.Time { return time.UnixMilli(now) })
	result, err := shell.Call(ctx, brain.T7Contract(a7GateAggregateKey, "", []brain.TaskKind{brain.TaskBug}, []string{"cat", "replay"}), brain.CallParams{Scope: storage.BrainScopeAggregate, SubjectKey: a7GateAggregateKey, Input: t7Input})
	if err != nil || result.Status != storage.BrainCallValid {
		t.Fatalf("T7 shell call = %#v err=%v", result, err)
	}
	if _, _, err := brain.PersistT7ProposalDraft(ctx, db, result, a7GateAggregateKey, []string{"cat", "replay"}, now); err != nil {
		t.Fatalf("PersistT7ProposalDraft: %v", err)
	}
}

// fakeT7Provider emits one scripted T7 inner result for the Brain shell.
type fakeT7Provider struct {
	result string
}

func (f *fakeT7Provider) Call(ctx context.Context, req brain.ExecRequest) brain.ExecResult {
	raw := brain.FakeEnvelope(f.result, 10, 6)
	zero := 0
	return brain.ExecResult{Stdout: raw, ExitCode: &zero}
}

// TestA7GateVerdictAndDigestInvariantUnderT7Proposal proves the pure verdict
// boundary: Evaluate and CanonicalInput are deterministic functions of the
// frozen Input, so a pending T7 draft in the store changes neither the
// verdict code nor the frozen input hash. The verdict stays the code_review
// HITL — it is not relaxed toward ready by calibration data.
func TestA7GateVerdictAndDigestInvariantUnderT7Proposal(t *testing.T) {
	in := hitlInput(t)

	verdictBefore, canonicalBefore, digestBefore := frozen(t, in)
	if verdictBefore.Code != "code_review" || verdictBefore.Kind != "hitl" {
		t.Fatalf("baseline verdict = %#v, want code_review HITL", verdictBefore)
	}

	// Seed a pending T7 policy proposal into a fresh store. The Gate verdict
	// path never reads it.
	ctx := context.Background()
	const now = int64(1_700_000_000_000)
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: time.UnixMilli(now)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedPendingT7Draft(t, ctx, db, now)

	verdictAfter, canonicalAfter, digestAfter := frozen(t, in)
	if verdictAfter.Code != "code_review" || verdictAfter.Kind != "hitl" {
		t.Fatalf("verdict relaxed under T7 proposal = %#v", verdictAfter)
	}
	if digestAfter != digestBefore {
		t.Fatalf("frozen Gate digest changed under T7 proposal: before=%s after=%s", digestBefore, digestAfter)
	}
	if string(canonicalAfter) != string(canonicalBefore) {
		t.Fatalf("frozen Gate canonical input changed under T7 proposal")
	}
}

// TestA7HITLNotSuppressedByT7Proposal proves the emission boundary: with a
// pending T7 proposal draft present, EvaluateRecordAndEmitInterrupt still
// emits the single code_review HITL at full severity — it is neither dropped
// nor demoted to a non-HITL verdict.
func TestA7HITLNotSuppressedByT7Proposal(t *testing.T) {
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

	in := hitlInput(t)
	if err := db.SetRunChangeHeadForTest(ctx, "r", in.Identity.ChangeID, in.Change.HeadSHA); err != nil {
		t.Fatal(err)
	}

	// Seed the proposal before the Gate runs, so the HITL path executes with a
	// concrete pending draft and its Brain trace in the same store.
	seedPendingT7Draft(t, ctx, db, now)

	cmd := storage.EmitInterruptCmd{RunID: "r", ExpectedRunVersion: 1, Reason: storage.InterruptCodeReview,
		Facts:      map[string]string{"change_ref": "https://forge.example/change/42", "head_sha": in.Change.HeadSHA, "review_requirement": "required", "recommended_action": "approve", "diff_ref": "https://forge.example/change/42/diff"},
		Generation: storage.InterruptGeneration{ChangeID: in.Identity.ChangeID, HeadSHA: in.Change.HeadSHA}, GatePhase: storage.GateReview, GuardrailLevel: storage.GuardrailNone,
		AttentionDailyQuota: map[storage.InterruptSeverity]int{storage.SeverityLow: 3, storage.SeverityNormal: 5, storage.SeverityHigh: 5}, DayTimezone: "UTC", Source: storage.SourceSystem, NowMS: now}
	verdict, record, interrupt, err := EvaluateRecordAndEmitInterrupt(ctx, db, in, false, []byte(`{"schema_version":1}`), cmd)
	if err != nil {
		t.Fatalf("EvaluateRecordAndEmitInterrupt: %v", err)
	}
	if verdict.Code != "code_review" || verdict.Kind != "hitl" {
		t.Fatalf("verdict relaxed/suppressed by T7 proposal = %#v", verdict)
	}
	if interrupt.ID == "" || interrupt.Reason != storage.InterruptCodeReview || interrupt.Severity != storage.SeverityNormal || record.CalibrationID == "" {
		t.Fatalf("HITL interrupt not emitted at full severity: verdict=%#v interrupt=%#v record=%#v", verdict, interrupt, record)
	}
}

// frozen evaluates the Input and returns its verdict and canonical digest.
func frozen(t *testing.T, in Input) (Verdict, []byte, string) {
	t.Helper()
	v, err := Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	b, d, err := CanonicalInput(in)
	if err != nil {
		t.Fatalf("CanonicalInput: %v", err)
	}
	return v, b, d
}
