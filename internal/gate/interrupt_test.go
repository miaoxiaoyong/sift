package gate

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/xsift/sift/internal/config"
	"github.com/xsift/sift/internal/storage"
)

func TestGateFailureReviewInterruptUsesClosedAttemptBinding(t *testing.T) {
	in := input(t)
	in.Identity.ChangeID = "change-01"
	in.Change.HeadSHA = "0123456789012345678901234567890123456789"
	candidate := storage.GateCandidate{RunID: "run-01", Version: 7, AttemptNo: 3, Generation: 4}
	cmd, err := interruptCommand(candidate, in, Verdict{Code: "gate_contract_failed"}, config.Attention{}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Reason != storage.InterruptFailureReview || cmd.FailureReviewVariant != storage.FailureReviewAttempt || cmd.FailureReviewRetryKind != storage.FailureReviewGateRecheck || cmd.AttemptNo == nil || *cmd.AttemptNo != 3 {
		t.Fatalf("failure-review command = %#v", cmd)
	}
	if cmd.Generation.AttemptNo != 3 || cmd.Generation.Generation != 4 || cmd.Generation.ChangeID != in.Identity.ChangeID || cmd.Generation.HeadSHA != in.Change.HeadSHA || cmd.Generation.FailureDigest == "" {
		t.Fatalf("failure-review identity = %#v", cmd.Generation)
	}
	if cmd.Facts["failure_class"] != "gate_contract_failed" || cmd.Facts["recommended_action"] != "retry" || cmd.Facts["failure_evidence_ref"] == "" {
		t.Fatalf("failure-review facts = %#v", cmd.Facts)
	}
}

func TestMergeabilityUnknownUsesFailureReviewSuccessor(t *testing.T) {
	in := input(t)
	in.Identity.ChangeID = "change-01"
	in.Change.HeadSHA = "0123456789012345678901234567890123456789"
	cmd, err := interruptCommand(storage.GateCandidate{RunID: "run", Version: 1, AttemptNo: 2, Generation: 3}, in, Verdict{Code: "mergeability_unknown"}, config.Attention{}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Reason != storage.InterruptFailureReview || cmd.FailureReviewVariant != storage.FailureReviewAttempt || cmd.FailureReviewRetryKind != storage.FailureReviewGateRecheck {
		t.Fatalf("unknown mergeability command = %#v", cmd)
	}
	if cmd.Generation.ChangeID != in.Identity.ChangeID || cmd.Generation.HeadSHA != in.Change.HeadSHA || cmd.Facts["failure_class"] != "mergeability_unknown" {
		t.Fatalf("unknown mergeability provenance = %#v facts=%v", cmd.Generation, cmd.Facts)
	}
}

func TestGateFailureReviewInterruptPersistsCalibrationAndBinding(t *testing.T) {
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
	if err := db.SeedGateCandidateForTest(ctx, "r", "p", "cfg", "42", now); err != nil {
		t.Fatal(err)
	}
	in := input(t)
	in.Change.Mergeability = "unknown"
	in.Risk.Source = Source{Kind: "fallback", Version: "T3/fallback/v1", Reason: "provider_disabled"}
	if err := db.SetRunChangeHeadForTest(ctx, "r", in.Identity.ChangeID, in.Change.HeadSHA); err != nil {
		t.Fatal(err)
	}
	candidate := storage.GateCandidate{RunID: "r", Version: 1, AttemptNo: 1, Generation: 1}
	attention := config.Attention{DailyQuota: config.DailyQuota{Low: 3, Normal: 3, High: 3}, DayTimezone: "UTC"}
	cmd, err := interruptCommand(candidate, in, Verdict{Code: "mergeability_unknown"}, attention, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	verdict, record, interrupt, err := EvaluateRecordAndEmitInterrupt(ctx, db, in, false, []byte(`{"schema_version":1}`), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Code != "mergeability_unknown" || record.CalibrationID == "" || interrupt.ID == "" || interrupt.Reason != storage.InterruptFailureReview || interrupt.AttemptNo == nil || *interrupt.AttemptNo != 1 {
		t.Fatalf("verdict=%#v record=%#v interrupt=%#v", verdict, record, interrupt)
	}
}

func TestMergeConflictInterruptUsesCanonicalConflictDigest(t *testing.T) {
	in := input(t)
	in.Identity.ChangeID = "change-01"
	in.Change.HeadSHA = "0123456789012345678901234567890123456789"
	cmd, err := interruptCommand(storage.GateCandidate{RunID: "run", Version: 1}, in, Verdict{Code: "merge_conflict"}, config.Attention{}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cmd.Generation.ConflictDigest, storage.MergeConflictDigest(in.Identity.ChangeID, in.Change.HeadSHA); got != want {
		t.Fatalf("conflict digest = %s, want %s", got, want)
	}
}
