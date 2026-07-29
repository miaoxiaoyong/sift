package intake

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/forge"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

const reconcilerNow = int64(1_704_000_000_000)

func reconcilerDB(t *testing.T, projectID string) (*storage.DB, Project) {
	t.Helper()
	db, err := storage.Open(context.Background(), storage.OpenConfig{
		Path: t.TempDir() + "/sift.db", BinaryVersion: "test-binary", Now: time.UnixMilli(reconcilerNow),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.SeedProjectForTest(context.Background(), "cfg-"+projectID, projectID, reconcilerNow); err != nil {
		t.Fatal(err)
	}
	return db, Project{ID: projectID, TriggerLabel: "sift", OperatorAllowlist: []string{"trusted"}, Ref: forge.ProjectRef{
		Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo-" + projectID,
	}}
}

func seedWaitingRun(t *testing.T, db *storage.DB, project Project, runID, issueID, changeID string) {
	t.Helper()
	if err := db.SeedReverseSyncRunForTest(context.Background(), runID, project.ID, "cfg-"+project.ID, issueID, changeID, "waiting_human", reconcilerNow); err != nil {
		t.Fatal(err)
	}
}

func reconcile(t *testing.T, db *storage.DB, fc forge.Client, projects ...Project) {
	t.Helper()
	if err := (&Reconciler{DB: db, Forge: fc, Projects: projects, Certification: config.DefaultConfig().Certification, Now: func() time.Time { return time.UnixMilli(reconcilerNow) }}).ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func addIssue(fc *forge.Fake, p forge.ProjectRef, id string, state forge.IssueState) {
	fc.AddIssue(p, forge.Issue{ID: id, Title: id, Body: "body", Author: "author", URL: "https://example.test/" + id, State: state})
}

func TestReconcilerOnceExternalMergeCompletesWaitingHuman(t *testing.T) {
	db, project := reconcilerDB(t, "merge")
	fc := forge.NewFake()
	addIssue(fc, project.Ref, "1", forge.IssueOpen)
	change := fc.AddChange(project.Ref, "c1", "head1")
	change.URL = "https://example.test/c1"
	if _, err := fc.InjectMerged(project.Ref, "c1", time.UnixMilli(reconcilerNow)); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(context.Background(), "run-merge", project.ID, "cfg-"+project.ID, "1", reconcilerNow); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordCreatedChange(context.Background(), "run-merge", "c1", reconcilerNow); err != nil {
		t.Fatal(err)
	}
	seed, err := sql.Open("sqlite", db.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`UPDATE runs SET kind='feature' WHERE id='run-merge'`); err != nil {
		seed.Close()
		t.Fatal(err)
	}
	seed.Close()
	recorded, _, err := db.RecordGateEvaluationAndEmitInterrupt(context.Background(), intakeGateRecord("run-merge"), intakeGateInterrupt("run-merge"))
	if err != nil {
		t.Fatal(err)
	}

	reconcile(t, db, fc, project)
	run, err := db.Run(context.Background(), "run-merge")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != storage.RunDone || !run.GateBypassed || run.ChangeID != change.ID {
		t.Fatalf("run after external merge = %+v, want done with gate_bypassed", run)
	}
	check, err := sql.Open("sqlite", db.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var facts, decisions int
	var boundCalibration, settledCalibration, humanDecision, certificationVersion string
	if err := check.QueryRow(`SELECT COUNT(*) FROM events WHERE type='forge_change_merged' AND run_id='run-merge'`).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow(`SELECT COUNT(*) FROM ledger_entries WHERE run_id='run-merge' AND entry_kind='human_decision'`).Scan(&decisions); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow(`SELECT b.calibration_id,c.human_decision FROM external_decision_bindings b JOIN calibration_entries c ON c.id=b.calibration_id`).Scan(&boundCalibration, &humanDecision); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow(`SELECT calibration_id FROM human_decision_receipts`).Scan(&settledCalibration); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow(`SELECT certification_version FROM certification_current WHERE task_kind='feature'`).Scan(&certificationVersion); err != nil {
		t.Fatal(err)
	}
	if facts != 1 || decisions != 1 || boundCalibration != recorded.CalibrationID || settledCalibration != recorded.CalibrationID || humanDecision != "allow" || certificationVersion == "" {
		t.Fatalf("external merge facts=%d decisions=%d binding=%q settlement=%q decision=%q certification=%q", facts, decisions, boundCalibration, settledCalibration, humanDecision, certificationVersion)
	}
	// A recovery tick after the terminal transition cannot append another fact,
	// decision, or certification revision.
	reconcile(t, db, fc, project)
	var factsAfter, decisionsAfter int
	if err := check.QueryRow(`SELECT COUNT(*) FROM events WHERE type='forge_change_merged' AND run_id='run-merge'`).Scan(&factsAfter); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow(`SELECT COUNT(*) FROM ledger_entries WHERE run_id='run-merge' AND entry_kind='human_decision'`).Scan(&decisionsAfter); err != nil {
		t.Fatal(err)
	}
	if factsAfter != facts || decisionsAfter != decisions {
		t.Fatalf("recovery appended facts=%d decisions=%d, want %d/%d", factsAfter, decisionsAfter, facts, decisions)
	}
}

func intakeGateRecord(runID string) storage.GateEvaluationRecord {
	return intakeGateRecordWithShadow(runID, "block")
}

func intakeGateRecordWithShadow(runID, shadow string) storage.GateEvaluationRecord {
	return storage.GateEvaluationRecord{RunID: runID, GateInputHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", GateVersion: "gate/v1", SnapshotSchemaVersion: 1, SnapshotJSON: []byte(`{"schema_version":1}`), VerdictJSON: []byte(`{"schema_version":1,"kind":"hitl"}`), HeadSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", EffectivePolicyHash: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", CertificationVersion: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", RiskSourceVersion: "T3/fallback/v1", VerdictDigest: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", ShadowDecision: shadow, FeaturesJSON: []byte(`{"schema_version":1}`), NowMS: reconcilerNow}
}

func intakeGateInterrupt(runID string) storage.EmitInterruptCmd {
	return storage.EmitInterruptCmd{RunID: runID, ExpectedRunVersion: 2, Reason: storage.InterruptCodeReview, Facts: map[string]string{"change_ref": "https://example.test/c1", "head_sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "review_requirement": "required", "recommended_action": "approve", "diff_ref": "https://example.test/c1/diff"}, Generation: storage.InterruptGeneration{ChangeID: "c1", HeadSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, GatePhase: storage.GateReview, GuardrailLevel: storage.GuardrailNone, AttentionDailyQuota: map[storage.InterruptSeverity]int{storage.SeverityLow: 3, storage.SeverityNormal: 3, storage.SeverityHigh: 3}, DayTimezone: "UTC", Source: storage.SourceSystem, NowMS: reconcilerNow}
}

func TestReconcilerExternalInconclusiveMergeConvergesWithoutSettlement(t *testing.T) {
	db, project := reconcilerDB(t, "inconclusive")
	fc := forge.NewFake()
	addIssue(fc, project.Ref, "1", forge.IssueOpen)
	change := fc.AddChange(project.Ref, "c1", "head1")
	change.URL = "https://example.test/c1"
	if _, err := fc.InjectMerged(project.Ref, "c1", time.UnixMilli(reconcilerNow)); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(context.Background(), "run-inconclusive", project.ID, "cfg-"+project.ID, "1", reconcilerNow); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordCreatedChange(context.Background(), "run-inconclusive", "c1", reconcilerNow); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.RecordGateEvaluationAndEmitInterrupt(context.Background(), intakeGateRecordWithShadow("run-inconclusive", "inconclusive"), intakeGateInterrupt("run-inconclusive")); err != nil {
		t.Fatal(err)
	}

	reconcile(t, db, fc, project)
	run, err := db.Run(context.Background(), "run-inconclusive")
	if err != nil || run.Status != storage.RunDone || !run.GateBypassed {
		t.Fatalf("inconclusive external merge run=%+v err=%v, want done + bypassed", run, err)
	}
	check, err := sql.Open("sqlite", db.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var facts, decisions, bindings, certified int
	var decision sql.NullString
	if err := check.QueryRow(`SELECT COUNT(*) FROM events WHERE type='forge_change_merged' AND run_id='run-inconclusive'`).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow(`SELECT COUNT(*) FROM ledger_entries WHERE run_id='run-inconclusive' AND entry_kind='human_decision'`).Scan(&decisions); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow(`SELECT COUNT(*) FROM external_decision_bindings`).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow(`SELECT human_decision FROM calibration_entries WHERE run_id='run-inconclusive'`).Scan(&decision); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow(`SELECT COUNT(*) FROM certification_current`).Scan(&certified); err != nil {
		t.Fatal(err)
	}
	if facts != 1 || decisions != 1 || bindings != 1 || decision.Valid || certified != 0 {
		t.Fatalf("inconclusive audit facts=%d decisions=%d bindings=%d human=%q valid=%v certifications=%d", facts, decisions, bindings, decision.String, decision.Valid, certified)
	}
	reconcile(t, db, fc, project)
	var factsAfter, decisionsAfter int
	if err := check.QueryRow(`SELECT COUNT(*) FROM events WHERE type='forge_change_merged' AND run_id='run-inconclusive'`).Scan(&factsAfter); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow(`SELECT COUNT(*) FROM ledger_entries WHERE run_id='run-inconclusive' AND entry_kind='human_decision'`).Scan(&decisionsAfter); err != nil {
		t.Fatal(err)
	}
	if factsAfter != facts || decisionsAfter != decisions {
		t.Fatalf("inconclusive recovery appended facts=%d decisions=%d", factsAfter, decisionsAfter)
	}
}

func TestReconcilerExternalMergeFactsFirstWithoutExactBinding(t *testing.T) {
	for _, tc := range []struct {
		name, status, shadow        string
		ambiguous                   bool
		wantBindings, wantDecisions int
	}{
		{name: "queued", status: "queued"},
		{name: "running", status: "running"},
		{name: "exact_binary", status: "waiting_human", shadow: "block", wantBindings: 1, wantDecisions: 1},
		{name: "exact_inconclusive", status: "waiting_human", shadow: "inconclusive", wantBindings: 1, wantDecisions: 1},
		{name: "missing", status: "waiting_human"},
		{name: "ambiguous", status: "waiting_human", shadow: "block", ambiguous: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, project := reconcilerDB(t, "facts-"+tc.name)
			fc := forge.NewFake()
			addIssue(fc, project.Ref, "1", forge.IssueOpen)
			fc.AddChange(project.Ref, "c1", "head1")
			if _, err := fc.InjectMerged(project.Ref, "c1", time.UnixMilli(reconcilerNow)); err != nil {
				t.Fatal(err)
			}
			if tc.shadow == "" {
				if err := db.SeedReverseSyncRunForTest(context.Background(), "run", project.ID, "cfg-"+project.ID, "1", "c1", tc.status, reconcilerNow); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := db.SeedForgeRunForTest(context.Background(), "run", project.ID, "cfg-"+project.ID, "1", reconcilerNow); err != nil {
					t.Fatal(err)
				}
				if err := db.RecordCreatedChange(context.Background(), "run", "c1", reconcilerNow); err != nil {
					t.Fatal(err)
				}
				q, err := sql.Open("sqlite", db.Path())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := q.Exec(`UPDATE runs SET kind='feature' WHERE id='run'`); err != nil {
					q.Close()
					t.Fatal(err)
				}
				q.Close()
				if _, _, err := db.RecordGateEvaluationAndEmitInterrupt(context.Background(), intakeGateRecordWithShadow("run", tc.shadow), intakeGateInterrupt("run")); err != nil {
					t.Fatal(err)
				}
				if tc.ambiguous {
					second := intakeGateInterrupt("run")
					second.ExpectedRunVersion = 3
					second.Generation.ChangeID = "c2"
					if _, _, err := db.RecordGateEvaluationAndEmitInterrupt(context.Background(), intakeGateRecordWithShadow("run", tc.shadow), second); err != nil {
						t.Fatal(err)
					}
				}
			}

			reconcile(t, db, fc, project)
			run, err := db.Run(context.Background(), "run")
			if err != nil || run.Status != storage.RunDone || !run.GateBypassed {
				t.Fatalf("run=%+v err=%v, want done + gate_bypassed", run, err)
			}
			q, err := sql.Open("sqlite", db.Path())
			if err != nil {
				t.Fatal(err)
			}
			defer q.Close()
			var facts, bindings, decisions int
			if err := q.QueryRow(`SELECT COUNT(*) FROM events WHERE run_id='run' AND type='forge_change_merged'`).Scan(&facts); err != nil {
				t.Fatal(err)
			}
			if err := q.QueryRow(`SELECT COUNT(*) FROM external_decision_bindings`).Scan(&bindings); err != nil {
				t.Fatal(err)
			}
			if err := q.QueryRow(`SELECT COUNT(*) FROM ledger_entries WHERE run_id='run' AND entry_kind='human_decision'`).Scan(&decisions); err != nil {
				t.Fatal(err)
			}
			if facts != 1 || bindings != tc.wantBindings || decisions != tc.wantDecisions {
				t.Fatalf("facts/bindings/decisions=%d/%d/%d, want 1/%d/%d", facts, bindings, decisions, tc.wantBindings, tc.wantDecisions)
			}
			reconcile(t, db, fc, project)
			var factsAfter int
			if err := q.QueryRow(`SELECT COUNT(*) FROM events WHERE run_id='run' AND type='forge_change_merged'`).Scan(&factsAfter); err != nil {
				t.Fatal(err)
			}
			if factsAfter != 1 {
				t.Fatalf("recovery facts=%d, want 1", factsAfter)
			}
		})
	}
}

func TestReconcilerClassifiesSucceededGateMergeWithoutBypass(t *testing.T) {
	db, project := reconcilerDB(t, "sift-merge")
	fc := forge.NewFake()
	addIssue(fc, project.Ref, "1", forge.IssueOpen)
	change := fc.AddChange(project.Ref, "c1", "head1")
	seedWaitingRun(t, db, project, "run-merge", "1", "c1")
	payload, err := json.Marshal(map[string]string{"project_id": project.ID, "run_id": "run-merge", "change_id": "c1", "gate_evaluation_id": "ge-1", "expected_head_sha": "head1", "method": "merge"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueOperation(context.Background(), storage.Operation{Key: storage.MergeChangeOperationKey("run-merge", "head1"), Kind: storage.OperationMergeChange, RunID: "run-merge", Payload: payload}, reconcilerNow); err != nil {
		t.Fatal(err)
	}
	claim, err := db.ClaimOutboxOperationKindProject(context.Background(), "test", storage.OperationMergeChange, project.ID, reconcilerNow, 1000)
	if err != nil || claim == nil {
		t.Fatalf("claim merge: %#v, %v", claim, err)
	}
	if err := db.CompleteOutboxAttempt(context.Background(), *claim, storage.CompleteOutcome{State: storage.OperationSucceeded, NowMS: reconcilerNow + 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := fc.InjectMerged(project.Ref, "c1", time.UnixMilli(reconcilerNow)); err != nil {
		t.Fatal(err)
	}

	reconcile(t, db, fc, project)
	run, err := db.Run(context.Background(), "run-merge")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != storage.RunDone || run.GateBypassed || run.ChangeID != change.ID {
		t.Fatalf("run after Sift merge = %+v, want done without bypass", run)
	}
	var decisions int
	check, err := sql.Open("sqlite", db.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if err := check.QueryRow(`SELECT COUNT(*) FROM ledger_entries WHERE run_id='run-merge' AND entry_kind='human_decision'`).Scan(&decisions); err != nil {
		t.Fatal(err)
	}
	if decisions != 0 {
		t.Fatalf("Sift merge wrote manual decision count=%d", decisions)
	}
}

func TestReconcilerOnceClosedChangeAndIssueFailRuns(t *testing.T) {
	db, project := reconcilerDB(t, "closed")
	fc := forge.NewFake()
	addIssue(fc, project.Ref, "issue-closed", forge.IssueClosed)
	addIssue(fc, project.Ref, "change-closed", forge.IssueOpen)
	fc.AddChange(project.Ref, "c-closed", "head")
	seedWaitingRun(t, db, project, "run-issue", "issue-closed", "")
	seedWaitingRun(t, db, project, "run-change", "change-closed", "c-closed")

	// The fake has no mutation helper for a closed Change; this client supplies
	// the forge's current object-state observation while delegating everything else.
	reconcile(t, db, &closedChangeClient{Fake: fc, project: project.Ref, changeID: "c-closed"}, project)
	for _, tc := range []struct{ id, reason string }{{"run-issue", "closed_upstream"}, {"run-change", "change_closed"}} {
		run, err := db.Run(context.Background(), tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != storage.RunFailed || run.FailureReason != tc.reason {
			t.Errorf("%s = %+v, want failed/%s", tc.id, run, tc.reason)
		}
	}
}

type closedChangeClient struct {
	*forge.Fake
	project  forge.ProjectRef
	changeID string
}

func (c *closedChangeClient) GetChange(ctx context.Context, p forge.ProjectRef, id string) (forge.Change, error) {
	change, err := c.Fake.GetChange(ctx, p, id)
	if err == nil && p == c.project && id == c.changeID {
		change.State = forge.ChangeClosed
	}
	return change, err
}

func TestReconcilerOnceUntriggerRequiresTrustedActor(t *testing.T) {
	db, project := reconcilerDB(t, "untrigger")
	fc := forge.NewFake()
	addIssue(fc, project.Ref, "trusted", forge.IssueOpen)
	addIssue(fc, project.Ref, "untrusted", forge.IssueOpen)
	fc.AddLabelEvent(project.Ref, forge.LabelEvent{TargetID: "trusted", Label: "sift", Action: forge.LabelRemoved, Actor: "trusted", ObservedAt: time.UnixMilli(reconcilerNow)})
	fc.AddLabelEvent(project.Ref, forge.LabelEvent{TargetID: "untrusted", Label: "sift", Action: forge.LabelRemoved, Actor: "outsider", ObservedAt: time.UnixMilli(reconcilerNow)})
	seedWaitingRun(t, db, project, "run-trusted", "trusted", "")
	seedWaitingRun(t, db, project, "run-untrusted", "untrusted", "")

	reconcile(t, db, fc, project)
	trusted, _ := db.Run(context.Background(), "run-trusted")
	untrusted, _ := db.Run(context.Background(), "run-untrusted")
	if trusted.Status != storage.RunFailed || trusted.FailureReason != "untriggered" {
		t.Fatalf("trusted removal = %+v, want untriggered failure", trusted)
	}
	if untrusted.Status != storage.RunWaitingHuman {
		t.Fatalf("untrusted removal = %+v, must be ignored", untrusted)
	}
}

func TestReconcilerOnceIsolationAlertsOnceAndContinues(t *testing.T) {
	db, bad := reconcilerDB(t, "bad")
	if err := db.SeedProjectForTest(context.Background(), "cfg-good", "good", reconcilerNow); err != nil {
		t.Fatal(err)
	}
	good := bad
	good.ID, good.Ref.ProjectKey = "good", "org/repo-good"
	seedWaitingRun(t, db, bad, "run-bad", "1", "")
	seedWaitingRun(t, db, good, "run-good", "2", "")
	fc := forge.NewFake()
	addIssue(fc, bad.Ref, "1", forge.IssueOpen)
	addIssue(fc, good.Ref, "2", forge.IssueOpen)
	client := &authForProjectClient{Fake: fc, bad: bad.Ref}
	isolated := 0
	r := &Reconciler{DB: db, Forge: client, Projects: []Project{bad, good}, Now: func() time.Time { return time.UnixMilli(reconcilerNow) }, Isolated: func(Project, error) { isolated++ }}
	if err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if isolated != 1 {
		t.Fatalf("isolation callbacks=%d, want 1", isolated)
	}
	alerts, err := db.CountOperationsByKind(context.Background(), storage.OperationForgeAlert)
	if err != nil {
		t.Fatal(err)
	}
	if alerts != 1 {
		t.Fatalf("forge alerts=%d, want one project-isolation alert", alerts)
	}
	if run, _ := db.Run(context.Background(), "run-good"); run.Status != storage.RunWaitingHuman {
		t.Fatalf("healthy project run=%+v, want untouched", run)
	}
}

type authForProjectClient struct {
	*forge.Fake
	bad forge.ProjectRef
}

func (c *authForProjectClient) GetIssue(ctx context.Context, p forge.ProjectRef, id string) (forge.Issue, error) {
	if p == c.bad {
		return forge.Issue{}, &forge.ClassifiedError{Class: forge.ErrAuthOrCapability, Summary: "forbidden"}
	}
	return c.Fake.GetIssue(ctx, p, id)
}
