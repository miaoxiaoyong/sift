package controlplane

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/hooks"
	"github.com/miaoxiaoyong/sift/internal/policy"
	runtimepkg "github.com/miaoxiaoyong/sift/internal/runtime"
	"github.com/miaoxiaoyong/sift/internal/storage"
	"github.com/miaoxiaoyong/sift/internal/version"
)

type doctorCheck struct {
	ID      string         `json:"id"`
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// doctorExecutable locates the running sift binary whose sibling wrapper is
// probed; it is a variable so tests can point the probe at a fabricated
// install directory.
var doctorExecutable = os.Executable

func doctor(ctx context.Context, offline bool, home config.Home) map[string]any {
	return doctorWithVersions(ctx, offline, home, Version, ProtocolMajor, nil)
}

func doctorWithVersions(ctx context.Context, offline bool, home config.Home, clientVersion string, clientProtocolMajor int, liveDaemon *storage.DoctorDaemonVersion) map[string]any {
	// Dependency probes are independent checks; a slow or unavailable command
	// must not consume the entire budget for the later SQLite and projection checks.
	ctx, cancel := context.WithTimeout(ctx, 15*deadline)
	defer cancel()
	checks := []doctorCheck{runtimeCheck(), versionCheck(), permissionCheck(home.Path, "home", 0o700, true)}
	if daemon, err := doctorExecutable(); err == nil {
		checks = append(checks, wrapperVersionChecks(ctx, daemon)...)
	}
	var cfg *config.Config
	if snapshot, err := config.Load(home, time.Now()); err != nil {
		checks = append(checks, errorCheck("config", err))
	} else {
		cfg = snapshot.Config
		checks = append(checks, executableChecks(ctx, cfg)...)
		checks = append(checks, processGroupChecks(ctx, filepath.Join(home.Path, "sift.db"), cfg)...)
	}
	dbPath := filepath.Join(home.Path, "sift.db")
	checks = append(checks, sqliteCheck(ctx, dbPath))
	checks = append(checks, versionChecks(ctx, dbPath, clientVersion, clientProtocolMajor, liveDaemon)...)
	checks = append(checks, outboxChecks(ctx, dbPath)...)
	if cfg != nil {
		checks = append(checks, hookChecks(ctx, dbPath, cfg)...)
		checks = append(checks, projectPolicyChecks(ctx, cfg)...)
	}
	checks = append(checks, attemptChecks(ctx, dbPath)...)
	checks = append(checks, homePermissions(home.Path, offline)...)
	checks = append(checks, platformPostureChecks()...)
	checks = append(checks, tm6ExposureChecks()...)

	exitCode := 0
	for _, check := range checks {
		if check.Level == "error" {
			exitCode = 2
			break
		}
		if check.Level == "warning" {
			exitCode = 1
		}
	}
	return map[string]any{"offline": offline, "exit_code": exitCode, "security_posture": "unsafe-local", "checks": checks}
}

func runtimeCheck() doctorCheck {
	return doctorCheck{
		ID: "runtime", Level: "ok", Message: "Go runtime is supported",
		Details: map[string]any{"go_version": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH},
	}
}

// versionCheck reports the release version of the binary running the doctor.
// The same value is injected through ldflags into sift, sift-agent-wrapper and
// the daemon (internal/version.Release); config.Version (protocol) is separate
// and unchanged (specs/release.md §1).
func versionCheck() doctorCheck {
	return doctorCheck{
		ID: "version", Level: "ok", Message: "release version is canonical",
		Details: map[string]any{"release_version": version.Release, "go_version": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH},
	}
}

// wrapperVersionChecks surfaces the wrapper/daemon release-version and
// protocol-major handshake to the doctor (WBS §8.1). It is the only emitter
// of version:wrapper and grades solely on the actual daemon-side probe:
// details carry the observed wrapper values, never client-reported input. The daemon refuses to start on a mismatch, so this
// is the visibility side of the invariant; the offline doctor (run from the
// sift binary in the same install directory) is the path that sees it.
// daemonPath is the running sift binary (os.Executable); it is a parameter so
// tests can drive the check against a fabricated install directory.
func wrapperVersionChecks(ctx context.Context, daemonPath string) []doctorCheck {
	wrapper := runtimepkg.WrapperPathNextTo(daemonPath)
	info, err := os.Stat(wrapper)
	if err != nil {
		return []doctorCheck{{ID: "version:wrapper", Level: "warning", Message: "wrapper is not installed next to the sift binary", Details: map[string]any{"wrapper_path": wrapper, "release_version": version.Release}}}
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return []doctorCheck{{ID: "version:wrapper", Level: "warning", Message: "installed wrapper is not an executable file", Details: map[string]any{"wrapper_path": wrapper, "release_version": version.Release}}}
	}
	out, _, err := runtimepkg.ProbeVersion(ctx, wrapper, []string{"--version"}, 0)
	if err != nil {
		return []doctorCheck{{ID: "version:wrapper", Level: "warning", Message: "cannot probe the installed wrapper", Details: map[string]any{"wrapper_path": wrapper, "release_version": version.Release, "error": err.Error()}}}
	}
	reported := strings.TrimSpace(string(out))
	protocolOut, _, err := runtimepkg.ProbeVersion(ctx, wrapper, []string{"--protocol-major"}, 0)
	if err != nil {
		return []doctorCheck{{ID: "version:wrapper", Level: "warning", Message: "cannot probe the installed wrapper protocol major", Details: map[string]any{"wrapper_path": wrapper, "release_version": version.Release, "wrapper_version": reported, "error": err.Error()}}}
	}
	reportedMajor, err := strconv.Atoi(strings.TrimSpace(string(protocolOut)))
	if err != nil {
		return []doctorCheck{{ID: "version:wrapper", Level: "error", Message: fmt.Sprintf("%v: sift %d, wrapper %s", runtimepkg.ErrWrapperProtocolMajor, ProtocolMajor, strings.TrimSpace(string(protocolOut))), Details: map[string]any{"wrapper_path": wrapper, "daemon_version": version.Release, "daemon_protocol_major": ProtocolMajor, "wrapper_version": reported, "wrapper_protocol_major": strings.TrimSpace(string(protocolOut))}}}
	}
	details := map[string]any{"wrapper_path": wrapper, "daemon_version": version.Release, "release_version": version.Release, "daemon_protocol_major": ProtocolMajor, "wrapper_version": reported, "wrapper_protocol_major": reportedMajor}
	if reported != version.Release {
		return []doctorCheck{{ID: "version:wrapper", Level: "error", Message: fmt.Sprintf("%v: sift %s, wrapper %s", runtimepkg.ErrWrapperVersion, version.Release, reported), Details: details}}
	}
	if reportedMajor != ProtocolMajor {
		return []doctorCheck{{ID: "version:wrapper", Level: "error", Message: fmt.Sprintf("%v: sift %d, wrapper %d", runtimepkg.ErrWrapperProtocolMajor, ProtocolMajor, reportedMajor), Details: details}}
	}
	return []doctorCheck{{ID: "version:wrapper", Level: "ok", Message: "wrapper matches the release version and protocol major", Details: details}}
}

func executableChecks(ctx context.Context, cfg *config.Config) []doctorCheck {
	checks := make([]doctorCheck, 0, len(cfg.Agents)+len(cfg.Projects)*2+2)
	for _, agent := range cfg.Agents {
		checks = append(checks, qualificationCommandCheck(ctx, "agent-cli:"+agent.ID, agent.Executable, agent.VersionArgs))
	}
	if cfg.Brain.Executable != "" {
		checks = append(checks, commandCheck(ctx, "brain-cli", cfg.Brain.Executable, cfg.Brain.VersionArgs))
	}
	if configUsesTmux(cfg) {
		checks = append(checks, commandCheck(ctx, "tmux", "tmux", []string{"-V"}))
	}

	seen := map[string]bool{}
	for _, project := range cfg.Projects {
		if !project.Enabled {
			continue
		}
		key := project.Forge.CLI + "\x00" + project.Forge.Host
		if seen[key] {
			continue
		}
		seen[key] = true
		checks = append(checks,
			commandCheck(ctx, "forge-cli:"+project.ID+":version", project.Forge.CLI, []string{"--version"}),
			commandCheck(ctx, "forge-cli:"+project.ID+":login", project.Forge.CLI, []string{"auth", "status", "--hostname", project.Forge.Host}),
		)
	}
	return checks
}

func processGroupChecks(ctx context.Context, dbPath string, cfg *config.Config) []doctorCheck {
	checks := make([]doctorCheck, 0, len(cfg.Agents))
	for _, agent := range cfg.Agents {
		q, err := runtimepkg.BuildQualification(runtimepkg.QualificationInput{AgentID: agent.ID, Args: agent.Args, TaskTransport: string(agent.TaskTransport), VersionArgs: agent.VersionArgs, Executable: agent.Executable, Context: ctx})
		if err != nil {
			checks = append(checks, doctorCheck{ID: "process-group:" + agent.ID, Level: "warning", Message: "process-group qualification identity is unavailable", Details: map[string]any{"agent_id": agent.ID, "status": "process-group-unverified", "reason": "identity_incomplete"}})
			continue
		}
		status, reason, statusErr := storage.ReadTopologyQualificationStatus(ctx, dbPath, q.Key)
		if statusErr != nil {
			status, reason = "process-group-unverified", "no-record"
		}
		level, message := "warning", "process-group qualification is not verified"
		if status == "process-group-verified" {
			level, message = "ok", "process-group qualification is verified"
		}
		checks = append(checks, doctorCheck{ID: "process-group:" + agent.ID, Level: level, Message: message, Details: map[string]any{"agent_id": agent.ID, "status": status, "reason": reason, "qualification_key": q.Key, "method_version": q.MethodVersion, "executable_path": q.ExecutablePath, "goos": q.GOOS, "goarch": q.GOARCH}})
	}
	return checks
}

func hookChecks(ctx context.Context, dbPath string, cfg *config.Config) []doctorCheck {
	baselines, _, err := storage.ReadDoctorState(ctx, dbPath)
	if err != nil {
		return []doctorCheck{{ID: "hooks:storage", Level: "warning", Message: err.Error(), Details: map[string]any{}}}
	}
	byProject := make(map[string]string, len(baselines))
	for _, b := range baselines {
		byProject[b.ProjectID] = b.Digest
	}
	checks := make([]doctorCheck, 0, len(cfg.Projects))
	for _, project := range cfg.Projects {
		if !project.Enabled {
			continue
		}
		snapshot, err := hooks.Capture(ctx, project.Repo)
		if err != nil {
			checks = append(checks, doctorCheck{ID: "hooks:" + project.ID, Level: "warning", Message: err.Error(), Details: map[string]any{"project_id": project.ID}})
			continue
		}
		details := map[string]any{"project_id": project.ID, "git_config_digest": snapshot.GitConfigDigest, "effective_hooks_path": snapshot.EffectiveHooksPath, "hooks_directory_digest": snapshot.DirectoryDigest, "digest": snapshot.Digest}
		if snapshot.CoreHooksPathValue != nil {
			details["core_hooks_path"] = *snapshot.CoreHooksPathValue
		}
		baseline, ok := byProject[project.ID]
		if !ok {
			checks = append(checks, doctorCheck{ID: "hooks:" + project.ID, Level: "warning", Message: "hooks baseline is absent", Details: details})
			continue
		}
		details["baseline_digest"] = baseline
		if baseline != snapshot.Digest {
			checks = append(checks, doctorCheck{ID: "hooks:" + project.ID, Level: "warning", Message: "hooks state drifted from baseline", Details: details})
		} else {
			checks = append(checks, doctorCheck{ID: "hooks:" + project.ID, Level: "ok", Message: "hooks match baseline", Details: details})
		}
	}
	return checks
}

func projectPolicyChecks(ctx context.Context, cfg *config.Config) []doctorCheck {
	type observedPolicy struct {
		projectID string
		hash      string
	}
	observed := make([]observedPolicy, 0, len(cfg.Projects))
	checks := make([]doctorCheck, 0, len(cfg.Projects)*2)
	for _, project := range cfg.Projects {
		if !project.Enabled {
			continue
		}
		baseSHA, data, missing, err := readProjectPolicy(ctx, project.Repo)
		if err != nil {
			checks = append(checks, errorCheck("policy:"+project.ID, err))
			continue
		}
		base := policy.Missing()
		fileState := "missing"
		if !missing {
			base, err = policy.Parse(data)
			if err != nil {
				checks = append(checks, errorCheck("policy:"+project.ID, err))
				continue
			}
			fileState = "valid"
		}
		// Doctor has no task-kind-specific certification projection to invent.
		// The all-zero revision makes unavailable certification fail closed while
		// still exposing the effective policy currently safe to use.
		_, hash, _, qualification, err := policy.Assemble(base, cfg.GateDefaults, "doctor", policy.CertificationProjection{TaskKind: "doctor", CertificationVersion: strings.Repeat("0", 64)}, false)
		if err != nil {
			checks = append(checks, errorCheck("policy:"+project.ID, err))
			continue
		}
		rulesVersion, err := config.CertificationRulesVersion(cfg.Certification)
		if err != nil {
			checks = append(checks, errorCheck("policy:"+project.ID, err))
			continue
		}
		details := map[string]any{
			"project_id": project.ID, "base_sha": baseSHA, "file_state": fileState,
			"effective_policy_hash": hash, "certification_rules_version": rulesVersion, "certification_version": "unknown",
			"auto_merge_qualification":  qualification.AutoMerge,
			"explicit_scalar_overrides": explicitScalarOverrides(base),
			"path_rules":                map[string][]string{"hard": base.Hard, "soft": base.Soft, "soft_exceptions": base.SoftExceptions},
		}
		level, message := "ok", "project policy matches global defaults"
		if hasExplicitDrift(base) {
			level, message = "warning", "project policy explicitly differs from global defaults"
		}
		if qualification.AutoMerge != policy.AutoMergeNotRequested {
			level, message = "warning", "auto_merge requested but certification or CAS qualification is unavailable"
		}
		checks = append(checks, doctorCheck{ID: "policy:" + project.ID, Level: level, Message: message, Details: details})
		observed = append(observed, observedPolicy{projectID: project.ID, hash: hash})
	}
	if len(observed) > 1 {
		baseline := observed[0]
		for _, current := range observed[1:] {
			if current.hash != baseline.hash {
				checks = append(checks, doctorCheck{ID: "policy-drift:" + current.projectID, Level: "info", Message: "effective policy differs from project baseline", Details: map[string]any{"project_id": current.projectID, "baseline_project_id": baseline.projectID, "effective_policy_hash": current.hash, "baseline_effective_policy_hash": baseline.hash}})
			}
		}
	}
	return checks
}

func readProjectPolicy(ctx context.Context, repo string) (baseSHA string, data []byte, missing bool, err error) {
	shaBytes, err := gitOutput(ctx, repo, "rev-parse", "HEAD^{commit}")
	if err != nil {
		return "", nil, false, fmt.Errorf("project policy base revision: %w", err)
	}
	sha := string(shaBytes)
	data, err = gitOutput(ctx, repo, "show", sha+":.sift/policy.yaml")
	if err == nil {
		return sha, data, false, nil
	}
	if _, probeErr := gitOutput(ctx, repo, "cat-file", "-e", sha+":.sift/policy.yaml"); probeErr != nil {
		return sha, nil, true, nil
	}
	return "", nil, false, fmt.Errorf("read project policy: %w", err)
}

func gitOutput(ctx context.Context, repo string, args ...string) ([]byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	output, err := exec.CommandContext(commandCtx, "git", append([]string{"-C", repo}, args...)...).Output()
	if err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(string(output), "\n")), nil
}

