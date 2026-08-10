// Package controlplane implements the V0 local Unix-socket RPC boundary.
package controlplane

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"regexp"

	"github.com/miaoxiaoyong/sift/internal/version"
)

const (
	ProtocolMajor = 1
	ProtocolMinor = 0
	MaxFrame      = 1048576
)

// Version is the canonical binary/release SemVer carried in every RPC
// envelope (client_version / server_version). It is the release version
// injected through ldflags (internal/version.Release), separate from the wire
// protocol version (ProtocolMajor/Minor) and from the config file protocol
// version (config.Version). control-plane.md §3.4 defines the handshake.
var Version = version.Release

var requestID = regexp.MustCompile(`^[0-9a-f]{32}$`)

type Auth struct {
	Kind    string `json:"kind"`
	Token   string `json:"token,omitempty"`
	Nonce   string `json:"nonce,omitempty"`
	Session string `json:"session,omitempty"`
	Permit  string `json:"permit,omitempty"`
}

type Request struct {
	ProtocolMajor int            `json:"protocol_major"`
	ProtocolMinor int            `json:"protocol_minor"`
	ClientVersion string         `json:"client_version"`
	RequestID     string         `json:"request_id"`
	Method        string         `json:"method"`
	Auth          Auth           `json:"auth"`
	Params        map[string]any `json:"params"`
}

type Error struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details"`
}

type Response struct {
	ProtocolMajor int    `json:"protocol_major"`
	ProtocolMinor int    `json:"protocol_minor"`
	ServerVersion string `json:"server_version"`
	RequestID     string `json:"request_id"`
	OK            bool   `json:"ok"`
	Result        any    `json:"result,omitempty"`
	Error         *Error `json:"error,omitempty"`
}

func failure(id, code, message string, retryable bool) Response {
	return Response{ProtocolMajor: ProtocolMajor, ProtocolMinor: ProtocolMinor, ServerVersion: Version, RequestID: id, Error: &Error{Code: code, Message: message, Retryable: retryable, Details: map[string]any{}}, OK: false}
}

func success(id string, result any) Response {
	return Response{ProtocolMajor: ProtocolMajor, ProtocolMinor: ProtocolMinor, ServerVersion: Version, RequestID: id, OK: true, Result: result}
}

func validToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil && token == stringLower(token)
}
func stringLower(s string) string {
	for _, c := range s {
		if c >= 'A' && c <= 'F' {
			return ""
		}
	}
	return s
}

func matchesToken(actual, presented string) bool {
	if !validToken(presented) {
		return false
	}
	a := sha256.Sum256([]byte(actual))
	b := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func validateEnvelope(r Request) (string, string) {
	// doctor is the one read-only endpoint that must remain callable across a
	// release boundary: it reports the mismatch instead of hiding it behind a
	// handshake rejection. All other methods retain strict envelope gating.
	if r.Method != "ops.doctor" && (r.ProtocolMajor != ProtocolMajor || r.ProtocolMinor > ProtocolMinor) {
		return "unsupported_protocol", "protocol version is not supported"
	}
	if len(r.ClientVersion) < 3 || r.ClientVersion[0] < '0' || r.ClientVersion[0] > '9' || r.ClientVersion[1] != '.' {
		return "unsupported_binary", "binary version is invalid"
	}
	if r.Method != "ops.doctor" && majorVersion(r.ClientVersion) != majorVersion(Version) {
		return "unsupported_binary", "binary major version differs"
	}
	if !requestID.MatchString(r.RequestID) {
		return "invalid_request", "invalid request id"
	}
	if r.Params == nil {
		return "invalid_request", "params is required"
	}
	return "", ""
}

func socketPath(home, socket string) string { return home + "/" + socket }
func errf(format string, a ...any) error    { return fmt.Errorf("control plane: "+format, a...) }
