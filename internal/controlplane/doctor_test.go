package controlplane

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/storage"
)

func TestDoctorBaselineChecksConfiguredDependencies(t *testing.T) {
	stubDoctorWrapper(t)
	home := testHome(t)
	bin := filepath.Join(home.Path, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agent", "gh", "tmux"} {
		writeDoctorExecutable(t, bin, name)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	configYAML := `version: 1
agents:
  - id: agent
    executable: agent
    backend: tmux
projects:
  - id: project
    repo: ` + home.Path + `
    forge:
      kind: github
      project: owner/repo
`
	initDoctorRepo(t, home.Path, "version: 1\n")
	if err := os.WriteFile(filepath.Join(home.Path, "config.yaml"), []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), storage.OpenConfig{Path: filepath.Join(home.Path, "sift.db"), BinaryVersion: Version, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	result := doctor(context.Background(), true, home)
	if result["exit_code"] != 1 || result["security_posture"] != "unsafe-local" {
		t.Fatalf("doctor result = %#v", result)
	}
	checks := doctorChecks(t, result)
	for _, id := range []string{"runtime", "sqlite", "agent-cli:agent", "forge-cli:project:version", "forge-cli:project:login", "tmux", "permissions:home"} {
		if check := checks[id]; check.Level != "ok" {
			t.Errorf("%s = %#v, want ok", id, check)
		}
	}
	if checks["operator-token-readable-by-agent"].Level != "warning" {
		t.Fatalf("unsafe-local check = %#v", checks["operator-token-readable-by-agent"])
	}
}

func TestDoctorReportsProjectPolicyDrift(t *testing.T) {
	home := testHome(t)
	for _, name := range []string{"agent", "gh"} {
		writeDoctorExecutable(t, home.Path, name)
	}
	t.Setenv("PATH", home.Path+string(os.PathListSeparator)+os.Getenv("PATH"))
	first := filepath.Join(home.Path, "first")
	second := filepath.Join(home.Path, "second")
	if err := os.MkdirAll(first, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(second, 0o700); err != nil {
		t.Fatal(err)
	}
	initDoctorRepo(t, first, "version: 1\nreview_policy: never\n")
	initDoctorRepo(t, second, "version: 1\n")
	configYAML := `version: 1
agents:
  - id: agent
    executable: agent
projects:
  - id: first
    repo: ` + first + `
    forge: {kind: github, project: owner/first}
  - id: second
    repo: ` + second + `
    forge: {kind: github, project: owner/second}
`
	if err := os.WriteFile(filepath.Join(home.Path, "config.yaml"), []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	result := doctor(context.Background(), true, home)
	checks := doctorChecks(t, result)
	if checks["policy:first"].Level != "warning" || checks["policy:second"].Level != "ok" {
		t.Fatalf("policy checks = %#v", checks)
	}
	if checks["policy-drift:second"].Level != "info" {
		t.Fatalf("policy comparison = %#v", checks["policy-drift:second"])
	}
	for _, id := range []string{"base_sha", "effective_policy_hash", "certification_version", "explicit_scalar_overrides", "path_rules"} {
		if _, ok := checks["policy:first"].Details[id]; !ok {
			t.Errorf("policy detail %q missing", id)
		}
	}
}

func TestDoctorReportsSQLiteAndPermissionErrors(t *testing.T) {
	home := testHome(t)
	if err := os.Chmod(home.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	result := doctor(context.Background(), true, home)
	if result["exit_code"] != 2 {
		t.Fatalf("exit_code = %v, want 2", result["exit_code"])
	}
	checks := doctorChecks(t, result)
	if checks["permissions:home"].Level != "error" {
		t.Fatalf("home permission check = %#v", checks["permissions:home"])
	}
	if checks["sqlite"].Level != "error" {
		t.Fatalf("sqlite check = %#v", checks["sqlite"])
	}
}

func TestDoctorRejectsNonSocketAtSocketPath(t *testing.T) {
	home := testHome(t)
	if err := os.WriteFile(filepath.Join(home.Path, "siftd.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	checks := doctorChecks(t, doctor(context.Background(), false, home))
	if checks["permissions:siftd.sock"].Level != "error" {
		t.Fatalf("socket check = %#v", checks["permissions:siftd.sock"])
	}
}

func TestDoctorReportsTM6PlatformsOutboxAndExitContract(t *testing.T) {
	stubDoctorWrapper(t)
	home := testHome(t)
	db, err := storage.Open(context.Background(), storage.OpenConfig{Path: filepath.Join(home.Path, "sift.db"), BinaryVersion: Version, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(context.Background(), `INSERT INTO outbox_operations (id,operation_key,kind,state,payload_schema_version,payload_json,payload_digest,attempt_count,next_attempt_at_ms,created_at_ms,updated_at_ms) VALUES ('pending','pending-key','forge_comment','pending',1,'{}','digest',0,0,0,0), ('failed','failed-key','forge_comment','failed',1,'{}','digest',3,0,0,0)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	result := doctor(context.Background(), true, home)
	if result["exit_code"] != 2 {
		t.Fatalf("exit_code = %v, want 2 for terminal outbox failure", result["exit_code"])
	}
	checks := doctorChecks(t, result)
	for _, id := range []string{"outbox:backlog", "outbox:push-failures", "security-posture:darwin", "security-posture:linux", "tm6:sift-home", "tm6:forge-cli-credentials", "tm6:operator-token-and-socket", "tm6:shared-git", "tm6:process-group-escape", "tm6:run-token", "tm6:bootstrap-credential"} {
		if _, ok := checks[id]; !ok {
			t.Errorf("missing doctor check %q", id)
		}
	}
	if checks["outbox:backlog"].Level != "warning" || checks["outbox:push-failures"].Level != "error" {
		t.Fatalf("outbox checks = %#v", checks)
	}
}

func TestDoctorReportsDaemonProtocolMismatch(t *testing.T) {
	stubDoctorWrapper(t)
	home := testHome(t)
	db, err := storage.Open(context.Background(), storage.OpenConfig{Path: filepath.Join(home.Path, "sift.db"), BinaryVersion: Version, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(context.Background(), `INSERT INTO config_snapshots(id,config_hash,schema_version,canonical_json,source_present,loaded_at_ms,binary_version) VALUES ('config','config',1,'{}',1,0,'test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.StartDaemonBoot(context.Background(), "config", "2.0.0", ProtocolMajor+1, os.Getpid(), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	checks := doctorChecks(t, doctor(context.Background(), true, home))
	if checks["version:daemon"].Level != "error" {
		t.Fatalf("daemon version check = %#v", checks["version:daemon"])
	}
}

func initDoctorRepo(t *testing.T, repo, policy string) {
	t.Helper()
	for _, args := range [][]string{{"init"}, {"config", "user.email", "doctor@example.test"}, {"config", "user.name", "Doctor"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	if err := os.Mkdir(filepath.Join(repo, ".sift"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".sift", "policy.yaml"), []byte(policy), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", repo, "add", ".sift/policy.yaml")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	cmd = exec.Command("git", "-C", repo, "commit", "-m", "policy")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, output)
	}
}

func writeDoctorExecutable(t *testing.T, dir, name string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'fixture '+\"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func stubDoctorWrapper(t *testing.T) {
	t.Helper()
	previous := resolveInstalledWrapper
	resolveInstalledWrapper = func(string) (string, error) { return "/fixture/sift-agent-wrapper", nil }
	t.Cleanup(func() { resolveInstalledWrapper = previous })
}

func doctorChecks(t *testing.T, result map[string]any) map[string]doctorCheck {
	t.Helper()
	checks, ok := result["checks"].([]doctorCheck)
	if !ok {
		t.Fatalf("checks type = %T", result["checks"])
	}
	byID := make(map[string]doctorCheck, len(checks))
	for _, check := range checks {
		byID[check.ID] = check
	}
	return byID
}