func explicitScalarOverrides(base policy.BasePolicy) map[string]string {
	overrides := map[string]string{}
	if base.ReviewPolicy != nil {
		overrides["review_policy"] = string(*base.ReviewPolicy)
	}
	if base.RiskyReviewThreshold != nil {
		overrides["risky_review_threshold"] = fmt.Sprint(*base.RiskyReviewThreshold)
	}
	if base.AutoMerge != nil {
		overrides["auto_merge"] = fmt.Sprint(*base.AutoMerge)
	}
	if base.ChecksPendingTimeout != nil {
		overrides["checks_pending_timeout"] = base.ChecksPendingTimeout.String()
	}
	if base.FlakyRetryLimit != nil {
		overrides["flaky_retry_limit"] = fmt.Sprint(*base.FlakyRetryLimit)
	}
	return overrides
}

func hasExplicitDrift(base policy.BasePolicy) bool {
	return len(base.Hard) != 0 || len(base.Soft) != 0 || len(base.SoftExceptions) != 0 || len(explicitScalarOverrides(base)) != 0
}

func attemptChecks(ctx context.Context, dbPath string) []doctorCheck {
	_, attempts, err := storage.ReadDoctorState(ctx, dbPath)
	if err != nil {
		return []doctorCheck{{ID: "attempts:storage", Level: "warning", Message: err.Error(), Details: map[string]any{}}}
	}
	checks := make([]doctorCheck, 0, len(attempts))
	for _, a := range attempts {
		level, message := "warning", "attempt remains active or isolated"
		if a.IsolationState == "frozen" {
			message = "attempt is isolated; worktree is intentionally retained"
		}
		checks = append(checks, doctorCheck{ID: fmt.Sprintf("attempt:%s/%d", a.RunID, a.AttemptNo), Level: level, Message: message, Details: map[string]any{"run_id": a.RunID, "attempt_no": a.AttemptNo, "phase": a.Phase, "isolation_state": a.IsolationState, "worktree_path": a.WorktreePath, "agent_id": a.AgentID}})
	}
	return checks
}

