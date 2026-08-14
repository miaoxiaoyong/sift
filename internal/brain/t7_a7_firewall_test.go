package brain

import (
	"context"
	"strings"
	"testing"

	"github.com/xsift/sift/internal/config"
	"github.com/xsift/sift/internal/storage"
)

// A7 firewall at the Brain layer (brain.md §13 / WBS §5.1 T7 / wave1 I5).
// T7 reuses the unified Brain shell: the same Call → trace → token charge
// path as T1–T6, with PersistT7ProposalDraft as the single, inert write
// port. A valid terminal call yields one pending_human_approval draft; any
// fallback yields no draft and never touches Gate/Interrupt/policy.

const a7AggregateKey = "aggregate:v1:global:all:1:2"

func a7Shell(db *storage.DB, provider Provider) *Shell {
	// newShellAt seeds deterministic wall-clock times for the single charge.
	return newShellAt(db, shellCfg(100), provider, shellTestBase+1, shellTestBase+2, shellTestBase+3, shellTestBase+4, shellTestBase+5, shellTestBase+6)
}

func a7CallParams(t *testing.T) (CallParams, []string) {
	t.Helper()
	in := issue415T7Input(a7AggregateKey, "", []TaskKind{TaskBug})
	canonical, err := BuildT7Input(in)
	if err != nil {
		t.Fatalf("BuildT7Input: %v", err)
	}
	return CallParams{Scope: storage.BrainScopeAggregate, SubjectKey: a7AggregateKey, Input: canonical}, []string{"cat", "replay"}
}

// validT7ResultText builds a closed T7 inner output citing the supplied
// evidence id and proposal kind for a global aggregate.
func validT7ResultText(kind, evidenceID string) string {
	return `{"proposal_kind":"` + kind + `","target_scope":"global","title":"Review trend","body":"Human review only; this draft never auto-applies.","evidence_entry_ids":["` + evidenceID + `"],"requires_human_approval":true}`
}

// TestT7ValidCallPersistsInertDraftViaSingleShellPort proves the production
// T7 path: Shell.Call (the same unified shell, trace and charge as T4/T6) →
// PersistT7ProposalDraft (the sole write port) → exactly one immutable
// pending_human_approval draft. Both proposal kinds are inert text only.
func TestT7ValidCallPersistsInertDraftViaSingleShellPort(t *testing.T) {
	ctx := context.Background()
	db := openShellDB(t)

	for _, kind := range []string{"policy", "context"} {
		provider := &FakeProvider{Responses: []FakeResponse{{ResultText: validT7ResultText(kind, "cat"), InputTokens: 12, OutputTokens: 8}}}
		shell := a7Shell(db, provider)
		params, evidenceIDs := a7CallParams(t)
		result, err := shell.Call(ctx, T7Contract(a7AggregateKey, "", []TaskKind{TaskBug}, evidenceIDs), params)
		if err != nil {
			t.Fatalf("%s Shell.Call: %v", kind, err)
		}
		if result.Status != storage.BrainCallValid {
			t.Fatalf("%s call did not converge valid: %#v", kind, result)
		}
		// The shell is the single charge path: one provider attempt, one
		// terminal call, with the versioned asset bound to the draft.
		if result.PromptVersion == "" || result.OutputSchemaVersion != 1 {
			t.Fatalf("%s call missing versioned asset: %#v", kind, result)
		}

		draft, source, err := PersistT7ProposalDraft(ctx, db, result, a7AggregateKey, evidenceIDs, shellTestBase+5)
		if err != nil {
			t.Fatalf("%s PersistT7ProposalDraft: %v", kind, err)
		}
		if draft.ID == "" || draft.Status != "pending_human_approval" || draft.ProposalKind != kind ||
			draft.TargetScope != "global" || draft.AggregateKey != a7AggregateKey ||
			draft.LogicalCallID != result.CallID || draft.PromptVersion != result.PromptVersion {
			t.Fatalf("%s draft = %#v", kind, draft)
		}
		if draft.Body != "Human review only; this draft never auto-applies." || len(draft.EvidenceEntryIDs) != 1 || draft.EvidenceEntryIDs[0] != "cat" {
			t.Fatalf("%s draft content = %#v", kind, draft)
		}
		// The draft's provenance is the Brain shell, not a second emitter.
		if source.Kind != "brain" || source.LogicalCallID != result.CallID || source.PromptVersion != result.PromptVersion {
			t.Fatalf("%s draft source = %#v, want brain shell", kind, source)
		}

		// Read-back via the storage port is identical (single source of truth).
		readBack, err := db.ProposalDraft(ctx, result.CallID)
		if err != nil {
			t.Fatalf("%s ProposalDraft readback: %v", kind, err)
		}
		if readBack.ID != draft.ID || readBack.Status != "pending_human_approval" || readBack.ProposalKind != kind {
			t.Fatalf("%s readback = %#v", kind, readBack)
		}

		// Idempotent: persisting the same terminal call again returns the
		// identical draft and charges nothing extra.
		again, _, err := PersistT7ProposalDraft(ctx, db, result, a7AggregateKey, evidenceIDs, shellTestBase+6)
		if err != nil || again.ID != draft.ID {
			t.Fatalf("%s re-persist = %#v %v (want identical idempotent draft)", kind, again, err)
		}
	}
}

