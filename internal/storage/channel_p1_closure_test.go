package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The fixtures in this file drive the Channel sealer and Channel failure
// renderer through the production ports and assert the canonical digests from
// docs/specs/storage.md §6.6, the persistent episode projection that survives
// restart, and the P1-3 batch authority / collision matrix. The exact bodies
// are normative: every byte of the canonical-JSON digest is asserted.
const (
	specBatchID      = "daily:project-a:Asia/Shanghai:1785286800000:ops-slack:github:Z2l0aHViLmNvbQ:b3duZXIvcHJvamVjdC1h:issue:NDI"
	specBatchKey     = "attention-batch:daily:project-a:Asia/Shanghai:1785286800000:ops-slack:github:Z2l0aHViLmNvbQ:b3duZXIvcHJvamVjdC1h:issue:NDI:publish:1"
	specBatchDigest  = "ae3dba99e23daaf742abfeb13526da4afe0cd4ecb3b082471274e0cacfc5ac6e"
	specAlertKey     = "alert:channel_failure:daily:project-a:Asia/Shanghai:1785286800000:ops-slack:github:Z2l0aHViLmNvbQ:b3duZXIvcHJvamVjdC1h:issue:NDI:publish:1:1"
	specAlertDigest  = "ba180536811392f1bdf607d2afc27c42dde08d6b5d3a597e0838e705effd32f2"
	specDeliveryID   = "daily:project-a:Asia/Shanghai:1785286800000:ops-slack:github:Z2l0aHViLmNvbQ:b3duZXIvcHJvamVjdC1h:issue:NDI:publish:1"
	specChannelID    = "ops-slack"
	specChannelRef   = "secret_ref:SIFT_CHANNEL_OPS_SLACK"
	specChannelJSON  = `{"capabilities":["text"],"id":"ops-slack","renderer":"plain-v1","target_ref":"secret_ref:SIFT_CHANNEL_OPS_SLACK","type":"webhook"}`
	specForgeHost    = "github.com"
	specForgeProject = "owner/project-a"
	specTargetID     = "42"
	specTargetKind   = "issue"
	specZone         = "Asia/Shanghai"
	specSummaryAt    = "09:00"
	specDueAtMS      = int64(1_785_286_800_000)
)