func configUsesTmux(cfg *config.Config) bool {
	if cfg.Runtime.Backend == config.BackendTmux {
		return true
	}
	for _, agent := range cfg.Agents {
		if agent.Backend == config.BackendTmux {
			return true
		}
	}
	return false
}

// qualificationCommandCheck is the credential-free, bounded path for Agent
// version checks. Forge and brain checks retain commandCheck because their
// authenticated capability checks have different contracts.
func qualificationCommandCheck(ctx context.Context, id, name string, args []string) doctorCheck {
	path, err := exec.LookPath(name)
	if err != nil {
		return errorCheck(id, fmt.Errorf("executable %q not found: %w", name, err))
	}
	stdout, stderr, err := runtimepkg.ProbeVersion(ctx, path, args, 0)
	if err != nil {
		output := strings.TrimSpace(string(append(stdout, stderr...)))
		if output != "" {
			return errorCheck(id, fmt.Errorf("%s: %w", output, err))
		}
		return errorCheck(id, err)
	}
	return doctorCheck{ID: id, Level: "ok", Message: "command is available", Details: map[string]any{"path": path, "output": strings.TrimSpace(string(append(stdout, stderr...)))}}
}

func commandCheck(ctx context.Context, id, name string, args []string) doctorCheck {
	path, err := exec.LookPath(name)
	if err != nil {
		return errorCheck(id, fmt.Errorf("executable %q not found: %w", name, err))
	}
	commandCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, path, args...)
	output, err := cmd.CombinedOutput()
	// macOS can transiently kill a shell-backed fixture when the full package
	// suite is CPU constrained. Retry that observation once; real command
	// failures still remain errors.
	if exit, ok := err.(*exec.ExitError); ok && exit.ProcessState.Sys() == syscall.SIGKILL && ctx.Err() == nil {
		cmd = exec.CommandContext(commandCtx, path, args...)
		output, err = cmd.CombinedOutput()
	}
	if err != nil {
		if text := strings.TrimSpace(string(output)); text != "" {
			return errorCheck(id, fmt.Errorf("%s: %w", text, err))
		}
		return errorCheck(id, err)
	}
	return doctorCheck{ID: id, Level: "ok", Message: "command is available", Details: map[string]any{"path": path, "output": strings.TrimSpace(string(output))}}
}

