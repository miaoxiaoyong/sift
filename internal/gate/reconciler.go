package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/miaoxiaoyong/sift/internal/brain"
	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/forge"
	"github.com/miaoxiaoyong/sift/internal/policy"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// Reconciler freezes remote Change facts and drives the production M4 path.
// All Forge and Brain reads finish before the Gate write boundary.
type Reconciler struct {
	DB            *storage.DB
	Forge         forge.Client
	Brain         *brain.Shell
	ProjectID     string
	Project       forge.ProjectRef
	Repo          string
	Defaults      config.GateDefaults
	Certification config.Certification
	Attention     config.Attention
	Channels      []storage.InterruptChannel
	Now           func() time.Time
}

func (r *Reconciler) ReconcileOnce(ctx context.Context) error {
	if r.DB == nil || r.Forge == nil || r.Brain == nil || r.ProjectID == "" || r.Repo == "" {
		return errors.New("gate reconciler: incomplete configuration")
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	candidates, err := r.DB.GateCandidates(ctx, r.ProjectID)
	if err != nil {
		return err
	}
	for _, c := range candidates {
		if err := r.reconcile(ctx, c, now()); err != nil {
			var policyErr *policyReadError
			if errors.As(err, &policyErr) {
				if isolateErr := r.DB.SetProjectHealth(ctx, r.ProjectID, "policy_invalid", now().UnixMilli()); isolateErr != nil {
					return isolateErr
				}
				return nil
			}
			return err
		}
	}
	return nil
}

func (r *Reconciler) reconcile(ctx context.Context, c storage.GateCandidate, now time.Time) error {
	ctx = forge.WithChargeKey(ctx, "gate:"+c.RunID+":"+c.ChangeID+":"+fmt.Sprint(now.UnixMilli()))
	before, err := r.Forge.GetChange(ctx, r.Project, c.ChangeID)
	if err != nil {
		return err
	}
	diff, err := r.Forge.GetChangeDiff(ctx, r.Project, c.ChangeID)
	if err != nil {
		return err
	}
	after, err := r.Forge.GetChange(ctx, r.Project, c.ChangeID)
	if err != nil {
		return err
	}
	if before.ID != after.ID || before.URL != after.URL || before.HeadSHA != after.HeadSHA {
		return nil
	}
	version, err := r.DB.FreezeGateChangeHead(ctx, c.RunID, c.ChangeID, after.HeadSHA, c.Version, now.UnixMilli())
	if err == storage.ErrRejectedStale {
		return nil
	}
	if err != nil {
		return err
	}
	c.Version = version
	paths := changedPaths(diff)
	if len(paths) == 0 {
		return fmt.Errorf("gate reconciler: Change %s has no complete changed-path facts", c.ChangeID)
	}
	checks, err := r.Forge.GetChecks(ctx, r.Project, after.HeadSHA)
	if err != nil {
		return err
	}
	in, err := r.input(ctx, c, after, paths, diff, checks, now)
	if err != nil {
		return err
	}
	features, _ := json.Marshal(map[string]any{"run": map[string]string{"kind": c.TaskKind}, "change": map[string]string{"id": after.ID, "head_sha": after.HeadSHA}})
	verdict, recorded, err := r.record(ctx, c, in, features, now)
	if err != nil {
		return err
	}
	if verdict.Kind == "ready" && verdict.Code == "merge" {
		return EnqueueMergeChange(ctx, r.DB, in, verdict, recorded.EvaluationID, "merge", now.UnixMilli())
	}
	return nil
}

func (r *Reconciler) input(ctx context.Context, c storage.GateCandidate, change forge.Change, paths []string, diff string, suite forge.CheckSuite, now time.Time) (Input, error) {
	risk, err := r.t3(ctx, c, change, diff)
	if err != nil {
		return Input{}, err
	}
	checks := Checks{Conclusion: suite.Conclusion, ExternalURL: suite.ExternalURL}
	if checks.ExternalURL == "" {
		checks.ExternalURL = change.URL
		suite.ExternalURL = change.URL
	}
	if checks.Conclusion == "pending" {
		n := now.UnixMilli()
		checks.PendingStartedAtMS, checks.ObservedAtMS, checks.PendingTimedOut = &n, &n, boolp(false)
	}
	if checks.Conclusion == "failure" {
		triage, jobs, err := r.t5(ctx, c, change, suite)
		if err != nil {
			return Input{}, err
		}
		checks.Triage = &triage
		checks.FailedJobs = jobs
	}
	base, err := readBasePolicy(ctx, r.Repo, c.BaseRef)
	if err != nil {
		return Input{}, err
	}
	cert, err := r.DB.Certification(ctx, c.TaskKind)
	if err != nil {
		cert.TaskKind = c.TaskKind
		cert.CertificationVersion, err = config.CertificationRulesVersion(r.Certification)
		if err != nil {
			return Input{}, err
		}
	}
	cas, err := r.DB.AutoMergeEnabled(ctx, r.Project)
	if err != nil {
		return Input{}, err
	}
	effective, hash, certVersion, _, err := policy.Assemble(base, r.Defaults, c.TaskKind, policy.CertificationProjection{TaskKind: cert.TaskKind, CertificationVersion: cert.CertificationVersion, Certified: cert.Certified}, cas)
	if err != nil {
		return Input{}, err
	}
	effects, err := r.DB.GateCommandEffectsForInput(ctx, c.RunID, change.ID, change.HeadSHA, hash)
	if err != nil {
		return Input{}, err
	}
	exemptions := make([]Exemption, len(effects.Exemptions))
	for i, exemption := range effects.Exemptions {
		exemptions[i] = Exemption{RunID: exemption.RunID, HeadSHA: exemption.HeadSHA, RuleID: exemption.RuleID, MatchedPathsDigest: exemption.MatchedPathsDigest}
	}
	if effects.ReviewApproved {
		change.ReviewState = forge.Approved
	}
	rules, err := config.CertificationRulesVersion(r.Certification)
	if err != nil {
		return Input{}, err
	}
	return Input{SchemaVersion: 1, Identity: Identity{RunID: c.RunID, ProjectID: c.ProjectID, TaskKind: c.TaskKind, ChangeID: change.ID}, Change: Change{State: string(change.State), HeadSHA: change.HeadSHA, BaseRef: c.BaseRef, HeadRef: c.HeadRef, IsDraft: change.IsDraft, Mergeability: string(change.Mergeability), ReviewState: string(change.ReviewState), PathsComplete: true, ChangedPaths: paths, FilesChanged: len(paths)}, Checks: checks, EffectivePolicy: effective, EffectivePolicyHash: hash, CertificationRulesVersion: rules, CertificationVersion: certVersion, Risk: risk, OneTimeExemptions: exemptions}, nil
}

func (r *Reconciler) t3(ctx context.Context, c storage.GateCandidate, change forge.Change, diff string) (Risk, error) {
	input, err := brain.BuildT3Input(brain.T3Input{RunID: c.RunID, TaskKind: brain.TaskKind(c.TaskKind), Change: brain.T3Change{ChangeInput: brain.ChangeInput{ID: change.ID, URL: change.URL, HeadSHA: change.HeadSHA}, Diff: diff}})
	if err != nil {
		return Risk{}, err
	}
	call, err := r.Brain.Call(ctx, brain.T3Contract(), brain.CallParams{Scope: "run", SubjectKey: "run:" + c.RunID, ProjectID: c.ProjectID, RunID: c.RunID, Input: input})
	if err != nil {
		return Risk{}, err
	}
	out, source, err := brain.T3ResultFromCall(call)
	if err != nil {
		return Risk{}, err
	}
	return Risk{RiskScore: *out.RiskScore, RiskPoints: *out.RiskPoints, Rationale: *out.Rationale, Source: sourceGate(source)}, nil
}

func (r *Reconciler) t5(ctx context.Context, c storage.GateCandidate, change forge.Change, suite forge.CheckSuite) (Triage, []Job, error) {
	jobs := make([]brain.T5Job, 0, len(suite.FailedJobs))
	gateJobs := make([]Job, 0, len(suite.FailedJobs))
	for _, j := range suite.FailedJobs {
		id := j.ID
		if id == "" {
			id = j.WebURL
		}
		jobs = append(jobs, brain.T5Job{ID: id, Name: j.Name, WebURL: j.WebURL, AllowFailure: j.AllowFailure})
		gateJobs = append(gateJobs, Job{ID: id, Name: j.Name, WebURL: j.WebURL, AllowFailure: j.AllowFailure})
	}
	input, err := brain.BuildT5Input(brain.T5Input{RunID: c.RunID, Change: brain.ChangeInput{ID: change.ID, URL: change.URL, HeadSHA: change.HeadSHA}, Checks: brain.T5Checks{ExternalURL: suite.ExternalURL, FailedJobs: jobs}})
	if err != nil {
		return Triage{}, nil, err
	}
	call, err := r.Brain.Call(ctx, brain.T5Contract(jobs), brain.CallParams{Scope: "run", SubjectKey: "run:" + c.RunID, ProjectID: c.ProjectID, RunID: c.RunID, Input: input})
	if err != nil {
		return Triage{}, nil, err
	}
	out, err := brain.T5TriageFromCall(call)
	if err != nil {
		return Triage{}, nil, err
	}
	return Triage{Classification: string(out.Classification), RetryCheckID: out.RetryCheckID, Source: sourceGate(out.Source)}, gateJobs, nil
}

func (r *Reconciler) record(ctx context.Context, c storage.GateCandidate, in Input, features json.RawMessage, now time.Time) (Verdict, storage.RecordedGateEvaluation, error) {
	v, err := Evaluate(in)
	if err != nil {
		return Verdict{}, storage.RecordedGateEvaluation{}, err
	}
	if v.Kind != "hitl" {
		got, recorded, err := EvaluateAndRecord(ctx, r.DB, in, false, features, now.UnixMilli())
		return got, recorded, err
	}
	cmd, err := interruptCommand(c, in, v, r.Attention, r.Channels, now.UnixMilli())
	if err != nil {
		return Verdict{}, storage.RecordedGateEvaluation{}, err
	}
	got, recorded, _, err := EvaluateRecordAndEmitInterrupt(ctx, r.DB, in, false, features, cmd)
	return got, recorded, err
}

func sourceGate(s brain.BrainSource) Source {
	return Source{Kind: s.Kind, LogicalCallID: s.LogicalCallID, PromptVersion: s.PromptVersion, OutputSchemaVersion: s.OutputSchemaVersion, Version: s.Version, Reason: s.Reason}
}
func boolp(v bool) *bool { return &v }

type policyReadError struct{ err error }

func (e *policyReadError) Error() string { return e.err.Error() }
func (e *policyReadError) Unwrap() error { return e.err }

func readBasePolicy(ctx context.Context, repo, base string) (policy.BasePolicy, error) {
	sha, err := exec.CommandContext(ctx, "git", "-C", repo, "rev-parse", "--verify", base+"^{commit}").Output()
	if err != nil {
		return policy.BasePolicy{}, &policyReadError{fmt.Errorf("read base policy %q: resolve base: %w", base, err)}
	}
	object := strings.TrimSpace(string(sha))
	out, err := exec.CommandContext(ctx, "git", "-C", repo, "show", object+":.sift/policy.yaml").Output()
	if err == nil {
		base, parseErr := policy.Parse(out)
		if parseErr != nil {
			return policy.BasePolicy{}, &policyReadError{fmt.Errorf("read base policy %q: parse: %w", object, parseErr)}
		}
		return base, nil
	}
	// A missing policy is normal, but only after the base commit has been
	// resolved. ls-tree itself must succeed; its empty result is the only
	// missing-file case, so repository/read failures cannot fail open.
	listed, listErr := exec.CommandContext(ctx, "git", "-C", repo, "ls-tree", "--name-only", object, "--", ".sift/policy.yaml").Output()
	if listErr != nil {
		return policy.BasePolicy{}, &policyReadError{fmt.Errorf("read base policy %q: inspect policy path: %w", object, listErr)}
	}
	if strings.TrimSpace(string(listed)) == "" {
		return policy.Missing(), nil
	}
	return policy.BasePolicy{}, &policyReadError{fmt.Errorf("read base policy %q: git show: %w", object, err)}
}
func changedPaths(diff string) []string {
	seen := map[string]bool{}
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+++ b/") {
			p := strings.TrimPrefix(line, "+++ b/")
			if p != "" && p != "/dev/null" {
				seen[p] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
