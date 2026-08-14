package brain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/xsift/sift/internal/schema"
)

// Task Spec v1 assembler (brain.md §9). The assembler is deterministic:
// description, goals, guardrails and the three context slices arrive already
// frozen by their owners (context path facts live in specs/config.md), and
// guardrails come only from the valid policy or hardcoded defaults — never
// from T2 output. The canonical JSON + digest form the immutable snapshot
// content.

// TaskSpecSchemaVersion is the v1 snapshot schema version.
const TaskSpecSchemaVersion = 1

// ContextSegment is one frozen context slice with its text; the hash is
// derived, so a caller cannot assert a digest that does not match the text.
// A missing slice is the empty text, whose hash is the SHA-256 of the
// prescribed empty content (config.md §4).
type ContextSegment struct {
	Text string
}

// Hash returns the SHA-256 lowercase-hex of the segment text.
func (c ContextSegment) Hash() string {
	sum := sha256.Sum256([]byte(c.Text))
	return hex.EncodeToString(sum[:])
}

// TaskSpecParams carries the four frozen sources of a Task Spec:
// Description (issue), Goals (validated T2 output), Guardrails (valid
// policy/default), Context (project/global/task annotations).
type TaskSpecParams struct {
	Title     string
	Body      string
	SourceURL string

	Goals []string

	PolicyHash string
	Rules      []string // hard guardrails from the valid policy; empty default

	ProjectContext  ContextSegment
	GlobalContext   ContextSegment
	TaskAnnotations []T2Annotation

	Kind            TaskKind
	Agent           string
	HITLBeforeStart bool

	LogicalCallID string
	PromptVersion string
}

// AssembleTaskSpec builds the §9 v1 document in the fixed
// Description → Goals → Guardrails → Context order, then returns its
// canonical JSON and content digest for the immutable snapshot.
func AssembleTaskSpec(p TaskSpecParams) (canonical []byte, digest string, err error) {
	if p.Title == "" {
		return nil, "", errors.New("brain: task spec description title is required")
	}
	if len(p.Goals) == 0 {
		return nil, "", errors.New("brain: task spec requires at least one goal")
	}
	if p.PolicyHash == "" {
		return nil, "", errors.New("brain: task spec guardrails policy_hash is required")
	}
	if p.Kind == "" || p.Agent == "" {
		return nil, "", errors.New("brain: task spec assignment kind/agent are required")
	}
	if p.LogicalCallID == "" || p.PromptVersion == "" {
		return nil, "", errors.New("brain: task spec brain provenance is required")
	}
	goals := append([]string(nil), p.Goals...)
	rules := append([]string(nil), p.Rules...)
	if rules == nil {
		rules = []string{}
	}
	annotations := append([]T2Annotation(nil), p.TaskAnnotations...)
	if annotations == nil {
		annotations = []T2Annotation{}
	}
	annDocs := make([]map[string]any, 0, len(annotations))
	for _, a := range annotations {
		if a.EventID == "" {
			return nil, "", errors.New("brain: task annotation event_id is required")
		}
		annDocs = append(annDocs, map[string]any{"event_id": a.EventID, "text": a.Text})
	}

	doc := map[string]any{
		"schema_version": TaskSpecSchemaVersion,
		"description": map[string]any{
			"title":      p.Title,
			"body":       p.Body,
			"source_url": p.SourceURL,
		},
		"goals": goals,
		"guardrails": map[string]any{
			"policy_hash": p.PolicyHash,
			"rules":       rules,
		},
		"context": map[string]any{
			"project": map[string]any{
				"blob_hash": p.ProjectContext.Hash(),
				"text":      p.ProjectContext.Text,
			},
			"global": map[string]any{
				"content_hash": p.GlobalContext.Hash(),
				"text":         p.GlobalContext.Text,
			},
			"task_annotations": annDocs,
		},
		"assignment": map[string]any{
			"kind":              string(p.Kind),
			"agent":             p.Agent,
			"hitl_before_start": p.HITLBeforeStart,
		},
		"brain": map[string]any{
			"logical_call_id": p.LogicalCallID,
			"prompt_version":  p.PromptVersion,
		},
	}
	canonical, err = schema.Canonical(doc)
	if err != nil {
		return nil, "", fmt.Errorf("brain: canonical task spec: %w", err)
	}
	return canonical, DigestBytes(canonical), nil
}
