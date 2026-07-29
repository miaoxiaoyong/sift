package storage

import (
	"context"
	"errors"
)

// GateCandidate is the immutable Run identity plus the latest execution refs
// needed to freeze a Gate input after create_change has converged.
type GateCandidate struct {
	RunID, ProjectID, TaskKind, ChangeID, BaseRef, HeadRef string
	Version                                                int64
	AttemptNo, Generation                                  int
}

func (d *DB) GateCandidates(ctx context.Context, projectID string) ([]GateCandidate, error) {
	if projectID == "" {
		return nil, errors.New("storage: gate candidates require project")
	}
	rows, err := d.db.QueryContext(ctx, `SELECT r.id,r.project_id,COALESCE(r.kind,''),r.change_id,r.version,
		COALESCE((SELECT a.base_ref FROM attempts a WHERE a.run_id=r.id ORDER BY a.attempt_no DESC LIMIT 1),''),
		COALESCE((SELECT a.branch_name FROM attempts a WHERE a.run_id=r.id ORDER BY a.attempt_no DESC LIMIT 1),''),
		COALESCE((SELECT a.attempt_no FROM attempts a WHERE a.run_id=r.id ORDER BY a.attempt_no DESC LIMIT 1),0),
		COALESCE((SELECT a.generation FROM attempts a WHERE a.run_id=r.id ORDER BY a.attempt_no DESC LIMIT 1),0)
		FROM runs r WHERE r.project_id=? AND r.change_id IS NOT NULL
		AND r.status IN ('queued','running','waiting_human') ORDER BY r.updated_at_ms,r.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GateCandidate
	for rows.Next() {
		var c GateCandidate
		if err := rows.Scan(&c.RunID, &c.ProjectID, &c.TaskKind, &c.ChangeID, &c.Version, &c.BaseRef, &c.HeadRef, &c.AttemptNo, &c.Generation); err != nil {
			return nil, err
		}
		if c.TaskKind == "" || c.BaseRef == "" || c.HeadRef == "" {
			continue
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
