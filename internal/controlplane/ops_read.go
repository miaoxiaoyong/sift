package controlplane

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/miaoxiaoyong/sift/internal/storage"
)

// This file implements the read-side operator methods (control-plane.md §6):
// ops.ps, ops.logs, ops.metrics and ops.timeline. They are read-only and never
// expose tokens, hashes, nonces or raw config snapshots. The WBS §5.7 metric
// and timeline surfaces are bootstrap additions: deterministic, fail-closed on
// missing V0 data, and explicitly honest in their coverage notes.

// handleOpsPs projects Run/attempt rows, today's remaining attention quota and
// the durable Channel delivery diagnostics (control-plane.md §6.2).
func (s *Server) handleOpsPs(req Request) Response {
	if !onlyKeys(req.Params, "run_id", "project_id", "status", "limit", "after_run_id") {
		return failure(req.RequestID, "invalid_request", "invalid params", false)
	}
	result := map[string]any{"runs": []any{}, "next_after_run_id": nil, "attention_remaining": map[string]int{"low": 0, "normal": 0, "high": 0}, "channel_deliveries": []any{}}
	if s.db == nil {
		return success(req.RequestID, result)
	}
	q := storage.PSQuery{
		RunID:           optString(req.Params["run_id"]),
		ProjectID:       optString(req.Params["project_id"]),
		Status:          optString(req.Params["status"]),
		Limit:           optInt(req.Params["limit"], 100),
		AfterRunID:      optString(req.Params["after_run_id"]),
		ConfiguredQuota: s.configuredQuota,
	}
	report, err := s.db.RunPS(context.Background(), q)
	if err != nil {
		return failure(req.RequestID, "storage", "ps projection unavailable", true)
	}
	result["runs"] = report.Runs
	result["next_after_run_id"] = nullableString(report.NextAfterRunID)
	result["attention_remaining"] = report.AttentionRemaining
	projections, err := s.db.ChannelDiagnostics(context.Background())
	if err != nil {
		return failure(req.RequestID, "storage", "channel projections unavailable", true)
	}
	result["channel_deliveries"] = projections
	return success(req.RequestID, result)
}

// handleOpsLogs performs a bounded read of a Run/attempt's raw agent.log
// (control-plane.md §6.2). offset is the logical byte offset; a cleaned-away
// offset returns not_found rather than silently restarting from zero.
func (s *Server) handleOpsLogs(req Request) Response {
	if !onlyKeys(req.Params, "run_id", "attempt_no", "offset", "limit") {
		return failure(req.RequestID, "invalid_request", "invalid params", false)
	}
	runID, ok := req.Params["run_id"].(string)
	if !ok || runID == "" {
		return failure(req.RequestID, "invalid_request", "invalid params", false)
	}
	offset := optInt64(req.Params["offset"], 0)
	if offset < 0 {
		return failure(req.RequestID, "invalid_request", "invalid params", false)
	}
	limit := optInt64(req.Params["limit"], 262144)
	if limit < 1 || limit > 262144 {
		return failure(req.RequestID, "invalid_request", "invalid params", false)
	}
	attemptNo := 0
	if v, ok := req.Params["attempt_no"]; ok {
		if f, ok := v.(float64); ok {
			attemptNo = int(f)
		}
	}
	if attemptNo == 0 {
		if s.db == nil {
			return failure(req.RequestID, "not_found", "run not found", false)
		}
		n, err := s.db.MaxAttemptNo(context.Background(), runID)
		if err != nil || n == 0 {
			return failure(req.RequestID, "not_found", "run not found", false)
		}
		attemptNo = n
	}
	logPath := filepath.Join(s.Home.Path, "runs", runID, "attempts", strconv.Itoa(attemptNo), "agent.log")
	info, err := os.Stat(logPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return failure(req.RequestID, "not_found", "log not found", false)
		}
		return failure(req.RequestID, "storage", "log unavailable", true)
	}
	if offset > 0 && offset > info.Size() {
		// The logical offset is past EOF — a rotated-away prefix. Fail closed
		// rather than returning current-file bytes for a stale offset.
		return failure(req.RequestID, "not_found", "log offset no longer reachable", false)
	}
	f, err := os.Open(logPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return failure(req.RequestID, "not_found", "log not found", false)
		}
		return failure(req.RequestID, "storage", "log unavailable", true)
	}
	defer f.Close()
	if offset > 0 {
		if _, err := io.CopyN(io.Discard, f, offset); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return failure(req.RequestID, "not_found", "log offset no longer reachable", false)
			}
			return failure(req.RequestID, "storage", "log read failed", true)
		}
	}
	buf := make([]byte, limit)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return failure(req.RequestID, "storage", "log read failed", true)
	}
	nextOffset := offset + int64(n)
	return success(req.RequestID, map[string]any{
		"attempt_no":  attemptNo,
		"offset":      offset,
		"next_offset": nextOffset,
		"eof":         nextOffset >= info.Size(),
		"data_base64": base64.StdEncoding.EncodeToString(buf[:n]),
	})
}

