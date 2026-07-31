package storage

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/command"
)

// TestAttentionOnceChargeAcrossDeliveryCommandAndRestart closes the WBS §5.2
// lifecycle invariant: only the initial Interrupt generation charges
// attention. Scheduling, Channel retries, escalation, Command ack retries,
// restart replay and Forge API charging reuse that authority without a second
// attention write.
func TestAttentionOnceChargeAcrossDeliveryCommandAndRestart(t *testing.T) {
	db, path := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)

	batchAt := int64(testNow + 10)
	cmd := t6Command(testNow)
	cmd.RunID = cmdRun
	cmd.Reason = InterruptDesignApproval
	cmd.Facts = map[string]string{
		"risk_summary":       "high",
		"recommended_action": "approve",
		"task_spec_ref":      "/r/task/" + cmdRun,
	}
	cmd.Generation = InterruptGeneration{TaskSpecSnapshotID: "task-01"}
	cmd.GatePhase = GateNone
	cmd.ExpiresAfterMS = 100
	cmd.OnExpire = ExpireEscalate
	cmd.OnMaxEscalations = ExpireHold
	cmd.MaxEscalations = 2
	cmd.BatchAtMS = &batchAt
	cmd.Channels = []InterruptChannel{coexistOpsChannel()}
	cmd.T6 = func(context.Context, InterruptT6Input) (InterruptT6Output, error) {
		return InterruptT6Output{Delivery: "batch", ChannelID: "ops"}, nil
	}

	// Concurrent producer replay uses the same generation key. Every caller
	// receives the same Interrupt and only one admission/charge is inserted.
	const producers = 8
	var wg sync.WaitGroup
	ids := make(chan string, producers)
	errs := make(chan error, producers)
	for range producers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in, err := db.EmitInterrupt(ctx, cmd)
			if err != nil {
				errs <- err
				return
			}
			ids <- in.ID
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent EmitInterrupt: %v", err)
	}
	var interruptID string
	for id := range ids {
		if interruptID == "" {
			interruptID = id
		} else if id != interruptID {
			t.Fatalf("generation forked: %s != %s", id, interruptID)
		}
	}
	assertAttentionLifecycleCounts(t, db, interruptID, 1, 1, 0)

	// The due batch creates one immutable member and one Channel operation.
	if err := db.SupervisorInterruptTick(ctx, batchAt); err != nil {
		t.Fatal(err)
	}
	assertAttentionLifecycleCounts(t, db, interruptID, 1, 1, 1)
	completeChannelWithResponseLossReplay(t, ctx, db, testNow+20)
	assertAttentionLifecycleCounts(t, db, interruptID, 1, 1, 1)

	// Expiry promotes normal->high. The first tick freezes immediate
	// redelivery; the second enqueues it. The old batch member is excluded, not
	// replaced, and the original attention charge remains authoritative.
	if err := db.SupervisorInterruptTick(ctx, testNow+100); err != nil {
		t.Fatal(err)
	}
	if err := db.SupervisorInterruptTick(ctx, testNow+101); err != nil {
		t.Fatal(err)
	}
	completeChannelWithResponseLossReplay(t, ctx, db, testNow+110)
	assertAttentionLifecycleCounts(t, db, interruptID, 1, 1, 1)

	// Restart and replay the original generation. The stale Run version is
	// irrelevant on a generation-key hit; no T6 call or charge is repeated.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(ctx, OpenConfig{Path: path, BinaryVersion: "test-binary", Now: time.UnixMilli(testNow + 200)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cmd.T6 = func(context.Context, InterruptT6Input) (InterruptT6Output, error) {
		t.Fatal("T6 called for replayed generation")
		return InterruptT6Output{}, nil
	}
	replayed, err := db.EmitInterrupt(ctx, cmd)
	if err != nil || replayed.ID != interruptID {
		t.Fatalf("restart replay=%+v err=%v", replayed, err)
	}
	assertAttentionLifecycleCounts(t, db, interruptID, 1, 1, 1)

	// A real accepted Command emits command_ack. Retry/reclaim its outbox
	// lifecycle without touching attention accounting.
	var nonce string
	if err := db.db.QueryRow(`SELECT nonce FROM interrupts WHERE id=?`, interruptID).Scan(&nonce); err != nil {
		t.Fatal(err)
	}
	env := commentEnv(t, "project", "once-command", "/sift hold "+cmdRun+" "+nonce+" 30m")
	result, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 210})
	if err != nil || result.Outcome != command.OutcomeApplied {
		t.Fatalf("ApplyCommandEvent outcome=%s err=%v", result.Outcome, err)
	}
	claim, err := db.ClaimOutboxOperationKind(ctx, "ack-1", OperationCommandAck, testNow+211, 100)
	if err != nil || claim == nil {
		t.Fatalf("ack claim=%v err=%v", claim, err)
	}
	if err := db.CompleteOutboxAttempt(ctx, *claim, CompleteOutcome{State: OperationRetryable, ErrorClass: ErrorTransient, ErrorSummary: "response_lost", NowMS: testNow + 212}); err != nil {
		t.Fatal(err)
	}
	claim, err = db.ClaimOutboxOperationKind(ctx, "ack-2", OperationCommandAck, testNow+213, 100)
	if err != nil || claim == nil {
		t.Fatalf("ack replay claim=%v err=%v", claim, err)
	}
	if err := db.CompleteOutboxAttempt(ctx, *claim, CompleteOutcome{State: OperationSucceeded, NowMS: testNow + 214}); err != nil {
		t.Fatal(err)
	}
	assertAttentionLifecycleCounts(t, db, interruptID, 1, 1, 1)

	// Forge API retries charge their own hourly budget. They neither reuse nor
	// increase the attention charge even when attached to the same project.
	for i, key := range []string{"once-forge:1", "once-forge:2"} {
		if _, err := db.ChargeForgeAPICall(ctx, ChargeForgeAPICallCmd{ProjectID: "project", CallAttemptKey: key, NowMS: testNow + 220 + int64(i), Limit: 100, WarningRatio: .8}); err != nil {
			t.Fatal(err)
		}
	}
	assertAttentionLifecycleCounts(t, db, interruptID, 1, 1, 1)
	var forgeEntries int
	if err := db.db.QueryRow(`SELECT count(*) FROM budget_entries WHERE kind='forge_api'`).Scan(&forgeEntries); err != nil || forgeEntries != 2 {
		t.Fatalf("forge entries=%d err=%v", forgeEntries, err)
	}
}

