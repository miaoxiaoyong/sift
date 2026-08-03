package skeleton

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/attempt"
	"github.com/miaoxiaoyong/sift/internal/brain"
	"github.com/miaoxiaoyong/sift/internal/forge"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// V9 first segment (WBS M1 §1.6): the fake skeleton chain drives one fake Issue
// all the way to done through the real storage write ports, the Brain call shell
// (fake provider) and the fake Forge/Agent ports — with no temporary Gate, no
// Create Change and no bypass adjudication. It is the CI spine M4/M5 extend.

const (
	segBase     = int64(1_700_000_000_000) // deterministic epoch for day-1 data
	segProject  = "proj-21"
	segConfig   = "cfg-21"
	segRunID    = "run-21"
	segChangeID = "change-21"
	segHeadSHA  = "deadbeefcafebabe0000000000000000deadbeef"
	segAgent    = "fake-agent"
	segPolicy   = "policy-hash-21"
	segTrigger  = "sift"
	segAuthor   = "trusted-operator" // trusted allowlist actor
)

func openSegDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.Open(context.Background(), storage.OpenConfig{
		Path:          t.TempDir() + "/sift-home/sift.db",
		BinaryVersion: "test-binary",
		Now:           time.UnixMilli(segBase),
	})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// newSegmentChain builds a fully wired chain with a scripted fake Issue, trigger
// label event, open Change and a fake agent whose head SHA matches the change.
func newSegmentChain(t *testing.T) (*Chain, *forge.Fake, *brain.FakeProvider, *attempt.FakeAgent) {
	t.Helper()
	db := openSegDB(t)
	ctx := context.Background()
	seedConfig := segConfig + "-seed"
	if err := db.SeedProjectForTest(ctx, seedConfig, segProject, segBase); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(ctx, `INSERT INTO config_snapshots
		(id, config_hash, schema_version, canonical_json, source_present, loaded_at_ms, binary_version)
		VALUES (?, ?, 1, ?, 1, ?, 'test')`, segConfig, "hash-"+segConfig, `{"agents":[{"id":"fake-agent","backend":"process"}]}`, segBase); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(ctx, `UPDATE projects SET config_snapshot_id=? WHERE id=?`, segConfig, segProject); err != nil {
		t.Fatal(err)
	}

	fc := forge.NewFake()
	project := forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo-" + segProject}
	issue := fc.AddIssue(project, forge.Issue{
		ID: "42", Title: "Implement issue #21 skeleton chain",
		Body:   "fake Issue → T1/T2 → queued → attempt evidence → forge merge → done",
		Author: segAuthor, URL: "https://github.com/org/repo-" + segProject + "/issues/42",
		Labels: []string{segTrigger},
	})
	// Trusted actor applied the trigger label — the only kind of event that can
	// drive an Issue (PRD §9.2).
	fc.AddLabelEvent(project, forge.LabelEvent{
		TargetID: issue.ID, Label: segTrigger, Action: forge.LabelAdded,
		Actor: segAuthor, ObservedAt: time.UnixMilli(segBase),
	})
	fc.AddChange(project, segChangeID, segHeadSHA)

	// Brain fake provider: T1 ready on first call, T2 assignment on second.
	provider := &brain.FakeProvider{Responses: []brain.FakeResponse{
		{ResultText: brain.ValidT1ResultText(), InputTokens: 10, OutputTokens: 4},
		{ResultText: brain.ValidT2ResultText(brain.TaskFeature, segAgent, []string{"wire the fake skeleton chain to done"}, false), InputTokens: 18, OutputTokens: 9},
	}}

	clock := NewClock(segBase)
	shell := brain.NewShell(db, DefaultBrainConfig(), provider, clock.Now)
	agent := attempt.NewFakeAgent(0, segHeadSHA, "result-digest-21", attempt.WithFakeNow(clock.Now))

	chain := NewChain(db, fc, shell, agent, clock, ChainConfig{
		ProjectID:         segProject,
		ConfigSnapshotID:  segConfig,
		Project:           project,
		CandidateAgentIDs: []string{segAgent},
		PolicyHash:        segPolicy,
		IssueBody:         issue.Body,
		LaunchLatency:     7 * time.Second, // realistic day-1 trigger→started delta
	})
	return chain, fc, provider, agent
}

