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
			before := projectionCounts(t, db, []string{"runs", "budget_entries", "budget_counters", "interrupts", "attention_admissions", "interrupt_command_effect_bindings", "events", "outbox_operations", "interrupt_deliveries", "interrupt_command_targets"})
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
			after := projectionCounts(t, db, before.names())
			if got, want := after, before; !equalCounts(got, want) {
				t.Fatalf("partial interrupt projection: before=%v after=%v", want, got)
			}
		})
	}
}

// TestV2RetryProbeSuccessCrashMatrix is deliberately a write-point matrix,
// rather than a single trigger at the last write.  It protects the ADR-013
// all-or-nothing successor handoff (including the durable ack).
func TestV2RetryProbeSuccessCrashMatrix(t *testing.T) {
	families := []struct{ name, table, event string }{
		{"probe", "attempt_probes", "UPDATE"},
		{"old attempt resolution", "attempts", "UPDATE"},
		{"interrupt close", "interrupts", "UPDATE"},
		{"run queued event", "events", "INSERT"},
		{"final outcome", "command_event_outcomes", "UPDATE"},
		{"isolation release", "attempts", "UPDATE"},
		{"successor attempt", "attempts", "INSERT"},
		{"successor claim", "attempt_claims", "INSERT"},
		{"launch operation", "outbox_operations", "INSERT"},
		{"ack operation", "outbox_operations", "INSERT"},
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
			trigger := "v2_probe_crash"
			condition := ""
			if family.table == "attempts" && family.event == "INSERT" {
				condition = " WHEN NEW.attempt_no=2"
			}
			if family.table == "attempts" && family.event == "UPDATE" {
				condition = " WHEN NEW.attempt_no=1"
			}
			if family.name == "launch operation" {
				condition = " WHEN NEW.kind='launch_agent'"
			}
			if family.name == "ack operation" {
				condition = " WHEN NEW.kind='command_ack'"
			}
			mustExec(t, db, fmt.Sprintf("CREATE TRIGGER %s BEFORE %s ON %s%s BEGIN SELECT RAISE(ABORT, 'injected crash'); END", trigger, family.event, family.table, condition))
			_, err := db.ApplyRetryProbeResult(ctx, RetryProbeResultCmd{InterruptID: interruptID, ProbeID: probeID, Succeeded: true, AbsenceEvidenceJSON: json.RawMessage(`{"absent":true}`), NowMS: testNow + 10})
			if err == nil || !strings.Contains(err.Error(), "injected crash") {
				t.Fatalf("ApplyRetryProbeResult error = %v", err)
			}
			if got := countRows(t, db, "attempts WHERE run_id='"+cmdRun+"' AND attempt_no=2"); got != 0 {
				t.Fatalf("successor leaked after %s crash: %d", family.name, got)
			}
			if status := runStatus(t, db); status != "waiting_human" {
				t.Fatalf("run status after %s crash = %s", family.name, status)
			}
		})
	}
}

type projectionSnapshot map[string]int

func (p projectionSnapshot) names() []string {
	names := make([]string, 0, len(p))
	for name := range p {
		names = append(names, name)
	}
	return names
}

func projectionCounts(t *testing.T, db *DB, names []string) projectionSnapshot {
	t.Helper()
	p := projectionSnapshot{}
	for _, name := range names {
		p[name] = countRows(t, db, name)
	}
	return p
}

func equalCounts(a, b projectionSnapshot) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func countRows(t *testing.T, db *DB, table string) int {
	t.Helper()
	var n int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
