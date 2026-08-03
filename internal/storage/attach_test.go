package storage

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func seedAttachTarget(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg-attach", "project-attach", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedLaunchRunForTest(ctx, "run-attach", "project-attach", "cfg-attach", testNow, "/work"); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE attempts SET backend='tmux' WHERE run_id='run-attach'`)
	mustExec(t, db, `UPDATE attempt_claims SET dispatch_id='dispatch-attach', bootstrap_nonce_hash=x'01', run_token_hash=x'02' WHERE run_id='run-attach'`)
	mustExec(t, db, `INSERT INTO gate_input_snapshots (id,gate_input_hash,schema_version,canonical_json,head_sha,effective_policy_hash,certification_version,risk_source_version,created_at_ms) VALUES ('gate-input-attach','gate-hash-attach',1,'{}','head','policy','cert','risk',?)`, testNow)
	mustExec(t, db, `INSERT INTO gate_evaluations (id,run_id,snapshot_id,gate_version,verdict_json,verdict_digest,cache_hit,created_at_ms) VALUES ('gate-attach','run-attach','gate-input-attach','gate/v1','{}','gate-verdict',0,?)`, testNow)
	mustExec(t, db, `INSERT INTO budget_entries (id,kind,scope,scope_id,bucket_start_ms,amount,reason,run_id,operation_key,created_at_ms) VALUES ('budget-attach','attention','run','run-attach',0,1,'test','run-attach','budget:attach',?)`, testNow)
	mustExec(t, db, `INSERT INTO interrupts (id,run_id,attempt_no,generation_key,reason,severity,headline,brief_markdown,options_json,min_modality,nonce,status,dispatch_state,expires_at_ms,on_expire,max_escalations,nonce_issued_at_ms,charged_budget_entry_id,created_at_ms,updated_at_ms) VALUES ('interrupt-attach','run-attach',1,'attach-generation','design_approval','normal','attach','attach test','{}','text','nonce','open','ready',?,'hold',0,?,'budget-attach',?,?)`, testNow+1000, testNow, testNow, testNow)
}

func attachProjection(t *testing.T, db *DB) map[string][]string {
	t.Helper()
	queries := map[string]string{
		"runs":       `SELECT id || '|' || status || '|' || version || '|' || updated_at_ms FROM runs ORDER BY id`,
		"attempts":   `SELECT run_id || '|' || attempt_no || '|' || phase || '|' || generation || '|' || backend || '|' || COALESCE(attempt_resolution,'') || '|' || isolation_state FROM attempts ORDER BY run_id,attempt_no`,
		"claims":     `SELECT run_id || '|' || attempt_no || '|' || generation || '|' || launch_operation_key || '|' || COALESCE(dispatch_id,'') FROM attempt_claims ORDER BY run_id,attempt_no`,
		"gates":      `SELECT id || '|' || run_id || '|' || snapshot_id || '|' || verdict_digest FROM gate_evaluations ORDER BY id`,
		"interrupts": `SELECT id || '|' || run_id || '|' || status || '|' || version FROM interrupts ORDER BY id`,
		"outbox":     `SELECT id || '|' || operation_key || '|' || state || '|' || attempt_count || '|' || COALESCE(run_id,'') FROM outbox_operations ORDER BY id`,
	}
	out := make(map[string][]string, len(queries))
	for name, query := range queries {
		rows, err := db.db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var row string
			if err := rows.Scan(&row); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			out[name] = append(out[name], row)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func TestAttachTargetForRunReturnsActiveTmuxBindingWithoutDomainWrites(t *testing.T) {
	db, _ := openTestDB(t)
	seedAttachTarget(t, db)
	before := attachProjection(t, db)
	target, err := db.AttachTargetForRun(context.Background(), "run-attach")
	if err != nil {
		t.Fatal(err)
	}
	want := AttachTarget{RunID: "run-attach", AttemptNo: 1, Generation: 1, Backend: "tmux", DispatchID: "dispatch-attach"}
	if target != want {
		t.Fatalf("attach target = %#v, want %#v", target, want)
	}
	if after := attachProjection(t, db); !reflect.DeepEqual(after, before) {
		t.Fatalf("attach changed durable domain projection:\nbefore=%#v\nafter =%#v", before, after)
	}
}

func TestAttachTargetForRunRejectsStaleGeneration(t *testing.T) {
	db, _ := openTestDB(t)
	seedAttachTarget(t, db)
	mustExec(t, db, `UPDATE attempt_claims SET generation=2, dispatch_id='stale-dispatch' WHERE run_id='run-attach' AND attempt_no=1`)

	if _, err := db.AttachTargetForRun(context.Background(), "run-attach"); !errors.Is(err, ErrAttachConflict) {
		t.Fatalf("AttachTargetForRun error = %v, want ErrAttachConflict", err)
	}
}

func TestAttachTargetForRunFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, db *DB)
		runID string
		want  error
	}{
		{name: "missing run", runID: "missing", want: ErrAttachRunNotFound},
		{name: "terminal attempt", runID: "run-attach", want: ErrAttachConflict, setup: func(t *testing.T, db *DB) {
			if err := db.SeedFailedAttemptForTest(context.Background(), "run-attach", 1, testNow+1); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "missing dispatch", runID: "run-attach", want: ErrAttachConflict, setup: func(t *testing.T, db *DB) {
			mustExec(t, db, `UPDATE attempt_claims SET dispatch_id=NULL, bootstrap_nonce_hash=NULL, run_token_hash=NULL WHERE run_id='run-attach'`)
		}},
		{name: "ambiguous active attempts", runID: "run-attach", want: ErrAttachConflict, setup: func(t *testing.T, db *DB) {
			mustExec(t, db, `DROP INDEX attempts_single_live_phase`)
			mustExec(t, db, `INSERT INTO attempts (run_id,attempt_no,phase,generation,backend,agent_id,task_spec_snapshot_id,worktree_path,branch_name,base_ref,base_sha,isolation_state,created_at_ms,updated_at_ms) VALUES ('run-attach',2,'starting',2,'tmux','agent','task-run-attach','/work','main','main','base','none',?,?)`, testNow, testNow)
			mustExec(t, db, `INSERT INTO attempt_claims (run_id,attempt_no,generation,launch_operation_key,dispatch_id,bootstrap_nonce_hash,run_token_hash,created_at_ms,updated_at_ms) VALUES ('run-attach',2,2,'launch:run-attach:2:2','dispatch-2',x'03',x'04',?,?)`, testNow, testNow)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := openTestDB(t)
			seedAttachTarget(t, db)
			if tc.setup != nil {
				tc.setup(t, db)
			}
			before := attachProjection(t, db)
			_, err := db.AttachTargetForRun(context.Background(), tc.runID)
			if !errors.Is(err, tc.want) {
				t.Fatalf("AttachTargetForRun error = %v, want %v", err, tc.want)
			}
			if after := attachProjection(t, db); !reflect.DeepEqual(after, before) {
				t.Fatalf("failed attach changed durable domain projection:\nbefore=%#v\nafter =%#v", before, after)
			}
		})
	}
}
