// Package gate implements the deterministic M4 Gate decision function.
// It deliberately has no storage, Forge, Brain, clock, or filesystem dependency.
package gate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"sort"
	"strings"

	"github.com/xsift/sift/internal/config"
	"github.com/xsift/sift/internal/policy"
)

const Version = "gate/v1"

type Identity struct {
	RunID     string `json:"run_id"`
	ProjectID string `json:"project_id"`
	TaskKind  string `json:"task_kind"`
	ChangeID  string `json:"change_id"`
}
type Change struct {
	State         string   `json:"state"`
	HeadSHA       string   `json:"head_sha"`
	BaseRef       string   `json:"base_ref"`
	HeadRef       string   `json:"head_ref"`
	IsDraft       bool     `json:"is_draft"`
	Mergeability  string   `json:"mergeability"`
	ReviewState   string   `json:"review_state"`
	PathsComplete bool     `json:"paths_complete"`
	ChangedPaths  []string `json:"changed_paths"`
	FilesChanged  int      `json:"files_changed"`
	Additions     int      `json:"additions"`
	Deletions     int      `json:"deletions"`
}
type Job struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	WebURL       string `json:"web_url"`
	AllowFailure bool   `json:"allow_failure"`
}
type Source struct {
	Kind                string `json:"kind"`
	LogicalCallID       string `json:"logical_call_id"`
	PromptVersion       string `json:"prompt_version,omitempty"`
	OutputSchemaVersion int    `json:"output_schema_version,omitempty"`
	Version             string `json:"version,omitempty"`
	Reason              string `json:"reason,omitempty"`
}
type Triage struct {
	Classification string  `json:"classification"`
	RetryCheckID   *string `json:"retry_check_id"`
	Source         Source  `json:"source"`
}
type Checks struct {
	Conclusion         string  `json:"conclusion"`
	FailedJobs         []Job   `json:"failed_jobs"`
	ExternalURL        string  `json:"external_url"`
	FlakyRetriesUsed   int     `json:"flaky_retries_used"`
	PendingStartedAtMS *int64  `json:"pending_started_at_ms"`
	ObservedAtMS       *int64  `json:"observed_at_ms"`
	PendingTimedOut    *bool   `json:"pending_timed_out"`
	Triage             *Triage `json:"triage"`
}
type Risk struct {
	RiskScore  int      `json:"risk_score"`
	RiskPoints []string `json:"risk_points"`
	Rationale  string   `json:"rationale"`
	Source     Source   `json:"source"`
}
type Exemption struct {
	RunID              string `json:"run_id"`
	HeadSHA            string `json:"head_sha"`
	RuleID             string `json:"rule_id"`
	MatchedPathsDigest string `json:"matched_paths_digest"`
}
type Input struct {
	SchemaVersion             int                      `json:"schema_version"`
	Identity                  Identity                 `json:"identity"`
	Change                    Change                   `json:"change"`
	Checks                    Checks                   `json:"checks"`
	EffectivePolicy           policy.EffectivePolicyV1 `json:"effective_policy"`
	EffectivePolicyHash       string                   `json:"effective_policy_hash"`
	CertificationRulesVersion string                   `json:"certification_rules_version"`
	CertificationVersion      string                   `json:"certification_version"`
	Risk                      Risk                     `json:"risk"`
	OneTimeExemptions         []Exemption              `json:"one_time_exemptions"`
}

type Verdict struct {
	SchemaVersion      int      `json:"schema_version"`
	HeadSHA            string   `json:"head_sha"`
	Kind               string   `json:"kind"`
	Code               string   `json:"code"`
	ChangeState        string   `json:"change_state,omitempty"`
	RuleID             string   `json:"rule_id,omitempty"`
	MatchedPaths       []string `json:"matched_paths,omitempty"`
	MatchedPathsDigest string   `json:"matched_paths_digest,omitempty"`
	ExternalURL        string   `json:"external_url,omitempty"`
	PendingStartedAtMS *int64   `json:"pending_started_at_ms,omitempty"`
	CheckRunID         string   `json:"check_run_id,omitempty"`
	RetryNo            int      `json:"retry_no,omitempty"`
	Classification     string   `json:"classification,omitempty"`
	ReviewPolicy       string   `json:"review_policy,omitempty"`
	RiskScore          *int     `json:"risk_score,omitempty"`
	Mergeability       string   `json:"mergeability,omitempty"`
	ChangeID           string   `json:"change_id,omitempty"`
	ExpectedHeadSHA    string   `json:"expected_head_sha,omitempty"`
	Reason             string   `json:"reason,omitempty"`
	Field              string   `json:"field,omitempty"`
}

type AssemblyError struct{ Code, Field string }

func (e *AssemblyError) Error() string { return "gate assembly: " + e.Code + " (" + e.Field + ")" }

