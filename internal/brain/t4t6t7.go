package brain

import (
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/miaoxiaoyong/sift/internal/schema"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// InterruptReason is the Brain view of the canonical storage Interrupt reason.
// EnumValues delegates to storage's single set of reason constants.
type InterruptReason string

func (InterruptReason) EnumValues() []string {
	reasons := storage.ActiveInterruptReasons()
	values := make([]string, len(reasons))
	for i, reason := range reasons {
		values[i] = string(reason)
	}
	return values
}

type InterruptSeverity string

func (InterruptSeverity) EnumValues() []string {
	return []string{"low", "normal", "high", "critical"}
}

type InterruptModality string

func (InterruptModality) EnumValues() []string { return []string{"voice", "text", "visual"} }

type T4Link struct {
	Label  string `json:"label"`
	Target string `json:"target"`
}

type T4Option struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Effect string `json:"effect"`
	Risk   string `json:"risk"`
}

type T4Interrupt struct {
	Reason           InterruptReason   `json:"reason"`
	BaseSeverity     InterruptSeverity `json:"base_severity"`
	MinModality      InterruptModality `json:"min_modality"`
	FallbackHeadline string            `json:"fallback_headline"`
	FallbackBrief    string            `json:"fallback_brief"`
	BriefFragments   []string          `json:"brief_fragments"`
	Links            []T4Link          `json:"links"`
	CandidateOptions []T4Option        `json:"candidate_options"`
}

type T4Input struct {
	RunID     string      `json:"run_id"`
	AttemptNo *int        `json:"attempt_no"`
	Interrupt T4Interrupt `json:"interrupt"`
}

// BuildT4Input validates the already-verified Interrupt skeleton and returns
// its canonical form. The caller remains responsible for literal canonical
// option/headline equality with the Interrupt emitter.
func BuildT4Input(in T4Input) ([]byte, error) {
	if len(in.RunID) == 0 || len(in.RunID) > 256 || (in.AttemptNo != nil && *in.AttemptNo < 1) {
		return nil, errors.New("brain: invalid T4 run or attempt identity")
	}
	i := in.Interrupt
	if !inEnum(i.Reason) || !inEnum(i.BaseSeverity) || !inEnum(i.MinModality) || runeCount(i.FallbackHeadline) < 1 || runeCount(i.FallbackHeadline) > 40 || hasControlOrNewline(i.FallbackHeadline) || len(i.FallbackBrief) < 1 || len(i.FallbackBrief) > 8192 || len(i.BriefFragments) < 1 || len(i.BriefFragments) > 32 || len(i.Links) > 32 || len(i.CandidateOptions) < 1 || len(i.CandidateOptions) > 4 {
		return nil, errors.New("brain: invalid T4 interrupt skeleton")
	}
	for n, f := range i.BriefFragments {
		if len(f) < 1 || len(f) > 1000 || hasControlOrNewline(f) || (n > 0 && i.BriefFragments[n-1] >= f) {
			return nil, errors.New("brain: T4 brief_fragments must be bounded, safe, sorted, and unique")
		}
	}
	for n, l := range i.Links {
		if len(l.Label) < 1 || len(l.Label) > 128 || len(l.Target) < 1 || len(l.Target) > 4096 || !validT4Link(l.Target) || (n > 0 && (i.Links[n-1].Target > l.Target || i.Links[n-1].Target == l.Target && i.Links[n-1].Label >= l.Label)) {
			return nil, errors.New("brain: invalid T4 links")
		}
	}
	seen := map[string]bool{}
	for _, o := range i.CandidateOptions {
		if !optionID.MatchString(o.ID) || seen[o.ID] || len(o.Label) < 1 || len(o.Label) > 256 || len(o.Effect) < 1 || len(o.Effect) > 1000 || len(o.Risk) < 1 || len(o.Risk) > 1000 || hasControlOrNewline(o.Label) || hasControlOrNewline(o.Effect) || hasControlOrNewline(o.Risk) {
			return nil, errors.New("brain: invalid T4 candidate option")
		}
		seen[o.ID] = true
	}
	return schema.Canonical(in)
}

