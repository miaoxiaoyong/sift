package forgeworker

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/xsift/sift/internal/forge"
	"github.com/xsift/sift/internal/storage"
)

// GateReEvaluationResultProducer assembles and verifies Gate facts outside the
// transaction (Forge/Brain reads), runs the pure Gate function and returns the
// canonical GateReEvaluationResultV1 bytes (storage.md §8.1). A Forge/assembly
// failure should normally be converted by the producer into a closed `failed`
// result union so the terminal protocol records the frozen failure evidence; a
// returned error aborts the attempt as retryable/failed.
type GateReEvaluationResultProducer func(ctx context.Context, payload storage.GateReEvaluationPayload) ([]byte, error)

// GateReEvaluationWorker executes gate_re_evaluation operations (storage.md
// §8.1). It mirrors the CommandAck/Comment pattern: claim, perform all
// Forge/Brain reads outside the transaction, then submit the closed result
// bytes to the single storage write port CompleteGateReEvaluation, which alone
// owns the lease CAS, Run/Interrupt assertions, terminal event, Run CAS and
// successor.
//
// The worker never calls EmitInterrupt or RecordGateEvaluation. Every §8.1
// verdict successor is wired in storage (HITL Interrupt successors, the
// ready/merge merge_change operation and the retry_checks/flaky_retry
// rerun_checks operation); the failed-arm failure_review successor is wired
// too. ErrGateReEvaluationSuccessorNotWired now only fires for a genuinely
// unknown verdict kind/code, in which case the worker terminates the operation
// so it is not permanently pending.
type GateReEvaluationWorker struct {
	DB       *storage.DB
	Produce  GateReEvaluationResultProducer
	Now      func() time.Time
	Lease    time.Duration
	WorkerID string
	// Complete overrides the storage terminal write port (tests).
	Complete func(ctx context.Context, claim storage.ClaimedOperation, resultJSON []byte, nowMS int64) error
}

// RunOnce claims at most one due gate_re_evaluation operation and drives it to
// a terminal Complete. It returns nil when there is nothing to claim.
func (w *GateReEvaluationWorker) RunOnce(ctx context.Context) error {
	now := time.Time{}
	if w.Now != nil {
		now = w.Now()
	}
	if now.IsZero() {
		now = time.UnixMilli(1)
	}
	c, err := w.DB.ClaimOutboxOperationKind(ctx, w.WorkerID, storage.OperationGateReEvaluation, now.UnixMilli(), w.Lease.Milliseconds())
	if err != nil || c == nil {
		return err
	}
	var payload storage.GateReEvaluationPayload
	if err = json.Unmarshal(c.Payload, &payload); err != nil || payload.OperationKey == "" || payload.OperationKey != c.Key {
		return w.finishFailed(ctx, *c, now.UnixMilli(), storage.ErrorContract, "invalid gate_re_evaluation payload")
	}
	if w.Produce == nil {
		return w.finishFailed(ctx, *c, now.UnixMilli(), storage.ErrorContract, "gate_re_evaluation producer not configured")
	}
	// Forge/Brain reads and Gate evaluation happen outside the transaction. The
	// attempt id is the stable charge-key base for every Forge call.
	ctx = forge.WithChargeKey(ctx, "gate-reeval:"+c.AttemptID)
	resultJSON, err := w.Produce(ctx, payload)
	if err != nil {
		return w.classified(ctx, *c, err, now)
	}
	if len(resultJSON) == 0 {
		return w.finishFailed(ctx, *c, now.UnixMilli(), storage.ErrorContract, "gate_re_evaluation producer returned no result")
	}
	complete := w.Complete
	if complete == nil {
		complete = w.DB.CompleteGateReEvaluation
	}
	if err := complete(ctx, *c, resultJSON, now.UnixMilli()); err != nil {
		if errors.Is(err, storage.ErrGateReEvaluationSuccessorNotWired) {
			// The submitted verdict requires a successor emission not yet wired
			// in this slice. Terminate the operation so it does not stay
			// pending; the Run is untouched by the rolled-back Complete.
			return w.finishFailed(ctx, *c, now.UnixMilli(), storage.ErrorContract, "gate_re_evaluation verdict successor not wired (deferred)")
		}
		if errors.Is(err, storage.ErrGateReEvaluationContract) {
			return w.finishFailed(ctx, *c, now.UnixMilli(), storage.ErrorContract, "gate_re_evaluation contract violation")
		}
		// Frozen-precondition failures (lost lease, moved Run, changed source
		// Interrupt) and unknown storage errors are surfaced to the daemon: the
		// lease reclaim path converges them without losing durable work.
		return err
	}
	return nil
}

// GateReEvaluationFailedResult builds the canonical closed `failed`
// GateReEvaluationResultV1 bytes for the given failure class and evidence. It
// is the fail-closed fallback used by daemon wiring when a producer cannot be
// routed or a run source cannot be resolved.
func GateReEvaluationFailedResult(class string, evidence map[string]string) []byte {
	evCanon, err := storage.CanonicalJSON(evidence)
	if err != nil {
		evCanon = []byte(`{"code":"schema_invalid","field":"unknown"}`)
	}
	body, err := storage.CanonicalJSON(map[string]any{
		"schema_version": 1,
		"kind":           "failed",
		"payload": map[string]any{
			"failure_class":    class,
			"failure_evidence": json.RawMessage(evCanon),
		},
	})
	if err != nil {
		return []byte(`{"kind":"failed","payload":{"failure_class":"gate_contract_failed","failure_evidence":{"code":"verdict_schema_invalid"}},"schema_version":1}`)
	}
	return body
}

func (w *GateReEvaluationWorker) finishFailed(ctx context.Context, c storage.ClaimedOperation, nowMS int64, class storage.ErrorClass, summary string) error {
	return w.DB.CompleteOutboxAttempt(ctx, c, storage.CompleteOutcome{State: storage.OperationFailed, ErrorClass: class, ErrorSummary: summary, NowMS: nowMS})
}

func (w *GateReEvaluationWorker) classified(ctx context.Context, c storage.ClaimedOperation, err error, now time.Time) error {
	var ce *forge.ClassifiedError
	o := storage.CompleteOutcome{State: storage.OperationRetryable, ErrorClass: storage.ErrorTransient, ErrorSummary: "gate_re_evaluation producer failed", NowMS: now.UnixMilli(), Backoff: storage.BackoffPolicy{InitialDelayMS: 1000, MaxDelayMS: 60000, Multiplier: 2}}
	if errors.As(err, &ce) {
		o.ErrorSummary = ce.Summary
		switch {
		case errors.Is(err, forge.ErrAuthOrCapability):
			o.State, o.ErrorClass = storage.OperationFailed, storage.ErrorAuthCapability
		case errors.Is(err, forge.ErrContractViolation):
			o.State, o.ErrorClass = storage.OperationFailed, storage.ErrorContract
		case errors.Is(err, forge.ErrSemanticConflict):
			o.State, o.ErrorClass = storage.OperationConflict, storage.ErrorSemanticConflict
		case errors.Is(err, forge.ErrRateLimited):
			o.ErrorClass = storage.ErrorRateLimited
			if !ce.RetryAt.IsZero() {
				o.RetryAfterMS = ce.RetryAt.Sub(now).Milliseconds()
				if o.RetryAfterMS < 0 {
					o.RetryAfterMS = 0
				}
			}
		}
	} else {
		o.ErrorSummary = err.Error()
	}
	return w.DB.CompleteOutboxAttempt(ctx, c, o)
}