func CanonicalInput(in Input) ([]byte, string, error) {
	if err := Validate(in); err != nil {
		return nil, "", err
	}
	b, err := canonical(in)
	if err != nil {
		return nil, "", err
	}
	return b, digest(b), nil
}
func VerdictDigest(v Verdict) (string, error) { b, e := canonical(v); return digest(b), e }
func ShadowDecision(v Verdict) string {
	switch v.Kind + "/" + v.Code {
	case "failed/change_not_open", "failed/hard_guardrail", "hitl/guardrail_violation", "hitl/failure_review":
		return "block"
	case "ready/merge", "ready/no_auto_merge":
		return "allow"
	default:
		return "inconclusive"
	}
}

func Evaluate(in Input) (Verdict, error) {
	if err := Validate(in); err != nil {
		return Verdict{}, err
	}
	v := func(kind, code string) Verdict {
		return Verdict{SchemaVersion: 1, HeadSHA: in.Change.HeadSHA, Kind: kind, Code: code}
	}
	if in.Change.State != "open" {
		x := v("failed", "change_not_open")
		x.ChangeState = in.Change.State
		return x, nil
	}
	if rule, paths, hard := guardrail(in); rule != "" {
		if hard {
			x := v("failed", "hard_guardrail")
			x.RuleID, x.MatchedPaths = rule, paths
			return x, nil
		}
		d := pathsDigest(paths)
		if !exempt(in, rule, d) {
			x := v("hitl", "guardrail_violation")
			x.RuleID, x.MatchedPathsDigest = rule, d
			return x, nil
		}
	}
	switch in.Checks.Conclusion {
	case "pending":
		if *in.Checks.PendingTimedOut {
			x := v("hitl", "checks_timeout")
			x.ExternalURL, x.PendingStartedAtMS = in.Checks.ExternalURL, in.Checks.PendingStartedAtMS
			return x, nil
		}
		x := v("wait_checks", "checks_pending")
		x.ExternalURL, x.PendingStartedAtMS = in.Checks.ExternalURL, in.Checks.PendingStartedAtMS
		return x, nil
	case "unknown":
		x := v("hitl", "input_unknown")
		x.Field, x.Reason = "checks.conclusion", "unknown"
		return x, nil
	case "failure":
		if in.Checks.Triage.Classification == "flaky" && in.Checks.FlakyRetriesUsed < in.EffectivePolicy.FlakyRetryLimit {
			x := v("retry_checks", "flaky_retry")
			x.CheckRunID = *in.Checks.Triage.RetryCheckID
			x.RetryNo = in.Checks.FlakyRetriesUsed + 1
			return x, nil
		}
		x := v("hitl", "failure_review")
		x.ExternalURL, x.Classification = in.Checks.ExternalURL, in.Checks.Triage.Classification
		return x, nil
	}
	if in.EffectivePolicy.ReviewPolicy == config.ReviewPolicyAlways || (in.EffectivePolicy.ReviewPolicy == config.ReviewPolicyRiskyOnly && in.Risk.RiskScore >= in.EffectivePolicy.RiskyReviewThreshold) {
		if in.Change.ReviewState != "approved" {
			x := v("hitl", "code_review")
			x.ReviewPolicy = string(in.EffectivePolicy.ReviewPolicy)
			x.RiskScore = &in.Risk.RiskScore
			return x, nil
		}
	}
	if !in.EffectivePolicy.AutoMerge {
		x := v("ready", "no_auto_merge")
		x.Reason = "policy_disabled"
		return x, nil
	}
	if in.Change.IsDraft {
		x := v("ready", "no_auto_merge")
		x.Reason = "draft"
		return x, nil
	}
	if in.Change.Mergeability == "conflicting" {
		x := v("hitl", "merge_conflict")
		x.Mergeability = "conflicting"
		return x, nil
	}
	if in.Change.Mergeability != "mergeable" {
		x := v("hitl", "mergeability_unknown")
		x.Mergeability = "unknown"
		return x, nil
	}
	x := v("ready", "merge")
	x.ChangeID, x.ExpectedHeadSHA = in.Identity.ChangeID, in.Change.HeadSHA
	return x, nil
}