type T4Output struct {
	schema.ClosedType   `json:"-"`
	Headline            *string   `json:"headline" sift:"required,maxbytes=160"`
	Conclusion          *string   `json:"conclusion" sift:"required,maxbytes=1000"`
	KeyPoints           *[]string `json:"key_points" sift:"required,minitems=1,maxitems=3,itemminbytes=1,itemmaxbytes=1000"`
	RecommendedOptionID *string   `json:"recommended_option_id" sift:"required,maxbytes=64"`
	Options             *[]string `json:"options" sift:"required,minitems=1,maxitems=4,itemminbytes=1,itemmaxbytes=64"`
}

func T4Contract(in T4Input) TouchpointContract {
	return TouchpointContract{Touchpoint: "T4", Asset: T4Asset(), FallbackOutput: func() []byte { return T4FallbackOutput(in) }, ValidateOutput: func(result []byte) ([]byte, error) {
		var out T4Output
		if err := schema.Decode(result, &out, schema.Closed); err != nil {
			return nil, err
		}
		if *out.Headline != in.Interrupt.FallbackHeadline || !contains(in.Interrupt.BriefFragments, *out.Conclusion) || !containsOption(in.Interrupt.CandidateOptions, *out.RecommendedOptionID) || len(*out.Options) != len(in.Interrupt.CandidateOptions) {
			return nil, errors.New("brain: T4 output does not preserve the frozen skeleton")
		}
		seen := map[string]bool{}
		for _, point := range *out.KeyPoints {
			if !contains(in.Interrupt.BriefFragments, point) || seen[point] {
				return nil, errors.New("brain: invalid T4 key point")
			}
			seen[point] = true
		}
		for n, id := range *out.Options {
			if id != in.Interrupt.CandidateOptions[n].ID {
				return nil, errors.New("brain: T4 options must exactly preserve canonical order")
			}
		}
		return schema.Canonical(out)
	}}
}

type T6Delivery string

func (T6Delivery) EnumValues() []string { return []string{"immediate", "batch", "next_window"} }

type T6AvailabilityState string

func (T6AvailabilityState) EnumValues() []string {
	return []string{"available", "unavailable", "unknown"}
}

type T6Quota struct {
	Severity  InterruptSeverity `json:"severity"`
	Remaining int64             `json:"remaining"`
}
type T6Availability struct {
	State          T6AvailabilityState `json:"state"`
	NextWindowAtMS *int64              `json:"next_window_at_ms"`
}
type T6Candidate struct {
	Reason            InterruptReason   `json:"reason"`
	Severity          InterruptSeverity `json:"severity"`
	MinModality       InterruptModality `json:"min_modality"`
	ExpiresAtMS       int64             `json:"expires_at_ms"`
	ChannelCandidates []string          `json:"channel_candidates"`
	DefaultChannelID  string            `json:"default_channel_id"`
}
type T6Attention struct {
	FallbackImmediateMinSeverity InterruptSeverity `json:"fallback_immediate_min_severity"`
	Remaining                    []T6Quota         `json:"remaining"`
}
type T6Input struct {
	RunID        string         `json:"run_id"`
	AttemptNo    *int           `json:"attempt_no"`
	FrozenAtMS   int64          `json:"frozen_at_ms"`
	Candidate    T6Candidate    `json:"candidate"`
	Availability T6Availability `json:"availability"`
	Attention    T6Attention    `json:"attention"`
}

