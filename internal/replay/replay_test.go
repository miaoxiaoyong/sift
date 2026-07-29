package replay

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/gate"
	"github.com/miaoxiaoyong/sift/internal/policy"
)

func TestReplayGateJSONLUsesFrozenInput(t *testing.T) {
	cert := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	effective, hash, _, _, err := policy.Assemble(policy.Missing(), config.GateDefaults{ReviewPolicy: config.ReviewPolicyNever, RiskyReviewThreshold: 1, AutoMerge: true, ChecksPendingTimeout: time.Hour, FlakyRetryLimit: 1}, "feature", policy.CertificationProjection{TaskKind: "feature", CertificationVersion: cert, Certified: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	in := gate.Input{SchemaVersion: 1, Identity: gate.Identity{RunID: "r", ProjectID: "p", TaskKind: "feature", ChangeID: "42"}, Change: gate.Change{State: "open", HeadSHA: "0123456789012345678901234567890123456789", BaseRef: "main", HeadRef: "sift/r", Mergeability: "mergeable", ReviewState: "approved", PathsComplete: true, ChangedPaths: []string{"cmd/a.go"}, FilesChanged: 1}, Checks: gate.Checks{Conclusion: "success", ExternalURL: "https://ci.example/run"}, EffectivePolicy: effective, EffectivePolicyHash: hash, CertificationRulesVersion: cert, CertificationVersion: cert, Risk: gate.Risk{RiskScore: 1}}
	expected, err := gate.Evaluate(in)
	if err != nil {
		t.Fatal(err)
	}
	record := map[string]any{"record_type": "gate", "record_id": "e1", "input": in, "expected_verdict": expected}
	line, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	report, err := ReplayGateJSONL(bytes.NewReader(append(line, '\n')))
	if err != nil {
		t.Fatal(err)
	}
	if report.Records != 1 || report.Unchanged != 1 || len(report.Deltas) != 0 {
		t.Fatalf("report = %+v", report)
	}
}
