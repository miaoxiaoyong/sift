package forgeworker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xsift/sift/internal/command"
	"github.com/xsift/sift/internal/forge"
	"github.com/xsift/sift/internal/storage"
)

// command_ack worker (issue #803 / outbox.md §5): the acknowledgement is a
// Forge comment posted to the immutable envelope target. Like the comment
// worker, every attempt lists the target first and converges on the embedded
// operation marker, so a remote success followed by a local crash (or a
// response loss) does not produce a second acknowledgement.

const (
	cawProject  = "p1"
	cawTarget   = "42"
	cawEventKey = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
)

// seedCommandAck enqueues a command_ack operation plus the append-only receipt
// that pins its immutable target and project, without driving the full command
// write port. The payload is a real canonical CommandAckV1.
func seedCommandAck(t *testing.T, db *storage.DB, ack command.CommandAckV1) {
	t.Helper()
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg1", cawProject, cwNow); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(ctx, `INSERT INTO command_receipts (id,project_id,event_kind,remote_event_id,event_key,target_kind,target_id,actor,raw_digest,disposition,domain_event_id,command_outcome_id,observed_at_ms) VALUES (?,?,'forge_comment',?,?,'issue',?,?,?,'accepted',NULL,NULL,?)`,
		"receipt-1", cawProject, "rc-1", cawEventKey, cawTarget, "alice", strings.Repeat("a", 64), cwNow); err != nil {
		t.Fatalf("seed receipt: %v", err)
	}
	body, err := ack.CanonicalBytes()
	if err != nil {
		t.Fatalf("canonical ack: %v", err)
	}
	op := storage.Operation{
		Key:     command.AckOperationKey(cawEventKey),
		Kind:    storage.OperationCommandAck,
		Payload: body,
	}
	if _, err := db.EnqueueOperation(ctx, op, cwNow); err != nil {
		t.Fatalf("EnqueueOperation: %v", err)
	}
}

func sampleAck(action command.CommandAction) command.CommandAckV1 {
	runID := "0123456789abcdef0123456789abcdef"
	interruptID := "interrupt-1"
	a := action
	nonce := "fedcba9876543210fedcba9876543210"
	return command.CommandAckV1{
		SchemaVersion:  1,
		CommandEventID: "event-1",
		Action:         &a,
		Disposition:    command.OutcomeApplied,
		RunID:          &runID,
		InterruptID:    &interruptID,
		NextNonce:      &nonce,
	}
}

func ackClients(fc *countingFake) map[string]forge.Client {
	return map[string]forge.Client{"github|github.com|org/repo-" + cawProject: fc}
}

// TestCommandAckWorkerFreshSendEmbedsMarkerAndCompletes is the happy path: with
// no prior acknowledgement the worker posts exactly once, embeds the marker,
// and completes succeeded. It pins the path the crash test recovers from.
func TestCommandAckWorkerFreshSendEmbedsMarkerAndCompletes(t *testing.T) {
	ctx := context.Background()
	db := openWorkerDB(t)
	seedCommandAck(t, db, sampleAck(command.ActionApprove))

	fc := &countingFake{Fake: forge.NewFake()}
	ref := forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo-" + cawProject}
	fc.AddIssue(ref, forge.Issue{ID: cawTarget, Title: "t", Body: "b", Author: "alice", URL: "https://x/42"})

	var got storage.CompleteOutcome
	w := &CommandAckWorker{
		DB:       db,
		Clients:  ackClients(fc),
		WorkerID: "worker-1",
		Lease:    cwLease,
		Now:      func() time.Time { return time.UnixMilli(cwNow) },
		Complete: func(ctx context.Context, c storage.ClaimedOperation, o storage.CompleteOutcome) error {
			got = o
			return db.CompleteOutboxAttempt(ctx, c, o)
		},
	}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n := fc.sentCount(); n != 1 {
		t.Fatalf("sends=%d, want 1", n)
	}
	if !strings.Contains(fc.lastBody, "sift-op:v1") {
		t.Fatalf("body must embed marker: %q", fc.lastBody)
	}
	// The deterministic renderer outputs action + disposition + Run + Interrupt,
	// and the newly issued nonce as an executable command (command.md §6.1).
	if !strings.Contains(fc.lastBody, "Disposition: applied") || !strings.Contains(fc.lastBody, "Action: approve") {
		t.Fatalf("body must render disposition/action: %q", fc.lastBody)
	}
	if !strings.Contains(fc.lastBody, "Current nonce: fedcba9876543210fedcba9876543210") {
		t.Fatalf("body must echo only the newly issued nonce: %q", fc.lastBody)
	}
	if got.State != storage.OperationSucceeded {
		t.Fatalf("state=%q, want succeeded", got.State)
	}
	if !strings.Contains(string(got.Evidence), "comment_id") {
		t.Fatalf("evidence must carry comment id: %s", got.Evidence)
	}
}

