package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/miaoxiaoyong/sift/internal/worktree"

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
		if disposition, err := c.resolveLateFact(ctx, attempt); err != nil {
			return err
		} else if disposition == storage.AttemptRaceSupersededByFact || disposition == storage.AttemptRaceDuplicate {
			continue
		}
		// A matching owner is still the single owner. Recovery must resume its
		// handoff/supervision rather than kill it merely because siftd restarted.
		// Running attempts additionally require a fresh heartbeat; a stale one
		// enters the same controlled-termination path as supervisor timeout.
		live, err := c.ownerIsLive(ctx, attempt)
		if err != nil {
			return err
		}
		if live && (attempt.Phase == "starting" || attempt.Phase == "spawning" || (attempt.Phase == "running" && !c.heartbeatStale(attempt))) {
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

func (c *TerminationCoordinator) heartbeatStale(attempt storage.RecoveryAttempt) bool {
	if c.Runtime.HeartbeatStaleAfter <= 0 {
		return false
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	return attempt.HeartbeatAtMS == 0 || attempt.HeartbeatAtMS < now().Add(-c.Runtime.HeartbeatStaleAfter).UnixMilli()
}

func (c *TerminationCoordinator) ownerIsLive(ctx context.Context, attempt storage.RecoveryAttempt) (bool, error) {
	if c.Terminator.Inspector == nil || attempt.WrapperPID <= 0 || attempt.WrapperStartedAtMS <= 0 || attempt.WrapperExecutable == "" || attempt.WrapperPGID <= 0 || attempt.ControlNonceHash == "" {
		return false, nil
	}
	want := runtimepkg.ProcessIdentity{PID: attempt.WrapperPID, StartedAtMS: attempt.WrapperStartedAtMS, Executable: attempt.WrapperExecutable, PGID: attempt.WrapperPGID, ControlNonceHash: attempt.ControlNonceHash, ControlPath: c.controlPath(attempt)}
	got, err := c.Terminator.Inspector.Observe(ctx, want)
	if err != nil || !got.Exists {
		return false, err
	}
	return got.PID == want.PID && got.StartedAtMS == want.StartedAtMS && got.Executable == want.Executable && got.PGID == want.PGID && got.ControlNonceHash == want.ControlNonceHash, nil
}

func (c *TerminationCoordinator) controlPath(attempt storage.RecoveryAttempt) string {
	if c.ControlRoot == "" {
		return ""
	}
	return filepath.Join(c.ControlRoot, "runs", attempt.RunID, "attempts", strconv.Itoa(attempt.AttemptNo), "control.json")
}

func (c *TerminationCoordinator) resultPath(a storage.RecoveryAttempt) string {
	if c.ControlRoot == "" {
		return ""
	}
	return filepath.Join(c.ControlRoot, "runs", a.RunID, "attempts", strconv.Itoa(a.AttemptNo), "result.json")
}

// resolveLateFact consumes durable wrapper evidence before recovery attempts
// termination. Both recovery-started and result.json use the same arbitration
// port; a decision-first result is deliberately left for terminate below.
func (c *TerminationCoordinator) resolveLateFact(ctx context.Context, a storage.RecoveryAttempt) (string, error) {
	path := c.resultPath(a)
	if path == "" {
		return "", nil
	}
	result, err := worktree.ReadResult(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", err
		}
		// A wrapper can crash after recording the Agent identity but before
		// result.json. Recovery must still feed that started fact through the
		// same CAS arbiter rather than silently terminating it.
		var control struct {
			Agent            attemptIdentityJSON `json:"agent"`
			AgentPID         int64               `json:"agent_pid"`
			AgentStartedAtMS int64               `json:"agent_started_at_ms"`
			AgentExecutable  string              `json:"agent_executable"`
		}
		data, readErr := os.ReadFile(c.controlPath(a))
		if readErr != nil {
			return "", nil
		}
		if json.Unmarshal(data, &control) != nil {
			return "", nil
		}
		agent := storage.AgentIdentity{PID: control.Agent.PID, StartedAtMS: control.Agent.StartedAtMS, Executable: control.Agent.Executable}
		if agent.PID == 0 {
			agent = storage.AgentIdentity{PID: control.AgentPID, StartedAtMS: control.AgentStartedAtMS, Executable: control.AgentExecutable}
		}
		if agent.PID <= 0 || agent.StartedAtMS <= 0 || agent.Executable == "" {
			return "", nil
		}
		now := time.Now
		if c.Now != nil {
			now = c.Now
		}
		return c.DB.ResolveAttemptRace(ctx, storage.AttemptRaceCommand{RunID: a.RunID, AttemptNo: a.AttemptNo, ExpectedGeneration: a.Generation, ExpectedRunVersion: a.RunVersion, FactKey: fmt.Sprintf("recovery-started:%d:%d:%s", agent.PID, agent.StartedAtMS, agent.Executable), NowMS: now().UnixMilli(), Agent: &agent})
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	exit := result.ExitCode
	fact := "late-result:" + result.Digest
	disposition, err := c.DB.ResolveAttemptRace(ctx, storage.AttemptRaceCommand{
		RunID: a.RunID, AttemptNo: a.AttemptNo, ExpectedGeneration: a.Generation,
		ExpectedRunVersion: a.RunVersion, FactKey: fact, NowMS: now().UnixMilli(),
		Agent:  &storage.AgentIdentity{PID: int64(result.Agent.PID), StartedAtMS: result.Agent.StartedAtMS, Executable: result.Agent.Executable},
		Result: &storage.AttemptResult{Agent: storage.AgentIdentity{PID: int64(result.Agent.PID), StartedAtMS: result.Agent.StartedAtMS, Executable: result.Agent.Executable}, ExitCode: exit, Signal: result.Signal, FinalHeadSHA: result.FinalHeadSHA, Digest: result.Digest, FinishedAtMS: result.FinishedAt.UnixMilli()},
	})
	return disposition, err
}

type attemptIdentityJSON struct {
	PID         int64  `json:"pid"`
	StartedAtMS int64  `json:"started_at_ms"`
	Executable  string `json:"executable"`
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
