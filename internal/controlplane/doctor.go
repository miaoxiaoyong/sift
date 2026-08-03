package controlplane

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/hooks"
	"github.com/miaoxiaoyong/sift/internal/policy"
	runtimepkg "github.com/miaoxiaoyong/sift/internal/runtime"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

type doctorCheck struct {
	ID      string         `json:"id"`
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

func doctor(ctx context.Context, offline bool, home config.Home) map[string]any {
	// Dependency probes are independent checks; a slow or unavailable command
	// must not consume the entire budget for the later SQLite and projection checks.
	ctx, cancel := context.WithTimeout(ctx, 15*deadline)
	defer cancel()
	checks := []doctorCheck{runtimeCheck(), permissionCheck(home.Path, "home", 0o700, true)}
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
	if cfg != nil {
		checks = append(checks, hookChecks(ctx, dbPath, cfg)...)
		checks = append(checks, projectPolicyChecks(ctx, cfg)...)
	}
	checks = append(checks, attemptChecks(ctx, dbPath)...)
	checks = append(checks, homePermissions(home.Path, offline)...)
	checks = append(checks, unsafeLocalCheck())

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

func executableChecks(ctx context.Context, cfg *config.Config) []doctorCheck {
	checks := make([]doctorCheck, 0, len(cfg.Agents)+len(cfg.Projects)*2+2)
	for _, agent := range cfg.Agents {
		checks = append(checks, commandCheck(ctx, "agent-cli:"+agent.ID, agent.Executable, agent.VersionArgs))
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
		q, err := runtimepkg.BuildQualification(runtimepkg.QualificationInput{AgentID: agent.ID, Args: agent.Args, TaskTransport: string(agent.TaskTransport), VersionArgs: agent.VersionArgs, Executable: agent.Executable})
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

func unsafeLocalCheck() doctorCheck {
	return doctorCheck{ID: "operator-token-readable-by-agent", Level: "warning", Message: "V0 same-UID agents can read the operator token", Details: map[string]any{}}
}

func errorCheck(id string, err error) doctorCheck {
	return doctorCheck{ID: id, Level: "error", Message: err.Error(), Details: map[string]any{}}
}
