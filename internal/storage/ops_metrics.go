package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/xsift/sift/internal/config"
	"sort"
)

func (d *DB) Metrics(ctx context.Context, q MetricsQuery) (MetricsReport, error) {
	scope := "global"
	runWhere, runArgs := "", []any{}
	if q.ProjectID != "" {
		scope = q.ProjectID
		runWhere = "WHERE project_id=?"
		runArgs = []any{q.ProjectID}
	}
	report := MetricsReport{Scope: scope, AttentionQuotaConsumption: []QuotaConsumption{}, ForgeAPIQuotaConsumption: []ForgeAPIQuotaConsumption{}}

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
	report.ForgeAPIQuotaConsumption, err = d.forgeAPIQuotaConsumption(ctx, q)
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

// forgeAPIQuotaConsumption projects the current hourly budget for every
// selected project through the same status read that Intake uses. No counter is
// created for an uncharged project; its configured limit is reported as unused.
func (d *DB) forgeAPIQuotaConsumption(ctx context.Context, q MetricsQuery) ([]ForgeAPIQuotaConsumption, error) {
	out := []ForgeAPIQuotaConsumption{}
	if q.NowMS <= 0 || q.ForgeAPIHourlyLimit < 1 || q.ForgeAPIWarningRatio <= 0 || q.ForgeAPIWarningRatio >= 1 {
		return out, nil
	}
	query, args := `SELECT id FROM projects`, []any{}
	if q.ProjectID != "" {
		query += ` WHERE id=?`
		args = append(args, q.ProjectID)
	}
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: forge api metric projects: %w", err)
	}
	projectIDs := []string{}
	for rows.Next() {
		var projectID string
		if err := rows.Scan(&projectID); err != nil {
			rows.Close()
			return nil, err
		}
		projectIDs = append(projectIDs, projectID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for _, projectID := range projectIDs {
		status, err := d.ForgeAPIBudgetStatus(ctx, projectID, q.NowMS, q.ForgeAPIHourlyLimit, q.ForgeAPIWarningRatio)
		if err != nil {
			return nil, fmt.Errorf("storage: forge api metric status: %w", err)
		}
		out = append(out, ForgeAPIQuotaConsumption{ProjectID: projectID, Consumed: status.Consumed, Limit: status.Limit, Unit: "calls"})
	}
	return out, nil
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
