package brain

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// Shell is the unified Brain call shell (brain.md §5). It is globally
// serial: every logical call runs pre-attempt gate → provider → closed
// decode → same-prompt retry once → touchpoint fallback, with the full
// call/attempt trace persisted through the storage brain ports and tokens
// charged at this single point.
type Shell struct {
	db       *storage.DB
	cfg      config.Brain
	provider Provider
	now      func() time.Time

	mu sync.Mutex // globally serial call flow (brain.md §6.1)
}

// NewShell wires the shell. now is injectable because storage logic never
// reads the wall clock (storage.md §1 invariant 7).
func NewShell(db *storage.DB, cfg config.Brain, provider Provider, now func() time.Time) *Shell {
	return &Shell{db: db, cfg: cfg, provider: provider, now: now}
}

// TouchpointContract is everything the shell needs about one touchpoint:
// the versioned assets, the closed output validator (including runtime-fact
// domain post-validation) and the deterministic fallback output.
type TouchpointContract struct {
	Touchpoint string
	Asset      PromptAsset
	// ValidateOutput closed-decodes one inner result_text and returns its
	// canonical form. Any error is a schema-level failure (brain.md §1.4:
	// no repair, no second LLM).
	ValidateOutput func(resultText []byte) (canonical []byte, err error)
	// FallbackOutput is the deterministic touchpoint fallback output; nil
	// means the fallback is a workflow state (T2: human assignment), not a
	// synthesized LLM output.
	FallbackOutput func() []byte
}

// CallParams is the logical-call identity plus its canonical input JSON.
type CallParams struct {
	Scope      string
	SubjectKey string
	ProjectID  string
	RunID      string
	AttemptNo  *int
	Input      []byte // canonical input JSON from BuildT1Input/BuildT2Input
}

// CallResult is the converged outcome of one logical call.
type CallResult struct {
	CallID              string
	CallSeq             int64
	Status              string // valid | fallback
	Output              []byte // validated canonical output (valid) or fallback output
	FallbackReason      string
	PromptVersion       string
	OutputSchemaVersion int
}

// Call executes one logical call per brain.md §5.
func (s *Shell) Call(ctx context.Context, tp TouchpointContract, p CallParams) (CallResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	message := BuildMessage(tp.Asset, p.Input)
	digest := DigestBytes(message)

	reserved, err := s.db.ReserveBrainCall(ctx, storage.ReserveBrainCallCmd{
		Scope:               p.Scope,
		SubjectKey:          p.SubjectKey,
		ProjectID:           p.ProjectID,
		RunID:               p.RunID,
		AttemptNo:           p.AttemptNo,
		Touchpoint:          tp.Touchpoint,
		PromptVersion:       tp.Asset.PromptVersion,
		OutputSchemaVersion: tp.Asset.OutputSchemaVersion,
		InputJSON:           p.Input,
		InputDigest:         digest,
		StartedAtMS:         s.now().UnixMilli(),
	})
	if err != nil {
		return CallResult{}, err
	}
	result := CallResult{CallID: reserved.ID, CallSeq: reserved.CallSeq, PromptVersion: tp.Asset.PromptVersion, OutputSchemaVersion: tp.Asset.OutputSchemaVersion}

	// Pre-flight gates (brain.md §5.2): each physical attempt re-checks; the
	// input bound is a per-call contract gate (§7.1/§8.1).
	gateReason := ""
	switch {
	case len(p.Input) > s.cfg.MaxInputBytes:
		gateReason = "input_too_large"
	case s.cfg.Executable == "":
		gateReason = "provider_disabled"
	case s.cfg.DailyTokenLimit == 0:
		gateReason = "provider_forbidden"
	}
	if gateReason != "" {
		return s.preflightFallback(ctx, tp, result, digest, gateReason)
	}

	maxAttempts := 1 + s.cfg.SchemaRetries
	lastFailure := "no attempt ran"
	for attemptNo := 1; attemptNo <= maxAttempts; attemptNo++ {
		start := s.now()
		consumed, err := s.db.TokenConsumed(ctx, storage.TokenBucketStartMS(start.UnixMilli()))
		if err != nil {
			return CallResult{}, err
		}
		if consumed >= int64(s.cfg.DailyTokenLimit) {
			// Token threshold: no more physical attempts (brain.md §6.1).
			if attemptNo == 1 {
				return s.preflightFallback(ctx, tp, result, digest, "token_budget_exceeded")
			}
			return s.finalizeFallback(ctx, tp, result, "token_budget_exceeded after attempt "+strconv.Itoa(attemptNo-1))
		}

		raw := s.provider.Call(ctx, ExecRequest{
			Prompt:         message,
			Timeout:        s.cfg.CallTimeout,
			MaxOutputBytes: int64(s.cfg.MaxRawOutputBytes),
		})
		finish := s.now()

		attempt, canonical, valid := s.classify(tp, raw, reserved.ID, attemptNo, digest, start.UnixMilli(), finish.UnixMilli())
		if _, err := s.db.RecordBrainAttempt(ctx, attempt); err != nil {
			return CallResult{}, err
		}
		if valid {
			if err := s.db.FinalizeBrainCall(ctx, storage.FinalizeBrainCallCmd{
				CallID:              reserved.ID,
				Status:              storage.BrainCallValid,
				SelectedAttemptNo:   &attemptNo,
				ValidatedOutputJSON: canonical,
				FinishedAtMS:        s.now().UnixMilli(),
			}); err != nil {
				return CallResult{}, err
			}
			result.Status = storage.BrainCallValid
			result.Output = canonical
			return result, nil
		}
		lastFailure = failureSummary(attempt)
	}
	return s.finalizeFallback(ctx, tp, result, "attempts exhausted: "+lastFailure)
}

