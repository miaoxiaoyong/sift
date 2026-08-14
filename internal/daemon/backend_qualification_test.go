package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xsift/sift/internal/config"
	"github.com/xsift/sift/internal/launchworker"
	runtimepkg "github.com/xsift/sift/internal/runtime"
	"github.com/xsift/sift/internal/storage"
)

func tmuxObservationFixture(t *testing.T, mode string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tmux")
	script := "#!/bin/sh\ncase \"$5\" in\nhas-session) "
	if mode == "absent" {
		script += "printf \"can't find session: %s\\n\" \"$7\" >&2; exit 1 ;;\n"
	} else {
		script += "exit 0 ;;\n"
	}
	script += "show-environment) printf 'SIFT_TMUX_BINDING=%s\\n' \"${7#=sift-}\" ;;\nlist-panes) printf '0\\n' ;;\nshow-options) printf 'off\\n' ;;\n*) exit 99 ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRecoveryTmuxSessionPresentWrapperAbsentDiagnostic(t *testing.T) {
	db, raw, _, now := seedRecoveryCoordinator(t, "running", 0)
	if _, err := raw.Exec(`UPDATE attempts SET backend='tmux' WHERE run_id='run'; UPDATE attempt_claims SET dispatch_id='dispatch',bootstrap_nonce_hash='nonce',run_token_hash='token' WHERE run_id='run'`); err != nil {
		t.Fatal(err)
	}
	before := recoveryExecutionProjection(t, raw)
	coordinator := &TerminationCoordinator{DB: db, Terminator: runtimepkg.Terminator{Inspector: recoveryInspector{}, Signaler: &recoverySignaler{}}, Runtime: config.Runtime{AbsenceRecheckCount: 1}, AttentionDailyQuota: recoveryQuota(), TmuxPath: tmuxObservationFixture(t, "present"), TmuxSocketPath: filepath.Join(t.TempDir(), "socket"), Now: func() time.Time { return now }}
	if err := coordinator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSessionDiagnostic(t, raw, "backend_session_present_wrapper_absent")
	if after := recoveryExecutionProjection(t, raw); after != before {
		t.Fatalf("session diagnostic rewrote execution evidence: before=%q after=%q", before, after)
	}
}

func TestRecoveryTmuxSessionAbsentWrapperPresentDiagnostic(t *testing.T) {
	db, raw, attempt, now := seedRecoveryCoordinator(t, "running", 0)
	if _, err := raw.Exec(`UPDATE attempts SET backend='tmux' WHERE run_id='run'; UPDATE attempt_claims SET dispatch_id='dispatch',bootstrap_nonce_hash='nonce',run_token_hash='token' WHERE run_id='run'`); err != nil {
		t.Fatal(err)
	}
	before := recoveryExecutionProjection(t, raw)
	coordinator := &TerminationCoordinator{DB: db, Terminator: runtimepkg.Terminator{Inspector: recoveryInspector{observation: runtimepkg.ProcessObservation{Exists: true, ProcessIdentity: runtimepkg.ProcessIdentity{PID: attempt.WrapperPID, StartedAtMS: attempt.WrapperStartedAtMS, Executable: attempt.WrapperExecutable, PGID: attempt.WrapperPGID, ControlNonceHash: attempt.ControlNonceHash}}}}, AttentionDailyQuota: recoveryQuota(), TmuxPath: tmuxObservationFixture(t, "absent"), TmuxSocketPath: filepath.Join(t.TempDir(), "socket"), Now: func() time.Time { return now }}
	if err := coordinator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSessionDiagnostic(t, raw, "backend_session_lost")
	if after := recoveryExecutionProjection(t, raw); after != before {
		t.Fatalf("session loss rewrote execution evidence: before=%q after=%q", before, after)
	}
}

func recoveryExecutionProjection(t *testing.T, raw *sql.DB) string {
	t.Helper()
	var started, finished, owner, replacement sql.NullString
	if err := raw.QueryRow(`SELECT CAST(agent_started_at_ms AS TEXT),CAST(finished_at_ms AS TEXT),wrapper_instance_id,(SELECT CAST(attempt_no AS TEXT) FROM attempts WHERE run_id='run' ORDER BY attempt_no DESC LIMIT 1) FROM attempts WHERE run_id='run' AND attempt_no=1`).Scan(&started, &finished, &owner, &replacement); err != nil {
		t.Fatal(err)
	}
	return strings.Join([]string{started.String, finished.String, owner.String, replacement.String}, "|")
}

