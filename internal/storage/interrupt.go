package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type InterruptReason string
type InterruptSeverity string

type GatePhase string
type GuardrailLevel string

type ExpireAction string

const (
	InterruptDesignApproval     InterruptReason = "design_approval"
	InterruptGuardrailViolation InterruptReason = "guardrail_violation"
	InterruptCodeReview         InterruptReason = "code_review"
	InterruptAgentBlocked       InterruptReason = "agent_blocked"
	InterruptMergeConflict      InterruptReason = "merge_conflict"
	InterruptFailureReview      InterruptReason = "failure_review"
	InterruptStartupStall       InterruptReason = "startup_stall"

	SeverityLow      InterruptSeverity = "low"
	SeverityNormal   InterruptSeverity = "normal"
	SeverityHigh     InterruptSeverity = "high"
	SeverityCritical InterruptSeverity = "critical"

	GateNone         GatePhase      = "none"
	GatePreStart     GatePhase      = "pre_start"
	GateReview       GatePhase      = "review"
	GateMerge        GatePhase      = "merge"
	GuardrailNone    GuardrailLevel = "none"
	GuardrailSoft    GuardrailLevel = "soft"
	GuardrailHard    GuardrailLevel = "hard"
	ExpireHold       ExpireAction   = "hold"
	ExpireEscalate   ExpireAction   = "escalate"
	ExpireAutoReject ExpireAction   = "auto_reject"
)

type InterruptOption struct{ ID, Label, Effect, Risk string }
type InterruptLink struct{ Label, Target string }

type InterruptGeneration struct {
	TaskSpecSnapshotID, PolicySnapshotID, ViolationCode, SubjectDigest string
	ChangeID, HeadSHA, ReportID, ConflictDigest, FailureDigest         string
	AttemptNo, Generation                                              int
}

// EmitInterruptCmd carries only facts. Templates, severity and the generation
// key are derived here so callers cannot manufacture a more urgent or broader
// Interrupt.
type EmitInterruptCmd struct {
	RunID                           string
	ExpectedRunVersion              int64
	AttemptNo                       *int
	Reason                          InterruptReason
	Facts                           map[string]string
	Generation                      InterruptGeneration
	GatePhase                       GatePhase
	GuardrailLevel                  GuardrailLevel
	EscalationCount, MaxEscalations int
	ExpiresAfterMS                  int64
	OnExpire                        ExpireAction
	AttentionDailyQuota             map[InterruptSeverity]int
	DayTimezone                     string
	Source                          EventSource
	NowMS                           int64
}

type Interrupt struct {
	ID, RunID, GenerationKey string
	AttemptNo                *int
	Reason                   InterruptReason
	Severity                 InterruptSeverity
	Headline, Brief          string
	Options                  []InterruptOption
	MinModality              string
	Links                    []InterruptLink
	ExpiresAtMS              int64
	OnExpire                 ExpireAction
	ChargedBudgetEntryID     string
}

var ErrInterruptRejected = errors.New("storage: interrupt rejected")
var ErrAttentionQuotaExceeded = errors.New("storage: attention quota exceeded")

type interruptTemplate struct {
	headline, modality string
	options            []InterruptOption
	facts, links       []string
	base               InterruptSeverity
	expires            int64
	onExpire           ExpireAction
}

