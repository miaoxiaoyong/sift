package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
	runtimepkg "github.com/miaoxiaoyong/sift/internal/runtime"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// TerminationCoordinator is the only application-level bridge from recovery,
// timeout and operator requests to controlled termination. Process IO happens
// before the storage port; every outcome, including an identity failure, is
// persisted through RecordTerminationObservation.
type TerminationCoordinator struct {
	DB                   *storage.DB
	Terminator           runtimepkg.Terminator
	Runtime              config.Runtime
	ProcessGroupVerified func(agentID string) bool
	Now                  func() time.Time
	AttentionDailyQuota  map[storage.InterruptSeverity]int
	DayTimezone          string
	ControlRoot          string
}

func (c *TerminationCoordinator) Recover(ctx context.Context) error {
	attempts, err := c.DB.RecoveryAttempts(ctx)
	if err != nil {
		return err
	}
	for _, attempt := range attempts {
		// pending attempts have no execution body. They are intentionally left
		// for the launch recovery path; only possible owners enter termination.
		if attempt.Phase == "pending" {
			continue
		}
		if err := c.terminate(ctx, attempt, storage.TerminationRecovery, attempt.RunVersion); err != nil {
			return err
		}
	}
	return nil
}

// Timeout scans persisted heartbeat facts; it never infers an orphan from a
// lease or an expired heartbeat alone.
func (c *TerminationCoordinator) Timeout(ctx context.Context) error {
	if c.Runtime.HeartbeatStaleAfter <= 0 {
		return nil
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	attempts, err := c.DB.StaleHeartbeatAttempts(ctx, now().Add(-c.Runtime.HeartbeatStaleAfter).UnixMilli())
	if err != nil {
		return err
	}
	for _, attempt := range attempts {
		if err := c.terminate(ctx, attempt, storage.TerminationRecovery, attempt.RunVersion); err != nil {
			return err
		}
	}
	return nil
}

func (c *TerminationCoordinator) Operator(ctx context.Context, runID string, expectedVersion int64, retry bool) error {
	attempt, err := c.DB.RecoveryAttemptForRun(ctx, runID)
	if err != nil {
		return err
	}
	if attempt.RunVersion != expectedVersion {
		return storage.ErrRejectedStale
	}
	source := storage.TerminationKill
	if retry {
		source = storage.TerminationRetry
	}
	return c.terminate(ctx, attempt, source, expectedVersion)
}

func (c *TerminationCoordinator) controlPath(attempt storage.RecoveryAttempt) string {
	if c.ControlRoot == "" {
		return ""
	}
	return filepath.Join(c.ControlRoot, "runs", attempt.RunID, "attempts", strconv.Itoa(attempt.AttemptNo), "control.json")
}

func (c *TerminationCoordinator) terminate(ctx context.Context, attempt storage.RecoveryAttempt, source storage.TerminationSource, expectedVersion int64) error {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	cmd := storage.RecordTerminationObservationCmd{RunID: attempt.RunID, AttemptNo: attempt.AttemptNo, ExpectedRunVersion: expectedVersion, ExpectedGeneration: attempt.Generation, Source: source, NowMS: now().UnixMilli(), AttentionDailyQuota: c.AttentionDailyQuota, DayTimezone: c.DayTimezone}
	identity := runtimepkg.ProcessIdentity{PID: attempt.WrapperPID, StartedAtMS: attempt.WrapperStartedAtMS, Executable: attempt.WrapperExecutable, PGID: attempt.WrapperPGID, ControlNonceHash: attempt.ControlNonceHash, ControlPath: c.controlPath(attempt)}
	if identity.PID <= 0 || identity.StartedAtMS <= 0 || identity.Executable == "" || identity.PGID <= 0 || identity.ControlNonceHash == "" {
		cmd.DiagnosticCause = "process_identity_unknown"
		_, err := c.DB.RecordTerminationObservation(ctx, cmd)
		return err
	}
	result, err := c.Terminator.Terminate(ctx, identity, runtimepkg.TerminationConfig{TermGrace: c.Runtime.TerminationTermGrace, KillGrace: c.Runtime.TerminationKillGrace, AbsenceRechecks: c.Runtime.AbsenceRecheckCount, RecheckInterval: c.Runtime.AbsenceRecheckInterval})
	if err != nil {
		return fmt.Errorf("controlled termination: %w", err)
	}
	if result.Absent && c.ProcessGroupVerified != nil && c.ProcessGroupVerified(attempt.AgentID) {
		cmd.Absent, cmd.Evidence = true, "verified process group absent"
	} else if result.Cause == runtimepkg.TerminationIdentityUnknown {
		cmd.DiagnosticCause = "process_identity_unknown"
	} else if result.Absent {
		// Group disappearance is not execution-body proof until an Agent/version
		// qualification explicitly says it is.
		cmd.DiagnosticCause = "process_group_unverified"
	} else {
		cmd.DiagnosticCause = "termination_unconfirmed"
	}
	_, err = c.DB.RecordTerminationObservation(ctx, cmd)
	return err
}