// TestV9FirstSegmentSkeletonChain is the V9 first-segment CI test (WBS M1 §1.6).
func TestV9FirstSegmentSkeletonChain(t *testing.T) {
	ctx := context.Background()
	chain, fc, provider, _ := newSegmentChain(t)

	out, err := chain.Drive(ctx, segRunID, forgeIssue(chain), forgeTrigger(chain), segChangeID)
	if err != nil {
		t.Fatalf("Drive: %v", err)
	}

	// The Run converged done purely on the injected forge merge fact.
	if out.Run.Status != storage.RunDone {
		t.Fatalf("final status = %s, want done", out.Run.Status)
	}
	if out.Run.ChangeID != segChangeID {
		t.Fatalf("change id = %q, want %q", out.Run.ChangeID, segChangeID)
	}
	// gate_bypassed records honestly that no Gate adjudicated the merge (PRD
	// §4.1 / §10.2); it is an audit attribute, not bypass adjudication.
	if !out.GateBypassed {
		t.Fatal("gate_bypassed must be true: done converged on a forge fact with no Gate")
	}
	// P50 anchors exist and the start strictly precedes the agent start.
	if out.TriggerObservedAtMS <= 0 || out.AgentStartedAtMS <= 0 {
		t.Fatalf("P50 anchors missing: observed=%d started=%d", out.TriggerObservedAtMS, out.AgentStartedAtMS)
	}
	if out.AgentStartedAtMS <= out.TriggerObservedAtMS {
		t.Fatalf("agent started (%d) must follow trigger observed (%d)", out.AgentStartedAtMS, out.TriggerObservedAtMS)
	}
	p50 := time.Duration(out.AgentStartedAtMS-out.TriggerObservedAtMS) * time.Millisecond
	if p50 <= 0 {
		t.Fatalf("P50 delta non-positive: %s", p50)
	}
	// The P50 delta is the deterministic clock advance between trigger-observed
	// (T0) and the queued→running transition, dominated by the launch latency.
	wantP50 := 7*time.Second + 4*time.Second // launch latency + 4 inter-step advances
	if p50 != wantP50 {
		t.Fatalf("P50 delta = %s, want deterministic %s", p50, wantP50)
	}

	// The final Run projection matches the outcome.
	run, err := chain.db.Run(ctx, segRunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != storage.RunDone || run.ChangeID != segChangeID || !run.GateBypassed {
		t.Fatalf("run projection = %+v", run)
	}
	if run.Kind != string(brain.TaskFeature) || run.AgentID != segAgent {
		t.Fatalf("run kind/agent = %s/%s", run.Kind, run.AgentID)
	}
	if run.CompletedAtMS == nil {
		t.Fatal("done run must carry completed_at_ms")
	}

	// Brain traces: both calls finalized valid on attempt 1 (brain.md §10).
	assertBrainCallValid(t, chain, out.Run.T1CallID, "T1")
	assertBrainCallValid(t, chain, out.Run.T2CallID, "T2")
	// The two calls sent byte-identical prompts per call (no retry needed), and
	// each touched the provider exactly once.
	if len(provider.Requests) != 2 {
		t.Fatalf("provider invoked %d times, want 2 (one T1 + one T2)", len(provider.Requests))
	}

	// The event timeline carries the P50 anchors deterministically.
	observed, ok, err := chain.db.FirstEventOfType(ctx, segRunID, "intake.trigger_observed")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || observed.OccurredAtMS != out.TriggerObservedAtMS {
		t.Fatalf("trigger_observed event: ok=%v at=%d want %d", ok, observed.OccurredAtMS, out.TriggerObservedAtMS)
	}
	if observed.Actor != segAuthor || observed.Source != string(storage.SourceForge) {
		t.Fatalf("trigger_observed actor/source = %q/%q", observed.Actor, observed.Source)
	}
	started, ok, err := chain.db.FirstEventOfType(ctx, segRunID, "run.transitioned")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing run.transitioned event")
	}
	// The first run.transitioned is queued→running = agent started (P50 end).
	if started.OccurredAtMS != out.AgentStartedAtMS {
		t.Fatalf("agent-started transition at=%d want %d", started.OccurredAtMS, out.AgentStartedAtMS)
	}
	if _, ok, err := chain.db.FirstEventOfType(ctx, segRunID, "attempt.completed"); err != nil || !ok {
		t.Fatalf("attempt.completed event missing (err=%v ok=%v)", err, ok)
	}
	if _, ok, err := chain.db.FirstEventOfType(ctx, segRunID, "change.merged_observed"); err != nil || !ok {
		t.Fatalf("change.merged_observed event missing (err=%v ok=%v)", err, ok)
	}

	// No create_change / merge_change outbox operations: M1 does not create the
	// Change or run a Gate (WBS M1 §1.6).
	if hasOutboxKind(t, chain, storage.OperationCreateChange) {
		t.Fatal("M1 skeleton must not create a change_change outbox operation")
	}
	if hasOutboxKind(t, chain, storage.OperationMergeChange) {
		t.Fatal("M1 skeleton must not create a merge_change outbox operation")
	}

	// The injected forge fact is observable: the change is merged.
	ch, err := fc.GetChange(ctx, chain.config.Project, segChangeID)
	if err != nil {
		t.Fatal(err)
	}
	if ch.State != forge.ChangeMerged {
		t.Fatalf("forge change state = %s, want merged", ch.State)
	}
}

