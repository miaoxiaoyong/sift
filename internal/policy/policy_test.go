package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
)

func TestParseClosedPolicyAndPatterns(t *testing.T) {
	valid := `version: 1
protected_paths:
  hard: ["z/**", "a/*"]
  soft: ["docs/?pi.md"]
  soft_exceptions: ["docs/generated/**"]
review_policy: risky-only
risky_review_threshold: 40
auto_merge: true
checks_pending_timeout: 60m
flaky_retry_limit: 2
`
	p, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if p.ChecksPendingTimeout == nil || *p.ChecksPendingTimeout != time.Hour || !*p.AutoMerge {
		t.Fatalf("parsed policy = %#v", p)
	}
	for _, input := range []string{
		"", "version: 1\nunknown: x", "version: 1\nprotected_paths: null", "version: 1\nprotected_paths:\n  hard: [x, x]", "version: 1\nprotected_paths:\n  hard: [/root]", "version: 1\nchecks_pending_timeout: 59s", "version: 2",
	} {
		if _, err := Parse([]byte(input)); err == nil {
			t.Errorf("Parse(%q) succeeded", input)
		}
	}
}

func TestAssembleCanonicalizesAndMonotonicallyNarrows(t *testing.T) {
	base, err := Parse([]byte(`version: 1
protected_paths:
  hard: ["z/**", ".sift/**"]
  soft: ["z/*", "a/*"]
auto_merge: true
checks_pending_timeout: 60m
`))
	if err != nil {
		t.Fatal(err)
	}
	defaults := config.GateDefaults{ReviewPolicy: config.ReviewPolicyAlways, RiskyReviewThreshold: 1, ChecksPendingTimeout: time.Hour, FlakyRetryLimit: 1}
	projection := CertificationProjection{TaskKind: "bugfix", Certified: true, CertificationVersion: strings.Repeat("a", 64)}
	e, hash, version, report, err := Assemble(base, defaults, "bugfix", projection, true)
	if err != nil {
		t.Fatal(err)
	}
	if !e.AutoMerge || report.AutoMerge != AutoMergeEffective || version != projection.CertificationVersion {
		t.Fatalf("qualification = %#v, %q, %#v", e, version, report)
	}
	if got, want := strings.Join(e.ProtectedPaths.Hard, ","), ".github/workflows/**,.gitlab-ci.yml,.sift/**,z/**"; got != want {
		t.Errorf("hard = %q, want %q", got, want)
	}
	if got := strings.Join(e.ProtectedPaths.Soft, ","); got != "a/*,z/*" {
		t.Errorf("soft = %q", got)
	}
	if len(hash) != 64 {
		t.Errorf("hash = %q", hash)
	}
	for _, tc := range []struct {
		name, kind string
		projection CertificationProjection
		cas        bool
		want       AutoMergeQualification
	}{
		{"wrong task kind", "bugfix", CertificationProjection{TaskKind: "feature", Certified: true, CertificationVersion: strings.Repeat("a", 64)}, true, AutoMergeTaskKindUncertified},
		{"bad version", "bugfix", CertificationProjection{TaskKind: "bugfix", Certified: true, CertificationVersion: "bad"}, true, AutoMergeTaskKindUncertified},
		{"no cas", "bugfix", projection, false, AutoMergeForgeCASUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, _, report, err := Assemble(base, defaults, tc.kind, tc.projection, tc.cas)
			if err != nil || e.AutoMerge || report.AutoMerge != tc.want {
				t.Fatalf("effective=%#v report=%#v err=%v", e, report, err)
			}
		})
	}
}

func TestFreezeInputBindsPolicyHashAndCertificationVersions(t *testing.T) {
	defaults := config.GateDefaults{ReviewPolicy: config.ReviewPolicyAlways, RiskyReviewThreshold: 1, ChecksPendingTimeout: time.Hour, FlakyRetryLimit: 1}
	effective, hash, version, _, err := Assemble(Missing(), defaults, "bugfix", CertificationProjection{TaskKind: "bugfix", CertificationVersion: strings.Repeat("b", 64)}, true)
	if err != nil {
		t.Fatal(err)
	}
	rulesVersion := strings.Repeat("c", 64)
	frozen, err := FreezeInput(effective, hash, rulesVersion, version)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.EffectivePolicyHash != hash || frozen.CertificationRulesVersion != rulesVersion || frozen.CertificationVersion != version {
		t.Fatalf("frozen input = %#v", frozen)
	}
	if _, err := FreezeInput(effective, strings.Repeat("0", 64), rulesVersion, version); err == nil {
		t.Fatal("FreezeInput accepted a mismatched policy hash")
	}
	if _, err := FreezeInput(effective, hash, "bad", version); err == nil {
		t.Fatal("FreezeInput accepted an invalid certification rules version")
	}
}

func TestMissingUsesDefaultsAndNotRequested(t *testing.T) {
	defaults := config.GateDefaults{ReviewPolicy: config.ReviewPolicyAlways, RiskyReviewThreshold: 1, ChecksPendingTimeout: time.Hour, FlakyRetryLimit: 1}
	e, _, _, report, err := Assemble(Missing(), defaults, "bugfix", CertificationProjection{TaskKind: "bugfix", CertificationVersion: strings.Repeat("b", 64)}, true)
	if err != nil {
		t.Fatal(err)
	}
	if e.AutoMerge || report.AutoMerge != AutoMergeNotRequested || e.ChecksPendingTimeoutMS != 3600000 {
		t.Fatalf("effective=%#v report=%#v", e, report)
	}
}
