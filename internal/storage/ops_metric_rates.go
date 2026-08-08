package storage

import (
	"context"
	"database/sql"
	"fmt"
)

func (d *DB) falseReleaseRate(ctx context.Context, projectID string) (RatioMetric, error) {
	query := `SELECT COUNT(DISTINCT o.run_id) FROM outbox_operations o JOIN runs r ON r.id=o.run_id WHERE o.kind='merge_change' AND o.state='succeeded' AND r.status='done'`
	args := []any{}
	if projectID != "" {
		query += ` AND r.project_id=?`
		args = append(args, projectID)
	}
	var denom int
	if err := d.db.QueryRowContext(ctx, query, args...).Scan(&denom); err != nil {
		return RatioMetric{}, fmt.Errorf("storage: false-release denominator: %w", err)
	}
	return RatioMetric{
		Numerator:   0,
		Denominator: float64(denom),
		Rate:        0,
		Coverage:    "denominator = Sift-initiated merges (succeeded merge_change outbox); gate_bypassed manual merges excluded. numerator = post-merge revert/fix follow-ups, not yet written — fails closed at 0",
	}, nil
}

// gateBypassRate: gate_bypassed done Runs / all done Runs.
func (d *DB) gateBypassRate(ctx context.Context, runWhere string, runArgs []any) (RatioMetric, error) {
	query := `SELECT COALESCE(SUM(CASE WHEN gate_bypassed=1 THEN 1 ELSE 0 END),0), COUNT(*) FROM runs `
	if runWhere == "" {
		query += `WHERE status='done'`
	} else {
		query += runWhere + ` AND status='done'`
	}
	var bypassed, total int
	if err := d.db.QueryRowContext(ctx, query, runArgs...).Scan(&bypassed, &total); err != nil {
		return RatioMetric{}, fmt.Errorf("storage: gate bypass rate: %w", err)
	}
	m := RatioMetric{Numerator: float64(bypassed), Denominator: float64(total), Coverage: "gate_bypassed done / all done (PRD §10.2)"}
	if total > 0 {
		m.Rate = float64(bypassed) / float64(total)
	}
	return m, nil
}

// gateConfusionRates derives the Gate miss (leak) and false-block rates from
// settled calibration samples (ledger.md §4.1). Gate miss uses negative samples
// (human blocks) as the denominator; false-block uses positive samples.
func (d *DB) gateConfusionRates(ctx context.Context) (RatioMetric, RatioMetric, error) {
	var leak, fblock, negative, positive sql.NullInt64
	err := d.db.QueryRowContext(ctx, `SELECT
		SUM(CASE WHEN predicted_decision='allow' AND human_decision='block' THEN 1 ELSE 0 END),
		SUM(CASE WHEN predicted_decision='block' AND human_decision='allow' THEN 1 ELSE 0 END),
		SUM(CASE WHEN human_decision='block' THEN 1 ELSE 0 END),
		SUM(CASE WHEN human_decision='allow' THEN 1 ELSE 0 END)
		FROM calibration_entries WHERE human_decision IN ('allow','block') AND predicted_decision IN ('allow','block')`).Scan(&leak, &fblock, &negative, &positive)
	if err != nil {
		return RatioMetric{}, RatioMetric{}, fmt.Errorf("storage: gate confusion rates: %w", err)
	}
	miss := RatioMetric{Numerator: float64(leak.Int64), Denominator: float64(negative.Int64), Coverage: "Gate leak / negative samples; authoritative per-kind windows are ledger.md §4; aggregate here"}
	if negative.Int64 > 0 {
		miss.Rate = float64(leak.Int64) / float64(negative.Int64)
	}
	fb := RatioMetric{Numerator: float64(fblock.Int64), Denominator: float64(positive.Int64), Coverage: "Gate false-block / positive samples (human allows); authoritative per-kind windows are ledger.md §4"}
	if positive.Int64 > 0 {
		fb.Rate = float64(fblock.Int64) / float64(positive.Int64)
	}
	return miss, fb, nil
}

