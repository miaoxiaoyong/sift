package forgeworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xsift/sift/internal/command"
	"github.com/xsift/sift/internal/forge"
	"github.com/xsift/sift/internal/storage"
)

// CommandAckWorker executes command_ack operations. It mirrors the comment
// worker's marker-then-send execution (outbox.md §5): every attempt lists the
// target first, so a remote success followed by a local crash converges on the
// embedded operation marker without a second post.
//
// Unlike a forge_comment, a command_ack payload is the closed CommandAckV1 and
// carries no Forge routing. The worker resolves the immutable target and the
// project's forge ref from the append-only command receipt (command.md §6.1:
// the ack target is the immutable envelope target), then selects the matching
// per-project adapter. It never reconstructs the target from the current Run,
// Change or Interrupt state.
type CommandAckWorker struct {
	DB       *storage.DB
	Clients  map[string]forge.Client // keyed forge_kind|host|project_key
	Now      func() time.Time
	Lease    time.Duration
	WorkerID string
	Complete func(context.Context, storage.ClaimedOperation, storage.CompleteOutcome) error
}

func (w *CommandAckWorker) RunOnce(ctx context.Context) error {
	now := time.UnixMilli(1)
	if w.Now != nil {
		now = w.Now()
	}
	c, err := w.DB.ClaimOutboxOperationKind(ctx, w.WorkerID, storage.OperationCommandAck, now.UnixMilli(), w.Lease.Milliseconds())
	if err != nil || c == nil {
		return err
	}
	eventKey, ok := ackEventKey(c.Key)
	if !ok {
		return w.finish(ctx, *c, "", storage.CompleteOutcome{State: storage.OperationFailed, ErrorClass: storage.ErrorContract, ErrorSummary: "invalid command_ack operation key", NowMS: now.UnixMilli()})
	}
	route, err := w.DB.ResolveCommandAckRouting(ctx, eventKey)
	if err != nil {
		return w.finish(ctx, *c, "", storage.CompleteOutcome{State: storage.OperationFailed, ErrorClass: storage.ErrorContract, ErrorSummary: "command ack routing not found", NowMS: now.UnixMilli()})
	}
	client := w.Clients[route.ForgeKind+"|"+route.ForgeHost+"|"+route.ForgeProjectKey]
	if client == nil {
		return w.finish(ctx, *c, route.ProjectID, storage.CompleteOutcome{State: storage.OperationFailed, ErrorClass: storage.ErrorAuthCapability, ErrorSummary: "no command_ack client for project forge", NowMS: now.UnixMilli()})
	}
	var ack command.CommandAckV1
	if err = json.Unmarshal(c.Payload, &ack); err != nil || ack.SchemaVersion != 1 || ack.CommandEventID == "" || ack.Disposition == "" {
		return w.finish(ctx, *c, route.ProjectID, storage.CompleteOutcome{State: storage.OperationFailed, ErrorClass: storage.ErrorContract, ErrorSummary: "invalid command_ack payload", NowMS: now.UnixMilli()})
	}
	ref := forge.ProjectRef{Kind: forge.Kind(route.ForgeKind), Host: route.ForgeHost, ProjectKey: route.ForgeProjectKey}
	target := forge.TargetRef{Kind: forge.TargetKind(route.TargetKind), ID: route.TargetID}
	if target.Kind != forge.TargetIssue && target.Kind != forge.TargetChange {
		return w.finish(ctx, *c, route.ProjectID, storage.CompleteOutcome{State: storage.OperationFailed, ErrorClass: storage.ErrorContract, ErrorSummary: "invalid command_ack target", NowMS: now.UnixMilli()})
	}
	// An outbox attempt is the replay identity for every Forge call made here,
	// including the evidence lookup. Production adapters reject calls without
	// this stable charge-key base.
	ctx = forge.WithChargeKey(ctx, "forge-call:"+c.AttemptID)
	digest := forge.PayloadDigest(c.Payload)
	body := forge.RenderOperationBody(renderAckMarkdown(ack), c.Key, digest)

	var comments []forge.Comment
	if target.Kind == forge.TargetChange {
		comments, _, err = client.ListChangeComments(ctx, ref, target.ID, "")
	} else {
		comments, _, err = client.ListIssueComments(ctx, ref, target.ID, "")
	}
	if err == nil {
		for _, comment := range comments {
			if forge.FindOperationMarker(comment.Body, c.Key, digest) {
				return w.finish(ctx, *c, route.ProjectID, storage.CompleteOutcome{State: storage.OperationSucceeded, Evidence: json.RawMessage(fmt.Sprintf(`{"comment_id":%q}`, comment.ID)), NowMS: now.UnixMilli()})
			}
		}
	}
	if err != nil {
		return w.classified(ctx, *c, route.ProjectID, err, now)
	}
	id, err := client.CommentTarget(ctx, ref, target, body)
	if err != nil {
		return w.classified(ctx, *c, route.ProjectID, err, now)
	}
	evidence := json.RawMessage(fmt.Sprintf(`{"comment_id":%q,"marker":%q}`, id, forge.OperationMarker(c.Key, digest)))
	return w.finish(ctx, *c, route.ProjectID, storage.CompleteOutcome{State: storage.OperationSucceeded, Evidence: evidence, NowMS: now.UnixMilli()})
}