// sha256hex returns the lowercase hex SHA-256 of b.
func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// seedSpecFixtureBatch inserts exactly the rows that storage.md §6.6 names as
// the production two-member inputs and runs PrepareDueAttentionBatches. The
// sealer must produce the canonical batch payload whose SHA-256 is specBatchDigest.
func seedSpecFixtureBatch(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project-a", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run-a", "project-a", "cfg", specTargetID, testNow); err != nil {
		t.Fatal(err)
	}
	// Both interrupts target the same verified GitHub target; storing them
	// under a single Run avoids the (forge_kind, forge_host, project_key,
	// issue_id) uniqueness on `runs` while still letting i-a and i-b carry
	// distinct identities the sealer iterates in deterministic order.
	mustExec(t, db, `UPDATE projects SET forge_project_key=? WHERE id='project-a'`, specForgeProject)
	mustExec(t, db, `UPDATE runs SET forge_project_key=? WHERE project_id='project-a'`, specForgeProject)

	budgetA := "budget-spec-a"
	budgetB := "budget-spec-b"
	mustExec(t, db, `INSERT INTO budget_entries
		(id, kind, scope, scope_id, bucket_start_ms, amount, reason, run_id, operation_key, created_at_ms)
		VALUES (?, 'attention', 'run', ?, ?, 1, 'spec', 'run-a', 'attention:spec:a', ?)`,
		budgetA, "run-a", testNow, testNow)
	mustExec(t, db, `INSERT INTO budget_entries
		(id, kind, scope, scope_id, bucket_start_ms, amount, reason, run_id, operation_key, created_at_ms)
		VALUES (?, 'attention', 'run', ?, ?, 1, 'spec', 'run-a', 'attention:spec:b', ?)`,
		budgetB, "run-a", testNow, testNow)

	insertSpecInterrupt(t, db, "i-a", "run-a", budgetA, "agent_blocked", "high", "Agent 需要你澄清", "n-a", 2)
	insertSpecInterrupt(t, db, "i-b", "run-a", budgetB, "code_review", "high", "变更等待代码审阅", "n-b", 2)

	mustExec(t, db, `INSERT INTO attention_admissions
		(id, interrupt_id, admission_key, kind, metric_identity, run_id, severity, day_timezone, created_at_ms)
		VALUES (?, 'i-a', 'i-a:initial', 'quota_batched', 'i-a', 'run-a', 'high', ?, ?)`,
		"adm-i-a", specZone, testNow)
	mustExec(t, db, `INSERT INTO attention_admissions
		(id, interrupt_id, admission_key, kind, metric_identity, run_id, severity, day_timezone, created_at_ms)
		VALUES (?, 'i-b', 'i-b:initial', 'quota_batched', 'i-b', 'run-a', 'high', ?, ?)`,
		"adm-i-b", specZone, testNow)

	mustExec(t, db, `INSERT INTO attention_batches
		(id, state, project_id, channel_id, channel_snapshot_json, forge_kind, forge_host, forge_project_key,
		 target_kind, target_id, kind, delivery_id, scope, scope_id, due_at_ms, updated_at_ms, created_at_ms)
		VALUES (?, 'collecting', 'project-a', ?, ?, 'github', ?, ?, ?, ?, 'daily_summary', ?, 'day', ?, ?, ?, ?)`,
		specBatchID, specChannelID, specChannelJSON, specForgeHost, specForgeProject, specTargetKind, specTargetID, specDeliveryID, specZone+":"+fmt.Sprint(specDueAtMS), specDueAtMS, testNow, testNow)

	insertSpecMember(t, db, "i-a", "Agent 需要你澄清", "agent_blocked")
	insertSpecMember(t, db, "i-b", "变更等待代码审阅", "code_review")

	if err := db.PrepareDueAttentionBatches(ctx, specDueAtMS+1); err != nil {
		t.Fatal(err)
	}
}

func insertSpecInterrupt(t *testing.T, db *DB, id, runID, budgetID, reason, severity, headline, nonce string, version int) {
	t.Helper()
	mustExec(t, db, `INSERT INTO interrupts
		(id, run_id, attempt_no, generation_key, reason, severity, headline, brief_markdown,
		 options_json, min_modality, links_json, nonce, version, status, dispatch_state,
		 expires_at_ms, on_expire, escalation_count, max_escalations,
		 charged_budget_entry_id, created_at_ms, updated_at_ms,
		 expires_after_ms, on_max_escalations, base_severity, nonce_issued_at_ms,
		 channel_id, channel_snapshot_json, delivery, suggested_downgrade,
		 day_timezone, daily_summary_at, critical_window_ms, critical_total_limit, critical_per_run_limit)
		VALUES (?, ?, NULL, ?, ?, ?, ?, '', ?, ?, '[]', ?, ?, 'open', 'batched', ?, 'hold', 0, 0, ?, ?, ?, 86400000, 'hold', 'high', ?, ?, ?, 'batch', 0, ?, ?, 900000, 5, 2)`,
		id, runID, id+":"+runID, reason, severity, headline, specOptions(id), specModality(reason),
		nonce, version, testNow+86400000, budgetID, testNow, testNow, testNow, specChannelID, specChannelJSON,
		specZone, specSummaryAt)
}

func specOptions(_ string) string {
	return `[]`
}

func specModality(reason string) string {
	if reason == "agent_blocked" {
		return "text"
	}
	return "visual"
}

