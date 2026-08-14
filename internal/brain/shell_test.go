package brain

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xsift/sift/internal/config"
	"github.com/xsift/sift/internal/schema"
	"github.com/xsift/sift/internal/storage"
)

// Shell flow tests (brain.md §5/§6/§10): gate → provider → closed decode →
// same-prompt retry once → touchpoint fallback, with full traces and token
// charging. Storage runs on a real migrated SQLite DB; the provider is fake.

const shellTestBase = int64(1698796800000) // 2023-11-01 00:00:00 UTC

func openShellDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.Open(context.Background(), storage.OpenConfig{
		Path:          t.TempDir() + "/sift-home/sift.db",
		BinaryVersion: "test-binary",
		Now:           time.UnixMilli(shellTestBase),
	})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedIntakeSubject(t *testing.T, db *storage.DB, projectID string) {
	t.Helper()
	if err := db.SeedProjectForTest(context.Background(), "cfg-"+projectID, projectID, shellTestBase); err != nil {
		t.Fatal(err)
	}
}

func shellCfg(limit int) config.Brain {
	return config.Brain{
		Executable:        "fake-cli",
		Args:              []string{"-p"},
		Protocol:          config.BrainProtocolClaudeJSONv1,
		DailyTokenLimit:   limit,
		CallTimeout:       time.Minute,
		SchemaRetries:     1,
		MaxInputBytes:     262144,
		MaxRawOutputBytes: 1048576,
	}
}

