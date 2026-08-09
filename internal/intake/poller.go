// Package intake owns per-project Forge polling and the handoff to T1.
package intake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/miaoxiaoyong/sift/internal/brain"
	"github.com/miaoxiaoyong/sift/internal/forge"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

type Project struct {
	ID                string
	Repo              string
	Ref               forge.ProjectRef
	TriggerLabel      string
	OperatorAllowlist []string
	// T2Agents is the immutable startup snapshot of agents eligible for this
	// project. The evaluator passes these facts to Brain; the storage write
	// port still freezes the backend from the Run's config snapshot.
	T2Agents      []brain.T2AgentCandidate
	AgentBackends map[string]string
}
type Poller struct {
	DB                 *storage.DB
	Forge              forge.Client
	Projects           []Project
	Now                func() time.Time
	Idle, Active, Slow time.Duration
	HourlyLimit        int64
	WarningRatio       float64
	OnIssue            func(context.Context, Project, forge.Issue) error
	Isolated           func(Project, error)
}

// PollOnce polls each project independently. A bad credential/capability stops
// only that project; healthy projects are still polled and scheduled.
func (p *Poller) PollOnce(ctx context.Context) error {
	now := time.Time{}
	if p.Now != nil {
		now = p.Now()
	}
	if now.IsZero() {
		now = time.UnixMilli(1)
	}
	for _, project := range p.Projects {
		cursor, err := p.DB.IntakeCursor(ctx, project.ID, "issues")
		if err != nil {
			return err
		}
		if cursor.NextPollAtMS > 0 && now.UnixMilli() < cursor.NextPollAtMS {
			continue
		}
		// Skip projects already quarantined by a prior auth/capability failure so
		// a bad credential is neither re-probed nor re-alerted every tick (WBS
		// §2.3: alert once, no hammering).
		if isolated, err := p.DB.ProjectIsolated(ctx, project.ID); err == nil && isolated {
			continue
		}
		if err := p.pollProject(ctx, project, now); err != nil {
			var ce *forge.ClassifiedError
			if errors.As(err, &ce) && errors.Is(err, forge.ErrAuthOrCapability) {
				_ = p.DB.SetProjectHealth(ctx, project.ID, "forge_auth_or_capability", now.UnixMilli())
				if p.Isolated != nil {
					p.Isolated(project, err)
				}
				continue
			}
			return err
		}
	}
	return nil
}
func (p *Poller) pollProject(ctx context.Context, project Project, now time.Time) error {
	cur, err := p.DB.IntakeCursor(ctx, project.ID, "issues")
	if err != nil {
		return err
	}
	// The cursor is frozen for this poll transaction, so it is also a stable
	// replay identity for every Forge call in the tick.
	ctx = forge.WithChargeKey(ctx, "intake:tick:"+now.Format(time.RFC3339Nano)+":"+project.ID)
	issues, next, err := p.Forge.ListIssuesByLabel(ctx, project.Ref, project.TriggerLabel, forge.Cursor(cur.Cursor))
	if err != nil {
		return err
	}
	items := make([]storage.IntakeItemInput, 0, len(issues))
	accepted := make([]forge.Issue, 0, len(issues))
	for _, i := range issues {
		if i.ID == "" || i.Author == "" || i.URL == "" {
			return &forge.ClassifiedError{Class: forge.ErrContractViolation, Summary: "normalized issue missing required facts"}
		}
		trigger, ok, err := p.currentTrustedTrigger(ctx, project, i.ID)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		digest := issueDigest(i)
		triggerDigest := labelEventDigest(i.ID, trigger)
		items = append(items, storage.IntakeItemInput{IssueID: i.ID, IssueURL: i.URL, IssueDigest: digest, ForgeKind: string(project.Ref.Kind), Host: project.Ref.Host, ProjectKey: project.Ref.ProjectKey, EventID: "label:" + triggerDigest, EventKind: "trigger_label_added", TargetKind: "issue", Actor: trigger.Actor, ObservedAtMS: trigger.ObservedAt.UnixMilli(), RawDigest: triggerDigest, ForceHITLBeforeStart: !isAllowedActor(project.OperatorAllowlist, i.Author)})
		accepted = append(accepted, i)
	}
	mode := "idle"
	interval := p.Idle
	if len(issues) > 0 {
		mode = "active"
		interval = p.Active
	}
	if p.HourlyLimit > 0 && p.WarningRatio > 0 && p.WarningRatio < 1 {
		if status, e := p.DB.ForgeAPIBudgetStatus(ctx, project.ID, now.UnixMilli(), p.HourlyLimit, p.WarningRatio); e == nil && status.SlowPoll {
			mode = "slow"
			interval = p.Slow
		}
	}
	if interval <= 0 {
		interval = time.Minute
	}
	if err := p.DB.PersistIntakeBatch(ctx, storage.PersistIntakeBatchCmd{ProjectID: project.ID, Stream: "issues", Cursor: string(next), PollMode: mode, NextPollAtMS: now.Add(interval).UnixMilli(), NowMS: now.UnixMilli(), Items: items}); err != nil {
		return err
	}
	// T1 is deliberately after the transaction. A crash here leaves the cursor
	// advanced but the durable intake item pending, ready for a later evaluator.
	if p.OnIssue != nil {
		for _, i := range accepted {
			if err := p.OnIssue(ctx, project, i); err != nil {
				return err
			}
		}
	}
	return nil
}
func (p *Poller) currentTrustedTrigger(ctx context.Context, project Project, issueID string) (forge.LabelEvent, bool, error) {
	events, _, err := p.Forge.ListLabelEvents(ctx, project.Ref, forge.TargetRef{Kind: forge.TargetIssue, ID: issueID}, "")
	if err != nil {
		return forge.LabelEvent{}, false, err
	}
	var latest forge.LabelEvent
	found := false
	for _, event := range events {
		if event.TargetID != issueID || event.Label != project.TriggerLabel || event.Actor == "" || event.ObservedAt.IsZero() {
			continue
		}
		if !found || event.ObservedAt.After(latest.ObservedAt) || (event.ObservedAt.Equal(latest.ObservedAt) && labelEventOrder(event) > labelEventOrder(latest)) {
			latest, found = event, true
		}
	}
	if !found || latest.Action != forge.LabelAdded || !isAllowedActor(project.OperatorAllowlist, latest.Actor) {
		return forge.LabelEvent{}, false, nil
	}
	return latest, true, nil
}

func isAllowedActor(allowlist []string, actor string) bool {
	for _, allowed := range allowlist {
		if allowed == actor {
			return true
		}
	}
	return false
}

func labelEventOrder(e forge.LabelEvent) string {
	return string(e.Action) + "\x00" + e.Actor
}

func labelEventDigest(issueID string, e forge.LabelEvent) string {
	b, _ := json.Marshal(struct {
		IssueID string           `json:"issue_id"`
		Event   forge.LabelEvent `json:"event"`
	}{issueID, e})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func issueDigest(i forge.Issue) string {
	b, _ := json.Marshal(i)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