func BuildT6Input(in T6Input) ([]byte, error) {
	if len(in.RunID) == 0 || len(in.RunID) > 256 || in.FrozenAtMS < 0 || (in.AttemptNo != nil && *in.AttemptNo < 1) || !inEnum(in.Candidate.Reason) || !inEnum(in.Candidate.Severity) || !inEnum(in.Candidate.MinModality) || in.Candidate.ExpiresAtMS <= in.FrozenAtMS || in.Attention.FallbackImmediateMinSeverity != "high" {
		return nil, errors.New("brain: invalid T6 frozen candidate")
	}
	if !inEnum(in.Availability.State) || (in.Availability.State == "available" && in.Availability.NextWindowAtMS != nil) || (in.Availability.State == "unavailable" && (in.Availability.NextWindowAtMS == nil || *in.Availability.NextWindowAtMS <= in.FrozenAtMS)) || (in.Availability.NextWindowAtMS != nil && *in.Availability.NextWindowAtMS <= in.FrozenAtMS) {
		return nil, errors.New("brain: invalid T6 availability")
	}
	if len(in.Candidate.ChannelCandidates) < 1 || len(in.Candidate.ChannelCandidates) > 8 || !contains(in.Candidate.ChannelCandidates, in.Candidate.DefaultChannelID) || len(in.Attention.Remaining) != 3 {
		return nil, errors.New("brain: invalid T6 channels or quota")
	}
	for n, c := range in.Candidate.ChannelCandidates {
		if len(c) < 1 || len(c) > 128 || (n > 0 && in.Candidate.ChannelCandidates[n-1] >= c) {
			return nil, errors.New("brain: T6 channels must be sorted and unique")
		}
	}
	for n, q := range in.Attention.Remaining {
		if q.Severity != []InterruptSeverity{"low", "normal", "high"}[n] || q.Remaining < 0 {
			return nil, errors.New("brain: invalid T6 quota snapshot")
		}
	}
	return schema.Canonical(in)
}

type T6Output struct {
	schema.ClosedType  `json:"-"`
	Delivery           *T6Delivery `json:"delivery" sift:"required"`
	ChannelID          *string     `json:"channel_id" sift:"required,maxbytes=128"`
	SuggestedDowngrade *bool       `json:"suggested_downgrade" sift:"required"`
	Rationale          *string     `json:"rationale" sift:"required,minbytes=1,maxbytes=2000"`
}

func T6Contract(in T6Input) TouchpointContract {
	return TouchpointContract{Touchpoint: "T6", Asset: T6Asset(), FallbackOutput: func() []byte { return T6FallbackOutput(in) }, ValidateOutput: func(result []byte) ([]byte, error) {
		var out T6Output
		if err := schema.Decode(result, &out, schema.Closed); err != nil {
			return nil, err
		}
		if !contains(in.Candidate.ChannelCandidates, *out.ChannelID) || *out.Rationale != strings.TrimSpace(*out.Rationale) || hasControlOrNewline(*out.Rationale) {
			return nil, errors.New("brain: invalid T6 output")
		}
		severity := in.Candidate.Severity
		if *out.SuggestedDowngrade {
			severity = downgrade(severity)
		}
		if (severity == "high" || severity == "critical") && *out.Delivery != "immediate" || (*out.Delivery == "immediate" && severity != "critical" && in.Availability.State == "unavailable") || (*out.Delivery == "next_window" && (in.Availability.NextWindowAtMS == nil || *in.Availability.NextWindowAtMS >= in.Candidate.ExpiresAtMS)) {
			return nil, errors.New("brain: T6 delivery violates frozen scheduling constraints")
		}
		return schema.Canonical(out)
	}}
}

type T7Window struct {
	StartMS int64 `json:"start_ms"`
	EndMS   int64 `json:"end_ms"`
}
type T7EvidenceSummary struct {
	WindowStartMS             int64  `json:"window_start_ms"`
	WindowEndMS               int64  `json:"window_end_ms"`
	CertificationRulesVersion string `json:"certification_rules_version"`
	EvidenceDigest            string `json:"evidence_digest"`
	TotalSamples              int64  `json:"total_samples"`
	NegativeSamples           int64  `json:"negative_samples"`
	LeakCount                 int64  `json:"leak_count"`
	FalseBlockCount           int64  `json:"false_block_count"`
}
type T7CategoryEvidence struct {
	EvidenceID           string            `json:"evidence_id"`
	TaskKind             TaskKind          `json:"task_kind"`
	CertificationVersion string            `json:"certification_version"`
	Certified            bool              `json:"certified"`
	EvidenceSummary      T7EvidenceSummary `json:"evidence_summary"`
}
type T7ReplaySummary struct {
	EvidenceID      string `json:"evidence_id"`
	DatasetVersion  string `json:"dataset_version"`
	GateVersion     string `json:"gate_version"`
	TotalSamples    int64  `json:"total_samples"`
	NegativeSamples int64  `json:"negative_samples"`
	LeakCount       int64  `json:"leak_count"`
	FalseBlockCount int64  `json:"false_block_count"`
}
type T7SemanticMaterial struct {
	EntryID      string `json:"entry_id"`
	MaterialKind string `json:"material_kind"`
	Text         string `json:"text"`
}
type T7Input struct {
	AggregateKey     string               `json:"aggregate_key"`
	Window           T7Window             `json:"window"`
	Categories       []T7CategoryEvidence `json:"categories"`
	ReplaySummary    T7ReplaySummary      `json:"replay_summary"`
	SemanticMaterial []T7SemanticMaterial `json:"semantic_material"`
	// TraceProjectID and AllCategoryKinds are deterministic builder context and
	// are deliberately not sent to the model.
	TraceProjectID   string     `json:"-"`
	AllCategoryKinds []TaskKind `json:"-"`
}