var interruptTemplates = map[InterruptReason]interruptTemplate{
	InterruptDesignApproval:     {"需要批准后再开始", "text", []InterruptOption{{"approve", "批准并开始", "Run 进入可启动状态", "开始后会消耗执行资源"}, {"reject", "拒绝并停止", "Run 停止", "需人工重新发起"}, {"hold", "暂缓决定", "保持等待", "Run 继续占用待处理项"}}, []string{"risk_summary", "recommended_action", "task_spec_ref"}, []string{"task_spec_ref"}, SeverityNormal, 24 * 3600 * 1000, ExpireHold},
	InterruptGuardrailViolation: {"软护栏需要人工裁决", "text", []InterruptOption{{"approve", "豁免本次规则", "继续当前 Run", "豁免会接受规则所述风险"}, {"reject", "拒绝并停止", "Run 停止", "需人工重新发起"}, {"hold", "暂缓决定", "保持等待", "Run 继续占用待处理项"}}, []string{"rule_id", "impact_scope", "recommended_action", "policy_evidence_ref"}, []string{"policy_evidence_ref"}, SeverityHigh, 24 * 3600 * 1000, ExpireHold},
	InterruptCodeReview:         {"变更等待代码审阅", "visual", []InterruptOption{{"approve", "批准审阅", "继续后续流程", "批准变更的当前内容"}, {"reject", "拒绝变更", "Run 停止", "需重新修改后发起"}, {"hold", "暂缓审阅", "保持等待", "Change 继续待审"}}, []string{"change_ref", "head_sha", "review_requirement", "recommended_action", "diff_ref"}, []string{"change_ref", "diff_ref"}, SeverityNormal, 72 * 3600 * 1000, ExpireHold},
	InterruptAgentBlocked:       {"Agent 需要你澄清", "voice", []InterruptOption{{"ask", "提供澄清", "写入澄清内容", "澄清会改变后续执行方向"}, {"retry", "重试当前工作", "再次尝试执行", "未澄清时可能再次阻塞"}, {"reject", "停止 Run", "Run 停止", "已完成工作不会继续"}, {"hold", "暂缓决定", "保持等待", "Run 继续占用待处理项"}}, []string{"blocker_summary", "attempted_summary", "recommended_action", "agent_log_ref"}, []string{"agent_log_ref"}, SeverityNormal, 8 * 3600 * 1000, ExpireEscalate},
	InterruptMergeConflict:      {"合并冲突需要处理", "voice", []InterruptOption{{"retry", "重新执行合并", "再次尝试合并", "冲突未变时会再次失败"}, {"reject", "停止 Run", "Run 停止", "Change 不会合并"}, {"hold", "暂缓决定", "保持等待", "Change 继续待处理"}}, []string{"change_ref", "head_sha", "conflict_summary", "recommended_action", "conflict_evidence_ref"}, []string{"change_ref", "conflict_evidence_ref"}, SeverityHigh, 8 * 3600 * 1000, ExpireEscalate},
	InterruptFailureReview:      {"失败需要人工决定", "voice", []InterruptOption{{"retry", "重试失败步骤", "再次执行", "相同故障可能再次发生"}, {"reject", "停止 Run", "Run 停止", "需人工重新发起"}, {"hold", "暂缓决定", "保持等待", "Run 继续占用待处理项"}}, []string{"failure_class", "failure_evidence_ref", "recommended_action"}, []string{"failure_evidence_ref"}, SeverityHigh, 24 * 3600 * 1000, ExpireAutoReject},
	InterruptStartupStall:       {"无法确认旧执行体已停止", "text", []InterruptOption{{"retry", "重新探测旧执行体", "请求受控终止再探测", "未确认消失时仍保持隔离"}, {"reject", "放弃此 Run", "停止处理并保持隔离", "不代表旧执行体已停止"}, {"hold", "继续等待", "保持等待和隔离", "旧执行体可能仍在运行"}}, []string{"attempt_no", "generation", "diagnostic_cause", "isolation_consequence", "recommended_action", "attempt_diagnostic_ref", "worktree_ref"}, []string{"attempt_diagnostic_ref", "worktree_ref"}, SeverityHigh, 3600 * 1000, ExpireEscalate},
}

