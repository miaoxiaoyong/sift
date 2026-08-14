package brain

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/xsift/sift/internal/schema"
	"github.com/xsift/sift/internal/storage"
)

func issue415T7Input(key, project string, kinds []TaskKind) T7Input {
	digest := strings.Repeat("a", 64)
	return T7Input{AggregateKey: key, Window: T7Window{StartMS: 1, EndMS: 2}, Categories: []T7CategoryEvidence{{EvidenceID: "cat", TaskKind: TaskBug, CertificationVersion: digest, EvidenceSummary: T7EvidenceSummary{WindowStartMS: 1, WindowEndMS: 2, CertificationRulesVersion: digest, EvidenceDigest: digest}}}, ReplaySummary: T7ReplaySummary{EvidenceID: "replay", DatasetVersion: "v1", GateVersion: "gate/v1"}, SemanticMaterial: []T7SemanticMaterial{}, TraceProjectID: project, AllCategoryKinds: kinds}
}

func TestIssue415ReasonsUseStorageCanonicalSet(t *testing.T) {
	for _, reason := range storage.ActiveInterruptReasons() {
		t4 := t4Input()
		t4.Interrupt.Reason = InterruptReason(reason)
		if _, err := BuildT4Input(t4); err != nil {
			t.Fatalf("T4 rejected %q: %v", reason, err)
		}
		t6 := t6Input()
		t6.Candidate.Reason = InterruptReason(reason)
		if _, err := BuildT6Input(t6); err != nil {
			t.Fatalf("T6 rejected %q: %v", reason, err)
		}
	}
	for _, reason := range []InterruptReason{"human_input", "merge_approval", "policy_block", "rate_limited", "run_stalled"} {
		t4 := t4Input()
		t4.Interrupt.Reason = reason
		if _, err := BuildT4Input(t4); err == nil {
			t.Fatalf("T4 accepted retired %q", reason)
		}
		t6 := t6Input()
		t6.Candidate.Reason = reason
		if _, err := BuildT6Input(t6); err == nil {
			t.Fatalf("T6 accepted retired %q", reason)
		}
	}
}

