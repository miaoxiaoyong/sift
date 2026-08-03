package storage

import (
	"context"
	"database/sql"
	"errors"
)

var (
	ErrAttachRunNotFound = errors.New("storage: attach run not found")
	ErrAttachConflict    = errors.New("storage: attach target conflict")
)

type AttachTarget struct {
	RunID      string
	AttemptNo  int
	Generation int
	Backend    string
	DispatchID string
}

// AttachTargetForRun is a read-only projection of the current durable launch
// identity. It deliberately does not accept a session name or mutate any
// execution projection.
func (d *DB) AttachTargetForRun(ctx context.Context, runID string) (AttachTarget, error) {
	if runID == "" {
		return AttachTarget{}, ErrAttachRunNotFound
	}
	rows, err := d.db.QueryContext(ctx, `SELECT a.attempt_no,a.generation,a.backend,c.dispatch_id
		FROM attempts a JOIN attempt_claims c ON c.run_id=a.run_id AND c.attempt_no=a.attempt_no AND c.generation=a.generation
		WHERE a.run_id=? AND a.phase IN ('pending','starting','spawning','running')
		ORDER BY a.attempt_no`, runID)
	if err != nil {
		return AttachTarget{}, err
	}
	defer rows.Close()
	var target AttachTarget
	count := 0
	for rows.Next() {
		var dispatch sql.NullString
		count++
		if err := rows.Scan(&target.AttemptNo, &target.Generation, &target.Backend, &dispatch); err != nil {
			return AttachTarget{}, err
		}
		if !dispatch.Valid || dispatch.String == "" {
			return AttachTarget{}, ErrAttachConflict
		}
		target.DispatchID = dispatch.String
	}
	if err := rows.Err(); err != nil {
		return AttachTarget{}, err
	}
	if count == 0 {
		var exists int
		if err := d.db.QueryRowContext(ctx, `SELECT 1 FROM runs WHERE id=?`, runID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return AttachTarget{}, ErrAttachRunNotFound
		} else if err != nil {
			return AttachTarget{}, err
		}
		return AttachTarget{}, ErrAttachConflict
	}
	if count != 1 {
		return AttachTarget{}, ErrAttachConflict
	}
	target.RunID = runID
	return target, nil
}
