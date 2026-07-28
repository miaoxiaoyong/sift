package intake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/miaoxiaoyong/sift/internal/forge"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// ReplyConsumer turns allowlisted, deterministic intake commands into durable
// ApplyIntakeReply calls. Generation is read from the current durable target;
// old comments remain auditable and are rejected by storage's CAS arbitration.
type ReplyConsumer struct {
	DB       *storage.DB
	Forge    forge.Client
	Projects []Project
	Now      func() time.Time
}

func (c *ReplyConsumer) RunOnce(ctx context.Context) error {
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	if now.IsZero() {
		now = time.UnixMilli(1)
	}
	for _, project := range c.Projects {
		items, err := c.DB.AwaitingIntakes(ctx, project.ID)
		if err != nil {
			return err
		}
		for _, item := range items {
			stream := "issue_comments:" + item.IssueID
			cursor, err := c.DB.IntakeCursor(ctx, project.ID, stream)
			if err != nil {
				return err
			}
			operations, err := c.DB.IntakeReplyOperations(ctx, item.ID)
			if err != nil {
				return err
			}
			if len(operations) == 0 {
				// No durable comment operation means there is no generation
				// identity to consume. Never infer one from the projection.
				continue
			}
			ctxCall := forge.WithChargeKey(ctx, "reply:"+project.ID+":"+item.IssueID+":"+cursor.Cursor)
			comments, next, err := c.Forge.ListIssueComments(ctxCall, project.Ref, item.IssueID, forge.Cursor(cursor.Cursor))
			if err != nil {
				return err
			}
			for _, comment := range comments {
				var matched storage.IntakeReplyOperation
				for _, op := range operations {
					if forge.FindOperationMarker(comment.Body, op.Key, forge.PayloadDigest(op.Payload)) {
						matched = op
						break
					}
				}
				if matched.Key == "" || !isAllowedActor(project.OperatorAllowlist, comment.Author) {
					continue
				}
				var markerPayload struct {
					Generation int `json:"generation"`
				}
				if json.Unmarshal(matched.Payload, &markerPayload) != nil || markerPayload.Generation < 1 {
					continue
				}
				accept, ok := intakeReply(comment.Body)
				if !ok {
					continue
				}
				raw := sha256.Sum256([]byte(comment.Body))
				if err := c.DB.ApplyIntakeReply(ctx, storage.IntakeReplyCmd{
					IntakeID: item.ID, EventID: comment.ID, Actor: comment.Author,
					RawDigest: hex.EncodeToString(raw[:]), Generation: markerPayload.Generation,
					Accept: accept, ObservedAtMS: comment.CreatedAt.UnixMilli(), NowMS: now.UnixMilli(),
				}); err != nil {
					return err
				}
			}
			if next != "" {
				if err := c.DB.SaveForgeCursor(ctx, project.ID, stream, string(next), now.UnixMilli()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func intakeReply(body string) (bool, bool) {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(body)))
	if len(fields) == 0 {
		return false, false
	}
	if len(fields) >= 2 && fields[0] == "/sift" {
		switch fields[1] {
		case "approve", "accept", "confirm", "yes":
			return true, true
		case "reject", "deny", "no":
			return false, true
		}
	}
	return false, false
}
