package attempt

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// FakeAgent is the in-memory agent runner of the M1 skeleton chain (WBS M1
// §1.6). It preserves the evidence contract the real Runtime (M3) honors:
// Launch records the agent-start timestamp immediately (the
// skeleton has no process to wait on), and Complete injects a finished result
// with the configured exit code and head SHA.
//
// The fake never spawns a process; the skeleton records the start and
// completion evidence as facts and drives the Run state machine from them,
// exactly as the M3 supervisor will from claim:started and result.json.
type FakeAgent struct {
	mu       sync.Mutex
	started  map[string]Started
	results  map[string]Result
	exitCode int
	headSHA  string
	digest   string
	now      func() time.Time
}

// FakeOption configures a FakeAgent.
type FakeOption func(*FakeAgent)

// WithFakeNow injects the clock; the skeleton owns time (storage.md §1.7), so
// the fake agent's start/finish timestamps come from the same clock the chain
// advances.
func WithFakeNow(now func() time.Time) FakeOption {
	return func(f *FakeAgent) { f.now = now }
}

// NewFakeAgent returns a fake runner. exitCode/headSHA/digest configure the
// evidence Complete will publish.
func NewFakeAgent(exitCode int, headSHA, digest string, opts ...FakeOption) *FakeAgent {
	f := &FakeAgent{
		started:  map[string]Started{},
		results:  map[string]Result{},
		exitCode: exitCode,
		headSHA:  headSHA,
		digest:   digest,
		now:      time.Now,
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

func fakeKey(runID string, attemptNo int) string {
	return runID + "#" + strconv.Itoa(attemptNo)
}

// Launch records the agent-start evidence at the injected now() and returns it
// immediately. There is no process to wait on in the skeleton; the chain
// advances the clock to the launch point before calling Launch.
func (f *FakeAgent) Launch(_ context.Context, l Launch) (Started, error) {
	if l.RunID == "" || l.AttemptNo < 1 {
		return Started{}, ErrNotFinished
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := Started{StartedAt: f.now()}
	f.started[fakeKey(l.RunID, l.AttemptNo)] = s
	return s, nil
}

// Result returns the completion evidence if Complete has been called, otherwise
// ErrNotFinished. The skeleton calls Complete once the fake agent's work is
// done, before injecting the forge merge fact.
func (f *FakeAgent) Result(_ context.Context, runID string, attemptNo int) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.results[fakeKey(runID, attemptNo)]
	if !ok {
		return Result{}, ErrNotFinished
	}
	return r, nil
}

// Complete publishes the completion evidence for one attempt at the injected
// now(). It carries the configured exit code and head SHA — the head SHA is what
// the reconciler matches against the injected forge Change merge fact.
func (f *FakeAgent) Complete(runID string, attemptNo int) Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	exit := f.exitCode
	r := Result{
		ExitCode:     &exit,
		FinalHeadSHA: f.headSHA,
		Digest:       f.digest,
		FinishedAt:   f.now(),
	}
	f.results[fakeKey(runID, attemptNo)] = r
	return r
}
