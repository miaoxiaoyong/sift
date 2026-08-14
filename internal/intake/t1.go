package intake

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/xsift/sift/internal/brain"
	"github.com/xsift/sift/internal/forge"
	"github.com/xsift/sift/internal/schema"
	"github.com/xsift/sift/internal/storage"
	"github.com/xsift/sift/internal/worktree"
)

type T1Evaluator struct {
	DB    *storage.DB
	Brain *brain.Shell
	Now   func() time.Time
}

// EvaluateIssue wires a normalized Forge Issue into T1. Provider disabled or
// unavailable is intentionally not a drop: the shell's deterministic fallback
// is persisted as ready and the Issue is enqueued through PersistIntakeDecision.
func (e *T1Evaluator) EvaluateIssue(ctx context.Context, project Project, issue forge.Issue) error {
	item, err := e.DB.FindPendingIntake(ctx, project.ID, issue.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // already consumed or not yet intake'd
		}
		return err
	}
	input, err := brain.BuildT1Input(brain.T1Input{Forge: brain.T1Forge{Kind: string(project.Ref.Kind), Host: project.Ref.Host, ProjectKey: project.Ref.ProjectKey}, Issue: brain.T1Issue{ID: issue.ID, Title: issue.Title, Body: issue.Body, Author: issue.Author, URL: issue.URL, Labels: issue.Labels}, KnownCandidates: []brain.T1Candidate{}})
	if err != nil {
		return err
	}
	now := time.Time{}
	if e.Now != nil {
		now = e.Now()
	}
	if now.IsZero() {
		now = time.UnixMilli(1)
	}
	result, err := e.Brain.Call(ctx, brain.T1Contract(nil), brain.CallParams{Scope: "intake", SubjectKey: fmt.Sprintf("forge:%s:%s:%s:issue:%s", project.Ref.Kind, project.Ref.Host, project.Ref.ProjectKey, issue.ID), ProjectID: project.ID, Input: input})
	if err != nil {
		return err
	}
	var out struct {
		Disposition string   `json:"disposition"`
		Questions   []string `json:"questions"`
		Possible    *string  `json:"possible_duplicate_run_id"`
		Rationale   string   `json:"rationale"`
	}
	if err = json.Unmarshal(result.Output, &out); err != nil {
		return err
	}
	q, _ := json.Marshal(out.Questions)
	runID := storage.NewID()
	if err := e.DB.PersistIntakeDecision(ctx, storage.IntakeDecisionCmd{IntakeID: item.ID, AssessmentID: storage.NewID(), LogicalCallID: result.CallID, ExpectedVersion: item.Version, Disposition: out.Disposition, QuestionsJSON: string(q), PossibleDuplicateRunID: out.Possible, Rationale: out.Rationale, NowMS: now.UnixMilli(), RunID: runID}); err != nil {
		return err
	}
	if out.Disposition != string(brain.T1Ready) {
		return nil
	}

	candidateIDs := make([]string, 0, len(project.T2Agents))
	for _, candidate := range project.T2Agents {
		candidateIDs = append(candidateIDs, candidate.ID)
	}
	t2Input, err := brain.BuildT2Input(brain.T2Input{
		RunID:           runID,
		Issue:           brain.T2Issue{Title: issue.Title, Body: issue.Body, URL: issue.URL},
		CandidateAgents: project.T2Agents,
		BaseContext:     brain.T2BaseContext{},
	})
	if err != nil {
		// An unavailable/invalid T2 input is the human-assignment fallback. T1
		// has already consumed the intake item and the Run remains queued with
		// no assignment, so a later operator can resume it safely.
		return nil
	}
	t2result, err := e.Brain.Call(ctx, brain.T2Contract(candidateIDs), brain.CallParams{
		Scope: "run", SubjectKey: "run:" + runID, ProjectID: project.ID, RunID: runID, Input: t2Input,
	})
	if err != nil {
		return err
	}
	if t2result.Status != storage.BrainCallValid || len(t2result.Output) == 0 {
		return nil
	}
	var t2out brain.T2Output
	if err := schema.Decode(t2result.Output, &t2out, schema.Closed); err != nil {
		return nil
	}
	if t2out.Kind == nil || t2out.Agent == nil || t2out.HITLBeforeStart == nil || t2out.Goals == nil || t2out.Rationale == nil {
		return nil
	}
	backend := project.AgentBackends[*t2out.Agent]
	if backend == "" {
		backend = "process"
	}
	assignment := storage.CommitT2AssignmentCmd{
		RunID: runID, ExpectedVersion: 1, Kind: string(*t2out.Kind), AgentID: *t2out.Agent,
		HITLBeforeStart: *t2out.HITLBeforeStart, Backend: backend, NowMS: now.UnixMilli(),
	}
	var worktrees *worktree.Manager
	var created worktree.Worktree
	if project.Repo != "" {
		worktrees, err = worktree.NewManager(project.Repo, filepath.Join(project.Repo, ".sift-worktrees"))
		if err != nil {
			return err
		}
		created, err = worktrees.Create(ctx, runID, 1, "HEAD", "sift/"+runID)
		if err != nil {
			return err
		}
		canonical, digest, assembleErr := brain.AssembleTaskSpec(brain.TaskSpecParams{
			Title: issue.Title, Body: issue.Body, SourceURL: issue.URL, Goals: *t2out.Goals,
			PolicyHash: project.ID, Kind: *t2out.Kind, Agent: *t2out.Agent,
			HITLBeforeStart: *t2out.HITLBeforeStart, LogicalCallID: t2result.CallID,
			PromptVersion: brain.T2Asset().PromptVersion,
		})
		if assembleErr != nil {
			_ = worktrees.Remove(ctx, created)
			return assembleErr
		}
		assignment.TaskSpecID = storage.NewID()
		assignment.TaskSpecJSON = canonical
		assignment.TaskSpecDigest = digest
		assignment.InitialAttempt = &storage.InitialAttemptSpec{
			WorktreePath: created.Path, BranchName: created.Branch, BaseRef: created.Base, BaseSHA: created.Base,
		}
	}
	if _, err = e.DB.CommitT2Assignment(ctx, assignment); err != nil {
		if worktrees != nil && created.Path != "" {
			_ = worktrees.Remove(ctx, created)
		}
		return err
	}
	return nil
}
