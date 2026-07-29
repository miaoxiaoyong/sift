package storage

import (
	"context"
	"encoding/json"
)

// RecordHandoffSecurityEvent preserves rejected wrapper handoffs without
// retaining credentials. A stale generation and a competing wrapper are both
// security-relevant: the rejection is the fencing boundary that prevented a
// second owner from obtaining spawn authority.
func (d *DB) RecordHandoffSecurityEvent(ctx context.Context, runID string, attemptNo int, method, disposition string, nowMS int64) error {
	if runID == "" || attemptNo < 1 || method == "" || disposition == "" || nowMS <= 0 {
		return nil
	}
	payload, _ := json.Marshal(map[string]string{"method": method, "disposition": disposition})
	_, err := d.db.ExecContext(ctx, `INSERT INTO events
		(id,run_id,attempt_no,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms)
		SELECT ?,?,?, 'security.handoff_rejected','agent',1,?,?,?
		WHERE EXISTS (SELECT 1 FROM attempts WHERE run_id=? AND attempt_no=?)`,
		newID(), runID, attemptNo, string(payload), nowMS, nowMS, runID, attemptNo)
	return err
}
