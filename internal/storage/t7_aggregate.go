package storage

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
)

type AppendT7ReplayEvidenceCmd struct {
	Scope                         string
	ProjectID                     string
	TaskKind                      string
	WindowStartMS, WindowEndMS    int64
	DatasetVersion, GateVersion   string
	TotalSamples, NegativeSamples int64
	LeakCount, FalseBlockCount    int64
	CreatedAtMS                   int64
}

type T7CategoryEvidence struct {
	EvidenceID, TaskKind, CertificationVersion string
	Certified                                  bool
	WindowStartMS, WindowEndMS                 int64
	CertificationRulesVersion, EvidenceDigest  string
	TotalSamples, NegativeSamples              int64
	LeakCount, FalseBlockCount                 int64
}

type T7ReplaySummary struct {
	EvidenceID, DatasetVersion, GateVersion string
	TotalSamples, NegativeSamples           int64
	LeakCount, FalseBlockCount              int64
}

type T7SemanticMaterial struct {
	EntryID, MaterialKind, Text string
}

type PendingT7Aggregate struct {
	ReplayEvidenceID string
	AggregateKey     string
	ProjectID        string
	TaskKind         string
	WindowStartMS    int64
	WindowEndMS      int64
	Categories       []T7CategoryEvidence
	ReplaySummary    T7ReplaySummary
	SemanticMaterial []T7SemanticMaterial
}

func (d *DB) AppendT7ReplayEvidence(ctx context.Context, cmd AppendT7ReplayEvidenceCmd) (string, error) {
	if !validT7ReplayEvidence(cmd) {
		return "", errors.New("storage: invalid T7 replay evidence")
	}
	if cmd.Scope == "project" {
		var exists int
		if err := d.db.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id=?`, cmd.ProjectID).Scan(&exists); err != nil {
			return "", fmt.Errorf("storage: T7 replay project: %w", err)
		}
	}
	identity, err := canonicalJSON(map[string]any{
		"dataset_version": cmd.DatasetVersion, "false_block_count": cmd.FalseBlockCount,
		"gate_version": cmd.GateVersion, "leak_count": cmd.LeakCount,
		"negative_samples": cmd.NegativeSamples, "project_id": nullable(cmd.ProjectID),
		"scope": cmd.Scope, "task_kind": cmd.TaskKind, "total_samples": cmd.TotalSamples,
		"window_end_ms": cmd.WindowEndMS, "window_start_ms": cmd.WindowStartMS,
	})
	if err != nil {
		return "", err
	}
	evidenceID := "t7:replay:" + sha256Hex(identity)
	id := newID()
	_, err = d.db.ExecContext(ctx, `INSERT INTO t7_replay_evidence
		(id,scope,project_id,task_kind,window_start_ms,window_end_ms,dataset_version,gate_version,total_samples,negative_samples,leak_count,false_block_count,evidence_id,created_at_ms)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
		id, cmd.Scope, nullable(cmd.ProjectID), cmd.TaskKind, cmd.WindowStartMS, cmd.WindowEndMS,
		cmd.DatasetVersion, cmd.GateVersion, cmd.TotalSamples, cmd.NegativeSamples, cmd.LeakCount,
		cmd.FalseBlockCount, evidenceID, cmd.CreatedAtMS)
	if err != nil {
		return "", fmt.Errorf("storage: append T7 replay evidence: %w", err)
	}
	var existingID, existingEvidence string
	err = d.db.QueryRowContext(ctx, `SELECT id,evidence_id FROM t7_replay_evidence
		WHERE scope=? AND COALESCE(project_id,'')=? AND task_kind=? AND window_start_ms=? AND window_end_ms=?`,
		cmd.Scope, cmd.ProjectID, cmd.TaskKind, cmd.WindowStartMS, cmd.WindowEndMS).Scan(&existingID, &existingEvidence)
	if err != nil {
		return "", err
	}
	if existingEvidence != evidenceID {
		return "", errors.New("storage: T7 replay evidence conflicts with frozen window")
	}
	return existingID, nil
}