// handleOpsMetrics derives the nine PRD §10.2 metric series plus the
// trigger→started latency distribution. It is fail-closed: missing V0 data is
// reported with an explicit coverage note, never an invented number.
func (s *Server) handleOpsMetrics(req Request) Response {
	if !onlyKeys(req.Params, "project_id") {
		return failure(req.RequestID, "invalid_request", "invalid params", false)
	}
	if s.db == nil {
		return failure(req.RequestID, "storage", "metrics unavailable", true)
	}
	q := storage.MetricsQuery{
		ProjectID:            optString(req.Params["project_id"]),
		NowMS:                time.Now().UnixMilli(),
		ForgeAPIHourlyLimit:  s.forgeAPIHourlyLimit,
		ForgeAPIWarningRatio: s.forgeAPIWarningRatio,
	}
	report, err := s.db.Metrics(context.Background(), q)
	if err != nil {
		return failure(req.RequestID, "storage", "metrics unavailable", true)
	}
	latency, err := s.db.TriggerStartedLatency(context.Background(), q)
	if err != nil {
		return failure(req.RequestID, "storage", "latency unavailable", true)
	}
	return success(req.RequestID, map[string]any{"metrics": report, "trigger_started_latency": latency})
}

// handleOpsTimeline returns a bounded, keyset-paginated slice of the persisted
// event stream (storage.md §7.1), globally ordered by occurred_at_ms
// descending (seq tie-breaker). It never reconstructs events from memory.
func (s *Server) handleOpsTimeline(req Request) Response {
	// Accept both the dual-cursor param set and the legacy set without
	// after_occurred_at_ms (pre-B2 ops.timeline), so old callers keep paging;
	// unknown keys are still rejected via subsetKeys.
	if !subsetKeys(req.Params, "run_id", "project_id", "type", "after_occurred_at_ms", "after_seq", "limit") {
		return failure(req.RequestID, "invalid_request", "invalid params", false)
	}
	if s.db == nil {
		return failure(req.RequestID, "storage", "timeline unavailable", true)
	}
	q := storage.TimelineQuery{
		RunID:             optString(req.Params["run_id"]),
		ProjectID:         optString(req.Params["project_id"]),
		Type:              optString(req.Params["type"]),
		AfterSeq:          optInt64(req.Params["after_seq"], 0),
		AfterOccurredAtMS: optInt64(req.Params["after_occurred_at_ms"], 0),
		Limit:             optInt(req.Params["limit"], 100),
	}
	report, err := s.db.RunTimeline(context.Background(), q)
	if err != nil {
		return failure(req.RequestID, "storage", "timeline unavailable", true)
	}
	return success(req.RequestID, report)
}

// subsetKeys reports whether every param key is one of the allowed keys.
// Unlike onlyKeys it permits absent optional keys, which ops.timeline needs to
// keep the legacy param set (no after_occurred_at_ms) valid; unknown keys are
// still rejected.
func subsetKeys(m map[string]any, keys ...string) bool {
	allowed := make(map[string]bool, len(keys))
	for _, k := range keys {
		allowed[k] = true
	}
	for k := range m {
		if !allowed[k] {
			return false
		}
	}
	return true
}

// optString returns a string param or "" when nil/absent.
func optString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// optInt returns an int param coerced from JSON float64, or def.
func optInt(v any, def int) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return def
}

// optInt64 returns an int64 param coerced from JSON float64, or def.
func optInt64(v any, def int64) int64 {
	if f, ok := v.(float64); ok {
		return int64(f)
	}
	return def
}

// nullableString returns the string or nil for JSON null emission.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
