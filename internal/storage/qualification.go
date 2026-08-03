package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

type TopologyQualification struct {
	ID, QualificationKey, MethodVersion, AgentID, AgentDefinitionHash   string
	ExecutablePath, ExecutableSHA256, VersionOutputDigest, GOOS, GOARCH string
	Status, Reason, EvidenceJSON, EvidenceDigest                        string
	RecordedAtMS                                                        int64
}

func (d *DB) RecordTopologyQualification(ctx context.Context, q TopologyQualification) error {
	if q.ID == "" || len(q.QualificationKey) != 64 || q.AgentID == "" || q.MethodVersion == "" || q.EvidenceJSON == "" || q.RecordedAtMS <= 0 {
		return errors.New("storage: invalid topology qualification")
	}
	if q.EvidenceDigest == "" {
		sum := sha256.Sum256([]byte(q.EvidenceJSON))
		q.EvidenceDigest = hex.EncodeToString(sum[:])
	}
	_, err := d.db.ExecContext(ctx, `INSERT INTO agent_topology_qualifications (id,qualification_key,method_version,agent_id,agent_definition_hash,executable_path,executable_sha256,version_output_digest,goos,goarch,status,reason,evidence_json,evidence_digest,recorded_at_ms) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(qualification_key,evidence_digest) DO NOTHING`, q.ID, q.QualificationKey, q.MethodVersion, q.AgentID, q.AgentDefinitionHash, q.ExecutablePath, q.ExecutableSHA256, q.VersionOutputDigest, q.GOOS, q.GOARCH, q.Status, q.Reason, q.EvidenceJSON, q.EvidenceDigest, q.RecordedAtMS)
	return err
}

// ProcessGroupQualified is fail-closed: no row or any negative evidence means
// the exact key cannot authorize automatic absence recovery.
func (d *DB) ProcessGroupQualified(ctx context.Context, key string) (bool, error) {
	if len(key) != 64 {
		return false, nil
	}
	var negative int
	if err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_topology_qualifications WHERE qualification_key=? AND status='process-group-unverified'`, key).Scan(&negative); err != nil {
		return false, err
	}
	if negative != 0 {
		return false, nil
	}
	var verified int
	if err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_topology_qualifications WHERE qualification_key=? AND status='process-group-verified'`, key).Scan(&verified); err != nil {
		return false, err
	}
	return verified > 0, nil
}

func ReadTopologyQualificationStatus(ctx context.Context, path, key string) (string, string, error) {
	pool, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return "", "", err
	}
	defer pool.Close()
	var status, reason string
	err = pool.QueryRowContext(ctx, `SELECT CASE WHEN SUM(status='process-group-unverified')>0 THEN 'process-group-unverified' WHEN SUM(status='process-group-verified')>0 THEN 'process-group-verified' ELSE 'process-group-unverified' END, COALESCE((SELECT reason FROM agent_topology_qualifications WHERE qualification_key=? ORDER BY status='process-group-unverified' DESC, recorded_at_ms DESC LIMIT 1),'no-record') FROM agent_topology_qualifications WHERE qualification_key=?`, key, key).Scan(&status, &reason)
	if err == sql.ErrNoRows {
		return "process-group-unverified", "no-record", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("storage: read qualification: %w", err)
	}
	return status, reason, nil
}