func validT7ReplayEvidence(cmd AppendT7ReplayEvidenceCmd) bool {
	scopeOK := cmd.Scope == "global" && cmd.ProjectID == "" || cmd.Scope == "project" && cmd.ProjectID != ""
	kindOK := cmd.TaskKind == "all" || validT7TaskKind(cmd.TaskKind)
	countsOK := cmd.TotalSamples >= 0 && cmd.NegativeSamples >= 0 && cmd.NegativeSamples <= cmd.TotalSamples && cmd.LeakCount >= 0 && cmd.LeakCount <= cmd.NegativeSamples && cmd.FalseBlockCount >= 0 && cmd.FalseBlockCount <= cmd.TotalSamples-cmd.NegativeSamples
	return scopeOK && kindOK && cmd.WindowStartMS >= 0 && cmd.WindowEndMS > cmd.WindowStartMS && len(cmd.DatasetVersion) >= 1 && len(cmd.DatasetVersion) <= 128 && len(cmd.GateVersion) >= 1 && len(cmd.GateVersion) <= 128 && countsOK && cmd.CreatedAtMS >= cmd.WindowEndMS
}

func validT7TaskKind(kind string) bool {
	switch kind {
	case "feature", "bug", "chore", "docs", "refactor":
		return true
	default:
		return false
	}
}

