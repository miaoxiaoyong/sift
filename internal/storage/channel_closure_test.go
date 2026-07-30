package storage

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestReportQuotaExhaustionProducesFrozenChannelDelivery(t *testing.T) {
	db, ctx := seedReportQuotaRun(t, 1)
	channelConfig := `{"attention":{"channels":[{"id":"ops","enabled":true,"type":"webhook","target_ref":"secret_ref:OPS","capabilities":["voice"],"renderer":"plain-v1","default":true}],"daily_quota":{"low":3,"normal":5,"high":5},"day_timezone":"UTC","daily_summary_at":"09:00","max_escalations":0,"critical_fuse":{"window":900000,"total_limit":5,"per_run_limit":2}},"report":{"burst":8,"dedupe_window":0,"events_per_minute":60,"interrupts_per_run_daily_quota":1,"max_payload_bytes":65536,"not_ready_initial_delay":100000000,"not_ready_max_delay":1000000000,"not_ready_total_timeout":10000000000},"runtime":{"retry_multiplier":2}}`
	mustExec(t, db, `INSERT INTO config_snapshots(id,config_hash,schema_version,canonical_json,source_present,loaded_at_ms,binary_version) VALUES ('cfg-report-channels','report-hash-channels',1,?,1,?, 'test')`, channelConfig, testNow)
	mustExec(t, db, `UPDATE runs SET config_snapshot_id='cfg-report-channels' WHERE id='run'`)
	if err := submitBlocker(ctx, db, "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if err := submitBlocker(ctx, db, "1123456789abcdef0123456789abcdef"); err == nil {
		t.Fatal("quota exhaustion unexpectedly succeeded")
	}
	var payload string
	if err := db.db.QueryRowContext(ctx, `SELECT payload_json FROM outbox_operations WHERE kind='channel_publish'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatal(err)
	}
	channel, ok := got["channel"].(map[string]any)
	if !ok || channel["id"] != "ops" || channel["target_ref"] != "secret_ref:OPS" || channel["type"] != "webhook" {
		t.Fatalf("frozen channel payload = %#v", got["channel"])
	}
	if got["delivery_id"] == "" || got["interrupt_id"] == "" || got["nonce"] == "" {
		t.Fatalf("incomplete report-to-channel payload = %#v", got)
	}
}

func TestReportQuotaCommandRetainsFrozenChannels(t *testing.T) {
	cfg := reportRuntimeConfig{}
	cfg.Attention.Channels = []reportRuntimeChannel{{ID: "ops", Enabled: true, Type: "webhook", TargetRef: "secret_ref:OPS", Capabilities: []string{"voice"}, Renderer: "plain-v1", Default: true}}
	cmd := reportQuotaCmd(ReportSubmitCmd{RunID: "run", NowMS: testNow}, 1, testNow, testNow+1, cfg)
	if len(cmd.Channels) != 1 || cmd.Channels[0].ID != "ops" || cmd.Channels[0].TargetRef != "secret_ref:OPS" || cmd.Channels[0].Isolated {
		t.Fatalf("quota channels = %#v", cmd.Channels)
	}
}

func TestMemberedBatchCannotBeRetargeted(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	at := int64(testNow + 1)
	cmd := t6Command(testNow)
	cmd.AttentionDailyQuota = map[InterruptSeverity]int{SeverityLow: 0, SeverityNormal: 0, SeverityHigh: 0}
	cmd.Channels = []InterruptChannel{{ID: "ops", Type: "webhook", TargetRef: "secret_ref:OPS", Renderer: "plain-v1", Capabilities: []string{"visual"}}}
	cmd.BatchAtMS = &at
	if _, err := emitTestInterrupt(t, ctx, db, cmd); err != nil {
		t.Fatal(err)
	}
	var batch string
	if err := db.db.QueryRow(`SELECT batch_id FROM attention_batch_members`).Scan(&batch); err != nil {
		t.Fatal(err)
	}
	for column, value := range map[string]any{
		"project_id":            "other-project",
		"channel_id":            "other-channel",
		"channel_snapshot_json": `{"id":"other"}`,
		"forge_kind":            "gitlab",
		"forge_host":            "retarget.invalid",
		"forge_project_key":     "other/project",
		"target_kind":           "change",
		"target_id":             "99",
		"kind":                  "critical_fuse",
		"delivery_id":           "other:publish:1",
		"scope":                 "global",
		"scope_id":              "global",
		"episode_admission_id":  "other-admission",
		"due_at_ms":             testNow + 99,
	} {
		if _, err := db.db.Exec(`UPDATE attention_batches SET `+column+`=? WHERE id=?`, value, batch); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("retarget %s error = %v", column, err)
		}
	}
}

func TestLegacyDailyBatchWithNullCriticalLimitsCanBeReopened(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	at := int64(testNow + 1)
	cmd := t6Command(testNow)
	cmd.AttentionDailyQuota = map[InterruptSeverity]int{SeverityLow: 0, SeverityNormal: 0, SeverityHigh: 0}
	cmd.Channels = []InterruptChannel{{ID: "ops", Type: "webhook", TargetRef: "secret_ref:OPS", Renderer: "plain-v1", Capabilities: []string{"visual"}}}
	cmd.BatchAtMS = &at
	in, err := emitTestInterrupt(t, ctx, db, cmd)
	if err != nil {
		t.Fatal(err)
	}
	var batch, nonce, snapshot, zone, summary string
	var version int64
	if err := db.db.QueryRowContext(ctx, `SELECT m.batch_id,i.nonce,i.channel_snapshot_json,i.day_timezone,i.daily_summary_at,i.version FROM attention_batch_members m JOIN interrupts i ON i.id=m.interrupt_id WHERE m.interrupt_id=?`, in.ID).Scan(&batch, &nonce, &snapshot, &zone, &summary, &version); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE attention_batches SET critical_window_ms=NULL,critical_total_limit=NULL,critical_per_run_limit=NULL WHERE id=?`, batch); err != nil {
		t.Fatal(err)
	}
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := addDailyBatchMemberTx(ctx, tx, in.ID, version, nonce, testNow+2, "ops", snapshot, zone, summary); err != nil {
		tx.Rollback()
		t.Fatalf("reopen legacy daily batch: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestDailyBatchCannotInjectCriticalLimitsDuringTerminalTransition(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	at := int64(testNow + 1)
	cmd := t6Command(testNow)
	cmd.AttentionDailyQuota = map[InterruptSeverity]int{SeverityLow: 0, SeverityNormal: 0, SeverityHigh: 0}
	cmd.Channels = []InterruptChannel{{ID: "ops", Type: "webhook", TargetRef: "secret_ref:OPS", Renderer: "plain-v1", Capabilities: []string{"visual"}}}
	cmd.BatchAtMS = &at
	in, err := emitTestInterrupt(t, ctx, db, cmd)
	if err != nil {
		t.Fatal(err)
	}
	var batch string
	if err := db.db.QueryRowContext(ctx, `SELECT batch_id FROM attention_batch_members WHERE interrupt_id=?`, in.ID).Scan(&batch); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"sealed", "delivered", "cancelled"} {
		_, err := db.db.ExecContext(ctx, `UPDATE attention_batches SET state=?,critical_window_ms=1,critical_total_limit=1,critical_per_run_limit=1 WHERE id=?`, state, batch)
		if err == nil || !strings.Contains(err.Error(), "daily attention batch cannot carry critical limits") {
			t.Fatalf("daily %s transition error = %v", state, err)
		}
	}
	var got string
	if err := db.db.QueryRowContext(ctx, `SELECT state FROM attention_batches WHERE id=?`, batch).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "collecting" {
		t.Fatalf("batch state after rejected transitions = %q", got)
	}
}

func TestDailyBatchRejectsEachCriticalLimitAcrossTerminalStates(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	at := int64(testNow + 1)
	cmd := t6Command(testNow)
	cmd.AttentionDailyQuota = map[InterruptSeverity]int{SeverityLow: 0, SeverityNormal: 0, SeverityHigh: 0}
	cmd.Channels = []InterruptChannel{{ID: "ops", Type: "webhook", TargetRef: "secret_ref:OPS", Renderer: "plain-v1", Capabilities: []string{"visual"}}}
	cmd.BatchAtMS = &at
	in, err := emitTestInterrupt(t, ctx, db, cmd)
	if err != nil {
		t.Fatal(err)
	}
	var batch string
	if err := db.db.QueryRowContext(ctx, `SELECT batch_id FROM attention_batch_members WHERE interrupt_id=?`, in.ID).Scan(&batch); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"sealed", "delivered", "cancelled"} {
		for _, column := range []string{"critical_window_ms", "critical_total_limit", "critical_per_run_limit"} {
			if _, err := db.db.ExecContext(ctx, `UPDATE attention_batches SET state=?, `+column+`=1 WHERE id=?`, state, batch); err == nil || !strings.Contains(err.Error(), "daily attention batch cannot carry critical limits") {
				t.Fatalf("daily %s %s transition error = %v", state, column, err)
			}
		}
	}
}