func assertSessionDiagnostic(t *testing.T, raw *sql.DB, code string) {
	t.Helper()
	var payload string
	if err := raw.QueryRow(`SELECT payload_json FROM events WHERE type='backend.session_diagnostic'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(payload), &got); err != nil || got["diagnostic_code"] != code {
		t.Fatalf("diagnostic=%q parsed=%v err=%v, want %s", payload, got, err, code)
	}
}

func TestQualificationRecoveryVerifiedAbsenceConfirmed(t *testing.T) {
	db, raw, _, now := seedRecoveryCoordinator(t, "running", 0)
	q := detachedQualification(t, runtimepkg.QualificationEvidence{Status: runtimepkg.ProcessGroupUnverified, Reason: "detached_descendant"})
	q.ID, q.Status, q.Reason = "verified", "process-group-verified", "qualified"
	var err error
	q.EvidenceJSON, err = storage.TopologyQualificationEvidenceJSON(q)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordTopologyQualification(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE attempts SET topology_qualification_key=? WHERE run_id='run'`, q.QualificationKey); err != nil {
		t.Fatal(err)
	}
	coordinator := &TerminationCoordinator{DB: db, Terminator: runtimepkg.Terminator{Inspector: recoveryInspector{}, Signaler: &recoverySignaler{}}, Runtime: config.Runtime{AbsenceRecheckCount: 1}, ProcessGroupQualified: func(key string) bool { ok, _ := db.ProcessGroupQualified(context.Background(), key); return ok }, AttentionDailyQuota: recoveryQuota(), Now: func() time.Time { return now }}
	if err := coordinator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	var phase string
	var stalls int
	if err := raw.QueryRow(`SELECT phase FROM attempts WHERE run_id='run' AND attempt_no=1`).Scan(&phase); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM interrupts WHERE run_id='run' AND reason='startup_stall'`).Scan(&stalls); err != nil {
		t.Fatal(err)
	}
	if phase != "orphaned" || stalls != 0 {
		t.Fatalf("verified absence phase=%q startup_stalls=%d", phase, stalls)
	}
}

func TestQualificationBinaryReplacementBetweenMeasurementAndAgentExecFailsClosed(t *testing.T) {
	db, raw, _, now := seedRecoveryCoordinator(t, "running", 0)
	q := detachedQualification(t, runtimepkg.QualificationEvidence{Status: runtimepkg.ProcessGroupUnverified, Reason: "detached_descendant"})
	q.ID, q.Status, q.Reason = "verified", "process-group-verified", "qualified"
	var err error
	q.EvidenceJSON, err = storage.TopologyQualificationEvidenceJSON(q)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordTopologyQualification(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE attempts SET topology_qualification_key=? WHERE run_id='run'`, q.QualificationKey); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	marker := filepath.Join(root, "runs", "run", "attempts", "1", "qualification-invalid")
	if err := os.MkdirAll(filepath.Dir(marker), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte(q.ExecutableSHA256), 0600); err != nil {
		t.Fatal(err)
	}
	coordinator := &TerminationCoordinator{DB: db, ControlRoot: root, Terminator: runtimepkg.Terminator{Inspector: recoveryInspector{}, Signaler: &recoverySignaler{}}, Runtime: config.Runtime{AbsenceRecheckCount: 1}, ProcessGroupQualified: func(key string) bool { ok, _ := db.ProcessGroupQualified(context.Background(), key); return ok }, AttentionDailyQuota: recoveryQuota(), Now: func() time.Time { return now }}
	if err := coordinator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	var phase string
	var stalls int
	if err := raw.QueryRow(`SELECT phase FROM attempts WHERE run_id='run' AND attempt_no=1`).Scan(&phase); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM interrupts WHERE run_id='run' AND reason='startup_stall'`).Scan(&stalls); err != nil {
		t.Fatal(err)
	}
	if phase == "orphaned" || stalls != 1 {
		t.Fatalf("invalidated qualification recovery phase=%q startup_stalls=%d", phase, stalls)
	}
}

func TestDetachedDescendantIsUnverifiedAndCannotRetry(t *testing.T) {
	observation := detachedTopologyObservation(t)
	evidence, err := runtimepkg.ClassifyDetachedDescendant(observation)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Status != runtimepkg.ProcessGroupUnverified || evidence.Reason != "detached_descendant" {
		t.Fatalf("detached topology evidence = %#v", evidence)
	}

	db, raw, _, now := seedRecoveryCoordinator(t, "running", 0)
	q := detachedQualification(t, evidence)
	if err := db.RecordTopologyQualification(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	var status, reason string
	if err := raw.QueryRow(`SELECT status,reason FROM agent_topology_qualifications WHERE qualification_key=?`, q.QualificationKey).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != string(evidence.Status) || reason != evidence.Reason {
		t.Fatalf("persisted detached qualification = %s/%s, want %s/%s", status, reason, evidence.Status, evidence.Reason)
	}
	// The production recovery candidate has no surviving wrapper or PGID. The
	// topology fixture above is the evidence source for this exact key; recovery
	// must nevertheless remain frozen rather than treating that absence as retry.
	if _, err := raw.Exec(`UPDATE attempts SET topology_qualification_key=?,wrapper_pid=NULL,wrapper_started_at_ms=NULL,wrapper_executable=NULL,wrapper_pgid=NULL,wrapper_instance_id=NULL,control_nonce_hash=NULL WHERE run_id='run'`, q.QualificationKey); err != nil {
		t.Fatal(err)
	}
	boot, err := db.StartDaemonBoot(context.Background(), "hash-cfg", "test", 1, 1, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &TerminationCoordinator{DB: db, ProcessGroupQualified: func(key string) bool { ok, _ := db.ProcessGroupQualified(context.Background(), key); return ok }, AttentionDailyQuota: recoveryQuota(), Now: func() time.Time { return now }}
	if err := coordinator.RecoverStartup(context.Background(), boot); err != nil {
		t.Fatal(err)
	}
	var state, isolationReason string
	var attempts, stalls int
	if err := raw.QueryRow(`SELECT isolation_state,isolation_reason FROM attempts WHERE run_id='run' AND attempt_no=1`).Scan(&state, &isolationReason); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM attempts WHERE run_id='run'`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM interrupts WHERE run_id='run' AND reason='startup_stall' AND status='open'`).Scan(&stalls); err != nil {
		t.Fatal(err)
	}
	if state != "frozen" || isolationReason != "process_group_unverified" || attempts != 1 || stalls != 1 {
		t.Fatalf("detached recovery = state=%s reason=%s attempts=%d stalls=%d", state, isolationReason, attempts, stalls)
	}
}

// TestQualificationBinaryReplacementSuccessorDispatchAndRecovery follows the
// production absence successor through launch preparation, a lease replay, and
// recovery. Replacing bytes at the frozen executable path must not let either
// the old key or its verified evidence authorize the successor.
func TestQualificationBinaryReplacementSuccessorDispatchAndRecovery(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(10_000)
	root := t.TempDir()
	agentPath := filepath.Join(root, "agent")
	writeAgent := func(version string) {
		t.Helper()
		if err := os.WriteFile(agentPath, []byte("#!/bin/sh\nprintf '"+version+"\\n'\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	writeAgent("old")
	agent := config.Agent{ID: "agent", Executable: agentPath, TaskTransport: config.TaskTransportStdin, Backend: config.BackendProcess}
	frozen := &config.Config{Version: 1, Agents: []config.Agent{agent}, Projects: []config.Project{{ID: "project", Repo: root, Enabled: true, Forge: config.ForgeRef{Kind: "github", Host: "github.com", Project: "org/repo"}}}}
	canonical, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(root, "sift.db"), BinaryVersion: "test", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ActivateConfig(ctx, &config.Snapshot{Config: frozen, Hash: "qualification-frozen", CanonicalJSON: canonical}, "test", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	snapshotID, err := db.ConfigSnapshotID(ctx, "qualification-frozen")
	if err != nil {
		t.Fatal(err)
	}
	created, err := db.CreateForgeRun(ctx, storage.CreateForgeRunCmd{RunID: "run", ProjectID: "project", ConfigSnapshotID: snapshotID, ForgeKind: "github", ForgeHost: "github.com", ForgeProjectKey: "org/repo", IssueID: "issue", TriggerLabelEventID: "event", TriggerActor: "operator", TriggerObservedAtMS: now.UnixMilli(), CreatedAtMS: now.UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetInitialTaskSpec(ctx, storage.SetInitialTaskSpecCmd{RunID: "run", ExpectedVersion: created.Version, TaskSpecID: "task", CanonicalJSON: []byte(`{"title":"qualification"}`), ContentDigest: "task-digest", Kind: "bug", AgentID: agent.ID, OccurredAtMS: now.UnixMilli(), InitialAttempt: &storage.InitialAttemptSpec{WorktreePath: root, BranchName: "branch", BaseRef: "main", BaseSHA: "base"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordHookBaseline(ctx, storage.RecordHookBaselineCmd{ProjectID: "project", Snapshot: storage.HookBaselineSnapshot{GitConfigDigest: "initial-config", EffectiveHooksPath: "/initial-hooks", HooksDirectoryDigest: "initial-directory", Digest: "initial"}, CapturedAtMS: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	boot, err := db.StartDaemonBoot(ctx, "qualification-frozen", "test", 1, 1, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	openQualificationLaunchGate(t, db, boot, now.UnixMilli()+1)

	oldQualification, err := runtimepkg.BuildQualification(runtimepkg.QualificationInput{AgentID: agent.ID, TaskTransport: string(agent.TaskTransport), Executable: agent.Executable})
	if err != nil {
		t.Fatal(err)
	}
	initialHost := &qualificationRecordingBackend{}
	initialWorker := &launchworker.Worker{DB: db, BootID: boot, WorkerID: "initial", Root: root, Lease: time.Millisecond, Now: func() time.Time { return now.Add(10 * time.Millisecond) }, Backends: launchworker.BackendRouter{config.BackendProcess: initialHost}, FrozenAgentsRequired: true}
	if err := initialWorker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(initialHost.calls) != 1 || initialHost.calls[0].AttemptNo != 1 {
		t.Fatalf("initial production dispatch = %#v", initialHost.calls)
	}
	if err := db.RecordTopologyQualification(ctx, topologyQualificationRecord(t, oldQualification, "process-group-verified", "qualified", now.UnixMilli()+11)); err != nil {
		t.Fatal(err)
	}
	run, err := db.Run(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordTerminationObservation(ctx, storage.RecordTerminationObservationCmd{RunID: "run", AttemptNo: 1, ExpectedRunVersion: run.Version, ExpectedGeneration: 1, Source: storage.TerminationRetry, Absent: true, Evidence: "verified fixture absence", NowMS: now.UnixMilli() + 12}); err != nil {
		t.Fatal(err)
	}
	var successorKey string
	if err := db.QueryRowForTest(ctx, `SELECT COALESCE(topology_qualification_key,'') FROM attempts WHERE run_id='run' AND attempt_no=2`).Scan(&successorKey); err != nil {
		t.Fatal(err)
	}
	if successorKey != "" {
		t.Fatalf("absence successor inherited old qualification key %q", successorKey)
	}

	writeAgent("new")
	newQualification, err := runtimepkg.BuildQualification(runtimepkg.QualificationInput{AgentID: agent.ID, TaskTransport: string(agent.TaskTransport), Executable: agent.Executable})
	if err != nil {
		t.Fatal(err)
	}
	if newQualification.Key == oldQualification.Key {
		t.Fatal("binary replacement retained qualification key")
	}
	responseLost := errors.New("response lost after production dispatch")
	preparedHost := &qualificationRecordingBackend{err: responseLost}
	preparedWorker := &launchworker.Worker{DB: db, BootID: boot, WorkerID: "prepared", Root: root, Lease: time.Millisecond, Now: func() time.Time { return now.Add(20 * time.Millisecond) }, Backends: launchworker.BackendRouter{config.BackendProcess: preparedHost}, FrozenAgentsRequired: true}
	if err := preparedWorker.RunOnce(ctx); !errors.Is(err, responseLost) {
		t.Fatalf("prepare successor = %v, want response loss", err)
	}
	if err := db.QueryRowForTest(ctx, `SELECT topology_qualification_key FROM attempts WHERE run_id='run' AND attempt_no=2`).Scan(&successorKey); err != nil || successorKey != newQualification.Key {
		t.Fatalf("successor dispatch qualification key=%q err=%v, want %q", successorKey, err, newQualification.Key)
	}
	var successorDispatch string
	if err := db.QueryRowForTest(ctx, `SELECT dispatch_id FROM attempt_claims WHERE run_id='run' AND attempt_no=2`).Scan(&successorDispatch); err != nil || successorDispatch == "" {
		t.Fatalf("successor dispatch id=%q err=%v", successorDispatch, err)
	}

	replayHost := &qualificationRecordingBackend{}
	replayWorker := &launchworker.Worker{DB: db, BootID: boot, WorkerID: "replay", Root: root, Lease: time.Millisecond, Now: func() time.Time { return now.Add(30 * time.Millisecond) }, Backends: launchworker.BackendRouter{config.BackendProcess: replayHost}, FrozenAgentsRequired: true}
	if err := replayWorker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(replayHost.calls) != 1 || replayHost.calls[0].AttemptNo != 2 || replayHost.calls[0].DispatchID != successorDispatch {
		t.Fatalf("replayed successor launch = %#v, want dispatch %q", replayHost.calls, successorDispatch)
	}
	if err := db.QueryRowForTest(ctx, `SELECT topology_qualification_key FROM attempts WHERE run_id='run' AND attempt_no=2`).Scan(&successorKey); err != nil || successorKey != newQualification.Key {
		t.Fatalf("replayed successor qualification key=%q err=%v, want %q", successorKey, err, newQualification.Key)
	}
	// The recording host accepted the production launch without a real wrapper;
	// make the successor an active recovery candidate while deliberately leaving
	// wrapper/PGID evidence absent.
	if _, err := db.ExecForTest(ctx, `UPDATE attempts SET phase='running',agent_pid=22,agent_started_at_ms=10030,agent_executable='/fixture/agent' WHERE run_id='run' AND attempt_no=2`); err != nil {
		t.Fatal(err)
	}

	var recoveredKeys []string
	coordinator := &TerminationCoordinator{DB: db, ProcessGroupQualified: func(key string) bool {
		recoveredKeys = append(recoveredKeys, key)
		ok, err := db.ProcessGroupQualified(ctx, key)
		return err == nil && ok
	}, AttentionDailyQuota: recoveryQuota(), Now: func() time.Time { return now.Add(40 * time.Millisecond) }}
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if len(recoveredKeys) != 1 || recoveredKeys[0] != newQualification.Key {
		t.Fatalf("recovery qualification keys=%q, want only current successor key %q", recoveredKeys, newQualification.Key)
	}
	var state string
	if err := db.QueryRowForTest(ctx, `SELECT isolation_state FROM attempts WHERE run_id='run' AND attempt_no=2`).Scan(&state); err != nil || state != "frozen" {
		t.Fatalf("replacement successor recovery isolation=%q err=%v, want frozen", state, err)
	}
}

type qualificationRecordingBackend struct {
	calls []runtimepkg.HostLaunch
	err   error
}

func (b *qualificationRecordingBackend) WrapperPath() string { return "/test/sift-agent-wrapper" }

func (b *qualificationRecordingBackend) Spawn(_ context.Context, launch runtimepkg.HostLaunch) (*os.Process, error) {
	b.calls = append(b.calls, launch)
	return nil, b.err
}

func openQualificationLaunchGate(t *testing.T, db *storage.DB, boot string, nowMS int64) {
	t.Helper()
	attempts, operations, err := db.StartupRecoveryPending(context.Background(), boot)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range attempts {
		if err := db.ApplyStartupRecoveryAction(context.Background(), storage.StartupRecoveryAction{BootID: boot, RunID: attempt.RunID, AttemptNo: attempt.AttemptNo, ExpectedGeneration: attempt.Generation, ObservationDigest: "qualification", Action: "supervise", NowMS: nowMS}); err != nil {
			t.Fatal(err)
		}
	}
	for _, operation := range operations {
		if err := db.ApplyStartupRecoveryAction(context.Background(), storage.StartupRecoveryAction{BootID: boot, OperationID: operation.ID, ExpectedOperationVersion: operation.Version, ObservationDigest: "qualification", Action: "converge_operation", NowMS: nowMS}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CompleteStartupRecovery(context.Background(), boot, nowMS); err != nil {
		t.Fatal(err)
	}
}

func topologyQualificationRecord(t *testing.T, q runtimepkg.Qualification, status, reason string, nowMS int64) storage.TopologyQualification {
	t.Helper()
	record := storage.TopologyQualification{ID: "qualification-" + q.Key[:8], QualificationKey: q.Key, MethodVersion: q.MethodVersion, AgentID: q.AgentID, AgentDefinitionHash: q.AgentDefinitionHash, ExecutablePath: q.ExecutablePath, ExecutableSHA256: q.ExecutableSHA256, VersionOutputDigest: q.VersionOutputDigest, GOOS: q.GOOS, GOARCH: q.GOARCH, Status: status, Reason: reason, RecordedAtMS: nowMS}
	var err error
	record.EvidenceJSON, err = storage.TopologyQualificationEvidenceJSON(record)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func detachedTopologyObservation(t *testing.T) runtimepkg.ProcessTopologyObservation {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("detached topology observation requires Linux procfs")
	}
	dir := t.TempDir()
	wrapper, agent := filepath.Join(dir, "wrapper"), filepath.Join(dir, "agent")
	agentPIDPath, descendantPIDPath := filepath.Join(dir, "agent.pid"), filepath.Join(dir, "descendant.pid")
	if err := os.WriteFile(agent, []byte("#!/bin/sh\necho \"$$\" > \"$SIFT_AGENT_PID\"\nsetsid sh -c 'echo $$ > \"$SIFT_DESCENDANT_PID\"; exec sleep 30' &\nchild=$!\nwait \"$child\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\n\"$SIFT_AGENT\" &\nagent=$!\nwait \"$agent\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(wrapper)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "SIFT_AGENT="+agent, "SIFT_AGENT_PID="+agentPIDPath, "SIFT_DESCENDANT_PID="+descendantPIDPath)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var agentPID, descendantPID int
	defer func() {
		_ = syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
		if agentPID > 0 {
			_ = syscall.Kill(agentPID, syscall.SIGKILL)
		}
		if descendantPID > 0 {
			_ = syscall.Kill(descendantPID, syscall.SIGKILL)
		}
		_ = cmd.Wait()
	}()
	readPID := func(path string) int {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if b, err := os.ReadFile(path); err == nil {
				raw := string(b)
				if strings.HasSuffix(raw, "\n") {
					pid, err := strconv.Atoi(strings.TrimSpace(raw))
					if err == nil && pid > 0 {
						return pid
					}
				}
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("fixture did not write %s", path)
		return 0
	}
	agentPID, descendantPID = readPID(agentPIDPath), readPID(descendantPIDPath)
	observation, err := runtimepkg.ObserveTopology(cmd.Process.Pid, agentPID, descendantPID)
	if err != nil {
		t.Fatal(err)
	}
	return observation
}

func detachedQualification(t *testing.T, evidence runtimepkg.QualificationEvidence) storage.TopologyQualification {
	t.Helper()
	projection := runtimepkg.Qualification{MethodVersion: runtimepkg.TopologyMethodVersion, AgentID: "agent", AgentDefinitionHash: strings.Repeat("a", 64), ExecutablePath: "/fixture/agent", ExecutableSHA256: strings.Repeat("b", 64), VersionOutputDigest: strings.Repeat("c", 64), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
	key, err := runtimepkg.QualificationKey(projection)
	if err != nil {
		t.Fatal(err)
	}
	q := storage.TopologyQualification{ID: "detached", QualificationKey: key, MethodVersion: projection.MethodVersion, AgentID: projection.AgentID, AgentDefinitionHash: projection.AgentDefinitionHash, ExecutablePath: projection.ExecutablePath, ExecutableSHA256: projection.ExecutableSHA256, VersionOutputDigest: projection.VersionOutputDigest, GOOS: projection.GOOS, GOARCH: projection.GOARCH, Status: string(evidence.Status), Reason: evidence.Reason, RecordedAtMS: nowMillis(time.Now())}
	q.EvidenceJSON, err = storage.TopologyQualificationEvidenceJSON(q)
	if err != nil {
		t.Fatal(err)
	}
	return q
}