func validDigest(s string) bool { return regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(s) }
func validCounts(total, negative, leaks, falseBlocks int64) bool {
	return total >= 0 && negative >= 0 && leaks >= 0 && falseBlocks >= 0 && negative <= total && leaks <= negative && falseBlocks <= total-negative
}

// BuildT7Input is the sole constructor for the closed §13.1 input.
func BuildT7Input(in T7Input) ([]byte, error) {
	parts, ok := aggregateParts(in.AggregateKey)
	if !ok {
		return nil, errors.New("brain: invalid T7 aggregate key")
	}
	if in.Window.StartMS != parts.start || in.Window.EndMS != parts.end {
		return nil, errors.New("brain: T7 window does not match aggregate key")
	}
	if parts.scope == "project" {
		if in.TraceProjectID == "" {
			return nil, errors.New("brain: T7 project trace identity is required")
		}
		decoded, err := base64.RawURLEncoding.DecodeString(parts.project)
		if err != nil || string(decoded) != in.TraceProjectID {
			return nil, errors.New("brain: T7 project does not match trace")
		}
	} else if in.TraceProjectID != "" {
		return nil, errors.New("brain: global T7 input cannot carry a project trace")
	}
	if len(in.Categories) < 1 || len(in.Categories) > 5 || len(in.SemanticMaterial) > 64 {
		return nil, errors.New("brain: invalid T7 evidence counts")
	}
	cats := append([]T7CategoryEvidence(nil), in.Categories...)
	for i := 1; i < len(cats); i++ {
		if cats[i-1].TaskKind >= cats[i].TaskKind {
			return nil, errors.New("brain: T7 categories must be sorted and unique")
		}
	}
	if parts.kind != "all" && (len(cats) != 1 || string(cats[0].TaskKind) != parts.kind) {
		return nil, errors.New("brain: T7 category does not match aggregate")
	}
	if parts.kind == "all" && in.AllCategoryKinds != nil && !sameTaskKinds(cats, in.AllCategoryKinds) {
		return nil, errors.New("brain: T7 all categories do not match deterministic aggregate")
	}
	seen := map[string]bool{}
	for _, c := range cats {
		if !validTaskKind(c.TaskKind) || len(c.EvidenceID) == 0 || len(c.EvidenceID) > 256 || seen[c.EvidenceID] || len(c.CertificationVersion) != 64 || !validDigest(c.CertificationVersion) || !validDigest(c.EvidenceSummary.CertificationRulesVersion) || !validDigest(c.EvidenceSummary.EvidenceDigest) || !validCounts(c.EvidenceSummary.TotalSamples, c.EvidenceSummary.NegativeSamples, c.EvidenceSummary.LeakCount, c.EvidenceSummary.FalseBlockCount) || c.EvidenceSummary.WindowStartMS < 0 || c.EvidenceSummary.WindowEndMS <= c.EvidenceSummary.WindowStartMS {
			return nil, errors.New("brain: invalid T7 category evidence")
		}
		seen[c.EvidenceID] = true
	}
	r := in.ReplaySummary
	if len(r.EvidenceID) == 0 || len(r.EvidenceID) > 256 || seen[r.EvidenceID] || len(r.DatasetVersion) == 0 || len(r.DatasetVersion) > 128 || len(r.GateVersion) == 0 || len(r.GateVersion) > 128 || !validCounts(r.TotalSamples, r.NegativeSamples, r.LeakCount, r.FalseBlockCount) {
		return nil, errors.New("brain: invalid T7 replay summary")
	}
	seen[r.EvidenceID] = true
	materials := append([]T7SemanticMaterial(nil), in.SemanticMaterial...)
	for i := 1; i < len(materials); i++ {
		if materials[i-1].EntryID >= materials[i].EntryID {
			return nil, errors.New("brain: T7 semantic material must be sorted and unique")
		}
	}
	for _, m := range materials {
		if len(m.EntryID) == 0 || len(m.EntryID) > 256 || seen[m.EntryID] || (m.MaterialKind != "reject_reason" && m.MaterialKind != "ask_text") || len(m.Text) == 0 || len(m.Text) > 16384 {
			return nil, errors.New("brain: invalid T7 semantic material")
		}
		seen[m.EntryID] = true
	}
	in.Categories = cats
	in.SemanticMaterial = materials
	if in.SemanticMaterial == nil {
		in.SemanticMaterial = []T7SemanticMaterial{}
	}
	return schema.Canonical(in)
}

