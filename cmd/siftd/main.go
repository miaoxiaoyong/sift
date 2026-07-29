// Command siftd runs the local Sift control-plane daemon.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/controlplane"
	"github.com/miaoxiaoyong/sift/internal/daemon"
	"github.com/miaoxiaoyong/sift/internal/runtime"
	"github.com/miaoxiaoyong/sift/internal/storage"
	"path/filepath"
)

func main() {
	home, err := config.ResolveHome()
	if err != nil {
		fatal(err)
	}
	snapshot, err := config.Load(home, time.Now())
	if err != nil {
		fatal(err)
	}
	if hasEnabledProjects(snapshot.Config) {
		if _, err := runtime.ResolveInstalledWrapper(controlplane.Version); err != nil {
			fatal(err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	now := time.Now()
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(home.Path, "sift.db"), BinaryVersion: controlplane.Version, Now: now})
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	if err := db.ActivateConfig(ctx, snapshot, controlplane.Version, now.UnixMilli()); err != nil {
		fatal(err)
	}
	termination := &daemon.TerminationCoordinator{
		DB: db, Terminator: runtime.Terminator{Inspector: runtime.UnknownProcessInspector{}, Signaler: runtime.UnixProcessSignaler{}}, Runtime: snapshot.Config.Runtime,
		AttentionDailyQuota: attentionQuota(snapshot.Config.Attention.DailyQuota), DayTimezone: snapshot.Config.Attention.DayTimezone, Now: time.Now,
	}
	// Recovery runs before Assemble starts any worker. Unknown process identity
	// is deliberately fail-closed and becomes a visible startup_stall instead
	// of allowing a launch lease to be reclaimed.
	if err := termination.Recover(ctx); err != nil {
		fatal(err)
	}
	workers, err := daemon.Assemble(db, snapshot.Config, time.Now)
	if err != nil {
		fatal(err)
	}
	s, err := controlplane.Start(home, db)
	if err != nil {
		fatal(err)
	}
	defer s.Close()
	s.SetOperatorAction(func(ctx context.Context, method, runID string, version int64) error {
		return termination.Operator(ctx, runID, version, method == "ops.retry")
	})
	go func() {
		ticker := time.NewTicker(snapshot.Config.Scheduler.SupervisorInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = termination.Timeout(ctx)
				_ = workers.Tick(ctx)
			}
		}
	}()
	if err := s.Serve(ctx); err != nil {
		fatal(err)
	}
}
func hasEnabledProjects(cfg *config.Config) bool {
	for _, project := range cfg.Projects {
		if project.Enabled {
			return true
		}
	}
	return false
}

func attentionQuota(q config.DailyQuota) map[storage.InterruptSeverity]int {
	return map[storage.InterruptSeverity]int{storage.SeverityLow: q.Low, storage.SeverityNormal: q.Normal, storage.SeverityHigh: q.High}
}

func fatal(err error) { fmt.Fprintln(os.Stderr, "siftd:", err); os.Exit(1) }
