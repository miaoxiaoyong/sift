package command

import "sort"

// CommandOutcome is the closed set of final command outcomes (§6.1 mapping
// table). It is the "outcome" field of CommandEventV1 and the "disposition" of
// CommandAckV1.
type CommandOutcome string

const (
	OutcomeRejectedSyntax       CommandOutcome = "rejected_syntax"
	OutcomeRejectedTarget       CommandOutcome = "rejected_target"
	OutcomeRejectedStale        CommandOutcome = "rejected_stale"
	OutcomeRejectedOption       CommandOutcome = "rejected_option"
	OutcomeProbeInProgress      CommandOutcome = "probe_in_progress"
	OutcomeApplied              CommandOutcome = "applied"
	OutcomeRetryPending         CommandOutcome = "retry_pending"
	OutcomeAbsenceUnconfirmed   CommandOutcome = "absence_unconfirmed"
	OutcomeSupersededByFact     CommandOutcome = "superseded_by_fact"
	OutcomeSupersededByDecision CommandOutcome = "superseded_by_decision"
)

// IsFinal reports whether the outcome is a final (ackable) outcome rather than
// the pending retry-request outcome. retry_pending has no ack yet (§6.1).
func (o CommandOutcome) IsFinal() bool {
	switch o {
	case OutcomeRetryPending:
		return false
	}
	return true
}

// InterruptView is the minimal, frozen view of the current open Interrupt that
// the compiler needs. It is supplied by the transaction from its own snapshot;
// callers cannot provide mutable Run/Forge state.
type InterruptView struct {
	ID                          string
	RunID                       string
	Version                     int64
	RunVersion                  int64
	Reason                      InterruptReason
	Status                      InterruptStatus
	DispatchState               DispatchState
	Nonce                       string
	Options                     []string
	HoldMaxDurationMS           int64
	ApprovalLabelCutoffPosition *string // NULL or a canonical positive decimal
}

// InterruptReason mirrors storage.InterruptReason without the import cycle.
type InterruptReason string

const (
	ReasonDesignApproval     InterruptReason = "design_approval"
	ReasonGuardrailViolation InterruptReason = "guardrail_violation"
	ReasonCodeReview         InterruptReason = "code_review"
	ReasonAgentBlocked       InterruptReason = "agent_blocked"
	ReasonMergeConflict      InterruptReason = "merge_conflict"
	ReasonFailureReview      InterruptReason = "failure_review"
	ReasonStartupStall       InterruptReason = "startup_stall"
)

// InterruptStatus and DispatchState mirror the storage CHECK domains.
type InterruptStatus string
type DispatchState string

const (
	StatusOpen              InterruptStatus = "open"
	StatusClosed            InterruptStatus = "closed"
	DispatchReady           DispatchState   = "ready"
	DispatchBatched         DispatchState   = "batched"
	DispatchHeld            DispatchState   = "held"
	DispatchProbeInProgress DispatchState   = "probe_in_progress"
)

// CompiledCommandV1 is the executable input to the single transaction (§4). The
// executable fields are populated only after the immutable target, one open
// Interrupt, current nonce/cutoff and option validation. The transaction loads
// its own current snapshots; callers cannot provide options, severity or SQL.
type CompiledCommandV1 struct {
	Envelope                 CommandEventEnvelopeV1
	Action                   CommandAction
	RunID                    string
	Nonce                    string
	HoldDurationMS           int64
	RejectReason             string
	AskText                  string
	InterruptID              string
	ExpectedRunVersion       int64
	ExpectedInterruptVersion int64
}

// Authorizer is the static forge actor allowlist from the Run's immutable
// config snapshot. It authenticates only; it never bypasses CAS, target,
// nonce, cutoff, Gate, isolation or options (§1.4).
type Authorizer struct {
	allowlist map[string]struct{}
}

// NewAuthorizer builds an allowlist from the resolved config operators. The
// list is normalized (de-duplicated) but never trimmed or case-folded: an exact
// match is required.
func NewAuthorizer(actors []string) *Authorizer {
	m := make(map[string]struct{}, len(actors))
	for _, a := range actors {
		if a != "" {
			m[a] = struct{}{}
		}
	}
	return &Authorizer{allowlist: m}
}

// Trusted reports whether actor is in the immutable allowlist. A nil/empty
// actor is never trusted.
func (a *Authorizer) Trusted(actor *string) bool {
	if actor == nil || *actor == "" {
		return false
	}
	_, ok := a.allowlist[*actor]
	return ok
}

