package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestV2InterruptFivePartCrashMatrix exercises every durable write family in
// EmitInterrupt independently.  The trigger is the crash boundary: a failed
// write must roll back the run transition, attention charge, interrupt,
// event, and publication together.
func TestV2InterruptFivePartCrashMatrix(t *testing.T) {
	families := []struct {
		name  string
		table string
		kind  string
	}{
		{"run transition", "runs", "update"},
		{"attention charge", "budget_entries", "insert"},
		{"interrupt", "interrupts", "insert"},
		{"admission", "attention_admissions", "insert"},
		{"binding", "interrupt_command_effect_bindings", "insert"},
		{"event", "events", "insert"},
		{"outbox", "outbox_operations", "insert"},
		{"delivery", "interrupt_deliveries", "insert"},
		{"target", "interrupt_command_targets", "insert"},
	}
	for _, family := range families {
		t.Run(family.name, func(t *testing.T) {
			db, _ := openTestDB(t)
			ctx := context.Background()
			seedCommandRun(t, db, ctx)
			before := projectionSnapshotFor(t, db, interruptProjectionTables)
			trigger := "v2_interrupt_crash"
			var sql string
			if family.kind == "update" {
				sql = fmt.Sprintf("CREATE TRIGGER %s BEFORE UPDATE ON %s WHEN NEW.id='%s' BEGIN SELECT RAISE(ABORT, 'injected crash'); END", trigger, family.table, cmdRun)
			} else {
				sql = fmt.Sprintf("CREATE TRIGGER %s BEFORE INSERT ON %s BEGIN SELECT RAISE(ABORT, 'injected crash'); END", trigger, family.table)
			}
			mustExec(t, db, sql)
			attempt := 1
			_, err := db.EmitInterrupt(ctx, EmitInterruptCmd{
				RunID: cmdRun, ExpectedRunVersion: 1, AttemptNo: &attempt, Reason: InterruptStartupStall,
				Facts:      map[string]string{"attempt_no": "1", "generation": "1", "diagnostic_cause": "termination_unconfirmed", "isolation_consequence": "worktree held", "attempt_diagnostic_ref": "/attempt", "worktree_ref": "/worktree", "recommended_action": "retry"},
				Generation: InterruptGeneration{AttemptNo: 1, Generation: 1}, GatePhase: GateNone, GuardrailLevel: GuardrailNone,
				ExpiresAfterMS: 10, OnExpire: ExpireEscalate, OnMaxEscalations: ExpireHold, MaxEscalations: 1,
				AttentionDailyQuota: interruptQuota(), DayTimezone: "UTC", Source: SourceRecovery, NowMS: testNow,
				Channels: []InterruptChannel{{ID: "text", Capabilities: []string{"text"}}},
			})
			if err == nil || !strings.Contains(err.Error(), "injected crash") {
				t.Fatalf("EmitInterrupt error = %v", err)
			}
			after := projectionSnapshotFor(t, db, interruptProjectionTables)
			assertProjectionUnchanged(t, "interrupt "+family.name, before, after)
		})
	}
}

