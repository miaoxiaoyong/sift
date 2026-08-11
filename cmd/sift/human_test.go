package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miaoxiaoyong/sift/internal/controlplane"
)

// TestHumanPSEmptyAndRows pins the ps renderer: the empty-friendly hint, the
// table header, and the Chinese status/phase labels (ux-2).
func TestHumanPSEmptyAndRows(t *testing.T) {
	var out bytes.Buffer
	renderPS(&out, map[string]any{
		"runs":                []any{},
		"attention_remaining": map[string]any{"low": 0, "normal": 0, "high": 0},
	})
	if !strings.Contains(out.String(), "暂无运行") {
		t.Fatalf("empty ps = %q, want friendly hint", out.String())
	}

	out.Reset()
	renderPS(&out, map[string]any{
		"runs": []any{map[string]any{
			"run_id": "run-1", "project_id": "proj-1", "status": "running", "version": 3,
			"attempt":              map[string]any{"attempt_no": 1, "agent_id": "claude", "phase": "running", "isolation_state": "none", "heartbeat_at_ms": 0},
			"open_interrupt_count": 0, "pending_outbox_count": 1, "gate_bypassed": false,
		}},
		"attention_remaining": map[string]any{"low": 3, "normal": 5, "high": 2},
	})
	got := out.String()
	for _, want := range []string{"运行列表", "运行 ID", "项目", "Agent", "run-1", "proj-1", "claude", "✓ 运行中", "运行中", "今日注意力剩余", "低 3", "普通 5", "高 2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("ps row output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "protocol_major") {
		t.Fatalf("ps renderer leaked the envelope:\n%s", got)
	}
}

// TestHumanTimelinePinsEventLabels renders events with the exact wire keys
// (outer lowercase, event fields Go-named) and asserts newest-first order,
// the date section, and the Chinese type label.
func TestHumanTimelinePinsEventLabels(t *testing.T) {
	var out bytes.Buffer
	renderTimeline(&out, map[string]any{
		"events": []any{
			map[string]any{"Seq": 2, "RunID": "run-1", "ProjectID": "proj-1", "Type": "intake.trigger_observed", "Source": "forge", "Actor": "", "AttemptNo": nil, "OccurredAtMS": 1_700_000_000_000},
			map[string]any{"Seq": 1, "RunID": "run-1", "ProjectID": "proj-1", "Type": "report.progress", "Source": "agent", "Actor": "claude", "AttemptNo": float64(1), "OccurredAtMS": 1_700_000_000_500},
		},
		"has_more": true,
		"next_seq": 2,
	})
	got := out.String()
	for _, want := range []string{"事件时间线", "最新在前", "──", "进度报告", "触发已观测", "run-1", "尝试 1", "Agent", "Forge", "--after-seq 2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("timeline output lacks %q:\n%s", want, got)
		}
	}
	// Newest first: report.progress has the lower seq but later occurrence time.
	if strings.Index(got, "进度报告") > strings.Index(got, "触发已观测") {
		t.Fatalf("timeline is not newest-first:\n%s", got)
	}
}

// TestHumanLogsPinsAttemptAndTruncation decodes the base64 payload, keeps the
// attempt header, and adds an honest truncation hint at eof=false.
func TestHumanLogsPinsAttemptAndTruncation(t *testing.T) {
	var out bytes.Buffer
	renderLogs(&out, "run-1", map[string]any{
		"attempt_no":  2,
		"offset":      0,
		"next_offset": 13,
		"eof":         false,
		"data_base64": base64.StdEncoding.EncodeToString([]byte("line one\nline two\n\x00x")),
	})
	got := out.String()
	for _, want := range []string{"运行 run-1 日志", "第 2 次尝试", "line one", "line two", "\\x00", "已显示", "后续内容未显示"} {
		if !strings.Contains(got, want) {
			t.Fatalf("logs output lacks %q:\n%s", want, got)
		}
	}
}

