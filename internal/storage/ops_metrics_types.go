package storage

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
