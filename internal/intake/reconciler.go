package intake

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/forge"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// Reconciler applies forge-owned facts to active Runs. It is intentionally a
// separate scheduled pass from intake: current object state (Issue/Change) is
// observation and has no actor gate, while removing the trigger label is an
// operator command and does require the project's allowlist.
type Reconciler struct {
	DB            *storage.DB
	Forge         forge.Client
	Projects      []Project
	Now           func() time.Time
	Certification config.Certification
	Isolated      func(Project, error)
}

// ReconcileOnce performs one independent reconciliation pass per project.
// An auth/capability failure quarantines only that project, matching intake's
// failure boundary; other projects continue to be reconciled.
func (r *Reconciler) ReconcileOnce(ctx context.Context) error {
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	if now.IsZero() {
		now = time.UnixMilli(1)
	}
	for _, project := range r.Projects {
		isolated, err := r.DB.ProjectIsolated(ctx, project.ID)
		if err != nil {
			return err
		}
		if isolated {
			continue
		}
		if err := r.reconcileProject(ctx, project, now); err != nil {
			var classified *forge.ClassifiedError
			if errors.As(err, &classified) && errors.Is(err, forge.ErrAuthOrCapability) {
				_ = r.DB.SetProjectHealth(ctx, project.ID, "forge_auth_or_capability", now.UnixMilli())
				if r.Isolated != nil {
					r.Isolated(project, err)
				}
				continue
			}
			return err
		}
	}
	return nil
}

func (r *Reconciler) reconcileProject(ctx context.Context, project Project, now time.Time) error {
	ctx = forge.WithChargeKey(ctx, "reconcile:tick:"+now.Format(time.RFC3339Nano)+":"+project.ID)
	candidates, err := r.DB.ReverseSyncCandidates(ctx, project.ID)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		// Object state is authoritative. Do not require an actor for these
		// reads: the read itself is the evidence, not a user instruction.
		issue, err := r.Forge.GetIssue(ctx, project.Ref, candidate.IssueID)
		if err != nil {
			return err
		}
		if issue.State == forge.IssueClosed {
			if err := r.fail(ctx, candidate, "closed_upstream", now); err != nil {
				return err
			}
			continue
		}

		if candidate.ChangeID != "" {
			change, err := r.Forge.GetChange(ctx, project.Ref, candidate.ChangeID)
			if err != nil {
				return err
			}
			switch change.State {
			case forge.ChangeMerged:
				siftMerge, err := r.DB.IsSiftMerge(ctx, candidate.RunID, change.ID, change.HeadSHA)
				if err != nil {
					return err
				}
				if !siftMerge {
					if err := r.recordExternalMerge(ctx, candidate, change, now); err != nil {
						return err
					}
				}
				if _, err := r.DB.TransitionRun(ctx, candidate.RunID, candidate.Version, storage.DomainCommand{
					To: storage.RunDone, Source: storage.SourceForge, ChangeID: change.ID,
					ChangeURL: change.URL, ChangeHeadSHA: change.HeadSHA, GateBypassed: !siftMerge,
					OccurredAtMS: now.UnixMilli(),
				}); err != nil && !errors.Is(err, storage.ErrRejectedStale) {
					return err
				}
				continue
			case forge.ChangeClosed:
				if err := r.fail(ctx, candidate, "change_closed", now); err != nil {
					return err
				}
				continue
			}
		}

		// A label removal is the only reverse-sync input treated as a command.
		// The latest event wins; an untrusted removal is observed but ignored.
		events, _, err := r.Forge.ListLabelEvents(ctx, project.Ref, forge.TargetRef{Kind: forge.TargetIssue, ID: candidate.IssueID}, "")
		if err != nil {
			return err
		}
		if event, ok := latestTriggerEvent(events, candidate.IssueID, project.TriggerLabel); ok && event.Action == forge.LabelRemoved && isAllowedActor(project.OperatorAllowlist, event.Actor) {
			if err := r.fail(ctx, candidate, "untriggered", now); err != nil {
				return err
			}
		}
	}
	return nil
}

// recordExternalMerge preserves the observed Forge fact and settles only an
// unambiguous, prior Gate calibration for exactly this Run/head. An unbound
// fact remains auditable but is never guessed into a calibration sample.
func (r *Reconciler) recordExternalMerge(ctx context.Context, c storage.ReverseSyncCandidate, change forge.Change, now time.Time) error {
	payload, err := json.Marshal(map[string]string{"change_id": change.ID, "head_sha": change.HeadSHA, "merge_sha": change.MergeSHA, "state": string(change.State)})
	if err != nil {
		return err
	}
	factID, err := r.DB.AppendExternalMergeFact(ctx, storage.EventCmd{RunID: c.RunID, ProjectID: c.ProjectID, Type: "forge_change_merged", Source: storage.SourceForge, PayloadJSON: payload, IdempotencyKey: "forge-change-merged:" + c.RunID + ":" + change.ID + ":" + change.HeadSHA, OccurredAtMS: now.UnixMilli(), RecordedAtMS: now.UnixMilli()}, change.HeadSHA)
	if err != nil {
		return err
	}
	_, err = r.DB.RecordHumanDecision(ctx, storage.RecordHumanDecisionCmd{Action: storage.DecisionManualMerge, ForgeFactEventID: factID, NowMS: now.UnixMilli(), Certification: r.Certification})
	if err != nil {
		return err
	}
	return nil
}

func (r *Reconciler) fail(ctx context.Context, c storage.ReverseSyncCandidate, reason string, now time.Time) error {
	_, err := r.DB.TransitionRun(ctx, c.RunID, c.Version, storage.DomainCommand{
		To: storage.RunFailed, Source: storage.SourceForge, FailureReason: reason, OccurredAtMS: now.UnixMilli(),
	})
	if errors.Is(err, storage.ErrRejectedStale) {
		return nil
	}
	return err
}

func latestTriggerEvent(events []forge.LabelEvent, issueID, label string) (forge.LabelEvent, bool) {
	var latest forge.LabelEvent
	found := false
	for _, event := range events {
		if event.TargetID != issueID || event.Label != label || event.Actor == "" || event.ObservedAt.IsZero() {
			continue
		}
		if !found || event.ObservedAt.After(latest.ObservedAt) || (event.ObservedAt.Equal(latest.ObservedAt) && labelEventOrder(event) > labelEventOrder(latest)) {
			latest, found = event, true
		}
	}
	return latest, found
}
