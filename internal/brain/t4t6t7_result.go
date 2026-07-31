package brain

import (
	"context"
	"fmt"

	"github.com/miaoxiaoyong/sift/internal/schema"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// T4FallbackOutput preserves the complete frozen input skeleton. A fallback
// is not a reduced model-shaped brief: the Interrupt consumer needs all facts,
// verified links, and candidate option fields to call the deterministic emitter.
func T4FallbackOutput(in T4Input) []byte {
	o, err := schema.Canonical(in)
	if err != nil {
		panic(fmt.Sprintf("brain: T4 fallback must be canonical: %v", err))
	}
	return o
}

func T6FallbackOutput(in T6Input) []byte {
	delivery := T6Delivery("batch")
	if in.Candidate.Severity == "high" || in.Candidate.Severity == "critical" {
		delivery = "immediate"
	}
	channel := in.Candidate.DefaultChannelID
	downgrade := false
	rationale := "fallback"
	o, err := schema.Canonical(T6Output{Delivery: &delivery, ChannelID: &channel, SuggestedDowngrade: &downgrade, Rationale: &rationale})
	if err != nil {
		panic(fmt.Sprintf("brain: T6 fallback must be canonical: %v", err))
	}
	return o
}

// T4CallResult is a closed terminal union. Exactly one branch is populated.
type T4CallResult struct {
	Normal   *T4Output
	Fallback *T4Input
}

// CallT4 adapts the unified Brain shell to the Interrupt emitter's advisory
// callback. The shell persists the logical call and its trace; the emitter
// remains responsible for deterministic admission and the write transaction.
func (s *Shell) CallT4(ctx context.Context, in storage.InterruptT4Input) (storage.InterruptT4Output, error) {
	options := make([]T4Option, len(in.Options))
	for i, option := range in.Options {
		options[i] = T4Option{ID: option.ID, Label: option.Label, Effect: option.Effect, Risk: option.Risk}
	}
	links := make([]T4Link, len(in.Links))
	for i, link := range in.Links {
		links[i] = T4Link{Label: link.Label, Target: link.Target}
	}
	input := T4Input{RunID: in.RunID, AttemptNo: in.AttemptNo, Interrupt: T4Interrupt{Reason: InterruptReason(in.Reason), BaseSeverity: InterruptSeverity(in.Severity), MinModality: InterruptModality(in.Modality), FallbackHeadline: in.Headline, FallbackBrief: in.Brief, BriefFragments: append([]string(nil), in.Fragments...), Links: links, CandidateOptions: options}}
	canonical, err := BuildT4Input(input)
	if err != nil {
		return storage.InterruptT4Output{}, err
	}
	result, err := s.Call(ctx, T4Contract(input), CallParams{Scope: storage.BrainScopeRun, SubjectKey: "run:" + in.RunID, RunID: in.RunID, AttemptNo: in.AttemptNo, Input: canonical})
	if err != nil {
		return storage.InterruptT4Output{}, err
	}
	admitted, _, err := T4ResultFromCall(result, input)
	if err != nil || admitted.Normal == nil {
		return storage.InterruptT4Output{}, err
	}
	out := admitted.Normal
	return storage.InterruptT4Output{Headline: *out.Headline, Conclusion: *out.Conclusion, KeyPoints: append([]string(nil), (*out.KeyPoints)...), Options: append([]string(nil), (*out.Options)...), RecommendedOptionID: *out.RecommendedOptionID}, nil
}

func T4ResultFromCall(result CallResult, in T4Input) (T4CallResult, BrainSource, error) {
	if result.Status == "valid" {
		canonical, err := T4Contract(in).ValidateOutput(result.Output)
		if err != nil {
			return T4CallResult{}, BrainSource{}, err
		}
		var out T4Output
		if err := schema.Decode(canonical, &out, schema.Closed); err != nil {
			return T4CallResult{}, BrainSource{}, err
		}
		return T4CallResult{Normal: &out}, brainSource(result), nil
	}
	if result.Status != "fallback" {
		return T4CallResult{}, BrainSource{}, fmt.Errorf("brain: T4 call %s is not terminal", result.CallID)
	}
	return T4CallResult{Fallback: &in}, fallbackSource(result, "T4"), nil
}

// CallT6 adapts the unified Brain shell to the frozen Interrupt dispatch
// candidate. The storage emitter retains final severity and dispatch authority.
func (s *Shell) CallT6(ctx context.Context, in storage.InterruptT6Input) (storage.InterruptT6Output, error) {
	candidate := T6Candidate{Reason: InterruptReason(in.Reason), Severity: InterruptSeverity(in.Severity), MinModality: InterruptModality(in.MinModality), ExpiresAtMS: in.ExpiresAtMS, ChannelCandidates: append([]string(nil), in.ChannelCandidates...), DefaultChannelID: in.DefaultChannelID}
	input := T6Input{RunID: in.RunID, AttemptNo: in.AttemptNo, FrozenAtMS: in.FrozenAtMS, Candidate: candidate, Availability: T6Availability{State: "unknown", NextWindowAtMS: in.NextWindowAtMS}, Attention: T6Attention{FallbackImmediateMinSeverity: "high", Remaining: []T6Quota{{Severity: "low"}, {Severity: "normal"}, {Severity: "high"}}}}
	canonical, err := BuildT6Input(input)
	if err != nil {
		return storage.InterruptT6Output{}, err
	}
	result, err := s.Call(ctx, T6Contract(input), CallParams{Scope: storage.BrainScopeRun, SubjectKey: "run:" + in.RunID, RunID: in.RunID, AttemptNo: in.AttemptNo, Input: canonical})
	if err != nil {
		return storage.InterruptT6Output{}, err
	}
	out, _, err := T6ResultFromCall(result, input)
	if err != nil {
		return storage.InterruptT6Output{}, err
	}
	return storage.InterruptT6Output{Delivery: string(*out.Delivery), ChannelID: *out.ChannelID, SuggestedDowngrade: *out.SuggestedDowngrade}, nil
}

func T6ResultFromCall(result CallResult, in T6Input) (T6Output, BrainSource, error) {
	var out T6Output
	if result.Status == "valid" {
		canonical, err := T6Contract(in).ValidateOutput(result.Output)
		if err != nil {
			return out, BrainSource{}, err
		}
		if err := schema.Decode(canonical, &out, schema.Closed); err != nil {
			return out, BrainSource{}, err
		}
		return out, brainSource(result), nil
	}
	if result.Status != "fallback" {
		return out, BrainSource{}, fmt.Errorf("brain: T6 call %s is not terminal", result.CallID)
	}
	if err := schema.Decode(T6FallbackOutput(in), &out, schema.Closed); err != nil {
		return out, BrainSource{}, err
	}
	return out, fallbackSource(result, "T6"), nil
}

// T7CallResult makes the fallback no-draft outcome explicit.
type T7CallResult struct {
	Proposal *T7Output
	NoDraft  bool
}

// PersistT7ProposalDraft validates a terminal T7 result and persists only its
// inert, pending-human-approval draft. Fallbacks deliberately produce no row.
func PersistT7ProposalDraft(ctx context.Context, db *storage.DB, result CallResult, aggregateKey string, evidenceIDs []string, createdAtMS int64) (storage.ProposalDraft, BrainSource, error) {
	converted, source, err := T7ResultFromCall(result, aggregateKey, evidenceIDs)
	if err != nil {
		return storage.ProposalDraft{}, BrainSource{}, err
	}
	if converted.NoDraft {
		return storage.ProposalDraft{}, source, nil
	}
	out := converted.Proposal
	draft, err := db.SaveProposalDraft(ctx, storage.SaveProposalDraftCmd{
		LogicalCallID: result.CallID, PromptVersion: result.PromptVersion, OutputSchemaVersion: result.OutputSchemaVersion,
		AggregateKey: aggregateKey, ProposalKind: string(*out.ProposalKind), TargetScope: string(*out.TargetScope),
		Title: *out.Title, Body: *out.Body, EvidenceEntryIDs: *out.EvidenceEntryIDs, CreatedAtMS: createdAtMS,
	})
	if err != nil {
		return storage.ProposalDraft{}, BrainSource{}, err
	}
	return draft, source, nil
}

func T7ResultFromCall(result CallResult, aggregateKey string, evidenceIDs []string) (T7CallResult, BrainSource, error) {
	return t7ResultFromCall(result, T7Contract(aggregateKey, "", nil, evidenceIDs))
}

func t7ResultFromCall(result CallResult, contract TouchpointContract) (T7CallResult, BrainSource, error) {
	if result.Status == "valid" {
		var out T7Output
		if err := schema.Decode(result.Output, &out, schema.Closed); err != nil {
			return T7CallResult{}, BrainSource{}, err
		}
		if _, err := contract.ValidateOutput(result.Output); err != nil {
			return T7CallResult{}, BrainSource{}, err
		}
		return T7CallResult{Proposal: &out}, brainSource(result), nil
	}
	if result.Status != "fallback" {
		return T7CallResult{}, BrainSource{}, fmt.Errorf("brain: T7 call %s is not terminal", result.CallID)
	}
	return T7CallResult{NoDraft: true}, fallbackSource(result, "T7"), nil
}

func brainSource(r CallResult) BrainSource {
	return BrainSource{Kind: "brain", LogicalCallID: r.CallID, PromptVersion: r.PromptVersion, OutputSchemaVersion: r.OutputSchemaVersion}
}
func fallbackSource(r CallResult, touchpoint string) BrainSource {
	return BrainSource{Kind: "fallback", LogicalCallID: r.CallID, Version: touchpoint + "/fallback/v1", Reason: fallbackReason(r.FallbackReason)}
}
