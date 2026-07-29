// Package policy validates project policy files and assembles the immutable
// policy consumed by Gate. It deliberately has no Forge, git, database, clock,
// or Gate dependency.
package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/decode"
)

const Version = 1

var BuiltinHardRules = []string{".github/workflows/**", ".gitlab-ci.yml", ".sift/**"}

type rawPolicy struct {
	Version              *int                 `json:"version" sift:"required"`
	ProtectedPaths       *rawProtectedPaths   `json:"protected_paths,omitempty"`
	ReviewPolicy         *config.ReviewPolicy `json:"review_policy,omitempty"`
	RiskyReviewThreshold *int                 `json:"risky_review_threshold,omitempty"`
	AutoMerge            *bool                `json:"auto_merge,omitempty"`
	ChecksPendingTimeout *string              `json:"checks_pending_timeout,omitempty"`
	FlakyRetryLimit      *int                 `json:"flaky_retry_limit,omitempty"`
}

type rawProtectedPaths struct {
	Hard           *[]string `json:"hard,omitempty"`
	Soft           *[]string `json:"soft,omitempty"`
	SoftExceptions *[]string `json:"soft_exceptions,omitempty"`
}

// BasePolicy retains presence information needed by doctor to distinguish an
// inherited value from an explicit same-as-default override.
type BasePolicy struct {
	Hard, Soft, SoftExceptions []string
	ReviewPolicy               *config.ReviewPolicy
	RiskyReviewThreshold       *int
	AutoMerge                  *bool
	ChecksPendingTimeout       *time.Duration
	FlakyRetryLimit            *int
}

// Parse validates a present .sift/policy.yaml. Missing files are represented
// by Missing(), never by an empty document.
func Parse(data []byte) (BasePolicy, error) {
	jsonBytes, err := config.YAMLToJSON(data)
	if err != nil {
		return BasePolicy{}, fmt.Errorf("policy: YAML: %w", err)
	}
	var tree map[string]any
	if err := json.Unmarshal(jsonBytes, &tree); err != nil {
		return BasePolicy{}, fmt.Errorf("policy: decode YAML JSON: %w", err)
	}
	if err := rejectNulls(tree); err != nil {
		return BasePolicy{}, err
	}
	var raw rawPolicy
	if err := decode.Decode(jsonBytes, &raw, decode.Closed); err != nil {
		return BasePolicy{}, fmt.Errorf("policy: decode: %w", err)
	}
	if *raw.Version != Version {
		return BasePolicy{}, fmt.Errorf("policy: version must be %d", Version)
	}
	p := BasePolicy{ReviewPolicy: raw.ReviewPolicy, RiskyReviewThreshold: raw.RiskyReviewThreshold, AutoMerge: raw.AutoMerge, FlakyRetryLimit: raw.FlakyRetryLimit}
	if raw.ProtectedPaths != nil {
		if raw.ProtectedPaths.Hard != nil {
			p.Hard = append([]string(nil), (*raw.ProtectedPaths.Hard)...)
		}
		if raw.ProtectedPaths.Soft != nil {
			p.Soft = append([]string(nil), (*raw.ProtectedPaths.Soft)...)
		}
		if raw.ProtectedPaths.SoftExceptions != nil {
			p.SoftExceptions = append([]string(nil), (*raw.ProtectedPaths.SoftExceptions)...)
		}
	}
	if raw.ChecksPendingTimeout != nil {
		d, err := time.ParseDuration(*raw.ChecksPendingTimeout)
		if err != nil || d < time.Minute || d > 24*time.Hour {
			return BasePolicy{}, errors.New("policy: checks_pending_timeout must be a duration from 1m through 24h")
		}
		p.ChecksPendingTimeout = &d
	}
	if err := p.validate(); err != nil {
		return BasePolicy{}, err
	}
	return p, nil
}

// Missing returns the normalized policy for an absent file.
func Missing() BasePolicy { return BasePolicy{} }

func (p BasePolicy) validate() error {
	if p.RiskyReviewThreshold != nil && (*p.RiskyReviewThreshold < 0 || *p.RiskyReviewThreshold > 100) {
		return errors.New("policy: risky_review_threshold must be 0..100")
	}
	if p.FlakyRetryLimit != nil && (*p.FlakyRetryLimit < 0 || *p.FlakyRetryLimit > 10) {
		return errors.New("policy: flaky_retry_limit must be 0..10")
	}
	for _, set := range [][]string{p.Hard, p.Soft, p.SoftExceptions} {
		if len(set) > 256 {
			return errors.New("policy: path rule list exceeds 256 entries")
		}
		seen := make(map[string]struct{}, len(set))
		for _, pattern := range set {
			if err := validatePattern(pattern); err != nil {
				return err
			}
			if _, ok := seen[pattern]; ok {
				return fmt.Errorf("policy: duplicate path pattern %q", pattern)
			}
			seen[pattern] = struct{}{}
		}
	}
	return nil
}

func rejectNulls(v any) error {
	switch x := v.(type) {
	case nil:
		return errors.New("policy: null is not a valid value")
	case map[string]any:
		for _, value := range x {
			if err := rejectNulls(value); err != nil {
				return err
			}
		}
	case []any:
		for _, value := range x {
			if err := rejectNulls(value); err != nil {
				return err
			}
		}
	}
	return nil
}

