// Package skeleton drives the M1 fake skeleton chain (WBS M1 §1.6): it wires the
// fake Forge port, the Brain call shell (with its fake provider) and the fake
// Agent runner against the real storage write ports, then advances a Run
//
//	fake Issue → T1/T2 → queued → fake attempt completion evidence
//	            → inject fake forge「Change 已合并」fact → done
//
// without a temporary Gate, without creating the Change, and without bypass
// adjudication. The Run converges done purely on the injected forge merge fact
// (PRD §4.1 / §4.5, DESIGN §8.2), honestly recorded as gate_bypassed because no
// Gate ran. Event timestamps cover trigger-observed → agent-started so the P50
// measurement (PRD §10.2) has day-1 data.
//
// The chain is the V9 first segment: it is an integration harness, not daemon
// code. The full intake/T2-commit/launch write ports land in M2/M3 and will
// replace the M1 skeleton stubs this driver composes.
package skeleton

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/miaoxiaoyong/sift/internal/attempt"
	"github.com/miaoxiaoyong/sift/internal/brain"
	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/forge"
	"github.com/miaoxiaoyong/sift/internal/schema"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// TriggerLabel is the trusted label that makes an Issue driving (PRD §9.2). The
// M1 skeleton uses a single stable label; the real intake reads it from config.
const TriggerLabel = "sift"

// Clock is the single injected time source the chain advances; storage logic
// never reads the wall clock (storage.md §1 invariant 7) and the chain owns the
// trigger→started deltas the P50 measurement reads.
type Clock struct {
	t int64 // milliseconds
}

// NewClock returns a clock frozen at startMS.
func NewClock(startMS int64) *Clock { return &Clock{t: startMS} }

// NowMS returns the current injected millisecond timestamp.
func (c *Clock) NowMS() int64 { return c.t }

// Now returns the current injected time.
func (c *Clock) Now() time.Time { return time.UnixMilli(c.t) }

// Advance moves the clock forward by d. The chain calls it between major steps
// to produce ordered, deterministic event timestamps.
func (c *Clock) Advance(d time.Duration) { c.t += d.Milliseconds() }

// Outcome is the result of one full skeleton-chain drive.
type Outcome struct {
	Run RunOutcome
	// TriggerObservedAtMS / AgentStartedAtMS are the P50 anchors (PRD §10.2):
	// trusted trigger-label observed → agent started. P50 = started − observed.
	TriggerObservedAtMS int64
	AgentStartedAtMS    int64
	// GateBypassed records that done converged on a forge merge fact with no
	// Gate having run (PRD §4.1 / §10.2): honest accounting, not adjudication.
	GateBypassed bool
}

// RunOutcome carries the final Run and the brain call ids the chain produced.
type RunOutcome struct {
	RunID    string
	Status   storage.RunStatus
	ChangeID string
	Version  int64
	T1CallID string
	T2CallID string
}

// Chain wires the fake ports and storage into the M1 skeleton chain.
type Chain struct {
	db     *storage.DB
	forge  *forge.Fake
	shell  *brain.Shell
	agent  attempt.Runner
	clock  *Clock
	config ChainConfig
}

// ChainConfig carries the frozen identity and policy facts the chain needs.
type ChainConfig struct {
	ProjectID         string
	ConfigSnapshotID  string
	Project           forge.ProjectRef
	CandidateAgentIDs []string // T2 input candidates; the T2 output must hit one
	PolicyHash        string
	IssueBody         string // untrusted issue body offered to T1/T2
	// LaunchLatency is the trigger→started contribution the skeleton injects
	// between assignment and agent launch, so the P50 measurement (PRD §10.2)
	// has realistic day-1 data rather than a zero delta.
	LaunchLatency time.Duration
}

// NewChain wires the chain. The brain Shell must be constructed with the
// FakeProvider; the Agent runner should be the FakeAgent. The clock is owned by
// the caller so tests can assert the trigger→started delta deterministically.
func NewChain(db *storage.DB, fc *forge.Fake, shell *brain.Shell, agent attempt.Runner, clock *Clock, cfg ChainConfig) *Chain {
	return &Chain{db: db, forge: fc, shell: shell, agent: agent, clock: clock, config: cfg}
}

// DefaultBrainConfig returns the Brain config the skeleton uses: a non-empty
// executable so the shell actually invokes the (fake) provider, the V0 protocol
// and a generous token budget so the chain's two brain calls always run.
func DefaultBrainConfig() config.Brain {
	d := config.DefaultConfig().Brain
	d.Executable = "fake-cli"
	return d
}