// TestHumanMetricsPinsSectionsAndUnits asserts the attention / token / latency
// sections and the honest coverage notes.
func TestHumanMetricsPinsSectionsAndUnits(t *testing.T) {
	var out bytes.Buffer
	renderMetrics(&out, map[string]any{
		"metrics": map[string]any{
			"scope": "global",
			"weighted_attention_per_merged_change": map[string]any{
				"weighted_minutes": 25, "delivered_metric_identities": 2, "merged_changes": 2, "per_merged_change": 12.5, "coverage": "north star",
			},
			"false_release_rate":    map[string]any{"numerator": 0, "denominator": 0, "rate": 0, "coverage": "fails closed"},
			"gate_bypass_rate":      map[string]any{"numerator": 0, "denominator": 0, "rate": 0, "coverage": "c"},
			"gate_miss_rate":        map[string]any{"numerator": 0, "denominator": 0, "rate": 0, "coverage": ""},
			"gate_false_block_rate": map[string]any{"numerator": 0, "denominator": 0, "rate": 0, "coverage": ""},
			"hitl_rate":             map[string]any{"numerator": 1, "denominator": 2, "rate": 0.5, "coverage": ""},
			"dispatch_accuracy":     map[string]any{"numerator": 1, "denominator": 1, "rate": 1, "coverage": "structural"},
			"attention_quota_consumption": []any{
				map[string]any{"severity": "low", "consumed": 3, "limit": 5, "rate": 0.6},
				map[string]any{"severity": "normal", "consumed": 5, "limit": 5, "rate": 1},
				map[string]any{"severity": "high", "consumed": 2, "limit": 5, "rate": 0.4},
			},
			"forge_api_quota_consumption": []any{
				map[string]any{"project_id": "proj-1", "consumed": 7, "limit": 10, "unit": "calls"},
			},
			"llm_cost_per_merged_change": map[string]any{
				"input_tokens": 1000, "output_tokens": 500, "merged_changes": 2,
				"per_merged_change_total_tokens": 750, "per_merged_change_input_tokens": 500, "per_merged_change_output_tokens": 250, "coverage": "tokens only",
			},
		},
		"trigger_started_latency": map[string]any{
			"count": 4, "min_ms": 1000, "p50_ms": 1500, "p90_ms": 2300, "max_ms": 3000,
			"samples": []any{}, "coverage": "real P50<60s is the M7 acceptance",
		},
	})
	got := out.String()
	for _, want := range []string{
		"指标（全局）", "注意力配额", "低：3 / 5", "普通：5 / 5", "高：2 / 5",
		"每合并变更注意力：12.5 分钟", "误放行率", "人工介入率：50.0%", "分派准确率：100.0%",
		"Forge API 用量", "项目 proj-1：7 / 10 calls", "LLM 用量", "输入 500 / 输出 250 tokens",
		"触发→启动延迟", "4 个样本", "P50 1.5s", "P90 2.3s",
		"覆盖说明：fails closed", "覆盖说明：north star", "覆盖说明：real P50<60s",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("metrics output lacks %q:\n%s", want, got)
		}
	}
}

// TestHumanWorktreePinsPathAndFailure renders both the success result and the
// humanized not_found path used by renderError.
func TestHumanWorktreePinsPathAndFailure(t *testing.T) {
	var out bytes.Buffer
	renderWorktree(&out, "run-1", map[string]any{
		"run_id": "run-1", "attempt_no": 1, "path": "/wt/run-1", "exists": true,
		"isolation_state": "none", "read_only_recommended": false,
	})
	if !strings.Contains(out.String(), "路径：/wt/run-1") || !strings.Contains(out.String(), "隔离状态：none") {
		t.Fatalf("worktree success = %q", out.String())
	}

	out.Reset()
	renderError(&out, controlplane.Response{OK: false, Error: &controlplane.Error{Code: "not_found", Message: "run not found", Details: map[string]any{}}}, failureContext("worktree", []string{"run-9"}))
	if !strings.Contains(out.String(), "✗ 未找到") || !strings.Contains(out.String(), "run-9") || !strings.Contains(out.String(), "工作树") {
		t.Fatalf("worktree not_found = %q", out.String())
	}
}

// TestHumanKillRetryPinsAcceptedAndStale renders the accepted termination and
// the stale failure with an actionable next step.
func TestHumanKillRetryPinsAcceptedAndStale(t *testing.T) {
	var out bytes.Buffer
	renderKillRetry(&out, "kill", "run-1", map[string]any{"accepted": true, "state": "terminating"})
	got := out.String()
	if !strings.Contains(got, "✓ 已请求停止运行 run-1") || !strings.Contains(got, "terminating") || !strings.Contains(got, "sift ps") {
		t.Fatalf("kill accepted = %q", got)
	}
	out.Reset()
	renderKillRetry(&out, "retry", "run-1", map[string]any{"disposition": "accepted", "probe_id": "probe-7", "message": "waiting for executor absence evidence"})
	if !strings.Contains(out.String(), "✓ 已请求重试运行 run-1") || !strings.Contains(out.String(), "probe-7") {
		t.Fatalf("retry accepted = %q", out.String())
	}

	out.Reset()
	renderError(&out, controlplane.Response{OK: false, Error: &controlplane.Error{Code: "stale", Message: "run or attempt changed", Details: map[string]any{}}}, failureContext("kill", []string{"run-1"}))
	if !strings.Contains(out.String(), "✗ 运行或尝试已变化（stale）") || !strings.Contains(out.String(), "sift ps") {
		t.Fatalf("kill stale = %q", out.String())
	}
}

