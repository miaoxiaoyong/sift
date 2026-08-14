package render

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/xsift/sift/internal/controlplane"
)

func renormalize(value any, out any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

// failureContext names the missing subject for the not_found humanization.
func FailureContext(command string, args []string) string {
	runID := ""
	if len(args) > 0 {
		runID = args[0]
	}
	switch command {
	case "logs":
		return fmt.Sprintf("运行 %s 的日志不存在（可能已被清理）", runID)
	case "worktree":
		return fmt.Sprintf("运行 %s 没有可用的工作树", runID)
	case "kill", "retry", "rm":
		return fmt.Sprintf("运行 %s 不存在", runID)
	default:
		return "请求的数据不存在"
	}
}

// renderError turns a failed RPC into an actionable Chinese line. The exit
// status is unchanged; only the presentation differs from the raw envelope.
func Error(w io.Writer, response controlplane.Response, context string) {
	code, message := "", ""
	if response.Error != nil {
		code, message = response.Error.Code, response.Error.Message
	}
	switch code {
	case "not_found":
		fmt.Fprintf(w, "✗ 未找到：%s\n", context)
	case "stale":
		fmt.Fprintln(w, "✗ 运行或尝试已变化（stale）：运行已变更，请先运行 sift ps 查看最新状态，再重试。")
	case "unauthorized":
		fmt.Fprintln(w, "✗ 凭据被拒绝（unauthorized）：请检查守护进程是否正常运行（sift doctor）。")
	case "unavailable":
		fmt.Fprintf(w, "✗ 暂不可用（unavailable）：%s\n", message)
	case "storage":
		fmt.Fprintf(w, "✗ 数据读取失败（storage）：%s\n", message)
	case "conflict":
		fmt.Fprintf(w, "✗ 操作被拒绝（conflict）：%s\n", message)
	case "invalid_request":
		fmt.Fprintf(w, "✗ 请求无效（invalid_request）：%s\n", message)
	default:
		if message == "" {
			message = code
		}
		fmt.Fprintf(w, "✗ 操作失败（%s）：%s\n", code, message)
	}
}

// runStatusLabel maps the Run status enum to a Chinese label with an icon.
func runStatusLabel(s string) string {
	switch s {
	case "queued":
		return "ℹ 排队"
	case "running":
		return "✓ 运行中"
	case "waiting_human":
		return "⚠ 等待人工"
	case "done":
		return "✓ 完成"
	case "failed":
		return "✗ 失败"
	}
	return s
}

// phaseLabel maps the attempt phase enum to Chinese.
func phaseLabel(p string) string {
	switch p {
	case "pending":
		return "等待"
	case "starting":
		return "启动中"
	case "spawning":
		return "派生中"
	case "running":
		return "运行中"
	case "finished":
		return "已完成"
	case "orphaned":
		return "已失联"
	}
	return p
}

// severityLabel maps the attention severity enum to Chinese.
func severityLabel(s string) string {
	switch s {
	case "low":
		return "低"
	case "normal":
		return "普通"
	case "high":
		return "高"
	}
	return s
}

// sourceLabel maps the event source enum to Chinese.
func sourceLabel(s string) string {
	switch s {
	case "system":
		return "系统"
	case "forge":
		return "Forge"
	case "operator":
		return "运维"
	case "agent":
		return "Agent"
	case "recovery":
		return "恢复"
	}
	return s
}

// eventTypeLabels covers the append-only event types emitted by the storage
// layer (storage.md §7.1). Unknown or future types fall back to the raw name.
var eventTypeLabels = map[string]string{
	"intake.trigger_observed":            "触发已观测",
	"intake.issue_observed":              "Issue 已观测",
	"intake.decision":                    "接纳决策",
	"intake.reply_accepted":              "回复已接纳",
	"intake.reply_ignored":               "回复已忽略",
	"run.assigned":                       "运行已分配",
	"run.transitioned":                   "运行状态迁移",
	"run.transition_rejected":            "状态迁移被拒",
	"attempt.completed":                  "尝试完成",
	"attempt.acquired":                   "尝试接管",
	"attempt.spawn_permitted":            "尝试派生已放行",
	"attempt.race_resolved":              "尝试竞争已解决",
	"report.progress":                    "进度报告",
	"report.goal":                        "目标报告",
	"report.blocker":                     "阻塞报告",
	"report.completed":                   "完成报告",
	"interrupt.emitted":                  "中断已发出",
	"interrupt.dispatched":               "中断已分派",
	"interrupt.escalated":                "中断已升级",
	"interrupt.expired":                  "中断已过期",
	"interrupt.expired_auto_reject":      "中断过期自动拒绝",
	"forge_change_merged":                "Forge 变更已合并",
	"change.merged_observed":             "合并已观测",
	"command.event":                      "命令事件",
	"command.ignored":                    "命令已忽略",
	"gate.reevaluation.conflict":         "门禁复审冲突",
	"gate.reevaluation.failed":           "门禁复审失败",
	"security.report_quota_exhausted":    "报告配额耗尽",
	"security.report_interrupt_rejected": "报告中断被拒",
	"security.handoff_rejected":          "交接被拒",
	"termination.absence_confirmed":      "终止已确认",
	"backend.session_diagnostic":         "后端会话诊断",
	"project.isolated":                   "项目已隔离",
	"project.capability_checked":         "能力检查",
	"hooks_baseline_missing":             "Hooks 基线缺失",
	"hooks_baseline_activation_missing":  "Hooks 基线激活缺失",
	"hooks_baseline_bootstrapped":        "Hooks 基线已引导",
	"hooks_drift_detected":               "Hooks 漂移已检测",
}

func eventTypeLabel(t string) string {
	if label, ok := eventTypeLabels[t]; ok {
		return label
	}
	if strings.HasPrefix(t, "gate.reevaluation.") {
		return "门禁复审"
	}
	return t
}

// renderPS humanizes the ops.ps result: a run table (run-id / project /
// status / phase / version / interrupt / outbox) and today's remaining
// attention quota per severity. An empty list gets a friendly hint.
func PS(w io.Writer, value any) {
	var result struct {
		Runs []struct {
			RunID     string  `json:"run_id"`
			ProjectID string  `json:"project_id"`
			Status    string  `json:"status"`
			Version   float64 `json:"version"`
			Attempt   *struct {
				AttemptNo float64 `json:"attempt_no"`
				AgentID   string  `json:"agent_id"`
				Phase     string  `json:"phase"`
			} `json:"attempt"`
			OpenInterruptCount float64 `json:"open_interrupt_count"`
			PendingOutboxCount float64 `json:"pending_outbox_count"`
		} `json:"runs"`
		AttentionRemaining map[string]float64 `json:"attention_remaining"`
	}
	if err := renormalize(value, &result); err != nil {
		fmt.Fprintln(w, "✗ 无法读取运行列表")
		return
	}
	fmt.Fprintf(w, "运行列表（共 %d 个）\n", len(result.Runs))
	if len(result.Runs) == 0 {
		fmt.Fprintln(w, "  暂无运行：触发 Issue 后，运行会自动出现在这里。也可以运行 sift doctor --offline 检查环境。")
	} else {
		rows := make([][]string, 0, len(result.Runs))
		for _, r := range result.Runs {
			phase, attempt := "-", "-"
			if r.Attempt != nil {
				phase = phaseLabel(r.Attempt.Phase)
				attempt = fmt.Sprintf("第 %d 次", int(r.Attempt.AttemptNo))
			}
			agent := "-"
			if r.Attempt != nil && r.Attempt.AgentID != "" {
				agent = r.Attempt.AgentID
			}
			rows = append(rows, []string{
				r.RunID, r.ProjectID, agent, runStatusLabel(r.Status), phase, attempt,
				fmt.Sprintf("%d", int(r.Version)),
				fmt.Sprintf("%d", int(r.OpenInterruptCount)),
				fmt.Sprintf("%d", int(r.PendingOutboxCount)),
			})
		}
		fmt.Fprint(w, Table([]string{"运行 ID", "项目", "Agent", "状态", "阶段", "尝试", "版本", "中断", "待发"}, rows))
	}
	if len(result.AttentionRemaining) > 0 {
		fmt.Fprintln(w, "今日注意力剩余：")
		for _, sev := range []string{"low", "normal", "high"} {
			fmt.Fprintf(w, "  %s %d", severityLabel(sev), int(result.AttentionRemaining[sev]))
		}
		fmt.Fprintln(w)
		allZero := result.AttentionRemaining["low"] == 0 && result.AttentionRemaining["normal"] == 0 && result.AttentionRemaining["high"] == 0
		if allZero {
			fmt.Fprintln(w, "  （未配置每日注意力配额）")
		}
	}
}

// renderTimeline humanizes the ops.timeline result: newest-first events,
// sectioned by local date, with Chinese event-type labels.
func Timeline(w io.Writer, value any) {
	var result struct {
		Events []struct {
			Seq          float64  `json:"Seq"`
			RunID        string   `json:"RunID"`
			Type         string   `json:"Type"`
			Source       string   `json:"Source"`
			Actor        string   `json:"Actor"`
			AttemptNo    *float64 `json:"AttemptNo"`
			OccurredAtMS float64  `json:"OccurredAtMS"`
		} `json:"events"`
		HasMore          bool    `json:"has_more"`
		NextSeq          float64 `json:"next_seq"`
		NextOccurredAtMS float64 `json:"next_occurred_at_ms"`
	}
	if err := renormalize(value, &result); err != nil {
		fmt.Fprintln(w, "✗ 无法读取事件时间线")
		return
	}
	if len(result.Events) == 0 {
		fmt.Fprintln(w, "事件时间线（暂无事件）")
		return
	}
	// Present newest-first by occurrence time; seq provides a stable tie-breaker.
	evs := result.Events
	sort.SliceStable(evs, func(i, j int) bool {
		if evs[i].OccurredAtMS != evs[j].OccurredAtMS {
			return evs[i].OccurredAtMS > evs[j].OccurredAtMS
		}
		return evs[i].Seq > evs[j].Seq
	})
	fmt.Fprintf(w, "事件时间线（最新在前，共 %d 条）\n", len(evs))
	lastDate := ""
	for _, e := range evs {
		t := time.UnixMilli(int64(e.OccurredAtMS))
		if date := t.Format("2006-01-02"); date != lastDate {
			fmt.Fprintf(w, "── %s ──\n", date)
			lastDate = date
		}
		attempt := ""
		if e.AttemptNo != nil {
			attempt = fmt.Sprintf(" · 尝试 %d", int(*e.AttemptNo))
		}
		actor := ""
		if e.Actor != "" {
			actor = " · " + e.Actor
		}
		fmt.Fprintf(w, "%s  %s  %s%s%s（%s）\n", t.Format("15:04:05"), eventTypeLabel(e.Type), e.RunID, attempt, actor, sourceLabel(e.Source))
	}
	if result.HasMore {
		fmt.Fprintf(w, "（还有更多事件：运行 sift timeline --after-seq %d --after-ms %d 查看下一页）\n", int64(result.NextSeq), int64(result.NextOccurredAtMS))
	}
}

// renderLogs humanizes the ops.logs result: the attempt header, the decoded
// log bytes (control characters escaped), and an honest truncation hint when
// the bounded read hit the byte limit before EOF.
func Logs(w io.Writer, runID string, value any) {
	var result struct {
		AttemptNo  float64 `json:"attempt_no"`
		EOF        bool    `json:"eof"`
		DataBase64 string  `json:"data_base64"`
	}
	if err := renormalize(value, &result); err != nil {
		fmt.Fprintln(w, "✗ 无法读取运行日志")
		return
	}
	data, err := base64.StdEncoding.DecodeString(result.DataBase64)
	if err != nil {
		fmt.Fprintln(w, "✗ 日志数据损坏（base64 解码失败）")
		return
	}
	fmt.Fprintf(w, "运行 %s 日志（第 %d 次尝试）\n", runID, int(result.AttemptNo))
	_, _ = w.Write(escapeLogBytes(data))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		fmt.Fprintln(w)
	}
	if !result.EOF {
		fmt.Fprintf(w, "（日志量较大，已显示 %d 字节；后续内容未显示）\n", len(data))
	}
}

