package replay

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/xsift/sift/internal/brain"
	"github.com/xsift/sift/internal/config"
	"github.com/xsift/sift/internal/gate"
	"github.com/xsift/sift/internal/policy"
)

func TestReplayBrainJSONLReplaysValidAndFallbackT3(t *testing.T) {
	result := `{"risk_score":7,"risk_points":["small diff"],"rationale":"bounded"}`
	raw, err := json.Marshal(map[string]any{"result_text": result, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}})
	if err != nil {
		t.Fatal(err)
	}
	validated, err := brain.T3Contract().ValidateOutput([]byte(result))
	if err != nil {
		t.Fatal(err)
	}
	valid := map[string]any{
		"record_type": "brain_call", "record_id": "t3-valid", "touchpoint": "T3",
		"prompt_version": brain.T3Asset().PromptVersion, "output_schema_version": brain.T3Asset().OutputSchemaVersion,
		"status": "valid", "selected_attempt_no": 1, "validated_output": json.RawMessage(validated),
		"attempts": []map[string]any{{"provider_attempt": 1, "raw_output": string(raw)}},
	}
	fallback := map[string]any{
		"record_type": "brain_call", "record_id": "t3-fallback", "touchpoint": "T3",
		"prompt_version": brain.T3Asset().PromptVersion, "output_schema_version": brain.T3Asset().OutputSchemaVersion,
		"status": "fallback", "selected_attempt_no": nil, "validated_output": nil,
		"attempts": []any{},
	}
	validLine, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	fallbackLine, err := json.Marshal(fallback)
	if err != nil {
		t.Fatal(err)
	}
	report, err := ReplayBrainJSONL(bytes.NewReader(append(append(validLine, '\n'), append(fallbackLine, '\n')...)), map[string]brain.TouchpointContract{"T3": brain.T3Contract()})
	if err != nil {
		t.Fatal(err)
	}
	if report.Records != 2 || report.Validated != 1 || report.Fallbacks != 1 {
		t.Fatalf("report = %+v", report)
	}
}

func TestReplayBrainJSONLFailsClosedForT4WithoutContract(t *testing.T) {
	line := []byte(`{"record_type":"brain_call","record_id":"t4-1","touchpoint":"T4","prompt_version":"T4/v1","output_schema_version":1,"status":"fallback","attempts":[]}` + "\n")
	if _, err := ReplayBrainJSONL(bytes.NewReader(line), map[string]brain.TouchpointContract{}); err == nil {
		t.Fatal("missing T4 contract replay succeeded")
	}
}

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
