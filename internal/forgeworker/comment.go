package forgeworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/miaoxiaoyong/sift/internal/forge"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// commentPayload mirrors the snake_case keys PersistIntakeDecision writes into
// the forge_comment operation payload. The json tags are load-bearing: Go's
// case-insensitive match does not bridge "forge_kind" to "ForgeKind".
type commentPayload struct {
	ForgeKind       string `json:"forge_kind"`
	ForgeHost       string `json:"forge_host"`
	ForgeProjectKey string `json:"forge_project_key"`
	TargetKind      string `json:"target_kind"`
	TargetID        string `json:"target_id"`
	Purpose         string `json:"purpose"`
	Markdown        string `json:"markdown"`
	IntakeID        string `json:"intake_id"`
	Generation      int    `json:"generation"`
}

// CommentWorker executes forge_comment operations. The marker lookup happens
// before every send, so a remote success followed by a local crash converges
// without a second comment.
type CommentWorker struct {
	DB        *storage.DB
	Client    forge.Client
	Now       func() time.Time
	Lease     time.Duration
	WorkerID  string
	ProjectID string
	Complete  func(context.Context, storage.ClaimedOperation, storage.CompleteOutcome) error
}

func (w *CommentWorker) RunOnce(ctx context.Context) error {
	now := time.Time{}
	if w.Now != nil {
		now = w.Now()
	}
	if now.IsZero() {
		now = time.UnixMilli(1)
	}
	c, err := w.DB.ClaimOutboxOperationKindProject(ctx, w.WorkerID, storage.OperationForgeComment, w.ProjectID, now.UnixMilli(), w.Lease.Milliseconds())
	if err != nil || c == nil {
		return err
	}
	var p commentPayload
	if err = json.Unmarshal(c.Payload, &p); err != nil {
		return w.finish(ctx, *c, storage.CompleteOutcome{State: storage.OperationFailed, ErrorClass: storage.ErrorContract, ErrorSummary: err.Error(), NowMS: now.UnixMilli()})
	}
	if p.ForgeKind != "github" && p.ForgeKind != "gitlab" {
		return w.finish(ctx, *c, storage.CompleteOutcome{State: storage.OperationFailed, ErrorClass: storage.ErrorContract, ErrorSummary: "invalid forge kind", NowMS: now.UnixMilli()})
	}
	ref := forge.ProjectRef{Kind: forge.Kind(p.ForgeKind), Host: p.ForgeHost, ProjectKey: p.ForgeProjectKey}
	target := forge.TargetRef{Kind: forge.TargetKind(p.TargetKind), ID: p.TargetID}
	// An outbox attempt is the replay identity for all Forge calls made here.
	ctx = forge.WithChargeKey(ctx, "forge-call:"+c.AttemptID)
	digest := stringDigest(c.Payload)
	marker := forge.OperationMarker(c.Key, digest)
	var comments []forge.Comment
	var next forge.Cursor
	if target.Kind == forge.TargetChange {
		comments, next, err = w.Client.ListChangeComments(ctx, ref, p.TargetID, "")
	} else {
		comments, next, err = w.Client.ListIssueComments(ctx, ref, p.TargetID, "")
	}
	_ = next
	if err == nil {
		for _, comment := range comments {
			if forge.FindOperationMarker(comment.Body, c.Key, digest) {
				return w.finish(ctx, *c, storage.CompleteOutcome{State: storage.OperationSucceeded, Evidence: json.RawMessage(fmt.Sprintf(`{"comment_id":%q}`, comment.ID)), NowMS: now.UnixMilli()})
			}
		}
	}
	if err != nil {
		return w.classified(ctx, *c, err, now)
	}
	id, err := w.Client.CommentTarget(ctx, ref, target, forge.RenderOperationBody(p.Markdown, c.Key, digest))
	if err != nil {
		return w.classified(ctx, *c, err, now)
	}
	// The marker is the authoritative evidence, not the returned id. Persisting
	// the returned id is best-effort evidence attached to the operation.
	evidence := json.RawMessage(fmt.Sprintf(`{"comment_id":%q,"marker":%q}`, id, marker))
	return w.finish(ctx, *c, storage.CompleteOutcome{State: storage.OperationSucceeded, Evidence: evidence, NowMS: now.UnixMilli()})
}
func stringDigest(b []byte) string { return forge.PayloadDigest(b) }
func (w *CommentWorker) finish(ctx context.Context, c storage.ClaimedOperation, o storage.CompleteOutcome) error {
	if w.Complete != nil {
		return w.Complete(ctx, c, o)
	}
	return w.DB.CompleteOutboxAttempt(ctx, c, o)
}
func (w *CommentWorker) classified(ctx context.Context, c storage.ClaimedOperation, err error, now time.Time) error {
	var ce *forge.ClassifiedError
	if errors.As(err, &ce) && errors.Is(err, forge.ErrAuthOrCapability) {
		_ = w.DB.SetProjectHealth(ctx, cProject(c), "forge_auth_or_capability", now.UnixMilli())
		return w.finish(ctx, c, storage.CompleteOutcome{State: storage.OperationFailed, ErrorClass: storage.ErrorAuthCapability, ErrorSummary: ce.Summary, NowMS: now.UnixMilli()})
	}
	return w.finish(ctx, c, storage.CompleteOutcome{State: storage.OperationRetryable, ErrorClass: storage.ErrorTransient, ErrorSummary: err.Error(), NowMS: now.UnixMilli(), Backoff: storage.BackoffPolicy{InitialDelayMS: 1000, MaxDelayMS: 60000, Multiplier: 2}})
}
func cProject(c storage.ClaimedOperation) string {
	var p struct {
		ProjectID string `json:"project_id"`
	}
	_ = json.Unmarshal(c.Payload, &p)
	return p.ProjectID
}
