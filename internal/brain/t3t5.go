package brain

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/miaoxiaoyong/sift/internal/contract"
	"github.com/miaoxiaoyong/sift/internal/decode"
)

var headSHA = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// ChangeInput is the frozen Change identity supplied to T3 and T5.
type ChangeInput struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	HeadSHA string `json:"head_sha"`
}

// T3Change adds the raw Forge unified diff to a ChangeInput.
type T3Change struct {
	ChangeInput
	Diff string `json:"diff"`
}

// T3Input is the risk-scoring input contract in brain.md §9.1.
type T3Input struct {
	RunID    string   `json:"run_id"`
	TaskKind TaskKind `json:"task_kind"`
	Change   T3Change `json:"change"`
}

func validateChange(c ChangeInput) error {
	if len(c.ID) == 0 || len(c.ID) > 256 || !headSHA.MatchString(c.HeadSHA) {
		return errors.New("brain: invalid Change identity")
	}
	if len(c.URL) == 0 || len(c.URL) > 2048 || !httpsURL(c.URL) {
		return errors.New("brain: Change URL must be HTTPS and within 2048 bytes")
	}
	return nil
}

func httpsURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

// BuildT3Input validates and canonicalizes the T3 input. Callers must obtain
// it from GetChange -> GetChangeDiff -> GetChange and reject a drifting Change
// before calling this function (brain.md §9.1).
func BuildT3Input(in T3Input) ([]byte, error) {
	if len(in.RunID) == 0 || len(in.RunID) > 256 {
		return nil, errors.New("brain: T3 run_id length out of 1..256")
	}
	if !validTaskKind(in.TaskKind) {
		return nil, fmt.Errorf("brain: invalid T3 task_kind %q", in.TaskKind)
	}
	if err := validateChange(in.Change.ChangeInput); err != nil {
		return nil, fmt.Errorf("brain: T3 %w", err)
	}
	return decode.Canonical(in)
}

// T3Output is the closed risk recommendation contract in brain.md §9.2.
type T3Output struct {
	contract.ClosedType `json:"-"`
	RiskScore           *int      `json:"risk_score" sift:"required,min=0,max=100"`
	RiskPoints          *[]string `json:"risk_points" sift:"required,maxitems=10,itemminbytes=1,itemmaxbytes=1000"`
	Rationale           *string   `json:"rationale" sift:"required,maxbytes=2000"`
}

func (o T3Output) Validate() error {
	if o.RiskPoints == nil {
		return nil
	}
	for i, p := range *o.RiskPoints {
		if p != strings.TrimSpace(p) {
			return errors.New("brain: T3 risk point is not trimmed")
		}
		if i > 0 && (*o.RiskPoints)[i-1] >= p {
			return errors.New("brain: T3 risk points must be UTF-8 byte sorted and unique")
		}
	}
	return nil
}

// T3FallbackOutput is the fixed high-risk fallback required by brain.md §9.3.
func T3FallbackOutput() []byte {
	out, err := decode.Canonical(T3Output{RiskScore: intp(100), RiskPoints: &[]string{"T3 unavailable; deterministic high-risk fallback"}, Rationale: strp("fallback")})
	if err != nil {
		panic(fmt.Sprintf("brain: T3 fallback: %v", err))
	}
	return out
}

func T3Contract() TouchpointContract {
	return TouchpointContract{Touchpoint: "T3", Asset: T3Asset(), ValidateOutput: func(result []byte) ([]byte, error) {
		var out T3Output
		if err := decode.Decode(result, &out, decode.Closed); err != nil {
			return nil, err
		}
		return decode.Canonical(out)
	}, FallbackOutput: T3FallbackOutput}
}

// BrainSource is the closed provenance object that callers include alongside
// a T3 risk result or T5 triage in a Gate input snapshot.
type BrainSource struct {
	Kind                string `json:"kind"`
	LogicalCallID       string `json:"logical_call_id"`
	PromptVersion       string `json:"prompt_version,omitempty"`
	OutputSchemaVersion int    `json:"output_schema_version,omitempty"`
	Version             string `json:"version,omitempty"`
	Reason              string `json:"reason,omitempty"`
}