func sqliteCheck(ctx context.Context, path string) doctorCheck {
	if err := storage.CheckReadOnly(ctx, path); err != nil {
		return errorCheck("sqlite", err)
	}
	return doctorCheck{ID: "sqlite", Level: "ok", Message: "SQLite database is readable", Details: map[string]any{"path": path}}
}

func homePermissions(home string, offline bool) []doctorCheck {
	paths := []struct {
		name     string
		mode     os.FileMode
		required bool
	}{
		{"config.yaml", 0o600, false}, {"sift.db", 0o600, false}, {"operator.token", 0o600, false},
		{"siftd.lock", 0o600, false}, {"logs", 0o700, false}, {"worktrees", 0o700, false}, {"runs", 0o700, false},
		{"siftd.sock", 0o600, !offline}, {"run.sock", 0o600, !offline},
	}
	checks := make([]doctorCheck, 0, len(paths))
	for _, p := range paths {
		checks = append(checks, permissionCheck(filepath.Join(home, p.name), p.name, p.mode, p.required))
	}
	return checks
}

func permissionCheck(path, name string, want os.FileMode, required bool) doctorCheck {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) && !required {
		return doctorCheck{ID: "permissions:" + name, Level: "ok", Message: "path is absent", Details: map[string]any{"path": path}}
	}
	if err != nil {
		return errorCheck("permissions:"+name, err)
	}
	if info.Mode().Perm() != want {
		return errorCheck("permissions:"+name, fmt.Errorf("%s mode %04o, want %04o", path, info.Mode().Perm(), want))
	}
	if name == "home" || name == "logs" || name == "worktrees" || name == "runs" {
		if !info.IsDir() {
			return errorCheck("permissions:"+name, fmt.Errorf("%s is not a directory", path))
		}
	} else if strings.HasSuffix(name, ".sock") {
		if info.Mode()&os.ModeSocket == 0 {
			return errorCheck("permissions:"+name, fmt.Errorf("%s is not a socket", path))
		}
	} else if !info.Mode().IsRegular() {
		return errorCheck("permissions:"+name, fmt.Errorf("%s is not a regular file", path))
	}
	return doctorCheck{ID: "permissions:" + name, Level: "ok", Message: "permissions are owner-only", Details: map[string]any{"path": path, "mode": fmt.Sprintf("%04o", info.Mode().Perm())}}
}

