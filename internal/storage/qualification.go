package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	runtimepkg "github.com/xsift/sift/internal/runtime"
)

type TopologyQualification struct {
	ID, QualificationKey, MethodVersion, AgentID, AgentDefinitionHash   string
	ExecutablePath, ExecutableSHA256, VersionOutputDigest, GOOS, GOARCH string
	Status, Reason, EvidenceJSON, EvidenceDigest                        string
	RecordedAtMS                                                        int64
}

// TopologyQualificationEvidenceJSON returns the closed evidence projection
// accepted by RecordTopologyQualification. Keeping the identity and outcome in
// the signed bytes makes a valid hash from another binary or outcome useless.
func TopologyQualificationEvidenceJSON(q TopologyQualification) (string, error) {
	if q.Status != "process-group-verified" && q.Status != "process-group-unverified" {
		return "", errors.New("storage: invalid topology qualification status")
	}
	if (q.Status == "process-group-verified") != (q.Reason == "qualified") {
		return "", errors.New("storage: inconsistent topology qualification reason")
	}
	return marshalTopologyQualificationEvidence(q)
}

func marshalTopologyQualificationEvidence(q TopologyQualification) (string, error) {
	b, err := json.Marshal(struct {
		SchemaVersion       int    `json:"schema_version"`
		QualificationKey    string `json:"qualification_key"`
		MethodVersion       string `json:"method_version"`
		AgentID             string `json:"agent_id"`
		AgentDefinitionHash string `json:"agent_definition_hash"`
		ExecutablePath      string `json:"executable_path"`
		ExecutableSHA256    string `json:"executable_sha256"`
		VersionOutputDigest string `json:"version_output_digest"`
		GOOS                string `json:"goos"`
		GOARCH              string `json:"goarch"`
		Status              string `json:"status"`
		Reason              string `json:"reason"`
	}{1, q.QualificationKey, q.MethodVersion, q.AgentID, q.AgentDefinitionHash, q.ExecutablePath, q.ExecutableSHA256, q.VersionOutputDigest, q.GOOS, q.GOARCH, q.Status, q.Reason})
	return string(b), err
}

func (d *DB) RecordTopologyQualification(ctx context.Context, q TopologyQualification) error {
	if q.ID == "" || q.AgentID == "" || q.EvidenceJSON == "" || q.RecordedAtMS <= 0 || !isLowerHex(q.QualificationKey) || !isLowerHex(q.AgentDefinitionHash) || !isLowerHex(q.ExecutableSHA256) || !isLowerHex(q.VersionOutputDigest) {
		return errors.New("storage: invalid topology qualification")
	}
	key, err := runtimepkg.QualificationKey(runtimepkg.Qualification{MethodVersion: q.MethodVersion, AgentID: q.AgentID, AgentDefinitionHash: q.AgentDefinitionHash, ExecutablePath: q.ExecutablePath, ExecutableSHA256: q.ExecutableSHA256, VersionOutputDigest: q.VersionOutputDigest, GOOS: q.GOOS, GOARCH: q.GOARCH})
	if err != nil || key != q.QualificationKey {
		return errors.New("storage: inconsistent topology qualification key")
	}
	evidence, err := TopologyQualificationEvidenceJSON(q)
	if err != nil || q.EvidenceJSON != evidence {
		return errors.New("storage: inconsistent topology qualification evidence")
	}
	sum := sha256.Sum256([]byte(q.EvidenceJSON))
	digest := hex.EncodeToString(sum[:])
	if q.EvidenceDigest != "" && q.EvidenceDigest != digest {
		return errors.New("storage: inconsistent topology qualification evidence digest")
	}
	q.EvidenceDigest = digest
	_, err = d.db.ExecContext(ctx, `INSERT INTO agent_topology_qualifications (id,qualification_key,method_version,agent_id,agent_definition_hash,executable_path,executable_sha256,version_output_digest,goos,goarch,status,reason,evidence_json,evidence_digest,recorded_at_ms) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(qualification_key,evidence_digest) DO NOTHING`, q.ID, q.QualificationKey, q.MethodVersion, q.AgentID, q.AgentDefinitionHash, q.ExecutablePath, q.ExecutableSHA256, q.VersionOutputDigest, q.GOOS, q.GOARCH, q.Status, q.Reason, q.EvidenceJSON, q.EvidenceDigest, q.RecordedAtMS)
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
