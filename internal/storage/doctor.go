package storage

import (
	"context"
	"database/sql"
	"fmt"
)

type HookBaseline struct {
	ProjectID string
	Digest    string
}

type DoctorAttempt struct {
	RunID          string
	AttemptNo      int
	Phase          string
	IsolationState string
	WorktreePath   string
	AgentID        string
}

// ReadDoctorState reads only diagnostic projections from an existing database.
// It never creates, migrates, or mutates the database.
func ReadDoctorState(ctx context.Context, path string) ([]HookBaseline, []DoctorAttempt, error) {
	pool, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, nil, fmt.Errorf("storage: open doctor database: %w", err)
	}
	defer pool.Close()
	hooks := []HookBaseline{}
	rows, err := pool.QueryContext(ctx, `SELECT project_id,baseline_digest FROM project_hook_baselines`)
	if err != nil {
		return nil, nil, fmt.Errorf("storage: read hook baselines: %w", err)
	}
	for rows.Next() {
		var h HookBaseline
		if err := rows.Scan(&h.ProjectID, &h.Digest); err != nil {
			rows.Close()
			return nil, nil, err
		}
		hooks = append(hooks, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	rows.Close()
	attempts := []DoctorAttempt{}
	rows, err = pool.QueryContext(ctx, `SELECT run_id,attempt_no,phase,isolation_state,worktree_path,agent_id FROM attempts WHERE isolation_state='frozen' OR phase NOT IN ('finished','orphaned') ORDER BY run_id,attempt_no`)
	if err != nil {
		return nil, nil, fmt.Errorf("storage: read attempt state: %w", err)
	}
	for rows.Next() {
		var a DoctorAttempt
		if err := rows.Scan(&a.RunID, &a.AttemptNo, &a.Phase, &a.IsolationState, &a.WorktreePath, &a.AgentID); err != nil {
			rows.Close()
			return nil, nil, err
		}
		attempts = append(attempts, a)
	}
	return hooks, attempts, rows.Err()
}