func t1CallParams(t *testing.T, projectID string) CallParams {
	t.Helper()
	input, err := BuildT1Input(T1Input{
		Forge: T1Forge{Kind: "github", Host: "github.com", ProjectKey: "org/repo-" + projectID},
		Issue: T1Issue{ID: "42", Title: "Add feature", Body: "please", Author: "alice", URL: "https://x/42"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return CallParams{
		Scope:      storage.BrainScopeIntake,
		SubjectKey: "forge:github:github.com:org/repo-" + projectID + ":issue:42",
		ProjectID:  projectID,
		Input:      input,
	}
}

func newShellAt(db *storage.DB, cfg config.Brain, p Provider, times ...int64) *Shell {
	var mu int
	return NewShell(db, cfg, p, func() time.Time {
		if mu >= len(times) {
			return time.UnixMilli(times[len(times)-1])
		}
		t := times[mu]
		mu++
		return time.UnixMilli(t)
	})
}

func TestShellValidFirstAttempt(t *testing.T) {
	db := openShellDB(t)
	ctx := context.Background()
	seedIntakeSubject(t, db, "p1")
	fake := &FakeProvider{Responses: []FakeResponse{{ResultText: ValidT1ResultText(), InputTokens: 10, OutputTokens: 4}}}
	shell := newShellAt(db, shellCfg(1000), fake, shellTestBase+1, shellTestBase+2, shellTestBase+3, shellTestBase+4)

	res, err := shell.Call(ctx, T1Contract(nil), t1CallParams(t, "p1"))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Status != storage.BrainCallValid || res.CallSeq != 1 {
		t.Fatalf("result = %+v", res)
	}

	call, attempts, err := db.BrainCallTrace(ctx, res.CallID)
	if err != nil {
		t.Fatal(err)
	}
	if call.Status != storage.BrainCallValid || call.SelectedAttemptNo == nil || *call.SelectedAttemptNo != 1 {
		t.Fatalf("call = %+v", call)
	}
	if len(attempts) != 1 || attempts[0].Outcome != storage.BrainAttemptValid {
		t.Fatalf("attempts = %+v", attempts)
	}
	if attempts[0].RequestDigest != call.InputDigest {
		t.Fatal("request digest must equal frozen input digest")
	}
	// Tokens post-charged at the single charging point.
	consumed, err := db.TokenConsumed(ctx, storage.TokenBucketStartMS(shellTestBase+2))
	if err != nil || consumed != 14 {
		t.Fatalf("consumed = %d %v", consumed, err)
	}
}

func TestShellInvalidThenValidSamePrompt(t *testing.T) {
	db := openShellDB(t)
	ctx := context.Background()
	seedIntakeSubject(t, db, "p2")
	fake := &FakeProvider{Responses: []FakeResponse{
		{ResultText: `{"disposition":"maybe","questions":[],"possible_duplicate_run_id":null,"rationale":""}`, InputTokens: 5, OutputTokens: 5},
		{ResultText: ValidT1ResultText(), InputTokens: 6, OutputTokens: 4},
	}}
	shell := newShellAt(db, shellCfg(1000), fake, shellTestBase+1, shellTestBase+2, shellTestBase+3, shellTestBase+4, shellTestBase+5, shellTestBase+6)

	res, err := shell.Call(ctx, T1Contract(nil), t1CallParams(t, "p2"))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Status != storage.BrainCallValid {
		t.Fatalf("result = %+v", res)
	}
	call, attempts, err := db.BrainCallTrace(ctx, res.CallID)
	if err != nil {
		t.Fatal(err)
	}
	// Schema failure retried once with byte-identical prompt (§10.2).
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d", len(attempts))
	}
	if attempts[0].Outcome != storage.BrainAttemptInvalidOutput || attempts[1].Outcome != storage.BrainAttemptValid {
		t.Fatalf("outcomes = %s/%s", attempts[0].Outcome, attempts[1].Outcome)
	}
	if attempts[0].RequestDigest != attempts[1].RequestDigest || attempts[0].RequestDigest != call.InputDigest {
		t.Fatal("attempt digests must match the frozen call digest")
	}
	if !fake.LastRequestIdentical() {
		t.Fatal("retry prompt bytes drifted")
	}
	if call.SelectedAttemptNo == nil || *call.SelectedAttemptNo != 2 {
		t.Fatalf("selected = %v", call.SelectedAttemptNo)
	}
	// Retry is a real second cost, charged separately (§6.2).
	consumed, _ := db.TokenConsumed(ctx, storage.TokenBucketStartMS(shellTestBase+2))
	if consumed != 20 {
		t.Fatalf("consumed = %d, want 10+10", consumed)
	}
}

func TestShellInvalidThenFallback(t *testing.T) {
	db := openShellDB(t)
	ctx := context.Background()
	seedIntakeSubject(t, db, "p3")
	bad := FakeResponse{ResultText: `not json at all`, InputTokens: 1, OutputTokens: 1}
	fake := &FakeProvider{Responses: []FakeResponse{bad, bad}}
	shell := newShellAt(db, shellCfg(1000), fake, shellTestBase+1, shellTestBase+2, shellTestBase+3, shellTestBase+4, shellTestBase+5)

	res, err := shell.Call(ctx, T1Contract(nil), t1CallParams(t, "p3"))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Status != storage.BrainCallFallback || res.FallbackReason != "provider_error" {
		t.Fatalf("result = %+v", res)
	}
	// T1 fallback is the fixed ready output (§7.2): the issue is never lost.
	if string(res.Output) != string(T1FallbackOutput()) {
		t.Fatalf("fallback output = %s", res.Output)
	}
	call, attempts, _ := db.BrainCallTrace(ctx, res.CallID)
	if call.FallbackReason == "" || call.SelectedAttemptNo != nil {
		t.Fatalf("call = %+v", call)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d", len(attempts))
	}
}

func TestShellProviderErrorTaxonomy(t *testing.T) {
	cases := []struct {
		name string
		resp FakeResponse
		code string
	}{
		{name: "timeout", resp: FakeResponse{TimedOut: true}, code: storage.ProviderErrTimeout},
		{name: "nonzero_exit", resp: FakeResponse{RawStdout: []byte("boom"), ExitCode: intPtr(2)}, code: storage.ProviderErrNonzeroExit},
		{name: "usage_missing", resp: FakeResponse{RawStdout: []byte(`{"result_text":"{}"}`)}, code: storage.ProviderErrUsageMissing},
		{name: "usage_invalid", resp: FakeResponse{RawStdout: []byte(`{"result_text":"{}","usage":{"input_tokens":-1,"output_tokens":0}}`)}, code: storage.ProviderErrUsageInvalid},
		{name: "invalid_envelope", resp: FakeResponse{RawStdout: []byte(`{"usage":{"input_tokens":1,"output_tokens":1}}`)}, code: storage.ProviderErrInvalidEnvelope},
		{name: "spawn_failed", resp: FakeResponse{SpawnErr: true}, code: storage.ProviderErrSpawnFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openShellDB(t)
			ctx := context.Background()
			seedIntakeSubject(t, db, "px")
			fake := &FakeProvider{Responses: []FakeResponse{tc.resp, tc.resp}}
			shell := newShellAt(db, shellCfg(1000), fake, shellTestBase+1, shellTestBase+2, shellTestBase+3, shellTestBase+4, shellTestBase+5)
			res, err := shell.Call(ctx, T1Contract(nil), t1CallParams(t, "px"))
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			if res.Status != storage.BrainCallFallback {
				t.Fatalf("result = %+v", res)
			}
			_, attempts, err := db.BrainCallTrace(ctx, res.CallID)
			if err != nil {
				t.Fatal(err)
			}
			if len(attempts) != 2 {
				t.Fatalf("attempts = %d", len(attempts))
			}
			for _, a := range attempts {
				if a.Outcome != storage.BrainAttemptProviderError || a.ProviderErrorCode != tc.code {
					t.Fatalf("attempt = %+v, want provider_error/%s", a, tc.code)
				}
				// Usage failures are never billed.
				if a.InputTokens != nil {
					t.Fatalf("attempt billed tokens on %s", tc.code)
				}
			}
		})
	}
}

func TestShellPreflightGates(t *testing.T) {
	t.Run("provider_disabled", func(t *testing.T) {
		db := openShellDB(t)
		ctx := context.Background()
		seedIntakeSubject(t, db, "pd")
		fake := &FakeProvider{}
		cfg := shellCfg(1000)
		cfg.Executable = "" // deterministic mode (config.md §3.4)
		shell := newShellAt(db, cfg, fake, shellTestBase+1, shellTestBase+2, shellTestBase+3)
		res, err := shell.Call(ctx, T1Contract(nil), t1CallParams(t, "pd"))
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != storage.BrainCallFallback || res.FallbackReason != "provider_disabled" {
			t.Fatalf("result = %+v", res)
		}
		_, attempts, _ := db.BrainCallTrace(ctx, res.CallID)
		if len(attempts) != 1 || attempts[0].ProviderAttempt != 0 || attempts[0].Outcome != storage.BrainAttemptFallback {
			t.Fatalf("attempts = %+v", attempts)
		}
		if attempts[0].InputTokens != nil || attempts[0].ExitCode != nil || attempts[0].RawOutputText != nil {
			t.Fatal("attempt 0 must carry no provider facts")
		}
		if len(fake.Requests) != 0 {
			t.Fatal("provider must not be invoked")
		}
	})
	t.Run("token_limit_zero", func(t *testing.T) {
		db := openShellDB(t)
		ctx := context.Background()
		seedIntakeSubject(t, db, "pz")
		fake := &FakeProvider{}
		shell := newShellAt(db, shellCfg(0), fake, shellTestBase+1, shellTestBase+2, shellTestBase+3)
		res, err := shell.Call(ctx, T1Contract(nil), t1CallParams(t, "pz"))
		if err != nil {
			t.Fatal(err)
		}
		if res.FallbackReason != "provider_disabled" {
			t.Fatalf("result = %+v", res)
		}
		_, attempts, _ := db.BrainCallTrace(ctx, res.CallID)
		if len(attempts) != 1 || attempts[0].ProviderAttempt != 0 {
			t.Fatalf("attempts = %+v", attempts)
		}
	})
	t.Run("token_threshold_before_first_attempt", func(t *testing.T) {
		db := openShellDB(t)
		ctx := context.Background()
		seedIntakeSubject(t, db, "pt")
		fake := &FakeProvider{Responses: []FakeResponse{{ResultText: ValidT1ResultText(), InputTokens: 100, OutputTokens: 0}}}
		shell := newShellAt(db, shellCfg(50), fake, shellTestBase+1, shellTestBase+2, shellTestBase+3, shellTestBase+4)
		res, err := shell.Call(ctx, T1Contract(nil), t1CallParams(t, "pt"))
		if err != nil {
			t.Fatal(err)
		}
		// Attempt 1 fires (0 < 50) and post-charges 100, crossing the limit.
		if res.Status != storage.BrainCallValid {
			t.Fatalf("first call = %+v", res)
		}
		// Second call: gate blocks before any provider run (consumed >= limit).
		fake.Responses = []FakeResponse{{ResultText: ValidT1ResultText(), InputTokens: 1, OutputTokens: 1}}
		shell2 := newShellAt(db, shellCfg(50), fake, shellTestBase+10, shellTestBase+11, shellTestBase+12)
		params := t1CallParams(t, "pt")
		params.SubjectKey = "forge:github:github.com:org/repo-pt:issue:43"
		res2, err := shell2.Call(ctx, T1Contract(nil), params)
		if err != nil {
			t.Fatal(err)
		}
		if res2.Status != storage.BrainCallFallback || res2.FallbackReason != "token_threshold" {
			t.Fatalf("second call = %+v", res2)
		}
		_, attempts, _ := db.BrainCallTrace(ctx, res2.CallID)
		if len(attempts) != 1 || attempts[0].ProviderAttempt != 0 {
			t.Fatalf("attempts = %+v", attempts)
		}
	})
	t.Run("attempt1_over_limit_blocks_attempt2", func(t *testing.T) {
		db := openShellDB(t)
		ctx := context.Background()
		seedIntakeSubject(t, db, "po")
		fake := &FakeProvider{Responses: []FakeResponse{
			// Valid envelope + usage, but the inner output fails closed decode:
			// the attempt is billed and fails the schema gate.
			{ResultText: `{"nope":1}`, InputTokens: 60, OutputTokens: 0},
			{ResultText: ValidT1ResultText(), InputTokens: 1, OutputTokens: 1},
		}}
		// limit 50: attempt 1 runs, post-charges 60 (over), attempt 2 must
		// NOT be launched even though the retry would have succeeded (§6.1).
		shell := newShellAt(db, shellCfg(50), fake, shellTestBase+1, shellTestBase+2, shellTestBase+3, shellTestBase+4, shellTestBase+5)
		res, err := shell.Call(ctx, T1Contract(nil), t1CallParams(t, "po"))
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != storage.BrainCallFallback || res.FallbackReason != "token_threshold" {
			t.Fatalf("result = %+v", res)
		}
		if len(fake.Requests) != 1 {
			t.Fatalf("provider invoked %d times, want exactly 1", len(fake.Requests))
		}
		consumed, _ := db.TokenConsumed(ctx, storage.TokenBucketStartMS(shellTestBase+2))
		if consumed != 60 {
			t.Fatalf("consumed = %d, want full over-limit charge 60", consumed)
		}
	})
	t.Run("input_too_large", func(t *testing.T) {
		db := openShellDB(t)
		ctx := context.Background()
		seedIntakeSubject(t, db, "pi")
		fake := &FakeProvider{}
		cfg := shellCfg(1000)
		cfg.MaxInputBytes = 64
		shell := newShellAt(db, cfg, fake, shellTestBase+1, shellTestBase+2, shellTestBase+3)
		res, err := shell.Call(ctx, T1Contract(nil), t1CallParams(t, "pi"))
		if err != nil {
			t.Fatal(err)
		}
		if res.FallbackReason != "input_too_large" || len(fake.Requests) != 0 {
			t.Fatalf("result = %+v requests %d", res, len(fake.Requests))
		}
		if string(res.Output) != string(T1FallbackOutput()) {
			t.Fatal("oversize input still converges to the T1 ready fallback")
		}
	})
}

func TestShellZeroUsageNoCharge(t *testing.T) {
	db := openShellDB(t)
	ctx := context.Background()
	seedIntakeSubject(t, db, "p0")
	fake := &FakeProvider{Responses: []FakeResponse{{ResultText: ValidT1ResultText(), InputTokens: 0, OutputTokens: 0}}}
	shell := newShellAt(db, shellCfg(1000), fake, shellTestBase+1, shellTestBase+2, shellTestBase+3, shellTestBase+4)
	if _, err := shell.Call(ctx, T1Contract(nil), t1CallParams(t, "p0")); err != nil {
		t.Fatal(err)
	}
	consumed, err := db.TokenConsumed(ctx, storage.TokenBucketStartMS(shellTestBase+2))
	if err != nil || consumed != 0 {
		t.Fatalf("consumed = %d %v", consumed, err)
	}
}

func TestShellCrossMidnightBucketFrozenAtStart(t *testing.T) {
	db := openShellDB(t)
	ctx := context.Background()
	seedIntakeSubject(t, db, "pm")
	const dayMS = 24 * 60 * 60 * 1000
	start := shellTestBase + dayMS - 5000  // 23:59:55 day 1
	finish := shellTestBase + dayMS + 1000 // 00:00:01 day 2
	fake := &FakeProvider{Responses: []FakeResponse{{ResultText: ValidT1ResultText(), InputTokens: 9, OutputTokens: 1}}}
	// reserve@start-1, gate@start, provider returns, finish@finish, finalize@finish+1
	shell := newShellAt(db, shellCfg(1000), fake, start-1, start, start, finish, finish+1)
	res, err := shell.Call(ctx, T1Contract(nil), t1CallParams(t, "pm"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != storage.BrainCallValid {
		t.Fatalf("result = %+v", res)
	}
	// Charged to the bucket frozen at attempt start (day 1), not finish (§6.5).
	day1, _ := db.TokenConsumed(ctx, storage.TokenBucketStartMS(start))
	day2, _ := db.TokenConsumed(ctx, storage.TokenBucketStartMS(finish))
	if day1 != 9+1 || day2 != 0 {
		t.Fatalf("day1 = %d day2 = %d, want 10/0", day1, day2)
	}
}

func TestShellRecovery(t *testing.T) {
	db := openShellDB(t)
	ctx := context.Background()
	seedIntakeSubject(t, db, "pr")

	// Simulate a crash: reserve + one valid attempt, no finalize.
	params := t1CallParams(t, "pr")
	message := BuildMessage(T1Asset(), params.Input)
	digest := DigestBytes(message)
	reserved, err := db.ReserveBrainCall(ctx, storage.ReserveBrainCallCmd{
		Scope: params.Scope, SubjectKey: params.SubjectKey, ProjectID: params.ProjectID,
		Touchpoint: "T1", PromptVersion: T1Asset().PromptVersion, OutputSchemaVersion: 1,
		InputJSON: params.Input, InputDigest: digest, StartedAtMS: shellTestBase + 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := FakeEnvelope(ValidT1ResultText(), 1, 1)
	rawText := string(raw)
	rawDigest := DigestBytes(raw)
	rawBytes := int64(len(raw))
	inTok, outTok := int64(1), int64(1)
	if _, err := db.RecordBrainAttempt(ctx, storage.BrainAttemptCmd{
		CallID: reserved.ID, ProviderAttempt: 1, Outcome: storage.BrainAttemptValid,
		RequestDigest: digest,
		RawOutputText: &rawText, RawOutputDigest: &rawDigest, RawOutputBytes: &rawBytes,
		InputTokens: &inTok, OutputTokens: &outTok,
		StartedAtMS: shellTestBase + 2, FinishedAtMS: shellTestBase + 3, TokenLimit: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	// A second leftover call with no attempts converges to fallback.
	orphan, err := db.ReserveBrainCall(ctx, storage.ReserveBrainCallCmd{
		Scope: params.Scope, SubjectKey: params.SubjectKey + ":other", ProjectID: params.ProjectID,
		Touchpoint: "T1", PromptVersion: T1Asset().PromptVersion, OutputSchemaVersion: 1,
		InputJSON: params.Input, InputDigest: digest, StartedAtMS: shellTestBase + 4,
	})
	if err != nil {
		t.Fatal(err)
	}

	shell := newShellAt(db, shellCfg(1000), &FakeProvider{}, shellTestBase+10, shellTestBase+11)
	converged, err := shell.RecoverRunning(ctx, map[string]TouchpointContract{"T1": T1Contract(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if converged != 2 {
		t.Fatalf("converged = %d", converged)
	}

	call, _, err := db.BrainCallTrace(ctx, reserved.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Recovery finalizes valid from the persisted attempt, never replaying
	// the provider (brain.md §5).
	if call.Status != storage.BrainCallValid || call.SelectedAttemptNo == nil || *call.SelectedAttemptNo != 1 {
		t.Fatalf("recovered call = %+v", call)
	}
	orphanCall, _, err := db.BrainCallTrace(ctx, orphan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if orphanCall.Status != storage.BrainCallFallback || !strings.Contains(orphanCall.FallbackReason, "recovery") {
		t.Fatalf("orphan call = %+v", orphanCall)
	}
}

func TestShellT2ValidThenTaskSpec(t *testing.T) {
	db := openShellDB(t)
	ctx := context.Background()
	seedIntakeSubject(t, db, "p2s")
	if err := db.SeedForgeRunForTest(ctx, "run-t2", "p2s", "cfg-p2s", "issue-42", shellTestBase); err != nil {
		t.Fatal(err)
	}

	input, err := BuildT2Input(T2Input{
		RunID: "run-t2",
		Issue: T2Issue{Title: "Add feature", Body: "please", URL: "https://x/42"},
		CandidateAgents: []T2AgentCandidate{
			{ID: "claude-code", Capabilities: []string{"go"}},
			{ID: "fake-agent", Capabilities: nil},
		},
		BaseContext: T2BaseContext{ProjectContext: "proj ctx", GlobalContext: "global ctx"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// M1 fake chain: the fake provider emits a legal T2 output (§10.9).
	fake := &FakeProvider{Responses: []FakeResponse{{
		ResultText:   ValidT2ResultText(TaskFeature, "claude-code", []string{"implement it"}, false),
		InputTokens:  12,
		OutputTokens: 6,
	}}}
	shell := newShellAt(db, shellCfg(1000), fake, shellTestBase+1, shellTestBase+2, shellTestBase+3, shellTestBase+4)
	res, err := shell.Call(ctx, T2Contract([]string{"claude-code", "fake-agent"}), CallParams{
		Scope: storage.BrainScopeRun, SubjectKey: "run:run-t2", RunID: "run-t2", Input: input,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != storage.BrainCallValid {
		t.Fatalf("result = %+v", res)
	}
	var out T2Output
	if err := schema.Decode(res.Output, &out, schema.Closed); err != nil {
		t.Fatal(err)
	}

	canonical, digest, err := AssembleTaskSpec(TaskSpecParams{
		Title: "Add feature", Body: "please", SourceURL: "https://x/42",
		Goals: *out.Goals, PolicyHash: "policyhash",
		ProjectContext: ContextSegment{Text: "proj ctx"}, GlobalContext: ContextSegment{Text: "global ctx"},
		Kind: *out.Kind, Agent: *out.Agent, HITLBeforeStart: EffectiveHITL(*out.HITLBeforeStart, false),
		LogicalCallID: res.CallID, PromptVersion: T2Asset().PromptVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if digest == "" || !strings.Contains(string(canonical), `"logical_call_id":"`+res.CallID+`"`) {
		t.Fatalf("task spec = %s", canonical)
	}
}

func intPtr(v int) *int { return &v }

// TestShellWithRealCLIFixture drives the unified shell against the real
// subprocess CLI shell (fixture helper process), proving the integration:
// schema failure on attempt 1 → same-prompt subprocess retry → valid.
func TestShellWithRealCLIFixture(t *testing.T) {
	db := openShellDB(t)
	ctx := context.Background()
	seedIntakeSubject(t, db, "pf")
	fx, st, cap := writeFixture(t,
		cliBehavior{Stdout: string(FakeEnvelope(`{"disposition":"bogus","questions":[],"possible_duplicate_run_id":null,"rationale":""}`, 3, 2))},
		cliBehavior{Stdout: string(FakeEnvelope(ValidT1ResultText(), 4, 3))},
	)
	setHelperEnv(t, fx, st, cap)
	provider := fixtureProvider(t, fx, st, cap)
	shell := newShellAt(db, shellCfg(1000), provider,
		shellTestBase+1, shellTestBase+2, shellTestBase+3, shellTestBase+4, shellTestBase+5, shellTestBase+6)

	res, err := shell.Call(ctx, T1Contract(nil), t1CallParams(t, "pf"))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Status != storage.BrainCallValid {
		t.Fatalf("result = %+v", res)
	}
	call, attempts, err := db.BrainCallTrace(ctx, res.CallID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || attempts[0].Outcome != storage.BrainAttemptInvalidOutput ||
		attempts[1].Outcome != storage.BrainAttemptValid {
		t.Fatalf("attempts = %+v", attempts)
	}
	// Both subprocess invocations received byte-identical prompt bytes.
	first := readStdinCapture(t, cap, 0)
	second := readStdinCapture(t, cap, 1)
	if string(first) != string(second) || DigestBytes(first) != call.InputDigest {
		t.Fatal("subprocess retry prompt bytes must be identical and match the frozen digest")
	}
	// Real usage was billed per physical attempt: 5 + 7.
	consumed, _ := db.TokenConsumed(ctx, storage.TokenBucketStartMS(shellTestBase+2))
	if consumed != 12 {
		t.Fatalf("consumed = %d, want 12", consumed)
	}
}
