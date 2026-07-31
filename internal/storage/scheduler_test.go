package storage

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNamedSchedulersKeepWakeupsIndependent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	counts := make([]chan struct{}, 3)
	for i := range counts {
		counts[i] = make(chan struct{}, 1)
	}
	type wakeScheduler interface {
		Wake()
		Run(context.Context) error
	}
	schedulers := []wakeScheduler{
		NewIntakeScheduler(func(context.Context) error { counts[0] <- struct{}{}; return nil }),
		NewSupervisorScheduler(func(context.Context) error { counts[1] <- struct{}{}; return nil }),
		NewOutboxScheduler(func(context.Context) error { counts[2] <- struct{}{}; return nil }),
	}
	for _, scheduler := range schedulers {
		go func(s wakeScheduler) { _ = s.Run(ctx) }(scheduler)
	}
	schedulers[0].Wake()
	select {
	case <-counts[0]:
	case <-time.After(time.Second):
		t.Fatal("intake scheduler did not run")
	}
	for i := 1; i < len(counts); i++ {
		select {
		case <-counts[i]:
			t.Fatalf("scheduler %d ran from intake wake", i)
		default:
		}
	}
	schedulers[1].Wake()
	schedulers[2].Wake()
	for i := 1; i < len(counts); i++ {
		select {
		case <-counts[i]:
		case <-time.After(time.Second):
			t.Fatalf("scheduler %d did not run", i)
		}
	}
}

func TestOutboxWakeupConvergesConcurrentCommits(t *testing.T) {
	db, _ := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const total = 16
	claimed := make(chan string, total)
	scheduler := NewOutboxScheduler(func(ctx context.Context) error {
		for {
			claim, err := db.ClaimOutboxOperation(ctx, "race:outbox", testNow, 1000)
			if err != nil {
				return err
			}
			if claim == nil {
				return nil
			}
			claimed <- claim.Key
		}
	})
	db.SetOutboxWakeup(scheduler.Wake)
	go func() { _ = scheduler.Run(ctx) }()
	var wg sync.WaitGroup
	errs := make(chan error, total)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := db.EnqueueOperation(ctx, Operation{Key: "alert:race:" + string(rune('a'+i)) + ":1", Kind: OperationForgeAlert, Payload: []byte(`{}`)}, testNow)
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for i := 0; i < total; i++ {
		select {
		case <-claimed:
		case <-time.After(time.Second):
			t.Fatalf("claimed %d/%d operations after concurrent wakeups", i, total)
		}
	}
}

func TestV8OutboxFairnessUnderHotRun(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	insertConfigSnapshot(t, db, "cfg-v8")
	insertProject(t, db, "project-v8", "cfg-v8")
	for _, runID := range []string{"hot", "peer-b", "peer-c"} {
		insertManualRun(t, db, runID, "project-v8", "cfg-v8")
	}
	for i := 0; i < 12; i++ {
		if _, err := db.EnqueueOperation(ctx, Operation{Key: fmt.Sprintf("v8:hot:%02d", i), Kind: OperationForgeAlert, RunID: "hot", Payload: []byte(`{}`)}, testNow); err != nil {
			t.Fatal(err)
		}
	}
	for i, runID := range []string{"peer-b", "peer-c"} {
		for n := 0; n < 2; n++ {
			if _, err := db.EnqueueOperation(ctx, Operation{Key: fmt.Sprintf("v8:%s:%d", runID, n), Kind: OperationForgeAlert, RunID: runID, Payload: []byte(`{}`)}, testNow+int64(i)+1); err != nil {
				t.Fatal(err)
			}
		}
	}
	var first []string
	for i := 0; i < 6; i++ {
		claim, err := db.ClaimOutboxOperationKind(ctx, fmt.Sprintf("v8-worker-%d", i), OperationForgeAlert, testNow+10, 100)
		if err != nil || claim == nil {
			t.Fatalf("claim %d=%v err=%v", i, claim, err)
		}
		first = append(first, claim.RunID)
		if err := db.CompleteOutboxAttempt(ctx, *claim, CompleteOutcome{State: OperationSucceeded, NowMS: testNow + 10}); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(first, ",") != "hot,peer-b,peer-c,hot,peer-b,peer-c" {
		t.Fatalf("first fairness rounds=%v", first)
	}
}

func TestOutboxCommitWakeupClaimsWithoutPeriodicTick(t *testing.T) {
	db, _ := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	claimed := make(chan *ClaimedOperation, 1)
	scheduler := NewOutboxScheduler(func(ctx context.Context) error {
		claim, err := db.ClaimOutboxOperation(ctx, "test:outbox", testNow, 1000)
		if err != nil {
			return err
		}
		if claim != nil {
			claimed <- claim
		}
		return nil
	})
	db.SetOutboxWakeup(scheduler.Wake)
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()

	if _, err := db.EnqueueOperation(ctx, Operation{Key: "alert:wakeup:subject:1", Kind: OperationForgeAlert, Payload: []byte(`{}`)}, testNow); err != nil {
		t.Fatal(err)
	}
	select {
	case claim := <-claimed:
		if claim.Key != "alert:wakeup:subject:1" {
			t.Fatalf("claimed key = %q", claim.Key)
		}
	case <-time.After(time.Second):
		t.Fatal("outbox was not claimed from the post-commit wakeup")
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("scheduler exit = %v", err)
	}
}