// hitlRate: Runs that produced at least one Interrupt (the human-attention
// request) / all Runs. Interrupt presence is the human-attention signal; a
// waiting_human projection refines this later without changing the definition.
func (d *DB) hitlRate(ctx context.Context, projectID string) (RatioMetric, error) {
	var hitlRuns int
	hArgs := []any{}
	hQuery := `SELECT COUNT(DISTINCT run_id) FROM interrupts`
	if projectID != "" {
		hQuery = `SELECT COUNT(DISTINCT i.run_id) FROM interrupts i JOIN runs r ON r.id=i.run_id WHERE r.project_id=?`
		hArgs = append(hArgs, projectID)
	}
	if err := d.db.QueryRowContext(ctx, hQuery, hArgs...).Scan(&hitlRuns); err != nil {
		return RatioMetric{}, fmt.Errorf("storage: hitl runs: %w", err)
	}
	var totalRuns int
	tArgs := []any{}
	tQuery := `SELECT COUNT(*) FROM runs`
	if projectID != "" {
		tQuery = `SELECT COUNT(*) FROM runs WHERE project_id=?`
		tArgs = append(tArgs, projectID)
	}
	if err := d.db.QueryRowContext(ctx, tQuery, tArgs...).Scan(&totalRuns); err != nil {
		return RatioMetric{}, fmt.Errorf("storage: total runs: %w", err)
	}
	m := RatioMetric{Numerator: float64(hitlRuns), Denominator: float64(totalRuns), Coverage: "Runs with ≥1 Interrupt / all Runs"}
	if totalRuns > 0 {
		m.Rate = float64(hitlRuns) / float64(totalRuns)
	}
	return m, nil
}

// attentionQuotaConsumption reads the latest daily attention bucket per
// severity from persisted budget_counters — the current/today bucket as the
// daemon wrote it, with no live timezone dependency.
func (d *DB) attentionQuotaConsumption(ctx context.Context) ([]QuotaConsumption, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT scope_id, consumed_value, limit_value FROM budget_counters WHERE kind='attention' AND scope='severity' AND (scope_id, bucket_start_ms) IN (SELECT scope_id, MAX(bucket_start_ms) FROM budget_counters WHERE kind='attention' AND scope='severity' GROUP BY scope_id) ORDER BY scope_id`)
	if err != nil {
		return nil, fmt.Errorf("storage: attention quota consumption: %w", err)
	}
	defer rows.Close()
	out := []QuotaConsumption{}
	for rows.Next() {
		var sev string
		var consumed, limit int
		if err := rows.Scan(&sev, &consumed, &limit); err != nil {
			return nil, err
		}
		q := QuotaConsumption{Severity: sev, Consumed: consumed, Limit: limit}
		if limit > 0 {
			q.Rate = float64(consumed) / float64(limit)
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// dispatchAccuracy: T2-chosen agent unchanged by a human. V0 has no human
// agent-rewrite path, so the rate is structural (no rewrites are possible),
// reported with an explicit note rather than an empirical claim.
func (d *DB) dispatchAccuracy(ctx context.Context, projectID string) (RatioMetric, error) {
	query := `SELECT COUNT(DISTINCT run_id) FROM brain_calls WHERE touchpoint='T2' AND status='valid'`
	args := []any{}
	if projectID != "" {
		query += ` AND project_id=?`
		args = append(args, projectID)
	}
	var assigned int
	if err := d.db.QueryRowContext(ctx, query, args...).Scan(&assigned); err != nil {
		return RatioMetric{}, fmt.Errorf("storage: dispatch accuracy: %w", err)
	}
	m := RatioMetric{Numerator: float64(assigned), Denominator: float64(assigned), Coverage: "V0 has no human agent-rewrite command/event; rate is structural (100% when a T2 assignment exists), not an empirical measurement"}
	if assigned > 0 {
		m.Rate = 1
	}
	return m, nil
}

// llmCost sums valid Brain provider-attempt token usage over merged Changes.
func (d *DB) llmCost(ctx context.Context, mergedChanges int) (LLMCostMetric, error) {
	var input, output sql.NullInt64
	if err := d.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0) FROM brain_attempts WHERE outcome='valid'`).Scan(&input, &output); err != nil {
		return LLMCostMetric{}, fmt.Errorf("storage: llm cost: %w", err)
	}
	m := LLMCostMetric{InputTokens: input.Int64, OutputTokens: output.Int64, MergedChanges: mergedChanges, Coverage: "token counts from valid Brain provider attempts; currency mapping is a later pricing decision"}
	if mergedChanges > 0 {
		total := float64(input.Int64 + output.Int64)
		mc := float64(mergedChanges)
		m.PerMergedChangeTokens = total / mc
		m.PerMergedChangeInput = float64(input.Int64) / mc
		m.PerMergedChangeOutput = float64(output.Int64) / mc
	}
	return m, nil
}

// This file holds the read projections that back operator surfaces
// (control-plane.md §6): the durable ps/timeline queries over runs, attempts,
// events and the persisted attention buckets. They never mutate state.

// PSQuery scopes a ps listing.
