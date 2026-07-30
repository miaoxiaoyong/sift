// Package command implements the M5 Command bootstrap: how an authenticated
// Forge comment (/sift …) or approval-label addition becomes a replayable
// domain command. This package owns the closed envelope, the canonical command
// identity, the byte grammar and the closed event/ack schemas. It contains no
// database or Forge IO: every external fact is supplied by the caller and the
// single transactional write port lives in package storage (ApplyCommandEvent).
//
// Authoritative source: docs/specs/command.md.
package command

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/miaoxiaoyong/sift/internal/schema"
)

// CommandSource is the closed set of candidate origins. It is exactly the
// envelope "source" field and the receipt event_kind.
type CommandSource string

const (
	SourceForgeComment  CommandSource = "forge_comment"
	SourceApprovalLabel CommandSource = "approval_label"
)

// CommandTargetKind is the immutable discussion target kind.
type CommandTargetKind string

const (
	TargetIssue  CommandTargetKind = "issue"
	TargetChange CommandTargetKind = "change"
)

// CommandTarget is the immutable (kind,id) bound to the initial forge_comment
// publish operation that created the Interrupt.
type CommandTarget struct {
	Kind CommandTargetKind `json:"kind"`
	ID   string            `json:"id"`
}

// CommandComment is the trusted comment candidate for forge_comment sources.
type CommandComment struct {
	ID   string `json:"id"`
	Body string `json:"body"`
}

// CommandLabel is the approval-label candidate. Only name==labels.approved and
// action=="added" may request approve; that decision is made by the caller from
// the immutable config snapshot, not by this struct.
type CommandLabel struct {
	EventID string `json:"event_id"`
	Name    string `json:"name"`
	Action  string `json:"action"`
}

// CommandEventEnvelopeV1 is the closed candidate envelope (§2.1). Unknown
// fields, duplicate JSON keys and size violations reject. Actor is explicitly
// nullable: it cannot be inferred from any other field.
type CommandEventEnvelopeV1 struct {
	SchemaVersion int             `json:"schema_version"`
	EventKey      string          `json:"event_key"`
	ProjectID     string          `json:"project_id"`
	Source        CommandSource   `json:"source"`
	RemoteEventID string          `json:"remote_event_id"`
	Target        CommandTarget   `json:"target"`
	Actor         *string         `json:"actor"`
	RawDigest     string          `json:"raw_digest"`
	OccurredAtMS  int64           `json:"occurred_at_ms"`
	Comment       *CommandComment `json:"comment"`
	Label         *CommandLabel   `json:"label"`
	LabelPosition *string         `json:"label_position"`
}

const (
	// MaxRemoteIDBytes is the 1–256 UTF-8 byte limit for a remote event id and
	// target id.
	MaxRemoteIDBytes = 256
	// MaxBodyBytes is the byte limit for a comment body.
	MaxBodyBytes = 16384
	// MaxLabelPositionDigits bounds the canonical positive decimal position.
	MaxLabelPositionDigits = 39
)

// ErrInvalidEnvelope is returned by Validate for any structural violation.
var ErrInvalidEnvelope = errors.New("command: invalid envelope")