// versionChecks emits the single version:daemon check: it pairs the client's
// self-reported envelope values with the daemon's actual version record (live
// values online, the durable boot row offline). The wrapper is deliberately
// not paired here; version:wrapper is emitted exactly once by
// wrapperVersionChecks from the actual daemon-side probe, never from client
// input.
func versionChecks(ctx context.Context, dbPath, clientVersion string, clientProtocolMajor int, liveDaemon *storage.DoctorDaemonVersion) []doctorCheck {
	var daemon storage.DoctorDaemonVersion
	var active bool
	var dbErr error
	if liveDaemon != nil {
		daemon, active = *liveDaemon, true
	} else {
		daemon, active, dbErr = storage.ReadDoctorDaemonVersion(ctx, dbPath)
	}
	checks := []doctorCheck{}
	if dbErr != nil {
		checks = append(checks, errorCheck("version:daemon", dbErr))
		return checks
	}
	if !active {
		checks = append(checks, doctorCheck{ID: "version:daemon", Level: "ok", Message: "no active daemon version record", Details: map[string]any{"cli_version": clientVersion, "protocol_major": clientProtocolMajor}})
		return checks
	}
	if daemon.ProtocolMajor != clientProtocolMajor || majorVersion(daemon.BinaryVersion) != majorVersion(clientVersion) {
		checks = append(checks, doctorCheck{ID: "version:daemon", Level: "error", Message: "CLI and daemon protocol major versions differ", Details: map[string]any{"cli_version": clientVersion, "cli_protocol_major": clientProtocolMajor, "daemon_version": daemon.BinaryVersion, "daemon_protocol_major": daemon.ProtocolMajor}})
		return checks
	}
	checks = append(checks, doctorCheck{ID: "version:daemon", Level: "ok", Message: "CLI and daemon protocol major versions match", Details: map[string]any{"cli_version": clientVersion, "cli_protocol_major": clientProtocolMajor, "daemon_version": daemon.BinaryVersion, "daemon_protocol_major": daemon.ProtocolMajor}})
	return checks
}

