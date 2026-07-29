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

type MergeChangePayload struct {
	ProjectID        string `json:"project_id"`
	RunID            string `json:"run_id"`
	ChangeID         string `json:"change_id"`
	GateEvaluationID string `json:"gate_evaluation_id"`
	ExpectedHeadSHA  string `json:"expected_head_sha"`
	Method           string `json:"method"`
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

// EnqueueMergeChange persists the only authority to merge: a ready/merge Gate
// verdict frozen for this Change head. ForgeWorker rechecks the same head and
// passes it as Forge's expected-head CAS value.
func EnqueueMergeChange(ctx context.Context, db *storage.DB, in Input, verdict Verdict, gateEvaluationID, method string, nowMS int64) error {
	if db == nil || in.Identity.RunID == "" || in.Identity.ProjectID == "" || in.Identity.ChangeID == "" || gateEvaluationID == "" || !validSHA(in.Change.HeadSHA) || verdict.Kind != "ready" || verdict.Code != "merge" || verdict.ExpectedHeadSHA != in.Change.HeadSHA || method == "" {
		return errors.New("gate: invalid merge authorization")
	}
	payload, err := json.Marshal(MergeChangePayload{ProjectID: in.Identity.ProjectID, RunID: in.Identity.RunID, ChangeID: in.Identity.ChangeID, GateEvaluationID: gateEvaluationID, ExpectedHeadSHA: in.Change.HeadSHA, Method: method})
	if err != nil {
		return err
	}
	_, err = db.EnqueueOperation(ctx, storage.Operation{Key: storage.MergeChangeOperationKey(in.Identity.RunID, in.Change.HeadSHA), Kind: storage.OperationMergeChange, Payload: payload, RunID: in.Identity.RunID}, nowMS)
	return err
}
