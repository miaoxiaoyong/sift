package intake

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/xsift/sift/internal/forge"
	"github.com/xsift/sift/internal/storage"
)

func TestReplyConsumerBindsRepliesToClarificationGeneration(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(2_000_000)
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project := Project{ID: "p1", Ref: forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "acme/repo"}, OperatorAllowlist: []string{"trusted"}}
	if err := db.SeedProjectForTest(ctx, "cfg-p1", project.ID, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := db.PersistIntakeBatch(ctx, storage.PersistIntakeBatchCmd{ProjectID: project.ID, Stream: "issues", NowMS: now.UnixMilli(), Items: []storage.IntakeItemInput{{IssueID: "42", IssueURL: "https://github.com/acme/repo/issues/42", IssueDigest: "issue", ForgeKind: "github", Host: "github.com", ProjectKey: "acme/repo", EventID: "label-42", EventKind: "trigger_label_added", Actor: "trusted", ObservedAtMS: now.UnixMilli(), RawDigest: "label"}}}); err != nil {
		t.Fatal(err)
	}
	item, err := db.PendingIntake(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(item) != 1 {
		t.Fatalf("pending intakes=%d", len(item))
	}
	call1, err := db.ReserveBrainCall(ctx, storage.ReserveBrainCallCmd{Scope: storage.BrainScopeIntake, SubjectKey: "issue:42", ProjectID: project.ID, Touchpoint: "T1", PromptVersion: "T1/v1", OutputSchemaVersion: 1, InputJSON: []byte(`{}`), InputDigest: "input1", StartedAtMS: now.UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PersistIntakeDecision(ctx, storage.IntakeDecisionCmd{IntakeID: item[0].ID, AssessmentID: "a1", LogicalCallID: call1.ID, ExpectedVersion: 1, Disposition: "needs_clarification", QuestionsJSON: `["q1"]`, Rationale: "first", NowMS: now.UnixMilli() + 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyIntakeReply(ctx, storage.IntakeReplyCmd{IntakeID: item[0].ID, EventID: "advance-1", Actor: "trusted", RawDigest: "advance", Generation: 1, Accept: true, ObservedAtMS: now.UnixMilli() + 2, NowMS: now.UnixMilli() + 2}); err != nil {
		t.Fatal(err)
	}
	current, err := db.PendingIntake(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	call2, err := db.ReserveBrainCall(ctx, storage.ReserveBrainCallCmd{Scope: storage.BrainScopeIntake, SubjectKey: "issue:42", ProjectID: project.ID, Touchpoint: "T1", PromptVersion: "T1/v1", OutputSchemaVersion: 1, InputJSON: []byte(`{}`), InputDigest: "input2", StartedAtMS: now.UnixMilli() + 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PersistIntakeDecision(ctx, storage.IntakeDecisionCmd{IntakeID: current[0].ID, AssessmentID: "a2", LogicalCallID: call2.ID, ExpectedVersion: 3, Disposition: "needs_clarification", QuestionsJSON: `["q2"]`, Rationale: "second", NowMS: now.UnixMilli() + 3}); err != nil {
		t.Fatal(err)
	}
	ops, err := db.IntakeReplyOperations(ctx, item[0].ID)
	if err != nil || len(ops) != 2 {
		t.Fatalf("clarification ops=%d err=%v", len(ops), err)
	}
	marker := func(op storage.IntakeReplyOperation) string {
		return forge.RenderOperationBody("clarification", op.Key, forge.PayloadDigest(op.Payload))
	}
	comments := []forge.Comment{
		{ID: "marker-1", Author: "siftd", Body: marker(ops[0]), CreatedAt: now.Add(time.Second)},
		{ID: "old-reply", Author: "trusted", Body: "/sift approve", CreatedAt: now.Add(2 * time.Second)},
		{ID: "marker-2", Author: "siftd", Body: marker(ops[1]), CreatedAt: now.Add(3 * time.Second)},
		{ID: "current-reply", Author: "trusted", Body: "/sift approve", CreatedAt: now.Add(4 * time.Second)},
	}
	fc := &replyFixture{Fake: forge.NewFake(), ref: project.Ref, issueID: "42", comments: comments}
	if err := (&ReplyConsumer{DB: db, Forge: fc, Projects: []Project{project}, Now: func() time.Time { return now.Add(5 * time.Second) }}).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	awaiting, err := db.AwaitingIntakes(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(awaiting) != 0 {
		t.Fatalf("awaiting=%+v, current-generation reply did not advance", awaiting)
	}
	check, err := sql.Open("sqlite", db.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var ignored, accepted int
	if err := check.QueryRow(`SELECT COUNT(*) FROM events WHERE type='intake.reply_ignored'`).Scan(&ignored); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow(`SELECT COUNT(*) FROM events WHERE type='intake.reply_accepted'`).Scan(&accepted); err != nil {
		t.Fatal(err)
	}
	if ignored != 1 || accepted != 2 {
		t.Fatalf("reply events ignored=%d accepted=%d, want 1/2", ignored, accepted)
	}
	var generations []int
	rows, err := check.Query(`SELECT json_extract(payload_json, '$.generation') FROM events WHERE type LIKE 'intake.reply_%' ORDER BY occurred_at_ms`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var generation int
		if err := rows.Scan(&generation); err != nil {
			t.Fatal(err)
		}
		generations = append(generations, generation)
	}
	if len(generations) != 3 || generations[1] != 1 || generations[2] != 2 {
		t.Fatalf("reply generations=%v, want setup gen1 then replies [1 2]", generations)
	}
}

type replyFixture struct {
	*forge.Fake
	ref      forge.ProjectRef
	issueID  string
	comments []forge.Comment
}

func (f *replyFixture) ListIssueComments(_ context.Context, ref forge.ProjectRef, issueID string, _ forge.Cursor) ([]forge.Comment, forge.Cursor, error) {
	if ref != f.ref || issueID != f.issueID {
		return nil, "", &forge.ClassifiedError{Class: forge.ErrSemanticConflict, Summary: "wrong project"}
	}
	return f.comments, "reply-cursor", nil
}

func TestReplyConsumerUsesPersistedGenerationAfterRestart(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(2_050_000)
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project := Project{ID: "p1", Ref: forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "acme/repo"}, OperatorAllowlist: []string{"trusted"}}
	if err := db.SeedProjectForTest(ctx, "cfg-p1", project.ID, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := db.PersistIntakeBatch(ctx, storage.PersistIntakeBatchCmd{ProjectID: project.ID, Stream: "issues", NowMS: now.UnixMilli(), Items: []storage.IntakeItemInput{{IssueID: "42", IssueURL: "https://github.com/acme/repo/issues/42", IssueDigest: "issue", ForgeKind: "github", Host: "github.com", ProjectKey: "acme/repo", EventID: "label-42", EventKind: "trigger_label_added", Actor: "trusted", ObservedAtMS: now.UnixMilli(), RawDigest: "label"}}}); err != nil {
		t.Fatal(err)
	}
	items, err := db.PendingIntake(ctx, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("pending=%+v err=%v", items, err)
	}
	call, err := db.ReserveBrainCall(ctx, storage.ReserveBrainCallCmd{Scope: storage.BrainScopeIntake, SubjectKey: "issue:42", ProjectID: project.ID, Touchpoint: "T1", PromptVersion: "T1/v1", OutputSchemaVersion: 1, InputJSON: []byte(`{}`), InputDigest: "gen1", StartedAtMS: now.UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PersistIntakeDecision(ctx, storage.IntakeDecisionCmd{IntakeID: items[0].ID, AssessmentID: "a1", LogicalCallID: call.ID, ExpectedVersion: 1, Disposition: "needs_clarification", QuestionsJSON: `["q"]`, Rationale: "clarify", NowMS: now.Add(time.Millisecond).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	ops, err := db.IntakeReplyOperations(ctx, items[0].ID)
	if err != nil || len(ops) != 1 {
		t.Fatalf("operations=%d err=%v", len(ops), err)
	}
	fc := &pagedReplyFixture{Fake: forge.NewFake(), ref: project.Ref, issueID: "42", pages: map[string][]forge.Comment{
		"":      {{ID: "marker-1", Author: "siftd", Body: forge.RenderOperationBody("clarification", ops[0].Key, forge.PayloadDigest(ops[0].Payload)), CreatedAt: now.Add(time.Second)}},
		"page1": {{ID: "reply-1", Author: "trusted", Body: "/sift approve", CreatedAt: now.Add(2 * time.Second)}},
	}}
	first := &ReplyConsumer{DB: db, Forge: fc, Projects: []Project{project}, Now: func() time.Time { return now.Add(3 * time.Second) }}
	if err := first.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := db.ReplyState(ctx, project.ID, "42")
	if err != nil || state.Generation != 1 || state.Cursor != "page1" {
		t.Fatalf("persisted state=%+v err=%v", state, err)
	}
	if err := (&ReplyConsumer{DB: db, Forge: fc, Projects: []Project{project}, Now: first.Now}).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	awaiting, err := db.AwaitingIntakes(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(awaiting) != 0 {
		t.Fatalf("awaiting=%+v, page-2 reply skipped after restart", awaiting)
	}
}

func TestReplyConsumerPersistsGenerationAcrossPagesAndRestart(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(2_100_000)
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project := Project{ID: "p1", Ref: forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "acme/repo"}, OperatorAllowlist: []string{"trusted"}}
	if err := db.SeedProjectForTest(ctx, "cfg-p1", project.ID, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := db.PersistIntakeBatch(ctx, storage.PersistIntakeBatchCmd{ProjectID: project.ID, Stream: "issues", NowMS: now.UnixMilli(), Items: []storage.IntakeItemInput{{IssueID: "42", IssueURL: "https://github.com/acme/repo/issues/42", IssueDigest: "issue", ForgeKind: "github", Host: "github.com", ProjectKey: "acme/repo", EventID: "label-42", EventKind: "trigger_label_added", Actor: "trusted", ObservedAtMS: now.UnixMilli(), RawDigest: "label"}}}); err != nil {
		t.Fatal(err)
	}
	item, err := db.PendingIntake(ctx, 1)
	if err != nil || len(item) != 1 {
		t.Fatalf("pending intake=%+v err=%v", item, err)
	}
	reserve := func(input string, started time.Time) string {
		call, e := db.ReserveBrainCall(ctx, storage.ReserveBrainCallCmd{Scope: storage.BrainScopeIntake, SubjectKey: "issue:42", ProjectID: project.ID, Touchpoint: "T1", PromptVersion: "T1/v1", OutputSchemaVersion: 1, InputJSON: []byte(`{}`), InputDigest: input, StartedAtMS: started.UnixMilli()})
		if e != nil {
			t.Fatal(e)
		}
		return call.ID
	}
	call1 := reserve("gen1", now)
	if err := db.PersistIntakeDecision(ctx, storage.IntakeDecisionCmd{IntakeID: item[0].ID, AssessmentID: "a1", LogicalCallID: call1, ExpectedVersion: 1, Disposition: "needs_clarification", QuestionsJSON: `["q1"]`, Rationale: "first", NowMS: now.Add(time.Millisecond).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyIntakeReply(ctx, storage.IntakeReplyCmd{IntakeID: item[0].ID, EventID: "setup-advance", Actor: "trusted", RawDigest: "setup", Generation: 1, Accept: true, ObservedAtMS: now.Add(2 * time.Millisecond).UnixMilli(), NowMS: now.Add(2 * time.Millisecond).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	current, err := db.PendingIntake(ctx, 1)
	if err != nil || len(current) != 1 {
		t.Fatalf("pending after setup reply=%+v err=%v", current, err)
	}
	call2 := reserve("gen2", now.Add(3*time.Millisecond))
	if err := db.PersistIntakeDecision(ctx, storage.IntakeDecisionCmd{IntakeID: current[0].ID, AssessmentID: "a2", LogicalCallID: call2, ExpectedVersion: 3, Disposition: "needs_clarification", QuestionsJSON: `["q2"]`, Rationale: "second", NowMS: now.Add(4 * time.Millisecond).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	ops, err := db.IntakeReplyOperations(ctx, item[0].ID)
	if err != nil || len(ops) != 2 {
		t.Fatalf("reply operations=%d err=%v", len(ops), err)
	}
	fc := &pagedReplyFixture{Fake: forge.NewFake(), ref: project.Ref, issueID: "42", pages: map[string][]forge.Comment{
		"":      {{ID: "marker-1", Author: "siftd", CreatedAt: now.Add(time.Second)}},
		"page1": {{ID: "old-reply", Author: "trusted", Body: "/sift approve", CreatedAt: now.Add(2 * time.Second)}, {ID: "marker-2", Author: "siftd", CreatedAt: now.Add(3 * time.Second)}, {ID: "current-reply", Author: "trusted", Body: "/sift approve", CreatedAt: now.Add(4 * time.Second)}},
	}}
	// The helper above cannot manufacture the operation payload; use the stored values directly.
	fc.pages[""][0].Body = forge.RenderOperationBody("clarification", ops[0].Key, forge.PayloadDigest(ops[0].Payload))
	fc.pages["page1"][1].Body = forge.RenderOperationBody("clarification", ops[1].Key, forge.PayloadDigest(ops[1].Payload))
	consumer := &ReplyConsumer{DB: db, Forge: fc, Projects: []Project{project}, Now: func() time.Time { return now.Add(5 * time.Second) }}
	if err := consumer.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := db.ReplyState(ctx, project.ID, "42")
	if err != nil || state.Generation != 1 || state.Cursor != "page1" {
		t.Fatalf("state after page1=%+v err=%v", state, err)
	}
	// A fresh consumer must use the persisted generation, not require page 1 again.
	if err := (&ReplyConsumer{DB: db, Forge: fc, Projects: []Project{project}, Now: consumer.Now}).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	awaiting, err := db.AwaitingIntakes(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(awaiting) != 0 {
		t.Fatalf("awaiting=%+v, current reply did not advance", awaiting)
	}
	var ignored int
	check, err := sql.Open("sqlite", db.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if err := check.QueryRow(`SELECT COUNT(*) FROM events WHERE type='intake.reply_ignored'`).Scan(&ignored); err != nil {
		t.Fatal(err)
	}
	if ignored < 1 {
		t.Fatalf("ignored replies=%d, want old reply audited", ignored)
	}
}

type pagedReplyFixture struct {
	*forge.Fake
	ref     forge.ProjectRef
	issueID string
	pages   map[string][]forge.Comment
}

func (f *pagedReplyFixture) ListIssueComments(_ context.Context, ref forge.ProjectRef, issueID string, cursor forge.Cursor) ([]forge.Comment, forge.Cursor, error) {
	if ref != f.ref || issueID != f.issueID {
		return nil, "", &forge.ClassifiedError{Class: forge.ErrSemanticConflict, Summary: "wrong project"}
	}
	return f.pages[string(cursor)], forge.Cursor(map[string]string{"": "page1", "page1": "page2"}[string(cursor)]), nil
}