// T3ResultFromCall turns a terminal shell result into the value and provenance
// consumed by Gate. It never lets a fallback look like a normal Brain output.
func T3ResultFromCall(result CallResult) (T3Output, BrainSource, error) {
	var out T3Output
	if result.Status == "valid" {
		if err := decode.Decode(result.Output, &out, decode.Closed); err != nil {
			return out, BrainSource{}, err
		}
		return out, BrainSource{Kind: "brain", LogicalCallID: result.CallID, PromptVersion: result.PromptVersion, OutputSchemaVersion: result.OutputSchemaVersion}, nil
	}
	if result.Status != "fallback" {
		return out, BrainSource{}, fmt.Errorf("brain: T3 call %s is not terminal", result.CallID)
	}
	if err := decode.Decode(T3FallbackOutput(), &out, decode.Closed); err != nil {
		return out, BrainSource{}, err
	}
	return out, BrainSource{Kind: "fallback", LogicalCallID: result.CallID, Version: "T3/fallback/v1", Reason: fallbackReason(result.FallbackReason)}, nil
}

func fallbackReason(reason string) string {
	switch {
	case strings.Contains(reason, "provider_disabled") || strings.Contains(reason, "provider_forbidden"):
		return "provider_disabled"
	case strings.Contains(reason, "token_budget"):
		return "token_threshold"
	case strings.Contains(reason, "input_too_large"):
		return "input_too_large"
	case strings.Contains(reason, "recovery"):
		return "recovery"
	case strings.Contains(reason, "invalid_output"):
		return "invalid_output"
	default:
		return "provider_error"
	}
}

// T5Job is a stable failed CI job offered to failure triage.
type T5Job struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	WebURL       string `json:"web_url"`
	AllowFailure bool   `json:"allow_failure"`
}

type T5Checks struct {
	ExternalURL string  `json:"external_url"`
	FailedJobs  []T5Job `json:"failed_jobs"`
}

// T5Input is the failure-triage input contract in brain.md §10.1.
type T5Input struct {
	RunID  string      `json:"run_id"`
	Change ChangeInput `json:"change"`
	Checks T5Checks    `json:"checks"`
}

func BuildT5Input(in T5Input) ([]byte, error) {
	if len(in.RunID) == 0 || len(in.RunID) > 256 {
		return nil, errors.New("brain: T5 run_id length out of 1..256")
	}
	if err := validateChange(in.Change); err != nil {
		return nil, fmt.Errorf("brain: T5 %w", err)
	}
	if len(in.Checks.ExternalURL) == 0 || len(in.Checks.ExternalURL) > 2048 || !httpsURL(in.Checks.ExternalURL) {
		return nil, errors.New("brain: T5 external_url must be HTTPS and within 2048 bytes")
	}
	jobs := append([]T5Job(nil), in.Checks.FailedJobs...)
	if len(jobs) < 1 || len(jobs) > 100 {
		return nil, fmt.Errorf("brain: T5 failed_jobs %d out of 1..100", len(jobs))
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].ID == jobs[j].ID {
			return jobs[i].Name < jobs[j].Name
		}
		return jobs[i].ID < jobs[j].ID
	})
	for i, job := range jobs {
		if len(job.ID) == 0 || len(job.ID) > 256 || len(job.Name) == 0 || len(job.Name) > 512 || len(job.WebURL) == 0 || len(job.WebURL) > 2048 || !httpsURL(job.WebURL) {
			return nil, fmt.Errorf("brain: invalid T5 failed job %q", job.ID)
		}
		if i > 0 && jobs[i-1].ID == job.ID {
			return nil, fmt.Errorf("brain: duplicate T5 failed job id %q", job.ID)
		}
	}
	in.Checks.FailedJobs = jobs
	return decode.Canonical(in)
}

