// Package attempt defines the Agent execution evidence port (DESIGN §8.4).
//
// The real Runtime (M3) spawns a per-attempt wrapper that directly spawns the
// agent into its process group, records the agent start facts atomically and
// writes result.json on completion. This file freezes the evidence types and
// the Runner port the M1 skeleton chain needs; FakeAgent in fake.go serves the
// V9 first-segment CI chain with the same contract.
//
// The port deliberately exposes only the two facts the skeleton chain drives
// state on — "the agent started" and "the attempt produced completion evidence"
// (DESIGN §8.4: running only admits agent-start evidence; done only admits a
// merged Change). The full launch protocol (claim:acquire/permit/started,
// process-group evidence, worktree lifecycle) lands in M3 behind this port.
package attempt

import (
	"context"
	"errors"
	"time"
)

// Launch is the request to start one attempt's agent. It carries the frozen
// identity the M3 wrapper would receive via control.json; the skeleton uses it
// to attribute the start event and the completion evidence.
type Launch struct {
	RunID      string
	AttemptNo  int
	AgentID    string
	Worktree   string // absolute path; empty in the M1 fake
	BranchName string
	BaseRef    string
	BaseSHA    string
}

// Started is the agent-start evidence. The M3 Runtime obtains it from
// claim:started (agent PID/executable/started timestamp); the skeleton records
// only StartedAt as the P50 "Agent started" timestamp (PRD §10.2).
type Started struct {
	StartedAt time.Time
}

// Result is the wrapper's result.json content, normalized (DESIGN §8.4 wrapper
// contract step 8). Exactly one of ExitCode/Signal is set on a finished
// attempt; FinalHeadSHA is what links the attempt to the Change the reconciler
// later observes as merged.
type Identity struct {
	PID         int    `json:"pid"`
	StartedAtMS int64  `json:"started_at_ms"`
	Executable  string `json:"executable"`
}

type Result struct {
	ExitCode     *int
	Signal       string
	FinalHeadSHA string
	Digest       string // result.json content digest
	FinishedAt   time.Time
	Agent        Identity
}

// ErrNotFinished is returned by Result before the fake agent has produced
// completion evidence.
var ErrNotFinished = errors.New("attempt: not finished")

// Runner is the agent execution port. Launch starts one attempt and returns the
// agent-start evidence; Result returns the completion evidence once available.
// The real process/tmux backends (M3) implement this by spawning the wrapper;
// FakeAgent serves the skeleton chain in-process.
type Runner interface {
	// Launch starts the agent for one attempt. The returned Started carries the
	// agent-start timestamp that drives the Run queued→running transition.
	Launch(ctx context.Context, l Launch) (Started, error)

	// Result returns the completion evidence for one attempt, or ErrNotFinished
	// while it is still running. The skeleton drives the reconciler from this.
	Result(ctx context.Context, runID string, attemptNo int) (Result, error)
}