// TestV9SkeletonChainIsIdempotentOnReplay proves Drive against an already-done
// Run fails at the first transition rather than double-converging: the spine is
// crash-safe because the storage write ports are CAS-protected.
func TestV9SkeletonChainRejectsDoubleDrive(t *testing.T) {
	ctx := context.Background()
	chain, _, _, _ := newSegmentChain(t)
	issue := forgeIssue(chain)
	trigger := forgeTrigger(chain)

	if _, err := chain.Drive(ctx, segRunID, issue, trigger, segChangeID); err != nil {
		t.Fatalf("first Drive: %v", err)
	}
	// A second Drive on the same run id fails: CreateForgeRun returns the
	// existing done Run, then the queued→running transition is rejected.
	if _, err := chain.Drive(ctx, segRunID, issue, trigger, segChangeID); err == nil {
		t.Fatal("second Drive must fail: the Run is already done")
	}
}

// TestV9SkeletonChainT1FallbackStillEnqueues proves the skeleton never silently
// drops an Issue: when the brain provider is unavailable, T1 converges to the
// deterministic ready fallback and the Run is still created (brain.md §7.2).
// The provider stays unavailable for T2 too, so the assignment cannot complete
// (T2 fallback is human assignment, an M3 path); the skeleton surfaces that as
// a Drive error after the Run was already safely enqueued.
func TestV9SkeletonChainT1FallbackStillEnqueues(t *testing.T) {
	ctx := context.Background()
	chain, _, provider, _ := newSegmentChain(t)
	// Drain the scripted responses so every provider call fails spawn: T1
	// converges to its ready fallback (Issue not dropped), T2 cannot assign.
	provider.Responses = nil

	out, err := chain.Drive(ctx, segRunID, forgeIssue(chain), forgeTrigger(chain), segChangeID)
	if err == nil {
		t.Fatal("Drive must error: T2 has no provider and cannot assign")
	}
	if !strings.Contains(err.Error(), "T2") {
		t.Fatalf("Drive error must point at the T2 assignment step: %v", err)
	}
	// The Run was nevertheless created (Issue never silently dropped).
	run, rerr := chain.db.Run(ctx, segRunID)
	if rerr != nil {
		t.Fatalf("run must exist after T1 fallback: %v", rerr)
	}
	if run.Status != storage.RunQueued {
		t.Fatalf("run status = %s, want queued (enqueued via T1 ready fallback)", run.Status)
	}
	t1, attempts, terr := chain.db.BrainCallTrace(ctx, out.Run.T1CallID)
	if terr != nil {
		t.Fatal(terr)
	}
	if t1.Status != storage.BrainCallFallback {
		t.Fatalf("T1 status = %s, want fallback when provider is unavailable", t1.Status)
	}
	if len(attempts) == 0 {
		t.Fatal("T1 fallback must record at least one attempt")
	}
}

func assertBrainCallValid(t *testing.T, chain *Chain, callID, touchpoint string) {
	t.Helper()
	if callID == "" {
		t.Fatalf("%s call id empty", touchpoint)
	}
	call, attempts, err := chain.db.BrainCallTrace(context.Background(), callID)
	if err != nil {
		t.Fatalf("%s trace: %v", touchpoint, err)
	}
	if call.Touchpoint != touchpoint || call.Status != storage.BrainCallValid {
		t.Fatalf("%s call = %+v", touchpoint, call)
	}
	if len(attempts) != 1 || attempts[0].Outcome != storage.BrainAttemptValid || attempts[0].ProviderAttempt != 1 {
		t.Fatalf("%s attempts = %+v, want one valid provider attempt", touchpoint, attempts)
	}
}

func hasOutboxKind(t *testing.T, chain *Chain, kind storage.OperationKind) bool {
	t.Helper()
	n, err := chain.db.CountOperationsByKind(context.Background(), kind)
	if err != nil {
		t.Fatalf("count outbox %s: %v", kind, err)
	}
	return n > 0
}

// forgeIssue / forgeTrigger re-read the scripted fake state so the test does not
// duplicate the Issue/label facts the chain config carries.
func forgeIssue(chain *Chain) forge.Issue {
	issues, _, err := chain.forge.ListIssuesByLabel(context.Background(), chain.config.Project, segTrigger, "")
	if err != nil || len(issues) != 1 {
		panic("skeleton test: expected exactly one scripted issue")
	}
	return issues[0]
}

func forgeTrigger(chain *Chain) forge.LabelEvent {
	events, _, err := chain.forge.ListLabelEvents(context.Background(), chain.config.Project, forge.TargetRef{Kind: forge.TargetIssue, ID: forgeIssue(chain).ID}, "")
	if err != nil || len(events) != 1 {
		panic("skeleton test: expected exactly one scripted trigger label event")
	}
	return events[0]
}