// TestHumanReportPinsAccepted pins the humanized submission result.
func TestHumanReportPinsAccepted(t *testing.T) {
	var out bytes.Buffer
	renderReport(&out, "progress", map[string]any{"disposition": "accepted", "receipt_id": "receipt-1", "event_id": "event-2"})
	got := out.String()
	for _, want := range []string{"✓ 报告已提交", "进度", "receipt-1", "event-2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("report output lacks %q:\n%s", want, got)
		}
	}
}

// serveFakeOperatorMulti serves any number of operator requests on one home
// with a fixed response, for tests that issue more than one CLI run.
func serveFakeOperatorMulti(t *testing.T, home string, response func(controlplane.Request) map[string]any) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "operator.token"), []byte(strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(home, "siftd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				var header [4]byte
				if _, err := io.ReadFull(conn, header[:]); err != nil {
					return
				}
				body := make([]byte, binary.BigEndian.Uint32(header[:]))
				if _, err := io.ReadFull(conn, body); err != nil {
					return
				}
				var request controlplane.Request
				if err := json.Unmarshal(body, &request); err != nil {
					return
				}
				out, err := json.Marshal(response(request))
				if err != nil {
					return
				}
				binary.BigEndian.PutUint32(header[:], uint32(len(out)))
				if _, err := conn.Write(header[:]); err == nil {
					_, _ = conn.Write(out)
				}
			}(conn)
		}
	}()
}

// TestHumanLogsJSONByteIdentity pins the --json protocol face: the raw
// envelope for a fixed daemon response is byte-identical to printJSON of the
// decoded Response (protocol regression, ux-2/ux-3).
func TestHumanLogsJSONByteIdentity(t *testing.T) {
	home := freshHome(t)
	result := map[string]any{"attempt_no": 1, "offset": 0, "next_offset": 6, "eof": true, "data_base64": "aGVsbG8="}
	serveFakeOperatorMulti(t, home, func(req controlplane.Request) map[string]any {
		return fakeDoctorSuccess(req.RequestID, result)
	})
	var out bytes.Buffer
	if code := run([]string{"sift", "logs", "run-1", "--json"}, &out, io.Discard); code != 0 {
		t.Fatalf("logs --json exit = %d; output:\n%s", code, out.String())
	}
	var response map[string]any
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatalf("logs --json is not JSON: %v; output=%q", err, out.String())
	}
	if ok, _ := response["ok"].(bool); !ok {
		t.Fatalf("logs --json ok = %v; output=%q", response["ok"], out.String())
	}
	got := response["result"].(map[string]any)
	if got["data_base64"] != "aGVsbG8=" || got["attempt_no"] != float64(1) {
		t.Fatalf("logs --json result = %v, want the exact wire values", got)
	}
	// Byte-identity: the printed text must equal printJSON of the decoded
	// envelope (the only JSON writer the CLI ever used for this command). The
	// request_id is client-random per control-plane.md §3.2, so it is taken
	// from the observed response; every other byte is pinned.
	requestID, _ := response["request_id"].(string)
	expected := controlplane.Response{
		ProtocolMajor: controlplane.ProtocolMajor, ProtocolMinor: controlplane.ProtocolMinor,
		ServerVersion: controlplane.Version, RequestID: requestID, OK: true, Result: result,
	}
	var want bytes.Buffer
	if err := printJSON(&want, expected); err != nil {
		t.Fatal(err)
	}
	if out.String() != want.String() {
		t.Fatalf("logs --json drifted from the protocol face:\n--- got ---\n%s\n--- want ---\n%s", out.String(), want.String())
	}

	// The humanized default replaces the envelope.
	out.Reset()
	if code := run([]string{"sift", "logs", "run-1"}, &out, io.Discard); code != 0 {
		t.Fatalf("logs human exit = %d; output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "运行 run-1 日志") || !strings.Contains(out.String(), "hello") {
		t.Fatalf("logs human = %q", out.String())
	}
}

