package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	DB                    *storage.DB
	Terminator            runtimepkg.Terminator
	Runtime               config.Runtime
	ProcessGroupVerified  func(agentID string) bool
	ProcessGroupQualified func(key string) bool
	Now                   func() time.Time
	AttentionDailyQuota   map[storage.InterruptSeverity]int
	DayTimezone           string
	DailySummaryAt        string
	CriticalWindowMS      int64
	CriticalTotalLimit    int
	CriticalPerRunLimit   int
	Channels              []storage.InterruptChannel
	// HookRecheck is invoked after durable result evidence is consumed. It is
	// deliberately outside the storage transaction: capture performs read-only
	// filesystem inspection and RecordHookBaseline is the CAS write port.
	HookRecheck    func(context.Context, string, int) error
	ControlRoot    string
	TmuxPath       string
	TmuxSocketPath string
}

func (c *TerminationCoordinator) Recover(ctx context.Context) error {
	attempts, err := c.DB.RecoveryAttempts(ctx)
	if err != nil {
		return err
	}
	for _, attempt := range attempts {
		if _, err := c.recoverAttempt(ctx, attempt); err != nil {
			return err
		}
	}
	return c.RecheckHooks(ctx)
}

// RecheckHooks drains terminal receipts left by every result commit. It runs
// on every supervisor pass as well as after result consumption, making the
// completion-to-audit crash boundary replayable. Hook inspection is audit-only
// and never turns a capture failure into a lifecycle failure.
func (c *TerminationCoordinator) RecheckHooks(ctx context.Context) error {
	if c.HookRecheck == nil {
		return nil
	}
	receipts, err := c.DB.PendingHookRechecks(ctx)
	if err != nil {
		return err
	}
	for _, receipt := range receipts {
		_ = c.HookRecheck(ctx, receipt.RunID, receipt.AttemptNo)
	}
	return nil
}

// RecoverStartup is the boot-barrier coordinator. It repeatedly reads the
// attempt/launch-operation union and persists one CAS-protected classification
// for every candidate before the caller may open the launch gate.
func (c *TerminationCoordinator) RecoverStartup(ctx context.Context, bootID string) error {
	for {
		attempts, operations, err := c.DB.StartupRecoveryPending(ctx, bootID)
		if err != nil {
			return err
		}
		if len(attempts) == 0 && len(operations) == 0 {
			return nil
		}
		for _, attempt := range attempts {
			observation, action := "", ""
			if attempt.Phase == "pending" {
				observation, action = c.recoverPreparedDispatch(ctx, attempt)
			} else {
				observation, err = c.recoverAttempt(ctx, attempt)
				if err != nil {
					return err
				}
				action = "frozen"
				if observation == "owner_live" || observation == "late_fact" {
					action = "supervise"
				}
			}
			if action == "frozen" {
				// A startup freeze is not a standalone attempt update. EmitInterrupt
				// atomically freezes the attempt with its reason/timestamp, transitions
				// the Run, charges attention, and queues the visible comment.
				_, err = c.DB.RecordTerminationObservation(ctx, storage.RecordTerminationObservationCmd{
					RunID: attempt.RunID, AttemptNo: attempt.AttemptNo,
					ExpectedRunVersion: attempt.RunVersion, ExpectedGeneration: attempt.Generation,
					Source: storage.TerminationRecovery, DiagnosticCause: "process_identity_unknown",
					AttentionDailyQuota: c.AttentionDailyQuota, DayTimezone: c.DayTimezone, DailySummaryAt: c.DailySummaryAt, CriticalWindowMS: c.CriticalWindowMS, CriticalTotalLimit: c.CriticalTotalLimit, CriticalPerRunLimit: c.CriticalPerRunLimit, Channels: c.Channels, NowMS: c.nowMS(),
				})
				if err == storage.ErrRejectedStale {
					continue
				}
				if err != nil {
					return err
				}
			}
			err := c.DB.ApplyStartupRecoveryAction(ctx, storage.StartupRecoveryAction{BootID: bootID, RunID: attempt.RunID, AttemptNo: attempt.AttemptNo, ExpectedGeneration: attempt.Generation, ObservationDigest: recoveryDigest(attempt, observation), Action: action, NowMS: c.nowMS()})
			if err != nil && err != storage.ErrRejectedStale {
				return err
			}
		}
		for _, operation := range operations {
			digest := recoveryOperationDigest(operation)
			err := c.DB.ApplyStartupRecoveryAction(ctx, storage.StartupRecoveryAction{BootID: bootID, OperationID: operation.ID, ExpectedOperationVersion: operation.Version, ObservationDigest: digest, Action: "converge_operation", NowMS: c.nowMS()})
			if err != nil && err != storage.ErrRejectedStale {
				return err
			}
		}
	}
}