// Validate enforces the closed envelope contract. It does not recompute the
// event key; use RecomputeEventKey for that and compare with EventKey.
func (e CommandEventEnvelopeV1) Validate() error {
	if e.SchemaVersion != 1 {
		return fmt.Errorf("%w: schema_version must be 1", ErrInvalidEnvelope)
	}
	if e.Source != SourceForgeComment && e.Source != SourceApprovalLabel {
		return fmt.Errorf("%w: source must be forge_comment or approval_label", ErrInvalidEnvelope)
	}
	if e.ProjectID == "" {
		return fmt.Errorf("%w: project_id is required", ErrInvalidEnvelope)
	}
	if !validRemoteID(e.RemoteEventID) {
		return fmt.Errorf("%w: remote_event_id must be 1–%d UTF-8 bytes without NUL", ErrInvalidEnvelope, MaxRemoteIDBytes)
	}
	if e.Target.Kind != TargetIssue && e.Target.Kind != TargetChange {
		return fmt.Errorf("%w: target.kind must be issue or change", ErrInvalidEnvelope)
	}
	if !validRemoteID(e.Target.ID) {
		return fmt.Errorf("%w: target.id must be 1–%d UTF-8 bytes without NUL", ErrInvalidEnvelope, MaxRemoteIDBytes)
	}
	if e.RawDigest == "" || len(e.RawDigest) != 64 || !isLowerHex(e.RawDigest) {
		return fmt.Errorf("%w: raw_digest must be 64 lowercase hex", ErrInvalidEnvelope)
	}
	if e.EventKey == "" || len(e.EventKey) != 64 || !isLowerHex(e.EventKey) {
		return fmt.Errorf("%w: event_key must be 64 lowercase hex", ErrInvalidEnvelope)
	}
	if e.OccurredAtMS < 0 {
		return fmt.Errorf("%w: occurred_at_ms must be nonnegative", ErrInvalidEnvelope)
	}
	switch e.Source {
	case SourceForgeComment:
		if e.Label != nil || e.LabelPosition != nil {
			return fmt.Errorf("%w: forge_comment must not carry label fields", ErrInvalidEnvelope)
		}
		if e.Comment == nil {
			return fmt.Errorf("%w: forge_comment requires comment", ErrInvalidEnvelope)
		}
		if e.Comment.ID != e.RemoteEventID {
			return fmt.Errorf("%w: comment.id must equal remote_event_id", ErrInvalidEnvelope)
		}
		if !validBody(e.Comment.Body) {
			return fmt.Errorf("%w: comment.body must be 1–%d UTF-8 bytes without NUL", ErrInvalidEnvelope, MaxBodyBytes)
		}
	case SourceApprovalLabel:
		if e.Comment != nil {
			return fmt.Errorf("%w: approval_label must not carry comment", ErrInvalidEnvelope)
		}
		if e.Label == nil {
			return fmt.Errorf("%w: approval_label requires label", ErrInvalidEnvelope)
		}
		if e.Label.EventID != e.RemoteEventID {
			return fmt.Errorf("%w: label.event_id must equal remote_event_id", ErrInvalidEnvelope)
		}
		if e.Label.Name == "" {
			return fmt.Errorf("%w: label.name is required", ErrInvalidEnvelope)
		}
		if e.Label.Action != "added" {
			return fmt.Errorf("%w: label.action must be added", ErrInvalidEnvelope)
		}
		if e.LabelPosition == nil {
			return fmt.Errorf("%w: approval_label requires label_position", ErrInvalidEnvelope)
		}
		if !validLabelPosition(*e.LabelPosition) {
			return fmt.Errorf("%w: label_position must be a canonical positive decimal", ErrInvalidEnvelope)
		}
	}
	return nil
}

// RecomputeEventKey returns SHA-256(canonical_json({"v":1,"project_id":…,
// "source":…,"remote_event_id":…})) as 64 lowercase hex (§1.3). It is the
// canonical command identity and must never be trusted from the adapter.
func RecomputeEventKey(projectID string, source CommandSource, remoteEventID string) (string, error) {
	if projectID == "" || (source != SourceForgeComment && source != SourceApprovalLabel) || remoteEventID == "" {
		return "", fmt.Errorf("command: event key requires project, source and remote id")
	}
	b, err := schema.Canonical(map[string]any{"v": 1, "project_id": projectID, "source": string(source), "remote_event_id": remoteEventID})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// VerifyEventKey reports whether the envelope's EventKey equals the recomputed
// canonical command identity.
func (e CommandEventEnvelopeV1) VerifyEventKey() bool {
	want, err := RecomputeEventKey(e.ProjectID, e.Source, e.RemoteEventID)
	if err != nil {
		return false
	}
	return subtleEqual(want, e.EventKey)
}

func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func validRemoteID(s string) bool {
	if len(s) == 0 || len(s) > MaxRemoteIDBytes {
		return false
	}
	return !strings.ContainsRune(s, '\x00')
}

func validBody(s string) bool {
	if len(s) == 0 || len(s) > MaxBodyBytes {
		return false
	}
	return !strings.ContainsRune(s, '\x00')
}

func validLabelPosition(s string) bool {
	if len(s) == 0 || len(s) > MaxLabelPositionDigits {
		return false
	}
	// Canonical positive decimal: no sign, no leading zero (unless "0", which
	// is not positive and therefore invalid as a position).
	if s[0] == '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
