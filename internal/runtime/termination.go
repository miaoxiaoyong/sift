package runtime

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"syscall"
	"time"
)

// ProcessIdentity is the complete, persisted identity required before Sift may
// signal an attempt's process group. PID alone is deliberately insufficient:
// it can be reused by an unrelated process.
type ProcessIdentity struct {
	PID              int
	StartedAtMS      int64
	Executable       string
	PGID             int
	ControlNonceHash string
}

// ProcessObservation is a fresh OS/control-file observation. ControlNonceHash
// is the SHA-256 digest of the control nonce, never the nonce itself.
type ProcessObservation struct {
	Exists bool
	ProcessIdentity
}

// ProcessInspector and ProcessSignaler isolate platform process inspection and
// signalling. Production implementations may use OS facilities; tests do not
// need to signal real processes.
type ProcessInspector interface {
	Observe(context.Context, int) (ProcessObservation, error)
}
type ProcessSignaler interface {
	SignalGroup(pgid int, signal syscall.Signal) error
}

// UnixProcessSignaler sends signals to a process group, rather than a single
// PID, so the wrapper's in-group Agent descendants are terminated together.
type UnixProcessSignaler struct{}

func (UnixProcessSignaler) SignalGroup(pgid int, signal syscall.Signal) error {
	if pgid <= 0 {
		return ErrInvalidTerminationConfig
	}
	return syscall.Kill(-pgid, signal)
}

type TerminationConfig struct {
	TermGrace       time.Duration
	KillGrace       time.Duration
	AbsenceRechecks int
	RecheckInterval time.Duration
}

type TerminationCause string

const (
	TerminationAbsent          TerminationCause = "absent"
	TerminationIdentityUnknown TerminationCause = "process_identity_unknown"
	TerminationUnconfirmed     TerminationCause = "termination_unconfirmed"
)

type TerminationResult struct {
	Absent  bool
	Cause   TerminationCause
	Signals []syscall.Signal
}

var ErrInvalidTerminationConfig = errors.New("runtime: invalid termination config")

// Terminate performs the only permitted termination sequence: verify the full
// identity, signal the recorded process group with TERM then KILL, and prove
// absence with bounded rechecks. It never signals an uncertain PID.
type Terminator struct {
	Inspector ProcessInspector
	Signaler  ProcessSignaler
	Sleep     func(context.Context, time.Duration) error
}

func (t Terminator) Terminate(ctx context.Context, id ProcessIdentity, cfg TerminationConfig) (TerminationResult, error) {
	if err := validTermination(id, cfg); err != nil {
		return TerminationResult{}, err
	}
	if t.Inspector == nil || t.Signaler == nil {
		return TerminationResult{}, errors.New("runtime: termination inspector and signaler are required")
	}
	sleep := t.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	observation, err := t.Inspector.Observe(ctx, id.PID)
	if err != nil {
		return TerminationResult{}, fmt.Errorf("runtime: observe process: %w", err)
	}
	if !observation.Exists {
		return TerminationResult{Absent: true, Cause: TerminationAbsent}, nil
	}
	if !sameIdentity(id, observation.ProcessIdentity) {
		return TerminationResult{Cause: TerminationIdentityUnknown}, nil
	}
	result := TerminationResult{}
	if err := t.Signaler.SignalGroup(id.PGID, syscall.SIGTERM); err != nil {
		return result, fmt.Errorf("runtime: signal process group TERM: %w", err)
	}
	result.Signals = append(result.Signals, syscall.SIGTERM)
	if err := sleep(ctx, cfg.TermGrace); err != nil {
		return result, err
	}
	if absent, err := t.absent(ctx, id, cfg, sleep); err != nil || absent {
		result.Absent, result.Cause = absent, TerminationAbsent
		return result, err
	}
	if err := t.Signaler.SignalGroup(id.PGID, syscall.SIGKILL); err != nil {
		return result, fmt.Errorf("runtime: signal process group KILL: %w", err)
	}
	result.Signals = append(result.Signals, syscall.SIGKILL)
	if err := sleep(ctx, cfg.KillGrace); err != nil {
		return result, err
	}
	absent, err := t.absent(ctx, id, cfg, sleep)
	if err != nil {
		return result, err
	}
	if absent {
		result.Absent, result.Cause = true, TerminationAbsent
		return result, nil
	}
	result.Cause = TerminationUnconfirmed
	return result, nil
}

func (t Terminator) absent(ctx context.Context, id ProcessIdentity, cfg TerminationConfig, sleep func(context.Context, time.Duration) error) (bool, error) {
	for i := 0; i < cfg.AbsenceRechecks; i++ {
		observation, err := t.Inspector.Observe(ctx, id.PID)
		if err != nil {
			return false, fmt.Errorf("runtime: recheck process: %w", err)
		}
		if !observation.Exists {
			return true, nil
		}
		// A changed identity is not proof that the recorded process group is
		// gone, so fail closed instead of treating PID reuse as absence.
		if !sameIdentity(id, observation.ProcessIdentity) {
			return false, nil
		}
		if i+1 < cfg.AbsenceRechecks {
			if err := sleep(ctx, cfg.RecheckInterval); err != nil {
				return false, err
			}
		}
	}
	return false, nil
}

func validTermination(id ProcessIdentity, cfg TerminationConfig) error {
	if id.PID <= 0 || id.PGID <= 0 || id.StartedAtMS <= 0 || id.Executable == "" || id.ControlNonceHash == "" || cfg.TermGrace < 0 || cfg.KillGrace < 0 || cfg.AbsenceRechecks < 1 || cfg.RecheckInterval < 0 {
		return ErrInvalidTerminationConfig
	}
	return nil
}
func sameIdentity(want, got ProcessIdentity) bool {
	return want.PID == got.PID && want.PGID == got.PGID && want.StartedAtMS == got.StartedAtMS &&
		want.Executable == got.Executable && len(want.ControlNonceHash) == len(got.ControlNonceHash) &&
		subtle.ConstantTimeCompare([]byte(want.ControlNonceHash), []byte(got.ControlNonceHash)) == 1
}
func sleepContext(ctx context.Context, d time.Duration) error {
	if d == 0 {
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