// Drive advances one fake Issue through the full skeleton chain to done. The
// issue is already scripted into the Fake forge (with its trigger-label event);
// changeID is the Change the reconciler will later observe as merged. The fake
// agent produces completion evidence whose head SHA matches the injected merge.
func (c *Chain) Drive(ctx context.Context, runID string, issue forge.Issue, trigger forge.LabelEvent, changeID string) (Outcome, error) {
	var out Outcome
	out.Run.RunID = runID

	// ---- Step 1: T1 intake体检 (brain.md §7). ---------------------------------
	// The intake worker resolves candidates (none in the M1 fake) and calls T1.
	t1Input, err := brain.BuildT1Input(brain.T1Input{
		Forge: brain.T1Forge{
			Kind:       string(c.config.Project.Kind),
			Host:       c.config.Project.Host,
			ProjectKey: c.config.Project.ProjectKey,
		},
		Issue: brain.T1Issue{
			ID: issue.ID, Title: issue.Title, Body: issue.Body,
			Author: issue.Author, URL: issue.URL, Labels: issue.Labels,
		},
	})
	if err != nil {
		return out, fmt.Errorf("skeleton: build T1 input: %w", err)
	}
	c.clock.Advance(time.Second)
	t1res, err := c.shell.Call(ctx, brain.T1Contract(nil), brain.CallParams{
		Scope:      storage.BrainScopeIntake,
		SubjectKey: fmt.Sprintf("forge:%s:%s:%s:issue:%s", c.config.Project.Kind, c.config.Project.Host, c.config.Project.ProjectKey, issue.ID),
		ProjectID:  c.config.ProjectID,
		Input:      t1Input,
	})
	if err != nil {
		return out, fmt.Errorf("skeleton: T1 call: %w", err)
	}
	out.Run.T1CallID = t1res.CallID
	// T1 fallback (provider unavailable) still converges ready (brain.md §7.2):
	// the skeleton enqueues the Issue either way — it never silently drops it.
	// A needs_clarification/possible_duplicate disposition belongs to M2 intake;
	// the M1 fake provider emits ready, so a non-ready result here is a wiring
	// bug, not a flow the chain should paper over.
	if t1res.Status == storage.BrainCallValid {
		var t1out brain.T1Output
		if err := schema.Decode(t1res.Output, &t1out, schema.Closed); err != nil {
			return out, fmt.Errorf("skeleton: decode T1 output: %w", err)
		}
		if t1out.Disposition == nil || *t1out.Disposition != brain.T1Ready {
			return out, fmt.Errorf("skeleton: T1 disposition %v not handled by M1 skeleton", t1out.Disposition)
		}
	}

	// ---- Step 2: T1 ready → create the forge Run (intake→Run). ----------------
	// The trigger-observed timestamp is the P50 start anchor (PRD §10.2).
	c.clock.Advance(time.Second)
	created, err := c.db.CreateForgeRun(ctx, storage.CreateForgeRunCmd{
		RunID:               runID,
		ProjectID:           c.config.ProjectID,
		ConfigSnapshotID:    c.config.ConfigSnapshotID,
		ForgeKind:           string(c.config.Project.Kind),
		ForgeHost:           c.config.Project.Host,
		ForgeProjectKey:     c.config.Project.ProjectKey,
		IssueID:             issue.ID,
		IssueURL:            issue.URL,
		IssueAuthor:         issue.Author,
		TriggerLabelEventID: trigger.TargetID + ":" + trigger.Label,
		TriggerActor:        trigger.Actor,
		TriggerObservedAtMS: trigger.ObservedAt.UnixMilli(),
		CreatedAtMS:         c.clock.NowMS(),
	})
	if err != nil {
		return out, fmt.Errorf("skeleton: create forge run: %w", err)
	}
	out.TriggerObservedAtMS = trigger.ObservedAt.UnixMilli()
	if created.Status != storage.RunQueued {
		return out, fmt.Errorf("skeleton: run created as %s, want queued", created.Status)
	}

	// ---- Step 3: T2 assignment (brain.md §8). --------------------------------
	t2Input, err := brain.BuildT2Input(brain.T2Input{
		RunID:           runID,
		Issue:           brain.T2Issue{Title: issue.Title, Body: issue.Body, URL: issue.URL},
		CandidateAgents: candidates(c.config.CandidateAgentIDs),
		BaseContext:     brain.T2BaseContext{ProjectContext: c.config.IssueBody},
	})
	if err != nil {
		return out, fmt.Errorf("skeleton: build T2 input: %w", err)
	}
	c.clock.Advance(time.Second)
	t2res, err := c.shell.Call(ctx, brain.T2Contract(c.config.CandidateAgentIDs), brain.CallParams{
		Scope:      storage.BrainScopeRun,
		SubjectKey: "run:" + runID,
		RunID:      runID,
		Input:      t2Input,
	})
	if err != nil {
		return out, fmt.Errorf("skeleton: T2 call: %w", err)
	}
	out.Run.T2CallID = t2res.CallID
	if t2res.Status != storage.BrainCallValid || t2res.Output == nil {
		return out, errors.New("skeleton: T2 did not produce a valid assignment (M1 fake provider must emit a legal T2 output)")
	}
	var t2out brain.T2Output
	if err := schema.Decode(t2res.Output, &t2out, schema.Closed); err != nil {
		return out, fmt.Errorf("skeleton: decode T2 output: %w", err)
	}
	// Effective HITL = LLM OR deterministic force (brain.md §8.3); the M1 fake
	// Issue author is trusted, so the force is false and the LLM value wins.
	hitl := brain.EffectiveHITL(*t2out.HITLBeforeStart, false)

	// ---- Step 4: assemble the Task Spec + commit the assignment. -------------
	c.clock.Advance(time.Second)
	canonical, digest, err := brain.AssembleTaskSpec(brain.TaskSpecParams{
		Title: issue.Title, Body: issue.Body, SourceURL: issue.URL,
		Goals:          *t2out.Goals,
		PolicyHash:     c.config.PolicyHash,
		ProjectContext: brain.ContextSegment{Text: c.config.IssueBody},
		Kind:           *t2out.Kind, Agent: *t2out.Agent,
		HITLBeforeStart: hitl,
		LogicalCallID:   t2res.CallID, PromptVersion: brain.T2Asset().PromptVersion,
	})
	if err != nil {
		return out, fmt.Errorf("skeleton: assemble task spec: %w", err)
	}
	if _, err := c.db.SetInitialTaskSpec(ctx, storage.SetInitialTaskSpecCmd{
		RunID:           runID,
		ExpectedVersion: created.Version,
		TaskSpecID:      digestID(runID, digest),
		CanonicalJSON:   canonical,
		ContentDigest:   digest,
		Kind:            string(*t2out.Kind),
		AgentID:         *t2out.Agent,
		HITLBeforeStart: hitl,
		OccurredAtMS:    c.clock.NowMS(),
	}); err != nil {
		return out, fmt.Errorf("skeleton: set initial task spec: %w", err)
	}

	// hitl=true needs the M3 Interrupt emission core; the M1 skeleton drives the
	// hitl=false path only. The fake provider emits hitl=false, so this is a
	// wiring guard, not a runtime branch.
	if hitl {
		return out, errors.New("skeleton: hitl=true T2 requires the M3 Interrupt core (out of M1 scope)")
	}

	// ---- Step 5: launch the fake agent → Run queued→running. -----------------
	// The agent-start timestamp is the P50 end anchor (PRD §10.2). The chain
	// advances its clock by the configured launch latency first so the P50
	// delta reflects realistic day-1 data rather than a zero gap.
	agentID := *t2out.Agent
	c.clock.Advance(c.config.LaunchLatency)
	if _, err := c.agent.Launch(ctx, attempt.Launch{
		RunID: runID, AttemptNo: 1, AgentID: agentID,
		Worktree: "", BranchName: "sift/" + runID, BaseRef: "main", BaseSHA: "base",
	}); err != nil {
		return out, fmt.Errorf("skeleton: launch agent: %w", err)
	}
	assigned, err := c.db.Run(ctx, runID)
	if err != nil {
		return out, err
	}
	running, err := c.db.TransitionRun(ctx, runID, assigned.Version, storage.DomainCommand{
		To: storage.RunRunning, Source: storage.SourceAgent, Actor: agentID,
		OccurredAtMS: c.clock.NowMS(),
	})
	if err != nil {
		return out, fmt.Errorf("skeleton: queued→running: %w", err)
	}
	out.AgentStartedAtMS = c.clock.NowMS()

	// ---- Step 6: fake attempt completion evidence. --------------------------
	// The fake agent publishes result.json-equivalent evidence (exit 0, head
	// SHA). The skeleton records it as an event; M3 will record it on the
	// attempt row via the launch protocol.
	c.clock.Advance(time.Second)
	// The M1 skeleton's runner is the FakeAgent; Complete publishes the
	// result.json-equivalent evidence the chain then reads. The real Runtime
	// (M3) publishes it from the wrapper, not from the supervisor.
	if cr, ok := c.agent.(*attempt.FakeAgent); ok {
		cr.Complete(runID, 1)
	}
	res, err := c.agent.Result(ctx, runID, 1)
	if err != nil {
		return out, fmt.Errorf("skeleton: agent result: %w", err)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		return out, fmt.Errorf("skeleton: agent exit %v, M1 chain expects success", res.ExitCode)
	}
	if err := c.recordAttemptCompleted(ctx, runID, agentID, res, c.clock.NowMS()); err != nil {
		return out, fmt.Errorf("skeleton: record attempt evidence: %w", err)
	}

	// ---- Step 7: inject fake forge「Change 已合并」fact → done. ---------------
	// No temporary Gate, no create_change/merge_change outbox: the merge is an
	// external forge fact (PRD §4.5). The reconciler converges done and records
	// gate_bypassed because no Gate adjudicated it (PRD §4.1 / §10.2). This is
	// honest accounting, not bypass adjudication.
	c.clock.Advance(time.Second)
	if _, err := c.forge.InjectMerged(c.config.Project, changeID, c.clock.Now()); err != nil {
		return out, fmt.Errorf("skeleton: inject merge fact: %w", err)
	}
	ch, err := c.forge.GetChange(ctx, c.config.Project, changeID)
	if err != nil {
		return out, fmt.Errorf("skeleton: observe merged change: %w", err)
	}
	if ch.State != forge.ChangeMerged {
		return out, fmt.Errorf("skeleton: change state %s, want merged", ch.State)
	}
	if err := c.recordChangeMergedObserved(ctx, runID, ch, c.clock.NowMS()); err != nil {
		return out, fmt.Errorf("skeleton: record merge fact: %w", err)
	}
	done, err := c.db.TransitionRun(ctx, runID, running.Version, storage.DomainCommand{
		To: storage.RunDone, Source: storage.SourceForge, Actor: issue.Author,
		ChangeID: ch.ID, ChangeHeadSHA: ch.HeadSHA, GateBypassed: true,
		OccurredAtMS: c.clock.NowMS(),
	})
	if err != nil {
		return out, fmt.Errorf("skeleton: running→done: %w", err)
	}
	out.Run.Status = done.Status
	out.Run.ChangeID = done.ChangeID
	out.Run.Version = done.Version
	out.GateBypassed = done.GateBypassed
	return out, nil
}

