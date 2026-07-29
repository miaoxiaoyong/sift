package storage

import "context"

// ReadyChangeCandidate is the persisted evidence a Gate-side verifier needs
// before it may enqueue create_change. It is read-only: EvaluateSuccess remains
// the authority that validates the worktree and agent identity.
type ReadyChangeCandidate struct {
	RunID, ProjectID, WorktreePath, Branch, Base string
	AttemptNo, Generation                        int
	Agent                                        AgentIdentity
	ExitCode                                     int
	FinalHeadSHA, Digest                         string
	FinishedAtMS                                 int64
}

// ReadyChangeCandidates returns successful finished attempts that have not
// yet converged to a remote Change. Re-reading a candidate is intentional:
// create_change is keyed by the verified head and is durably idempotent.
func (d *DB) ReadyChangeCandidates(ctx context.Context, projectID string) ([]ReadyChangeCandidate, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT a.run_id,r.project_id,a.worktree_path,a.branch_name,a.base_ref,a.attempt_no,a.generation,
		a.agent_pid,a.agent_started_at_ms,a.agent_executable,a.result_exit_code,a.final_head_sha,a.result_digest,a.finished_at_ms
		FROM attempts a JOIN runs r ON r.id=a.run_id
		WHERE r.project_id=? AND r.change_id IS NULL AND r.status NOT IN ('done','failed')
		AND a.phase='finished' AND a.result_exit_code=0 AND a.result_signal IS NULL
		AND a.final_head_sha IS NOT NULL AND a.result_digest IS NOT NULL
		AND a.agent_pid IS NOT NULL AND a.agent_started_at_ms IS NOT NULL AND a.agent_executable IS NOT NULL
		ORDER BY a.finished_at_ms,a.run_id,a.attempt_no`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReadyChangeCandidate
	for rows.Next() {
		var c ReadyChangeCandidate
		if err := rows.Scan(&c.RunID, &c.ProjectID, &c.WorktreePath, &c.Branch, &c.Base, &c.AttemptNo, &c.Generation,
			&c.Agent.PID, &c.Agent.StartedAtMS, &c.Agent.Executable, &c.ExitCode, &c.FinalHeadSHA, &c.Digest, &c.FinishedAtMS); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
