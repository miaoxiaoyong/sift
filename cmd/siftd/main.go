// Command siftd runs the local Sift control-plane daemon.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/miaoxiaoyong/sift/internal/brain"
	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/controlplane"
	"github.com/miaoxiaoyong/sift/internal/daemon"
	"github.com/miaoxiaoyong/sift/internal/launchworker"
	"github.com/miaoxiaoyong/sift/internal/runtime"
	"github.com/miaoxiaoyong/sift/internal/storage"
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
	tmuxDiagnostics := config.NewDiagnostics()
	if err := config.TmuxProbe(snapshot.Config, tmuxDiagnostics).Run(context.Background()); err != nil {
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
	// All production Interrupt sources, including startup recovery, share this
	// single Brain shell. T4 and T6 both run outside EmitInterrupt's write
	// transaction; the deterministic acceptor keeps final severity/dispatch.
	shell := brain.NewShell(db, snapshot.Config.Brain, brain.SubprocessProvider{Executable: snapshot.Config.Brain.Executable, Args: snapshot.Config.Brain.Args}, time.Now)
	db.SetInterruptT4(shell.CallT4)
	db.SetInterruptT6(shell.CallT6)
	bootID, err := db.StartDaemonBoot(ctx, snapshot.Hash, controlplane.Version, controlplane.ProtocolMajor, os.Getpid(), now.UnixMilli())
	if err != nil {
		fatal(err)
	}
	termination := &daemon.TerminationCoordinator{
		DB: db, Terminator: runtime.Terminator{Inspector: runtime.PlatformProcessInspector{}, Signaler: runtime.UnixProcessSignaler{}}, Runtime: snapshot.Config.Runtime,
		ControlRoot: home.Path,
		ProcessGroupQualified: func(key string) bool {
			ok, err := db.ProcessGroupQualified(ctx, key)
			return err == nil && ok
		},
		AttentionDailyQuota: attentionQuota(snapshot.Config.Attention.DailyQuota), DayTimezone: snapshot.Config.Attention.DayTimezone, DailySummaryAt: snapshot.Config.Attention.DailySummaryAt, CriticalWindowMS: snapshot.Config.Attention.CriticalFuse.Window.Milliseconds(), CriticalTotalLimit: snapshot.Config.Attention.CriticalFuse.TotalLimit, CriticalPerRunLimit: snapshot.Config.Attention.CriticalFuse.PerRunLimit, Channels: interruptChannels(snapshot.Config.Attention), Now: time.Now,
	}
	// startup_stall retry probe process-check shares the same process inspector
	// and control root as termination. It runs on the supervisor tick and drives
	// pending|running probes to the unique ApplyRetryProbeResult finalizer; it is
	// not an outbox worker and never signals a process (specs/storage.md §5.5).
	probeCheck := &daemon.ProbeProcessCheckCoordinator{
		DB: db, Inspector: runtime.PlatformProcessInspector{}, Runtime: snapshot.Config.Runtime, ControlRoot: home.Path, Now: time.Now,
	}
	// Recovery runs before Assemble starts any worker. Incomplete process
	// evidence deliberately fails closed and becomes a visible startup_stall
	// instead of allowing a launch lease to be reclaimed.
	if err := termination.RecoverStartup(ctx, bootID); err != nil {
		fatal(err)
	}
	if err := db.CompleteStartupRecovery(ctx, bootID, time.Now().UnixMilli()); err != nil {
		fatal(err)
	}
	workers, err := daemon.Assemble(db, snapshot.Config, time.Now)
	if err != nil {
		fatal(err)
	}
	workers.SetT7Scheduler(&brain.T7Scheduler{DB: db, Shell: shell, Now: time.Now, Limit: snapshot.Config.Scheduler.PerClassTickLimit})
	daemonPath, err := os.Executable()
	if err != nil {
		fatal(err)
	}
	backend, err := runtime.NewProcessBackend(daemonPath, controlplane.Version)
	if err != nil {
		fatal(err)
	}
	backends := launchworker.BackendRouter{
		config.BackendProcess: launchworker.ProcessBackend{Backend: backend},
	}
	if usesTmux(snapshot.Config) {
		// This is the process-level tmux startup probe. Process-only
		// configurations intentionally never resolve or require tmux.
		verifyBinding := func(ctx context.Context, launch runtime.HostLaunch) error {
			return db.VerifyLaunchBinding(ctx, launch.OperationID, launch.LeaseOwner, launch.LeaseExpiresAtMS, launch.RunID, launch.AttemptNo, launch.Generation, launch.DispatchID, launch.Backend, time.Now().UnixMilli())
		}
		tmux, backendErr := runtime.NewTmuxBackend(tmuxDiagnostics.TmuxPath, backend.WrapperPath(), filepath.Join(home.Path, "tmux.sock"), verifyBinding)
		if backendErr != nil {
			fatal(backendErr)
		}
		backends[config.BackendTmux] = launchworker.TmuxBackend{Backend: tmux}
	}
	workers.SetLaunchWorker(&launchworker.Worker{
		DB: db, BootID: bootID, WorkerID: "siftd:launch_agent", Root: home.Path,
		Lease: snapshot.Config.Runtime.SpawnOperationLeaseTTL, Now: time.Now,
		Backends: backends, Agents: snapshot.Config.Agents,
	})
	s, err := controlplane.Start(home, db)
	if err != nil {
		fatal(err)
	}
	defer s.Close()
	s.SetOperatorAction(func(ctx context.Context, method, runID string, version int64) error {
		return termination.Operator(ctx, runID, version, method == "ops.retry")
	})
	s.SetAttentionQuota(attentionQuotaStrings(snapshot.Config.Attention.DailyQuota))
	if usesTmux(snapshot.Config) {
		s.SetTmuxObserver(tmuxDiagnostics.TmuxPath, runtime.TmuxSocketPath(filepath.Join(home.Path, "tmux.sock")))
	}
	if err := startSchedulers(ctx, db, workers, termination, probeCheck, snapshot.Config.Scheduler); err != nil {
		fatal(err)
	}
	defer db.SetOutboxWakeup(nil)
	if err := s.Serve(ctx); err != nil {
		fatal(err)
	}
}
func usesTmux(cfg *config.Config) bool {
	if cfg.Runtime.Backend == config.BackendTmux {
		return true
	}
	for _, agent := range cfg.Agents {
		if agent.Backend == config.BackendTmux {
			return true
		}
	}
	return false
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

// attentionQuotaStrings is the string-keyed form the control-plane server uses
// for its attention_remaining projection (low/normal/high).
func attentionQuotaStrings(q config.DailyQuota) map[string]int {
	return map[string]int{"low": q.Low, "normal": q.Normal, "high": q.High}
}

// startSchedulers is the sole owner of siftd's three DESIGN §6.1 clocks.
// Intake's cursor still determines whether a poll is due; the supervisor
// interval also bounds recovery of persisted outbox retry deadlines.
func interruptChannels(attention config.Attention) []storage.InterruptChannel {
	channels := make([]storage.InterruptChannel, 0, len(attention.Channels))
	for _, c := range attention.Channels {
		if !c.Enabled {
			continue
		}
		channels = append(channels, storage.InterruptChannel{ID: c.ID, Type: c.Type, TargetRef: c.TargetRef, Capabilities: append([]string(nil), c.Capabilities...), Renderer: c.Renderer, Default: c.Default})
	}
	return channels
}

func startSchedulers(ctx context.Context, db *storage.DB, workers *daemon.Daemon, termination *daemon.TerminationCoordinator, probeCheck *daemon.ProbeProcessCheckCoordinator, cfg config.Scheduler) error {
	return startSchedulersWithFactory(ctx, db, workers, termination, probeCheck, cfg, productionSchedulerFactory{}, schedulerHooks{})
}

type wakeScheduler interface {
	Wake()
	WakeAndWait(context.Context) error
	Run(context.Context) error
}

type schedulerFactory interface {
	Intake(func(context.Context) error) wakeScheduler
	Supervisor(func(context.Context) error) wakeScheduler
	Outbox(func(context.Context) error) wakeScheduler
}

type productionSchedulerFactory struct{}

func (productionSchedulerFactory) Intake(run func(context.Context) error) wakeScheduler {
	return storage.NewIntakeScheduler(run)
}
func (productionSchedulerFactory) Supervisor(run func(context.Context) error) wakeScheduler {
	return storage.NewSupervisorScheduler(run)
}
func (productionSchedulerFactory) Outbox(run func(context.Context) error) wakeScheduler {
	return storage.NewOutboxScheduler(run)
}

// schedulerHooks make the production wiring observable in bounded tests. They
// are deliberately invoked at the same call sites as the real daemon methods.
type schedulerHooks struct {
	Intake, Supervisor, Outbox func()
}

func startSchedulersWithFactory(ctx context.Context, db *storage.DB, workers *daemon.Daemon, termination *daemon.TerminationCoordinator, probeCheck *daemon.ProbeProcessCheckCoordinator, cfg config.Scheduler, factory schedulerFactory, hooks schedulerHooks) error {
	intake := factory.Intake(reportSchedulerError("intake", func(ctx context.Context) error {
		if hooks.Intake != nil {
			hooks.Intake()
		}
		return workers.IntakeTick(ctx)
	}))
	supervisor := factory.Supervisor(reportSchedulerError("supervisor", func(ctx context.Context) error {
		if hooks.Supervisor != nil {
			hooks.Supervisor()
		}
		if err := termination.Timeout(ctx); err != nil {
			return err
		}
		// startup_stall retry probes are driven on the supervisor domain, never
		// the outbox: process observation and idempotent finalization stay out
		// of the outbox and resume from pending|running after a crash. Probes
		// finalize before SupervisorInterruptTick so a failed probe's reverted
		// batched state can be escalated in the same tick.
		if probeCheck != nil {
			if err := probeCheck.Tick(ctx); err != nil {
				return err
			}
		}
		if err := workers.T7Tick(ctx); err != nil {
			return err
		}
		return db.SupervisorInterruptTick(ctx, time.Now().UnixMilli())
	}))
	outbox := factory.Outbox(reportSchedulerError("outbox", func(ctx context.Context) error {
		if hooks.Outbox != nil {
			hooks.Outbox()
		}
		return workers.OutboxTick(ctx)
	}))
	db.SetOutboxWakeup(outbox.Wake)

	go runScheduler(ctx, intake, minIntakeInterval(cfg))
	go runScheduler(ctx, supervisor, cfg.SupervisorInterval)
	if err := startScheduler(ctx, outbox, cfg.SupervisorInterval); err != nil {
		return fmt.Errorf("outbox startup recovery: %w", err)
	}
	return nil
}

// startScheduler waits for the first outbox sweep before returning, so a later
// commit wakeup cannot be mistaken for a delayed startup wake.
func startScheduler(ctx context.Context, scheduler wakeScheduler, interval time.Duration) error {
	go func() { _ = scheduler.Run(ctx) }()
	if err := scheduler.WakeAndWait(ctx); err != nil {
		return err
	}
	go runSchedulerClock(ctx, scheduler, interval)
	return nil
}

func runScheduler(ctx context.Context, scheduler wakeScheduler, interval time.Duration) {
	go func() { _ = scheduler.Run(ctx) }()
	scheduler.Wake()
	runSchedulerClock(ctx, scheduler, interval)
}

func runSchedulerClock(ctx context.Context, scheduler wakeScheduler, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			scheduler.Wake()
		}
	}
}

func minIntakeInterval(cfg config.Scheduler) time.Duration {
	interval := cfg.IntakeIdleInterval
	for _, candidate := range []time.Duration{cfg.IntakeActiveInterval, cfg.IntakeInterruptInterval} {
		if candidate > 0 && (interval == 0 || candidate < interval) {
			interval = candidate
		}
	}
	return interval
}

func reportSchedulerError(name string, run func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := run(ctx); err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "siftd: %s scheduler: %v\n", name, err)
		}
		return nil
	}
}

func fatal(err error) { fmt.Fprintln(os.Stderr, "siftd:", err); os.Exit(1) }
