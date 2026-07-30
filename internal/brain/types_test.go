package brain

import (
	"errors"
	"strings"
	"testing"

	"github.com/miaoxiaoyong/sift/internal/schema"
)

// T1/T2 contract tests (brain.md §7/§8, §10.3): unknown fields, wrong types,
// bad enums, fenced/trailing JSON are all rejected without repair; the mutex
// matrix, content rules and runtime-fact checks (candidate membership) hold.

func validateT1(t *testing.T, resultText string, candidates ...string) error {
	t.Helper()
	_, err := T1Contract(candidates).ValidateOutput([]byte(resultText))
	return err
}

func TestT1OutputValid(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{name: "ready", json: `{"disposition":"ready","questions":[],"possible_duplicate_run_id":null,"rationale":""}`},
		{name: "clarify", json: `{"disposition":"needs_clarification","questions":["what version?"],"possible_duplicate_run_id":null,"rationale":"missing info"}`},
		{name: "duplicate", json: `{"disposition":"possible_duplicate","questions":[],"possible_duplicate_run_id":"run-1","rationale":"same issue"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			canonical, err := T1Contract([]string{"run-1"}).ValidateOutput([]byte(tc.json))
			if err != nil {
				t.Fatalf("ValidateOutput: %v", err)
			}
			if !strings.Contains(string(canonical), `"disposition"`) {
				t.Fatalf("canonical = %s", canonical)
			}
		})
	}
}

func TestT1OutputRejected(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{name: "unknown_field", json: `{"disposition":"ready","questions":[],"possible_duplicate_run_id":null,"rationale":"","extra":1}`},
		{name: "wrong_type", json: `{"disposition":"ready","questions":"none","possible_duplicate_run_id":null,"rationale":""}`},
		{name: "bad_enum", json: `{"disposition":"maybe","questions":[],"possible_duplicate_run_id":null,"rationale":""}`},
		{name: "missing_dup_key", json: `{"disposition":"ready","questions":[],"rationale":""}`},
		{name: "missing_rationale", json: `{"disposition":"ready","questions":[],"possible_duplicate_run_id":null}`},
		{name: "matrix_ready_with_questions", json: `{"disposition":"ready","questions":["q"],"possible_duplicate_run_id":null,"rationale":""}`},
		{name: "matrix_ready_with_dup", json: `{"disposition":"ready","questions":[],"possible_duplicate_run_id":"run-1","rationale":""}`},
		{name: "matrix_clarify_no_questions", json: `{"disposition":"needs_clarification","questions":[],"possible_duplicate_run_id":null,"rationale":""}`},
		{name: "matrix_clarify_with_dup", json: `{"disposition":"needs_clarification","questions":["q"],"possible_duplicate_run_id":"run-1","rationale":""}`},
		{name: "matrix_dup_no_id", json: `{"disposition":"possible_duplicate","questions":[],"possible_duplicate_run_id":null,"rationale":""}`},
		{name: "matrix_dup_with_questions", json: `{"disposition":"possible_duplicate","questions":["q"],"possible_duplicate_run_id":"run-1","rationale":""}`},
		{name: "too_many_questions", json: `{"disposition":"needs_clarification","questions":["1","2","3","4","5","6"],"possible_duplicate_run_id":null,"rationale":""}`},
		{name: "untrimmed_question", json: `{"disposition":"needs_clarification","questions":[" q "],"possible_duplicate_run_id":null,"rationale":""}`},
		{name: "duplicate_questions", json: `{"disposition":"needs_clarification","questions":["q","q"],"possible_duplicate_run_id":null,"rationale":""}`},
		{name: "rationale_too_long", json: `{"disposition":"ready","questions":[],"possible_duplicate_run_id":null,"rationale":"` + strings.Repeat("r", 2001) + `"}`},
		{name: "unknown_candidate", json: `{"disposition":"possible_duplicate","questions":[],"possible_duplicate_run_id":"run-999","rationale":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateT1(t, tc.json, "run-1"); err == nil {
				t.Fatal("must be rejected")
			}
		})
	}
}

func TestT1FallbackShape(t *testing.T) {
	fallback := T1FallbackOutput()
	canonical, err := T1Contract(nil).ValidateOutput(fallback)
	if err != nil {
		t.Fatalf("fallback must itself satisfy the output contract: %v", err)
	}
	if string(canonical) != string(fallback) {
		t.Fatalf("fallback not canonical: %s vs %s", canonical, fallback)
	}
	var doc map[string]any
	if err := schema.Decode(fallback, &doc, schema.OpenEnvelope); err != nil {
		t.Fatal(err)
	}
	if doc["disposition"] != "ready" || doc["rationale"] != "fallback" {
		t.Fatalf("fallback = %s", fallback)
	}
	if v, ok := doc["possible_duplicate_run_id"]; !ok || v != nil {
		t.Fatalf("duplicate must be present and null: %v", doc)
	}
}

func TestBuildT1Input(t *testing.T) {
	in := T1Input{
		Forge: T1Forge{Kind: "github", Host: "github.com", ProjectKey: "org/repo"},
		Issue: T1Issue{ID: "42", Title: "t", Body: "b", Author: "a", URL: "u", Labels: []string{"z", "a", "z"}},
		KnownCandidates: []T1Candidate{
			{RunID: "run-2", IssueID: "2", Title: "x", Status: "done"},
			{RunID: "run-1", IssueID: "1", Title: "y", Status: "queued"},
		},
	}
	canonical, err := BuildT1Input(in)
	if err != nil {
		t.Fatalf("BuildT1Input: %v", err)
	}
	s := string(canonical)
	if !strings.Contains(s, `"labels":["a","z"]`) {
		t.Fatalf("labels not sorted/deduped: %s", s)
	}
	if strings.Index(s, "run-1") > strings.Index(s, "run-2") {
		t.Fatalf("candidates not sorted: %s", s)
	}
	// Deterministic.
	again, _ := BuildT1Input(in)
	if string(again) != s {
		t.Fatal("input canonicalization must be deterministic")
	}

	in.Forge.Kind = "bitbucket"
	if _, err := BuildT1Input(in); err == nil {
		t.Fatal("bad forge kind must fail")
	}
}

