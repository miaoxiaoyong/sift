// Package attempt defines the Agent execution evidence used by the M1
// skeleton chain (DESIGN §8.4). FakeAgent supplies deterministic started and
// completion facts; the production M3 process runtime uses its own launch and
// wrapper protocol.
package attempt

import (
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
	ExitCode      *int
	Signal        string
	FailureReason string
	FinalHeadSHA  string
	Digest        string // result.json content digest
	FinishedAt    time.Time
	Agent         Identity
}

// ErrNotFinished is returned by Result before the fake agent has produced
// completion evidence.
var ErrNotFinished = errors.New("attempt: not finished")
