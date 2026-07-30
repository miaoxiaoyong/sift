package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/miaoxiaoyong/sift/internal/config"
)

// This file is the deterministic read port for PRD §10.2 metrics (WBS §5.7).
// Every value below is derived from persisted events / Ledger / budget rows
// the M1–M5 write ports have already produced. Where the field that a metric
// needs has not been written yet (false-release numerator, empirical dispatch
// rewrites, real-world latency), the query fails closed: it reports the
// denominator and an explicit Coverage note rather than inventing a number.
// The frozen config snapshot — not the live config — supplies the north-star
// reason weights (config.md §3.13); history is never recomputed.

// MetricsQuery scopes a metric report. ProjectID empty means global.
type MetricsQuery struct {
	ProjectID string
}

// RatioMetric is a numerator/denominator pair with its honest V0 coverage note.
// Rate is numerator/denominator (0 when the denominator is 0).
type RatioMetric struct {
	Numerator   float64 `json:"numerator"`
	Denominator float64 `json:"denominator"`
	Rate        float64 `json:"rate"`
	Coverage    string  `json:"coverage"`
}

// WeightedAttentionMetric is the PRD §10.2 north star: weighted attention
// minutes per merged Change. Weights come from the frozen config snapshot of
// each delivered Interrupt's Run, never the current config.
type WeightedAttentionMetric struct {
	WeightedMinutes         float64 `json:"weighted_minutes"`
	DeliveredMetricIdentity int     `json:"delivered_metric_identities"`
	MergedChanges           int     `json:"merged_changes"`
	PerMergedChange         float64 `json:"per_merged_change"`
	Coverage                string  `json:"coverage"`
}

// QuotaConsumption is one severity's latest daily attention bucket.
type QuotaConsumption struct {
	Severity string  `json:"severity"`
	Consumed int     `json:"consumed"`
	Limit    int     `json:"limit"`
	Rate     float64 `json:"rate"`
}

// LLMCostMetric is the token-cost-per-merged-Change series. V0 reports token
// counts; mapping tokens to currency is a later pricing decision.
type LLMCostMetric struct {
	InputTokens           int64   `json:"input_tokens"`
	OutputTokens          int64   `json:"output_tokens"`
	MergedChanges         int     `json:"merged_changes"`
	PerMergedChangeTokens float64 `json:"per_merged_change_total_tokens"`
	PerMergedChangeInput  float64 `json:"per_merged_change_input_tokens"`
	PerMergedChangeOutput float64 `json:"per_merged_change_output_tokens"`
	Coverage              string  `json:"coverage"`
}

// MetricsReport is the nine PRD §10.2 series derived from persisted state.
type MetricsReport struct {
	Scope string `json:"scope"` // "global" or a project id

	WeightedAttentionPerChange WeightedAttentionMetric `json:"weighted_attention_per_merged_change"`
	FalseReleaseRate           RatioMetric             `json:"false_release_rate"`
	GateBypassRate             RatioMetric             `json:"gate_bypass_rate"`
	GateMissRate               RatioMetric             `json:"gate_miss_rate"`
	GateFalseBlockRate         RatioMetric             `json:"gate_false_block_rate"`
	HITLRate                   RatioMetric             `json:"hitl_rate"`
	AttentionQuotaConsumption  []QuotaConsumption      `json:"attention_quota_consumption"`
	DispatchAccuracy           RatioMetric             `json:"dispatch_accuracy"`
	LLMCostPerMergedChange     LLMCostMetric           `json:"llm_cost_per_merged_change"`
}

// Metrics derives the nine PRD §10.2 metric series deterministically. It is a
// read-only aggregate and never mutates state.
func (d *DB) Metrics(ctx context.Context, q MetricsQuery) (MetricsReport, error) {
	scope := "global"
	runWhere, runArgs := "", []any{}
	if q.ProjectID != "" {
		scope = q.ProjectID
		runWhere = "WHERE project_id=?"
		runArgs = []any{q.ProjectID}
	}
	report := MetricsReport{Scope: scope, AttentionQuotaConsumption: []QuotaConsumption{}}

	mergedChanges, err := d.countDoneChanges(ctx, runWhere, runArgs)
	if err != nil {
		return report, err
	}

	weights, err := d.weightedAttention(ctx, q.ProjectID, mergedChanges)
	if err != nil {
		return report, err
	}
	report.WeightedAttentionPerChange = weights

	report.FalseReleaseRate, err = d.falseReleaseRate(ctx, q.ProjectID)
	if err != nil {
		return report, err
	}
	report.GateBypassRate, err = d.gateBypassRate(ctx, runWhere, runArgs)
	if err != nil {
		return report, err
	}
	report.GateMissRate, report.GateFalseBlockRate, err = d.gateConfusionRates(ctx)
	if err != nil {
		return report, err
	}
	report.HITLRate, err = d.hitlRate(ctx, q.ProjectID)
	if err != nil {
		return report, err
	}
	report.AttentionQuotaConsumption, err = d.attentionQuotaConsumption(ctx)
	if err != nil {
		return report, err
	}
	report.DispatchAccuracy, err = d.dispatchAccuracy(ctx, q.ProjectID)
	if err != nil {
		return report, err
	}
	report.LLMCostPerMergedChange, err = d.llmCost(ctx, mergedChanges)
	if err != nil {
		return report, err
	}
	return report, nil
}

