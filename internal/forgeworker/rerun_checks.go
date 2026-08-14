package forgeworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xsift/sift/internal/forge"
	"github.com/xsift/sift/internal/schema"
	"github.com/xsift/sift/internal/storage"
)

type RerunCheckClient interface {
	RerunCheck(context.Context, forge.ProjectRef, string, string) error
}

type rerunChecksPayloadV1 struct {
	schema.ClosedType  `json:"-"`
	RunID              *string `json:"run_id" sift:"required"`
	ChangeID           *string `json:"change_id" sift:"required"`
	HeadSHA            *string `json:"head_sha" sift:"required"`
	CheckRunID         *string `json:"check_run_id" sift:"required"`
	RetryNo            *int    `json:"retry_no" sift:"required,min=1"`
	TriageSourceDigest *string `json:"triage_source_digest" sift:"required"`
	CreatedFromEventID *string `json:"created_from_event_id" sift:"required"`
}

// RerunChecksWorker owns the at-most-once rerun_checks side effect. It commits
// storage's request-start boundary before entering the Forge adapter; after
// that boundary every uncertain result is terminal conflict, never retry.
type RerunChecksWorker struct {
	DB       *storage.DB
	Clients  map[string]RerunCheckClient // forge_kind|host|project_key
	Now      func() time.Time
	Lease    time.Duration
	WorkerID string
	Mark     func(context.Context, storage.ClaimedOperation, int64) error
	Complete func(context.Context, storage.ClaimedOperation, storage.CompleteOutcome) error
}

func (w *RerunChecksWorker) RunOnce(ctx context.Context) error {
	now := time.UnixMilli(1)
	if w.Now != nil {
		now = w.Now()
	}
	claim, err := w.DB.ClaimOutboxOperationKind(ctx, w.WorkerID, storage.OperationRerunChecks, now.UnixMilli(), w.Lease.Milliseconds())
	if err != nil || claim == nil {
		return err
	}
	var payload rerunChecksPayloadV1
	if err := schema.Decode(claim.Payload, &payload, schema.Closed); err != nil || !validRerunChecksPayload(payload, claim) {
		return w.finish(ctx, *claim, storage.CompleteOutcome{State: storage.OperationFailed, ErrorClass: storage.ErrorContract, ErrorSummary: "invalid rerun_checks payload", NowMS: now.UnixMilli()})
	}
	route, err := w.DB.ResolveRerunChecksRouting(ctx, *payload.RunID, *payload.ChangeID)
	if err != nil {
		return w.finish(ctx, *claim, storage.CompleteOutcome{State: storage.OperationFailed, ErrorClass: storage.ErrorContract, ErrorSummary: "rerun_checks routing not found", NowMS: now.UnixMilli()})
	}
	client := w.Clients[route.ForgeKind+"|"+route.ForgeHost+"|"+route.ForgeProjectKey]
	if client == nil {
		return w.finish(ctx, *claim, storage.CompleteOutcome{State: storage.OperationFailed, ErrorClass: storage.ErrorAuthCapability, ErrorSummary: "no rerun_checks client for project forge", NowMS: now.UnixMilli()})
	}
	mark := w.Mark
	if mark == nil {
		mark = w.DB.MarkOutboxAttemptRequestStarted
	}
	if err := mark(ctx, *claim, now.UnixMilli()); err != nil {
		return err
	}
	ctx = forge.WithChargeKey(ctx, "rerun-checks:"+claim.AttemptID)
	ref := forge.ProjectRef{Kind: forge.Kind(route.ForgeKind), Host: route.ForgeHost, ProjectKey: route.ForgeProjectKey}
	if err := client.RerunCheck(ctx, ref, *payload.CheckRunID, *payload.HeadSHA); err != nil {
		return w.finish(ctx, *claim, rerunConflictOutcome(err, now.UnixMilli()))
	}
	evidence, _ := storage.CanonicalJSON(map[string]any{"check_run_id": *payload.CheckRunID, "expected_head_sha": *payload.HeadSHA, "retry_no": *payload.RetryNo})
	return w.finish(ctx, *claim, storage.CompleteOutcome{State: storage.OperationSucceeded, Evidence: json.RawMessage(evidence), NowMS: now.UnixMilli()})
}

func validRerunChecksPayload(p rerunChecksPayloadV1, claim *storage.ClaimedOperation) bool {
	if p.RunID == nil || p.ChangeID == nil || p.HeadSHA == nil || p.CheckRunID == nil || p.RetryNo == nil || p.TriageSourceDigest == nil || p.CreatedFromEventID == nil {
		return false
	}
	if *p.RunID == "" || *p.ChangeID == "" || *p.HeadSHA == "" || *p.CheckRunID == "" || *p.RetryNo < 1 || !isLowerHex(*p.TriageSourceDigest, 64) || !strings.HasPrefix(*p.CreatedFromEventID, "event:") {
		return false
	}
	return claim.RunID == *p.RunID && claim.Key == storage.RerunChecksOperationKey(*p.RunID, *p.HeadSHA, *p.CheckRunID, *p.RetryNo)
}

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func rerunConflictOutcome(err error, nowMS int64) storage.CompleteOutcome {
	o := storage.CompleteOutcome{State: storage.OperationConflict, ErrorClass: storage.ErrorSemanticConflict, ErrorSummary: "rerun_checks result ambiguous after request start", NowMS: nowMS}
	var classified *forge.ClassifiedError
	if errors.As(err, &classified) {
		o.ErrorSummary = classified.Summary
		switch {
		case errors.Is(err, forge.ErrAuthOrCapability):
			o.ErrorClass = storage.ErrorAuthCapability
		case errors.Is(err, forge.ErrContractViolation):
			o.ErrorClass = storage.ErrorContract
		case errors.Is(err, forge.ErrRateLimited):
			o.ErrorClass = storage.ErrorRateLimited
		}
	} else if err != nil {
		o.ErrorSummary = err.Error()
	}
	return o
}

func (w *RerunChecksWorker) finish(ctx context.Context, claim storage.ClaimedOperation, outcome storage.CompleteOutcome) error {
	if w.Complete != nil {
		return w.Complete(ctx, claim, outcome)
	}
	if w.DB == nil {
		return fmt.Errorf("rerun_checks: database is required")
	}
	return w.DB.CompleteOutboxAttempt(ctx, claim, outcome)
}