// TestT7FallbackNeverPersistsADraft proves the deterministic no-draft
// fallback (brain.md §13.3): every fallback reason produces zero proposal
// rows, and crucially the absence of a draft never changes Gate or Interrupt
// behavior (the firewall does not depend on T7 existing).
func TestT7FallbackNeverPersistsADraft(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		build  func() (config.Brain, Provider)
		reason string
	}{
		{"provider_disabled", func() (config.Brain, Provider) {
			cfg := shellCfg(100)
			cfg.Executable = ""
			return cfg, &FakeProvider{}
		}, "provider_disabled"},
		{"invalid_output", func() (config.Brain, Provider) {
			return shellCfg(100), &FakeProvider{Responses: []FakeResponse{{ResultText: `{"bogus":true}`}, {ResultText: `{"bogus":true}`}}}
		}, "invalid_output"},
		{"provider_error_nonzero_exit", func() (config.Brain, Provider) {
			return shellCfg(100), &FakeProvider{Responses: []FakeResponse{{ExitCode: intPtrExit(2), Stderr: "boom"}}}
		}, "provider_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openShellDB(t)
			cfg, provider := tc.build()
			shell := newShellAt(db, cfg, provider, shellTestBase+1, shellTestBase+2, shellTestBase+3, shellTestBase+4, shellTestBase+5, shellTestBase+6)
			params, evidenceIDs := a7CallParams(t)
			result, err := shell.Call(ctx, T7Contract(a7AggregateKey, "", []TaskKind{TaskBug}, evidenceIDs), params)
			if err != nil {
				t.Fatalf("%s Shell.Call: %v", tc.name, err)
			}
			if result.Status != storage.BrainCallFallback || result.FallbackReason != tc.reason {
				t.Fatalf("%s call = %#v, want fallback %q", tc.name, result, tc.reason)
			}
			draft, source, err := PersistT7ProposalDraft(ctx, db, result, a7AggregateKey, evidenceIDs, shellTestBase+5)
			if err != nil {
				t.Fatalf("%s PersistT7ProposalDraft: %v", tc.name, err)
			}
			if draft.ID != "" {
				t.Fatalf("%s fallback persisted a draft: %#v", tc.name, draft)
			}
			if source.Kind != "fallback" || !strings.HasPrefix(source.Version, "T7/fallback/v1") || source.Reason != tc.reason {
				t.Fatalf("%s fallback source = %#v", tc.name, source)
			}
			if _, err := db.ProposalDraft(ctx, result.CallID); err == nil {
				t.Fatalf("%s fallback left a readable proposal_draft row", tc.name)
			}
		})
	}
}

// intPtrExit returns a non-nil exit code for the fake provider.
func intPtrExit(code int) *int { return &code }