// LatencySample is one run's trigger→started latency (PRD §10.2).
type LatencySample struct {
	RunID               string `json:"run_id"`
	TriggerObservedAtMS int64  `json:"trigger_observed_at_ms"`
	AgentStartedAtMS    int64  `json:"agent_started_at_ms"`
	LatencyMS           int64  `json:"latency_ms"`
}

// LatencyDistribution summarizes trigger-label→agent-started latency over all
// runs that have both persisted anchors. Real P50 < 60s remains the M7
// acceptance; this query exposes the fixture/production path over events.
type LatencyDistribution struct {
	Count    int             `json:"count"`
	MinMS    int64           `json:"min_ms"`
	P50MS    int64           `json:"p50_ms"`
	P90MS    int64           `json:"p90_ms"`
	MaxMS    int64           `json:"max_ms"`
	Samples  []LatencySample `json:"samples"`
	Coverage string          `json:"coverage"`
}

// TriggerStartedLatency computes the trigger→started distribution from the
// append-only event stream: the first `intake.trigger_observed` (P50 start
// anchor) and the first `run.transitioned` to `running` (P50 end anchor). Runs
// missing either anchor are excluded rather than imputed.
func (d *DB) TriggerStartedLatency(ctx context.Context, q MetricsQuery) (LatencyDistribution, error) {
	dist := LatencyDistribution{Samples: []LatencySample{}, Coverage: "trigger→started latency over persisted events; real P50<60s is the M7 acceptance, not this slice"}
	args := []any{}
	query := `SELECT run_id, occurred_at_ms, payload_json FROM events WHERE type='intake.trigger_observed'`
	if q.ProjectID != "" {
		query += ` AND project_id=?`
		args = append(args, q.ProjectID)
	}
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return dist, fmt.Errorf("storage: latency trigger events: %w", err)
	}
	type anchor struct{ observed, started int64 }
	anchors := map[string]*anchor{}
	for rows.Next() {
		var runID string
		var observed int64
		var payload []byte
		if err := rows.Scan(&runID, &observed, &payload); err != nil {
			rows.Close()
			return dist, err
		}
		if a, ok := anchors[runID]; ok {
			if observed < a.observed {
				a.observed = observed
			}
		} else {
			anchors[runID] = &anchor{observed: observed}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return dist, err
	}
	rows.Close()

	startArgs := []any{}
	startQuery := `SELECT run_id, occurred_at_ms, payload_json FROM events WHERE type='run.transitioned'`
	if q.ProjectID != "" {
		startQuery += ` AND project_id=?`
		startArgs = append(startArgs, q.ProjectID)
	}
	startRows, err := d.db.QueryContext(ctx, startQuery, startArgs...)
	if err != nil {
		return dist, fmt.Errorf("storage: latency transition events: %w", err)
	}
	defer startRows.Close()
	for startRows.Next() {
		var runID string
		var occurred int64
		var payload []byte
		if err := startRows.Scan(&runID, &occurred, &payload); err != nil {
			return dist, err
		}
		var p struct {
			To string `json:"to"`
		}
		if json.Unmarshal(payload, &p) != nil || p.To != string(RunRunning) {
			continue
		}
		a, ok := anchors[runID]
		if !ok {
			continue
		}
		if a.started == 0 || occurred < a.started {
			a.started = occurred
		}
	}
	if err := startRows.Err(); err != nil {
		return dist, err
	}

	for runID, a := range anchors {
		// Zero latency (started == observed) is a valid instantaneous start; only
		// negative latency (started < observed) is excluded as an invalid anchor.
		if a.observed <= 0 || a.started <= 0 || a.started < a.observed {
			continue
		}
		dist.Samples = append(dist.Samples, LatencySample{RunID: runID, TriggerObservedAtMS: a.observed, AgentStartedAtMS: a.started, LatencyMS: a.started - a.observed})
	}
	sort.Slice(dist.Samples, func(i, j int) bool {
		if dist.Samples[i].LatencyMS != dist.Samples[j].LatencyMS {
			return dist.Samples[i].LatencyMS < dist.Samples[j].LatencyMS
		}
		return dist.Samples[i].RunID < dist.Samples[j].RunID
	})
	dist.Count = len(dist.Samples)
	if dist.Count == 0 {
		dist.MinMS, dist.P50MS, dist.P90MS, dist.MaxMS = 0, 0, 0, 0
		return dist, nil
	}
	latencies := make([]int64, dist.Count)
	for i, s := range dist.Samples {
		latencies[i] = s.LatencyMS
	}
	dist.MinMS = latencies[0]
	dist.MaxMS = latencies[dist.Count-1]
	dist.P50MS = percentile(latencies, 50)
	dist.P90MS = percentile(latencies, 90)
	return dist, nil
}