// escapeLogBytes makes raw agent.log bytes printable: line/tab/CR survive,
// other control bytes become visible \xNN escapes (control-plane.md §6.2).
func escapeLogBytes(data []byte) []byte {
	var b bytes.Buffer
	for _, c := range data {
		switch {
		case c == '\n' || c == '\t' || c == '\r' || c >= 0x20 && c != 0x7f:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "\\x%02x", c)
		}
	}
	return b.Bytes()
}

// renderMetrics humanizes the ops.metrics result in sections: attention
// quota consumption, the ratio series with honest coverage notes, LLM token
// usage, and the trigger→started latency distribution.
func Metrics(w io.Writer, value any) {
	type ratio struct {
		Numerator   float64 `json:"numerator"`
		Denominator float64 `json:"denominator"`
		Rate        float64 `json:"rate"`
		Coverage    string  `json:"coverage"`
	}
	var result struct {
		Metrics struct {
			Scope                      string `json:"scope"`
			WeightedAttentionPerChange struct {
				WeightedMinutes float64 `json:"weighted_minutes"`
				MergedChanges   float64 `json:"merged_changes"`
				PerMergedChange float64 `json:"per_merged_change"`
				Coverage        string  `json:"coverage"`
			} `json:"weighted_attention_per_merged_change"`
			FalseReleaseRate          ratio `json:"false_release_rate"`
			GateBypassRate            ratio `json:"gate_bypass_rate"`
			GateMissRate              ratio `json:"gate_miss_rate"`
			GateFalseBlockRate        ratio `json:"gate_false_block_rate"`
			HITLRate                  ratio `json:"hitl_rate"`
			DispatchAccuracy          ratio `json:"dispatch_accuracy"`
			AttentionQuotaConsumption []struct {
				Severity string  `json:"severity"`
				Consumed float64 `json:"consumed"`
				Limit    float64 `json:"limit"`
				Rate     float64 `json:"rate"`
			} `json:"attention_quota_consumption"`
			ForgeAPIQuotaConsumption []struct {
				ProjectID string  `json:"project_id"`
				Consumed  float64 `json:"consumed"`
				Limit     float64 `json:"limit"`
				Unit      string  `json:"unit"`
			} `json:"forge_api_quota_consumption"`
			LLMCostPerMergedChange struct {
				PerMergedChangeInput  float64 `json:"per_merged_change_input_tokens"`
				PerMergedChangeOutput float64 `json:"per_merged_change_output_tokens"`
				MergedChanges         float64 `json:"merged_changes"`
				Coverage              string  `json:"coverage"`
			} `json:"llm_cost_per_merged_change"`
		} `json:"metrics"`
		Latency struct {
			Count    float64 `json:"count"`
			MinMS    float64 `json:"min_ms"`
			P50MS    float64 `json:"p50_ms"`
			P90MS    float64 `json:"p90_ms"`
			MaxMS    float64 `json:"max_ms"`
			Coverage string  `json:"coverage"`
		} `json:"trigger_started_latency"`
	}
	if err := renormalize(value, &result); err != nil {
		fmt.Fprintln(w, "✗ 无法读取指标")
		return
	}
	m := &result.Metrics
	scope := "全局"
	if m.Scope != "" && m.Scope != "global" {
		scope = "项目 " + m.Scope
	}
	fmt.Fprintf(w, "指标（%s）\n", scope)

	if len(m.AttentionQuotaConsumption) == 0 {
		fmt.Fprintln(w, "注意力配额：暂无已记录的消耗")
	} else {
		fmt.Fprintln(w, "注意力配额（今日已用 / 上限）：")
		for _, q := range m.AttentionQuotaConsumption {
			fmt.Fprintf(w, "  %s：%d / %d（%.1f%%）\n", severityLabel(q.Severity), int(q.Consumed), int(q.Limit), q.Rate*100)
		}
	}

	if len(m.ForgeAPIQuotaConsumption) == 0 {
		fmt.Fprintln(w, "Forge API 用量：暂无项目")
	} else {
		fmt.Fprintln(w, "Forge API 用量（本小时已用 / 上限）：")
		for _, q := range m.ForgeAPIQuotaConsumption {
			fmt.Fprintf(w, "  项目 %s：%d / %d %s\n", q.ProjectID, int(q.Consumed), int(q.Limit), q.Unit)
		}
	}

	wMetrics := m.WeightedAttentionPerChange
	fmt.Fprintf(w, "每合并变更注意力：%.1f 分钟（%d 个合并变更）\n", wMetrics.PerMergedChange, int(wMetrics.MergedChanges))
	if wMetrics.Coverage != "" {
		fmt.Fprintf(w, "  覆盖说明：%s\n", wMetrics.Coverage)
	}

	ratioLine := func(label string, r ratio) {
		fmt.Fprintf(w, "%s：%.1f%%（%d/%d）\n", label, r.Rate*100, int(r.Numerator), int(r.Denominator))
		if r.Coverage != "" {
			fmt.Fprintf(w, "  覆盖说明：%s\n", r.Coverage)
		}
	}
	ratioLine("误放行率", m.FalseReleaseRate)
	ratioLine("门禁绕过率", m.GateBypassRate)
	ratioLine("门禁漏检率", m.GateMissRate)
	ratioLine("门禁误拦率", m.GateFalseBlockRate)
	ratioLine("人工介入率", m.HITLRate)
	ratioLine("分派准确率", m.DispatchAccuracy)

	llm := m.LLMCostPerMergedChange
	fmt.Fprintf(w, "LLM 用量（每合并变更）：输入 %d / 输出 %d tokens（%d 个合并变更）\n", int(llm.PerMergedChangeInput), int(llm.PerMergedChangeOutput), int(llm.MergedChanges))
	if llm.Coverage != "" {
		fmt.Fprintf(w, "  覆盖说明：%s\n", llm.Coverage)
	}

	l := &result.Latency
	if l.Count == 0 {
		fmt.Fprintln(w, "触发→启动延迟：暂无样本")
	} else {
		fmt.Fprintf(w, "触发→启动延迟：%d 个样本 · 最小 %s · P50 %s · P90 %s · 最大 %s\n",
			int(l.Count), Duration(int64(l.MinMS)), Duration(int64(l.P50MS)), Duration(int64(l.P90MS)), Duration(int64(l.MaxMS)))
	}
	if l.Coverage != "" {
		fmt.Fprintf(w, "  覆盖说明：%s\n", l.Coverage)
	}
}