// ackEventKey extracts the canonical command event key from a command_ack
// operation key of the form command:<event_key>:ack. The event key is a 64-char
// lowercase hex digest (command.md §1) and therefore contains no colons.
func ackEventKey(opKey string) (string, bool) {
	const prefix, suffix = "command:", ":ack"
	if !strings.HasPrefix(opKey, prefix) || !strings.HasSuffix(opKey, suffix) || len(opKey) <= len(prefix)+len(suffix) {
		return "", false
	}
	key := opKey[len(prefix) : len(opKey)-len(suffix)]
	if len(key) != 64 {
		return "", false
	}
	return key, true
}

// renderAckMarkdown deterministically renders a CommandAckV1 to the human-facing
// acknowledgement body (command.md §6.1): it outputs action, disposition, Run
// and Interrupt, and when a next_nonce was issued it echoes only that newly
// issued nonce. It never echoes the submitted/old nonce, actor, token, the
// original comment, reject/ask text, process identity, evidence of
// disappearance or database errors (§1.4).
func renderAckMarkdown(ack command.CommandAckV1) string {
	var b strings.Builder
	b.WriteString("Sift command acknowledged.\n\n")
	b.WriteString("Disposition: ")
	b.WriteString(string(ack.Disposition))
	b.WriteByte('\n')
	if ack.Action != nil {
		b.WriteString("Action: ")
		b.WriteString(string(*ack.Action))
		b.WriteByte('\n')
	}
	if ack.RunID != nil && *ack.RunID != "" {
		b.WriteString("Run: ")
		b.WriteString(*ack.RunID)
		b.WriteByte('\n')
	}
	if ack.InterruptID != nil && *ack.InterruptID != "" {
		b.WriteString("Interrupt: ")
		b.WriteString(*ack.InterruptID)
		b.WriteByte('\n')
	}
	// next_nonce is a public anti-replay correlator for the next round, not a
	// capability secret (command.md §6.1). It is optional and emitted as the
	// bare newly issued value rather than an action-templated command: hold/ask
	// require trailing arguments, so a templated command could be incomplete. It
	// never echoes the submitted/old nonce.
	if ack.NextNonce != nil && *ack.NextNonce != "" {
		b.WriteString("\nCurrent nonce: ")
		b.WriteString(*ack.NextNonce)
		b.WriteByte('\n')
	}
	return b.String()
}

func (w *CommandAckWorker) finish(ctx context.Context, c storage.ClaimedOperation, projectID string, o storage.CompleteOutcome) error {
	if w.Complete != nil {
		return w.Complete(ctx, c, o)
	}
	if w.DB == nil {
		return fmt.Errorf("command_ack: database is required")
	}
	return w.DB.CompleteOutboxAttempt(ctx, c, o)
}

func (w *CommandAckWorker) classified(ctx context.Context, c storage.ClaimedOperation, projectID string, err error, now time.Time) error {
	var ce *forge.ClassifiedError
	o := storage.CompleteOutcome{State: storage.OperationRetryable, ErrorClass: storage.ErrorTransient, ErrorSummary: "command_ack delivery failed", NowMS: now.UnixMilli(), Backoff: storage.BackoffPolicy{InitialDelayMS: 1000, MaxDelayMS: 60000, Multiplier: 2}}
	if errors.As(err, &ce) {
		o.ErrorSummary = ce.Summary
		switch {
		case errors.Is(err, forge.ErrAuthOrCapability):
			o.State, o.ErrorClass = storage.OperationFailed, storage.ErrorAuthCapability
			if projectID != "" && w.DB != nil {
				_ = w.DB.SetProjectHealth(ctx, projectID, "forge_auth_or_capability", now.UnixMilli())
			}
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
	return w.finish(ctx, c, projectID, o)
}
