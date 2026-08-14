package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/xsift/sift/internal/config"
	runtimepkg "github.com/xsift/sift/internal/runtime"
	"github.com/xsift/sift/internal/storage"
)

// ProbeProcessCheckCoordinator drives startup_stall attempt_probes from
// pending|running to the unique ApplyRetryProbeResult finalizer
// (specs/command.md §5, specs/storage.md §5.5). It is a supervisor-tick domain
// — NOT an outbox worker — because process observation and idempotent
// finalization must never enter the outbox: a probe that crashes mid-observation
// simply resumes from pending|running on the next tick.
//
// Each tick scans pending|running probes, claims pending->running
// (crash-resumable), observes the bound attempt's wrapper process outside any
// transaction, and commits exactly once through ApplyRetryProbeResult. It never
// closes an Interrupt, releases isolation, or creates an attempt outside that
// single finalizer transaction.
type ProbeProcessCheckCoordinator struct {
	DB          *storage.DB
	Inspector   runtimepkg.ProcessInspector
	Runtime     config.Runtime
	ControlRoot string
	Now         func() time.Time
	// Sleep paces the bounded absence recheck. Defaults to a context-aware
	// sleep; tests inject an instant sleep.
	Sleep func(context.Context, time.Duration) error
}

func (c *ProbeProcessCheckCoordinator) nowMS() int64 {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	return now().UnixMilli()
}

func (c *ProbeProcessCheckCoordinator) sleep() func(context.Context, time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep
	}
	return sleepDuration
}

func (c *ProbeProcessCheckCoordinator) controlPath(runID string, attemptNo int) string {
	if c.ControlRoot == "" {
		return ""
	}
	return filepath.Join(c.ControlRoot, "runs", runID, "attempts", strconv.Itoa(attemptNo), "control.json")
}

// Tick advances all pending|running startup_stall probes toward the unique
// ApplyRetryProbeResult finalizer. A probe whose observation errors is left
// running for the next tick; every deterministic outcome (proven absence or
// unconfirmed) is committed through the finalizer exactly once. ErrRejectedStale
// from the finalizer is swallowed: a concurrent fact-win, escalation or replay
// already finalized this probe, and the finalizer's CAS guarantees at most one
// success/failure transition.
func (c *ProbeProcessCheckCoordinator) Tick(ctx context.Context) error {
	candidates, err := c.DB.PendingRetryProbes(ctx)
	if err != nil {
		return fmt.Errorf("probe process-check: list probes: %w", err)
	}
	for _, candidate := range candidates {
		if err := c.advanceProbe(ctx, candidate); err != nil {
			return err
		}
	}
	return nil
}

func (c *ProbeProcessCheckCoordinator) advanceProbe(ctx context.Context, candidate storage.RetryProbeCandidate) error {
	// pending -> running CAS (crash-resumable). A no-op for an already-running
	// probe; either way the finalizer's own pending|running CAS is the
	// at-most-once guard, so the claim result is intentionally ignored.
	if _, err := c.DB.ClaimRetryProbe(ctx, candidate.ProbeID, c.nowMS()); err != nil {
		return fmt.Errorf("probe process-check: claim probe %s: %w", candidate.ProbeID, err)
	}
	succeeded, evidence, err := c.observe(ctx, candidate)
	if err != nil {
		// Leave the probe running; the next tick re-observes. The Interrupt
		// stays probe_in_progress, which is the honest state for an unresolved
		// observation error. Escalation cannot fire while a probe is live.
		return fmt.Errorf("probe process-check: observe probe %s: %w", candidate.ProbeID, err)
	}
	if _, err := c.DB.ApplyRetryProbeResult(ctx, storage.RetryProbeResultCmd{
		InterruptID:         candidate.InterruptID,
		ProbeID:             candidate.ProbeID,
		Succeeded:           succeeded,
		AbsenceEvidenceJSON: evidence,
		ExpectedRunVersion:  candidate.ExpectedRunVersion,
		NowMS:               c.nowMS(),
	}); err != nil && err != storage.ErrRejectedStale {
		return fmt.Errorf("probe process-check: finalize probe %s: %w", candidate.ProbeID, err)
	}
	return nil
}