type T5Classification string

const (
	T5Flaky          T5Classification = "flaky"
	T5RealFailure    T5Classification = "real_failure"
	T5Infrastructure T5Classification = "infrastructure"
)

func (T5Classification) EnumValues() []string {
	return []string{string(T5Flaky), string(T5RealFailure), string(T5Infrastructure)}
}

// T5Output is the closed triage recommendation contract in brain.md §10.2.
type T5Output struct {
	contract.ClosedType `json:"-"`
	Classification      *T5Classification `json:"classification" sift:"required"`
	RetryCheckID        decode.NullString `json:"retry_check_id" sift:"keyrequired,maxbytes=256"`
	Rationale           *string           `json:"rationale" sift:"required,minbytes=1,maxbytes=2000"`
}

func (o T5Output) Validate() error {
	if o.Classification == nil {
		return nil
	}
	hasRetry := o.RetryCheckID.Present && !o.RetryCheckID.Null && o.RetryCheckID.Value != ""
	if *o.Classification == T5Flaky && !hasRetry {
		return errors.New("brain: T5 flaky requires retry_check_id")
	}
	if *o.Classification != T5Flaky && !o.RetryCheckID.Null {
		return errors.New("brain: only T5 flaky may have retry_check_id")
	}
	if o.Rationale != nil && *o.Rationale != strings.TrimSpace(*o.Rationale) {
		return errors.New("brain: T5 rationale is not trimmed")
	}
	return nil
}

func T5Contract(jobs []T5Job) TouchpointContract {
	allowed := make(map[string]bool, len(jobs))
	for _, job := range jobs {
		if !job.AllowFailure {
			allowed[job.ID] = true
		}
	}
	return TouchpointContract{Touchpoint: "T5", Asset: T5Asset(), ValidateOutput: func(result []byte) ([]byte, error) {
		var out T5Output
		if err := decode.Decode(result, &out, decode.Closed); err != nil {
			return nil, err
		}
		if out.Classification != nil && *out.Classification == T5Flaky && !allowed[out.RetryCheckID.Value] {
			return nil, fmt.Errorf("brain: T5 retry_check_id %q is not a non-allow-failure input job", out.RetryCheckID.Value)
		}
		return decode.Canonical(out)
	}, FallbackOutput: nil}
}

// T5Triage is the value and provenance which a failure Gate snapshot stores.
type T5Triage struct {
	Classification T5Classification `json:"classification"`
	RetryCheckID   *string          `json:"retry_check_id"`
	Source         BrainSource      `json:"source"`
}

// T5TriageFromCall turns a terminal shell result into a Gate triage. Fallback
// is always unknown and deliberately has no retry target.
func T5TriageFromCall(result CallResult) (T5Triage, error) {
	if result.Status == "valid" {
		var out T5Output
		if err := decode.Decode(result.Output, &out, decode.Closed); err != nil {
			return T5Triage{}, err
		}
		var retry *string
		if !out.RetryCheckID.Null {
			v := out.RetryCheckID.Value
			retry = &v
		}
		return T5Triage{Classification: *out.Classification, RetryCheckID: retry, Source: BrainSource{Kind: "brain", LogicalCallID: result.CallID, PromptVersion: result.PromptVersion, OutputSchemaVersion: result.OutputSchemaVersion}}, nil
	}
	if result.Status != "fallback" {
		return T5Triage{}, fmt.Errorf("brain: T5 call %s is not terminal", result.CallID)
	}
	return T5Triage{Classification: "unknown", Source: BrainSource{Kind: "fallback", LogicalCallID: result.CallID, Version: "T5/fallback/v1", Reason: fallbackReason(result.FallbackReason)}}, nil
}

func validTaskKind(k TaskKind) bool {
	switch k {
	case TaskFeature, TaskBug, TaskChore, TaskDocs, TaskRefactor:
		return true
	}
	return false
}
func intp(v int) *int       { return &v }
func strp(v string) *string { return &v }