func BaseSeverity(reason InterruptReason, phase GatePhase, guard GuardrailLevel, escalation, max int) (InterruptSeverity, error) {
	t, ok := interruptTemplates[reason]
	if !ok {
		return "", fmt.Errorf("%w: unknown reason %q", ErrInterruptRejected, reason)
	}
	if guard == GuardrailHard {
		return "", fmt.Errorf("%w: hard guardrails do not emit interrupts", ErrInterruptRejected)
	}
	if phase != GateNone && phase != GatePreStart && phase != GateReview && phase != GateMerge {
		return "", fmt.Errorf("%w: invalid gate phase", ErrInterruptRejected)
	}
	if guard != GuardrailNone && guard != GuardrailSoft {
		return "", fmt.Errorf("%w: invalid guardrail level", ErrInterruptRejected)
	}
	if escalation < 0 || max < 0 {
		return "", fmt.Errorf("%w: negative escalation", ErrInterruptRejected)
	}
	s := t.base
	if guard == GuardrailSoft && s == SeverityLow || guard == GuardrailSoft && s == SeverityNormal {
		s = SeverityHigh
	}
	if phase == GateMerge && reason == InterruptCodeReview && s == SeverityNormal {
		s = SeverityHigh
	}
	for i := 0; i < minInt(escalation, max); i++ {
		s = promoteSeverity(s)
	}
	return s, nil
}

