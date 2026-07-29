package gate

import (
	"context"
	"fmt"
	"time"

	"github.com/miaoxiaoyong/sift/internal/attempt"
	"github.com/miaoxiaoyong/sift/internal/storage"
	"github.com/miaoxiaoyong/sift/internal/worktree"
)

// SuccessReconciler is the production bridge from completed execution evidence
// to the durable create_change operation. It deliberately reuses
// worktree.EvaluateSuccess rather than treating exit code zero as sufficient
// evidence for a Change.
type SuccessReconciler struct {
	DB        *storage.DB
	ProjectID string
	Worktrees *worktree.Manager
	Now       func() time.Time
}

// ReconcileOnce validates each finished successful attempt against its frozen
// worktree and agent identity. A rejected/no-commit attempt is left without a
// Change; only a verified ReadyChange can reach EnqueueCreateChange.
func (r *SuccessReconciler) ReconcileOnce(ctx context.Context) error {
	if r.DB == nil || r.ProjectID == "" || r.Worktrees == nil {
		return fmt.Errorf("gate success reconciler: incomplete configuration")
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	candidates, err := r.DB.ReadyChangeCandidates(ctx, r.ProjectID)
	if err != nil {
		return err
	}
	for _, c := range candidates {
		ready, err := r.Worktrees.EvaluateSuccess(ctx,
			worktree.Worktree{Path: c.WorktreePath, Branch: c.Branch, Base: c.Base}, c.RunID,
			attempt.Result{ExitCode: &c.ExitCode, FinalHeadSHA: c.FinalHeadSHA, Digest: c.Digest, Agent: attempt.Identity{PID: int(c.Agent.PID), StartedAtMS: c.Agent.StartedAtMS, Executable: c.Agent.Executable}},
			attempt.Identity{PID: int(c.Agent.PID), StartedAtMS: c.Agent.StartedAtMS, Executable: c.Agent.Executable})
		if err != nil {
			// Invalid or incomplete execution evidence is not an authorization to
			// create a Change. It remains observable to the existing attempt
			// recovery/termination paths.
			continue
		}
		if err := EnqueueCreateChange(ctx, r.DB, ready, r.ProjectID, "Sift change for "+c.RunID, "Verified execution result for run "+c.RunID+".", now().UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}
