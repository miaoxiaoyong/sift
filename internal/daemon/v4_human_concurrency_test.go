package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/xsift/sift/internal/config"
	"github.com/xsift/sift/internal/storage"
)

// TestV4FourDiscoverersConverge exercises the production coordinator entry
// points, rather than calling EmitInterrupt directly. All four callers observe
// one ambiguous stall at the same time; the first durable CAS wins and the
// remaining callers must replay that projection without creating a second
// owner, interrupt, charge, or publication.
func TestV4FourDiscoverersConverge(t *testing.T) {
	for _, backend := range []string{"process", "tmux"} {
		t.Run(backend, func(t *testing.T) {
			db, raw, _, now := seedRecoveryCoordinator(t, "running", nowForV4().Add(-2*time.Second).UnixMilli())
			if _, err := raw.Exec(`UPDATE attempts SET backend=? WHERE run_id='run'`, backend); err != nil {
				t.Fatal(err)
			}
			clearRecoveryWrapper(t, raw)
			c := &TerminationCoordinator{
				DB:                  db,
				Runtime:             config.Runtime{HeartbeatStaleAfter: time.Second},
				AttentionDailyQuota: recoveryQuota(),
				Now:                 func() time.Time { return now },
			}
			ctx := context.Background()
			start := make(chan struct{})
			errs := make(chan error, 4)
			var wg sync.WaitGroup
			call := func(fn func() error) {
				wg.Add(1)
				go func() { defer wg.Done(); <-start; errs <- fn() }()
			}
			call(func() error { return c.Recover(ctx) })
			call(func() error { return c.Timeout(ctx) })
			call(func() error { return c.Operator(ctx, "run", 1, false) })
			call(func() error { return c.Operator(ctx, "run", 1, true) })
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil && err != storage.ErrRejectedStale {
					t.Fatalf("discoverer error=%v", err)
				}
			}

			assertSingleFrozenStartupStall(t, raw, "", "process_identity_unknown")
			var attempts, claims, launches, publications int
			if err := raw.QueryRow(`SELECT count(*) FROM attempts WHERE run_id='run'`).Scan(&attempts); err != nil {
				t.Fatal(err)
			}
			if err := raw.QueryRow(`SELECT count(*) FROM attempt_claims WHERE run_id='run'`).Scan(&claims); err != nil {
				t.Fatal(err)
			}
			if err := raw.QueryRow(`SELECT count(*) FROM outbox_operations WHERE run_id='run'`).Scan(&launches); err != nil {
				t.Fatal(err)
			}
			if err := raw.QueryRow(`SELECT count(*) FROM outbox_operations WHERE run_id='run' AND kind='forge_comment'`).Scan(&publications); err != nil {
				t.Fatal(err)
			}
			if attempts != 1 || claims != 1 || launches != 1 || publications != 1 {
				t.Fatalf("projection attempts/claims/launches/publications=%d/%d/%d/%d", attempts, claims, launches, publications)
			}
		})
	}
}

func nowForV4() time.Time { return time.UnixMilli(10_000) }
