package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

const TopologyMethodVersion = "runtime-topology/v1"

// ErrImmutableSystemExecutable means Darwin's sealed system volume already
// supplies an immutable execution image, so no private hard link is possible.
var ErrImmutableSystemExecutable = errors.New("runtime: immutable system executable")

// QualificationProbeTimeout bounds execution of an untrusted Agent's version
// command. Version probes receive no inherited daemon environment.
const QualificationProbeTimeout = 15 * time.Second

type QualificationInput struct {
	AgentID       string
	Args          []string
	TaskTransport string
	VersionArgs   []string
	Executable    string
	GOOS          string
	GOARCH        string
	// Context and ProbeTimeout bound the untrusted version command. A nil
	// Context uses Background; a non-positive timeout uses the safe default.
	Context      context.Context
	ProbeTimeout time.Duration
}

type Qualification struct {
	Key, AgentDefinitionHash, ExecutablePath, ExecutableSHA256, VersionOutputDigest string
	MethodVersion, AgentID, GOOS, GOARCH                                            string
}

func sha256Hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// QualificationKey returns the canonical exact key for a frozen topology
// projection. Storage uses it again before accepting evidence, so callers
// cannot authorize a different executable under a supplied digest.
func QualificationKey(q Qualification) (string, error) {
	if q.MethodVersion != TopologyMethodVersion || q.AgentID == "" || q.AgentDefinitionHash == "" || q.ExecutablePath == "" || q.ExecutableSHA256 == "" || q.VersionOutputDigest == "" || q.GOOS == "" || q.GOARCH == "" {
		return "", errors.New("runtime: incomplete qualification projection")
	}
	keyJSON, err := json.Marshal(struct {
		SchemaVersion       int    `json:"schema_version"`
		MethodVersion       string `json:"method_version"`
		AgentID             string `json:"agent_id"`
		AgentDefinitionHash string `json:"agent_definition_hash"`
		ExecutablePath      string `json:"executable_path"`
		ExecutableSHA256    string `json:"executable_sha256"`
		VersionOutputDigest string `json:"version_output_digest"`
		GOOS                string `json:"goos"`
		GOARCH              string `json:"goarch"`
	}{1, q.MethodVersion, q.AgentID, q.AgentDefinitionHash, q.ExecutablePath, q.ExecutableSHA256, q.VersionOutputDigest, q.GOOS, q.GOARCH})
	if err != nil {
		return "", err
	}
	return sha256Hex(keyJSON), nil
}

