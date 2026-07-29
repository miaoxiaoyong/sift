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
		checks = append(checks, processGroupChecks(cfg)...)
	}
	dbPath := filepath.Join(home.Path, "sift.db")
	checks = append(checks, sqliteCheck(ctx, dbPath))
	if cfg != nil {
		checks = append(checks, hookChecks(ctx, dbPath, cfg)...)
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

func processGroupChecks(cfg *config.Config) []doctorCheck {
	checks := make([]doctorCheck, 0, len(cfg.Agents))
	for _, agent := range cfg.Agents {
		checks = append(checks, doctorCheck{ID: "process-group:" + agent.ID, Level: "warning", Message: "process-group qualification is not verified", Details: map[string]any{"agent_id": agent.ID, "status": "process-group-unverified"}})
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
