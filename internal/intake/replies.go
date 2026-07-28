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
// ApplyIntakeReply calls. A reply is bound to the latest Sift clarification
// marker observed before it, rather than being assigned the current projection
// generation.
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
			ctxCall := forge.WithChargeKey(ctx, "reply:"+project.ID+":"+item.IssueID+":"+cursor.Cursor)
			comments, next, err := c.Forge.ListIssueComments(ctxCall, project.Ref, item.IssueID, forge.Cursor(cursor.Cursor))
			if err != nil {
				return err
			}

			// Markers belong to Sift-authored clarification comments. Walk the
			// page in forge order and retain the latest marker seen so replies
			// from different generations in one page are arbitrated correctly.
			latestGeneration := 0
			var latestMarkerAt time.Time
			for _, comment := range comments {
				isMarker := false
				for _, op := range operations {
					if !forge.FindOperationMarker(comment.Body, op.Key, forge.PayloadDigest(op.Payload)) {
						continue
					}
					var markerPayload struct {
						Generation int `json:"generation"`
					}
					if json.Unmarshal(op.Payload, &markerPayload) == nil && markerPayload.Generation >= 1 {
						latestGeneration = markerPayload.Generation
						latestMarkerAt = comment.CreatedAt
					}
					isMarker = true
					break
				}
				// Never parse Sift's clarification body as an operator reply.
				if isMarker || latestGeneration < 1 || (!comment.CreatedAt.IsZero() && comment.CreatedAt.Before(latestMarkerAt)) || !isAllowedActor(project.OperatorAllowlist, comment.Author) {
					continue
				}
				accept, ok := intakeReply(comment.Body)
				if !ok {
					continue
				}
				raw := sha256.Sum256([]byte(comment.Body))
				if err := c.DB.ApplyIntakeReply(ctx, storage.IntakeReplyCmd{
					IntakeID: item.ID, EventID: comment.ID, Actor: comment.Author,
					RawDigest: hex.EncodeToString(raw[:]), Generation: latestGeneration,
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