func (d *DB) PendingT7Aggregates(ctx context.Context, nowMS int64, limit int) ([]PendingT7Aggregate, error) {
	if nowMS < 0 || limit < 1 {
		return nil, errors.New("storage: invalid T7 aggregate scan")
	}
	rows, err := d.db.QueryContext(ctx, `SELECT r.id,r.scope,COALESCE(r.project_id,''),r.task_kind,r.window_start_ms,r.window_end_ms,r.evidence_id,r.dataset_version,r.gate_version,r.total_samples,r.negative_samples,r.leak_count,r.false_block_count
		FROM t7_replay_evidence r LEFT JOIN t7_aggregate_completions c ON c.replay_evidence_id=r.id
		WHERE c.aggregate_key IS NULL AND r.window_end_ms<=?
		AND ((r.task_kind='all' AND EXISTS (SELECT 1 FROM certification_current))
			OR EXISTS (SELECT 1 FROM certification_current x WHERE x.task_kind=r.task_kind))
		ORDER BY r.window_end_ms,r.window_start_ms,r.scope,COALESCE(r.project_id,''),r.task_kind,r.id LIMIT ?`, nowMS, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []PendingT7Aggregate
	for rows.Next() {
		var scope string
		var a PendingT7Aggregate
		if err := rows.Scan(&a.ReplayEvidenceID, &scope, &a.ProjectID, &a.TaskKind, &a.WindowStartMS, &a.WindowEndMS,
			&a.ReplaySummary.EvidenceID, &a.ReplaySummary.DatasetVersion, &a.ReplaySummary.GateVersion,
			&a.ReplaySummary.TotalSamples, &a.ReplaySummary.NegativeSamples, &a.ReplaySummary.LeakCount, &a.ReplaySummary.FalseBlockCount); err != nil {
			return nil, err
		}
		a.AggregateKey = t7AggregateKey(scope, a.ProjectID, a.TaskKind, a.WindowStartMS, a.WindowEndMS)
		candidates = append(candidates, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]PendingT7Aggregate, 0, len(candidates))
	for _, a := range candidates {
		categories, err := d.t7Categories(ctx, a.TaskKind)
		if err != nil {
			return nil, err
		}
		if len(categories) == 0 {
			continue
		}
		a.Categories = categories
		a.SemanticMaterial, err = d.t7SemanticMaterial(ctx, a.ProjectID, a.WindowStartMS, a.WindowEndMS)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func t7AggregateKey(scope, projectID, kind string, start, end int64) string {
	if scope == "global" {
		return fmt.Sprintf("aggregate:v1:global:%s:%d:%d", kind, start, end)
	}
	project := base64.RawURLEncoding.EncodeToString([]byte(projectID))
	return fmt.Sprintf("aggregate:v1:project:%s:%s:%d:%d", project, kind, start, end)
}

func (d *DB) t7Categories(ctx context.Context, kind string) ([]T7CategoryEvidence, error) {
	query := `SELECT c.task_kind,c.certification_version,c.certified,c.window_start_ms,c.window_end_ms,c.certification_rules_version,c.evidence_digest,c.total_samples,c.negative_samples,c.leak_count,c.false_block_count
		FROM certification_current x JOIN certifications c ON c.task_kind=x.task_kind AND c.certification_version=x.certification_version`
	args := []any{}
	if kind != "all" {
		query += ` WHERE c.task_kind=?`
		args = append(args, kind)
	}
	query += ` ORDER BY c.task_kind`
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T7CategoryEvidence
	for rows.Next() {
		var c T7CategoryEvidence
		var certified int
		if err := rows.Scan(&c.TaskKind, &c.CertificationVersion, &certified, &c.WindowStartMS, &c.WindowEndMS,
			&c.CertificationRulesVersion, &c.EvidenceDigest, &c.TotalSamples, &c.NegativeSamples, &c.LeakCount, &c.FalseBlockCount); err != nil {
			return nil, err
		}
		c.Certified = certified != 0
		c.EvidenceID = "t7:certification:" + c.TaskKind + ":" + c.CertificationVersion
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *DB) t7SemanticMaterial(ctx context.Context, projectID string, start, end int64) ([]T7SemanticMaterial, error) {
	query := `SELECT l.id,json_extract(l.features_json,'$.material_kind'),l.natural_language
		FROM ledger_entries l LEFT JOIN runs r ON r.id=l.run_id
		WHERE l.entry_kind='semantic_material' AND l.natural_language IS NOT NULL AND l.created_at_ms>=? AND l.created_at_ms<?`
	args := []any{start, end}
	if projectID != "" {
		query += ` AND r.project_id=?`
		args = append(args, projectID)
	}
	query += ` ORDER BY l.id LIMIT 64`
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T7SemanticMaterial
	for rows.Next() {
		var m T7SemanticMaterial
		if err := rows.Scan(&m.EntryID, &m.MaterialKind, &m.Text); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (d *DB) CompleteT7Aggregate(ctx context.Context, replayEvidenceID, aggregateKey, logicalCallID, outcome string, completedAtMS int64) error {
	if replayEvidenceID == "" || aggregateKey == "" || logicalCallID == "" || completedAtMS < 0 || outcome != BrainCallValid && outcome != BrainCallFallback {
		return errors.New("storage: invalid T7 aggregate completion")
	}
	_, err := d.db.ExecContext(ctx, `INSERT INTO t7_aggregate_completions(aggregate_key,replay_evidence_id,logical_call_id,outcome,completed_at_ms)
		SELECT ?,?,?,?,? FROM brain_calls b JOIN t7_replay_evidence r ON r.id=?
		WHERE b.id=? AND b.scope='aggregate' AND b.touchpoint='T7' AND b.subject_key=? AND b.status=?
		ON CONFLICT(aggregate_key) DO NOTHING`, aggregateKey, replayEvidenceID, logicalCallID, outcome, completedAtMS,
		replayEvidenceID, logicalCallID, aggregateKey, outcome)
	if err != nil {
		return fmt.Errorf("storage: complete T7 aggregate: %w", err)
	}
	var gotReplay, gotCall, gotOutcome string
	err = d.db.QueryRowContext(ctx, `SELECT replay_evidence_id,logical_call_id,outcome FROM t7_aggregate_completions WHERE aggregate_key=?`, aggregateKey).Scan(&gotReplay, &gotCall, &gotOutcome)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("storage: T7 aggregate completion does not match terminal call")
	}
	if err != nil {
		return err
	}
	if gotReplay != replayEvidenceID || gotCall != logicalCallID || gotOutcome != outcome {
		return errors.New("storage: T7 aggregate completion conflicts with existing cursor")
	}
	return nil
}
