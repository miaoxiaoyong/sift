package storage

import "context"

// Scheduler is a wakeup-driven skeleton shared by the only three daemon
// schedulers. It intentionally owns no ticker: callers wake it after a commit
// or arm it from persisted next-at timestamps, so restart recovery has no
// in-memory timing authority.
type Scheduler struct {
	wake   chan chan error
	onWake func(context.Context) error
}

func newScheduler(onWake func(context.Context) error) Scheduler {
	return Scheduler{wake: make(chan chan error, 1), onWake: onWake}
}
func (s *Scheduler) Wake() {
	select {
	case s.wake <- nil:
	default:
	}
}

// WakeAndWait establishes that a startup sweep has completed. Commit wakeups
// remain non-blocking through Wake.
func (s *Scheduler) WakeAndWait(ctx context.Context) error {
	done := make(chan error, 1)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.wake <- done:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}

func (s *Scheduler) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case done := <-s.wake:
			var err error
			if s.onWake != nil {
				err = s.onWake(ctx)
			}
			if done != nil {
				done <- err
			}
			if err != nil {
				return err
			}
		}
	}
}

// IntakeScheduler owns persisted forge-cursor polling wakeups.
type IntakeScheduler struct{ Scheduler }

func NewIntakeScheduler(run func(context.Context) error) *IntakeScheduler {
	return &IntakeScheduler{newScheduler(run)}
}

// OutboxScheduler owns committed side effects and their durable retries.
type OutboxScheduler struct{ Scheduler }

func NewOutboxScheduler(run func(context.Context) error) *OutboxScheduler {
	return &OutboxScheduler{newScheduler(run)}
}

// SupervisorScheduler owns attempts, interrupt expiry and timeout scans.
type SupervisorScheduler struct{ Scheduler }

func NewSupervisorScheduler(run func(context.Context) error) *SupervisorScheduler {
	return &SupervisorScheduler{newScheduler(run)}
}

// Wakeups groups the three named scheduler entry points; write ports may call
// the appropriate one after commit, never while a transaction is open.
type Wakeups struct {
	Intake     *IntakeScheduler
	Supervisor *SupervisorScheduler
	Outbox     *OutboxScheduler
}
