package brain

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/miaoxiaoyong/sift/internal/schema"
)

// T1/T2 boundary contracts (specs/brain.md §7/§8). Input structs are built by
// deterministic code and serialized to canonical JSON; output structs are the
// closed decode targets whose `sift` tags generate the versioned
// v1.schema.json assets. Schema-level rules (types, enums, lengths, mutex
// matrix) live on the structs; domain post-validation only adds rules that
// depend on runtime facts (candidate membership).

// ---------------------------------------------------------------------------
// T1 Intake 体检
// ---------------------------------------------------------------------------

// T1Forge is the forge identity of the issue under evaluation (brain.md §7.1).
type T1Forge struct {
	Kind       string `json:"kind"`
	Host       string `json:"host"`
	ProjectKey string `json:"project_key"`
}

// T1Issue is the untrusted issue payload offered to the model.
type T1Issue struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Author string   `json:"author"`
	URL    string   `json:"url"`
	Labels []string `json:"labels"`
}

// T1Candidate is one known Run offered as a duplicate suspect.
type T1Candidate struct {
	RunID   string `json:"run_id"`
	IssueID string `json:"issue_id"`
	Title   string `json:"title"`
	Status  string `json:"status"`
}

// T1Input is the §7.1 top-level input contract.
type T1Input struct {
	Forge           T1Forge       `json:"forge"`
	Issue           T1Issue       `json:"issue"`
	KnownCandidates []T1Candidate `json:"known_candidates"`
}

// BuildT1Input canonicalizes a T1 input: labels are sorted/deduped by the
// caller-facing contract and candidates are sorted by run_id. It returns the
// canonical JSON bytes whose length is compared against brain.max_input_bytes.
func BuildT1Input(in T1Input) ([]byte, error) {
	if in.Forge.Kind != "github" && in.Forge.Kind != "gitlab" {
		return nil, fmt.Errorf("brain: T1 forge kind %q not in {github, gitlab}", in.Forge.Kind)
	}
	if len(in.Forge.Host) == 0 || len(in.Forge.Host) > 253 {
		return nil, fmt.Errorf("brain: T1 forge host length %d out of 1..253", len(in.Forge.Host))
	}
	if len(in.Forge.ProjectKey) == 0 || len(in.Forge.ProjectKey) > 255 {
		return nil, fmt.Errorf("brain: T1 project_key length %d out of 1..255", len(in.Forge.ProjectKey))
	}
	if len(in.Issue.ID) == 0 || len(in.Issue.ID) > 64 {
		return nil, fmt.Errorf("brain: T1 issue id length %d out of 1..64", len(in.Issue.ID))
	}
	if len(in.Issue.Title) > 512 || len(in.Issue.Body) > 65536 || len(in.Issue.Author) > 128 || len(in.Issue.URL) > 1024 {
		return nil, errors.New("brain: T1 issue field exceeds §7.1 byte bound")
	}
	labels := append([]string(nil), in.Issue.Labels...)
	sort.Strings(labels)
	labels = dedupeStrings(labels)
	if len(labels) > 32 {
		return nil, fmt.Errorf("brain: T1 labels %d exceed 32", len(labels))
	}
	for _, l := range labels {
		if len(l) > 128 {
			return nil, fmt.Errorf("brain: T1 label %q exceeds 128 bytes", l)
		}
	}
	in.Issue.Labels = labels

	candidates := append([]T1Candidate(nil), in.KnownCandidates...)
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].RunID < candidates[j].RunID })
	if len(candidates) > 20 {
		return nil, fmt.Errorf("brain: T1 candidates %d exceed 20", len(candidates))
	}
	for _, c := range candidates {
		if len(c.IssueID) == 0 || len(c.IssueID) > 64 || len(c.Title) > 512 {
			return nil, fmt.Errorf("brain: T1 candidate %q field exceeds §7.1 bound", c.RunID)
		}
		switch c.Status {
		case "queued", "running", "waiting_human", "done", "failed":
		default:
			return nil, fmt.Errorf("brain: T1 candidate %q status %q invalid", c.RunID, c.Status)
		}
	}
	in.KnownCandidates = candidates
	if in.KnownCandidates == nil {
		in.KnownCandidates = []T1Candidate{}
	}
	if in.Issue.Labels == nil {
		in.Issue.Labels = []string{}
	}
	return schema.Canonical(in)
}