func Validate(in Input) error {
	if in.SchemaVersion != 1 {
		return errors.New("gate: unsupported schema version")
	}
	for _, s := range []string{in.Identity.RunID, in.Identity.ProjectID, in.Identity.TaskKind, in.Identity.ChangeID, in.Change.BaseRef, in.Change.HeadRef} {
		if s == "" || len(s) > 256 {
			return errors.New("gate: invalid identity or ref")
		}
	}
	if !in.Change.PathsComplete {
		return &AssemblyError{"paths_incomplete", "change.changed_paths"}
	}
	if in.Change.State != "open" && in.Change.State != "closed" && in.Change.State != "merged" {
		return errors.New("gate: invalid change state")
	}
	if !validSHA(in.Change.HeadSHA) {
		return errors.New("gate: invalid head sha")
	}
	if in.Change.FilesChanged != len(in.Change.ChangedPaths) || in.Change.Additions < 0 || in.Change.Deletions < 0 {
		return errors.New("gate: invalid change size")
	}
	if !sortedPaths(in.Change.ChangedPaths) {
		return errors.New("gate: changed paths must be sorted, unique, repo-relative")
	}
	if in.Checks.Conclusion != "success" && in.Checks.Conclusion != "failure" && in.Checks.Conclusion != "pending" && in.Checks.Conclusion != "unknown" {
		return errors.New("gate: invalid checks conclusion")
	}
	if in.Checks.ExternalURL == "" || in.Checks.FlakyRetriesUsed < 0 {
		return errors.New("gate: invalid checks")
	}
	if in.Checks.Conclusion == "pending" {
		if in.Checks.PendingStartedAtMS == nil || in.Checks.ObservedAtMS == nil || in.Checks.PendingTimedOut == nil || *in.Checks.ObservedAtMS < *in.Checks.PendingStartedAtMS {
			return errors.New("gate: invalid pending facts")
		}
	} else if in.Checks.PendingStartedAtMS != nil || in.Checks.ObservedAtMS != nil || in.Checks.PendingTimedOut != nil {
		return errors.New("gate: pending facts outside pending")
	}
	if in.Checks.Conclusion == "failure" {
		if in.Checks.Triage == nil {
			return errors.New("gate: failure requires triage")
		}
		t := in.Checks.Triage
		if t.Classification != "flaky" && t.Classification != "real_failure" && t.Classification != "infrastructure" && t.Classification != "unknown" {
			return errors.New("gate: invalid triage")
		}
		if t.Classification == "flaky" && (t.RetryCheckID == nil || !retryable(*t.RetryCheckID, in.Checks.FailedJobs)) {
			return errors.New("gate: invalid flaky retry")
		}
	} else if in.Checks.Triage != nil {
		return errors.New("gate: triage outside failure")
	}
	if in.Risk.RiskScore < 0 || in.Risk.RiskScore > 100 {
		return errors.New("gate: invalid risk")
	}
	b, e := canonical(in.EffectivePolicy)
	if e != nil {
		return e
	}
	if digest(b) != in.EffectivePolicyHash || !validHash(in.EffectivePolicyHash) || !validHash(in.CertificationRulesVersion) || !validHash(in.CertificationVersion) {
		return errors.New("gate: invalid frozen policy hash")
	}
	return nil
}
func guardrail(in Input) (string, []string, bool) {
	for _, set := range []struct {
		p    []string
		hard bool
	}{{in.EffectivePolicy.ProtectedPaths.Hard, true}, {in.EffectivePolicy.ProtectedPaths.Soft, false}} {
		for _, p := range set.p {
			var hits []string
			for _, x := range in.Change.ChangedPaths {
				if match(p, x) {
					hits = append(hits, x)
				}
			}
			if len(hits) > 0 {
				if set.hard {
					return "hard:" + p, hits, true
				}
				except := false
				for _, e := range in.EffectivePolicy.ProtectedPaths.SoftExceptions {
					for _, x := range hits {
						if match(e, x) {
							except = true
							break
						}
					}
				}
				if !except {
					return "soft:" + p, hits, false
				}
			}
		}
	}
	return "", nil, false
}
func exempt(in Input, rule, d string) bool {
	for _, e := range in.OneTimeExemptions {
		if e.RunID == in.Identity.RunID && e.HeadSHA == in.Change.HeadSHA && e.RuleID == rule && e.MatchedPathsDigest == d {
			return true
		}
	}
	return false
}
func match(pattern, p string) bool { // path.Match supplies * and ?; ** is expanded segment-wise.
	a, b := strings.Split(pattern, "/"), strings.Split(p, "/")
	var f func(int, int) bool
	f = func(i, j int) bool {
		if i == len(a) {
			return j == len(b)
		}
		if a[i] == "**" {
			for k := j; k <= len(b); k++ {
				if f(i+1, k) {
					return true
				}
			}
			return false
		}
		if j == len(b) {
			return false
		}
		ok, e := path.Match(a[i], b[j])
		return e == nil && ok && f(i+1, j+1)
	}
	return f(0, 0)
}
func sortedPaths(xs []string) bool {
	for i, x := range xs {
		if x == "" || strings.HasPrefix(x, "/") || strings.Contains(x, "\\") {
			return false
		}
		for _, s := range strings.Split(x, "/") {
			if s == "" || s == "." || s == ".." {
				return false
			}
		}
		if i > 0 && bytes.Compare([]byte(xs[i-1]), []byte(x)) >= 0 {
			return false
		}
	}
	return true
}
func retryable(id string, j []Job) bool {
	for _, x := range j {
		if x.ID == id && !x.AllowFailure {
			return true
		}
	}
	return false
}
func validSHA(s string) bool  { return (len(s) == 40 || len(s) == 64) && validHex(s) }
func validHash(s string) bool { return len(s) == 64 && validHex(s) }
func validHex(s string) bool  { _, e := hex.DecodeString(s); return e == nil && s == strings.ToLower(s) }
func canonical(v any) ([]byte, error) {
	// Marshal through a generic JSON tree so object keys use encoding/json's
	// lexical map order, matching config/policy canonical JSON rather than Go
	// struct field declaration order.
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, err
	}
	return json.Marshal(tree)
}
func digest(b []byte) string { x := sha256.Sum256(b); return hex.EncodeToString(x[:]) }
func pathsDigest(xs []string) string {
	v := append([]string(nil), xs...)
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	b, _ := canonical(v)
	return digest(b)
}