// TestCommandAckWorkerCrashReplayNoResend mirrors the comment worker's
// crash-marker convergence: the first run posts remotely but "crashes" before
// committing completion; the recovery run reclaims, re-lists comments, finds
// the marker and converges WITHOUT a second send.
func TestCommandAckWorkerCrashReplayNoResend(t *testing.T) {
	ctx := context.Background()
	db := openWorkerDB(t)
	seedCommandAck(t, db, sampleAck(command.ActionReject))

	fc := &countingFake{Fake: forge.NewFake()}
	ref := forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo-" + cawProject}
	fc.AddIssue(ref, forge.Issue{ID: cawTarget, Title: "t", Body: "b", Author: "alice", URL: "https://x/42"})

	var recovered storage.CompleteOutcome
	crashed := false
	w := &CommandAckWorker{
		DB:       db,
		Clients:  ackClients(fc),
		WorkerID: "worker-1",
		Lease:    cwLease,
		Complete: func(ctx context.Context, c storage.ClaimedOperation, o storage.CompleteOutcome) error {
			if !crashed {
				crashed = true
				return errors.New("crash before local commit")
			}
			recovered = o
			return db.CompleteOutboxAttempt(ctx, c, o)
		},
	}

	// Run 1: no prior comment -> worker posts the ack (remote success), then
	// "crashes" before committing. Exactly one send so far.
	t0 := time.UnixMilli(cwNow)
	w.Now = func() time.Time { return t0 }
	if err := w.RunOnce(ctx); err == nil {
		t.Fatal("run 1 must surface the simulated crash error")
	}
	if n := fc.sentCount(); n != 1 {
		t.Fatalf("after run 1: sends=%d, want 1", n)
	}
	if !strings.Contains(fc.lastBody, "sift-op:v1") {
		t.Fatalf("posted body must embed marker: %q", fc.lastBody)
	}

	// Run 2: the lease has expired; the worker reclaims, re-lists comments,
	// finds the marker from run 1 and converges WITHOUT re-sending.
	t1 := t0.Add(cwLease + time.Second)
	w.Now = func() time.Time { return t1 }
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run 2 (recovery): %v", err)
	}
	if n := fc.sentCount(); n != 1 {
		t.Fatalf("after recovery: sends=%d, want 1 (no resend)", n)
	}
	if recovered.State != storage.OperationSucceeded {
		t.Fatalf("recovery outcome state=%q, want succeeded", recovered.State)
	}
	if !strings.Contains(string(recovered.Evidence), "comment_id") {
		t.Fatalf("recovery evidence must carry comment id: %s", recovered.Evidence)
	}

	// Run 3: nothing is claimable - the operation is terminal, so a recovery
	// scan finds no outstanding work and will not reprocess it.
	if c, err := db.ClaimOutboxOperation(ctx, "worker-probe", t1.Add(time.Second).UnixMilli(), cwLease.Milliseconds()); err != nil || c != nil {
		t.Fatalf("post-recovery claim must be empty (operation terminal): c=%v err=%v", c, err)
	}

	// Only one acknowledgement exists remotely: recovery found it via marker.
	comments, _, err := fc.ListIssueComments(ctx, ref, cawTarget, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 {
		t.Fatalf("remote comments=%d, want 1 (marker convergence, no duplicate)", len(comments))
	}
}

// TestCommandAckWorkerMissingRouteContractFailure fails closed: an ack whose
// receipt has been removed (or never persisted) must not be posted to an
// unproven target. It terminates as a permanent contract violation rather than
// retrying indefinitely.
func TestCommandAckWorkerMissingRouteContractFailure(t *testing.T) {
	ctx := context.Background()
	db := openWorkerDB(t)
	if err := db.SeedProjectForTest(ctx, "cfg1", cawProject, cwNow); err != nil {
		t.Fatal(err)
	}
	body, err := sampleAck(command.ActionApprove).CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	// No command_receipts row: routing cannot be proven.
	if _, err := db.EnqueueOperation(ctx, storage.Operation{
		Key: command.AckOperationKey(cawEventKey), Kind: storage.OperationCommandAck, Payload: body,
	}, cwNow); err != nil {
		t.Fatalf("EnqueueOperation: %v", err)
	}

	fc := &countingFake{Fake: forge.NewFake()}
	var got storage.CompleteOutcome
	w := &CommandAckWorker{
		DB:       db,
		Clients:  ackClients(fc),
		WorkerID: "worker-1",
		Lease:    cwLease,
		Now:      func() time.Time { return time.UnixMilli(cwNow) },
		Complete: func(ctx context.Context, c storage.ClaimedOperation, o storage.CompleteOutcome) error {
			got = o
			return db.CompleteOutboxAttempt(ctx, c, o)
		},
	}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got.State != storage.OperationFailed || got.ErrorClass != storage.ErrorContract {
		t.Fatalf("outcome=%+v, want failed/contract_violation", got)
	}
	if n := fc.sentCount(); n != 0 {
		t.Fatalf("must not post without proven routing: sends=%d", n)
	}
}