// T1Disposition is the closed T1 output enum (brain.md §7.2).
type T1Disposition string

const (
	T1Ready              T1Disposition = "ready"
	T1NeedsClarification T1Disposition = "needs_clarification"
	T1PossibleDuplicate  T1Disposition = "possible_duplicate"
)

// EnumValues satisfies schema.Enumerated.
func (T1Disposition) EnumValues() []string {
	return []string{string(T1Ready), string(T1NeedsClarification), string(T1PossibleDuplicate)}
}

// T1Output is the §7.2 closed output contract. possible_duplicate_run_id is
// required but nullable (schema.NullString tracks key presence).
type T1Output struct {
	schema.ClosedType `json:"-"`

	Disposition            *T1Disposition    `json:"disposition" sift:"required"`
	Questions              *[]string         `json:"questions" sift:"required,maxitems=5,itemminbytes=1,itemmaxbytes=1000"`
	PossibleDuplicateRunID schema.NullString `json:"possible_duplicate_run_id" sift:"keyrequired,maxbytes=64"`
	Rationale              *string           `json:"rationale" sift:"required,maxbytes=2000"`
}

// Validate enforces the disposition mutex matrix and the trim/dedupe content
// rules (brain.md §7.2), re-checking what the schema layer expresses.
func (o T1Output) Validate() error {
	if o.Disposition == nil || o.Questions == nil {
		return nil // required checks report first
	}
	questions := *o.Questions
	trimmed := make([]string, 0, len(questions))
	seen := map[string]bool{}
	for _, q := range questions {
		if q != strings.TrimSpace(q) {
			return errors.New("brain: T1 question is not trimmed")
		}
		if seen[q] {
			return errors.New("brain: T1 questions contain duplicates")
		}
		seen[q] = true
		trimmed = append(trimmed, q)
	}
	dup := o.PossibleDuplicateRunID.Present && !o.PossibleDuplicateRunID.Null && o.PossibleDuplicateRunID.Value != ""
	switch *o.Disposition {
	case T1Ready:
		if len(questions) != 0 || dup {
			return errors.New("brain: T1 ready requires empty questions and null duplicate")
		}
	case T1NeedsClarification:
		if len(questions) < 1 || len(questions) > 5 || dup {
			return errors.New("brain: T1 needs_clarification requires 1..5 questions and null duplicate")
		}
	case T1PossibleDuplicate:
		if !dup || len(questions) != 0 {
			return errors.New("brain: T1 possible_duplicate requires a duplicate run id and empty questions")
		}
	}
	return nil
}

// ExtendJSONSchema expresses the disposition mutex matrix at the schema layer
// (brain.md §7.2); Validate re-checks it at runtime.
func (o T1Output) ExtendJSONSchema() map[string]any {
	rule := func(disposition string, minQ, maxQ int, dupNull bool) map[string]any {
		q := map[string]any{"type": "array"}
		if minQ > 0 {
			q["minItems"] = minQ
		}
		q["maxItems"] = maxQ
		dupSchema := map[string]any{}
		if dupNull {
			dupSchema["type"] = "null"
		} else {
			dupSchema["type"] = "string"
		}
		return map[string]any{
			"if": map[string]any{"properties": map[string]any{"disposition": map[string]any{"const": disposition}}},
			"then": map[string]any{"properties": map[string]any{
				"questions":                 q,
				"possible_duplicate_run_id": dupSchema,
			}},
		}
	}
	return map[string]any{"allOf": []any{
		rule("ready", 0, 0, true),
		rule("needs_clarification", 1, 5, true),
		rule("possible_duplicate", 0, 0, false),
	}}
}

// T1FallbackOutput is the fixed deterministic T1 fallback (brain.md §7.2):
// ready with empty questions, null duplicate and rationale "fallback".
func T1FallbackOutput() []byte {
	d := T1Ready
	q := []string{}
	r := "fallback"
	out, err := schema.Canonical(T1Output{
		Disposition:            &d,
		Questions:              &q,
		PossibleDuplicateRunID: schema.NullString{Present: true, Null: true},
		Rationale:              &r,
	})
	if err != nil {
		panic(fmt.Sprintf("brain: T1 fallback must be canonical: %v", err))
	}
	return out
}