func validatePattern(p string) error {
	if len(p) == 0 || len(p) > 1024 || !utf8.ValidString(p) || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || strings.ContainsRune(p, '\x00') || strings.Contains(p, "\\") {
		return fmt.Errorf("policy: invalid path pattern %q", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." || (strings.Contains(part, "**") && part != "**") {
			return fmt.Errorf("policy: invalid path pattern %q", p)
		}
	}
	return nil
}

// CertificationProjection is the frozen, task-kind-specific Ledger result.
type CertificationProjection struct {
	TaskKind, CertificationVersion string
	Certified                      bool
}

// QualificationReport is explanatory only; it must never be passed to Gate.
type QualificationReport struct {
	AutoMerge AutoMergeQualification `json:"auto_merge"`
}
type AutoMergeQualification string

const (
	AutoMergeNotRequested        AutoMergeQualification = "not_requested"
	AutoMergeTaskKindUncertified AutoMergeQualification = "task_kind_uncertified"
	AutoMergeForgeCASUnavailable AutoMergeQualification = "forge_cas_unavailable"
	AutoMergeEffective           AutoMergeQualification = "effective"
)

// EffectivePolicyV1 is the sole policy shape Gate may consume.
type EffectivePolicyV1 struct {
	SchemaVersion          int                 `json:"schema_version"`
	ProtectedPaths         ProtectedPaths      `json:"protected_paths"`
	ReviewPolicy           config.ReviewPolicy `json:"review_policy"`
	RiskyReviewThreshold   int                 `json:"risky_review_threshold"`
	AutoMerge              bool                `json:"auto_merge"`
	ChecksPendingTimeoutMS int64               `json:"checks_pending_timeout_ms"`
	FlakyRetryLimit        int                 `json:"flaky_retry_limit"`
}
type ProtectedPaths struct {
	Hard           []string `json:"hard"`
	Soft           []string `json:"soft"`
	SoftExceptions []string `json:"soft_exceptions"`
}

// Assemble applies defaults then monotonically narrows privileged settings.
func Assemble(base BasePolicy, defaults config.GateDefaults, taskKind string, certification CertificationProjection, forgeCAS bool) (EffectivePolicyV1, string, string, QualificationReport, error) {
	if err := base.validate(); err != nil {
		return EffectivePolicyV1{}, "", "", QualificationReport{}, err
	}
	if taskKind == "" {
		return EffectivePolicyV1{}, "", "", QualificationReport{}, errors.New("policy: task kind is required")
	}
	if defaults.RiskyReviewThreshold < 0 || defaults.RiskyReviewThreshold > 100 || defaults.FlakyRetryLimit < 0 || defaults.FlakyRetryLimit > 10 || defaults.ChecksPendingTimeout < time.Minute || defaults.ChecksPendingTimeout > 24*time.Hour {
		return EffectivePolicyV1{}, "", "", QualificationReport{}, errors.New("policy: invalid gate defaults")
	}
	e := EffectivePolicyV1{SchemaVersion: Version, ReviewPolicy: defaults.ReviewPolicy, RiskyReviewThreshold: defaults.RiskyReviewThreshold, AutoMerge: defaults.AutoMerge, ChecksPendingTimeoutMS: defaults.ChecksPendingTimeout.Milliseconds(), FlakyRetryLimit: defaults.FlakyRetryLimit, ProtectedPaths: ProtectedPaths{Hard: union(BuiltinHardRules, base.Hard), Soft: sorted(base.Soft), SoftExceptions: sorted(base.SoftExceptions)}}
	if base.ReviewPolicy != nil {
		e.ReviewPolicy = *base.ReviewPolicy
	}
	if base.RiskyReviewThreshold != nil {
		e.RiskyReviewThreshold = *base.RiskyReviewThreshold
	}
	if base.AutoMerge != nil {
		e.AutoMerge = *base.AutoMerge
	}
	if base.ChecksPendingTimeout != nil {
		e.ChecksPendingTimeoutMS = base.ChecksPendingTimeout.Milliseconds()
	}
	if base.FlakyRetryLimit != nil {
		e.FlakyRetryLimit = *base.FlakyRetryLimit
	}
	report := QualificationReport{AutoMerge: AutoMergeNotRequested}
	if e.AutoMerge {
		switch {
		case certification.TaskKind != taskKind || !certification.Certified || !validVersion(certification.CertificationVersion):
			report.AutoMerge = AutoMergeTaskKindUncertified
			e.AutoMerge = false
		case !forgeCAS:
			report.AutoMerge = AutoMergeForgeCASUnavailable
			e.AutoMerge = false
		default:
			report.AutoMerge = AutoMergeEffective
		}
	}
	canonical, err := canonicalJSON(e)
	if err != nil {
		return EffectivePolicyV1{}, "", "", QualificationReport{}, err
	}
	sum := sha256.Sum256(canonical)
	return e, hex.EncodeToString(sum[:]), certification.CertificationVersion, report, nil
}

func validVersion(v string) bool {
	if len(v) != 64 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}
func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Slice(out, func(i, j int) bool { return bytes.Compare([]byte(out[i]), []byte(out[j])) < 0 })
	return out
}
func union(a, b []string) []string {
	seen := map[string]struct{}{}
	for _, v := range append(append([]string(nil), a...), b...) {
		seen[v] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	return sorted(out)
}
func canonicalJSON(v any) ([]byte, error) {
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