func validTaskKinds(kinds []TaskKind) bool {
	if len(kinds) == 0 {
		return false
	}
	for i, kind := range kinds {
		if !validTaskKind(kind) || (i > 0 && kinds[i-1] >= kind) {
			return false
		}
	}
	return true
}

func sameTaskKinds(categories []T7CategoryEvidence, expected []TaskKind) bool {
	if len(categories) != len(expected) || !validTaskKinds(expected) {
		return false
	}
	for i, kind := range expected {
		if categories[i].TaskKind != kind {
			return false
		}
	}
	return true
}

func aggregateParts(key string) (struct {
	scope, project, kind string
	start, end           int64
}, bool) {
	var out struct {
		scope, project, kind string
		start, end           int64
	}
	p := strings.Split(key, ":")
	if len(p) == 6 && p[0] == "aggregate" && p[1] == "v1" {
		out.scope, out.kind = p[2], p[3]
		out.start, _ = strconv.ParseInt(p[4], 10, 64)
		out.end, _ = strconv.ParseInt(p[5], 10, 64)
	} else if len(p) == 7 && p[0] == "aggregate" && p[1] == "v1" && p[2] == "project" {
		out.scope, out.project, out.kind = p[2], p[3], p[4]
		out.start, _ = strconv.ParseInt(p[5], 10, 64)
		out.end, _ = strconv.ParseInt(p[6], 10, 64)
	} else {
		return out, false
	}
	_, valid := aggregateScope(key)
	return out, valid && out.end > out.start
}

type T7ProposalKind string

func (T7ProposalKind) EnumValues() []string { return []string{"policy", "context"} }

type T7TargetScope string

func (T7TargetScope) EnumValues() []string { return []string{"project", "global"} }

type T7Output struct {
	schema.ClosedType     `json:"-"`
	ProposalKind          *T7ProposalKind `json:"proposal_kind" sift:"required"`
	TargetScope           *T7TargetScope  `json:"target_scope" sift:"required"`
	Title                 *string         `json:"title" sift:"required,minbytes=1,maxbytes=160"`
	Body                  *string         `json:"body" sift:"required,minbytes=1,maxbytes=8192"`
	EvidenceEntryIDs      *[]string       `json:"evidence_entry_ids" sift:"required,minitems=1,maxitems=64,itemminbytes=1,itemmaxbytes=256"`
	RequiresHumanApproval *bool           `json:"requires_human_approval" sift:"required"`
}