// TestV2RetryProbeSuccessCrashMatrix is deliberately a write-point matrix,
// rather than a single trigger at the last write.  It protects the ADR-013
// all-or-nothing successor handoff (including the durable ack).
// TestV2InterruptFivePartSuccessProjection is the complementary happy path:
// every projection protected above must be committed together, including the
// Run version and attention counter that count-only crash tests used to miss.
func TestV2InterruptFivePartSuccessProjection(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	attempt := 1
	if _, err := db.EmitInterrupt(ctx, EmitInterruptCmd{RunID: cmdRun, ExpectedRunVersion: 1, AttemptNo: &attempt, Reason: InterruptStartupStall,
		Facts:      map[string]string{"attempt_no": "1", "generation": "1", "diagnostic_cause": "termination_unconfirmed", "isolation_consequence": "worktree held", "attempt_diagnostic_ref": "/attempt", "worktree_ref": "/worktree", "recommended_action": "retry"},
		Generation: InterruptGeneration{AttemptNo: 1, Generation: 1}, GatePhase: GateNone, GuardrailLevel: GuardrailNone,
		ExpiresAfterMS: 10, OnExpire: ExpireEscalate, OnMaxEscalations: ExpireHold, MaxEscalations: 1,
		AttentionDailyQuota: interruptQuota(), DayTimezone: "UTC", Source: SourceRecovery, NowMS: testNow,
		Channels: []InterruptChannel{{ID: "text", Capabilities: []string{"text"}}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, table := range interruptProjectionTables[1:] { // runs existed before emission.
		if got := countRows(t, db, table); got == 0 {
			t.Fatalf("successful interrupt omitted %s", table)
		}
	}
	var status string
	var version, attention int
	if err := db.db.QueryRow(`SELECT status,version FROM runs WHERE id=?`, cmdRun).Scan(&status, &version); err != nil || status != "waiting_human" || version != 2 {
		t.Fatalf("successful interrupt run=%s/%d: %v", status, version, err)
	}
	if err := db.db.QueryRow(`SELECT consumed_value FROM budget_counters WHERE kind='attention'`).Scan(&attention); err != nil || attention != 1 {
		t.Fatalf("successful interrupt attention counter=%d: %v", attention, err)
	}
}

// TestV2RetryProbeSuccessProjection proves the success transaction contains
// every write point simultaneously; CrashMatrix then aborts each one in turn.
func TestV2RetryProbeSuccessProjection(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	interruptID, nonce := emitStartupStallInterrupt(t, db, ctx)
	env := commentEnv(t, "project", "v2-retry-success", "/sift retry "+cmdRun+" "+nonce)
	if _, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5}); err != nil {
		t.Fatal(err)
	}
	var probeID string
	if err := db.db.QueryRow(`SELECT id FROM attempt_probes WHERE interrupt_id=?`, interruptID).Scan(&probeID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ApplyRetryProbeResult(ctx, RetryProbeResultCmd{InterruptID: interruptID, ProbeID: probeID, Succeeded: true, AbsenceEvidenceJSON: json.RawMessage(`{"absent":true}`), NowMS: testNow + 10}); err != nil {
		t.Fatal(err)
	}
	var status, probeState, resolution, isolation, interruptStatus string
	var version int64
	if err := db.db.QueryRow(`SELECT status,version FROM runs WHERE id=?`, cmdRun).Scan(&status, &version); err != nil || status != "queued" || version < 3 {
		t.Fatalf("successful retry run=%s/%d: %v", status, version, err)
	}
	if err := db.db.QueryRow(`SELECT state FROM attempt_probes WHERE id=?`, probeID).Scan(&probeState); err != nil || probeState != "succeeded" {
		t.Fatalf("successful retry probe=%s: %v", probeState, err)
	}
	if err := db.db.QueryRow(`SELECT attempt_resolution,isolation_state FROM attempts WHERE run_id=? AND attempt_no=1`, cmdRun).Scan(&resolution, &isolation); err != nil || resolution != "retry_after_absence" || isolation != "none" {
		t.Fatalf("successful retry old attempt=%s/%s: %v", resolution, isolation, err)
	}
	if err := db.db.QueryRow(`SELECT status FROM interrupts WHERE id=?`, interruptID).Scan(&interruptStatus); err != nil || interruptStatus != "closed" {
		t.Fatalf("successful retry interrupt=%s: %v", interruptStatus, err)
	}
	for _, table := range retryProjectionTables {
		if got := countRows(t, db, table); got == 0 {
			t.Fatalf("successful retry omitted %s", table)
		}
	}
	if got := countRows(t, db, "attempts WHERE run_id='"+cmdRun+"' AND attempt_no=2"); got != 1 {
		t.Fatalf("successful retry successors=%d, want 1", got)
	}
	if got := countRows(t, db, "attempt_claims WHERE run_id='"+cmdRun+"' AND attempt_no=2"); got != 1 {
		t.Fatalf("successful retry claim=%d, want 1", got)
	}
	if got := countRows(t, db, "outbox_operations WHERE run_id='"+cmdRun+"' AND kind IN ('launch_agent','command_ack')"); got < 2 {
		t.Fatalf("successful retry launch/ack outbox=%d, want both", got)
	}
}

func TestV2RetryProbeSuccessCrashMatrix(t *testing.T) {
	families := []struct{ name, table, event, condition string }{
		{"probe", "attempt_probes", "UPDATE", " WHEN NEW.state='succeeded' AND OLD.state IN ('pending','running')"},
		// These are two distinct attempts UPDATE write points. A bare BEFORE
		// UPDATE trigger would always abort resolution and leave release untested.
		{"old attempt resolution", "attempts", "UPDATE", " WHEN NEW.attempt_resolution='retry_after_absence' AND OLD.attempt_resolution IS NULL"},
		{"interrupt close", "interrupts", "UPDATE", " WHEN NEW.status='closed' AND OLD.status='open'"},
		{"run queued event", "events", "INSERT", " WHEN NEW.type='run.transitioned'"},
		{"final outcome", "command_event_outcomes", "UPDATE", " WHEN NEW.state='final' AND OLD.state='pending'"},
		{"isolation release", "attempts", "UPDATE", " WHEN NEW.isolation_state='none' AND OLD.isolation_state='frozen'"},
		{"successor attempt", "attempts", "INSERT", " WHEN NEW.attempt_no=2"},
		{"successor claim", "attempt_claims", "INSERT", " WHEN NEW.attempt_no=2"},
		{"launch operation", "outbox_operations", "INSERT", " WHEN NEW.kind='launch_agent'"},
		{"ack operation", "outbox_operations", "INSERT", " WHEN NEW.kind='command_ack'"},
	}
	for _, family := range families {
		t.Run(family.name, func(t *testing.T) {
			db, _ := openTestDB(t)
			ctx := context.Background()
			seedCommandRun(t, db, ctx)
			interruptID, nonce := emitStartupStallInterrupt(t, db, ctx)
			env := commentEnv(t, "project", "v2-retry-"+family.name, "/sift retry "+cmdRun+" "+nonce)
			if _, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5}); err != nil {
				t.Fatal(err)
			}
			var probeID string
			if err := db.db.QueryRow(`SELECT id FROM attempt_probes WHERE interrupt_id=?`, interruptID).Scan(&probeID); err != nil {
				t.Fatal(err)
			}
			before := projectionSnapshotFor(t, db, retryProjectionTables)
			trigger := "v2_probe_crash"
			mustExec(t, db, fmt.Sprintf("CREATE TRIGGER %s BEFORE %s ON %s%s BEGIN SELECT RAISE(ABORT, 'injected crash'); END", trigger, family.event, family.table, family.condition))
			_, err := db.ApplyRetryProbeResult(ctx, RetryProbeResultCmd{InterruptID: interruptID, ProbeID: probeID, Succeeded: true, AbsenceEvidenceJSON: json.RawMessage(`{"absent":true}`), NowMS: testNow + 10})
			if err == nil || !strings.Contains(err.Error(), "injected crash") {
				t.Fatalf("ApplyRetryProbeResult error = %v", err)
			}
			after := projectionSnapshotFor(t, db, retryProjectionTables)
			assertProjectionUnchanged(t, "retry "+family.name, before, after)
		})
	}
}