// EmitInterrupt is the sole creation port. Its transaction contains the Run
// transition, budget charge, Interrupt, audit event and forge-comment outbox.
func (d *DB) EmitInterrupt(ctx context.Context, cmd EmitInterruptCmd) (Interrupt, error) {
	t, ok := interruptTemplates[cmd.Reason]
	if !ok {
		return Interrupt{}, fmt.Errorf("%w: unknown reason", ErrInterruptRejected)
	}
	if cmd.RunID == "" || cmd.NowMS <= 0 || !validSource(cmd.Source) {
		return Interrupt{}, fmt.Errorf("%w: run, source and timestamp are required", ErrInterruptRejected)
	}
	if cmd.Reason == InterruptStartupStall && cmd.AttemptNo == nil {
		return Interrupt{}, fmt.Errorf("%w: startup_stall requires attempt", ErrInterruptRejected)
	}
	if cmd.ExpiresAfterMS == 0 {
		cmd.ExpiresAfterMS = t.expires
	}
	if cmd.OnExpire == "" {
		cmd.OnExpire = t.onExpire
	}
	if cmd.ExpiresAfterMS <= 0 || (cmd.OnExpire != ExpireHold && cmd.OnExpire != ExpireEscalate && cmd.OnExpire != ExpireAutoReject) || (cmd.Reason == InterruptStartupStall && cmd.OnExpire == ExpireAutoReject) {
		return Interrupt{}, fmt.Errorf("%w: invalid expiry policy", ErrInterruptRejected)
	}
	brief, links, err := renderInterrupt(t, cmd.Facts, cmd.Reason)
	if err != nil {
		return Interrupt{}, err
	}
	severity, err := BaseSeverity(cmd.Reason, cmd.GatePhase, cmd.GuardrailLevel, cmd.EscalationCount, cmd.MaxEscalations)
	if err != nil {
		return Interrupt{}, err
	}
	key, err := interruptGenerationKey(cmd.RunID, cmd.Reason, cmd.Generation)
	if err != nil {
		return Interrupt{}, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return Interrupt{}, err
	}
	defer tx.Rollback()
	if existing, found, err := interruptByKeyTx(ctx, tx, key); err != nil {
		return Interrupt{}, err
	} else if found {
		if err := tx.Commit(); err != nil {
			return Interrupt{}, err
		}
		return existing, nil
	}
	var status string
	var version int64
	var projectID, forgeKind, forgeHost, forgeProject string
	var issueID, issueURL, changeID, targetKind, targetID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT status,version,project_id,forge_kind,forge_host,forge_project_key,issue_id,issue_url,change_id,discussion_target_kind,discussion_target_id FROM runs WHERE id=?`, cmd.RunID).Scan(&status, &version, &projectID, &forgeKind, &forgeHost, &forgeProject, &issueID, &issueURL, &changeID, &targetKind, &targetID); err != nil {
		return Interrupt{}, err
	}
	if version != cmd.ExpectedRunVersion {
		return Interrupt{}, ErrRejectedStale
	}
	if RunStatus(status) != RunWaitingHuman {
		if !legalTransition(RunStatus(status), RunWaitingHuman) {
			return Interrupt{}, fmt.Errorf("%w: %s cannot wait for human", ErrInterruptRejected, status)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET status='waiting_human',version=version+1,updated_at_ms=? WHERE id=? AND version=?`, cmd.NowMS, cmd.RunID, version); err != nil {
			return Interrupt{}, err
		}
	}
	if cmd.Reason == InterruptStartupStall {
		if cmd.Generation.AttemptNo != *cmd.AttemptNo || cmd.Generation.Generation < 1 {
			return Interrupt{}, fmt.Errorf("%w: startup_stall attempt identity mismatch", ErrInterruptRejected)
		}
		diagnostic := cmd.Facts["diagnostic_cause"]
		if diagnostic != "process_identity_unknown" && diagnostic != "termination_unconfirmed" && diagnostic != "process_group_unverified" {
			return Interrupt{}, fmt.Errorf("%w: invalid startup_stall diagnostic cause", ErrInterruptRejected)
		}
		var generation int
		var isolation string
		if err := tx.QueryRowContext(ctx, `SELECT generation,isolation_state FROM attempts WHERE run_id=? AND attempt_no=?`, cmd.RunID, *cmd.AttemptNo).Scan(&generation, &isolation); err != nil {
			return Interrupt{}, err
		}
		if generation != cmd.Generation.Generation {
			return Interrupt{}, ErrRejectedStale
		}
		if isolation == "none" {
			if _, err := tx.ExecContext(ctx, `UPDATE attempts SET isolation_state='frozen',isolation_reason=?,isolated_at_ms=?,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND isolation_state='none'`, diagnostic, cmd.NowMS, cmd.NowMS, cmd.RunID, *cmd.AttemptNo); err != nil {
				return Interrupt{}, err
			}
		} else if isolation != "frozen" {
			return Interrupt{}, fmt.Errorf("%w: invalid attempt isolation state", ErrInterruptRejected)
		}
	}
	kind, id := "issue", issueID.String
	if id == "" {
		kind, id = "change", changeID.String
	}
	if id == "" {
		kind, id = targetKind.String, targetID.String
	}
	if id == "" {
		return Interrupt{}, fmt.Errorf("%w: interrupt_publish_target_missing", ErrInterruptRejected)
	}
	if issueURL.Valid && validLink(issueURL.String) {
		links = append(links, InterruptLink{Label: "Issue", Target: issueURL.String})
		sort.Slice(links, func(i, j int) bool {
			if links[i].Target == links[j].Target {
				return links[i].Label < links[j].Label
			}
			return links[i].Target < links[j].Target
		})
	}
	entryID, err := chargeAttentionTx(ctx, tx, cmd, severity)
	if err != nil {
		return Interrupt{}, err
	}
	in := Interrupt{ID: newID(), RunID: cmd.RunID, AttemptNo: cmd.AttemptNo, GenerationKey: key, Reason: cmd.Reason, Severity: severity, Headline: t.headline, Brief: brief, Options: t.options, MinModality: t.modality, Links: links, ExpiresAtMS: cmd.NowMS + cmd.ExpiresAfterMS, OnExpire: cmd.OnExpire, ChargedBudgetEntryID: entryID}
	optionsJSON, _ := json.Marshal(in.Options)
	linksJSON, _ := json.Marshal(in.Links)
	if _, err := tx.ExecContext(ctx, `INSERT INTO interrupts (id,run_id,attempt_no,generation_key,reason,severity,headline,brief_markdown,options_json,min_modality,links_json,nonce,version,status,dispatch_state,expires_at_ms,on_expire,escalation_count,max_escalations,charged_budget_entry_id,created_at_ms,updated_at_ms) VALUES (?,?,?,?,?,?,?,?,?,?,?,? ,1,'open','ready',?,?,?,?,?,?,?)`, in.ID, in.RunID, in.AttemptNo, in.GenerationKey, in.Reason, in.Severity, in.Headline, in.Brief, string(optionsJSON), in.MinModality, string(linksJSON), newID(), in.ExpiresAtMS, in.OnExpire, cmd.EscalationCount, cmd.MaxEscalations, in.ChargedBudgetEntryID, cmd.NowMS, cmd.NowMS); err != nil {
		return Interrupt{}, err
	}
	eventID := newID()
	eventPayload, _ := json.Marshal(map[string]any{"interrupt_id": in.ID, "reason": in.Reason, "generation_key": in.GenerationKey})
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (id,run_id,attempt_no,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES (?,?,?,?, 'interrupt.emitted',?,1,?,?,?)`, eventID, in.RunID, in.AttemptNo, projectID, cmd.Source, string(eventPayload), cmd.NowMS, cmd.NowMS); err != nil {
		return Interrupt{}, err
	}
	markdown := renderComment(in)
	// forge_comment workers consume this flat body; the operation envelope is
	// represented by the outbox row and its event association.
	payload, _ := json.Marshal(map[string]any{"project_id": projectID, "forge_kind": forgeKind, "forge_host": forgeHost, "forge_project_key": forgeProject, "target_kind": kind, "target_id": id, "purpose": "interrupt", "markdown": markdown})
	opKey := CommentOperationKey("interrupt", in.ID, 1)
	if err := insertOperation(ctx, tx, Operation{Key: opKey, Kind: OperationForgeComment, Payload: payload, RunID: in.RunID, AttemptNo: in.AttemptNo, InterruptID: in.ID}, in.RunID, eventID, cmd.NowMS); err != nil {
		return Interrupt{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO interrupt_deliveries (id,interrupt_id,surface,priority,operation_key,state,attempt_count,created_at_ms) VALUES (?,?,'forge_comment','normal',?,'pending',0,?)`, newID(), in.ID, opKey, cmd.NowMS); err != nil {
		return Interrupt{}, err
	}
	if err := tx.Commit(); err != nil {
		return Interrupt{}, err
	}
	d.wakeOutbox()
	return in, nil
}

