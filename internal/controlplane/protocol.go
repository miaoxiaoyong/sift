// Package controlplane implements the V0 local Unix-socket RPC boundary.
package controlplane

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"regexp"

	"github.com/xsift/sift/internal/version"
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
// It is a variable so integration tests can rewrite version.Release to
// assemble a genuinely different client release; the wire contract
// (ProtocolMajor/Minor) stays constant.
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

	envelopeValidated bool
}

// EnvelopeValidated reports whether this response passed client-side envelope,
// request-id, protocol, and binary-major validation.
func (r Response) EnvelopeValidated() bool { return r.envelopeValidated }

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
	// The handshake is fail-closed for every method, including the read-only
	// ops.doctor (release.md §4, control-plane.md §3.4): an incompatible
	// client is rejected here, and the CLI surfaces that rejection as the
	// version:daemon doctor error instead of the daemon relaxing the gate.
	// V0 is a closed contract (control-plane.md §3.2): protocol_minor must be
	// 0. Reject anything above the server minor without guessing compatibility,
	// and reject negative values just as fail-closed instead of treating them
	// as silently compatible.
	if r.ProtocolMajor != ProtocolMajor || r.ProtocolMinor < 0 || r.ProtocolMinor > ProtocolMinor {
		return "unsupported_protocol", "protocol version is not supported"
	}
	if !version.IsValidSemver(r.ClientVersion) {
		return "unsupported_binary", "binary version is invalid"
	}
	if majorVersion(r.ClientVersion) != majorVersion(Version) {
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
