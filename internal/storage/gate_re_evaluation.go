package storage

import (
	"context"
	"errors"
	"fmt"
)

// Gate re-evaluation terminal protocol v1 (storage.md §8.1).
//
// CompleteGateReEvaluation is the sole write port that closes a
// gate_re_evaluation outbox operation. The worker assembles and verifies Gate
// facts outside the transaction, then submits the closed
// GateReEvaluationResultV1 canonical bytes. This method alone allocates the
// evaluation/event IDs, performs the lease + Run + source-Interrupt
// assertions, writes the terminal event, applies the Run CAS and persists any
// frozen successor in one transaction. It never calls EmitInterrupt or
// RecordGateEvaluation from the outside: the succeeded arm reuses the internal
// snapshot/cache/evaluation recorder inside this transaction.
//
// Scope of this implementation (storage.md §8.1 matrix):
//   - succeeded + verdicts with no successor Interrupt/operation:
//     failed/change_not_open, failed/hard_guardrail (Run -> failed(gate_verdict)),
//     wait_checks/checks_pending (Run -> running),
//     ready/no_auto_merge (Run -> done(gate_passed_no_auto_merge)).
//   - succeeded HITL verdicts: Run stays waiting_human (version+1) plus the
//     section 8.1 Interrupt successor in the same transaction.
//   - succeeded ready/merge: Run -> running(gate_merge_requested) plus the
//     sole merge_change successor operation enqueued in the same transaction
//     (Run CAS + terminal gate.reevaluation.ready.merge event).
//   - succeeded retry_checks/flaky_retry: Run -> running(gate_retry_checks) plus
//     the sole rerun_checks successor operation and one check_rerun_consumptions
//     row enqueued in the same transaction (Run CAS + terminal
//     gate.reevaluation.retry_checks.flaky_retry event).
//   - failed result union (forge_read_failed | gate_input_assembly_failed |
//     gate_contract_failed): terminal gate.reevaluation.failed event + Run CAS
//     (waiting_human, version+1) + failure_review Interrupt successor.
//   - conflict: replacement-head successor operation + Run CAS + terminal
//     gate.reevaluation.conflict event.
//
// Deferred to a follow-up slice (returned as ErrGateReEvaluationSuccessorNotWired
// so the worker can terminate the operation rather than leave it pending): none.
// The retry_checks/flaky_retry -> rerun_checks successor is wired below and
// consumed by RerunChecksWorker through the §8.5 request-start boundary.
//
// This slice wires all seven HITL verdict successors via closed
// GateReEvaluationInterruptV1 -> EmitInterrupt inside CompleteGateReEvaluation.
// It does not claim once-charge or M5 completion.
//
// The exact digest vectors in storage.md section 8.1 for the failed result union and
// the continuous conflict-to-replacement Complete are reproduced by the tests.

// ErrGateReEvaluationContract is a closed contract violation: non-canonical
// result bytes, an unknown result kind, a hash/version/digest mismatch, or an
// illegal verdict payload.
var ErrGateReEvaluationContract = errors.New("storage: gate re-evaluation contract violation")

// ErrGateReEvaluationSuccessorNotWired signals that the submitted result
// requires a successor whose emission is not yet wired in this slice. All
// §8.1 verdict and failed-arm successors are wired, including the
// retry_checks/flaky_retry -> rerun_checks successor; this error now only fires
// for a genuinely unknown verdict kind/code.
var ErrGateReEvaluationSuccessorNotWired = errors.New("storage: gate re-evaluation successor not wired")

// ErrGateReEvaluationAssertion signals that the frozen lease/Run/Interrupt/
// close-event/binding precondition did not hold.
var ErrGateReEvaluationAssertion = errors.New("storage: gate re-evaluation assertion failed")

// CompleteGateReEvaluation closes one claimed gate_re_evaluation operation
// using the §8.1 terminal protocol. resultJSON must be canonical
// GateReEvaluationResultV1 bytes.
func (d *DB) CompleteGateReEvaluation(ctx context.Context, claim ClaimedOperation, resultJSON []byte, nowMS int64) error {
	if claim.Kind != OperationGateReEvaluation {
		return fmt.Errorf("%w: not a gate_re_evaluation operation", ErrGateReEvaluationContract)
	}
	if nowMS <= 0 {
		return errors.New("storage: NowMS is required")
	}
	// Reject non-canonical bytes: the worker must submit canonical JSON. Any
	// drift (key order, whitespace, escaping) is a contract violation.
	canon, kind, payload, err := decodeGateReEvalResult(resultJSON)
	if err != nil {
		return err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	op, err := assertGateReEvalPreconditionsTx(ctx, tx, claim, nowMS)
	if err != nil {
		return err
	}
	switch kind {
	case "succeeded":
		err = d.completeGateReEvalSucceededTx(ctx, tx, claim, op, payload, canon, nowMS)
	case "failed":
		err = d.completeGateReEvalFailedTx(ctx, tx, claim, op, payload, canon, nowMS)
	case "conflict":
		err = d.completeGateReEvalConflictTx(ctx, tx, claim, op, payload, canon, nowMS)
	default:
		return fmt.Errorf("%w: unknown result kind %q", ErrGateReEvaluationContract, kind)
	}
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	d.wakeOutbox()
	return nil
}
