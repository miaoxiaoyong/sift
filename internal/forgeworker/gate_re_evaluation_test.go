package forgeworker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/storage"
)

const grNow int64 = 1_700_000_000_000

func grWorker(db *storage.DB, produce GateReEvaluationResultProducer, complete func(ctx context.Context, claim storage.ClaimedOperation, resultJSON []byte, nowMS int64) error) *GateReEvaluationWorker {
	return &GateReEvaluationWorker{DB: db, Produce: produce, Complete: complete, Now: func() time.Time { return time.UnixMilli(grNow) }, Lease: 60 * time.Second, WorkerID: "test:gate_re_evaluation"}
}

// seedGateReEvalOp enqueues a minimal gate_re_evaluation operation against a
// seeded run and returns its id. It does not drive the full Command/Gate
// precondition or claim the op; the worker claims it via RunOnce. The worker's
// terminal port is overridden in these tests.
func seedGateReEvalOp(t *testing.T, db *storage.DB) string {
	t.Helper()
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", grNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run-01", "project", "cfg", "42", grNow); err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("0", 40)
	opKey := "gate:int-01:" + head + ":reeval:1"
	payload, err := storage.CanonicalJSON(map[string]any{
		"source_interrupt_id":     "int-01",
		"source_command_event_id": "event:gate:int-01:" + head + ":reeval:1",
		"source_run_version":      1,
		"run_id":                  "run-01",
		"attempt_no":              1,
		"generation":              1,
		"change_id":               "change-01",
		"head_sha":                head,
		"gate_input_snapshot_id":  "snap-01",
		"gate_input_hash":         strings.Repeat("a", 64),
		"gate_version":            "gate/v1",
		"effect_binding_digest":   strings.Repeat("b", 64),
		"operation_key":           opKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	op := storage.Operation{
		Key: opKey, Kind: storage.OperationGateReEvaluation, Payload: payload, RunID: "run-01",
	}
	if _, err = db.EnqueueOperation(ctx, op, grNow); err != nil {
		t.Fatal(err)
	}
	return opKey
}

func opState(t *testing.T, db *storage.DB, key string) string {
	t.Helper()
	var s string
	if err := db.QueryRowForTest(context.Background(), `SELECT state FROM outbox_operations WHERE operation_key=?`, key).Scan(&s); err != nil {
		t.Fatalf("op state: %v", err)
	}
	return s
}

// TestGateReEvaluationWorkerSuccess drives claim -> produce -> Complete and
// verifies the terminal port receives canonical result bytes.
func TestGateReEvaluationWorkerSuccess(t *testing.T) {
	db := openWorkerDB(t)
	opKey := seedGateReEvalOp(t, db)
	var got storage.ClaimedOperation
	var gotResult []byte
	complete := func(ctx context.Context, c storage.ClaimedOperation, r []byte, nowMS int64) error {
		got, gotResult = c, r
		return nil
	}
	result := []byte(`{"kind":"failed","payload":{"failure_class":"gate_contract_failed","failure_evidence":{"code":"verdict_digest_mismatch"}},"schema_version":1}`)
	w := grWorker(db, func(ctx context.Context, p storage.GateReEvaluationPayload) ([]byte, error) { return result, nil }, complete)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got.Key != opKey {
		t.Fatalf("complete claim key = %s, want %s", got.Key, opKey)
	}
	if string(gotResult) != string(result) {
		t.Fatalf("complete result = %s", gotResult)
	}
}

// TestGateReEvaluationWorkerDeferredVerdictTerminatesOp verifies that a verdict
// whose successor is not wired terminates the operation rather than leaving it
// pending.
func TestGateReEvaluationWorkerDeferredVerdictTerminatesOp(t *testing.T) {
	db := openWorkerDB(t)
	opKey := seedGateReEvalOp(t, db)
	complete := func(ctx context.Context, c storage.ClaimedOperation, r []byte, nowMS int64) error {
		return storage.ErrGateReEvaluationSuccessorNotWired
	}
	w := grWorker(db, func(ctx context.Context, p storage.GateReEvaluationPayload) ([]byte, error) {
		return []byte(`{"kind":"succeeded","payload":{"gate_input_json":"{}","gate_input_hash":"x","gate_version":"gate/v1","verdict_json":"{}","verdict_digest":"y"},"schema_version":1}`), nil
	}, complete)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := opState(t, db, opKey); got != "failed" {
		t.Fatalf("op state = %s, want failed", got)
	}
}

// TestGateReEvaluationWorkerContractViolationTerminatesOp verifies a contract
// violation terminates the operation.
func TestGateReEvaluationWorkerContractViolationTerminatesOp(t *testing.T) {
	db := openWorkerDB(t)
	opKey := seedGateReEvalOp(t, db)
	complete := func(ctx context.Context, c storage.ClaimedOperation, r []byte, nowMS int64) error {
		return fmt.Errorf("%w: boom", storage.ErrGateReEvaluationContract)
	}
	w := grWorker(db, func(ctx context.Context, p storage.GateReEvaluationPayload) ([]byte, error) {
		return []byte(`{"kind":"succeeded","payload":{},"schema_version":1}`), nil
	}, complete)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := opState(t, db, opKey); got != "failed" {
		t.Fatalf("op state = %s, want failed", got)
	}
}

// TestGateReEvaluationWorkerProducerErrorIsRetryable verifies a producer error
// completes the op as retryable so the daemon reclaims it, rather than
// terminating it.
func TestGateReEvaluationWorkerProducerErrorIsRetryable(t *testing.T) {
	db := openWorkerDB(t)
	opKey := seedGateReEvalOp(t, db)
	w := grWorker(db, func(ctx context.Context, p storage.GateReEvaluationPayload) ([]byte, error) {
		return nil, errors.New("forge unavailable")
	}, func(ctx context.Context, c storage.ClaimedOperation, r []byte, nowMS int64) error {
		t.Fatal("complete should not run")
		return nil
	})
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := opState(t, db, opKey); got != "retryable" {
		t.Fatalf("op state = %s, want retryable", got)
	}
}

// TestGateReEvaluationWorkerEmptyResultTerminatesOp verifies an empty producer
// result is treated as a contract failure.
func TestGateReEvaluationWorkerEmptyResultTerminatesOp(t *testing.T) {
	db := openWorkerDB(t)
	opKey := seedGateReEvalOp(t, db)
	w := grWorker(db, func(ctx context.Context, p storage.GateReEvaluationPayload) ([]byte, error) { return nil, nil }, nil)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := opState(t, db, opKey); got != "failed" {
		t.Fatalf("op state = %s, want failed", got)
	}
}
