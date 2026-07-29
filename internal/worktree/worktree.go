// Package worktree owns the git operations Sift performs for an attempt.
// Agent-controlled git configuration is never used for these operations.
package worktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/miaoxiaoyong/sift/internal/attempt"
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
var safeBranch = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./-]*[A-Za-z0-9]$`)

var ErrNoCommit = errors.New("worktree: no commit beyond base")
var ErrEvidenceRejected = errors.New("worktree: success evidence rejected")

type Manager struct{ repo, root string }
type Worktree struct{ Path, Branch, Base string }

func NewManager(repo, root string) (*Manager, error) {
	if !filepath.IsAbs(repo) || !filepath.IsAbs(root) {
		return nil, errors.New("worktree: repo and root must be absolute")
	}
	if st, err := os.Stat(repo); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("worktree: invalid repository: %w", err)
	}
	return &Manager{repo: filepath.Clean(repo), root: filepath.Clean(root)}, nil
}

func (m *Manager) command(ctx context.Context, args ...string) *exec.Cmd {
	// This option must be present on every Sift-owned git invocation.
	argv := append([]string{"-C", m.repo, "-c", "core.hooksPath=/dev/null"}, args...)
	return exec.CommandContext(ctx, "git", argv...)
}

func (m *Manager) Create(ctx context.Context, runID string, attemptNo int, base, branch string) (Worktree, error) {
	if m == nil {
		return Worktree{}, errors.New("worktree: nil manager")
	}
	if !safeID.MatchString(runID) || attemptNo < 1 || !safeBranch.MatchString(branch) || strings.Contains(branch, "..") || strings.TrimSpace(base) == "" {
		return Worktree{}, errors.New("worktree: invalid run, attempt, base, or branch")
	}
	path := filepath.Join(m.root, runID, strconv.Itoa(attemptNo))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return Worktree{}, err
	}
	if out, err := m.command(ctx, "worktree", "add", "-b", branch, path, base).CombinedOutput(); err != nil {
		return Worktree{}, fmt.Errorf("worktree: add: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return Worktree{Path: path, Branch: branch, Base: base}, nil
}

func (m *Manager) Remove(ctx context.Context, wt Worktree) error {
	if m == nil || !filepath.IsAbs(wt.Path) {
		return errors.New("worktree: invalid worktree")
	}
	out, err := m.command(ctx, "worktree", "remove", "--force", wt.Path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("worktree: remove: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ReadBaseFile reads policy/context from the requested base, never from wt.
// A missing optional file is represented as an empty byte slice.
func (m *Manager) ReadBaseFile(ctx context.Context, base, name string) ([]byte, error) {
	if m == nil || strings.TrimSpace(base) == "" || name == "" || filepath.IsAbs(name) || strings.Contains(name, "..") {
		return nil, errors.New("worktree: invalid base file")
	}
	if _, err := m.command(ctx, "rev-parse", "--verify", base+"^{commit}").Output(); err != nil {
		return nil, fmt.Errorf("worktree: invalid base: %w", err)
	}
	out, err := m.command(ctx, "show", base+":"+filepath.ToSlash(name)).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return []byte{}, nil
		}
		return nil, fmt.Errorf("worktree: read base file: %w", err)
	}
	return out, nil
}

type ReadyChange struct{ RunID, Branch, Base, FinalHeadSHA string }

// EvaluateSuccess freezes the observed final head and is the only operation
// that can produce the domain fact used by the later Change reconciler.
func (m *Manager) EvaluateSuccess(ctx context.Context, wt Worktree, runID string, result attempt.Result, expected attempt.Identity) (ReadyChange, error) {
	if wt.Path == "" || runID == "" || result.ExitCode == nil || *result.ExitCode != 0 || result.Signal != "" || result.FinalHeadSHA == "" || result.Digest == "" {
		return ReadyChange{}, ErrEvidenceRejected
	}
	if result.Agent != expected || expected.PID <= 0 || expected.StartedAtMS <= 0 || expected.Executable == "" {
		return ReadyChange{}, fmt.Errorf("%w: agent identity mismatch", ErrEvidenceRejected)
	}
	head, err := m.revParse(ctx, wt.Path, "HEAD")
	if err != nil || head != result.FinalHeadSHA {
		return ReadyChange{}, fmt.Errorf("%w: final head mismatch", ErrEvidenceRejected)
	}
	count, err := m.revListCount(ctx, wt.Path, wt.Base+"..HEAD")
	if err != nil || count < 1 {
		return ReadyChange{}, ErrNoCommit
	}
	// The returned SHA is immutable evidence; callers must not re-read HEAD
	// after this point when constructing the createChange operation.
	return ReadyChange{RunID: runID, Branch: wt.Branch, Base: wt.Base, FinalHeadSHA: head}, nil
}

func (m *Manager) revParse(ctx context.Context, dir, ref string) (string, error) {
	out, err := m.command(ctx, "-C", dir, "rev-parse", "--verify", ref).Output()
	return strings.TrimSpace(string(out)), err
}
func (m *Manager) revListCount(ctx context.Context, dir, spec string) (int, error) {
	out, err := m.command(ctx, "-C", dir, "rev-list", "--count", spec).Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// ReadResult decodes the wrapper result file without accepting unknown fields.
func ReadResult(path string) (attempt.Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return attempt.Result{}, err
	}
	var wire struct {
		SchemaVersion     int              `json:"schema_version"`
		RunID             string           `json:"run_id"`
		AttemptNo         int              `json:"attempt_no"`
		Generation        int              `json:"generation"`
		WrapperInstanceID string           `json:"wrapper_instance_id"`
		AgentIdentity     attempt.Identity `json:"agent_identity"`
		ExitCode          *int             `json:"exit_code"`
		Signal            *string          `json:"signal"`
		FinalHeadSHA      string           `json:"final_head_sha"`
		ControlDigest     string           `json:"control_digest"`
		Digest            string           `json:"digest"`
		FinishedAt        string           `json:"finished_at"`
		FinishedAtMS      int64            `json:"finished_at_ms"`
		Agent             attempt.Identity `json:"agent"`
		AgentPID          int              `json:"agent_pid"`
		AgentStartedAtMS  int64            `json:"agent_started_at_ms"`
		AgentExecutable   string           `json:"agent_executable"`
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return attempt.Result{}, fmt.Errorf("worktree: decode result: %w", err)
	}
	var finished time.Time
	if wire.FinishedAtMS > 0 {
		finished = time.UnixMilli(wire.FinishedAtMS)
	} else if wire.FinishedAt != "" {
		parsed, parseErr := time.Parse(time.RFC3339Nano, wire.FinishedAt)
		if parseErr != nil {
			return attempt.Result{}, fmt.Errorf("worktree: invalid finished_at: %w", parseErr)
		}
		finished = parsed
	}
	agent := wire.AgentIdentity
	if agent == (attempt.Identity{}) {
		agent = wire.Agent
	}
	if agent == (attempt.Identity{}) {
		agent = attempt.Identity{PID: wire.AgentPID, StartedAtMS: wire.AgentStartedAtMS, Executable: wire.AgentExecutable}
	}
	digest := wire.Digest
	if digest == "" {
		sum := sha256.Sum256(data)
		digest = hex.EncodeToString(sum[:])
	}
	signal := ""
	if wire.Signal != nil {
		signal = *wire.Signal
	}
	return attempt.Result{ExitCode: wire.ExitCode, Signal: signal, FinalHeadSHA: wire.FinalHeadSHA, Digest: digest, FinishedAt: finished, Agent: agent}, nil
}