// T7Contract validates an inert proposal against the aggregate scope and its
// deterministically selected evidence IDs. It deliberately has no action,
// Gate, Interrupt, or policy-write capability.
func T7Contract(aggregateKey, traceProjectID string, allCategoryKinds []TaskKind, evidenceIDs []string) TouchpointContract {
	return TouchpointContract{Touchpoint: "T7", Asset: T7Asset(), ValidateInput: func(p CallParams) error {
		var in T7Input
		if err := schema.Decode(p.Input, &in, schema.Closed); err != nil {
			return err
		}
		if p.Scope != storage.BrainScopeAggregate {
			return errors.New("brain: T7 scope does not match aggregate trace")
		}
		if p.SubjectKey != aggregateKey || in.AggregateKey != p.SubjectKey {
			return errors.New("brain: T7 aggregate key does not match trace subject")
		}
		if p.ProjectID != traceProjectID {
			return errors.New("brain: T7 project does not match trace")
		}
		parts, ok := aggregateParts(p.SubjectKey)
		if !ok {
			return errors.New("brain: invalid T7 aggregate key")
		}
		if parts.kind == "all" && !validTaskKinds(allCategoryKinds) {
			return errors.New("brain: T7 all categories require deterministic expected kinds")
		}
		in.TraceProjectID = p.ProjectID
		in.AllCategoryKinds = allCategoryKinds
		canonical, err := BuildT7Input(in)
		if err != nil {
			return err
		}
		if string(canonical) != string(p.Input) {
			return errors.New("brain: T7 input is not canonical")
		}
		return nil
	}, ValidateOutput: func(result []byte) ([]byte, error) {
		var out T7Output
		if err := schema.Decode(result, &out, schema.Closed); err != nil {
			return nil, err
		}
		scope, ok := aggregateScope(aggregateKey)
		if !ok || string(*out.TargetScope) != scope || !*out.RequiresHumanApproval || *out.Title != strings.TrimSpace(*out.Title) || hasControlOrNewline(*out.Title) || !validT7Body(*out.Body) {
			return nil, errors.New("brain: invalid T7 inert proposal")
		}
		seen := map[string]bool{}
		for n, id := range *out.EvidenceEntryIDs {
			if !contains(evidenceIDs, id) || seen[id] || (n > 0 && (*out.EvidenceEntryIDs)[n-1] >= id) {
				return nil, errors.New("brain: T7 evidence IDs must be supplied, sorted, and unique")
			}
			seen[id] = true
		}
		return schema.Canonical(out)
	}}
}

var optionID = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

func inEnum(v interface{ EnumValues() []string }) bool {
	s := fmt.Sprint(v)
	return contains(v.EnumValues(), s)
}
func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
func containsOption(options []T4Option, id string) bool {
	for _, o := range options {
		if o.ID == id {
			return true
		}
	}
	return false
}
func runeCount(s string) int { return utf8.RuneCountInString(s) }
func hasControlOrNewline(s string) bool {
	return strings.ContainsAny(s, "\r\n") || strings.IndexFunc(s, unicode.IsControl) >= 0
}
func validT4Link(s string) bool {
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "/") || regexp.MustCompile(`^sift://event/[0-9a-f]{32}$`).MatchString(s)
}
func downgrade(s InterruptSeverity) InterruptSeverity {
	if s == "critical" {
		return "high"
	}
	if s == "high" {
		return "normal"
	}
	if s == "normal" {
		return "low"
	}
	return "low"
}
func validT7Body(s string) bool {
	return !strings.ContainsAny(s, "\r\x00") && strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) < 0 && strings.TrimSpace(s) == s
}
func aggregateScope(key string) (string, bool) {
	p := strings.Split(key, ":")
	if len(p) < 6 || p[0] != "aggregate" || p[1] != "v1" {
		return "", false
	}
	if p[2] == "global" && len(p) == 6 && (p[3] == "all" || validTaskKind(TaskKind(p[3]))) {
		start, a := aggregateTime(p[4])
		end, b := aggregateTime(p[5])
		return "global", a && b && end > start
	}
	if p[2] == "project" && len(p) == 7 && len(p[3]) > 0 && (p[4] == "all" || validTaskKind(TaskKind(p[4]))) {
		if _, err := base64.RawURLEncoding.DecodeString(p[3]); err != nil {
			return "", false
		}
		start, a := aggregateTime(p[5])
		end, b := aggregateTime(p[6])
		return "project", a && b && end > start
	}
	return "", false
}

func aggregateTime(raw string) (int64, bool) {
	if raw == "" || (len(raw) > 1 && raw[0] == '0') {
		return 0, false
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	return value, err == nil && value >= 0
}