// TestSIFTJSONEnvironmentSelectsEnvelope pins the SIFT_JSON=1 priority rule:
// the environment variable alone must switch every command to the raw dump.
func TestSIFTJSONEnvironmentSelectsEnvelope(t *testing.T) {
	home := freshHome(t)
	t.Setenv("SIFT_JSON", "1")
	serveFakeDoctorResponse(t, home, func(req controlplane.Request) map[string]any {
		return fakeDoctorSuccess(req.RequestID, map[string]any{"runs": []any{}, "attention_remaining": map[string]int{"low": 0, "normal": 0, "high": 0}})
	})
	var out bytes.Buffer
	if code := run([]string{"sift", "ps"}, &out, io.Discard); code != 0 {
		t.Fatalf("ps SIFT_JSON exit = %d; output:\n%s", code, out.String())
	}
	var response map[string]any
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatalf("SIFT_JSON output is not JSON: %v; output=%q", err, out.String())
	}
	if response["protocol_major"] != float64(controlplane.ProtocolMajor) {
		t.Fatalf("SIFT_JSON protocol_major = %v; output=%q", response["protocol_major"], out.String())
	}
}

// TestSplitJSONFlagPinsPriority asserts --json is stripped from args anywhere
// and OR'd with the environment, and never forwarded to the daemon params.
func TestSplitJSONFlagPinsPriority(t *testing.T) {
	rest, jsonOutput := splitJSONFlag([]string{"run-1", "--json"})
	if !jsonOutput || len(rest) != 1 || rest[0] != "run-1" {
		t.Fatalf("splitJSONFlag = %v %v, want [run-1] true", rest, jsonOutput)
	}
	rest, jsonOutput = splitJSONFlag([]string{"--json", "run-1", "--json"})
	if !jsonOutput || len(rest) != 1 {
		t.Fatalf("splitJSONFlag repeated = %v %v", rest, jsonOutput)
	}
}

// TestHumanLogsNotFoundFailure pins the humanized not_found for a missing log.
func TestHumanLogsNotFoundFailure(t *testing.T) {
	home := freshHome(t)
	serveFakeDoctorResponse(t, home, func(req controlplane.Request) map[string]any {
		return fakeDoctorError(req.RequestID, controlplane.ProtocolMajor, controlplane.ProtocolMinor, controlplane.Version, "not_found", "log not found")
	})
	var out bytes.Buffer
	if code := run([]string{"sift", "logs", "run-9"}, &out, io.Discard); code != 1 {
		t.Fatalf("logs not_found exit = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "✗ 未找到") || !strings.Contains(out.String(), "run-9") || !strings.Contains(out.String(), "日志") {
		t.Fatalf("logs not_found = %q", out.String())
	}
}

// TestHumanMetricsEmptyQuotaAndLatency pins the honest empty states: no
// consumption buckets and no latency samples never invent numbers.
func TestHumanMetricsEmptyQuotaAndLatency(t *testing.T) {
	var out bytes.Buffer
	renderMetrics(&out, map[string]any{
		"metrics": map[string]any{
			"scope":                                "global",
			"attention_quota_consumption":          []any{},
			"weighted_attention_per_merged_change": map[string]any{"weighted_minutes": 0, "merged_changes": 0, "per_merged_change": 0, "coverage": "x"},
			"false_release_rate":                   map[string]any{"numerator": 0, "denominator": 0, "rate": 0, "coverage": "y"},
			"gate_bypass_rate":                     map[string]any{"numerator": 0, "denominator": 0, "rate": 0, "coverage": ""},
			"gate_miss_rate":                       map[string]any{"numerator": 0, "denominator": 0, "rate": 0, "coverage": ""},
			"gate_false_block_rate":                map[string]any{"numerator": 0, "denominator": 0, "rate": 0, "coverage": ""},
			"hitl_rate":                            map[string]any{"numerator": 0, "denominator": 0, "rate": 0, "coverage": ""},
			"dispatch_accuracy":                    map[string]any{"numerator": 0, "denominator": 0, "rate": 0, "coverage": ""},
			"llm_cost_per_merged_change":           map[string]any{"input_tokens": 0, "output_tokens": 0, "merged_changes": 0, "per_merged_change_input_tokens": 0, "per_merged_change_output_tokens": 0, "coverage": ""},
		},
		"trigger_started_latency": map[string]any{"count": 0, "min_ms": 0, "p50_ms": 0, "p90_ms": 0, "max_ms": 0, "samples": []any{}, "coverage": ""},
	})
	got := out.String()
	for _, want := range []string{"暂无已记录的消耗", "暂无样本"} {
		if !strings.Contains(got, want) {
			t.Fatalf("metrics empty output lacks %q:\n%s", want, got)
		}
	}
}
