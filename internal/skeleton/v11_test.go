package skeleton

import (
	"context"
	"testing"
	"time"

	"github.com/xsift/sift/internal/forge"
	"github.com/xsift/sift/internal/storage"
)

// V11 external-merge fact convergence, first segment (WBS M2 §2.5 / gate; PRD
// §4.1 / §4.5 / §10.2). A Run parked in waiting_human is converged to done when
// its Change is merged externally (a human merged it out from under Sift).
// Because no Sift Gate adjudicated that merge, the Run records gate_bypassed —
// honest accounting, not bypass adjudication. The Gate/audit/Ledger closure
// lands in M4/M5; this is the M2 fact-convergence first segment: the forge fact
// is observable, the state machine accepts the transition, and the flag is set.

const (
	v11Base     = int64(1_701_000_000_000)
	v11Project  = "proj-v11"
	v11Config   = "cfg-v11"
	v11RunID    = "run-v11"
	v11ChangeID = "change-v11"
	v11HeadSHA  = "feedfacecafef00dbaadf00dcafebabe00000000"
	v11Issue    = "77"
	v11Actor    = "trusted-operator"
)

func openV11DB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.Open(context.Background(), storage.OpenConfig{
		Path:          t.TempDir() + "/sift-home/sift.db",
		BinaryVersion: "test-binary",
		Now:           time.UnixMilli(v11Base),
	})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestV11ExternalMergeConvergesWaitingHumanToDoneGateBypassed drives the
// waiting_human → done convergence on an externally merged Change using a fake
// forge, and pins that gate_bypassed is recorded.
func TestV11ExternalMergeConvergesWaitingHumanToDoneGateBypassed(t *testing.T) {
	ctx := context.Background()
	db := openV11DB(t)
	if err := db.SeedProjectForTest(ctx, v11Config, v11Project, v11Base); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, v11RunID, v11Project, v11Config, v11Issue, v11Base); err != nil {
		t.Fatal(err)
	}

	fc := forge.NewFake()
	project := forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo-" + v11Project}
	fc.AddChange(project, v11ChangeID, v11HeadSHA)

	clock := NewClock(v11Base)

	// queued → running → waiting_human: the Run reaches the human-approval gate.
	running, err := db.TransitionRun(ctx, v11RunID, 1, storage.DomainCommand{
		To: storage.RunRunning, Source: storage.SourceSystem, OccurredAtMS: clock.NowMS(),
	})
	if err != nil {
		t.Fatalf("queued→running: %v", err)
	}
	waiting, err := db.TransitionRun(ctx, v11RunID, running.Version, storage.DomainCommand{
		To: storage.RunWaitingHuman, Source: storage.SourceSystem, OccurredAtMS: clock.NowMS(),
	})
	if err != nil {
		t.Fatalf("running→waiting_human: %v", err)
	}
	if waiting.Status != storage.RunWaitingHuman {
		t.Fatalf("status=%s, want waiting_human", waiting.Status)
	}

	// The human merges the Change directly on the forge while Sift waits.
	clock.Advance(time.Minute)
	mergedAt := clock.Now()
	if _, err := fc.InjectMerged(project, v11ChangeID, mergedAt); err != nil {
		t.Fatalf("inject external merge: %v", err)
	}

	// The reconciler observes the forge fact. The Change is merged; Sift did not
	// merge it, so this is an external fact (PRD §4.5), not a Sift merge.
	ch, err := fc.GetChange(ctx, project, v11ChangeID)
	if err != nil {
		t.Fatalf("observe change: %v", err)
	}
	if ch.State != forge.ChangeMerged {
		t.Fatalf("change state=%s, want merged (external fact)", ch.State)
	}

	// Record the merge-observed event (audit spine) and converge done with
	// gate_bypassed: no Gate adjudicated the human's external merge.
	body := []byte(`{"change_id":"` + ch.ID + `","head_sha":"` + ch.HeadSHA + `"}`)
	if _, err := db.AppendEvent(ctx, storage.EventCmd{
		RunID: v11RunID, Type: "change.merged_observed", Source: storage.SourceForge,
		PayloadJSON: body, OccurredAtMS: clock.NowMS(), RecordedAtMS: clock.NowMS(),
	}); err != nil {
		t.Fatalf("record merge-observed: %v", err)
	}
	done, err := db.TransitionRun(ctx, v11RunID, waiting.Version, storage.DomainCommand{
		To: storage.RunDone, Source: storage.SourceForge, Actor: v11Actor,
		ChangeID: ch.ID, ChangeHeadSHA: ch.HeadSHA, GateBypassed: true,
		OccurredAtMS: clock.NowMS(),
	})
	if err != nil {
		t.Fatalf("waiting_human→done: %v", err)
	}
	if done.Status != storage.RunDone {
		t.Fatalf("status=%s, want done", done.Status)
	}
	if !done.GateBypassed {
		t.Fatal("gate_bypassed must be true: done converged on an external merge with no Gate")
	}
	if done.ChangeID != v11ChangeID {
		t.Fatalf("change id=%q, want %q", done.ChangeID, v11ChangeID)
	}
	if done.CompletedAtMS == nil {
		t.Fatal("done run must carry completed_at_ms")
	}

	// The persisted projection matches the outcome.
	run, err := db.Run(ctx, v11RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != storage.RunDone || !run.GateBypassed {
		t.Fatalf("run projection = %+v", run)
	}
	observed, ok, err := db.FirstEventOfType(ctx, v11RunID, "change.merged_observed")
	if err != nil || !ok {
		t.Fatalf("change.merged_observed event missing (ok=%v err=%v)", ok, err)
	}
	if observed.Source != string(storage.SourceForge) {
		t.Fatalf("merge-observed source=%q, want forge", observed.Source)
	}
}

// TestV11GateBypassedIsFalseForSiftMerge pins the honest-accounting invariant:
// gate_bypassed is set only when no Gate ran. A plain done transition records
// the flag as carried by the command, so a future Sift-adjudicated merge path
// can record gate_bypassed=false. This guards against the flag drifting to
// "always true".
func TestV11GateBypassedReflectsCommandFlag(t *testing.T) {
	ctx := context.Background()
	db := openV11DB(t)
	if err := db.SeedProjectForTest(ctx, v11Config, v11Project, v11Base); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run-flag", v11Project, v11Config, "78", v11Base); err != nil {
		t.Fatal(err)
	}
	clock := NewClock(v11Base)
	waiting, err := db.TransitionRun(ctx, "run-flag", 1, storage.DomainCommand{
		To: storage.RunWaitingHuman, Source: storage.SourceSystem, OccurredAtMS: clock.NowMS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// A converged done that did NOT bypass (e.g. a future Gate-adjudicated path)
	// records gate_bypassed=false, proving the flag is commanded, not defaulted.
	done, err := db.TransitionRun(ctx, "run-flag", waiting.Version, DomainCommandMerge(false, clock.NowMS()))
	if err != nil {
		t.Fatal(err)
	}
	if done.GateBypassed {
		t.Fatal("gate_bypassed must reflect the command flag, not default to true")
	}
}

// DomainCommandMerge builds a done DomainCommand with the gate_bypassed flag
// under test control, isolating the flag from the convergence path above.
func DomainCommandMerge(bypass bool, nowMS int64) storage.DomainCommand {
	return storage.DomainCommand{
		To: storage.RunDone, Source: storage.SourceForge, ChangeID: "c-flag",
		GateBypassed: bypass, OccurredAtMS: nowMS,
	}
}
