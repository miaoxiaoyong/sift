package storage

import (
	"context"
	"testing"

	"github.com/xsift/sift/internal/command"
)

// ResolveCommandAckRouting must return the immutable target and project forge
// ref for a command ack produced by the real write port. The worker never
// reconstructs this from current Run/Change/Interrupt state; the append-only
// receipt is the single source (command.md §6.1).
func TestResolveCommandAckRoutingFromAppliedCommand(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	_, nonce := emitDesignApprovalInterrupt(t, db, ctx, command.CommandTarget{Kind: command.TargetIssue, ID: "42"})

	body := "/sift reject " + cmdRun + " " + nonce + " too risky"
	env := commentEnv(t, "project", "c1", body)
	if _, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5}); err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}

	route, err := db.ResolveCommandAckRouting(ctx, env.EventKey)
	if err != nil {
		t.Fatalf("ResolveCommandAckRouting: %v", err)
	}
	if route.ProjectID != "project" || route.ForgeKind != "github" || route.ForgeHost != "github.com" || route.ForgeProjectKey != "org/repo-project" {
		t.Fatalf("forge routing mismatch: %+v", route)
	}
	if route.TargetKind != "issue" || route.TargetID != "42" {
		t.Fatalf("target mismatch: %+v", route)
	}
}

// A rejected_syntax candidate has no resolved Interrupt, but its receipt still
// pins the immutable envelope target. Routing must resolve from the receipt
// alone, proving the worker can acknowledge rejected commands.
func TestResolveCommandAckRoutingRejectedSyntax(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	emitDesignApprovalInterrupt(t, db, ctx, command.CommandTarget{Kind: command.TargetIssue, ID: "42"})

	env := commentEnv(t, "project", "c1", "/sift bogus "+cmdRun)
	if _, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5}); err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}

	route, err := db.ResolveCommandAckRouting(ctx, env.EventKey)
	if err != nil {
		t.Fatalf("ResolveCommandAckRouting: %v", err)
	}
	if route.TargetKind != "issue" || route.TargetID != "42" || route.ProjectID != "project" {
		t.Fatalf("routing must resolve from receipt alone: %+v", route)
	}
}

// An ack whose receipt is absent must fail closed rather than post to an
// unproven target.
func TestResolveCommandAckRoutingMissingFailsClosed(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if _, err := db.ResolveCommandAckRouting(ctx, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"); err != ErrMissingCommandAckRoute {
		t.Fatalf("err=%v, want ErrMissingCommandAckRoute", err)
	}
}