// ---------------------------------------------------------------------------
// T2 分派
// ---------------------------------------------------------------------------

// T2Issue is the issue slice offered to the model (brain.md §8.1).
type T2Issue struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
}

// T2AgentCandidate is one pre-filtered candidate agent.
type T2AgentCandidate struct {
	ID           string   `json:"id"`
	Capabilities []string `json:"capabilities"`
}

// T2Annotation is one task annotation carried into context.
type T2Annotation struct {
	EventID string `json:"event_id"`
	Text    string `json:"text"`
}

// T2BaseContext bundles the untrusted context slices.
type T2BaseContext struct {
	ProjectContext  string         `json:"project_context"`
	GlobalContext   string         `json:"global_context"`
	TaskAnnotations []T2Annotation `json:"task_annotations"`
}

// T2Input is the §8.1 top-level input contract.
type T2Input struct {
	RunID           string             `json:"run_id"`
	Issue           T2Issue            `json:"issue"`
	CandidateAgents []T2AgentCandidate `json:"candidate_agents"`
	BaseContext     T2BaseContext      `json:"base_context"`
}

// BuildT2Input canonicalizes a T2 input, enforcing the §8.1 bounds and the
// candidate id ordering.
func BuildT2Input(in T2Input) ([]byte, error) {
	if in.RunID == "" {
		return nil, errors.New("brain: T2 run_id is required")
	}
	if len(in.Issue.Title) > 512 || len(in.Issue.Body) > 65536 || len(in.Issue.URL) > 1024 {
		return nil, errors.New("brain: T2 issue field exceeds §8.1 byte bound")
	}
	if len(in.CandidateAgents) < 1 || len(in.CandidateAgents) > 32 {
		return nil, fmt.Errorf("brain: T2 candidate_agents %d out of 1..32", len(in.CandidateAgents))
	}
	candidates := append([]T2AgentCandidate(nil), in.CandidateAgents...)
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	for _, c := range candidates {
		if len(c.ID) == 0 || len(c.ID) > 64 {
			return nil, fmt.Errorf("brain: T2 candidate id length %d out of 1..64", len(c.ID))
		}
		if len(c.Capabilities) > 16 {
			return nil, fmt.Errorf("brain: T2 candidate %q capabilities %d exceed 16", c.ID, len(c.Capabilities))
		}
		for _, cap := range c.Capabilities {
			if len(cap) > 64 {
				return nil, fmt.Errorf("brain: T2 candidate %q capability exceeds 64 bytes", c.ID)
			}
		}
	}
	in.CandidateAgents = candidates
	if len(in.BaseContext.ProjectContext) > 65536 || len(in.BaseContext.GlobalContext) > 65536 {
		return nil, errors.New("brain: T2 context field exceeds §8.1 byte bound")
	}
	if len(in.BaseContext.TaskAnnotations) > 50 {
		return nil, fmt.Errorf("brain: T2 task_annotations %d exceed 50", len(in.BaseContext.TaskAnnotations))
	}
	for _, a := range in.BaseContext.TaskAnnotations {
		if a.EventID == "" || len(a.Text) > 2000 {
			return nil, fmt.Errorf("brain: T2 annotation %q invalid", a.EventID)
		}
	}
	if in.BaseContext.TaskAnnotations == nil {
		in.BaseContext.TaskAnnotations = []T2Annotation{}
	}
	return schema.Canonical(in)
}

// TaskKind is the closed T2 task-kind enum (brain.md §8.2).
type TaskKind string

const (
	TaskFeature  TaskKind = "feature"
	TaskBug      TaskKind = "bug"
	TaskChore    TaskKind = "chore"
	TaskDocs     TaskKind = "docs"
	TaskRefactor TaskKind = "refactor"
)

// EnumValues satisfies schema.Enumerated.
func (TaskKind) EnumValues() []string {
	return []string{string(TaskFeature), string(TaskBug), string(TaskChore), string(TaskDocs), string(TaskRefactor)}
}