// percentile returns the nearest-rank percentile of a pre-sorted slice (1..99).
func percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	if p < 1 {
		p = 1
	}
	if p > 99 {
		p = 99
	}
	// Nearest-rank: the smallest value at or above the p-th percentile rank.
	rank := (p*len(sorted) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// countDoneChanges counts merged Changes: every done Run carries a change_id
// (the runs CHECK constraint guarantees status='done' ⇒ change_id NOT NULL).
func (d *DB) countDoneChanges(ctx context.Context, runWhere string, runArgs []any) (int, error) {
	var n int
	query := `SELECT COUNT(*) FROM runs `
	if runWhere == "" {
		query += `WHERE status='done'`
	} else {
		query += runWhere + ` AND status='done'`
	}
	if err := d.db.QueryRowContext(ctx, query, runArgs...).Scan(&n); err != nil {
		return 0, fmt.Errorf("storage: count merged changes: %w", err)
	}
	return n, nil
}

// weightedAttention sums each delivered metric identity's frozen reason weight.
func (d *DB) weightedAttention(ctx context.Context, projectID string, mergedChanges int) (WeightedAttentionMetric, error) {
	m := WeightedAttentionMetric{MergedChanges: mergedChanges, Coverage: "weights from each Run's frozen config_snapshot; response interval never used as human-minutes"}
	// deliveredExists is the OR of the two delivery projections. It must be
	// parenthesized when combined with a project filter so AND does not bind
	// only the first branch and leak the second (batch) path across projects.
	deliveredExists := `EXISTS (SELECT 1 FROM interrupt_deliveries d WHERE d.interrupt_id=i.id AND d.state='delivered') OR EXISTS (SELECT 1 FROM attention_batch_members m JOIN batch_deliveries b ON b.delivery_id=m.delivery_id WHERE m.interrupt_id=i.id AND b.state='delivered')`
	query := `SELECT i.id, i.run_id, i.reason FROM interrupts i`
	args := []any{}
	if projectID != "" {
		query += ` JOIN runs r ON r.id=i.run_id WHERE r.project_id=? AND (` + deliveredExists + `)`
		args = append(args, projectID)
	} else {
		query += ` WHERE ` + deliveredExists
	}
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return m, fmt.Errorf("storage: weighted attention deliveries: %w", err)
	}
	type delivered struct {
		runID, reason string
	}
	var list []delivered
	for rows.Next() {
		var id, runID, reason string
		if err := rows.Scan(&id, &runID, &reason); err != nil {
			rows.Close()
			return m, err
		}
		list = append(list, delivered{runID: runID, reason: reason})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return m, err
	}
	rows.Close()

	weights := map[string]float64{}
	var total float64
	for _, dv := range list {
		w, err := d.reasonWeight(ctx, dv.runID, dv.reason, weights)
		if err != nil {
			return m, err
		}
		total += w
	}
	m.WeightedMinutes = total
	m.DeliveredMetricIdentity = len(list)
	if mergedChanges > 0 {
		m.PerMergedChange = total / float64(mergedChanges)
	}
	return m, nil
}