func completeChannelWithResponseLossReplay(t *testing.T, ctx context.Context, db *DB, nowMS int64) {
	t.Helper()
	claim, err := db.ClaimOutboxOperationKind(ctx, "channel-1", OperationChannelPublish, nowMS, 100)
	if err != nil || claim == nil {
		t.Fatalf("channel claim=%v err=%v", claim, err)
	}
	if err := db.CompleteOutboxAttempt(ctx, *claim, CompleteOutcome{State: OperationRetryable, ErrorClass: ErrorTransient, ErrorSummary: "response_lost", NowMS: nowMS + 1}); err != nil {
		t.Fatal(err)
	}
	claim, err = db.ClaimOutboxOperationKind(ctx, "channel-2", OperationChannelPublish, nowMS+2, 100)
	if err != nil || claim == nil {
		t.Fatalf("channel replay claim=%v err=%v", claim, err)
	}
	if err := db.CompleteOutboxAttempt(ctx, *claim, CompleteOutcome{State: OperationSucceeded, NowMS: nowMS + 3}); err != nil {
		t.Fatal(err)
	}
}

func assertAttentionLifecycleCounts(t *testing.T, db *DB, interruptID string, entries, admissions, members int) {
	t.Helper()
	checks := []struct {
		name  string
		query string
		args  []any
		want  int
	}{
		{"attention entries", `SELECT count(*) FROM budget_entries WHERE kind='attention' AND run_id=?`, []any{cmdRun}, entries},
		{"attention admissions", `SELECT count(*) FROM attention_admissions WHERE interrupt_id=?`, []any{interruptID}, admissions},
		{"batch members", `SELECT count(*) FROM attention_batch_members WHERE interrupt_id=?`, []any{interruptID}, members},
	}
	for _, check := range checks {
		var got int
		if err := db.db.QueryRow(check.query, check.args...).Scan(&got); err != nil || got != check.want {
			t.Fatalf("%s=%d, want %d (err=%v)", check.name, got, check.want, err)
		}
	}
}
