package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"database/sql"
	"encoding/json"
	"unicode/utf8"
)

type CertificationProjection struct {
	TaskKind, CertificationVersion                            string
	Certified                                                 bool
	TotalSamples, NegativeSamples, LeakCount, FalseBlockCount int
	WindowStartMS, WindowEndMS                                int64
}

func (d *DB) Certification(ctx context.Context, kind string) (CertificationProjection, error) {
	var p CertificationProjection
	var certified int
	err := d.db.QueryRowContext(ctx, `SELECT c.task_kind,c.certification_version,c.certified,c.total_samples,c.negative_samples,c.leak_count,c.false_block_count,c.window_start_ms,c.window_end_ms FROM certification_current x JOIN certifications c ON c.task_kind=x.task_kind AND c.certification_version=x.certification_version WHERE x.task_kind=?`, kind).Scan(&p.TaskKind, &p.CertificationVersion, &certified, &p.TotalSamples, &p.NegativeSamples, &p.LeakCount, &p.FalseBlockCount, &p.WindowStartMS, &p.WindowEndMS)
	p.Certified = certified != 0
	return p, err
}

// ProposalDraft is the inert persistence shape for a terminal T7 proposal.
// There is intentionally no approval, policy, context, Gate, or action field.
type ProposalDraft struct {
	ID                  string
	LogicalCallID       string
	PromptVersion       string
	OutputSchemaVersion int
	AggregateKey        string
	ProposalKind        string
	TargetScope         string
	Title               string
	Body                string
	EvidenceEntryIDs    []string
	Status              string
	CreatedAtMS         int64
}

type SaveProposalDraftCmd struct {
	LogicalCallID       string
	PromptVersion       string
	OutputSchemaVersion int
	AggregateKey        string
	ProposalKind        string
	TargetScope         string
	Title               string
	Body                string
	EvidenceEntryIDs    []string
	CreatedAtMS         int64
}

// SaveProposalDraft is the only proposal write port. It accepts only a
// terminal valid T7 call and insert-or-returns the identical draft. It does
// not create an outbox operation or touch any Gate, Interrupt, policy, or
// context projection.
func (d *DB) SaveProposalDraft(ctx context.Context, cmd SaveProposalDraftCmd) (ProposalDraft, error) {
	if cmd.LogicalCallID == "" || cmd.PromptVersion == "" || cmd.AggregateKey == "" || cmd.Title == "" || cmd.Body == "" || cmd.CreatedAtMS < 0 {
		return ProposalDraft{}, errors.New("storage: incomplete proposal draft")
	}
	if cmd.OutputSchemaVersion < 1 || (cmd.ProposalKind != "policy" && cmd.ProposalKind != "context") || (cmd.TargetScope != "project" && cmd.TargetScope != "global") || len(cmd.EvidenceEntryIDs) == 0 || len(cmd.Title) > 160 || len(cmd.Body) > 8192 || !utf8.ValidString(cmd.Title) || !utf8.ValidString(cmd.Body) || strings.ContainsAny(cmd.Title, "\r\n") || strings.ContainsAny(cmd.Body, "\r\x00") || strings.IndexFunc(cmd.Title, unicode.IsControl) >= 0 || strings.IndexFunc(cmd.Body, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) >= 0 {
		return ProposalDraft{}, errors.New("storage: invalid proposal draft contract")
	}
	ids := append([]string(nil), cmd.EvidenceEntryIDs...)
	for i, id := range ids {
		if id == "" || (i > 0 && ids[i-1] >= id) {
			return ProposalDraft{}, errors.New("storage: proposal evidence IDs must be sorted and unique")
		}
	}
	evidence, err := json.Marshal(ids)
	if err != nil {
		return ProposalDraft{}, fmt.Errorf("storage: encode proposal evidence: %w", err)
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return ProposalDraft{}, err
	}
	defer tx.Rollback()
	var touchpoint, status, callPrompt string
	var callSchema int
	if err := tx.QueryRowContext(ctx, `SELECT touchpoint,status,prompt_version,output_schema_version FROM brain_calls WHERE id=?`, cmd.LogicalCallID).Scan(&touchpoint, &status, &callPrompt, &callSchema); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProposalDraft{}, errors.New("storage: proposal call not found")
		}
		return ProposalDraft{}, err
	}
	if touchpoint != "T7" || status != BrainCallValid || callPrompt != cmd.PromptVersion || callSchema != cmd.OutputSchemaVersion {
		return ProposalDraft{}, errors.New("storage: proposal requires terminal valid T7 call")
	}
	requestedEvidence := string(evidence)
	id := newID()
	_, err = tx.ExecContext(ctx, `INSERT INTO proposal_drafts
		(id,logical_call_id,prompt_version,output_schema_version,aggregate_key,proposal_kind,target_scope,title,body,evidence_entry_ids,status,created_at_ms)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(logical_call_id) DO NOTHING`, id, cmd.LogicalCallID, cmd.PromptVersion, cmd.OutputSchemaVersion, cmd.AggregateKey, cmd.ProposalKind, cmd.TargetScope, cmd.Title, cmd.Body, string(evidence), "pending_human_approval", cmd.CreatedAtMS)
	if err != nil {
		return ProposalDraft{}, fmt.Errorf("storage: insert proposal draft: %w", err)
	}
	var out ProposalDraft
	err = tx.QueryRowContext(ctx, `SELECT id,logical_call_id,prompt_version,output_schema_version,aggregate_key,proposal_kind,target_scope,title,body,evidence_entry_ids,status,created_at_ms FROM proposal_drafts WHERE logical_call_id=?`, cmd.LogicalCallID).Scan(&out.ID, &out.LogicalCallID, &out.PromptVersion, &out.OutputSchemaVersion, &out.AggregateKey, &out.ProposalKind, &out.TargetScope, &out.Title, &out.Body, &evidence, &out.Status, &out.CreatedAtMS)
	if err != nil {
		return ProposalDraft{}, err
	}
	if out.PromptVersion != cmd.PromptVersion || out.OutputSchemaVersion != cmd.OutputSchemaVersion || out.AggregateKey != cmd.AggregateKey || out.ProposalKind != cmd.ProposalKind || out.TargetScope != cmd.TargetScope || out.Title != cmd.Title || out.Body != cmd.Body || string(evidence) != requestedEvidence || out.Status != "pending_human_approval" {
		return ProposalDraft{}, errors.New("storage: proposal draft content conflicts with existing call")
	}
	if err := json.Unmarshal(evidence, &out.EvidenceEntryIDs); err != nil {
		return ProposalDraft{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProposalDraft{}, err
	}
	return out, nil
}

func (d *DB) ProposalDraft(ctx context.Context, logicalCallID string) (ProposalDraft, error) {
	var out ProposalDraft
	var evidence string
	err := d.db.QueryRowContext(ctx, `SELECT id,logical_call_id,prompt_version,output_schema_version,aggregate_key,proposal_kind,target_scope,title,body,evidence_entry_ids,status,created_at_ms FROM proposal_drafts WHERE logical_call_id=?`, logicalCallID).Scan(&out.ID, &out.LogicalCallID, &out.PromptVersion, &out.OutputSchemaVersion, &out.AggregateKey, &out.ProposalKind, &out.TargetScope, &out.Title, &out.Body, &evidence, &out.Status, &out.CreatedAtMS)
	if err != nil {
		return ProposalDraft{}, err
	}
	if err := json.Unmarshal([]byte(evidence), &out.EvidenceEntryIDs); err != nil {
		return ProposalDraft{}, err
	}
	return out, nil
}