func issue436T7JSON(t *testing.T, in T7Input) []byte {
	t.Helper()
	b, err := schema.Canonical(in)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func issue436T7Without(t *testing.T, input []byte, path ...string) []byte {
	t.Helper()
	var value any
	if err := json.Unmarshal(input, &value); err != nil {
		t.Fatal(err)
	}
	for i, part := range path {
		switch current := value.(type) {
		case map[string]any:
			if i == len(path)-1 {
				delete(current, part)
				continue
			}
			value = current[part]
		case []any:
			index := 0
			if _, err := fmt.Sscanf(part, "%d", &index); err != nil || index < 0 || index >= len(current) {
				t.Fatalf("invalid T7 test path %q", strings.Join(path, "."))
			}
			value = current[index]
		default:
			t.Fatalf("invalid T7 test path %q", strings.Join(path, "."))
		}
	}
	b, err := schema.Canonical(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func issue436TraceCounts(t *testing.T, db *storage.DB) (calls, attempts int) {
	t.Helper()
	var replay bytes.Buffer
	if err := db.ExportBrainCallsJSONL(context.Background(), &replay); err != nil {
		t.Fatal(err)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(replay.Bytes()), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var record struct {
			Attempts []json.RawMessage `json:"attempts"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		calls++
		attempts += len(record.Attempts)
	}
	return calls, attempts
}

func TestIssue436T7MalformedInputsDoNotReserveOrCallProvider(t *testing.T) {
	const globalAll = "aggregate:v1:global:all:1:2"
	project := "project-7"
	projectKey := "aggregate:v1:project:" + base64.RawURLEncoding.EncodeToString([]byte(project)) + ":bug:1:2"
	base := func() T7Input { return issue415T7Input(globalAll, "", []TaskKind{TaskBug}) }
	canonical := func(in T7Input) []byte {
		b, err := BuildT7Input(in)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	params := func(input []byte) CallParams {
		return CallParams{Scope: storage.BrainScopeAggregate, SubjectKey: globalAll, Input: input}
	}
	secondCategory := func(kind TaskKind, id string) T7CategoryEvidence {
		c := base().Categories[0]
		c.TaskKind, c.EvidenceID = kind, id
		return c
	}

	valid := canonical(base())
	projectInput := canonical(issue415T7Input(projectKey, project, nil))
	concrete := issue415T7Input("aggregate:v1:global:bug:1:2", "", nil)
	concrete.Categories[0].TaskKind = TaskFeature
	allMissing := base()
	allMissing.Categories = []T7CategoryEvidence{secondCategory(TaskBug, "cat")}
	allExtra := base()
	allExtra.Categories = []T7CategoryEvidence{secondCategory(TaskBug, "cat"), secondCategory(TaskChore, "cat-chore"), secondCategory(TaskDocs, "cat-docs")}

	cases := []struct {
		name     string
		contract TouchpointContract
		params   CallParams
	}{
		{"required_missing", T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), params([]byte(`{"aggregate_key":"aggregate:v1:global:all:1:2"}`))},
		{"category_negative_count", T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), func() CallParams {
			in := base()
			in.Categories[0].EvidenceSummary.TotalSamples = -1
			return params(issue436T7JSON(t, in))
		}()},
		{"category_negative_samples_negative", T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), func() CallParams {
			in := base()
			in.Categories[0].EvidenceSummary.NegativeSamples = -1
			return params(issue436T7JSON(t, in))
		}()},
		{"category_leak_count_negative", T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), func() CallParams {
			in := base()
			in.Categories[0].EvidenceSummary.LeakCount = -1
			return params(issue436T7JSON(t, in))
		}()},
		{"category_false_block_count_negative", T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), func() CallParams {
			in := base()
			in.Categories[0].EvidenceSummary.FalseBlockCount = -1
			return params(issue436T7JSON(t, in))
		}()},
		{"category_negative_exceeds_total", T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), func() CallParams {
			in := base()
			in.Categories[0].EvidenceSummary.NegativeSamples = 1
			return params(issue436T7JSON(t, in))
		}()},
		{"category_leaks_exceed_negative", T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), func() CallParams {
			in := base()
			in.Categories[0].EvidenceSummary.NegativeSamples, in.Categories[0].EvidenceSummary.LeakCount = 1, 2
			return params(issue436T7JSON(t, in))
		}()},
		{"category_false_blocks_exceed_remainder", T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), func() CallParams {
			in := base()
			in.Categories[0].EvidenceSummary.TotalSamples, in.Categories[0].EvidenceSummary.FalseBlockCount = 1, 2
			return params(issue436T7JSON(t, in))
		}()},
		{"replay_negative_count", T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), func() CallParams {
			in := base()
			in.ReplaySummary.TotalSamples = -1
			return params(issue436T7JSON(t, in))
		}()},
		{"replay_negative_samples_negative", T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), func() CallParams {
			in := base()
			in.ReplaySummary.NegativeSamples = -1
			return params(issue436T7JSON(t, in))
		}()},
		{"replay_leak_count_negative", T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), func() CallParams {
			in := base()
			in.ReplaySummary.LeakCount = -1
			return params(issue436T7JSON(t, in))
		}()},
		{"replay_false_block_count_negative", T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), func() CallParams {
			in := base()
			in.ReplaySummary.FalseBlockCount = -1
			return params(issue436T7JSON(t, in))
		}()},
		{"replay_negative_exceeds_total", T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), func() CallParams {
			in := base()
			in.ReplaySummary.NegativeSamples = 1
			return params(issue436T7JSON(t, in))
		}()},
		{"replay_leaks_exceed_negative", T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), func() CallParams {
			in := base()
			in.ReplaySummary.NegativeSamples, in.ReplaySummary.LeakCount = 1, 2
			return params(issue436T7JSON(t, in))
		}()},
		{"replay_false_blocks_exceed_remainder", T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), func() CallParams {
			in := base()
			in.ReplaySummary.TotalSamples, in.ReplaySummary.FalseBlockCount = 1, 2
			return params(issue436T7JSON(t, in))
		}()},
		{"scope_drift", T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), CallParams{Scope: storage.BrainScopeRun, SubjectKey: globalAll, Input: valid}},
		{"subject_drift", T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), CallParams{Scope: storage.BrainScopeAggregate, SubjectKey: "aggregate:v1:global:bug:1:2", Input: valid}},
		{"project_drift", T7Contract(projectKey, project, nil, []string{"cat"}), CallParams{Scope: storage.BrainScopeAggregate, SubjectKey: projectKey, ProjectID: "wrong-project", Input: projectInput}},
		{"window_drift", T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), func() CallParams { in := base(); in.Window.EndMS = 3; return params(issue436T7JSON(t, in)) }()},
		{"concrete_category_drift", T7Contract("aggregate:v1:global:bug:1:2", "", nil, []string{"cat"}), CallParams{Scope: storage.BrainScopeAggregate, SubjectKey: "aggregate:v1:global:bug:1:2", Input: issue436T7JSON(t, concrete)}},
		{"all_nil_expected_kinds", T7Contract(globalAll, "", nil, []string{"cat"}), params(valid)},
		{"all_empty_expected_kinds", T7Contract(globalAll, "", []TaskKind{}, []string{"cat"}), params(valid)},
		{"all_missing_category", T7Contract(globalAll, "", []TaskKind{TaskBug, TaskChore}, []string{"cat", "cat-chore"}), params(issue436T7JSON(t, allMissing))},
		{"all_extra_category", T7Contract(globalAll, "", []TaskKind{TaskBug, TaskChore}, []string{"cat", "cat-chore"}), params(issue436T7JSON(t, allExtra))},
		{"duplicate_category_replay_evidence", T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), func() CallParams {
			in := base()
			in.ReplaySummary.EvidenceID = "cat"
			return params(issue436T7JSON(t, in))
		}()},
		{"duplicate_category_semantic_evidence", T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), func() CallParams {
			in := base()
			in.SemanticMaterial = []T7SemanticMaterial{{EntryID: "cat", MaterialKind: "ask_text", Text: "text"}}
			return params(issue436T7JSON(t, in))
		}()},
		{"duplicate_replay_semantic_evidence", T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), func() CallParams {
			in := base()
			in.SemanticMaterial = []T7SemanticMaterial{{EntryID: "replay", MaterialKind: "ask_text", Text: "text"}}
			return params(issue436T7JSON(t, in))
		}()},
		{"duplicate_category_category_evidence", T7Contract(globalAll, "", []TaskKind{TaskBug, TaskChore}, []string{"same"}), func() CallParams {
			in := base()
			in.Categories = []T7CategoryEvidence{secondCategory(TaskBug, "same"), secondCategory(TaskChore, "same")}
			return params(issue436T7JSON(t, in))
		}()},
		{"duplicate_semantic_semantic_entry", T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), func() CallParams {
			in := base()
			in.SemanticMaterial = []T7SemanticMaterial{{EntryID: "same", MaterialKind: "ask_text", Text: "one"}, {EntryID: "same", MaterialKind: "ask_text", Text: "two"}}
			return params(issue436T7JSON(t, in))
		}()},
		{"categories_unsorted", T7Contract(globalAll, "", []TaskKind{TaskBug, TaskChore}, []string{"cat", "cat-chore"}), func() CallParams {
			in := base()
			in.Categories = []T7CategoryEvidence{secondCategory(TaskChore, "cat-chore"), secondCategory(TaskBug, "cat")}
			return params(issue436T7JSON(t, in))
		}()},
		{"semantic_unsorted", T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), func() CallParams {
			in := base()
			in.SemanticMaterial = []T7SemanticMaterial{{EntryID: "z", MaterialKind: "ask_text", Text: "z"}, {EntryID: "a", MaterialKind: "ask_text", Text: "a"}}
			return params(issue436T7JSON(t, in))
		}()},
	}

	for _, field := range []struct {
		name string
		path []string
	}{
		{"missing_aggregate_key", []string{"aggregate_key"}},
		{"missing_window", []string{"window"}},
		{"missing_window_start", []string{"window", "start_ms"}},
		{"missing_window_end", []string{"window", "end_ms"}},
		{"missing_categories", []string{"categories"}},
		{"missing_category_evidence_id", []string{"categories", "0", "evidence_id"}},
		{"missing_category_task_kind", []string{"categories", "0", "task_kind"}},
		{"missing_category_certification_version", []string{"categories", "0", "certification_version"}},
		{"missing_category_certified", []string{"categories", "0", "certified"}},
		{"missing_category_evidence_summary", []string{"categories", "0", "evidence_summary"}},
		{"missing_summary_window_start", []string{"categories", "0", "evidence_summary", "window_start_ms"}},
		{"missing_summary_window_end", []string{"categories", "0", "evidence_summary", "window_end_ms"}},
		{"missing_summary_certification_rules", []string{"categories", "0", "evidence_summary", "certification_rules_version"}},
		{"missing_summary_digest", []string{"categories", "0", "evidence_summary", "evidence_digest"}},
		{"missing_summary_total", []string{"categories", "0", "evidence_summary", "total_samples"}},
		{"missing_summary_negative", []string{"categories", "0", "evidence_summary", "negative_samples"}},
		{"missing_summary_leaks", []string{"categories", "0", "evidence_summary", "leak_count"}},
		{"missing_summary_false_blocks", []string{"categories", "0", "evidence_summary", "false_block_count"}},
		{"missing_replay_summary", []string{"replay_summary"}},
		{"missing_replay_evidence_id", []string{"replay_summary", "evidence_id"}},
		{"missing_replay_dataset_version", []string{"replay_summary", "dataset_version"}},
		{"missing_replay_gate_version", []string{"replay_summary", "gate_version"}},
		{"missing_replay_total", []string{"replay_summary", "total_samples"}},
		{"missing_replay_negative", []string{"replay_summary", "negative_samples"}},
		{"missing_replay_leaks", []string{"replay_summary", "leak_count"}},
		{"missing_replay_false_blocks", []string{"replay_summary", "false_block_count"}},
		{"missing_semantic_material", []string{"semantic_material"}},
		{"missing_semantic_entry_id", []string{"semantic_material", "0", "entry_id"}},
		{"missing_semantic_kind", []string{"semantic_material", "0", "material_kind"}},
		{"missing_semantic_text", []string{"semantic_material", "0", "text"}},
	} {
		in := base()
		in.SemanticMaterial = []T7SemanticMaterial{{EntryID: "entry", MaterialKind: "ask_text", Text: "text"}}
		cases = append(cases, struct {
			name     string
			contract TouchpointContract
			params   CallParams
		}{field.name, T7Contract(globalAll, "", []TaskKind{TaskBug}, []string{"cat"}), params(issue436T7Without(t, issue436T7JSON(t, in), field.path...))})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openShellDB(t)
			provider := &FakeProvider{}
			shell := newShellAt(db, shellCfg(100), provider, shellTestBase+1)
			beforeCalls, beforeAttempts := issue436TraceCounts(t, db)
			if _, err := shell.Call(context.Background(), tc.contract, tc.params); err == nil {
				t.Fatal("malformed T7 input was accepted")
			}
			afterCalls, afterAttempts := issue436TraceCounts(t, db)
			if afterCalls != beforeCalls || afterAttempts != beforeAttempts || len(provider.Requests) != 0 {
				t.Fatalf("malformed input created trace or provider request: calls %d→%d attempts %d→%d requests=%d", beforeCalls, afterCalls, beforeAttempts, afterAttempts, len(provider.Requests))
			}
		})
	}
}

func TestIssue436ValidAdaptersPreserveOutputAndCompleteBrainSource(t *testing.T) {
	result := CallResult{CallID: "call-valid", Status: "valid", PromptVersion: "T/v1/test", OutputSchemaVersion: 7}
	wantSource := BrainSource{Kind: "brain", LogicalCallID: result.CallID, PromptVersion: result.PromptVersion, OutputSchemaVersion: result.OutputSchemaVersion}
	t4JSON := []byte(`{"headline":"Review required","conclusion":"check failed","key_points":["review needed"],"recommended_option_id":"review","options":["review"]}`)
	var wantT4 T4Output
	if err := schema.Decode(t4JSON, &wantT4, schema.Closed); err != nil {
		t.Fatal(err)
	}
	t4, source, err := T4ResultFromCall(CallResult{CallID: result.CallID, Status: result.Status, PromptVersion: result.PromptVersion, OutputSchemaVersion: result.OutputSchemaVersion, Output: t4JSON}, t4Input())
	if err != nil || t4.Normal == nil || t4.Fallback != nil || !reflect.DeepEqual(*t4.Normal, wantT4) || !reflect.DeepEqual(source, wantSource) {
		t.Fatalf("T4 valid adapter = %#v %#v %v", t4, source, err)
	}
	t6JSON := []byte(`{"delivery":"batch","channel_id":"chat","suggested_downgrade":false,"rationale":"wait"}`)
	var wantT6 T6Output
	if err := schema.Decode(t6JSON, &wantT6, schema.Closed); err != nil {
		t.Fatal(err)
	}
	t6, source, err := T6ResultFromCall(CallResult{CallID: result.CallID, Status: result.Status, PromptVersion: result.PromptVersion, OutputSchemaVersion: result.OutputSchemaVersion, Output: t6JSON}, t6Input())
	if err != nil || !reflect.DeepEqual(t6, wantT6) || !reflect.DeepEqual(source, wantSource) {
		t.Fatalf("T6 valid adapter = %#v %#v %v", t6, source, err)
	}
	t7JSON := []byte(`{"proposal_kind":"policy","target_scope":"global","title":"Review trend","body":"Human review only.","evidence_entry_ids":["cat"],"requires_human_approval":true}`)
	var wantT7 T7Output
	if err := schema.Decode(t7JSON, &wantT7, schema.Closed); err != nil {
		t.Fatal(err)
	}
	t7, source, err := T7ResultFromCall(CallResult{CallID: result.CallID, Status: result.Status, PromptVersion: result.PromptVersion, OutputSchemaVersion: result.OutputSchemaVersion, Output: t7JSON}, "aggregate:v1:global:all:1:2", []string{"cat"})
	if err != nil || t7.Proposal == nil || t7.NoDraft || !reflect.DeepEqual(*t7.Proposal, wantT7) || !reflect.DeepEqual(source, wantSource) {
		t.Fatalf("T7 valid adapter = %#v %#v %v", t7, source, err)
	}
}

func TestIssue436FallbackAdaptersPreserveSourceAndT4Skeleton(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{{"provider_disabled", "provider_disabled"}, {"token_budget_exceeded", "token_threshold"}, {"input_too_large", "input_too_large"}, {"invalid_output", "invalid_output"}, {"attempts exhausted: provider detail must not leak", "provider_error"}, {"recovery: private operator detail must not leak", "recovery"}} {
		call := CallResult{CallID: "call-" + tc.want, Status: "fallback", FallbackReason: tc.raw}
		assertSource := func(t *testing.T, got BrainSource, touchpoint string) {
			t.Helper()
			want := BrainSource{Kind: "fallback", LogicalCallID: call.CallID, Version: touchpoint + "/fallback/v1", Reason: tc.want}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s source = %#v, want %#v", touchpoint, got, want)
			}
			if strings.Contains(got.Reason, "detail") || strings.Contains(got.Reason, "private") {
				t.Fatalf("%s source leaked raw fallback reason: %#v", touchpoint, got)
			}
		}
		t4, source, err := T4ResultFromCall(call, t4Input())
		if err != nil || t4.Fallback == nil || t4.Normal != nil {
			t.Fatalf("T4 %q: %#v %v", tc.want, t4, err)
		}
		assertSource(t, source, "T4")
		t7, source, err := T7ResultFromCall(call, "aggregate:v1:global:all:1:2", []string{"cat"})
		if err != nil || !t7.NoDraft || t7.Proposal != nil {
			t.Fatalf("T7 %q: %#v %v", tc.want, t7, err)
		}
		assertSource(t, source, "T7")
		for _, severity := range []InterruptSeverity{"low", "normal", "high", "critical"} {
			in := t6Input()
			in.Candidate.Severity = severity
			out, source, err := T6ResultFromCall(call, in)
			want := T6Delivery("batch")
			if severity == "high" || severity == "critical" {
				want = "immediate"
			}
			if err != nil || out.Delivery == nil || *out.Delivery != want {
				t.Fatalf("T6 %s: %#v %v", severity, out, err)
			}
			assertSource(t, source, "T6")
		}
	}

	in := t4Input()
	in.AttemptNo = intPtr(3)
	in.Interrupt.FallbackBrief = "check failed: build 17"
	in.Interrupt.BriefFragments = []string{"build 17 failed", "review needed"}
	in.Interrupt.Links = []T4Link{{Label: "evidence", Target: "https://example.test/e"}, {Label: "event", Target: "sift://event/0123456789abcdef0123456789abcdef"}}
	in.Interrupt.CandidateOptions = []T4Option{{ID: "review", Label: "Review", Effect: "open review", Risk: "delay"}, {ID: "retry", Label: "Retry", Effect: "retry check", Risk: "cost"}}
	if _, err := BuildT4Input(in); err != nil {
		t.Fatalf("lossless T4 fixture is not a valid frozen skeleton: %v", err)
	}
	var fallback T4Input
	if err := schema.Decode(T4FallbackOutput(in), &fallback, schema.Closed); err != nil || !reflect.DeepEqual(fallback, in) {
		t.Fatalf("lossless T4 fallback skeleton = %#v, want %#v: %v", fallback, in, err)
	}
}