// reasonWeight resolves a reason's weight from the Run's frozen snapshot. The
// cache avoids re-parsing the same snapshot across delivered interrupts.
func (d *DB) reasonWeight(ctx context.Context, runID, reason string, cache map[string]float64) (float64, error) {
	var snapshotID string
	if err := d.db.QueryRowContext(ctx, `SELECT config_snapshot_id FROM runs WHERE id=?`, runID).Scan(&snapshotID); err != nil {
		return 0, fmt.Errorf("storage: run snapshot for weight: %w", err)
	}
	if w, ok := cache[snapshotID+":"+reason]; ok {
		return w, nil
	}
	var canonical []byte
	if err := d.db.QueryRowContext(ctx, `SELECT canonical_json FROM config_snapshots WHERE id=?`, snapshotID).Scan(&canonical); err != nil {
		return 0, fmt.Errorf("storage: snapshot canonical for weight: %w", err)
	}
	w := snapshotReasonWeight(canonical, reason)
	if w < 0 {
		// The frozen snapshot is the authoritative source. If a future schema
		// omits a reason, fall back to the binary's default rather than 0 so a
		// missing key cannot silently zero the north star.
		w = defaultReasonWeight(reason)
	}
	cache[snapshotID+":"+reason] = w
	return w, nil
}

// snapshotReasonWeight extracts one reason's weight from a canonical config
// snapshot. The canonical form stores metrics as a flat object
// {"metrics":{"design_approval":10,...}} (config CanonicalJSON). Returns -1 if
// the reason is absent so the caller can apply a documented default.
func snapshotReasonWeight(canonical []byte, reason string) float64 {
	var snap struct {
		Metrics map[string]float64 `json:"metrics"`
	}
	if err := json.Unmarshal(canonical, &snap); err != nil || snap.Metrics == nil {
		return -1
	}
	v, ok := snap.Metrics[reason]
	if !ok {
		return -1
	}
	return v
}

// defaultReasonWeight mirrors config.md §3.13 defaults, used only when a frozen
// snapshot is missing a reason (it never is for a binary-produced snapshot).
func defaultReasonWeight(reason string) float64 {
	def := config.DefaultConfig().Metrics
	switch reason {
	case string(InterruptDesignApproval):
		return def.DesignApproval
	case string(InterruptGuardrailViolation):
		return def.GuardrailViolation
	case string(InterruptCodeReview):
		return def.CodeReview
	case string(InterruptAgentBlocked):
		return def.AgentBlocked
	case string(InterruptMergeConflict):
		return def.MergeConflict
	case string(InterruptFailureReview):
		return def.FailureReview
	case string(InterruptStartupStall):
		return def.StartupStall
	}
	return 0
}

// falseReleaseRate: denominator = Sift-initiated merges (done Runs backed by a
// succeeded merge_change outbox operation). gate_bypassed manual merges have no
// such operation and are therefore excluded (PRD §10.2 quality red line).
// Numerator = post-merge revert/fix follow-ups, which V0 does not yet write;
// it fails closed at 0.
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
type PSQuery struct {
	RunID           string // exact run filter; empty means list
	ProjectID       string
	Status          string // RunStatus; empty means any
	Limit           int
	AfterRunID      string         // keyset pagination cursor
	ConfiguredQuota map[string]int // optional live per-severity daily ceilings
}

// PSAttempt is the current-attempt projection shown by ops.ps.
type PSAttempt struct {
	AttemptNo      int    `json:"attempt_no"`
	Generation     int    `json:"generation"`
	Phase          string `json:"phase"`
	IsolationState string `json:"isolation_state"`
	HeartbeatAtMS  int64  `json:"heartbeat_at_ms"`
}

// PSRun is one row of an ops.ps listing.
type PSRun struct {
	RunID              string     `json:"run_id"`
	ProjectID          string     `json:"project_id"`
	Status             string     `json:"status"`
	Version            int64      `json:"version"`
	Attempt            *PSAttempt `json:"attempt"`
	OpenInterruptCount int        `json:"open_interrupt_count"`
	PendingOutboxCount int        `json:"pending_outbox_count"`
	GateBypassed       bool       `json:"gate_bypassed"`
	UpdatedAtMS        int64      `json:"updated_at_ms"`
}

// PSReport is the ops.ps result.
type PSReport struct {
	Runs               []PSRun        `json:"runs"`
	NextAfterRunID     string         `json:"next_after_run_id"`
	AttentionRemaining map[string]int `json:"attention_remaining"`
}

