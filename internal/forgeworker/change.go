package forgeworker

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/miaoxiaoyong/sift/internal/forge"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

type createChangePayload struct {
	ProjectID string `json:"project_id"`
	RunID     string `json:"run_id"`
	BaseRef   string `json:"base_ref"`
	HeadRef   string `json:"head_ref"`
	HeadSHA   string `json:"head_sha"`
	Title     string `json:"title"`
	Body      string `json:"body"`
}

// ChangeWorker converges create_change operations by searching the operation
// marker across every remote state before it creates anything.
type ChangeWorker struct {
	DB                  *storage.DB
	Client              forge.Client
	Now                 func() time.Time
	Lease               time.Duration
	WorkerID, ProjectID string
}

func (w *ChangeWorker) RunOnce(ctx context.Context) error {
	now := time.UnixMilli(1)
	if w.Now != nil && !w.Now().IsZero() {
		now = w.Now()
	}
	c, err := w.DB.ClaimOutboxOperationKindProject(ctx, w.WorkerID, storage.OperationCreateChange, w.ProjectID, now.UnixMilli(), w.Lease.Milliseconds())
	if err != nil || c == nil {
		return err
	}
	var p createChangePayload
	if err := json.Unmarshal(c.Payload, &p); err != nil || p.ProjectID == "" || p.RunID == "" || p.BaseRef == "" || p.HeadRef == "" || p.HeadSHA == "" || p.Title == "" {
		if err == nil {
			err = errors.New("invalid create_change payload")
		}
		return w.finish(ctx, *c, storage.OperationFailed, storage.ErrorContract, err.Error(), nil, now)
	}
	// The durable payload is project-scoped but carries no mutable Forge facts;
	// resolve its configured ref from the run/project projection.
	kind, host, key, err := w.DB.ProjectForgeRef(ctx, p.ProjectID)
	if err != nil {
		return w.finish(ctx, *c, storage.OperationFailed, storage.ErrorContract, err.Error(), nil, now)
	}
	ref := forge.ProjectRef{Kind: forge.Kind(kind), Host: host, ProjectKey: key}
	ctx = forge.WithChargeKey(ctx, "forge-call:"+c.AttemptID)
	change, result, err := w.Client.FindChangeForCreateOperation(ctx, ref, c.Key, p.HeadRef, p.BaseRef)
	if err != nil {
		return w.classified(ctx, *c, err, now)
	}
	if result == forge.SemanticConflict {
		return w.finish(ctx, *c, storage.OperationConflict, storage.ErrorSemanticConflict, "change marker conflict", nil, now)
	}
	if result == forge.NoMatch {
		body := forge.RenderOperationBody(p.Body, c.Key, forge.PayloadDigest(c.Payload))
		created, createErr := w.Client.CreateChange(ctx, ref, p.HeadRef, p.BaseRef, p.Title, body)
		if createErr != nil {
			return w.classified(ctx, *c, createErr, now)
		}
		change = &created
	}
	if change == nil {
		return w.finish(ctx, *c, storage.OperationFailed, storage.ErrorContract, "marker lookup returned no change", nil, now)
	}
	if err := w.DB.RecordCreatedChange(ctx, p.RunID, change.ID, now.UnixMilli()); err != nil {
		return err
	}
	evidence, _ := json.Marshal(map[string]string{"change_id": change.ID, "head_sha": change.HeadSHA, "state": string(change.State)})
	return w.finish(ctx, *c, storage.OperationSucceeded, "", "", evidence, now)
}
func (w *ChangeWorker) finish(ctx context.Context, c storage.ClaimedOperation, state storage.OperationState, class storage.ErrorClass, summary string, evidence json.RawMessage, now time.Time) error {
	return w.DB.CompleteOutboxAttempt(ctx, c, storage.CompleteOutcome{State: state, ErrorClass: class, ErrorSummary: summary, Evidence: evidence, NowMS: now.UnixMilli(), Backoff: storage.BackoffPolicy{InitialDelayMS: 1000, MaxDelayMS: 60000, Multiplier: 2}})
}
func (w *ChangeWorker) classified(ctx context.Context, c storage.ClaimedOperation, err error, now time.Time) error {
	var ce *forge.ClassifiedError
	if errors.As(err, &ce) && errors.Is(err, forge.ErrSemanticConflict) {
		return w.finish(ctx, c, storage.OperationConflict, storage.ErrorSemanticConflict, ce.Summary, nil, now)
	}
	if errors.As(err, &ce) && errors.Is(err, forge.ErrAuthOrCapability) {
		return w.finish(ctx, c, storage.OperationFailed, storage.ErrorAuthCapability, ce.Summary, nil, now)
	}
	return w.finish(ctx, c, storage.OperationRetryable, storage.ErrorTransient, err.Error(), nil, now)
}