// CompileResult is the outcome of attempting to compile a trusted candidate.
// Outcome is always set. Compiled is populated only when Outcome is applied or
// retry_pending; for rejections it is zero.
type CompileResult struct {
	Outcome  CommandOutcome
	Compiled CompiledCommandV1
}

// Compile validates the immutable target, current Interrupt, nonce/cutoff and
// options, then assembles the executable fields (§4). It must be called only
// for a trusted, syntactically-valid candidate. approval_label candidates
// compile to ActionApprove without grammar parsing.
//
// Parameters:
//   - env: the validated, event-key-verified envelope
//   - parsed: the grammar result (zero value for approval_label)
//   - interrupt: the frozen current-Interrupt view, or nil if none is open
//   - bindingTarget: the immutable target bound to the Interrupt's initial
//     forge_comment publish operation (env.Target must equal it byte-for-byte)
func Compile(env CommandEventEnvelopeV1, parsed ParsedCommand, interrupt *InterruptView, bindingTarget CommandTarget) CompileResult {
	if interrupt == nil || interrupt.Status != StatusOpen {
		return CompileResult{Outcome: OutcomeRejectedTarget}
	}
	// Immutable target: compare only with the binding, never with current
	// Run/Issue/Change/comment text.
	if env.Target != bindingTarget {
		return CompileResult{Outcome: OutcomeRejectedTarget}
	}
	action := parsed.Action
	if env.Source == SourceApprovalLabel {
		action = ActionApprove
	}
	// nonce / cutoff anti-replay.
	if env.Source == SourceForgeComment {
		if parsed.Nonce != interrupt.Nonce {
			return CompileResult{Outcome: OutcomeRejectedStale}
		}
		if parsed.RunID != interrupt.RunID {
			return CompileResult{Outcome: OutcomeRejectedStale}
		}
	} else { // approval_label
		pos := env.LabelPosition
		if pos == nil || !labelCurrent(*pos, interrupt.ApprovalLabelCutoffPosition) {
			return CompileResult{Outcome: OutcomeRejectedStale}
		}
	}
	// options: action must be in current options[]. startup_stall exposes only
	// retry/reject/hold, so approve is rejected here (§5).
	if !optionAllowed(action, interrupt.Options) {
		return CompileResult{Outcome: OutcomeRejectedOption}
	}
	// startup_stall: a probe already in flight rejects every later candidate
	// with one final probe_in_progress outcome (§5).
	if interrupt.Reason == ReasonStartupStall && interrupt.DispatchState == DispatchProbeInProgress {
		return CompileResult{Outcome: OutcomeProbeInProgress}
	}
	if action == ActionHold && interrupt.HoldMaxDurationMS > 0 && parsed.HoldDurationMS > interrupt.HoldMaxDurationMS {
		return CompileResult{Outcome: OutcomeRejectedOption}
	}
	compiled := CompiledCommandV1{
		Envelope:                 env,
		Action:                   action,
		RunID:                    interrupt.RunID,
		Nonce:                    interrupt.Nonce,
		HoldDurationMS:           parsed.HoldDurationMS,
		RejectReason:             parsed.RejectReason,
		AskText:                  parsed.AskText,
		InterruptID:              interrupt.ID,
		ExpectedRunVersion:       interrupt.RunVersion,
		ExpectedInterruptVersion: interrupt.Version,
	}
	return CompileResult{Outcome: OutcomeApplied, Compiled: compiled}
}

// labelCurrent reports label_position > approval_label_cutoff_position (§3.2).
// Equality and all earlier positions reject. A NULL cutoff (zero sentinel)
// accepts every positive position.
func labelCurrent(position string, cutoff *string) bool {
	if position == "" {
		return false
	}
	if cutoff == nil || *cutoff == "" {
		return true
	}
	return decimalGreaterThan(position, *cutoff)
}

// optionAllowed reports whether action is present in the frozen options list.
// The list is the canonical option-id set rendered to the human.
func optionAllowed(action CommandAction, options []string) bool {
	for _, o := range options {
		if CommandAction(o) == action {
			return true
		}
	}
	return false
}

// decimalGreaterThan compares two canonical positive decimals without parsing
// to a fixed-width integer: a longer string is larger, and equal length
// compares lexicographically.
func decimalGreaterThan(a, b string) bool {
	if len(a) != len(b) {
		return len(a) > len(b)
	}
	return a > b
}

// SortedOptions returns a sorted copy of the option ids for deterministic
// rendering and tests.
func SortedOptions(options []string) []string {
	out := append([]string(nil), options...)
	sort.Strings(out)
	return out
}
