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
	ID         string `json:"id"`
	Executable string `json:"executable"`
	// ExecutableSHA256 binds the wrapper's final exec to the bytes measured for
	// the attempt's topology qualification key.
	ExecutableSHA256 string   `json:"executable_sha256,omitempty"`
	Args             []string `json:"args"`
	TaskTransport    string   `json:"task_transport"`
	// LaunchEnv is the Agent's frozen HOME/PATH snapshot (config.md §3.2),
	// carried so the wrapper's sole Launcher call uses the exact environment
	// the qualification probe measured (issue #993).
	LaunchEnv map[string]string `json:"launch_env,omitempty"`
}