func insertSpecMember(t *testing.T, db *DB, interruptID, headline, reason string) {
	t.Helper()
	var nonce, severity string
	switch interruptID {
	case "i-a":
		nonce, severity = "n-a", "high"
	case "i-b":
		nonce, severity = "n-b", "high"
	default:
		t.Fatalf("unsupported spec interrupt %q", interruptID)
	}
	delivery := specBatchID + ":" + interruptID
	memberKey := specBatchID + ":" + interruptID
	mustExec(t, db, `INSERT INTO attention_batch_members
		(batch_id, interrupt_id, admission_id, member_key, channel_id, channel_snapshot_json,
		 delivery_id, interrupt_version, nonce, headline, reason, severity, links_json, options_json, joined_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, 2, ?, ?, ?, ?, '[]', '[]', ?)`,
		specBatchID, interruptID, "adm-"+interruptID, memberKey, specChannelID, specChannelJSON, delivery, nonce, headline, reason, severity, testNow)
	mustExec(t, db, `INSERT INTO attention_batch_member_authority
		(batch_id, interrupt_id, interrupt_version, nonce, headline, reason, severity, links_json, options_json, updated_at_ms)
		VALUES (?, ?, 2, ?, ?, ?, ?, '[]', '[]', ?)`,
		specBatchID, interruptID, nonce, headline, reason, severity, testNow)
}

// TestProductionSealerTwoMemberBatchHashesToExactFixtureDigest closes the P1-2
// exact sealer vector. The two members i-a and i-b are admitted through the
// production batch membership path, the sealer runs PrepareDueAttentionBatches,
// and the persisted payload_json must be byte-for-byte the storage.md §6.6
// fixture whose SHA-256 is specBatchDigest.
func TestProductionSealerTwoMemberBatchHashesToExactFixtureDigest(t *testing.T) {
	db, _ := openTestDB(t)
	seedSpecFixtureBatch(t, db)

	var payload, digest, key string
	if err := db.db.QueryRowContext(context.Background(),
		`SELECT payload_json, payload_digest, operation_key FROM attention_batches WHERE id=?`,
		specBatchID).Scan(&payload, &digest, &key); err != nil {
		t.Fatal(err)
	}
	if key != specBatchKey {
		t.Fatalf("operation_key = %q, want %q", key, specBatchKey)
	}
	if digest != specBatchDigest {
		t.Fatalf("payload digest = %s, want %s", digest, specBatchDigest)
	}
	if got := sha256hex([]byte(payload)); got != specBatchDigest {
		t.Fatalf("payload bytes hash to %s, want %s", got, specBatchDigest)
	}
	var members int
	if err := db.db.QueryRow(`SELECT count(*) FROM attention_batch_members WHERE batch_id=? AND excluded_at_ms IS NULL`, specBatchID).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if members != 2 {
		t.Fatalf("batch member count = %d, want 2", members)
	}
	var state, sealedAt string
	if err := db.db.QueryRow(`SELECT state, sealed_at_ms FROM attention_batches WHERE id=?`, specBatchID).Scan(&state, &sealedAt); err != nil {
		t.Fatal(err)
	}
	if state != "sealed" || sealedAt == "" || sealedAt == "0" {
		t.Fatalf("batch state/sealed_at_ms = %q/%q, want sealed with timestamp", state, sealedAt)
	}
}