// BuildQualification derives the exact identity used by the process-group gate.
// It performs no topology claim: a successful command probe is not evidence of
// process-group qualification.
func BuildQualification(in QualificationInput) (Qualification, error) {
	if in.AgentID == "" || in.Executable == "" {
		return Qualification{}, errors.New("runtime: qualification requires agent and executable")
	}
	path, err := filepath.Abs(in.Executable)
	if err != nil {
		return Qualification{}, err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return Qualification{}, fmt.Errorf("runtime: resolve agent executable: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return Qualification{}, fmt.Errorf("runtime: agent executable is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Qualification{}, err
	}
	definition := struct {
		SchemaVersion int      `json:"schema_version"`
		Args          []string `json:"args"`
		TaskTransport string   `json:"task_transport"`
		VersionArgs   []string `json:"version_args"`
	}{1, in.Args, in.TaskTransport, in.VersionArgs}
	definitionJSON, _ := json.Marshal(definition)
	stdoutBytes, stderrBytes, err := ProbeVersion(in.Context, path, in.VersionArgs, in.ProbeTimeout)
	if err != nil {
		return Qualification{}, fmt.Errorf("runtime: agent version probe: %w", err)
	}
	versionBytes := append(append(append([]byte{}, stdoutBytes...), 0), stderrBytes...)
	versionDigest := sha256Hex(versionBytes)
	q := Qualification{MethodVersion: TopologyMethodVersion, AgentID: in.AgentID, AgentDefinitionHash: sha256Hex(definitionJSON), ExecutablePath: path, ExecutableSHA256: sha256Hex(data), VersionOutputDigest: versionDigest, GOOS: in.GOOS, GOARCH: in.GOARCH}
	if q.GOOS == "" {
		q.GOOS = runtime.GOOS
	}
	if q.GOARCH == "" {
		q.GOARCH = runtime.GOARCH
	}
	q.Key, err = QualificationKey(q)
	if err != nil {
		return Qualification{}, err
	}
	return q, nil
}

// ProbeVersion runs only an Agent's version command with a bounded context and
// an empty environment. It is deliberately separate from task launch: no task
// input, run directory, or daemon credential can enter this probe.
func ProbeVersion(parent context.Context, executable string, args []string, timeout time.Duration) ([]byte, []byte, error) {
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		timeout = QualificationProbeTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Env = []string{}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return stdout.Bytes(), stderr.Bytes(), ctx.Err()
	}
	if err != nil {
		return stdout.Bytes(), stderr.Bytes(), err
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}

// MaterializeQualifiedExecutable verifies the path's current bytes through a
// single open descriptor, then returns an executable image separate from
// ExecutablePath. Linux uses an unlinked read-only copy; Darwin uses a private
// hard link. It must be released by its caller after Launcher.Start has
// inherited it.
func MaterializeQualifiedExecutable(q Qualification) (*os.File, error) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return nil, errors.New("runtime: sealed executable images are unsupported on this platform")
	}
	if q.ExecutablePath == "" || q.ExecutableSHA256 == "" {
		return nil, errors.New("runtime: incomplete executable qualification")
	}
	source, err := os.Open(q.ExecutablePath)
	if err != nil {
		return nil, fmt.Errorf("runtime: open qualified executable: %w", err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("runtime: qualified executable is unavailable")
	}
	data, err := io.ReadAll(source)
	if err != nil {
		return nil, fmt.Errorf("runtime: read qualified executable: %w", err)
	}
	if sha256Hex(data) != q.ExecutableSHA256 {
		return nil, errors.New("runtime: qualified executable changed")
	}

	dir, err := os.MkdirTemp("", "sift-agent-image-")
	if err != nil {
		return nil, fmt.Errorf("runtime: create executable image directory: %w", err)
	}
	name := filepath.Join(dir, "agent")
	if runtime.GOOS == "darwin" {
		// The link and its post-link hash bind exec to one inode. A later rename
		// of ExecutablePath cannot change the private link.
		if err := os.Link(q.ExecutablePath, name); err != nil {
			_ = os.RemoveAll(dir)
			if darwinSystemExecutable(q.ExecutablePath) {
				return nil, fmt.Errorf("%w: %v", ErrImmutableSystemExecutable, err)
			}
			return nil, fmt.Errorf("runtime: link executable image: %w", err)
		}
		linked, err := os.ReadFile(name)
		if err != nil || sha256Hex(linked) != q.ExecutableSHA256 {
			_ = os.RemoveAll(dir)
			return nil, errors.New("runtime: qualified executable changed")
		}
		image, err := os.Open(name)
		if err != nil {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("runtime: open executable image: %w", err)
		}
		return image, nil
	}
	image, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0500)
	if err != nil {
		_ = os.Remove(dir)
		return nil, fmt.Errorf("runtime: create executable image: %w", err)
	}
	defer os.RemoveAll(dir)
	if _, err := image.Write(data); err != nil {
		ReleaseExecutableImage(image)
		return nil, fmt.Errorf("runtime: write executable image: %w", err)
	}
	if err := image.Close(); err != nil {
		return nil, fmt.Errorf("runtime: seal executable image: %w", err)
	}
	image, err = os.Open(name)
	if err != nil {
		return nil, fmt.Errorf("runtime: reopen executable image: %w", err)
	}
	return image, nil
}

// ValidateQualificationExecutable closes the measurement-to-exec boundary.
// The launch worker repeats this check before handing off to the wrapper.
func ValidateQualificationExecutable(q Qualification) error {
	if q.ExecutablePath == "" || q.ExecutableSHA256 == "" {
		return errors.New("runtime: incomplete executable qualification")
	}
	info, err := os.Stat(q.ExecutablePath)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("runtime: qualified executable is unavailable")
	}
	data, err := os.ReadFile(q.ExecutablePath)
	if err != nil {
		return fmt.Errorf("runtime: read qualified executable: %w", err)
	}
	if sha256Hex(data) != q.ExecutableSHA256 {
		return errors.New("runtime: qualified executable changed")
	}
	return nil
}

type QualificationStatus string

const (
	ProcessGroupVerified   QualificationStatus = "process-group-verified"
	ProcessGroupUnverified QualificationStatus = "process-group-unverified"
)

type QualificationEvidence struct {
	Status         QualificationStatus
	Reason         string
	EvidenceJSON   string
	EvidenceDigest string
}