func renderComment(in Interrupt) string { return in.Headline + "\n\n" + in.Brief }
func renderInterrupt(t interruptTemplate, facts map[string]string, reason InterruptReason) (string, []InterruptLink, error) {
	vals := make([]string, len(t.facts))
	for i, k := range t.facts {
		v, ok := facts[k]
		if !ok || v == "" {
			return "", nil, fmt.Errorf("%w: missing fact %s", ErrInterruptRejected, k)
		}
		e, err := escapeBrief(v)
		if err != nil {
			return "", nil, err
		}
		vals[i] = e
	}
	parts := make([]string, len(t.facts))
	for i, k := range t.facts {
		parts[i] = k + "=" + vals[i]
	}
	brief := "事实：" + strings.Join(parts, "；") + "。建议：" + vals[indexOf(t.facts, "recommended_action")]
	links := []InterruptLink{}
	for _, k := range t.links {
		v := facts[k]
		if !validLink(v) {
			return "", nil, fmt.Errorf("%w: invalid required link %s", ErrInterruptRejected, k)
		}
		links = append(links, InterruptLink{k, v})
	}
	sort.Slice(links, func(i, j int) bool {
		if links[i].Target == links[j].Target {
			return links[i].Label < links[j].Label
		}
		return links[i].Target < links[j].Target
	})
	out := links[:0]
	for _, l := range links {
		if len(out) == 0 || out[len(out)-1] != l {
			out = append(out, l)
		}
	}
	return brief, out, nil
}
func escapeBrief(v string) (string, error) {
	if strings.Contains(v, "\r\n") {
		return "", fmt.Errorf("%w: interrupt_brief_crlf_rejected", ErrInterruptRejected)
	}
	if strings.Contains(v, "\r") {
		return "", fmt.Errorf("%w: interrupt_brief_cr_rejected", ErrInterruptRejected)
	}
	if strings.Contains(v, "\n") {
		return "", fmt.Errorf("%w: interrupt_brief_lf_rejected", ErrInterruptRejected)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: interrupt_brief_control_rejected", ErrInterruptRejected)
		}
	}
	return strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)", "#", "\\#", "+", "\\+", "-", "\\-", "!", "\\!", ">", "\\>").Replace(v), nil
}
func validLink(v string) bool { return strings.HasPrefix(v, "/") || strings.HasPrefix(v, "https://") }
func indexOf(a []string, s string) int {
	for i, v := range a {
		if v == s {
			return i
		}
	}
	return -1
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func promoteSeverity(s InterruptSeverity) InterruptSeverity {
	switch s {
	case SeverityLow:
		return SeverityNormal
	case SeverityNormal:
		return SeverityHigh
	default:
		return SeverityCritical
	}
}

func interruptGenerationKey(run string, reason InterruptReason, g InterruptGeneration) (string, error) {
	fields := [][2]string{{"string:domain", "sift.interrupt.generation"}, {"uint:version", "1"}, {"string:run_id", run}, {"enum:reason", string(reason)}}
	add := func(t, n, v string) { fields = append(fields, [2]string{t + ":" + n, v}) }
	switch reason {
	case InterruptDesignApproval:
		add("string", "task_spec_snapshot_id", g.TaskSpecSnapshotID)
	case InterruptGuardrailViolation:
		add("string", "policy_snapshot_id", g.PolicySnapshotID)
		add("string", "violation_code", g.ViolationCode)
		add("sha256", "subject_digest", g.SubjectDigest)
	case InterruptCodeReview:
		add("string", "change_id", g.ChangeID)
		add("git_oid", "head_sha", g.HeadSHA)
	case InterruptAgentBlocked:
		add("uint", "attempt_no", fmt.Sprint(g.AttemptNo))
		add("uint", "generation", fmt.Sprint(g.Generation))
		add("string", "report_id", g.ReportID)
	case InterruptMergeConflict:
		add("string", "change_id", g.ChangeID)
		add("git_oid", "head_sha", g.HeadSHA)
		add("sha256", "conflict_digest", g.ConflictDigest)
	case InterruptFailureReview:
		add("uint", "attempt_no", fmt.Sprint(g.AttemptNo))
		add("uint", "generation", fmt.Sprint(g.Generation))
		add("sha256", "failure_digest", g.FailureDigest)
	case InterruptStartupStall:
		add("uint", "attempt_no", fmt.Sprint(g.AttemptNo))
		add("uint", "generation", fmt.Sprint(g.Generation))
		add("enum", "cause", "startup_stall")
	default:
		return "", fmt.Errorf("%w: unknown reason", ErrInterruptRejected)
	}
	var b strings.Builder
	for _, f := range fields {
		if f[1] == "" || !utf8.ValidString(f[1]) || strings.ContainsRune(f[1], 0) {
			return "", fmt.Errorf("%w: invalid generation field", ErrInterruptRejected)
		}
		switch {
		case strings.HasPrefix(f[0], "uint:"):
			if f[1] == "0" || strings.HasPrefix(f[1], "-") || !decimal(f[1]) {
				return "", fmt.Errorf("%w: invalid uint", ErrInterruptRejected)
			}
		case strings.HasPrefix(f[0], "sha256:"):
			if len(f[1]) != 64 || !lowerHex(f[1]) {
				return "", fmt.Errorf("%w: invalid sha256", ErrInterruptRejected)
			}
		case strings.HasPrefix(f[0], "git_oid:"):
			if (len(f[1]) != 40 && len(f[1]) != 64) || !lowerHex(f[1]) {
				return "", fmt.Errorf("%w: invalid git oid", ErrInterruptRejected)
			}
		}
		b.WriteString(f[0])
		b.WriteByte(0)
		b.WriteString(f[1])
		b.WriteByte(0)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:]), nil
}

