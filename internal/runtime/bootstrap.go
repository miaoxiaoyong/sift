package runtime

import "encoding/json"

// Bootstrap is the owner-only v2 file passed from the launch worker to exactly
// one wrapper process. Secrets never leave this file except in RPC auth.
type Bootstrap struct {
	SchemaVersion      int             `json:"schema_version"`
	ProtocolMajor      int             `json:"protocol_major"`
	ProtocolMinor      int             `json:"protocol_minor"`
	DaemonVersion      string          `json:"daemon_version"`
	WrapperVersion     string          `json:"wrapper_version"`
	RunID              string          `json:"run_id"`
	AttemptNo          int             `json:"attempt_no"`
	Generation         int             `json:"generation"`
	DispatchID         string          `json:"dispatch_id"`
	BootstrapNonce     string          `json:"bootstrap_nonce"`
	RunToken           string          `json:"run_token"`
	RunDir             string          `json:"run_dir"`
	WorktreePath       string          `json:"worktree_path"`
	Agent              BootstrapAgent  `json:"agent"`
	TaskSpecSnapshotID string          `json:"task_spec_snapshot_id"`
	TaskSpec           json.RawMessage `json:"task_spec"`
}
type BootstrapAgent struct {
	ID            string   `json:"id"`
	Executable    string   `json:"executable"`
	Args          []string `json:"args"`
	TaskTransport string   `json:"task_transport"`
}
