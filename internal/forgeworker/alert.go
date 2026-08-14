package forgeworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/xsift/sift/internal/forge"
	"github.com/xsift/sift/internal/storage"
)

// AlertWorker delivers forge_alert operations as marked Forge comments. Alerts
// are intentionally a separate consumer: channel failure alerts must not be
// sent through the channel webhook and must never create another alert.
type AlertWorker struct {
	DB       *storage.DB
	Client   forge.Client
	Clients  map[string]forge.Client // forge_kind|host|project_key
	Now      func() time.Time
	Lease    time.Duration
	WorkerID string
	Complete func(context.Context, storage.ClaimedOperation, storage.CompleteOutcome) error
}

type alertPayload struct {
	ForgeKind       string `json:"forge_kind"`
	ForgeHost       string `json:"forge_host"`
	ForgeProjectKey string `json:"forge_project_key"`
	TargetKind      string `json:"target_kind"`
	TargetID        string `json:"target_id"`
	Purpose         string `json:"purpose"`
	Markdown        string `json:"markdown"`
	ProjectID       string `json:"project_id"`
}

func (w *AlertWorker) RunOnce(ctx context.Context) error {
	now := time.UnixMilli(1)
	if w.Now != nil {
		now = w.Now()
	}
	c, err := w.DB.ClaimOutboxOperationKindPurpose(ctx, w.WorkerID, storage.OperationForgeAlert, "channel_failure", now.UnixMilli(), w.Lease.Milliseconds())
	if err != nil || c == nil {
		return err
	}
	var p alertPayload
	if err := json.Unmarshal(c.Payload, &p); err != nil || p.Purpose == "" || p.Markdown == "" || p.ForgeKind == "" || p.ForgeHost == "" || p.ForgeProjectKey == "" || p.TargetKind == "" || p.TargetID == "" {
		return w.finish(ctx, *c, storage.CompleteOutcome{State: storage.OperationFailed, ErrorClass: storage.ErrorContract, ErrorSummary: "invalid forge alert payload", NowMS: now.UnixMilli()})
	}
	client := w.Client
	if w.Clients != nil {
		client = w.Clients[p.ForgeKind+"|"+p.ForgeHost+"|"+p.ForgeProjectKey]
	}
	if client == nil {
		return w.finish(ctx, *c, storage.CompleteOutcome{State: storage.OperationFailed, ErrorClass: storage.ErrorAuthCapability, ErrorSummary: "no forge alert client", NowMS: now.UnixMilli()})
	}
	ref := forge.ProjectRef{Kind: forge.Kind(p.ForgeKind), Host: p.ForgeHost, ProjectKey: p.ForgeProjectKey}
	target := forge.TargetRef{Kind: forge.TargetKind(p.TargetKind), ID: p.TargetID}
	if target.Kind != forge.TargetIssue && target.Kind != forge.TargetChange {
		return w.finish(ctx, *c, storage.CompleteOutcome{State: storage.OperationFailed, ErrorClass: storage.ErrorContract, ErrorSummary: "invalid forge alert target", NowMS: now.UnixMilli()})
	}
	// An outbox attempt is the replay identity for every evidence lookup and
	// comment call. Production adapters reject calls without this stable base.
	ctx = forge.WithChargeKey(ctx, "forge-call:"+c.AttemptID)
	var comments []forge.Comment
	if target.Kind == forge.TargetChange {
		comments, _, err = client.ListChangeComments(ctx, ref, target.ID, "")
	} else {
		comments, _, err = client.ListIssueComments(ctx, ref, target.ID, "")
	}
	digest := forge.PayloadDigest(c.Payload)
	if err == nil {
		for _, comment := range comments {
			if forge.FindOperationMarker(comment.Body, c.Key, digest) {
				return w.finish(ctx, *c, storage.CompleteOutcome{State: storage.OperationSucceeded, NowMS: now.UnixMilli()})
			}
		}
	}
	if err == nil {
		_, err = client.CommentTarget(ctx, ref, target, forge.RenderOperationBody(p.Markdown, c.Key, digest))
	}
	if err == nil {
		return w.finish(ctx, *c, storage.CompleteOutcome{State: storage.OperationSucceeded, NowMS: now.UnixMilli()})
	}
	var ce *forge.ClassifiedError
	o := storage.CompleteOutcome{State: storage.OperationRetryable, ErrorClass: storage.ErrorTransient, ErrorSummary: "forge alert delivery failed", NowMS: now.UnixMilli(), Backoff: storage.BackoffPolicy{InitialDelayMS: 1000, MaxDelayMS: 60000, Multiplier: 2}}
	if errors.As(err, &ce) {
		o.ErrorSummary = ce.Summary
		switch {
		case errors.Is(err, forge.ErrAuthOrCapability):
			o.State, o.ErrorClass = storage.OperationFailed, storage.ErrorAuthCapability
		case errors.Is(err, forge.ErrContractViolation):
			o.State, o.ErrorClass = storage.OperationFailed, storage.ErrorContract
		case errors.Is(err, forge.ErrSemanticConflict):
			o.State, o.ErrorClass = storage.OperationConflict, storage.ErrorSemanticConflict
		case errors.Is(err, forge.ErrRateLimited):
			o.ErrorClass = storage.ErrorRateLimited
			if !ce.RetryAt.IsZero() {
				o.RetryAfterMS = ce.RetryAt.Sub(now).Milliseconds()
				if o.RetryAfterMS < 0 {
					o.RetryAfterMS = 0
				}
			}
		}
	}
	return w.finish(ctx, *c, o)
}

func (w *AlertWorker) finish(ctx context.Context, c storage.ClaimedOperation, o storage.CompleteOutcome) error {
	if w.Complete != nil {
		return w.Complete(ctx, c, o)
	}
	if w.DB == nil {
		return fmt.Errorf("forge alert: database is required")
	}
	return w.DB.CompleteOutboxAttempt(ctx, c, o)
}
