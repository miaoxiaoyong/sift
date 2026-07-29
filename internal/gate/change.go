package gate

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/miaoxiaoyong/sift/internal/storage"
	"github.com/miaoxiaoyong/sift/internal/worktree"
)

// CreateChangePayload is frozen from EvaluateSuccess. Workers must use this
// payload (and its operation marker), rather than reconstructing a Change from
// an Agent report or a mutable worktree.
type CreateChangePayload struct {
	ProjectID string `json:"project_id"`
	RunID     string `json:"run_id"`
	BaseRef   string `json:"base_ref"`
	HeadRef   string `json:"head_ref"`
	HeadSHA   string `json:"head_sha"`
	Title     string `json:"title"`
	Body      string `json:"body"`
}

// EnqueueCreateChange is the only Gate-side bridge from M3's verified success
// fact to a create_change operation. The stable key makes the fact idempotent.
func EnqueueCreateChange(ctx context.Context, db *storage.DB, ready worktree.ReadyChange, projectID, title, body string, nowMS int64) error {
	if db == nil || projectID == "" || ready.RunID == "" || ready.Base == "" || ready.Branch == "" || !validSHA(ready.FinalHeadSHA) || title == "" {
		return errors.New("gate: invalid verified change fact")
	}
	payload, err := json.Marshal(CreateChangePayload{ProjectID: projectID, RunID: ready.RunID, BaseRef: ready.Base, HeadRef: ready.Branch, HeadSHA: ready.FinalHeadSHA, Title: title, Body: body})
	if err != nil {
		return err
	}
	_, err = db.EnqueueOperation(ctx, storage.Operation{Key: storage.CreateChangeOperationKey(ready.RunID, ready.FinalHeadSHA), Kind: storage.OperationCreateChange, Payload: payload, RunID: ready.RunID}, nowMS)
	return err
}
