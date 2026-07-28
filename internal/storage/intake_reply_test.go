package storage

import (
	"context"
	"testing"
)

// V7 intake reply arbitration (WBS M2 §2.3 / §2.5, storage.md): a reply is
// always recorded, but only a reply from the CURRENT clarification generation
// can advance the intake projection. A reply from a stale generation is
// audit-only — it must never move the state machine or bump the CAS version.
// Replaying the same forge event is a receipt-idempotent no-op.

// seedAwaitingIntake installs an intake item in an awaiting state with a known
// clarification_generation, plus the assessment the awaiting CHECK requires.
func seedAwaitingIntake(t *testing.T, db *DB, intakeID, state string, generation, version int) {
	t.Helper()
	ctx := context.Background()
	insertConfigSnapshot(t, db, "cfg1")
	insertProject(t, db, "p1", "cfg1")
	insertManualRun(t, db, "r1", "p1", "cfg1") // duplicate-candidate run for duplicate_confirmation
	insertIntakeItem(t, db, intakeID, "p1", "42")
	insertBrainCallT1(t, db, "bc1", "p1", "s", 1)
	insertIntakeAssessment(t, db, "ia1", intakeID, "bc1")
	mustExec(t, db, `UPDATE intake_items
		SET state=?, latest_assessment_id='ia1', clarification_generation=?, version=?,
		    duplicate_candidate_run_id=CASE WHEN ?='awaiting_duplicate_confirmation' THEN 'r1' ELSE duplicate_candidate_run_id END,
		    updated_at_ms=?
		WHERE id=?`, state, generation, version, state, testNow, intakeID)
	// Sanity: the seed landed in the expected state.
	var got string
	if err := db.db.QueryRowContext(ctx, `SELECT state FROM intake_items WHERE id=?`, intakeID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != state {
		t.Fatalf("seed state=%q want %q", got, state)
	}
}

func intakeStateVersion(t *testing.T, db *DB, id string) (string, int) {
	t.Helper()
	var state string
	var version int
	if err := db.db.QueryRowContext(context.Background(),
		`SELECT state, version FROM intake_items WHERE id=?`, id).Scan(&state, &version); err != nil {
		t.Fatal(err)
	}
	return state, version
}

func countEvents(t *testing.T, db *DB, eventType string) int {
	t.Helper()
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM events WHERE type=?`, eventType).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestV7ReplyCursorIsolatedByIssue(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg-p1", "p1", testNow); err != nil {
		t.Fatal(err)
	}
	// Both targets belong to the same project; their reply cursors must still
	// be independent because each issue has its own comment stream.
	if err := db.SaveReplyCursor(ctx, "p1", "issue-a", "a-1", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveReplyCursor(ctx, "p1", "issue-b", "b-1", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveReplyCursor(ctx, "p1", "issue-a", "a-2", testNow+1); err != nil {
		t.Fatal(err)
	}
	a, err := db.ReplyState(ctx, "p1", "issue-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.ReplyState(ctx, "p1", "issue-b")
	if err != nil {
		t.Fatal(err)
	}
	if a.Cursor != "a-2" || b.Cursor != "b-1" {
		t.Fatalf("reply cursors A=%q B=%q, want a-2/b-1", a.Cursor, b.Cursor)
	}
}

func TestV7StaleGenerationReplyIsAuditOnly(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	const id = "ii-stale"
	seedAwaitingIntake(t, db, id, "awaiting_clarification", 2, 2)

	beforeState, beforeVersion := intakeStateVersion(t, db, id)

	// A reply quoting the PREVIOUS generation (1) while the item sits at
	// generation 2 is stale: recorded as reply_ignored, projection untouched.
	if err := db.ApplyIntakeReply(ctx, IntakeReplyCmd{
		IntakeID: id, EventID: "ev-old-gen", Actor: "alice", RawDigest: "d1",
		Generation: 1, Accept: true, ObservedAtMS: testNow, NowMS: testNow,
	}); err != nil {
		t.Fatalf("stale reply: %v", err)
	}
	if got := countEvents(t, db, "intake.reply_ignored"); got != 1 {
		t.Fatalf("reply_ignored events=%d, want 1", got)
	}
	state, version := intakeStateVersion(t, db, id)
	if state != beforeState || version != beforeVersion {
		t.Fatalf("stale reply mutated projection: state=%s version=%d (want %s/%d)", state, version, beforeState, beforeVersion)
	}
	if got := countEvents(t, db, "intake.reply_accepted"); got != 0 {
		t.Fatalf("stale reply must not be accepted: events=%d", got)
	}
}

func TestV7CurrentGenerationReplyAdvances(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	const id = "ii-current"
	seedAwaitingIntake(t, db, id, "awaiting_clarification", 2, 2)

	// A reply at the current generation and accepted advances the item back to
	// pending_evaluation and bumps the CAS version.
	if err := db.ApplyIntakeReply(ctx, IntakeReplyCmd{
		IntakeID: id, EventID: "ev-cur-gen", Actor: "alice", RawDigest: "d2",
		Generation: 2, Accept: true, ObservedAtMS: testNow, NowMS: testNow,
	}); err != nil {
		t.Fatalf("current reply: %v", err)
	}
	state, version := intakeStateVersion(t, db, id)
	if state != "pending_evaluation" || version != 3 {
		t.Fatalf("current reply: state=%s version=%d, want pending_evaluation/3", state, version)
	}
	if got := countEvents(t, db, "intake.reply_accepted"); got != 1 {
		t.Fatalf("reply_accepted events=%d, want 1", got)
	}
}

func TestV7ReplyReceiptIdempotent(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	const id = "ii-idem"
	seedAwaitingIntake(t, db, id, "awaiting_clarification", 1, 2)

	cmd := IntakeReplyCmd{
		IntakeID: id, EventID: "ev-replay", Actor: "alice", RawDigest: "d3",
		Generation: 1, Accept: true, ObservedAtMS: testNow, NowMS: testNow,
	}
	if err := db.ApplyIntakeReply(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	state1, version1 := intakeStateVersion(t, db, id)
	// Replaying the identical forge event is a receipt no-op: no second event,
	// no second version bump, no error.
	if err := db.ApplyIntakeReply(ctx, cmd); err != nil {
		t.Fatalf("replay: %v", err)
	}
	state2, version2 := intakeStateVersion(t, db, id)
	if state1 != state2 || version1 != version2 {
		t.Fatalf("replay mutated projection: %s/%d -> %s/%d", state1, version1, state2, version2)
	}
	if got := countEvents(t, db, "intake.reply_accepted"); got != 1 {
		t.Fatalf("reply_accepted events=%d, want 1 after replay", got)
	}
}

func TestV7CurrentGenerationReplyOnDuplicateConfirmation(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	const id = "ii-dup"
	seedAwaitingIntake(t, db, id, "awaiting_duplicate_confirmation", 3, 2)

	if err := db.ApplyIntakeReply(ctx, IntakeReplyCmd{
		IntakeID: id, EventID: "ev-dup", Actor: "alice", RawDigest: "d4",
		Generation: 3, Accept: true, ObservedAtMS: testNow, NowMS: testNow,
	}); err != nil {
		t.Fatalf("dup reply: %v", err)
	}
	state, version := intakeStateVersion(t, db, id)
	if state != "pending_evaluation" || version != 3 {
		t.Fatalf("dup reply: state=%s version=%d, want pending_evaluation/3", state, version)
	}
}

func TestV7ReplyRejectsIncomplete(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedAwaitingIntake(t, db, "ii-x", "awaiting_clarification", 1, 2)
	cases := []IntakeReplyCmd{
		{EventID: "e", Actor: "a", Generation: 1, ObservedAtMS: 1, NowMS: 1},
		{IntakeID: "ii-x", Actor: "a", Generation: 1, ObservedAtMS: 1, NowMS: 1},
		{IntakeID: "ii-x", EventID: "e", Generation: 1, ObservedAtMS: 1, NowMS: 1},
		{IntakeID: "ii-x", EventID: "e", Actor: "a", ObservedAtMS: 1, NowMS: 1},
		{IntakeID: "ii-x", EventID: "e", Actor: "a", Generation: 0, ObservedAtMS: 1, NowMS: 1},
	}
	for i, cmd := range cases {
		if err := db.ApplyIntakeReply(ctx, cmd); err == nil {
			t.Fatalf("case %d: expected error for incomplete reply", i)
		}
	}
}