// RunPS lists Runs with their current attempt, open-Interrupt / pending-outbox
// counts and today's remaining attention quota per severity. It is keyset
// paginated by run_id ascending (control-plane.md §6.2).
func (d *DB) RunPS(ctx context.Context, q PSQuery) (PSReport, error) {
	if q.Limit < 1 {
		q.Limit = 100
	}
	if q.Limit > 1000 {
		q.Limit = 1000
	}
	report := PSReport{Runs: []PSRun{}, AttentionRemaining: map[string]int{}}

	where := ""
	args := []any{}
	if q.RunID != "" {
		where = "WHERE r.id=?"
		args = append(args, q.RunID)
	} else {
		conds := []string{}
		if q.ProjectID != "" {
			conds = append(conds, "r.project_id=?")
			args = append(args, q.ProjectID)
		}
		if q.Status != "" {
			conds = append(conds, "r.status=?")
			args = append(args, q.Status)
		}
		if q.AfterRunID != "" {
			conds = append(conds, "r.id>?")
			args = append(args, q.AfterRunID)
		}
		if len(conds) > 0 {
			where = "WHERE " + strings.Join(conds, " AND ")
		}
	}
	args = append(args, q.Limit)
	query := `SELECT r.id,r.project_id,r.status,r.version,r.gate_bypassed,r.updated_at_ms,
		a.attempt_no,a.generation,a.phase,a.isolation_state,COALESCE(a.heartbeat_at_ms,0)
		FROM runs r
		LEFT JOIN attempts a ON a.run_id=r.id AND a.attempt_no=(SELECT MAX(attempt_no) FROM attempts WHERE run_id=r.id)
		` + where + ` ORDER BY r.id ASC LIMIT ?`
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return report, fmt.Errorf("storage: ps runs: %w", err)
	}
	type runRow struct {
		ps PSRun
	}
	var list []runRow
	for rows.Next() {
		var r PSRun
		var status string
		var bypass int
		var attemptNo sql.NullInt64
		var gen sql.NullInt64
		var phase, isolation sql.NullString
		var hb sql.NullInt64
		if err := rows.Scan(&r.RunID, &r.ProjectID, &status, &r.Version, &bypass, &r.UpdatedAtMS, &attemptNo, &gen, &phase, &isolation, &hb); err != nil {
			rows.Close()
			return report, err
		}
		r.Status, r.GateBypassed = status, bypass != 0
		if attemptNo.Valid {
			r.Attempt = &PSAttempt{AttemptNo: int(attemptNo.Int64), Generation: int(gen.Int64), Phase: phase.String, IsolationState: isolation.String, HeartbeatAtMS: hb.Int64}
		}
		list = append(list, runRow{ps: r})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return report, err
	}
	rows.Close()

	for _, row := range list {
		r := row.ps
		r.OpenInterruptCount, err = d.countWhere(ctx, "interrupts", "run_id=? AND status='open'", []any{r.RunID})
		if err != nil {
			return report, err
		}
		r.PendingOutboxCount, err = d.countWhere(ctx, "outbox_operations", "run_id=? AND state IN ('pending','executing','retryable')", []any{r.RunID})
		if err != nil {
			return report, err
		}
		report.Runs = append(report.Runs, r)
	}
	if q.RunID == "" && len(report.Runs) == q.Limit {
		report.NextAfterRunID = report.Runs[len(report.Runs)-1].RunID
	}

	// Today's remaining attention per severity = ceiling − consumed. The
	// ceiling is the live configured quota when supplied, else the limit
	// persisted on the latest bucket; consumed always comes from persisted
	// budget_counters so a restart never loses the count.
	report.AttentionRemaining = d.attentionRemaining(ctx, q.ConfiguredQuota)
	return report, nil
}

// attentionRemaining returns today's remaining attention per severity. It
// merges the live configured ceilings with persisted consumption: a severity
// that has never been bucketed reports its configured ceiling as fully
// remaining, while a consumed bucket decrements from the same ceiling.
func (d *DB) attentionRemaining(ctx context.Context, configured map[string]int) map[string]int {
	buckets, _ := d.attentionConsumed(ctx)
	out := map[string]int{}
	for _, sev := range []string{"low", "normal", "high"} {
		limit, ok := configured[sev]
		if !ok {
			limit = 0
		}
		consumed := 0
		if b, ok := buckets[sev]; ok {
			// A persisted bucket carries its own frozen limit; prefer the live
			// configured ceiling when supplied so a same-day quota raise shows.
			if limit == 0 {
				limit = b.limit
			}
			consumed = b.consumed
		}
		remaining := limit - consumed
		if remaining < 0 {
			remaining = 0
		}
		out[sev] = remaining
	}
	return out
}

type attentionBucket struct{ consumed, limit int }