// recoverPreparedDispatch distinguishes the only pending-state crash windows.
// A verified bootstrap can be reused by a new lease owner. Without one, a
// missing control file proves no wrapper reached the handoff, so fencing and
// redispatch are safe. Any ambiguous file evidence freezes rather than racing
// an unknown wrapper.
func (c *TerminationCoordinator) recoverPreparedDispatch(ctx context.Context, attempt storage.RecoveryAttempt) (string, string) {
	bootstrapPath := filepath.Join(c.ControlRoot, "runs", attempt.RunID, "attempts", strconv.Itoa(attempt.AttemptNo), "bootstrap.json")
	controlPath := c.controlPath(attempt)
	data, err := safeRecoveryFile(bootstrapPath)
	if err == nil {
		var bootstrap runtimepkg.Bootstrap
		if json.Unmarshal(data, &bootstrap) == nil {
			sum := sha256.Sum256(data)
			if c.DB.ValidatePreparedBootstrap(ctx, attempt.RunID, attempt.AttemptNo, attempt.Generation, bootstrap.DispatchID, bootstrap.BootstrapNonce, bootstrap.RunToken, hex.EncodeToString(sum[:])) == nil {
				return "validated_bootstrap", "reuse_dispatch"
			}
		}
		return "invalid_bootstrap", "frozen"
	}
	if !os.IsNotExist(err) || recoveryFileExists(controlPath) {
		return "ambiguous_file_evidence", "frozen"
	}
	// bootstrap.json may already have been consumed while acquire is in flight.
	// That wrapper has no control/session/permit and cannot spawn, but it is a
	// distinct recovery observation from a dispatch that never left the worker.
	// Observe it before fencing. A matching live owner has no spawn capability,
	// so fencing it and redispatching is safe; an absent or mismatched owner is
	// ambiguous and must stay frozen.
	if attempt.WrapperPID > 0 && attempt.WrapperStartedAtMS > 0 && attempt.WrapperExecutable != "" && attempt.WrapperPGID > 0 && attempt.ControlNonceHash != "" {
		live, observeErr := c.ownerIsLive(ctx, attempt)
		if observeErr != nil || !live {
			return "preacquire_owner_unknown", "frozen"
		}
		return "preacquire_without_control", "redispatch"
	}
	return "no_bootstrap_or_control", "redispatch"
}

func safeRecoveryFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("unsafe recovery file")
	}
	return os.ReadFile(path)
}

func recoveryFileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func (c *TerminationCoordinator) recoverAttempt(ctx context.Context, attempt storage.RecoveryAttempt) (string, error) {
	if attempt.Phase == "pending" {
		return "no_execution_body", nil
	}
	if disposition, err := c.resolveLateFact(ctx, attempt); err != nil {
		return "", err
	} else if disposition == storage.AttemptRaceRunning || disposition == storage.AttemptRaceSupersededByFact || disposition == storage.AttemptRaceDuplicate {
		return "late_fact", nil
	}
	live, err := c.ownerIsLive(ctx, attempt)
	if err != nil {
		return "", err
	}
	if err := c.recordBackendSessionDiagnostic(ctx, attempt, live); err != nil {
		return "", err
	}
	if live && (attempt.Phase == "starting" || attempt.Phase == "spawning" || (attempt.Phase == "running" && !c.heartbeatStale(attempt))) {
		return "owner_live", nil
	}
	if err := c.terminate(ctx, attempt, storage.TerminationRecovery, attempt.RunVersion); err != nil {
		return "", err
	}
	return "owner_not_live", nil
}

func (c *TerminationCoordinator) nowMS() int64 {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	return now().UnixMilli()
}