func majorVersion(version string) string {
	if i := strings.IndexByte(version, '.'); i >= 0 {
		return version[:i]
	}
	return version
}

func outboxChecks(ctx context.Context, dbPath string) []doctorCheck {
	outbox, err := storage.ReadDoctorOutbox(ctx, dbPath)
	if err != nil {
		return []doctorCheck{errorCheck("outbox:backlog", err), errorCheck("outbox:push-failures", err)}
	}
	backlog := doctorCheck{ID: "outbox:backlog", Level: "ok", Message: "outbox has no pending operations", Details: map[string]any{"pending_count": outbox.Pending}}
	if outbox.Pending > 0 {
		backlog.Level, backlog.Message = "warning", "outbox operations remain pending"
	}
	pushFailures := make([]storage.DoctorOutboxFailure, 0, len(outbox.Failed))
	for _, failure := range outbox.Failed {
		if isRemoteDeliveryKind(failure.Kind) {
			pushFailures = append(pushFailures, failure)
		}
	}
	generic := doctorCheck{ID: "outbox:failures", Level: "ok", Message: "outbox has no terminal failures", Details: map[string]any{"failed_count": len(outbox.Failed), "failures": outbox.Failed}}
	if len(outbox.Failed) > 0 {
		generic.Level, generic.Message = "error", "outbox contains terminal failures"
	}
	push := doctorCheck{ID: "outbox:push-failures", Level: "ok", Message: "outbox has no terminal delivery failures", Details: map[string]any{"failed_count": len(pushFailures), "failures": pushFailures}}
	if len(pushFailures) > 0 {
		push.Level, push.Message = "error", "outbox contains terminal delivery failures"
	}
	return []doctorCheck{backlog, generic, push}
}

