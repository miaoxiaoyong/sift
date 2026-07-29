package brain

import (
	"strings"
	"testing"
)

func TestT3ContractAndFallback(t *testing.T) {
	valid := `{"risk_score":7,"risk_points":["a","z"],"rationale":"low risk"}`
	if _, err := T3Contract().ValidateOutput([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"risk_score":101,"risk_points":[],"rationale":"x"}`,
		`{"risk_score":1,"risk_points":["z","a"],"rationale":"x"}`,
		`{"risk_score":1,"risk_points":["a","a"],"rationale":"x"}`,
		`{"risk_score":1,"risk_points":[],"rationale":"x","verdict":"merge"}`,
	} {
		if _, err := T3Contract().ValidateOutput([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid T3 output %s", raw)
		}
	}
	out, source, err := T3ResultFromCall(CallResult{CallID: "call-1", Status: "fallback", FallbackReason: "attempts exhausted: invalid_output"})
	if err != nil || *out.RiskScore != 100 || source.Kind != "fallback" || source.Reason != "invalid_output" {
		t.Fatalf("fallback = %#v %#v, %v", out, source, err)
	}
}

func TestBuildT3Input(t *testing.T) {
	in := T3Input{RunID: "run-1", TaskKind: TaskBug, Change: T3Change{ChangeInput: ChangeInput{ID: "42", URL: "https://example.test/pr/42", HeadSHA: strings.Repeat("a", 40)}, Diff: "diff --git a/a b/a"}}
	got, err := BuildT3Input(in)
	if err != nil || !strings.Contains(string(got), `"head_sha"`) {
		t.Fatalf("BuildT3Input = %s, %v", got, err)
	}
	in.Change.URL = "http://example.test/pr/42"
	if _, err := BuildT3Input(in); err == nil {
		t.Fatal("non-HTTPS URL accepted")
	}
}

func TestT5ContractAndFallback(t *testing.T) {
	jobs := []T5Job{{ID: "job-1", Name: "test", WebURL: "https://ci.test/1"}, {ID: "job-2", Name: "optional", WebURL: "https://ci.test/2", AllowFailure: true}}
	if _, err := T5Contract(jobs).ValidateOutput([]byte(`{"classification":"flaky","retry_check_id":"job-1","rationale":"transient"}`)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"classification":"flaky","retry_check_id":"job-2","rationale":"x"}`,
		`{"classification":"real_failure","retry_check_id":"job-1","rationale":"x"}`,
		`{"classification":"real_failure","retry_check_id":"","rationale":"x"}`,
		`{"classification":"infrastructure","retry_check_id":null,"rationale":" "}`,
		`{"classification":"unknown","retry_check_id":null,"rationale":"x"}`,
	} {
		if _, err := T5Contract(jobs).ValidateOutput([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid T5 output %s", raw)
		}
	}
	triage, err := T5TriageFromCall(CallResult{CallID: "call-5", Status: "fallback", FallbackReason: "token_budget_exceeded"})
	if err != nil || triage.Classification != "unknown" || triage.Source.Reason != "token_threshold" || triage.RetryCheckID != nil {
		t.Fatalf("triage = %#v, %v", triage, err)
	}
}

func TestBuildT5InputCanonicalizesJobs(t *testing.T) {
	in := T5Input{RunID: "run-1", Change: ChangeInput{ID: "42", URL: "https://example.test/pr/42", HeadSHA: strings.Repeat("a", 64)}, Checks: T5Checks{ExternalURL: "https://ci.test/run", FailedJobs: []T5Job{{ID: "b", Name: "b", WebURL: "https://ci.test/b"}, {ID: "a", Name: "a", WebURL: "https://ci.test/a"}}}}
	got, err := BuildT5Input(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(string(got), `"id":"a"`) > strings.Index(string(got), `"id":"b"`) {
		t.Fatalf("jobs not sorted: %s", got)
	}
	in.Checks.FailedJobs = append(in.Checks.FailedJobs, T5Job{ID: "a", Name: "other", WebURL: "https://ci.test/other"})
	if _, err := BuildT5Input(in); err == nil {
		t.Fatal("duplicate job ID accepted")
	}
}
