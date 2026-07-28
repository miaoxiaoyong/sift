package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/forge"
	"github.com/miaoxiaoyong/sift/internal/intake"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

func TestDaemonTickExecutesDueGitHubAndGitLabRepliesWhileSkippingNotDuePoll(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(3_000_000)
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	github := intake.Project{ID: "github", Ref: forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "acme/gh"}, OperatorAllowlist: []string{"trusted"}}
	gitlab := intake.Project{ID: "gitlab", Ref: forge.ProjectRef{Kind: forge.KindGitLab, Host: "gitlab.com", ProjectKey: "acme/gl"}, OperatorAllowlist: []string{"trusted"}}
	for _, project := range []intake.Project{github, gitlab} {
		if err := db.SeedProjectForTest(ctx, "cfg-"+project.ID, project.ID, now.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	ghClient := &tickFixture{Fake: forge.NewFake(), ref: github.Ref, issueID: "gh-1"}
	glClient := &tickFixture{Fake: forge.NewFake(), ref: gitlab.Ref, issueID: "gl-1"}
	ghClient.intakeID = seedReplyTarget(t, db, github, now, "gh-1")
	glClient.intakeID = seedReplyTarget(t, db, gitlab, now, "gl-1")
	// The GitLab issue cursor is deliberately in the future; its reply stream is
	// still independently consumed by the same daemon tick.
	if err := db.PersistIntakeBatch(ctx, storage.PersistIntakeBatchCmd{ProjectID: gitlab.ID, Stream: "issues", Cursor: "not-due", NextPollAtMS: now.Add(time.Hour).UnixMilli(), NowMS: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []*tickFixture{ghClient, glClient} {
		op, err := db.IntakeReplyOperations(ctx, fixture.intakeID)
		if err != nil {
			t.Fatal(err)
		}
		fixture.comments = []forge.Comment{{ID: fixture.ref.ProjectKey + ":marker", Author: "siftd", Body: forge.RenderOperationBody("clarification", op[0].Key, forge.PayloadDigest(op[0].Payload)), CreatedAt: now.Add(time.Second)}, {ID: fixture.ref.ProjectKey + ":reply", Author: "trusted", Body: "/sift approve", CreatedAt: now.Add(2 * time.Second)}}
	}
	d := &Daemon{DB: db, Now: func() time.Time { return now }, Pollers: []*intake.Poller{
		{DB: db, Forge: ghClient, Projects: []intake.Project{github}, Now: func() time.Time { return now }, Idle: time.Minute},
		{DB: db, Forge: glClient, Projects: []intake.Project{gitlab}, Now: func() time.Time { return now }, Idle: time.Minute},
	}, Replies: []*intake.ReplyConsumer{
		{DB: db, Forge: ghClient, Projects: []intake.Project{github}, Now: func() time.Time { return now }},
		{DB: db, Forge: glClient, Projects: []intake.Project{gitlab}, Now: func() time.Time { return now }},
	}}
	if err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if ghClient.issuePolls != 1 || glClient.issuePolls != 0 {
		t.Fatalf("issue polls github=%d gitlab=%d, want 1/0", ghClient.issuePolls, glClient.issuePolls)
	}
	for _, project := range []intake.Project{github, gitlab} {
		awaiting, err := db.AwaitingIntakes(ctx, project.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(awaiting) != 0 {
			t.Fatalf("%s awaiting=%+v, reply path did not execute", project.ID, awaiting)
		}
	}
}

func seedReplyTarget(t *testing.T, db *storage.DB, project intake.Project, now time.Time, issueID string) string {
	t.Helper()
	ctx := context.Background()
	if err := db.PersistIntakeBatch(ctx, storage.PersistIntakeBatchCmd{ProjectID: project.ID, Stream: "issues", NowMS: now.UnixMilli(), Items: []storage.IntakeItemInput{{IssueID: issueID, IssueURL: "https://example.test/" + issueID, IssueDigest: "digest-" + issueID, ForgeKind: string(project.Ref.Kind), Host: project.Ref.Host, ProjectKey: project.Ref.ProjectKey, EventID: "label-" + issueID, EventKind: "trigger_label_added", Actor: "trusted", ObservedAtMS: now.UnixMilli(), RawDigest: "raw-" + issueID}}}); err != nil {
		t.Fatal(err)
	}
	items, err := db.PendingIntake(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var item storage.PendingIntake
	for _, candidate := range items {
		if candidate.ProjectID == project.ID {
			item = candidate
		}
	}
	if item.ID == "" {
		t.Fatalf("no item for %s", project.ID)
	}
	call, err := db.ReserveBrainCall(ctx, storage.ReserveBrainCallCmd{Scope: storage.BrainScopeIntake, SubjectKey: "issue:" + issueID, ProjectID: project.ID, Touchpoint: "T1", PromptVersion: "T1/v1", OutputSchemaVersion: 1, InputJSON: []byte(`{}`), InputDigest: "input-" + issueID, StartedAtMS: now.UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PersistIntakeDecision(ctx, storage.IntakeDecisionCmd{IntakeID: item.ID, AssessmentID: "assessment-" + issueID, LogicalCallID: call.ID, ExpectedVersion: 1, Disposition: "needs_clarification", QuestionsJSON: "[\"question\"]", Rationale: "clarify", NowMS: now.Add(time.Millisecond).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	return item.ID
}

type tickFixture struct {
	*forge.Fake
	ref               forge.ProjectRef
	issueID, intakeID string
	issuePolls        int
	comments          []forge.Comment
}

func (f *tickFixture) ListIssuesByLabel(ctx context.Context, ref forge.ProjectRef, label string, cursor forge.Cursor) ([]forge.Issue, forge.Cursor, error) {
	f.issuePolls++
	return f.Fake.ListIssuesByLabel(ctx, ref, label, cursor)
}
func (f *tickFixture) ListIssueComments(_ context.Context, ref forge.ProjectRef, issueID string, _ forge.Cursor) ([]forge.Comment, forge.Cursor, error) {
	return f.comments, "comments", nil
}

var _ = time.Second
