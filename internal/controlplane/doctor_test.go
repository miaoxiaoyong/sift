package controlplane

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/storage"
)

func TestDoctorBaselineChecksConfiguredDependencies(t *testing.T) {
	stubDoctorInstall(t, Version, ProtocolMajor)
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

func TestDoctorMissingDatabaseKeepsIndependentVersionAndOutboxChecks(t *testing.T) {
	home := testHome(t)
	stubDoctorInstall(t, "9.9.9-other", ProtocolMajor)
	result := doctor(context.Background(), true, home)
	checks := doctorChecks(t, result)
	for _, id := range []string{"version:wrapper", "version:daemon", "outbox:backlog", "outbox:push-failures"} {
		if check, ok := checks[id]; !ok || check.Level == "ok" {
			t.Fatalf("%s = %#v, want independent error", id, check)
		}
	}
	if checks["version:wrapper"].Level != "error" {
		t.Fatalf("wrapper mismatch = %#v", checks["version:wrapper"])
	}
}

func TestDoctorCorruptDatabaseKeepsIndependentChecks(t *testing.T) {
	home := testHome(t)
	if err := os.WriteFile(filepath.Join(home.Path, "sift.db"), []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	checks := doctorChecks(t, doctor(context.Background(), true, home))
	for _, id := range []string{"version:wrapper", "version:daemon", "outbox:backlog", "outbox:push-failures"} {
		if check, ok := checks[id]; !ok || check.Level == "ok" {
			t.Fatalf("%s = %#v, want independent non-ok result", id, check)
		}
	}
}

func TestDoctorReportsPushFailuresByKind(t *testing.T) {
	home := testHome(t)
	db, err := storage.Open(context.Background(), storage.OpenConfig{Path: filepath.Join(home.Path, "sift.db"), BinaryVersion: Version, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecForTest(context.Background(), `INSERT INTO outbox_operations (id,operation_key,kind,state,payload_schema_version,payload_json,payload_digest,attempt_count,next_attempt_at_ms,created_at_ms,updated_at_ms) VALUES ('local','local-key','launch_agent','failed',1,'{}','d',1,0,0,0), ('remote','remote-key','channel_publish','failed',1,'{}','d',1,0,0,0)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	checks := doctorChecks(t, doctor(context.Background(), true, home))
	if checks["outbox:push-failures"].Details["failed_count"] != 1 {
		t.Fatalf("push failures = %#v", checks["outbox:push-failures"])
	}
	if checks["outbox:failures"].Details["failed_count"] != 2 {
		t.Fatalf("generic failures = %#v", checks["outbox:failures"])
	}
}

func TestDoctorReportsDaemonProtocolMismatch(t *testing.T) {
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

// TestDoctorWrapperProtocolMismatch proves version:wrapper pairs only when
// the installed wrapper actually reports the daemon's protocol major: a
// fixture wrapper with the same SemVer but a different --protocol-major must
// surface as an error through the actual probe, not a daemon constant.
func TestDoctorWrapperProtocolMismatch(t *testing.T) {
	home := testHome(t)
	stubDoctorInstall(t, Version, ProtocolMajor+1)

	checks := doctorChecks(t, doctor(context.Background(), true, home))
	if checks["version:wrapper"].Level != "error" {
		t.Fatalf("wrapper version check = %#v", checks["version:wrapper"])
	}
	if !strings.Contains(checks["version:wrapper"].Message, "protocol major") {
		t.Fatalf("wrapper check message = %q, want protocol major mismatch", checks["version:wrapper"].Message)
	}
}

// TestDoctorWrapperUniqueAndPairing proves version:wrapper is emitted exactly
// once per doctor run and is graded by the actual daemon↔wrapper probe, never
// by client-reported envelope values. Three fixtures: a matching install, a
// client that differs from the daemon (the wrapper check must still pair
// daemon↔wrapper and stay ok without backfilling client input), and a wrapper
// that matches the stale client but not the daemon (a single error, never an
// error/ok conflict under the same ID).
func TestDoctorWrapperUniqueAndPairing(t *testing.T) {
	home := testHome(t)

	wrapperChecks := func(result map[string]any) []doctorCheck {
		t.Helper()
		checks, ok := result["checks"].([]doctorCheck)
		if !ok {
			t.Fatalf("checks type = %T", result["checks"])
		}
		var matches []doctorCheck
		for _, check := range checks {
			if check.ID == "version:wrapper" {
				matches = append(matches, check)
			}
		}
		return matches
	}

	t.Run("matching install emits one ok check with probed values", func(t *testing.T) {
		stubDoctorInstall(t, Version, ProtocolMajor)
		matches := wrapperChecks(doctorWithVersions(context.Background(), true, home, Version, ProtocolMajor, nil))
		if len(matches) != 1 {
			t.Fatalf("version:wrapper count = %d, want exactly 1: %#v", len(matches), matches)
		}
		check := matches[0]
		if check.Level != "ok" {
			t.Fatalf("version:wrapper = %#v, want ok", check)
		}
		if check.Details["wrapper_version"] != Version || check.Details["wrapper_protocol_major"] != ProtocolMajor {
			t.Fatalf("details = %v, want actual probed wrapper values", check.Details)
		}
	})

	t.Run("client differs from daemon keeps daemon wrapper pairing", func(t *testing.T) {
		stubDoctorInstall(t, Version, ProtocolMajor)
		matches := wrapperChecks(doctorWithVersions(context.Background(), true, home, "2.0.0", ProtocolMajor+1, nil))
		if len(matches) != 1 {
			t.Fatalf("version:wrapper count = %d, want exactly 1: %#v", len(matches), matches)
		}
		check := matches[0]
		if check.Level != "ok" {
			t.Fatalf("version:wrapper = %#v, want ok: daemon and wrapper are paired", check)
		}
		if check.Details["wrapper_version"] != Version || check.Details["wrapper_protocol_major"] != ProtocolMajor {
			t.Fatalf("details = %v, want actual daemon-side probe, not client input", check.Details)
		}
		for key, value := range check.Details {
			if value == "2.0.0" || value == ProtocolMajor+1 {
				t.Fatalf("details[%q] = %v, client input must not backfill wrapper details", key, value)
			}
		}
	})

	t.Run("wrapper matching stale client but not daemon is one error", func(t *testing.T) {
		stubDoctorInstall(t, "2.0.0", ProtocolMajor+1)
		matches := wrapperChecks(doctorWithVersions(context.Background(), true, home, "2.0.0", ProtocolMajor+1, nil))
		if len(matches) != 1 {
			t.Fatalf("version:wrapper count = %d, want exactly 1 (no error/ok conflict): %#v", len(matches), matches)
		}
		check := matches[0]
		if check.Level != "error" {
			t.Fatalf("version:wrapper = %#v, want error: wrapper does not pair with the daemon", check)
		}
		if !strings.Contains(check.Message, "2.0.0") {
			t.Fatalf("message = %q, want the actual probed wrapper version", check.Message)
		}
	})
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

// stubDoctorInstall points the doctor's wrapper probe at a fabricated install
// directory whose wrapper answers --version/--protocol-major with the given
// values.
func stubDoctorInstall(t *testing.T, wrapperVersion string, wrapperProtocolMajor int) {
	t.Helper()
	dir := t.TempDir()
	daemon := filepath.Join(dir, "sift")
	if err := os.WriteFile(daemon, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then printf '%s\\n'; fi\nif [ \"$1\" = \"--protocol-major\" ]; then printf '%d\\n'; fi\n", wrapperVersion, wrapperProtocolMajor)
	if err := os.WriteFile(filepath.Join(dir, "sift-agent-wrapper"), []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	previous := doctorExecutable
	doctorExecutable = func() (string, error) { return daemon, nil }
	t.Cleanup(func() { doctorExecutable = previous })
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
