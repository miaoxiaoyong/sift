package command

import (
	"errors"
	"fmt"

	"github.com/miaoxiaoyong/sift/internal/schema"
)

// CommandEventV1 is the closed, canonical command event (§6.1, max 64 KiB). It
// is emitted by the single transaction port and never contains natural-language
// input.
type CommandEventV1 struct {
	SchemaVersion   int            `json:"schema_version"` // 1
	EventKey        string         `json:"event_key"`
	Source          CommandSource  `json:"source"`
	RemoteEventID   string         `json:"remote_event_id"`
	Outcome         CommandOutcome `json:"outcome"`
	Action          *CommandAction `json:"action"`             // nullable
	RunID           *string        `json:"run_id"`             // nullable
	InterruptID     *string        `json:"interrupt_id"`       // nullable
	NextNonce       *string        `json:"next_nonce"`         // nullable 32 hex
	FinalForEventID *string        `json:"final_for_event_id"` // nullable
}

// CommandAckV1 is the closed, canonical acknowledgement (§6.1, max 8 KiB). It
// references the final CommandEventV1 by id.
type CommandAckV1 struct {
	SchemaVersion  int            `json:"schema_version"` // 1
	CommandEventID string         `json:"command_event_id"`
	Action         *CommandAction `json:"action"`       // nullable
	Disposition    CommandOutcome `json:"disposition"`  // non-pending
	RunID          *string        `json:"run_id"`       // nullable
	InterruptID    *string        `json:"interrupt_id"` // nullable
	NextNonce      *string        `json:"next_nonce"`   // nullable
}

const (
	// MaxEventBytes / MaxAckBytes are the closed-schema size limits.
	MaxEventBytes = 64 * 1024
	MaxAckBytes   = 8 * 1024
)

// Stage keys (§6.1). A non-retry command has exactly command:<key>:initial
// (that event is both initial and final). A retry request has
// command:<key>:initial; its only legal final keys are the four below.
const (
	StageInitial             = "initial"
	StageFinalProbeSucceeded = "final:probe-succeeded"
	StageFinalProbeFailed    = "final:probe-failed"
	StageFinalFactWins       = "final:fact-wins"
	StageFinalDecisionWins   = "final:decision-wins"
)

// EventStageKey returns the idempotency stage key for a command event.
func EventStageKey(eventKey, stage string) string {
	return "command:" + eventKey + ":" + stage
}

// AckOperationKey returns the final ack operation key.
func AckOperationKey(eventKey string) string {
	return "command:" + eventKey + ":ack"
}

// FinalStageForOutcome maps a final retry outcome to its stage suffix. For
// non-final or non-retry outcomes it returns the empty string.
func FinalStageForOutcome(o CommandOutcome) string {
	switch o {
	case OutcomeAbsenceUnconfirmed:
		return StageFinalProbeFailed
	case OutcomeSupersededByFact:
		return StageFinalFactWins
	case OutcomeSupersededByDecision:
		return StageFinalDecisionWins
	case OutcomeApplied:
		return StageFinalProbeSucceeded
	}
	return ""
}

// NewEvent assembles a CommandEventV1 from a compiled command and its outcome.
// Action/run/interrupt are nullable with the same limits as the envelope. It
// never echoes the submitted nonce: NextNonce is null or the newly issued
// current nonce. action is the resolved command action (approve for an
// approval_label candidate); pass the empty string to leave it null, which is
// only valid for rejected_syntax/rejected_target outcomes.
func NewEvent(env CommandEventEnvelopeV1, outcome CommandOutcome, action CommandAction, runID, interruptID, nextNonce string, finalForEventID string) CommandEventV1 {
	ev := CommandEventV1{
		SchemaVersion: 1,
		EventKey:      env.EventKey,
		Source:        env.Source,
		RemoteEventID: env.RemoteEventID,
		Outcome:       outcome,
	}
	if outcome != OutcomeRejectedSyntax && outcome != OutcomeRejectedTarget {
		ev.Action = ptrAction(action)
	}
	if runID != "" {
		r := runID
		ev.RunID = &r
	}
	if interruptID != "" {
		i := interruptID
		ev.InterruptID = &i
	}
	if nextNonce != "" {
		n := nextNonce
		ev.NextNonce = &n
	}
	if finalForEventID != "" {
		f := finalForEventID
		ev.FinalForEventID = &f
	}
	return ev
}

func ptrAction(a CommandAction) *CommandAction {
	if a == "" {
		a = ActionApprove
	}
	return &a
}

// NewAck assembles a CommandAckV1 referencing the final event. Non-null
// action/run/Interrupt must equal that final event.
func NewAck(finalEventID string, ev CommandEventV1) CommandAckV1 {
	return CommandAckV1{
		SchemaVersion:  1,
		CommandEventID: finalEventID,
		Action:         ev.Action,
		Disposition:    ev.Outcome,
		RunID:          ev.RunID,
		InterruptID:    ev.InterruptID,
		NextNonce:      ev.NextNonce,
	}
}

// CanonicalBytes returns the canonical JSON for an event, rejecting unknown
// fields, non-canonical JSON or incompatible nulls at marshal time.
func (e CommandEventV1) CanonicalBytes() ([]byte, error) {
	b, err := schema.Canonical(e)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxEventBytes {
		return nil, fmt.Errorf("command: event exceeds %d bytes", MaxEventBytes)
	}
	if err := e.invariant(); err != nil {
		return nil, err
	}
	return b, nil
}

// CanonicalBytes returns the canonical JSON for an ack.
func (a CommandAckV1) CanonicalBytes() ([]byte, error) {
	b, err := schema.Canonical(a)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxAckBytes {
		return nil, fmt.Errorf("command: ack exceeds %d bytes", MaxAckBytes)
	}
	if !a.Disposition.IsFinal() {
		return nil, errors.New("command: ack disposition must be final")
	}
	return b, nil
}

// invariant enforces the closed-field rules that JSON tags cannot express.
func (e CommandEventV1) invariant() error {
	if e.SchemaVersion != 1 {
		return errors.New("command: event schema_version must be 1")
	}
	if e.Outcome == "" {
		return errors.New("command: event outcome is required")
	}
	// rejected_syntax/rejected_target carry null action/run/interrupt; every
	// other outcome requires a non-null action.
	if e.Outcome != OutcomeRejectedSyntax && e.Outcome != OutcomeRejectedTarget {
		if e.Action == nil {
			return errors.New("command: event action is required for this outcome")
		}
	}
	if e.NextNonce != nil && (len(*e.NextNonce) != 32 || !isLowerHex(*e.NextNonce)) {
		return errors.New("command: next_nonce must be 32 lowercase hex")
	}
	if e.FinalForEventID != nil && *e.FinalForEventID == "" {
		return errors.New("command: final_for_event_id must not be empty")
	}
	return nil
}

// String is a debug helper; it is never part of the wire format.
func (a CommandAction) String() string { return string(a) }
