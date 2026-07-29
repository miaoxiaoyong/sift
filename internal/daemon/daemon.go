// Package daemon assembles the production siftd workers. Keeping construction
// here makes it difficult for a command entry point to accidentally create a
// Forge adapter without the budget and stable-key policy.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/miaoxiaoyong/sift/internal/brain"
	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/forge"
	"github.com/miaoxiaoyong/sift/internal/forgebudget"
	"github.com/miaoxiaoyong/sift/internal/forgeworker"
	"github.com/miaoxiaoyong/sift/internal/intake"
	"github.com/miaoxiaoyong/sift/internal/launchworker"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

type Daemon struct {
	DB          *storage.DB
	Pollers     []*intake.Poller
	Evaluators  []*intake.T1Evaluator
	Reconcilers []*intake.Reconciler
	Comments    []*forgeworker.CommentWorker
	Changes     []*forgeworker.ChangeWorker
	Replies     []*intake.ReplyConsumer
	Launch      *launchworker.Worker
	Now         func() time.Time
	mu          sync.Mutex
}

// Assemble probes and records auto-merge capability, then creates one
// production Forge adapter, Intake poller/T1 evaluator, reverse-sync
// reconciler, and kind-scoped comment worker per enabled project.
// CommentWorker is also the reply receipt path for intake comments: it lists
// the target before sending and recognizes the durable operation marker, so a
// remotely accepted comment is
// acknowledged after a crash without a second post (covered by forgeworker's
// crash-recovery test). The caller owns DB.
func Assemble(db *storage.DB, cfg *config.Config, now func() time.Time) (*Daemon, error) {
	return assemble(db, cfg, now, nil, forge.NewProductionAdapter)
}

// AssembleWithRunner is the fixture-injection seam for daemon integration
// tests. Production callers should use Assemble, which executes the configured
// Forge CLI.
func AssembleWithRunner(db *storage.DB, cfg *config.Config, now func() time.Time, runner forge.Runner) (*Daemon, error) {
	return assemble(db, cfg, now, runner, forge.NewProductionAdapter)
}

func assemble(db *storage.DB, cfg *config.Config, now func() time.Time, runner forge.Runner, newAdapter func(forge.Kind, string, forge.Runner, forge.Charger) (*forge.Adapter, error)) (*Daemon, error) {
	if db == nil || cfg == nil {
		return nil, errors.New("daemon: database and config are required")
	}
	if now == nil {
		now = time.Now
	}
	d := &Daemon{DB: db, Now: now}
	for _, p := range cfg.Projects {
		if !p.Enabled {
			continue
		}
		ref := forge.ProjectRef{Kind: forge.Kind(p.Forge.Kind), Host: p.Forge.Host, ProjectKey: p.Forge.Project}
		charger := &forgebudget.Charger{DB: db, Limit: int64(cfg.Forge.HourlyAPILimit), WarningRatio: cfg.Forge.WarningRatio, Now: now}
		adapter, err := newAdapter(ref.Kind, p.Forge.CLI, runner, charger)
		if err != nil {
			return nil, fmt.Errorf("project %s: %w", p.ID, err)
		}
		adapter.WithAutoMergeCapabilityReader(db)
		probeCtx := context.Background()
		if cfg.Forge.CommandTimeout > 0 {
			var cancel context.CancelFunc
			probeCtx, cancel = context.WithTimeout(probeCtx, cfg.Forge.CommandTimeout)
			defer cancel()
		}
		if err := adapter.ProbeAndRecordAutoMergeCapability(probeCtx, p.ID, ref, db, now()); err != nil {
			return nil, fmt.Errorf("project %s: record auto-merge capability: %w", p.ID, err)
		}
		project := intake.Project{ID: p.ID, Ref: ref, TriggerLabel: cfg.Labels.Trigger, OperatorAllowlist: operators(cfg.Operators, ref.Kind)}
		evaluator := &intake.T1Evaluator{DB: db, Brain: brain.NewShell(db, cfg.Brain, brain.SubprocessProvider{Executable: cfg.Brain.Executable, Args: cfg.Brain.Args}, now), Now: now}
		poller := &intake.Poller{DB: db, Forge: adapter, Projects: []intake.Project{project}, Now: now, Idle: cfg.Scheduler.IntakeIdleInterval, Active: cfg.Scheduler.IntakeActiveInterval, Slow: cfg.Forge.SlowPollInterval, HourlyLimit: int64(cfg.Forge.HourlyAPILimit), WarningRatio: cfg.Forge.WarningRatio, OnIssue: evaluator.EvaluateIssue}
		d.Pollers = append(d.Pollers, poller)
		d.Evaluators = append(d.Evaluators, evaluator)
		d.Reconcilers = append(d.Reconcilers, &intake.Reconciler{DB: db, Forge: adapter, Projects: []intake.Project{project}, Now: now})
		d.Comments = append(d.Comments, &forgeworker.CommentWorker{DB: db, Client: adapter, ProjectID: p.ID, Now: now, Lease: cfg.Outbox.LeaseTTL, WorkerID: "siftd:comment:" + p.ID})
		d.Changes = append(d.Changes, &forgeworker.ChangeWorker{DB: db, Client: adapter, ProjectID: p.ID, Now: now, Lease: cfg.Outbox.LeaseTTL, WorkerID: "siftd:change:" + p.ID})
		d.Replies = append(d.Replies, &intake.ReplyConsumer{DB: db, Forge: adapter, Projects: []intake.Project{project}, Now: now})
	}
	return d, nil
}

// SetLaunchWorker installs the sole launch_agent consumer after startup
// recovery has produced this daemon's boot ID.
func (d *Daemon) SetLaunchWorker(w *launchworker.Worker) { d.Launch = w }

func operators(o config.Operators, k forge.Kind) []string {
	if k == forge.KindGitLab {
		return append([]string(nil), o.GitLab...)
	}
	return append([]string(nil), o.GitHub...)
}

// Tick advances Intake, reverse-sync reconciliation, and then one comment
// operation per project. A project failure is returned with its identity; the
// scheduler may continue other projects on the next tick.
func (d *Daemon) Tick(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, p := range d.Pollers {
		if err := p.PollOnce(ctx); err != nil {
			return fmt.Errorf("intake[%d]: %w", i, err)
		}
	}
	for i, r := range d.Reconcilers {
		if err := r.ReconcileOnce(ctx); err != nil {
			return fmt.Errorf("reconciler[%d]: %w", i, err)
		}
	}
	for i, r := range d.Replies {
		if err := r.RunOnce(ctx); err != nil {
			return fmt.Errorf("reply[%d]: %w", i, err)
		}
	}
	for i, w := range d.Comments {
		if err := w.RunOnce(ctx); err != nil {
			return fmt.Errorf("comment[%d]: %w", i, err)
		}
	}
	for i, w := range d.Changes {
		if err := w.RunOnce(ctx); err != nil {
			return fmt.Errorf("change[%d]: %w", i, err)
		}
	}
	if d.Launch != nil {
		if err := d.Launch.RunOnce(ctx); err != nil {
			return fmt.Errorf("launch_agent: %w", err)
		}
	}
	return nil
}