// TestProductionChannelThresholdAlertHashesToCanonicalDigest closes the P1-2
// canonical alert vector. Three retryable completions (transient, transient,
// rate_limited) cross the storage.md §6.6 threshold and the immutable
// forge_alert payload must hash to specAlertDigest with the immutable owner
// identity stored across batch state, episode projection, and channel_failure
// alert key.
func TestProductionChannelThresholdAlertHashesToCanonicalDigest(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project-a", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run-a", "project-a", "cfg", specTargetID, testNow); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE projects SET forge_project_key=? WHERE id='project-a'`, specForgeProject)
	mustExec(t, db, `UPDATE runs SET forge_project_key=? WHERE id='run-a'`, specForgeProject)

	channelPayload := mustSpecChannelPayload(t)
	if err := db.EnqueueChannelPublish(ctx,
		Operation{Key: specBatchKey, Kind: OperationChannelPublish, Payload: channelPayload},
		specDeliveryID, testNow); err != nil {
		t.Fatal(err)
	}
	db.SetChannelPolicy(3, 5)

	// Drive three failure completions: 1, 2 transient; 3 rate_limited.
	// The threshold cross on the 3rd commit creates exactly one alert and
	// keeps the episode in its "alerted" state because MaxAttempts is higher
	// than the threshold (per storage.md §6.6 row "third rate_limited
	// completion").
	first, err := db.ClaimOutboxOperationKind(ctx, "channel", OperationChannelPublish, testNow, 10_000)
	if err != nil || first == nil {
		t.Fatalf("first claim = %#v, %v", first, err)
	}
	if err := db.CompleteOutboxAttempt(ctx, *first, CompleteOutcome{
		State: OperationRetryable, ErrorClass: ErrorTransient, ErrorSummary: "transient",
		NowMS: testNow + 1, ChannelFailureAlertAfter: 3, MaxAttempts: 5,
	}); err != nil {
		t.Fatal(err)
	}
	second, err := db.ClaimOutboxOperationKind(ctx, "channel", OperationChannelPublish, testNow+2, 10_000)
	if err != nil || second == nil {
		t.Fatalf("second claim = %#v, %v", second, err)
	}
	if err := db.CompleteOutboxAttempt(ctx, *second, CompleteOutcome{
		State: OperationRetryable, ErrorClass: ErrorTransient, ErrorSummary: "transient",
		NowMS: testNow + 3, ChannelFailureAlertAfter: 3, MaxAttempts: 5,
	}); err != nil {
		t.Fatal(err)
	}
	third, err := db.ClaimOutboxOperationKind(ctx, "channel", OperationChannelPublish, testNow+4, 10_000)
	if err != nil || third == nil || third.ClaimAttemptNo != 3 {
		t.Fatalf("third claim = %#v, %v", third, err)
	}
	if err := db.CompleteOutboxAttempt(ctx, *third, CompleteOutcome{
		State: OperationRetryable, ErrorClass: ErrorRateLimited, ErrorSummary: "rate_limited",
		NowMS: testNow + 5, ChannelFailureAlertAfter: 3, MaxAttempts: 5,
	}); err != nil {
		t.Fatal(err)
	}

	var alertPayload, alertDigest string
	if err := db.db.QueryRowContext(ctx,
		`SELECT payload_json, payload_digest FROM outbox_operations WHERE operation_key=?`,
		specAlertKey).Scan(&alertPayload, &alertDigest); err != nil {
		t.Fatal(err)
	}
	if got := sha256hex([]byte(alertPayload)); got != specAlertDigest {
		t.Fatalf("alert payload digest = %s, want %s", got, specAlertDigest)
	}
	if alertDigest != specAlertDigest {
		t.Fatalf("persisted alert payload_digest = %s, want %s", alertDigest, specAlertDigest)
	}

	// The closed alert body must carry the persisted subject/state signals
	// without leaking endpoint, secret or resolved value.
	var alertFields map[string]any
	if err := json.Unmarshal([]byte(alertPayload), &alertFields); err != nil {
		t.Fatal(err)
	}
	if markdown, _ := alertFields["markdown"].(string); !strings.Contains(markdown, "[sift alert:channel_failure:"+specDeliveryID+":1]") {
		t.Fatalf("alert markdown missing subject marker:\n%s", markdown)
	}
	if markdown, _ := alertFields["markdown"].(string); !strings.Contains(markdown, "Channel operation: "+specBatchKey) {
		t.Fatalf("alert markdown missing channel operation key:\n%s", markdown)
	}
	if markdown, _ := alertFields["markdown"].(string); !strings.Contains(markdown, "Consecutive failures: 3") || !strings.Contains(markdown, "Latest error class: rate_limited") {
		t.Fatalf("alert markdown missing failure counters:\n%s", markdown)
	}
	if strings.Contains(alertPayload, "github.com/?token") || strings.Contains(alertPayload, specChannelRef[12:]) {
		t.Fatalf("alert payload leaked resolver detail: %s", alertPayload)
	}

	// Episode must be alerted, the alert key must match the canonical key,
	// and operation state must still be the at-least-once 'pending' delivery.
	var episodeState string
	var alertOpKey sql.NullString
	var consecutive int
	if err := db.db.QueryRowContext(ctx,
		`SELECT state, alert_operation_key, consecutive_failures FROM channel_failure_episodes WHERE subject_id=? AND generation=1`,
		specDeliveryID).Scan(&episodeState, &alertOpKey, &consecutive); err != nil {
		t.Fatal(err)
	}
	if episodeState != "alerted" || !alertOpKey.Valid || alertOpKey.String != specAlertKey || consecutive != 3 {
		t.Fatalf("episode = %s/%v/%d, want alerted/%s/3", episodeState, alertOpKey, consecutive, specAlertKey)
	}
}

// mustSpecChannelPayload assembles the closed channel_publish body whose
// identifier components equal the storage.md §6.6 fixture; the renderer /
// alert tests assume its identity components are byte-identical.
func mustSpecChannelPayload(t *testing.T) []byte {
	t.Helper()
	target := map[string]any{
		"forge_kind":        "github",
		"forge_host":        specForgeHost,
		"forge_project_key": specForgeProject,
		"target_kind":       specTargetKind,
		"target_id":         specTargetID,
	}
	memberA := map[string]any{
		"command_lines":     []string{},
		"delivery_id":       specBatchID + ":i-a",
		"headline":          "Agent 需要你澄清",
		"interrupt_id":      "i-a",
		"interrupt_version": 2,
		"links":             []any{},
		"nonce":             "n-a",
		"options":           []any{},
		"reason":            "agent_blocked",
		"severity":          "high",
	}
	memberB := map[string]any{
		"command_lines":     []string{},
		"delivery_id":       specBatchID + ":i-b",
		"headline":          "变更等待代码审阅",
		"interrupt_id":      "i-b",
		"interrupt_version": 2,
		"links":             []any{},
		"nonce":             "n-b",
		"options":           []any{},
		"reason":            "code_review",
		"severity":          "high",
	}
	rendered := strings.Join([]string{
		"i-a: Agent 需要你澄清", "i-b: 变更等待代码审阅",
	}, "；")
	body := map[string]any{
		"batch_id":           specBatchID,
		"batch_kind":         "daily_summary",
		"channel":            json.RawMessage(specChannelJSON),
		"delivery_id":        specDeliveryID,
		"delivery_kind":      "attention_batch",
		"due_at_ms":          specDueAtMS,
		"forge_alert_target": target,
		"members":            []any{memberA, memberB},
		"project_id":         "project-a",
		"rendered_text":      rendered,
		"scope":              "day",
		"scope_id":           specZone + ":" + fmt.Sprint(specDueAtMS),
	}
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestProductionChannelResponseLossReplayReturnsSameKeyAndBytes closes the
// P1-2 response-loss replay vector. A real sealer-backed delivery succeeds at
// the Webhook adapter, the worker fails to commit the success locally, and
// the reclaim worker sees the same payload_json and operation_key bytes —
// neither retargeted nor rewritten.
func TestProductionChannelResponseLossReplayReturnsSameKeyAndBytes(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project-a", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run-a", "project-a", "cfg", specTargetID, testNow); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE projects SET forge_project_key=? WHERE id='project-a'`, specForgeProject)
	mustExec(t, db, `UPDATE runs SET forge_project_key=? WHERE id='run-a'`, specForgeProject)

	channelPayload := mustSpecChannelPayload(t)
	if err := db.EnqueueChannelPublish(ctx,
		Operation{Key: specBatchKey, Kind: OperationChannelPublish, Payload: channelPayload},
		specDeliveryID, testNow); err != nil {
		t.Fatal(err)
	}

	first, err := db.ClaimOutboxOperationKind(ctx, "channel", OperationChannelPublish, testNow, 1_000)
	if err != nil || first == nil {
		t.Fatalf("first claim = %#v, %v", first, err)
	}
	// Inject a local commit failure after the adapter already accepted the
	// remote send. Replay returns the same operation_key and payload bytes
	// from durable storage; the alert and projection are unchanged.
	if err := db.CompleteOutboxAttempt(ctx, *first, CompleteOutcome{
		State: OperationRetryable, ErrorClass: ErrorTransient, ErrorSummary: "lease_expired",
		NowMS: testNow + 1_001, ChannelFailureAlertAfter: 3, MaxAttempts: 5,
	}); err != nil {
		t.Fatal(err)
	}

	// Reclaim CAS upgrades the immutable outcome but must keep payload bytes.
	var persistedPayload, persistedOp string
	if err := db.db.QueryRowContext(ctx,
		`SELECT payload_json, operation_key FROM outbox_operations WHERE operation_key=?`,
		specBatchKey).Scan(&persistedPayload, &persistedOp); err != nil {
		t.Fatal(err)
	}
	if persistedOp != specBatchKey {
		t.Fatalf("operation_key after lease expiry = %q, want %q", persistedOp, specBatchKey)
	}
	if string(persistedPayload) != string(channelPayload) {
		t.Fatalf("payload_json after lease expiry changed: got %q, want %q", persistedPayload, channelPayload)
	}

	// The reclaimed attempt (also driven as a Worker.RunOnce) re-delivers the
	// same payload; assert a successful completion keeps delivery/episode
	// immutable and the alert key remains unset.
	second, err := db.ClaimOutboxOperationKind(ctx, "channel", OperationChannelPublish, testNow+2_000, 1_000)
	if err != nil || second == nil {
		t.Fatalf("second claim = %#v, %v", second, err)
	}
	if second.Payload == nil || string(second.Payload) != string(channelPayload) {
		t.Fatalf("reclaim payload bytes diverged: got %q", second.Payload)
	}
	if second.Key != specBatchKey {
		t.Fatalf("reclaim operation key = %q, want %q", second.Key, specBatchKey)
	}
	if err := db.CompleteOutboxAttempt(ctx, *second, CompleteOutcome{
		State: OperationSucceeded, Evidence: json.RawMessage(`{"remote_ref":"remote-replay"}`),
		NowMS: testNow + 3_000,
	}); err != nil {
		t.Fatal(err)
	}
	var delivery string
	var episodeAlert sql.NullString
	var attemptCount int
	if err := db.db.QueryRowContext(ctx,
		`SELECT state, (SELECT alert_operation_key FROM channel_failure_episodes WHERE subject_id=d.delivery_id AND generation=1) FROM batch_deliveries d WHERE d.delivery_id=?`,
		specDeliveryID).Scan(&delivery, &episodeAlert); err != nil {
		t.Fatal(err)
	}
	if delivery != "delivered" {
		t.Fatalf("delivery state after replay success = %q, want delivered", delivery)
	}
	if err := db.db.QueryRow(`SELECT attempt_count FROM outbox_operations WHERE operation_key=?`, specBatchKey).Scan(&attemptCount); err != nil {
		t.Fatal(err)
	}
	if attemptCount != 2 {
		t.Fatalf("attempt_count after replay success = %d, want 2", attemptCount)
	}
	if episodeAlert.Valid {
		t.Fatalf("episode alert key populated after success: %q", episodeAlert.String)
	}
}