// classify maps a raw provider result onto an immutable attempt command,
// returning the validated canonical output when the attempt is valid.
func (s *Shell) classify(tp TouchpointContract, raw ExecResult, callID string, attemptNo int, digest string, startMS, finishMS int64) (storage.BrainAttemptCmd, []byte, bool) {
	attempt := storage.BrainAttemptCmd{
		CallID:             callID,
		ProviderAttempt:    attemptNo,
		RequestDigest:      digest,
		StartedAtMS:        startMS,
		FinishedAtMS:       finishMS,
		TokenLimit:         int64(s.cfg.DailyTokenLimit),
		RawOutputTruncated: raw.StdoutTruncated,
		ExitCode:           raw.ExitCode,
	}
	if raw.StderrSummary != "" {
		attempt.StderrSummary = &raw.StderrSummary
		attempt.StderrTruncated = raw.StderrTruncated
	}

	providerErr := func(code string) (storage.BrainAttemptCmd, []byte, bool) {
		attempt.Outcome = storage.BrainAttemptProviderError
		attempt.ProviderErrorCode = code
		return attempt, nil, false
	}

	switch {
	case raw.SpawnErr != nil:
		return providerErr(storage.ProviderErrSpawnFailed)
	case raw.TimedOut:
		return providerErr(storage.ProviderErrTimeout)
	case raw.StdoutTruncated:
		// Save the first max_raw_output_bytes plus digest/bytes of the read
		// portion (brain.md §4.2).
		text := string(raw.Stdout)
		d := DigestBytes(raw.Stdout)
		n := int64(len(raw.Stdout))
		attempt.RawOutputText, attempt.RawOutputDigest, attempt.RawOutputBytes = &text, &d, &n
		return providerErr(storage.ProviderErrOutputTooLarge)
	case raw.ExitCode != nil && *raw.ExitCode != 0:
		text := string(raw.Stdout)
		d := DigestBytes(raw.Stdout)
		n := int64(len(raw.Stdout))
		attempt.RawOutputText, attempt.RawOutputDigest, attempt.RawOutputBytes = &text, &d, &n
		return providerErr(storage.ProviderErrNonzeroExit)
	}

	text := string(raw.Stdout)
	d := DigestBytes(raw.Stdout)
	n := int64(len(raw.Stdout))
	attempt.RawOutputText, attempt.RawOutputDigest, attempt.RawOutputBytes = &text, &d, &n

	resultText, inTok, outTok, err := ParseEnvelope(raw.Stdout)
	if err != nil {
		if ee, ok := err.(*EnvelopeError); ok {
			return providerErr(ee.Code)
		}
		return providerErr(storage.ProviderErrInvalidEnvelope)
	}
	attempt.InputTokens, attempt.OutputTokens = &inTok, &outTok

	canonical, err := tp.ValidateOutput(resultText)
	if err != nil {
		attempt.Outcome = storage.BrainAttemptInvalidOutput
		return attempt, nil, false
	}
	attempt.Outcome = storage.BrainAttemptValid
	return attempt, canonical, true
}