// observe performs the transaction-free process observation for one probe and
// returns the finalizer inputs: proven absence (Succeeded=true with evidence)
// or unconfirmed (Succeeded=false, no evidence). A missing wrapper identity
// cannot be observed, so it is unconfirmed (absence cannot be proven).
//
// Controlled TERM->KILL is intentionally NOT performed here. A late started
// fact can legitimately revive the bound attempt (fact wins, specs/command.md
// §5), and the probe must never signal a process that may have just become
// legitimate. Controlled termination already ran through
// RecordTerminationObservation during recovery; this worker is an absence
// recheck, not a re-termination. The bounded absence recheck reuses the same
// identity comparison and fail-closed semantics as Terminator.absent.
func (c *ProbeProcessCheckCoordinator) observe(ctx context.Context, candidate storage.RetryProbeCandidate) (bool, json.RawMessage, error) {
	identity := runtimepkg.ProcessIdentity{
		PID:              candidate.WrapperPID,
		StartedAtMS:      candidate.WrapperStartedAtMS,
		Executable:       candidate.WrapperExecutable,
		PGID:             candidate.WrapperPGID,
		ControlNonceHash: candidate.ControlNonceHash,
		ControlPath:      c.controlPath(candidate.RunID, candidate.AttemptNo),
	}
	if identity.PID <= 0 || identity.StartedAtMS <= 0 || identity.Executable == "" || identity.PGID <= 0 || identity.ControlNonceHash == "" {
		// No recorded identity -> cannot observe -> absence unconfirmed.
		return false, nil, nil
	}
	if c.Inspector == nil {
		return false, nil, fmt.Errorf("process inspector not configured")
	}
	absent, err := c.absentAfterRecheck(ctx, identity)
	if err != nil {
		return false, nil, err
	}
	if !absent {
		return false, nil, nil
	}
	return true, absenceEvidenceJSON(c.nowMS()), nil
}

// absentAfterRecheck observes the wrapper process and, while it remains present
// with a matching identity, rechecks up to Runtime.AbsenceRecheckCount times
// separated by Runtime.AbsenceRecheckInterval. A missing PID is absence; a
// changed identity is not (PID reuse must fail closed). It never signals.
func (c *ProbeProcessCheckCoordinator) absentAfterRecheck(ctx context.Context, identity runtimepkg.ProcessIdentity) (bool, error) {
	rechecks := c.Runtime.AbsenceRecheckCount
	if rechecks < 1 {
		rechecks = 1
	}
	interval := c.Runtime.AbsenceRecheckInterval
	sleep := c.sleep()
	for i := 0; i < rechecks; i++ {
		observation, err := c.Inspector.Observe(ctx, identity)
		if err != nil {
			return false, fmt.Errorf("observe process: %w", err)
		}
		if !observation.Exists {
			return true, nil
		}
		if !runtimepkg.SameIdentity(identity, observation.ProcessIdentity) {
			// A changed identity is not proof the recorded group is gone.
			return false, nil
		}
		if i+1 < rechecks {
			if err := sleep(ctx, interval); err != nil {
				return false, err
			}
		}
	}
	return false, nil
}

// absenceEvidenceJSON is the proven-absence evidence committed by
// ApplyRetryProbeResult on the success arm. It records that absence was
// confirmed by observation (no signal was sent) and when.
func absenceEvidenceJSON(nowMS int64) json.RawMessage {
	body, _ := json.Marshal(map[string]any{
		"cause":             "absent",
		"method":            "observed_absent",
		"controlled_signal": false,
		"observed_at_ms":    nowMS,
	})
	return body
}

func sleepDuration(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
