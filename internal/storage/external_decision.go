package storage

import (
	"context"
	"database/sql"
)

// BindExternalDecisionForHead binds a Forge fact only when its Run/head has
// exactly one prior Gate calibration. Ambiguity is deliberately left unbound:
// selecting the latest evaluation would fabricate causality.
func (d *DB) BindExternalDecisionForHead(ctx context.Context, forgeFactEventID, runID, headSHA string, nowMS int64) (bool, error) {
	var calibrationID string
	err := d.db.QueryRowContext(ctx, `SELECT c.id
		FROM calibration_entries c
		JOIN gate_evaluations e ON e.id=c.gate_evaluation_id
		JOIN gate_input_snapshots s ON s.id=e.snapshot_id
		WHERE c.run_id=? AND s.head_sha=?
		ORDER BY c.id`, runID, headSHA).Scan(&calibrationID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var another string
	err = d.db.QueryRowContext(ctx, `SELECT c.id
		FROM calibration_entries c
		JOIN gate_evaluations e ON e.id=c.gate_evaluation_id
		JOIN gate_input_snapshots s ON s.id=e.snapshot_id
		WHERE c.run_id=? AND s.head_sha=? AND c.id<>?
		LIMIT 1`, runID, headSHA, calibrationID).Scan(&another)
	if err == nil {
		return false, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}
	if err := d.BindExternalDecision(ctx, forgeFactEventID, calibrationID, nowMS); err != nil {
		return false, err
	}
	return true, nil
}