var interruptProjectionTables = []string{
	"runs", "budget_entries", "budget_counters", "interrupts", "attention_admissions",
	"interrupt_command_effect_bindings", "events", "outbox_operations", "interrupt_deliveries", "interrupt_command_targets",
}

// retryProjectionTables intentionally includes every ADR-013 projection rather
// than only row counts: updates to Run/version, evidence, resolution,
// isolation, Interrupt, counters, and outbox must roll back too.
var retryProjectionTables = []string{
	"runs", "attempt_probes", "attempts", "attempt_claims", "interrupts", "budget_entries", "budget_counters",
	"events", "command_event_outcomes", "outbox_operations",
}

type projectionSnapshot map[string]string

func projectionSnapshotFor(t *testing.T, db *DB, tables []string) projectionSnapshot {
	t.Helper()
	snapshot := make(projectionSnapshot, len(tables))
	for _, table := range tables {
		rows, err := db.db.Query("SELECT * FROM " + table + " ORDER BY rowid")
		if err != nil {
			t.Fatalf("snapshot %s: %v", table, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		var b strings.Builder
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			for i, value := range values {
				fmt.Fprintf(&b, "%s=%T:%v|", columns[i], value, value)
			}
			b.WriteByte('\n')
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		snapshot[table] = b.String()
	}
	return snapshot
}

func assertProjectionUnchanged(t *testing.T, name string, before, after projectionSnapshot) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("%s projection tables changed: before=%d after=%d", name, len(before), len(after))
	}
	for table, want := range before {
		if got := after[table]; got != want {
			t.Fatalf("%s partial projection in %s:\nbefore=%s\nafter=%s", name, table, want, got)
		}
	}
}

func countRows(t *testing.T, db *DB, table string) int {
	t.Helper()
	var n int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