// attentionConsumed reads the latest daily attention bucket per severity from
// persisted budget_counters — the current/today bucket as the daemon wrote it.
func (d *DB) attentionConsumed(ctx context.Context) (map[string]attentionBucket, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT scope_id, consumed_value, limit_value FROM budget_counters WHERE kind='attention' AND scope='severity' AND (scope_id, bucket_start_ms) IN (SELECT scope_id, MAX(bucket_start_ms) FROM budget_counters WHERE kind='attention' AND scope='severity' GROUP BY scope_id)`)
	if err != nil {
		return nil, fmt.Errorf("storage: attention consumed: %w", err)
	}
	defer rows.Close()
	out := map[string]attentionBucket{}
	for rows.Next() {
		var sev string
		var consumed, limit int
		if err := rows.Scan(&sev, &consumed, &limit); err != nil {
			return nil, err
		}
		out[sev] = attentionBucket{consumed: consumed, limit: limit}
	}
	return out, rows.Err()
}

// MaxAttemptNo returns the highest attempt number for a Run, or 0 if none. It
// resolves the attempt scope for ops.logs when the caller omits attempt_no.
func (d *DB) MaxAttemptNo(ctx context.Context, runID string) (int, error) {
	var n sql.NullInt64
	if err := d.db.QueryRowContext(ctx, `SELECT MAX(attempt_no) FROM attempts WHERE run_id=?`, runID).Scan(&n); err != nil {
		return 0, fmt.Errorf("storage: max attempt no: %w", err)
	}
	if !n.Valid {
		return 0, nil
	}
	return int(n.Int64), nil
}

func (d *DB) countWhere(ctx context.Context, table, clause string, args []any) (int, error) {
	var n int
	if err := d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s`, table, clause), args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("storage: count %s: %w", table, err)
	}
	return n, nil
}

// TimelineQuery scopes an event-timeline read.
type TimelineQuery struct {
	RunID     string
	ProjectID string
	Type      string // event type filter; empty means any
	AfterSeq  int64  // keyset pagination cursor by seq
	Limit     int
}

// TimelineReport is the ops.timeline result.
type TimelineReport struct {
	Events  []Event `json:"events"`
	NextSeq int64   `json:"next_seq"`
	HasMore bool    `json:"has_more"`
}

// RunTimeline returns a bounded, keyset-paginated slice of the append-only
// event stream (storage.md §7.1). It never reconstructs events from memory.
func (d *DB) RunTimeline(ctx context.Context, q TimelineQuery) (TimelineReport, error) {
	if q.Limit < 1 {
		q.Limit = 100
	}
	if q.Limit > 1000 {
		q.Limit = 1000
	}
	if q.AfterSeq < 0 {
		q.AfterSeq = 0
	}
	report := TimelineReport{Events: []Event{}}
	conds := []string{"seq>?"}
	args := []any{q.AfterSeq}
	if q.RunID != "" {
		conds = append(conds, "run_id=?")
		args = append(args, q.RunID)
	}
	if q.ProjectID != "" {
		conds = append(conds, "project_id=?")
		args = append(args, q.ProjectID)
	}
	if q.Type != "" {
		conds = append(conds, "type=?")
		args = append(args, q.Type)
	}
	args = append(args, q.Limit)
	rows, err := d.db.QueryContext(ctx, `SELECT seq, id, COALESCE(run_id,''), attempt_no, COALESCE(project_id,''),
		type, source, actor, payload_json, occurred_at_ms, recorded_at_ms
		FROM events WHERE `+strings.Join(conds, " AND ")+` ORDER BY seq ASC LIMIT ?`, args...)
	if err != nil {
		return report, fmt.Errorf("storage: timeline: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e Event
		var actor sql.NullString
		var attemptNo sql.NullInt64
		if err := rows.Scan(&e.Seq, &e.ID, &e.RunID, &attemptNo, &e.ProjectID, &e.Type, &e.Source,
			&actor, &e.PayloadJSON, &e.OccurredAtMS, &e.RecordedAtMS); err != nil {
			return report, err
		}
		if attemptNo.Valid {
			v := int(attemptNo.Int64)
			e.AttemptNo = &v
		}
		e.Actor = actor.String
		report.Events = append(report.Events, e)
	}
	if err := rows.Err(); err != nil {
		return report, err
	}
	if len(report.Events) > 0 {
		last := report.Events[len(report.Events)-1].Seq
		report.NextSeq = last
		var more int
		if err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE seq>?`, last).Scan(&more); err == nil {
			report.HasMore = more > 0
		}
	}
	return report, nil
}