func isRemoteDeliveryKind(kind string) bool {
	switch storage.OperationKind(kind) {
	case storage.OperationForgeComment, storage.OperationForgeLabels, storage.OperationCreateChange,
		storage.OperationMergeChange, storage.OperationRerunChecks, storage.OperationChannelPublish,
		storage.OperationCommandAck, storage.OperationForgeAlert:
		return true
	default:
		return false
	}
}

func platformPostureChecks() []doctorCheck {
	checks := make([]doctorCheck, 0, 2)
	for _, platform := range []string{"darwin", "linux"} {
		checks = append(checks, doctorCheck{ID: "security-posture:" + platform, Level: "warning", Message: platform + " V0 posture is unsafe-local; agent isolation is not implemented", Details: map[string]any{"platform": platform, "current_platform": runtime.GOOS == platform, "security_posture": "unsafe-local", "agent_isolation": "not-implemented"}})
	}
	return checks
}

func tm6ExposureChecks() []doctorCheck {
	return []doctorCheck{
		unsafeLocalCheck(),
		{ID: "tm6:sift-home", Level: "warning", Message: "same-UID agents can read ~/.sift/ despite owner-only permissions", Details: map[string]any{"exposure": "~/.sift/ configuration, database, and local state", "v0_status": "unclosed"}},
		{ID: "tm6:forge-cli-credentials", Level: "warning", Message: "same-UID agents can use already logged-in forge CLIs", Details: map[string]any{"exposure": "gh/glab credentials", "v0_status": "unclosed"}},
		{ID: "tm6:operator-token-and-socket", Level: "warning", Message: "same-UID agents can read operator.token and call kill or retry over the operator socket", Details: map[string]any{"exposure": "operator.token and siftd.sock", "v0_status": "unclosed"}},
		{ID: "tm6:shared-git", Level: "warning", Message: "shared .git, other worktrees, and non-Sift git writes remain reachable by same-UID agents", Details: map[string]any{"exposure": "shared .git and worktrees", "v0_status": "unclosed", "sift_git_control": "hooks disabled and fingerprinted"}},
		{ID: "tm6:process-group-escape", Level: "warning", Message: "an agent or descendant can leave the wrapper process group", Details: map[string]any{"exposure": "process supervision", "v0_status": "unclosed", "mitigation": "qualification limits automatic retry"}},
		{ID: "tm6:run-token", Level: "warning", Message: "same-UID processes can read the run token from owner-only control.json", Details: map[string]any{"exposure": "run token", "v0_status": "unclosed"}},
		{ID: "tm6:bootstrap-credential", Level: "warning", Message: "another same-UID agent can race the short bootstrap credential window", Details: map[string]any{"exposure": "attempt bootstrap credential", "v0_status": "unclosed", "mitigation": "single-use claim binding"}},
	}
}

func unsafeLocalCheck() doctorCheck {
	return doctorCheck{ID: "operator-token-readable-by-agent", Level: "warning", Message: "V0 same-UID agents can read the operator token", Details: map[string]any{}}
}

func errorCheck(id string, err error) doctorCheck {
	return doctorCheck{ID: id, Level: "error", Message: err.Error(), Details: map[string]any{}}
}