// T2Output is the §8.2 closed output contract. Guardrails, attempts and
// concurrency never appear here: extra fields are rejected by the closed contract.
type T2Output struct {
	schema.ClosedType `json:"-"`

	Kind            *TaskKind `json:"kind" sift:"required"`
	Agent           *string   `json:"agent" sift:"required,maxbytes=64"`
	HITLBeforeStart *bool     `json:"hitl_before_start" sift:"required"`
	Goals           *[]string `json:"goals" sift:"required,minitems=1,maxitems=10,itemminbytes=1,itemmaxbytes=1000"`
	RiskNotes       *string   `json:"risk_notes" sift:"required,maxbytes=2000"`
	Rationale       *string   `json:"rationale" sift:"required,maxbytes=2000"`
}

// Validate enforces the goals content rules (trim, dedupe, 8000-byte total)
// of brain.md §8.2.
func (o T2Output) Validate() error {
	if o.Goals == nil {
		return nil
	}
	total := 0
	seen := map[string]bool{}
	for _, g := range *o.Goals {
		if g != strings.TrimSpace(g) {
			return errors.New("brain: T2 goal is not trimmed")
		}
		if seen[g] {
			return errors.New("brain: T2 goals contain duplicates")
		}
		seen[g] = true
		total += len(g)
	}
	if total > 8000 {
		return fmt.Errorf("brain: T2 goals total %d bytes exceed 8000", total)
	}
	return nil
}

// EffectiveHITL combines the LLM suggestion with the deterministic force
// (brain.md §8.3): a forced true can never be overridden by an LLM false.
func EffectiveHITL(llmSuggestion, deterministicForce bool) bool {
	return llmSuggestion || deterministicForce
}

// ---------------------------------------------------------------------------
// Touchpoint contracts
// ---------------------------------------------------------------------------

// T1Contract returns the shell contract for T1. candidateRunIDs carries the
// runtime fact the domain post-validation needs: a suggested duplicate must
// exactly hit one of the input candidates (brain.md §7.2).
func T1Contract(candidateRunIDs []string) TouchpointContract {
	allowed := map[string]bool{}
	for _, id := range candidateRunIDs {
		allowed[id] = true
	}
	return TouchpointContract{
		Touchpoint: "T1",
		Asset:      T1Asset(),
		ValidateOutput: func(resultText []byte) ([]byte, error) {
			var out T1Output
			if err := schema.Decode(resultText, &out, schema.Closed); err != nil {
				return nil, err
			}
			if out.PossibleDuplicateRunID.Present && !out.PossibleDuplicateRunID.Null &&
				out.PossibleDuplicateRunID.Value != "" && !allowed[out.PossibleDuplicateRunID.Value] {
				return nil, fmt.Errorf("brain: T1 duplicate %q is not an input candidate", out.PossibleDuplicateRunID.Value)
			}
			return schema.Canonical(out)
		},
		FallbackOutput: T1FallbackOutput,
	}
}

// T2Contract returns the shell contract for T2. candidateIDs is the filtered
// candidate set the suggested agent must hit exactly (brain.md §8.2).
func T2Contract(candidateIDs []string) TouchpointContract {
	allowed := map[string]bool{}
	for _, id := range candidateIDs {
		allowed[id] = true
	}
	return TouchpointContract{
		Touchpoint: "T2",
		Asset:      T2Asset(),
		ValidateOutput: func(resultText []byte) ([]byte, error) {
			var out T2Output
			if err := schema.Decode(resultText, &out, schema.Closed); err != nil {
				return nil, err
			}
			if !allowed[*out.Agent] {
				return nil, fmt.Errorf("brain: T2 agent %q is not an input candidate", *out.Agent)
			}
			return schema.Canonical(out)
		},
		// T2 has no synthetic output: the fallback keeps the Run for human
		// assignment (brain.md §8.2), never a fabricated first-agent pick.
		FallbackOutput: func() []byte { return nil },
	}
}

func dedupeStrings(in []string) []string {
	out := in[:0]
	for i, s := range in {
		if i == 0 || in[i-1] != s {
			out = append(out, s)
		}
	}
	return out
}
