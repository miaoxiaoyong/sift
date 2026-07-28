package intake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
			ctxCall := forge.WithChargeKey(ctx, "reply:tick:"+now.Format(time.RFC3339Nano)+":"+project.ID+":"+item.IssueID)
			comments, _, err := c.Forge.ListIssueComments(ctxCall, project.Ref, item.IssueID, "")
			if err != nil {
				return err
			}
			for _, comment := range comments {
				if !isAllowedActor(project.OperatorAllowlist, comment.Author) {
					continue
				}
				accept, ok := intakeReply(comment.Body)
				if !ok {
					continue
				}
				raw := sha256.Sum256([]byte(comment.Body))
				if err := c.DB.ApplyIntakeReply(ctx, storage.IntakeReplyCmd{
					IntakeID: item.ID, EventID: comment.ID, Actor: comment.Author,
					RawDigest: hex.EncodeToString(raw[:]), Generation: item.Generation,
					Accept: accept, ObservedAtMS: comment.CreatedAt.UnixMilli(), NowMS: now.UnixMilli(),
				}); err != nil {
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