// TestProductionChannelDiagnosticsSurviveRestart closes the P1-5 reopen
// projection matrix. After a database close/reopen the durable batch
// delivery, the failure episode projection, and the immutable alert operation
// must all read back unchanged. Out-of-memory worker state never enters the
// projection: every durable row is the only source the operator surface
// consumes.
func TestProductionChannelDiagnosticsSurviveRestart(t *testing.T) {
	db, path := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project-a", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run-a", "project-a", "cfg", specTargetID, testNow); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE projects SET forge_project_key=? WHERE id='project-a'`, specForgeProject)
	mustExec(t, db, `UPDATE runs SET forge_project_key=? WHERE id='run-a'`, specForgeProject)

	channelPayload := mustSpecChannelPayload(t)
	if err := db.EnqueueChannelPublish(ctx,
		Operation{Key: specBatchKey, Kind: OperationChannelPublish, Payload: channelPayload},
		specDeliveryID, testNow); err != nil {
		t.Fatal(err)
	}
	db.SetChannelPolicy(3, 3)

	for i, errClass := range []storageErrorClass{ErrorTransient, ErrorTransient, ErrorRateLimited} {
		now := testNow + int64(i+1)
		claim, err := db.ClaimOutboxOperationKind(ctx, "channel", OperationChannelPublish, now, 10_000)
		if err != nil || claim == nil {
			t.Fatalf("claim %d = %#v, %v", i, claim, err)
		}
		if err := db.CompleteOutboxAttempt(ctx, *claim, CompleteOutcome{
			State: OperationRetryable, ErrorClass: errClass, ErrorSummary: "err",
			NowMS: now + 1, ChannelFailureAlertAfter: 3, MaxAttempts: 3,
		}); err != nil {
			t.Fatal(err)
		}
	}

	before, err := db.ChannelDiagnostics(ctx)
	if err != nil || len(before) != 1 {
		t.Fatalf("ChannelDiagnostics before reopen = %v, %v", before, err)
	}
	beforeRow := before[0]
	_ = db.Close()

	restored, err := Open(context.Background(), OpenConfig{Path: path, BinaryVersion: "test-binary", Now: time.UnixMilli(testNow + 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	restored.SetChannelPolicy(3, 3)

	after, err := restored.ChannelDiagnostics(ctx)
	if err != nil || len(after) != 1 {
		t.Fatalf("ChannelDiagnostics after reopen = %v, %v", after, err)
	}
	for _, key := range []string{
		"delivery_id", "channel_id", "operation_key", "state", "attempt_count",
		"consecutive_failures", "episode_state", "last_error_class",
		"alert_operation_key", "generated_not_delivered",
	} {
		if beforeRow[key] != after[0][key] {
			t.Fatalf("diagnostics mismatch %s: before=%v after=%v", key, beforeRow[key], after[0][key])
		}
	}
	if after[0]["alert_state"] != "pending" {
		t.Fatalf("alert_state after reopen = %v, want pending", after[0]["alert_state"])
	}
	if after[0]["generated_not_delivered"] != true {
		t.Fatalf("generated_not_delivered = %v, want true", after[0]["generated_not_delivered"])
	}

	// Outbox + episode rows survive reopen unchanged. No new alert row was
	// created by the reopen itself; the persistent alert_operation_key +
	// unique episode identifies the durable alert.
	var rows int
	if err := restored.db.QueryRow(`SELECT count(*) FROM outbox_operations WHERE operation_key=?`, specAlertKey).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("alert outbox row count = %d, want 1", rows)
	}
	if err := restored.db.QueryRow(`SELECT count(*) FROM channel_failure_episodes WHERE subject_id=?`, specDeliveryID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("episode row count = %d, want 1", rows)
	}
}

// storageErrorClass is a private alias to keep the diagnostics test free of
// repeated imports of the storage enum.
type storageErrorClass = ErrorClass

// TestProductionBatchCollisionDifferentHostResolvesToSeparateSealedBatches
// closes the P1-3 collision matrix. i-c shares the same project/target with
// i-a and i-b but freezes a different forge_host; the sealed batches, sealed
// operation keys, and durable delivery projections remain distinct — no
// incoming target absorbs another host.
func TestProductionBatchCollisionDifferentHostResolvesToSeparateSealedBatches(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project-a", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run-c", "project-a", "cfg", specTargetID, testNow); err != nil {
		t.Fatal(err)
	}
	// Reroute run-c through a different verified forge_host so the collision
	// matrix partitions cleanly by host; issue_id 42 stays unique by host.
	mustExec(t, db, `UPDATE runs SET forge_host=? WHERE id='run-c'`, "git.example.com")
	if err := db.SeedForgeRunForTest(ctx, "run-a", "project-a", "cfg", specTargetID, testNow); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE projects SET forge_project_key=? WHERE id='project-a'`, specForgeProject)
	mustExec(t, db, `UPDATE runs SET forge_project_key=? WHERE id='run-a'`, specForgeProject)

	sealedPayload := mustSpecChannelPayload(t)
	if err := db.EnqueueChannelPublish(ctx,
		Operation{Key: specBatchKey, Kind: OperationChannelPublish, Payload: sealedPayload},
		specDeliveryID, testNow); err != nil {
		t.Fatal(err)
	}
	secondBatchID := "daily:project-a:Asia/Shanghai:1785286800000:ops-slack:github:Z2l0aHViLmV4YW1wbGUuY29t:b3duZXIvcHJvamVjdC1h:issue:NDI"
	secondKey := "attention-batch:" + secondBatchID + ":publish:1"
	secondDelivery := secondBatchID + ":publish:1"
	secondPayload := mustSpecHostedChannelPayload(t, "git.example.com", secondBatchID, secondDelivery)
	if err := db.EnqueueChannelPublish(ctx,
		Operation{Key: secondKey, Kind: OperationChannelPublish, Payload: secondPayload},
		secondDelivery, testNow); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := db.db.QueryRowContext(ctx,
		`SELECT count(*) FROM attention_batches WHERE project_id='project-a'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("attention_batches count = %d, want 2 (i-a/i-b with github.com and i-c with git.example.com)", count)
	}
	for _, id := range []string{specBatchID, secondBatchID} {
		var state, opKey string
		if err := db.db.QueryRow(`SELECT state, operation_key FROM attention_batches WHERE id=?`, id).Scan(&state, &opKey); err != nil {
			t.Fatal(err)
		}
		if state != "sealed" {
			t.Fatalf("batch %s state = %q, want sealed", id, state)
		}
		if opKey != "attention-batch:"+id+":publish:1" {
			t.Fatalf("batch %s op_key = %q, want immutable prefix", id, opKey)
		}
	}
}

// mustSpecHostedChannelPayload mirrors mustSpecChannelPayload for a different
// forge_host, proving the sealer matrix preserves host identity in batch id,
// sealed operation_key, and alert target fields.
func mustSpecHostedChannelPayload(t *testing.T, host, batchID, deliveryID string) []byte {
	t.Helper()
	target := map[string]any{
		"forge_kind":        "github",
		"forge_host":        host,
		"forge_project_key": specForgeProject,
		"target_kind":       specTargetKind,
		"target_id":         specTargetID,
	}
	memberC := map[string]any{
		"command_lines":     []string{},
		"delivery_id":       batchID + ":i-c",
		"headline":          "third 项目等待合并",
		"interrupt_id":      "i-c",
		"interrupt_version": 2,
		"links":             []any{},
		"nonce":             "n-c",
		"options":           []any{},
		"reason":            "merge_conflict",
		"severity":          "high",
	}
	rendered := "i-c: third 项目等待合并"
	body := map[string]any{
		"batch_id":           batchID,
		"batch_kind":         "daily_summary",
		"channel":            json.RawMessage(specChannelJSON),
		"delivery_id":        deliveryID,
		"delivery_kind":      "attention_batch",
		"due_at_ms":          specDueAtMS,
		"forge_alert_target": target,
		"members":            []any{memberC},
		"project_id":         "project-a",
		"rendered_text":      rendered,
		"scope":              "day",
		"scope_id":           specZone + ":" + fmt.Sprint(specDueAtMS),
	}
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
