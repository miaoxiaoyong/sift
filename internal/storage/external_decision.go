package storage

import (
	"context"
	"encoding/json"
)

// IsSiftMerge returns true only when the observed Change/head is backed by a
// succeeded merge_change operation carrying its immutable Gate evaluation ID.
// Reverse-sync uses this causal identity rather than inferring intent from a
// Run/head calibration.
func (d *DB) IsSiftMerge(ctx context.Context, runID, changeID, headSHA string) (bool, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT payload_json FROM outbox_operations WHERE run_id=? AND kind='merge_change' AND state='succeeded'`, runID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		var payload struct {
			ChangeID         string `json:"change_id"`
			GateEvaluationID string `json:"gate_evaluation_id"`
			ExpectedHeadSHA  string `json:"expected_head_sha"`
		}
		if json.Unmarshal(raw, &payload) == nil && payload.ChangeID == changeID && payload.ExpectedHeadSHA == headSHA && payload.GateEvaluationID != "" {
			return true, nil
		}
	}
	return false, rows.Err()
}