// renderWorktree humanizes the ops.worktree result (spec §6.2). The current
// daemon always answers not_found, which the humanized error path explains.
func Worktree(w io.Writer, runID string, value any) {
	var result struct {
		RunID               string  `json:"run_id"`
		AttemptNo           float64 `json:"attempt_no"`
		Path                string  `json:"path"`
		Exists              bool    `json:"exists"`
		IsolationState      string  `json:"isolation_state"`
		ReadOnlyRecommended bool    `json:"read_only_recommended"`
	}
	if err := renormalize(value, &result); err != nil {
		fmt.Fprintln(w, "✗ 无法读取工作树信息")
		return
	}
	if result.Path == "" {
		fmt.Fprintf(w, "✗ 运行 %s 没有可用的工作树\n", runID)
		return
	}
	fmt.Fprintf(w, "运行 %s 的工作树（第 %d 次尝试）\n", result.RunID, int(result.AttemptNo))
	state := "存在"
	if !result.Exists {
		state = "不存在"
	}
	fmt.Fprintf(w, "路径：%s（%s）\n", result.Path, state)
	fmt.Fprintf(w, "隔离状态：%s\n", result.IsolationState)
	if result.ReadOnlyRecommended {
		fmt.Fprintln(w, "建议只读：是（请勿在工作树内直接修改）")
	}
}