func recoveryDigest(a storage.RecoveryAttempt, observation string) string {
	b, _ := json.Marshal(struct {
		Attempt     storage.RecoveryAttempt `json:"attempt"`
		Observation string                  `json:"observation"`
	}{a, observation})
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func recoveryOperationDigest(o storage.RecoveryLaunchOperation) string {
	b, _ := json.Marshal(o)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
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

// recordBackendSessionDiagnostic observes tmux only after deriving the exact
// durable binding. It appends explanatory evidence and deliberately has no
// attempt/claim/owner/replacement write path.
func (c *TerminationCoordinator) recordBackendSessionDiagnostic(ctx context.Context, attempt storage.RecoveryAttempt, wrapperLive bool) error {
	if attempt.Backend != "tmux" || attempt.DispatchID == "" || c.TmuxPath == "" || c.TmuxSocketPath == "" {
		return nil
	}
	name, err := runtimepkg.TmuxSessionName(attempt.RunID, attempt.AttemptNo, attempt.Generation, attempt.DispatchID)
	if err != nil {
		return nil
	}
	observation := runtimepkg.ObserveBackendSession(ctx, c.TmuxPath, c.TmuxSocketPath, name, name[len("sift-"):])
	code := ""
	switch {
	case observation.State == runtimepkg.SessionPresent && !wrapperLive:
		code = "backend_session_present_wrapper_absent"
	case observation.State == runtimepkg.SessionAbsent && wrapperLive:
		code = "backend_session_lost"
	default:
		return nil
	}
	payload, _ := json.Marshal(struct {
		Backend        string `json:"backend"`
		State          string `json:"state"`
		DiagnosticCode string `json:"diagnostic_code"`
		BindingDigest  string `json:"binding_digest"`
	}{observation.Backend, string(observation.State), code, observation.BindingDigest})
	attemptNo := attempt.AttemptNo
	_, err = c.DB.AppendEvent(ctx, storage.EventCmd{RunID: attempt.RunID, AttemptNo: &attemptNo, Type: "backend.session_diagnostic", Source: storage.SourceRecovery, PayloadJSON: payload, IdempotencyKey: fmt.Sprintf("backend-session:%s:%d:%d:%s", attempt.RunID, attempt.AttemptNo, attempt.Generation, code), OccurredAtMS: c.nowMS(), RecordedAtMS: c.nowMS()})
	return err
}

func (c *TerminationCoordinator) processGroupQualified(attempt storage.RecoveryAttempt) bool {
	if c.ProcessGroupQualified != nil && attempt.TopologyQualificationKey != "" {
		return c.ProcessGroupQualified(attempt.TopologyQualificationKey)
	}
	return c.ProcessGroupVerified != nil && c.ProcessGroupVerified(attempt.AgentID)
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

func (c *TerminationCoordinator) qualificationInvalidated(a storage.RecoveryAttempt) bool {
	if c.ControlRoot == "" {
		return false
	}
	_, err := safeRecoveryFile(filepath.Join(c.ControlRoot, "runs", a.RunID, "attempts", strconv.Itoa(a.AttemptNo), "qualification-invalid"))
	// A malformed marker is also fail-closed; only its complete absence permits
	// a qualification row to participate in recovery.
	return err == nil || !os.IsNotExist(err)
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
			AgentIdentity    attemptIdentityJSON `json:"agent_identity"`
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
		agent := storage.AgentIdentity{PID: control.AgentIdentity.PID, StartedAtMS: control.AgentIdentity.StartedAtMS, Executable: control.AgentIdentity.Executable}
		if agent.PID == 0 {
			agent = storage.AgentIdentity{PID: control.Agent.PID, StartedAtMS: control.Agent.StartedAtMS, Executable: control.Agent.Executable}
		}
		if agent.PID == 0 {
			agent = storage.AgentIdentity{PID: control.AgentPID, StartedAtMS: control.AgentStartedAtMS, Executable: control.AgentExecutable}
		}
		if agent.PID <= 0 || agent.StartedAtMS <= 0 || agent.Executable == "" || (hasDurableAgent(a) && !matchesDurableAgent(a, agent)) {
			return "", nil
		}
		if a.Phase == "spawning" {
			live, observeErr := c.agentIsLive(ctx, agent)
			if observeErr != nil || !live {
				return "", observeErr
			}
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
	agent := storage.AgentIdentity{PID: int64(result.Agent.PID), StartedAtMS: result.Agent.StartedAtMS, Executable: result.Agent.Executable}
	if hasDurableAgent(a) && !matchesDurableAgent(a, agent) {
		return "", nil
	}
	exit := result.ExitCode
	fact := "late-result:" + result.Digest
	disposition, err := c.DB.ResolveAttemptRace(ctx, storage.AttemptRaceCommand{
		RunID: a.RunID, AttemptNo: a.AttemptNo, ExpectedGeneration: a.Generation,
		ExpectedRunVersion: a.RunVersion, FactKey: fact, NowMS: now().UnixMilli(),
		Agent:  &agent,
		Result: &storage.AttemptResult{Agent: agent, ExitCode: exit, Signal: result.Signal, FailureReason: result.FailureReason, FinalHeadSHA: result.FinalHeadSHA, Digest: result.Digest, FinishedAtMS: result.FinishedAt.UnixMilli()},
	})
	if err == nil && c.HookRecheck != nil {
		// Hooks are audit evidence only. A bad path must not reclassify a
		// completed attempt or fail the daemon's recovery loop.
		_ = c.HookRecheck(ctx, a.RunID, a.AttemptNo)
	}
	return disposition, err
}

type attemptIdentityJSON struct {
	PID         int64  `json:"pid"`
	StartedAtMS int64  `json:"started_at_ms"`
	Executable  string `json:"executable"`
}

func hasDurableAgent(a storage.RecoveryAttempt) bool {
	return a.AgentPID > 0 && a.AgentStartedAtMS > 0 && a.AgentExecutable != ""
}

func matchesDurableAgent(a storage.RecoveryAttempt, agent storage.AgentIdentity) bool {
	return int64(a.AgentPID) == agent.PID && a.AgentStartedAtMS == agent.StartedAtMS && a.AgentExecutable == agent.Executable
}

// agentIsLive validates the independently persisted Agent identity before a
// missing started response may be recovered as running. Agent identity has no
// control nonce: that nonce belongs only to its wrapper.
func (c *TerminationCoordinator) agentIsLive(ctx context.Context, agent storage.AgentIdentity) (bool, error) {
	if c.Terminator.Inspector == nil {
		return false, nil
	}
	got, err := c.Terminator.Inspector.Observe(ctx, runtimepkg.ProcessIdentity{PID: int(agent.PID), StartedAtMS: agent.StartedAtMS, Executable: agent.Executable})
	if err != nil || !got.Exists {
		return false, err
	}
	return got.PID == int(agent.PID) && got.StartedAtMS == agent.StartedAtMS && got.Executable == agent.Executable, nil
}

func (c *TerminationCoordinator) terminate(ctx context.Context, attempt storage.RecoveryAttempt, source storage.TerminationSource, expectedVersion int64) error {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	cmd := storage.RecordTerminationObservationCmd{RunID: attempt.RunID, AttemptNo: attempt.AttemptNo, ExpectedRunVersion: expectedVersion, ExpectedGeneration: attempt.Generation, Source: source, NowMS: now().UnixMilli(), AttentionDailyQuota: c.AttentionDailyQuota, DayTimezone: c.DayTimezone, DailySummaryAt: c.DailySummaryAt, CriticalWindowMS: c.CriticalWindowMS, CriticalTotalLimit: c.CriticalTotalLimit, CriticalPerRunLimit: c.CriticalPerRunLimit, Channels: c.Channels}
	if c.qualificationInvalidated(attempt) {
		cmd.DiagnosticCause = "process_group_unverified"
		_, err := c.DB.RecordTerminationObservation(ctx, cmd)
		return err
	}
	identity := runtimepkg.ProcessIdentity{PID: attempt.WrapperPID, StartedAtMS: attempt.WrapperStartedAtMS, Executable: attempt.WrapperExecutable, PGID: attempt.WrapperPGID, ControlNonceHash: attempt.ControlNonceHash, ControlPath: c.controlPath(attempt)}
	if identity.PID <= 0 || identity.StartedAtMS <= 0 || identity.Executable == "" || identity.PGID <= 0 || identity.ControlNonceHash == "" {
		// A missing wrapper identity is not absence proof. When this attempt has
		// an exact but unverified topology, name that stronger fail-closed cause
		// so detached descendants cannot be retried through an identity fallback.
		if attempt.TopologyQualificationKey != "" && !c.processGroupQualified(attempt) {
			cmd.DiagnosticCause = "process_group_unverified"
		} else {
			cmd.DiagnosticCause = "process_identity_unknown"
		}
		_, err := c.DB.RecordTerminationObservation(ctx, cmd)
		return err
	}
	result, err := c.Terminator.Terminate(ctx, identity, runtimepkg.TerminationConfig{TermGrace: c.Runtime.TerminationTermGrace, KillGrace: c.Runtime.TerminationKillGrace, AbsenceRechecks: c.Runtime.AbsenceRecheckCount, RecheckInterval: c.Runtime.AbsenceRecheckInterval})
	if err != nil {
		return fmt.Errorf("controlled termination: %w", err)
	}
	qualified := c.processGroupQualified(attempt)
	if result.Absent && qualified {
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