func validateT2(t *testing.T, resultText string, candidates ...string) error {
	t.Helper()
	_, err := T2Contract(candidates).ValidateOutput([]byte(resultText))
	return err
}

func TestT2OutputValid(t *testing.T) {
	canonical, err := T2Contract([]string{"claude-code"}).ValidateOutput([]byte(
		`{"kind":"bug","agent":"claude-code","hitl_before_start":true,"goals":["fix it","add test"],"risk_notes":"","rationale":"r"}`))
	if err != nil {
		t.Fatalf("ValidateOutput: %v", err)
	}
	if !strings.Contains(string(canonical), `"hitl_before_start":true`) {
		t.Fatalf("canonical = %s", canonical)
	}
}

func TestT2OutputRejected(t *testing.T) {
	base := func(mut string) string {
		return `{"kind":"bug","agent":"claude-code","hitl_before_start":false,"goals":["g"],"risk_notes":"","rationale":""` + mut + `}`
	}
	cases := []struct {
		name string
		json string
	}{
		{name: "unknown_field_guardrails", json: base(`,"guardrails":[]`)},
		{name: "unknown_field_max_attempts", json: base(`,"max_attempts":3`)},
		{name: "bad_kind", json: `{"kind":"epic","agent":"claude-code","hitl_before_start":false,"goals":["g"],"risk_notes":"","rationale":""}`},
		{name: "missing_hitl", json: `{"kind":"bug","agent":"claude-code","goals":["g"],"risk_notes":"","rationale":""}`},
		{name: "empty_goals", json: `{"kind":"bug","agent":"claude-code","hitl_before_start":false,"goals":[],"risk_notes":"","rationale":""}`},
		{name: "too_many_goals", json: `{"kind":"bug","agent":"claude-code","hitl_before_start":false,"goals":["1","2","3","4","5","6","7","8","9","10","11"],"risk_notes":"","rationale":""}`},
		{name: "untrimmed_goal", json: `{"kind":"bug","agent":"claude-code","hitl_before_start":false,"goals":[" g"],"risk_notes":"","rationale":""}`},
		{name: "duplicate_goals", json: `{"kind":"bug","agent":"claude-code","hitl_before_start":false,"goals":["g","g"],"risk_notes":"","rationale":""}`},
		{name: "noncandidate_agent", json: `{"kind":"bug","agent":"not-a-candidate","hitl_before_start":false,"goals":["g"],"risk_notes":"","rationale":""}`},
		{name: "agent_too_long", json: `{"kind":"bug","agent":"` + strings.Repeat("a", 65) + `","hitl_before_start":false,"goals":["g"],"risk_notes":"","rationale":""}`},
		{name: "goals_total_too_long", json: `{"kind":"bug","agent":"claude-code","hitl_before_start":false,"goals":["` + strings.Repeat("g", 1000) + `","` + strings.Repeat("h", 1000) + `","` + strings.Repeat("i", 1000) + `","` + strings.Repeat("j", 1000) + `","` + strings.Repeat("k", 1000) + `","` + strings.Repeat("l", 1000) + `","` + strings.Repeat("m", 1000) + `","` + strings.Repeat("n", 1000) + `","` + strings.Repeat("o", 1000) + `"],"risk_notes":"","rationale":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateT2(t, tc.json, "claude-code"); err == nil {
				t.Fatal("must be rejected")
			}
		})
	}
}

func TestT2EffectiveHITL(t *testing.T) {
	// The deterministic force can never be overridden by an LLM false (§8.3).
	if !EffectiveHITL(false, true) {
		t.Fatal("forced HITL must win over LLM false")
	}
	if !EffectiveHITL(true, false) || EffectiveHITL(false, false) {
		t.Fatal("OR semantics broken")
	}
}

func TestBuildT2Input(t *testing.T) {
	in := T2Input{
		RunID: "run-1",
		Issue: T2Issue{Title: "t", Body: "b", URL: "u"},
		CandidateAgents: []T2AgentCandidate{
			{ID: "b-agent", Capabilities: []string{"go"}},
			{ID: "a-agent", Capabilities: nil},
		},
		BaseContext: T2BaseContext{ProjectContext: "p", GlobalContext: "g"},
	}
	canonical, err := BuildT2Input(in)
	if err != nil {
		t.Fatalf("BuildT2Input: %v", err)
	}
	s := string(canonical)
	if strings.Index(s, "a-agent") > strings.Index(s, "b-agent") {
		t.Fatalf("candidates not sorted by id: %s", s)
	}
	in.CandidateAgents = nil
	if _, err := BuildT2Input(in); err == nil {
		t.Fatal("1..32 candidate bound must fail on empty")
	}
}

// T1 disposition schema-level matrix check must reject what runtime rejects;
// this anchors that the decode path surfaces a DecodeError kind callers can
// map to schema failure.
func TestT1UnknownFieldKind(t *testing.T) {
	err := validateT1(t, `{"disposition":"ready","questions":[],"possible_duplicate_run_id":null,"rationale":"","bogus":true}`)
	var de *schema.DecodeError
	if !errors.As(err, &de) || de.Kind != schema.KindUnknownField {
		t.Fatalf("kind = %v", err)
	}
}
