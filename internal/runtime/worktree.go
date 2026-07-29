package runtime

import "github.com/miaoxiaoyong/sift/internal/worktree"

// WorktreeManager is the Runtime-facing worktree lifecycle and evidence port.
type WorktreeManager = worktree.Manager
type Worktree = worktree.Worktree
type ReadyChange = worktree.ReadyChange

func NewWorktreeManager(repo, root string) (*WorktreeManager, error) {
	return worktree.NewManager(repo, root)
}

var ErrNoCommit = worktree.ErrNoCommit
var ErrEvidenceRejected = worktree.ErrEvidenceRejected