func TestSealedBatchMemberAuthorityCannotBeRetargeted(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	at := int64(testNow + 1)
	cmd := t6Command(testNow)
	cmd.AttentionDailyQuota = map[InterruptSeverity]int{SeverityLow: 0, SeverityNormal: 0, SeverityHigh: 0}
	cmd.Channels = []InterruptChannel{{ID: "ops", Type: "webhook", TargetRef: "secret_ref:OPS", Renderer: "plain-v1", Capabilities: []string{"visual"}}}
	cmd.BatchAtMS = &at
	in, err := emitTestInterrupt(t, ctx, db, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PrepareDueAttentionBatches(ctx, testNow+48*60*60*1000); err != nil {
		t.Fatal(err)
	}
	var batch string
	if err := db.db.QueryRowContext(ctx, `SELECT batch_id FROM attention_batch_members WHERE interrupt_id=?`, in.ID).Scan(&batch); err != nil {
		t.Fatal(err)
	}
	for column, value := range map[string]any{"episode_admission_id": "other-admission", "critical_window_ms": int64(99), "critical_total_limit": 99, "critical_per_run_limit": 99} {
		if _, err := db.db.ExecContext(ctx, `UPDATE attention_batches SET `+column+`=? WHERE id=?`, value, batch); err == nil || (!strings.Contains(err.Error(), "immutable") && !strings.Contains(err.Error(), "daily attention batch cannot carry critical limits")) {
			t.Fatalf("sealed %s update error = %v", column, err)
		}
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE attention_batch_members SET channel_id='other' WHERE batch_id=? AND interrupt_id=?`, batch, in.ID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("member retarget error = %v", err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE attention_batch_members SET nonce='other' WHERE batch_id=? AND interrupt_id=?`, batch, in.ID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("member nonce update error = %v", err)
	}
	var authorityCount int
	if err := db.db.QueryRowContext(ctx, `SELECT count(*) FROM attention_batch_member_authority WHERE batch_id=? AND interrupt_id=?`, batch, in.ID).Scan(&authorityCount); err != nil {
		t.Fatal(err)
	}
	if authorityCount != 1 {
		t.Fatalf("authority rows = %d, want 1", authorityCount)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE attention_batch_member_authority SET nonce='other' WHERE batch_id=? AND interrupt_id=?`, batch, in.ID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("snapshot retarget error = %v", err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE attention_batch_member_authority SET updated_at_ms=updated_at_ms+1 WHERE batch_id=? AND interrupt_id=?`, batch, in.ID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("snapshot timestamp update error = %v", err)
	}
	if _, err := db.db.ExecContext(ctx, `DELETE FROM attention_batch_member_authority WHERE batch_id=? AND interrupt_id=?`, batch, in.ID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("snapshot delete error = %v", err)
	}
	if _, err := db.db.ExecContext(ctx, `DELETE FROM attention_batch_members WHERE batch_id=? AND interrupt_id=?`, batch, in.ID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("member delete error = %v", err)
	}
}

func TestSealedBatchPayloadDigestAndAuthoritySurviveReseal(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	at := int64(testNow + 1)
	cmd := t6Command(testNow)
	cmd.AttentionDailyQuota = map[InterruptSeverity]int{SeverityLow: 0, SeverityNormal: 0, SeverityHigh: 0}
	cmd.Channels = []InterruptChannel{{ID: "ops", Type: "webhook", TargetRef: "secret_ref:OPS", Renderer: "plain-v1", Capabilities: []string{"visual"}}}
	cmd.BatchAtMS = &at
	if _, err := emitTestInterrupt(t, ctx, db, cmd); err != nil {
		t.Fatal(err)
	}
	if err := db.PrepareDueAttentionBatches(ctx, testNow+48*60*60*1000); err != nil {
		t.Fatal(err)
	}
	var payload, digest, key string
	if err := db.db.QueryRowContext(ctx, `SELECT payload_json,payload_digest,operation_key FROM attention_batches WHERE state='sealed'`).Scan(&payload, &digest, &key); err != nil {
		t.Fatal(err)
	}
	if digest != digestJSON([]byte(payload)) || key == "" {
		t.Fatalf("sealed payload identity = %q/%q", digest, key)
	}
	if err := db.PrepareDueAttentionBatches(ctx, testNow+48*60*60*1000+1); err != nil {
		t.Fatal(err)
	}
	var payloadAgain, digestAgain string
	if err := db.db.QueryRowContext(ctx, `SELECT payload_json,payload_digest FROM attention_batches WHERE state='sealed'`).Scan(&payloadAgain, &digestAgain); err != nil {
		t.Fatal(err)
	}
	if payloadAgain != payload || digestAgain != digest {
		t.Fatal("reseal changed immutable batch payload")
	}
}

func TestChannelDiagnosticsIncludesBatchFailureProjection(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"batch_id":"batch","batch_kind":"daily_summary","channel":{"id":"ops","renderer":"plain-v1","target_ref":"secret_ref:OPS","type":"webhook"},"delivery_id":"batch:publish:1","delivery_kind":"attention_batch","due_at_ms":1,"forge_alert_target":{"forge_host":"github.com","forge_kind":"github","forge_project_key":"org/repo-project","target_id":"42","target_kind":"issue"},"project_id":"project","scope":"day","scope_id":"UTC:1"}`)
	if err := db.EnqueueChannelPublish(ctx, Operation{Key: "attention-batch:batch:publish:1", Kind: OperationChannelPublish, Payload: payload}, "batch:publish:1", testNow); err != nil {
		t.Fatal(err)
	}
	db.SetChannelPolicy(3, 3)
	wakes := 0
	db.SetOutboxWakeup(func() { wakes++ })
	claim, err := db.ClaimOutboxOperationKind(ctx, "worker", OperationChannelPublish, testNow, 100)
	if err != nil || claim == nil {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	if err := db.CompleteOutboxAttempt(ctx, *claim, CompleteOutcome{State: OperationRetryable, ErrorClass: ErrorTransient, ErrorSummary: "safe", NowMS: testNow + 1}); err != nil {
		t.Fatal(err)
	}
	got, err := db.ChannelDiagnostics(ctx)
	if err != nil || len(got) != 1 || got[0]["consecutive_failures"] != int64(1) || got[0]["generated_not_delivered"] != true {
		t.Fatalf("diagnostics = %#v, %v", got, err)
	}
	second, err := db.ClaimOutboxOperationKind(ctx, "worker", OperationChannelPublish, testNow+2, 100)
	if err != nil || second == nil {
		t.Fatalf("second claim = %#v, %v", second, err)
	}
	if err := db.CompleteOutboxAttempt(ctx, *second, CompleteOutcome{State: OperationRetryable, ErrorClass: ErrorRateLimited, ErrorSummary: "safe", NowMS: testNow + 3}); err != nil {
		t.Fatal(err)
	}
	third, err := db.ClaimOutboxOperationKind(ctx, "worker", OperationChannelPublish, testNow+4, 100)
	if err != nil || third == nil || third.ClaimAttemptNo != 3 {
		t.Fatalf("third claim = %#v, %v", third, err)
	}
	if claimed, err := db.ClaimOutboxOperationKind(ctx, "reclaimer", OperationChannelPublish, testNow+105, 100); err != nil || claimed != nil {
		t.Fatalf("terminal reclaim = %#v, %v", claimed, err)
	}
	if wakes != 3 {
		t.Fatalf("channel completion/reclaim wakeups = %d, want 3", wakes)
	}
	if err := db.CompleteOutboxAttempt(ctx, *third, CompleteOutcome{State: OperationSucceeded, NowMS: testNow + 106}); err != ErrRejectedStaleWorker {
		t.Fatalf("stale terminal completion = %v", err)
	}
	var state string
	var failures int
	if err := db.db.QueryRow(`SELECT state FROM outbox_operations WHERE operation_key='attention-batch:batch:publish:1'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT consecutive_failures FROM channel_failure_episodes WHERE subject_id='batch:publish:1'`).Scan(&failures); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || failures != 3 {
		t.Fatalf("terminal state/failures = %s/%d", state, failures)
	}
	assertCount(t, db, "outbox_attempts", 3)
	assertCount(t, db, "outbox_attempt_results", 3)
	assertCount(t, db, "channel_failure_episodes", 1)
	assertCount(t, db, "outbox_operations", 2)
}