// recordAttemptCompleted appends an attempt.completed event carrying the fake
// agent's completion evidence (exit code + head SHA). It does not write an
// attempts row — the full launch protocol (claim:acquire/permit/started) is M3;
// the M1 skeleton records the spine evidence only.
func (c *Chain) recordAttemptCompleted(ctx context.Context, runID, agentID string, res attempt.Result, nowMS int64) error {
	exit := -1
	if res.ExitCode != nil {
		exit = *res.ExitCode
	}
	body, _ := json.Marshal(map[string]any{
		"attempt_no":    1,
		"agent":         agentID,
		"exit_code":     exit,
		"final_head":    res.FinalHeadSHA,
		"result_digest": res.Digest,
	})
	_, err := c.db.AppendEvent(ctx, storage.EventCmd{
		RunID: runID, Type: "attempt.completed", Source: storage.SourceAgent, Actor: agentID,
		PayloadJSON: body, OccurredAtMS: nowMS, RecordedAtMS: nowMS,
	})
	return err
}

// recordChangeMergedObserved appends a change.merged_observed event — the forge
// fact the reconciler converged done on.
func (c *Chain) recordChangeMergedObserved(ctx context.Context, runID string, ch forge.Change, nowMS int64) error {
	body, _ := json.Marshal(map[string]any{
		"change_id": ch.ID,
		"head_sha":  ch.HeadSHA,
		"merged_at": ch.MergedAt.UnixMilli(),
	})
	_, err := c.db.AppendEvent(ctx, storage.EventCmd{
		RunID: runID, Type: "change.merged_observed", Source: storage.SourceForge,
		PayloadJSON: body, OccurredAtMS: nowMS, RecordedAtMS: nowMS,
	})
	return err
}

func candidates(ids []string) []brain.T2AgentCandidate {
	out := make([]brain.T2AgentCandidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, brain.T2AgentCandidate{ID: id, Capabilities: []string{"go"}})
	}
	return out
}

func digestID(runID, digest string) string {
	return runID + "-spec-" + digest[:12]
}
