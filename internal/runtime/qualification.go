package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

const TopologyMethodVersion = "runtime-topology/v1"

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

// ValidateQualificationExecutable closes the measurement-to-exec boundary.
// The wrapper repeats this check immediately before its sole Launcher call.
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
