package storage

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
)

func interruptBriefFragments(t interruptTemplate, facts map[string]string) []string {
	if t.headline == "报告打扰额度已耗尽" {
		return []string{"请人工处理", "额度已耗尽"}
	}
	fragments := make([]string, 0, len(t.facts))
	for _, key := range t.facts {
		if value, ok := facts[key]; ok && safeT4Fragment(value) {
			fragments = append(fragments, value)
		}
	}
	sort.Strings(fragments)
	return slices.Compact(fragments)
}

func safeT4Fragment(value string) bool {
	return !strings.ContainsAny(value, "\r\n") && strings.IndexFunc(value, unicode.IsControl) < 0
}

func canonicalRecommendedAction(options []InterruptOption, action string) bool {
	for _, option := range options {
		if action == option.ID {
			return true
		}
	}
	return false
}

func acceptInterruptT4(in InterruptT4Input, out InterruptT4Output) (bool, string) {
	if out.Headline != in.Headline || !containsString(in.Fragments, out.Conclusion) || len(out.KeyPoints) < 1 || len(out.KeyPoints) > 3 || len(out.Options) != len(in.Options) {
		return false, ""
	}
	for i, option := range in.Options {
		if out.Options[i] != option.ID {
			return false, ""
		}
	}
	seen := map[string]bool{}
	for _, point := range out.KeyPoints {
		if !containsString(in.Fragments, point) || seen[point] {
			return false, ""
		}
		seen[point] = true
	}
	label := ""
	for _, option := range in.Options {
		if option.ID == out.RecommendedOptionID {
			label = option.Label
			break
		}
	}
	if label == "" {
		return false, ""
	}
	points := make([]string, len(out.KeyPoints))
	for i, point := range out.KeyPoints {
		points[i] = escapeT4Text(point)
	}
	return true, "结论：" + escapeT4Text(out.Conclusion) + "；要点：" + strings.Join(points, "；") + "；建议：" + escapeT4Text(label) + "（" + out.RecommendedOptionID + "）"
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
func escapeT4Text(value string) string {
	return strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)", "#", "\\#", "+", "\\+", "-", "\\-", "!", "\\!", ">", "\\>", "<", "\\<", "&", "\\&").Replace(value)
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
	return strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)", "#", "\\#", "+", "\\+", "-", "\\-", "!", "\\!", ">", "\\>", "_", "\\_").Replace(v), nil
}
func timezoneOrUTC(v string) string {
	if v == "" || v == "local" {
		return "UTC"
	}
	return v
}
func summaryOrDefault(v string) string {
	if v == "" {
		return "09:00"
	}
	return v
}
func fuseWindowOrDefault(v int64) int64 {
	if v <= 0 {
		return 15 * 60 * 1000
	}
	return v
}
func fuseTotalOrDefault(v int) int {
	if v <= 0 {
		return 5
	}
	return v
}
func fuseRunOrDefault(v int) int {
	if v <= 0 {
		return 2
	}
	return v
}

func quotaDay(now int64, zone string) string {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc = time.UTC
	}
	return time.UnixMilli(now).In(loc).Format("2006-01-02")
}

func nullableInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
func validLink(v string) bool {
	if strings.HasPrefix(v, "/") || strings.HasPrefix(v, "https://") {
		return true
	}
	const changePrefix = "sift://change/"
	if strings.HasPrefix(v, changePrefix) && len(v) > len(changePrefix) {
		return true
	}
	const prefix = "sift://event/"
	if !strings.HasPrefix(v, prefix) {
		return false
	}
	rest := v[len(prefix):]
	// Security event reference (report quota): sift://event/<32 lowercase hex>.
	if len(rest) == 32 && lowerHex(rest) {
		return true
	}
	// Terminal event reference (storage.md §8.1): sift://event/event:<K>, where
	// <K> is a server-allocated event key over [a-z0-9:_] (operation key plus a
	// closed suffix such as :failed or :verdict:<kind>:<code>).
	const terminalPrefix = "event:"
	if strings.HasPrefix(rest, terminalPrefix) {
		if k := rest[len(terminalPrefix):]; k != "" && validEventKey(k) {
			return true
		}
	}
	return false
}

func validEventKey(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == ':' || c == '_') {
			return false
		}
	}
	return true
}
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
