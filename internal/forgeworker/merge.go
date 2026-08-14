package forgeworker

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/xsift/sift/internal/forge"
	"github.com/xsift/sift/internal/storage"
)

type mergeChangePayload struct {
	ProjectID        string `json:"project_id"`
	RunID            string `json:"run_id"`
	ChangeID         string `json:"change_id"`
	GateEvaluationID string `json:"gate_evaluation_id"`
	ExpectedHeadSHA  string `json:"expected_head_sha"`
	Method           string `json:"method"`
}

// MergeWorker is the sole consumer of Gate-authorized merge_change operations.
// It re-observes the Change before calling Forge, so an operation authorized for
// head A becomes stale rather than merging a later head B.
type MergeWorker struct {
	DB                  *storage.DB
	Client              forge.Client
	Now                 func() time.Time
	Lease               time.Duration
	WorkerID, ProjectID string
}

func (w *MergeWorker) RunOnce(ctx context.Context) error {
	now := time.UnixMilli(1)
	if w.Now != nil && !w.Now().IsZero() {
		now = w.Now()
	}
	c, err := w.DB.ClaimOutboxOperationKindProject(ctx, w.WorkerID, storage.OperationMergeChange, w.ProjectID, now.UnixMilli(), w.Lease.Milliseconds())
	if err != nil || c == nil {
		return err
	}
	var p mergeChangePayload
	if err := json.Unmarshal(c.Payload, &p); err != nil || p.ProjectID == "" || p.RunID == "" || p.ChangeID == "" || p.GateEvaluationID == "" || p.ExpectedHeadSHA == "" || p.Method == "" {
		if err == nil {
			err = errors.New("invalid merge_change payload")
		}
		return w.finish(ctx, *c, storage.OperationFailed, storage.ErrorContract, err.Error(), nil, now)
	}
	kind, host, key, err := w.DB.ProjectForgeRef(ctx, p.ProjectID)
	if err != nil {
		return w.finish(ctx, *c, storage.OperationFailed, storage.ErrorContract, err.Error(), nil, now)
	}
	ref := forge.ProjectRef{Kind: forge.Kind(kind), Host: host, ProjectKey: key}
	ctx = forge.WithChargeKey(ctx, "forge-call:"+c.AttemptID)
	current, err := w.Client.GetChange(ctx, ref, p.ChangeID)
	if err != nil {
		return w.classified(ctx, *c, err, now)
	}
	if current.State == forge.ChangeMerged && current.HeadSHA == p.ExpectedHeadSHA && current.MergeSHA != "" {
		return w.finish(ctx, *c, storage.OperationSucceeded, "", "", mergeEvidence(current), now)
	}
	if current.State != forge.ChangeOpen || current.HeadSHA != p.ExpectedHeadSHA {
		return w.finish(ctx, *c, storage.OperationStale, "", "head changed or change is no longer open", mergeEvidence(current), now)
	}
	if _, err := w.Client.MergeChange(ctx, ref, p.ChangeID, p.ExpectedHeadSHA, p.Method); err != nil {
		var classified *forge.ClassifiedError
		if errors.As(err, &classified) && errors.Is(err, forge.ErrSemanticConflict) {
			return w.finish(ctx, *c, storage.OperationStale, "", classified.Summary, nil, now)
		}
		return w.classified(ctx, *c, err, now)
	}
	// A merge response is not terminal evidence: persist only a fresh,
	// authoritative observation of the expected Gate head.
	terminal, err := w.Client.GetChange(ctx, ref, p.ChangeID)
	if err != nil {
		return w.classified(ctx, *c, err, now)
	}
	if terminal.State != forge.ChangeMerged || terminal.HeadSHA != p.ExpectedHeadSHA || terminal.MergeSHA == "" {
		return w.finish(ctx, *c, storage.OperationConflict, storage.ErrorSemanticConflict, "merge terminal state does not match Gate authorization", mergeEvidence(terminal), now)
	}
	return w.finish(ctx, *c, storage.OperationSucceeded, "", "", mergeEvidence(terminal), now)
}

func (w *MergeWorker) finish(ctx context.Context, c storage.ClaimedOperation, state storage.OperationState, class storage.ErrorClass, summary string, evidence json.RawMessage, now time.Time) error {
	return w.DB.CompleteOutboxAttempt(ctx, c, storage.CompleteOutcome{State: state, ErrorClass: class, ErrorSummary: summary, Evidence: evidence, NowMS: now.UnixMilli(), Backoff: storage.BackoffPolicy{InitialDelayMS: 1000, MaxDelayMS: 60000, Multiplier: 2}})
}

func (w *MergeWorker) classified(ctx context.Context, c storage.ClaimedOperation, err error, now time.Time) error {
	var ce *forge.ClassifiedError
	if errors.As(err, &ce) && errors.Is(err, forge.ErrSemanticConflict) {
		return w.finish(ctx, c, storage.OperationConflict, storage.ErrorSemanticConflict, ce.Summary, nil, now)
	}
	if errors.As(err, &ce) && errors.Is(err, forge.ErrAuthOrCapability) {
		return w.finish(ctx, c, storage.OperationFailed, storage.ErrorAuthCapability, ce.Summary, nil, now)
	}
	return w.finish(ctx, c, storage.OperationRetryable, storage.ErrorTransient, err.Error(), nil, now)
}

func mergeEvidence(c forge.Change) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"change_id": c.ID, "head_sha": c.HeadSHA, "merge_sha": c.MergeSHA, "state": string(c.State)})
	return b
}
