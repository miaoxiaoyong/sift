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
	"github.com/miaoxiaoyong/sift/internal/channelworker"
	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/forge"
	"github.com/miaoxiaoyong/sift/internal/forgeworker"
	"github.com/miaoxiaoyong/sift/internal/gate"
	"github.com/miaoxiaoyong/sift/internal/hooks"
	"github.com/miaoxiaoyong/sift/internal/intake"
	"github.com/miaoxiaoyong/sift/internal/launchworker"
	"github.com/miaoxiaoyong/sift/internal/storage"
	"github.com/miaoxiaoyong/sift/internal/worktree"
)

type Daemon struct {
	DB                *storage.DB
	Pollers           []*intake.Poller
	Evaluators        []*intake.T1Evaluator
	Reconcilers       []*intake.Reconciler
	Comments          []*forgeworker.CommentWorker
	Changes           []*forgeworker.ChangeWorker
	Merges            []*forgeworker.MergeWorker
	Channels          []*channelworker.Worker
	Alerts            *forgeworker.AlertWorker
	CommandAcks       *forgeworker.CommandAckWorker
	GateReEvaluations *forgeworker.GateReEvaluationWorker
	RerunChecks       *forgeworker.RerunChecksWorker
	Successes         []*gate.SuccessReconciler
	Gates             []*gate.Reconciler
	Replies           []*intake.ReplyConsumer
	Launch            *launchworker.Worker
	T7                *brain.T7Scheduler
	Now               func() time.Time
	intakeMu          sync.Mutex
	outboxMu          sync.Mutex
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

// CaptureHookBaselines is deliberately audit-only: an unreadable hooks path
// records a diagnostic but never blocks activation or a future launch gate.
func CaptureHookBaselines(ctx context.Context, db *storage.DB, cfg *config.Config, now func() time.Time) {
	if db == nil || cfg == nil {
		return
	}
	if now == nil {
		now = time.Now
	}
	for _, p := range cfg.Projects {
		if !p.Enabled || p.Repo == "" {
			continue
		}
		snapshot, err := hooks.Capture(ctx, p.Repo)
		if err != nil {
			_ = db.RecordHookDiagnostic(ctx, p.ID, "", 0, "hooks_capture_failed", err.Error(), now().UnixMilli())
			continue
		}
		if err := db.RecordHookBaseline(ctx, storage.RecordHookBaselineCmd{ProjectID: p.ID, Snapshot: storage.HookBaselineSnapshot{GitConfigDigest: snapshot.GitConfigDigest, CoreHooksPathValue: snapshot.CoreHooksPathValue, EffectiveHooksPath: snapshot.EffectiveHooksPath, HooksDirectoryDigest: snapshot.DirectoryDigest, Digest: snapshot.Digest}, CapturedAtMS: now().UnixMilli()}); err != nil {
			_ = db.RecordHookDiagnostic(ctx, p.ID, "", 0, "hooks_baseline_record_failed", err.Error(), now().UnixMilli())
		}
	}
}

// BootstrapHookBaseline is the explicit, authenticated upgrade path for a
// project that has terminal Agent history but predates persisted baselines.
// Unlike activation capture, it may trust the observed state only because the
// operator deliberately invoked it; it never runs from an Agent completion.
func BootstrapHookBaseline(ctx context.Context, db *storage.DB, cfg *config.Config, projectID string, now func() time.Time) error {
	if db == nil || cfg == nil || projectID == "" {
		return errors.New("daemon: hook bootstrap requires database, config, and project")
	}
	if now == nil {
		now = time.Now
	}
	for _, p := range cfg.Projects {
		if p.ID != projectID {
			continue
		}
		if !p.Enabled || p.Repo == "" {
			return errors.New("daemon: hook bootstrap project is unavailable")
		}
		snapshot, err := hooks.Capture(ctx, p.Repo)
		if err != nil {
			_ = db.RecordHookDiagnostic(ctx, p.ID, "", 0, "hooks_capture_failed", err.Error(), now().UnixMilli())
			return err
		}
		return db.RecordHookBaseline(ctx, storage.RecordHookBaselineCmd{ProjectID: p.ID, Snapshot: storage.HookBaselineSnapshot{GitConfigDigest: snapshot.GitConfigDigest, CoreHooksPathValue: snapshot.CoreHooksPathValue, EffectiveHooksPath: snapshot.EffectiveHooksPath, HooksDirectoryDigest: snapshot.DirectoryDigest, Digest: snapshot.Digest}, TrustedBootstrap: true, CapturedAtMS: now().UnixMilli()})
	}
	return errors.New("daemon: hook bootstrap project is unknown")
}

// HookRechecker captures only after a terminal receipt exists. Every outcome
// completes that receipt, so bad local hook configuration remains visible but
// cannot stall attempt lifecycle or retry the Agent.
func HookRechecker(db *storage.DB, now func() time.Time) func(context.Context, string, int) error {
	return func(ctx context.Context, runID string, attemptNo int) error {
		if db == nil {
			return nil
		}
		at := time.Now
		if now != nil {
			at = now
		}
		projectID, repo, err := db.HookProjectForRun(ctx, runID)
		if err != nil {
			return nil
		}
		observed, err := hooks.Capture(ctx, repo)
		if err != nil {
			_ = db.RecordHookDiagnostic(ctx, projectID, runID, attemptNo, "hooks_capture_failed", err.Error(), at().UnixMilli())
			_ = db.CompleteHookRecheck(ctx, runID, attemptNo, at().UnixMilli())
			return nil
		}
		source := attemptNo
		if err := db.RecordHookBaseline(ctx, storage.RecordHookBaselineCmd{ProjectID: projectID, Snapshot: storage.HookBaselineSnapshot{GitConfigDigest: observed.GitConfigDigest, CoreHooksPathValue: observed.CoreHooksPathValue, EffectiveHooksPath: observed.EffectiveHooksPath, HooksDirectoryDigest: observed.DirectoryDigest, Digest: observed.Digest}, SourceRunID: runID, SourceAttemptNo: &source, CapturedAtMS: at().UnixMilli()}); err != nil {
			_ = db.RecordHookDiagnostic(ctx, projectID, runID, attemptNo, "hooks_baseline_record_failed", err.Error(), at().UnixMilli())
		}
		_ = db.CompleteHookRecheck(ctx, runID, attemptNo, at().UnixMilli())
		return nil
	}
}

func assemble(db *storage.DB, cfg *config.Config, now func() time.Time, runner forge.Runner, newAdapter func(forge.Kind, string, forge.Runner, forge.Charger) (*forge.Adapter, error)) (*Daemon, error) {
	if db == nil || cfg == nil {
		return nil, errors.New("daemon: database and config are required")
	}
	if now == nil {
		now = time.Now
	}
	d := &Daemon{DB: db, Now: now}
	// One per-project adapter map is shared by the cross-project consumers
	// (forge_alert and command_ack): their payloads do not carry project
	// routing, so each worker resolves the frozen target and selects the
	// matching adapter by forge_kind|host|project_key.
	forgeClients := make(map[string]forge.Client)
	rerunClients := make(map[string]forgeworker.RerunCheckClient)
	db.SetChannelPolicy(cfg.Attention.ChannelFailureAlertAfter, cfg.Outbox.MaxAttempts)
	db.SetGateReEvalInterruptEmission(gateReEvalInterruptEmission(cfg.Attention, interruptChannels(cfg.Attention)))
	// Channel payloads are already sealed by storage. The production consumer
	// owns the only resolver and HTTP side effect; it is not project-scoped.
	d.AddChannelWorker(&channelworker.Worker{
		DB: db, Adapter: channelworker.WebhookAdapter{Resolver: channelworker.EnvironmentSecretResolver{}, Sender: channelworker.HTTPWebhookSender{}},
		Now: func() int64 { return now().UnixMilli() }, LeaseMS: cfg.Outbox.LeaseTTL.Milliseconds(), WorkerID: "siftd:channel", AlertAfter: cfg.Attention.ChannelFailureAlertAfter,
		Backoff: storage.BackoffPolicy{InitialDelayMS: cfg.Outbox.RetryInitialDelay.Milliseconds(), MaxDelayMS: cfg.Outbox.RetryMaxDelay.Milliseconds(), Multiplier: cfg.Outbox.RetryMultiplier}, MaxAttempts: cfg.Outbox.MaxAttempts,
	})
	for _, p := range cfg.Projects {
		if !p.Enabled {
			continue
		}
		ref := forge.ProjectRef{Kind: forge.Kind(p.Forge.Kind), Host: p.Forge.Host, ProjectKey: p.Forge.Project}
		charger := &forgeBudgetCharger{DB: db, Limit: int64(cfg.Forge.HourlyAPILimit), WarningRatio: cfg.Forge.WarningRatio, Now: now}
		adapter, err := newAdapter(ref.Kind, p.Forge.CLI, runner, charger)
		if err != nil {
			return nil, fmt.Errorf("project %s: %w", p.ID, err)
		}
		adapter.WithAutoMergeCapabilityReader(db)
		clientKey := string(ref.Kind) + "|" + ref.Host + "|" + ref.ProjectKey
		forgeClients[clientKey] = adapter
		rerunClients[clientKey] = adapter
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
		d.Reconcilers = append(d.Reconcilers, &intake.Reconciler{DB: db, Forge: adapter, Projects: []intake.Project{project}, Now: now, Certification: cfg.Certification})
		d.Comments = append(d.Comments, &forgeworker.CommentWorker{DB: db, Client: adapter, ProjectID: p.ID, Now: now, Lease: cfg.Outbox.LeaseTTL, WorkerID: "siftd:comment:" + p.ID})
		d.Changes = append(d.Changes, &forgeworker.ChangeWorker{DB: db, Client: adapter, ProjectID: p.ID, Now: now, Lease: cfg.Outbox.LeaseTTL, WorkerID: "siftd:change:" + p.ID})
		d.Merges = append(d.Merges, &forgeworker.MergeWorker{DB: db, Client: adapter, ProjectID: p.ID, Now: now, Lease: cfg.Outbox.LeaseTTL, WorkerID: "siftd:merge:" + p.ID})
		if p.Repo != "" {
			worktrees, err := worktree.NewManager(p.Repo, p.Repo+"/.sift-worktrees")
			if err != nil {
				return nil, fmt.Errorf("project %s: gate success worktree manager: %w", p.ID, err)
			}
			d.Successes = append(d.Successes, &gate.SuccessReconciler{DB: db, ProjectID: p.ID, Worktrees: worktrees, Now: now})
			d.Gates = append(d.Gates, &gate.Reconciler{DB: db, Forge: adapter, Brain: brain.NewShell(db, cfg.Brain, brain.SubprocessProvider{Executable: cfg.Brain.Executable, Args: cfg.Brain.Args}, now), ProjectID: p.ID, Project: ref, Repo: p.Repo, Defaults: cfg.GateDefaults, Certification: cfg.Certification, Attention: cfg.Attention, Channels: interruptChannels(cfg.Attention), Now: now})
		}
		d.Replies = append(d.Replies, &intake.ReplyConsumer{DB: db, Forge: adapter, Projects: []intake.Project{project}, Now: now})
	}
	if len(forgeClients) > 0 {
		d.Alerts = &forgeworker.AlertWorker{DB: db, Clients: forgeClients, Now: now, Lease: cfg.Outbox.LeaseTTL, WorkerID: "siftd:forge-alert"}
		// command_ack shares the per-project adapter map. Its operation payload
		// (CommandAckV1) carries no forge routing; the worker resolves the
		// immutable target from the append-only command receipt.
		d.CommandAcks = &forgeworker.CommandAckWorker{DB: db, Clients: forgeClients, Now: now, Lease: cfg.Outbox.LeaseTTL, WorkerID: "siftd:command_ack"}
		d.RerunChecks = &forgeworker.RerunChecksWorker{DB: db, Clients: rerunClients, Now: now, Lease: cfg.Outbox.LeaseTTL, WorkerID: "siftd:rerun_checks"}
	}
	// gate_re_evaluation resolves the run's project, then delegates Forge/Brain
	// assembly to the matching Gate reconciler (storage.md §8.1). It is wired
	// after the per-project reconcilers are known.
	projectReconcilers := map[string]*gate.Reconciler{}
	for _, g := range d.Gates {
		projectReconcilers[g.ProjectID] = g
	}
	d.GateReEvaluations = &forgeworker.GateReEvaluationWorker{
		DB: db, Now: now, Lease: cfg.Outbox.LeaseTTL, WorkerID: "siftd:gate_re_evaluation",
		Produce: func(ctx context.Context, payload storage.GateReEvaluationPayload) ([]byte, error) {
			src, err := db.GateReevaluationSource(ctx, payload.RunID)
			if err != nil {
				return forgeworker.GateReEvaluationFailedResult("gate_input_assembly_failed", map[string]string{"code": "schema_invalid", "field": "run_id"}), nil
			}
			rec := projectReconcilers[src.ProjectID]
			if rec == nil {
				return forgeworker.GateReEvaluationFailedResult("gate_input_assembly_failed", map[string]string{"code": "schema_invalid", "field": "project_id"}), nil
			}
			return rec.ProduceReevaluation(ctx, payload, src)
		},
		Complete: db.CompleteGateReEvaluation,
	}
	return d, nil
}

// SetLaunchWorker installs the sole launch_agent consumer after startup
// recovery has produced this daemon's boot ID.
func (d *Daemon) SetLaunchWorker(w *launchworker.Worker) { d.Launch = w }

func (d *Daemon) SetT7Scheduler(s *brain.T7Scheduler) { d.T7 = s }

func (d *Daemon) T7Tick(ctx context.Context) error {
	if d.T7 == nil {
		return nil
	}
	return d.T7.Tick(ctx)
}

// AddChannelWorker installs the independently scoped channel_publish consumer.
// Assembly owns production construction; this seam is also used by integration
// tests that provide a secret resolver and webhook transport.
func (d *Daemon) AddChannelWorker(w *channelworker.Worker) {
	if w != nil {
		d.Channels = append(d.Channels, w)
	}
}

func interruptChannels(attention config.Attention) []storage.InterruptChannel {
	channels := make([]storage.InterruptChannel, 0, len(attention.Channels))
	for _, c := range attention.Channels {
		if !c.Enabled {
			continue
		}
		channels = append(channels, storage.InterruptChannel{ID: c.ID, Type: c.Type, TargetRef: c.TargetRef, Renderer: c.Renderer, Capabilities: append([]string(nil), c.Capabilities...), Default: c.Default})
	}
	return channels
}

func gateReEvalInterruptEmission(attention config.Attention, channels []storage.InterruptChannel) storage.GateReEvalInterruptEmission {
	return storage.GateReEvalInterruptEmission{
		AttentionDailyQuota: map[storage.InterruptSeverity]int{
			storage.SeverityLow:    attention.DailyQuota.Low,
			storage.SeverityNormal: attention.DailyQuota.Normal,
			storage.SeverityHigh:   attention.DailyQuota.High,
		},
		DayTimezone:         attention.DayTimezone,
		DailySummaryAt:      attention.DailySummaryAt,
		MaxEscalations:      attention.MaxEscalations,
		CriticalWindowMS:    attention.CriticalFuse.Window.Milliseconds(),
		CriticalTotalLimit:  attention.CriticalFuse.TotalLimit,
		CriticalPerRunLimit: attention.CriticalFuse.PerRunLimit,
		Channels:            channels,
	}
}

func operators(o config.Operators, k forge.Kind) []string {
	if k == forge.KindGitLab {
		return append([]string(nil), o.GitLab...)
	}
	return append([]string(nil), o.GitHub...)
}

// IntakeTick advances persisted-cursor polling and all Forge fact
// reconciliation. PollOnce consults each cursor's NextPollAtMS, so the named
// intake scheduler may wake more often without changing its adaptive cadence.
func (d *Daemon) IntakeTick(ctx context.Context) error {
	d.intakeMu.Lock()
	defer d.intakeMu.Unlock()
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
	for i, r := range d.Successes {
		if err := r.ReconcileOnce(ctx); err != nil {
			return fmt.Errorf("gate-success[%d]: %w", i, err)
		}
	}
	for i, r := range d.Gates {
		if err := r.ReconcileOnce(ctx); err != nil {
			return fmt.Errorf("gate[%d]: %w", i, err)
		}
	}
	return nil
}

// OutboxTick advances committed external effects independently of Forge fact
// intake. It is woken after every outbox-writing transaction and periodically
// to discover durable retry deadlines after a restart.
func (d *Daemon) OutboxTick(ctx context.Context) error {
	d.outboxMu.Lock()
	defer d.outboxMu.Unlock()
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
	for i, w := range d.Merges {
		if err := w.RunOnce(ctx); err != nil {
			return fmt.Errorf("merge[%d]: %w", i, err)
		}
	}
	for i, w := range d.Channels {
		if err := w.RunOnce(ctx); err != nil {
			return fmt.Errorf("channel_publish[%d]: %w", i, err)
		}
	}
	if d.Alerts != nil {
		if err := d.Alerts.RunOnce(ctx); err != nil {
			return fmt.Errorf("forge_alert: %w", err)
		}
	}
	if d.CommandAcks != nil {
		if err := d.CommandAcks.RunOnce(ctx); err != nil {
			return fmt.Errorf("command_ack: %w", err)
		}
	}
	if d.GateReEvaluations != nil {
		if err := d.GateReEvaluations.RunOnce(ctx); err != nil {
			return fmt.Errorf("gate_re_evaluation: %w", err)
		}
	}
	if d.RerunChecks != nil {
		if err := d.RerunChecks.RunOnce(ctx); err != nil {
			return fmt.Errorf("rerun_checks: %w", err)
		}
	}
	if d.Launch != nil {
		if err := d.Launch.RunOnce(ctx); err != nil {
			return fmt.Errorf("launch_agent: %w", err)
		}
	}
	return nil
}

// Tick preserves the integration-test entry point while production schedules
// its two independently paced domains through IntakeTick and OutboxTick.
func (d *Daemon) Tick(ctx context.Context) error {
	if err := d.IntakeTick(ctx); err != nil {
		return err
	}
	return d.OutboxTick(ctx)
}
