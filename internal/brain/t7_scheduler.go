package brain

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/miaoxiaoyong/sift/internal/schema"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// T7Scheduler is the periodic aggregate caller. Offline replay evidence is
// immutable; the aggregate completion cursor is appended only after the
// terminal trace and optional inert draft are durable.
type T7Scheduler struct {
	DB    *storage.DB
	Shell *Shell
	Now   func() time.Time
	Limit int

	mu sync.Mutex
}

func (s *T7Scheduler) Tick(ctx context.Context) error {
	if s == nil || s.DB == nil || s.Shell == nil || s.Now == nil {
		return errors.New("brain: incomplete T7 scheduler")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.Shell.RecoverRunningT7(ctx); err != nil {
		return err
	}
	limit := s.Limit
	if limit < 1 {
		limit = 16
	}
	nowMS := s.Now().UnixMilli()
	pending, err := s.DB.PendingT7Aggregates(ctx, nowMS, limit)
	if err != nil {
		return err
	}
	for _, aggregate := range pending {
		if err := s.process(ctx, aggregate, nowMS); err != nil {
			return err
		}
	}
	return nil
}

func (s *T7Scheduler) process(ctx context.Context, aggregate storage.PendingT7Aggregate, nowMS int64) error {
	input := t7InputFromStorage(aggregate)
	canonical, err := BuildT7Input(input)
	if err != nil {
		return err
	}
	evidenceIDs := t7EvidenceIDs(input)
	kinds := t7CategoryKinds(input)
	contract := T7Contract(aggregate.AggregateKey, aggregate.ProjectID, kinds, evidenceIDs)
	result, err := s.Shell.Call(ctx, contract, CallParams{
		Scope: storage.BrainScopeAggregate, SubjectKey: aggregate.AggregateKey,
		ProjectID: aggregate.ProjectID, Input: canonical, T7AggregateOnce: true,
	})
	if err != nil {
		return err
	}
	if _, _, err := PersistT7ProposalDraftFromInput(ctx, s.DB, result, nowMS); err != nil {
		return err
	}
	return s.DB.CompleteT7Aggregate(ctx, aggregate.ReplayEvidenceID, aggregate.AggregateKey, result.CallID, result.Status, nowMS)
}

func t7InputFromStorage(a storage.PendingT7Aggregate) T7Input {
	categories := make([]T7CategoryEvidence, len(a.Categories))
	for i, c := range a.Categories {
		categories[i] = T7CategoryEvidence{
			EvidenceID: c.EvidenceID, TaskKind: TaskKind(c.TaskKind), CertificationVersion: c.CertificationVersion,
			Certified: c.Certified, EvidenceSummary: T7EvidenceSummary{
				WindowStartMS: c.WindowStartMS, WindowEndMS: c.WindowEndMS,
				CertificationRulesVersion: c.CertificationRulesVersion, EvidenceDigest: c.EvidenceDigest,
				TotalSamples: c.TotalSamples, NegativeSamples: c.NegativeSamples,
				LeakCount: c.LeakCount, FalseBlockCount: c.FalseBlockCount,
			},
		}
	}
	materials := make([]T7SemanticMaterial, len(a.SemanticMaterial))
	for i, material := range a.SemanticMaterial {
		materials[i] = T7SemanticMaterial{EntryID: material.EntryID, MaterialKind: material.MaterialKind, Text: material.Text}
	}
	input := T7Input{
		AggregateKey: a.AggregateKey, Window: T7Window{StartMS: a.WindowStartMS, EndMS: a.WindowEndMS},
		Categories: categories, ReplaySummary: T7ReplaySummary{
			EvidenceID: a.ReplaySummary.EvidenceID, DatasetVersion: a.ReplaySummary.DatasetVersion,
			GateVersion: a.ReplaySummary.GateVersion, TotalSamples: a.ReplaySummary.TotalSamples,
			NegativeSamples: a.ReplaySummary.NegativeSamples, LeakCount: a.ReplaySummary.LeakCount,
			FalseBlockCount: a.ReplaySummary.FalseBlockCount,
		},
		SemanticMaterial: materials, TraceProjectID: a.ProjectID,
	}
	if a.TaskKind == "all" {
		input.AllCategoryKinds = t7CategoryKinds(input)
	}
	return input
}

func t7CategoryKinds(input T7Input) []TaskKind {
	kinds := make([]TaskKind, len(input.Categories))
	for i, category := range input.Categories {
		kinds[i] = category.TaskKind
	}
	return kinds
}

func t7EvidenceIDs(input T7Input) []string {
	ids := make([]string, 0, len(input.Categories)+len(input.SemanticMaterial)+1)
	for _, category := range input.Categories {
		ids = append(ids, category.EvidenceID)
	}
	ids = append(ids, input.ReplaySummary.EvidenceID)
	for _, material := range input.SemanticMaterial {
		ids = append(ids, material.EntryID)
	}
	sort.Strings(ids)
	return ids
}

func t7ContractFromFrozenInput(call storage.BrainCall) (TouchpointContract, []string, error) {
	var input T7Input
	if err := schema.Decode(call.InputJSON, &input, schema.Closed); err != nil {
		return TouchpointContract{}, nil, err
	}
	input.TraceProjectID = call.ProjectID
	parts, ok := aggregateParts(call.SubjectKey)
	if !ok {
		return TouchpointContract{}, nil, errors.New("brain: invalid frozen T7 aggregate key")
	}
	var kinds []TaskKind
	if parts.kind == "all" {
		kinds = t7CategoryKinds(input)
	}
	input.AllCategoryKinds = kinds
	canonical, err := BuildT7Input(input)
	if err != nil || string(canonical) != string(call.InputJSON) {
		return TouchpointContract{}, nil, errors.New("brain: invalid frozen T7 input")
	}
	evidenceIDs := t7EvidenceIDs(input)
	return T7Contract(call.SubjectKey, call.ProjectID, kinds, evidenceIDs), evidenceIDs, nil
}

// RecoverRunningT7 converges ambiguous provider windows before they can be
// reconsidered. A persisted valid attempt is validated against the frozen
// input; every other running call becomes the recovery no-draft fallback.
func (s *Shell) RecoverRunningT7(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	running, err := s.db.RunningT7AggregateCalls(ctx)
	if err != nil {
		return 0, err
	}
	converged := 0
	for _, call := range running {
		if call.Touchpoint != "T7" || call.Scope != storage.BrainScopeAggregate {
			continue
		}
		contract, _, err := t7ContractFromFrozenInput(call)
		if err != nil {
			return converged, err
		}
		attempts, err := s.db.BrainCallAttempts(ctx, call.ID)
		if err != nil {
			return converged, err
		}
		var validAttempt *storage.BrainAttempt
		for i := range attempts {
			if attempts[i].Outcome == storage.BrainAttemptValid {
				validAttempt = &attempts[i]
			}
		}
		if validAttempt != nil && validAttempt.RawOutputText != nil {
			resultText, _, _, parseErr := ParseEnvelope([]byte(*validAttempt.RawOutputText))
			if parseErr == nil {
				if canonical, validateErr := contract.ValidateOutput(resultText); validateErr == nil {
					if err := s.db.FinalizeBrainCall(ctx, storage.FinalizeBrainCallCmd{CallID: call.ID, Status: storage.BrainCallValid, SelectedAttemptNo: &validAttempt.ProviderAttempt, ValidatedOutputJSON: canonical, FinishedAtMS: s.now().UnixMilli()}); err != nil {
						return converged, err
					}
					converged++
					continue
				}
			}
		}
		if err := s.db.FinalizeBrainCall(ctx, storage.FinalizeBrainCallCmd{CallID: call.ID, Status: storage.BrainCallFallback, FallbackReason: "recovery: converged without a persisted valid attempt", FinishedAtMS: s.now().UnixMilli()}); err != nil {
			return converged, err
		}
		converged++
	}
	return converged, nil
}

// PersistT7ProposalDraftFromInput binds output validation and citations to the
// call's frozen input. Replayed terminal calls therefore cannot pick up newer
// certification or semantic evidence.
func PersistT7ProposalDraftFromInput(ctx context.Context, db *storage.DB, result CallResult, createdAtMS int64) (storage.ProposalDraft, BrainSource, error) {
	if len(result.Input) == 0 {
		return storage.ProposalDraft{}, BrainSource{}, errors.New("brain: T7 result has no frozen input")
	}
	var input T7Input
	if err := schema.Decode(result.Input, &input, schema.Closed); err != nil {
		return storage.ProposalDraft{}, BrainSource{}, err
	}
	parts, ok := aggregateParts(input.AggregateKey)
	if !ok {
		return storage.ProposalDraft{}, BrainSource{}, errors.New("brain: invalid T7 result aggregate key")
	}
	projectID := ""
	if parts.scope == "project" {
		decoded, err := decodeAggregateProject(parts.project)
		if err != nil {
			return storage.ProposalDraft{}, BrainSource{}, err
		}
		projectID = decoded
	}
	call := storage.BrainCall{SubjectKey: input.AggregateKey, ProjectID: projectID, InputJSON: result.Input}
	contract, _, err := t7ContractFromFrozenInput(call)
	if err != nil {
		return storage.ProposalDraft{}, BrainSource{}, err
	}
	converted, source, err := t7ResultFromCall(result, contract)
	if err != nil || converted.NoDraft {
		return storage.ProposalDraft{}, source, err
	}
	out := converted.Proposal
	draft, err := db.SaveProposalDraft(ctx, storage.SaveProposalDraftCmd{
		LogicalCallID: result.CallID, PromptVersion: result.PromptVersion, OutputSchemaVersion: result.OutputSchemaVersion,
		AggregateKey: input.AggregateKey, ProposalKind: string(*out.ProposalKind), TargetScope: string(*out.TargetScope),
		Title: *out.Title, Body: *out.Body, EvidenceEntryIDs: *out.EvidenceEntryIDs, CreatedAtMS: createdAtMS,
	})
	return draft, source, err
}

func decodeAggregateProject(encoded string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("brain: decode aggregate project: %w", err)
	}
	if len(decoded) == 0 {
		return "", errors.New("brain: empty aggregate project identity")
	}
	return string(decoded), nil
}