// preflightFallback records the provider_attempt=0 row and finalizes
// fallback for gates that fire before any provider ran (brain.md §3/§5.2).
func (s *Shell) preflightFallback(ctx context.Context, tp TouchpointContract, result CallResult, digest, reason string) (CallResult, error) {
	now := s.now().UnixMilli()
	if _, err := s.db.RecordBrainAttempt(ctx, storage.BrainAttemptCmd{
		CallID:          result.CallID,
		ProviderAttempt: 0,
		Outcome:         storage.BrainAttemptFallback,
		RequestDigest:   digest,
		StartedAtMS:     now,
		FinishedAtMS:    now,
		TokenLimit:      int64(s.cfg.DailyTokenLimit),
	}); err != nil {
		return CallResult{}, err
	}
	return s.finalizeFallback(ctx, tp, result, reason)
}

func (s *Shell) finalizeFallback(ctx context.Context, tp TouchpointContract, result CallResult, reason string) (CallResult, error) {
	if err := s.db.FinalizeBrainCall(ctx, storage.FinalizeBrainCallCmd{
		CallID:         result.CallID,
		Status:         storage.BrainCallFallback,
		FallbackReason: reason,
		FinishedAtMS:   s.now().UnixMilli(),
	}); err != nil {
		return CallResult{}, err
	}
	result.Status = storage.BrainCallFallback
	result.FallbackReason = reason
	if tp.FallbackOutput != nil {
		result.Output = tp.FallbackOutput()
	}
	return result, nil
}

// RecoverRunning converges leftover running calls after a daemon restart
// (brain.md §5): a call with a persisted valid attempt finalizes valid; any
// other call finalizes fallback with a recovery reason. It never replays a
// provider attempt that cannot be proven not to have run.
func (s *Shell) RecoverRunning(ctx context.Context, contracts map[string]TouchpointContract) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	running, err := s.db.RunningBrainCalls(ctx)
	if err != nil {
		return 0, err
	}
	converged := 0
	for _, call := range running {
		tp, ok := contracts[call.Touchpoint]
		if !ok {
			return converged, fmt.Errorf("brain: no contract for touchpoint %s", call.Touchpoint)
		}
		attempts, err := s.db.BrainCallAttempts(ctx, call.ID)
		if err != nil {
			return converged, err
		}
		var validAttempt *storage.BrainAttempt
		for i := range attempts {
			if attempts[i].Outcome == storage.BrainAttemptValid {
				validAttempt = &attempts[i]
			}
		}
		if validAttempt != nil && validAttempt.RawOutputText != nil {
			resultText, _, _, err := ParseEnvelope([]byte(*validAttempt.RawOutputText))
			if err == nil {
				if canonical, err := tp.ValidateOutput(resultText); err == nil {
					if err := s.db.FinalizeBrainCall(ctx, storage.FinalizeBrainCallCmd{
						CallID:              call.ID,
						Status:              storage.BrainCallValid,
						SelectedAttemptNo:   &validAttempt.ProviderAttempt,
						ValidatedOutputJSON: canonical,
						FinishedAtMS:        s.now().UnixMilli(),
					}); err != nil {
						return converged, err
					}
					converged++
					continue
				}
			}
		}
		if err := s.db.FinalizeBrainCall(ctx, storage.FinalizeBrainCallCmd{
			CallID:         call.ID,
			Status:         storage.BrainCallFallback,
			FallbackReason: "recovery: converged without a persisted valid attempt",
			FinishedAtMS:   s.now().UnixMilli(),
		}); err != nil {
			return converged, err
		}
		converged++
	}
	return converged, nil
}

func failureSummary(a storage.BrainAttemptCmd) string {
	if a.Outcome == storage.BrainAttemptProviderError {
		return a.ProviderErrorCode
	}
	return a.Outcome
}