// renderKillRetry humanizes the ops.kill/ops.retry success result. Failures
// are rendered by renderError with the exit status unchanged.
func KillRetry(w io.Writer, verb, runID string, value any) {
	var result struct {
		Accepted    bool   `json:"accepted"`
		State       string `json:"state"`
		Disposition string `json:"disposition"`
		ProbeID     string `json:"probe_id"`
		Message     string `json:"message"`
	}
	if err := renormalize(value, &result); err != nil {
		fmt.Fprintln(w, "✗ 无法读取操作结果")
		return
	}
	verbLabel := "停止"
	if verb == "retry" {
		verbLabel = "重试"
	}
	if result.Accepted || result.Disposition == "accepted" {
		fmt.Fprintf(w, "✓ 已请求%s运行 %s", verbLabel, runID)
		state := result.State
		if state == "" {
			state = result.Message
		}
		if state != "" {
			fmt.Fprintf(w, "（%s）", state)
		}
		if result.ProbeID != "" {
			fmt.Fprintf(w, "，验证标识 %s", result.ProbeID)
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  已启动受控终止流程，等待执行体消失证据；用 sift ps 查看最新状态。")
		return
	}
	fmt.Fprintf(w, "✗ 未能%s运行 %s\n", verbLabel, runID)
	if result.Message != "" {
		fmt.Fprintf(w, "  原因：%s\n", result.Message)
	}
}

// reportKindLabel maps the closed report kind to Chinese.
func reportKindLabel(kind string) string {
	switch kind {
	case "progress":
		return "进度"
	case "goal":
		return "目标"
	case "blocker":
		return "阻塞"
	case "completed":
		return "完成"
	}
	return kind
}

// renderReport humanizes the report.submit success result.
func Report(w io.Writer, kind string, value any) {
	var result struct {
		Disposition string `json:"disposition"`
		ReceiptID   string `json:"receipt_id"`
		EventID     string `json:"event_id"`
	}
	if err := renormalize(value, &result); err != nil {
		fmt.Fprintln(w, "✗ 无法读取报告结果")
		return
	}
	fmt.Fprintf(w, "✓ 报告已提交（%s）：receipt %s\n", reportKindLabel(kind), result.ReceiptID)
	if result.EventID != "" {
		fmt.Fprintf(w, "  事件 %s 已记录\n", result.EventID)
	}
}

// renderReportError turns a permanent report.submit failure into an actionable
// Chinese line. The exit status is unchanged.
func ReportError(w io.Writer, response controlplane.Response) {
	code, message, detailCode := "", "", ""
	if response.Error != nil {
		code, message = response.Error.Code, response.Error.Message
		if v, ok := response.Error.Details["code"].(string); ok {
			detailCode = v
		}
	}
	switch {
	case detailCode == "report_interrupt_quota_exhausted":
		fmt.Fprintln(w, "✗ 报告被拒绝：报告中断配额已用尽（report_interrupt_quota_exhausted）")
	case code == "unauthorized":
		fmt.Fprintln(w, "✗ 报告被拒绝：凭据无效（unauthorized）")
	case code == "stale":
		fmt.Fprintln(w, "✗ 报告被拒绝：运行或尝试已变化（stale），请核对运行状态后重试")
	case code == "conflict":
		fmt.Fprintln(w, "✗ 报告被拒绝：与既有报告冲突或超出频率限制（conflict）")
	case code == "invalid_request":
		fmt.Fprintln(w, "✗ 报告被拒绝：请求参数无效（invalid_request）")
	case code == "internal":
		fmt.Fprintf(w, "✗ 报告提交失败（internal）：%s\n", message)
	default:
		fmt.Fprintf(w, "✗ 报告被拒绝（%s）：%s\n", code, message)
	}
}