func chargeAttentionTx(ctx context.Context, tx *sql.Tx, cmd EmitInterruptCmd, s InterruptSeverity) (string, error) {
	id := newID()
	key := "interrupt-charge:" + mustGenerationKey(cmd)
	bucket := cmd.NowMS
	if s != SeverityCritical {
		loc := time.Local
		if cmd.DayTimezone != "" && cmd.DayTimezone != "local" {
			var err error
			loc, err = time.LoadLocation(cmd.DayTimezone)
			if err != nil {
				return "", fmt.Errorf("%w: invalid day timezone", ErrInterruptRejected)
			}
		}
		t := time.UnixMilli(cmd.NowMS).In(loc)
		start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
		bucket = start.UnixMilli()
		limit, ok := cmd.AttentionDailyQuota[s]
		if !ok {
			return "", fmt.Errorf("%w: attention quota missing", ErrInterruptRejected)
		}
		end := start.AddDate(0, 0, 1).UnixMilli()
		if _, err := tx.ExecContext(ctx, `INSERT INTO budget_counters (kind,scope,scope_id,bucket_start_ms,bucket_end_ms,limit_value,consumed_value,version,updated_at_ms) VALUES ('attention','severity',?,?,?, ?,0,1,?) ON CONFLICT DO NOTHING`, string(s), bucket, end, limit, cmd.NowMS); err != nil {
			return "", err
		}
		res, err := tx.ExecContext(ctx, `UPDATE budget_counters SET consumed_value=consumed_value+1,version=version+1,updated_at_ms=? WHERE kind='attention' AND scope='severity' AND scope_id=? AND bucket_start_ms=? AND consumed_value<limit_value`, cmd.NowMS, string(s), bucket)
		if err != nil {
			return "", err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return "", ErrAttentionQuotaExceeded
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO budget_entries (id,kind,scope,scope_id,bucket_start_ms,amount,reason,run_id,operation_key,created_at_ms) VALUES (?,'attention','severity',?,?,1,?,?,?,?)`, id, string(s), bucket, string(cmd.Reason), cmd.RunID, key, cmd.NowMS); err != nil {
		return "", err
	}
	return id, nil
}
func decimal(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
func lowerHex(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func mustGenerationKey(cmd EmitInterruptCmd) string {
	k, err := interruptGenerationKey(cmd.RunID, cmd.Reason, cmd.Generation)
	if err != nil {
		panic(err)
	}
	return k
}
func interruptByKeyTx(ctx context.Context, tx *sql.Tx, key string) (Interrupt, bool, error) {
	var in Interrupt
	var n sql.NullInt64
	var opts, links string
	var reason, severity, on string
	err := tx.QueryRowContext(ctx, `SELECT id,run_id,attempt_no,generation_key,reason,severity,headline,brief_markdown,options_json,min_modality,links_json,expires_at_ms,on_expire,charged_budget_entry_id FROM interrupts WHERE generation_key=?`, key).Scan(&in.ID, &in.RunID, &n, &in.GenerationKey, &reason, &severity, &in.Headline, &in.Brief, &opts, &in.MinModality, &links, &in.ExpiresAtMS, &on, &in.ChargedBudgetEntryID)
	if errors.Is(err, sql.ErrNoRows) {
		return Interrupt{}, false, nil
	}
	if err != nil {
		return Interrupt{}, false, err
	}
	if n.Valid {
		x := int(n.Int64)
		in.AttemptNo = &x
	}
	in.Reason = InterruptReason(reason)
	in.Severity = InterruptSeverity(severity)
	in.OnExpire = ExpireAction(on)
	if json.Unmarshal([]byte(opts), &in.Options) != nil || json.Unmarshal([]byte(links), &in.Links) != nil {
		return Interrupt{}, false, errors.New("storage: corrupt interrupt JSON")
	}
	return in, true, nil
}
