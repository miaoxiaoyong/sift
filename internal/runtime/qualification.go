package runtime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const TopologyMethodVersion = "runtime-topology/v1"

type QualificationInput struct {
	AgentID       string
	Args          []string
	TaskTransport string
	VersionArgs   []string
	Executable    string
	GOOS          string
	GOARCH        string
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
	version := exec.Command(path, in.VersionArgs...)
	var stdout, stderr bytes.Buffer
	version.Stdout, version.Stderr = &stdout, &stderr
	if err := version.Run(); err != nil {
		return Qualification{}, fmt.Errorf("runtime: agent version probe: %w", err)
	}
	versionBytes := append(append(append([]byte{}, stdout.Bytes()...), 0), stderr.Bytes()...)
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
