package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Runner func(context.Context, string, []string, []byte) ([]byte, []byte, error)

func ExecRunner(c context.Context, n string, a []string, in []byte) ([]byte, []byte, error) {
	x := exec.CommandContext(c, n, a...)
	x.Stdin = bytes.NewReader(in)
	var o, e bytes.Buffer
	x.Stdout = &o
	x.Stderr = &e
	z := x.Run()
	return o.Bytes(), e.Bytes(), z
}

type Adapter struct {
	CLI  string
	Kind Kind
	Run  Runner

	// charger reserves one unit of Forge API budget before each CLI subprocess.
	// Production adapters set requireBudget; tests may omit it.
	charger       Charger
	requireBudget bool

	mu                 sync.RWMutex
	autoMergeSupported map[string]bool
	chargeSeqs         map[string]int64
	capabilities       AutoMergeCapabilityReader
}

func NewAdapter(k Kind, cli string, r Runner) *Adapter {
	if cli == "" {
		cli = "gh"
		if k == KindGitLab {
			cli = "glab"
		}
	}
	if r == nil {
		r = ExecRunner
	}
	return &Adapter{CLI: cli, Kind: k, Run: r, autoMergeSupported: map[string]bool{}, chargeSeqs: map[string]int64{}}
}

// WithCharger installs the forge API budget charger. Without it the adapter
// does not charge, preserving the M1 fake/no-budget behaviour. Returns the
// adapter for constructor chaining.
func (a *Adapter) WithCharger(c Charger) *Adapter {
	a.charger = c
	return a
}

// RequireBudget makes this adapter reject external calls unless the daemon has
// installed a charger and the caller supplied a stable charge-key context.
// Fake adapters intentionally leave this disabled.
func (a *Adapter) RequireBudget() *Adapter {
	a.requireBudget = true
	return a
}

// WithAutoMergeCapabilityReader makes MergeChange consume the durable project
// capability projection in addition to this process's startup probe result.
func (a *Adapter) WithAutoMergeCapabilityReader(r AutoMergeCapabilityReader) *Adapter {
	a.capabilities = r
	return a
}

// chargeAPICall reserves one unit of forge API budget before a CLI subprocess
// launches (forge.md §9: the sole charging point is inside the adapter). The
// stable charge key is the caller-supplied base (WithChargeKey) plus an
// incrementing per-base sequence, so each request is distinct yet
// replay-stable across crash recovery. When the budget is exhausted it
// returns an ErrRateLimited classified error and the subprocess is not run;
// with no charger or no charge-key base it is a no-op.
func (a *Adapter) chargeAPICall(ctx context.Context, p ProjectRef) error {
	if a.charger == nil {
		if a.requireBudget {
			return &ClassifiedError{Class: ErrContractViolation, Summary: "production forge adapter requires a charger"}
		}
		return nil
	}
	base, ok := chargeKeyBaseFrom(ctx)
	if !ok || base == "" {
		if a.requireBudget {
			return &ClassifiedError{Class: ErrContractViolation, Summary: "forge call requires a stable charge key"}
		}
		return nil
	}
	a.mu.Lock()
	a.chargeSeqs[base]++
	seq := a.chargeSeqs[base]
	a.mu.Unlock()
	key := base + ":" + strconv.FormatInt(seq, 10)
	res, err := a.charger.Charge(ctx, p, key)
	if err != nil {
		return &ClassifiedError{Class: ErrTransient, Summary: "forge api budget charge failed: " + err.Error()}
	}
	if res.Exhausted {
		return &ClassifiedError{Class: ErrRateLimited, Summary: "forge api budget exhausted for project"}
	}
	return nil
}

// AutoMergeSupported reports whether this process has proved the project's
// expected-head CAS path during startup. The zero value is deliberately false:
// a first real merge must never be capability discovery.
func (a *Adapter) AutoMergeSupported(p ProjectRef) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.autoMergeSupported[projectCapabilityKey(p)]
}

// ProbeAndRecordAutoMergeCapability is the startup handoff: probe before any
// worker can merge, then persist both result and audit evidence. Recording an
// unproven result is intentional and must not be skipped.
func (a *Adapter) ProbeAndRecordAutoMergeCapability(ctx context.Context, projectID string, p ProjectRef, recorder AutoMergeCapabilityRecorder, now time.Time) error {
	if recorder == nil {
		return errors.New("forge: auto-merge capability recorder is required")
	}
	proven, evidence := a.ProbeAutoMergeCapability(ctx, p)
	return recorder.UpdateProjectAutoMergeCapability(ctx, projectID, proven, evidence, now.UnixMilli())
}

// ProbeAutoMergeCapability performs a non-mutating startup proof that this CLI
// can submit a JSON request body to the platform API. Both supported forge
// merge endpoints accept expected-head SHA in that body; the actual endpoint
// is not called, so no Change is used as a probe. Any ambiguity remains false.
func (a *Adapter) ProbeAutoMergeCapability(ctx context.Context, p ProjectRef) (proven bool, evidence string) {
	if p.Kind != a.Kind {
		return false, "adapter kind mismatch"
	}
	out, stderr, err := a.Run(ctx, a.CLI, []string{"api", "--help"}, nil)
	if err != nil {
		return false, "api help failed: " + strings.TrimSpace(string(stderr))
	}
	if !strings.Contains(string(out), "--input") {
		return false, "api command does not advertise --input"
	}
	a.mu.Lock()
	a.autoMergeSupported[projectCapabilityKey(p)] = true
	a.mu.Unlock()
	return true, "api --input supports expected-head CAS request body"
}

func projectCapabilityKey(p ProjectRef) string {
	return string(p.Kind) + "\x00" + p.Host + "\x00" + p.ProjectKey
}

func (a *Adapter) disableAutoMerge(p ProjectRef) {
	a.mu.Lock()
	a.autoMergeSupported[projectCapabilityKey(p)] = false
	a.mu.Unlock()
}

func unsupportedCAS(err error) bool {
	var classified *ClassifiedError
	return errors.As(err, &classified) && classified.Class == ErrAuthOrCapability &&
		(strings.Contains(strings.ToLower(classified.Summary), "unknown parameter") ||
			strings.Contains(strings.ToLower(classified.Summary), "unsupported parameter") ||
			strings.Contains(strings.ToLower(classified.Summary), "capability_unsupported"))
}
func NewGitHub(c string, r Runner) *Adapter { return NewAdapter(KindGitHub, c, r) }
func NewGitLab(c string, r Runner) *Adapter { return NewAdapter(KindGitLab, c, r) }

// NewProductionAdapter is the daemon-only constructor. Unlike NewAdapter,
// which remains useful for contract tests and fakes, it cannot be used
// without the storage-backed budget charger.
func NewProductionAdapter(k Kind, cli string, r Runner, charger Charger) (*Adapter, error) {
	if charger == nil {
		return nil, errors.New("forge: production adapter requires charger")
	}
	return NewAdapter(k, cli, r).WithCharger(charger).RequireBudget(), nil
}

var retryAfterPattern = regexp.MustCompile(`(?i)(?:retry-after|x-ratelimit-reset|rate-limit-reset)[:= ]+([0-9]+)`)

func classify(s string, e error) error {
	q := strings.ToLower(s)
	cl := ErrTransient
	if strings.Contains(q, "429") || strings.Contains(q, "rate limit") {
		cl = ErrRateLimited
	}
	if strings.Contains(q, "401") || strings.Contains(q, "403") || strings.Contains(q, "unauthorized") || strings.Contains(q, "forbidden") || strings.Contains(q, "permission") {
		cl = ErrAuthOrCapability
	}
	if strings.Contains(q, "409") || strings.Contains(q, "head sha") || strings.Contains(q, "head commit") || strings.Contains(q, "sha does not match") {
		cl = ErrSemanticConflict
	}
	if strings.Contains(q, "unknown parameter") || strings.Contains(q, "unsupported parameter") || strings.Contains(q, "capability_unsupported") {
		cl = ErrAuthOrCapability
	}
	if s == "" {
		s = e.Error()
	}
	if len(s) > 2048 {
		s = s[:2048]
	}
	ce := &ClassifiedError{Class: cl, Summary: strings.TrimSpace(s)}
	if cl == ErrRateLimited {
		if m := retryAfterPattern.FindStringSubmatch(s); len(m) == 2 {
			if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
				if strings.Contains(strings.ToLower(m[0]), "retry-after") {
					ce.RetryAt = time.Now().Add(time.Duration(n) * time.Second)
				} else {
					ce.RetryAt = time.Unix(n, 0)
				}
			}
		}
	}
	return ce
}
func (a *Adapter) call(ctx context.Context, p ProjectRef, path, method string, in []byte, v any) error {
	if p.Kind != a.Kind {
		return &ClassifiedError{Class: ErrAuthOrCapability, Summary: "adapter kind mismatch"}
	}
	if err := a.chargeAPICall(ctx, p); err != nil {
		return err
	}
	args := []string{"api", path, "--hostname", p.Host}
	if method != "GET" {
		args = append(args, "--method", method, "--input", "-")
	}
	o, s, e := a.Run(ctx, a.CLI, args, in)
	if e != nil {
		return classify(string(s), e)
	}
	if v != nil && len(bytes.TrimSpace(o)) == 0 {
		return &ClassifiedError{Class: ErrContractViolation, Summary: "empty response"}
	}
	if v != nil {
		if e = json.Unmarshal(o, v); e != nil {
			return &ClassifiedError{Class: ErrContractViolation, Summary: "invalid JSON response"}
		}
	}
	return nil
}
func pathPart(s string) string { return url.PathEscape(s) }
func (a *Adapter) base(p ProjectRef) string {
	if a.Kind == KindGitHub {
		return "/repos/" + pathPart(p.ProjectKey)
	}
	return "/projects/" + pathPart(p.ProjectKey)
}
func pagePath(p string, n int) string {
	sep := "?"
	if strings.Contains(p, "?") {
		sep = "&"
	}
	return p + sep + "page=" + strconv.Itoa(n) + "&per_page=100"
}
func (a *Adapter) pages(ctx context.Context, p ProjectRef, path string, fn func([]byte) error) error {
	for n := 1; ; n++ {
		var raw json.RawMessage
		if e := a.call(ctx, p, pagePath(path, n), "GET", nil, &raw); e != nil {
			return e
		}
		if e := fn(raw); e != nil {
			return e
		}
		var xs []json.RawMessage
		if json.Unmarshal(raw, &xs) != nil {
			return &ClassifiedError{Class: ErrContractViolation, Summary: "list response is not an array"}
		}
		if len(xs) < 100 {
			return nil
		}
	}
}

type rawIssue struct {
	Number  int    `json:"number"`
	IID     int    `json:"iid"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	WebURL  string `json:"web_url"`
	State   string `json:"state"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
	Author struct {
		Username string `json:"username"`
	} `json:"author"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	UpdatedAt time.Time        `json:"updated_at"`
	Pull      *json.RawMessage `json:"pull_request"`
}

func (a *Adapter) issue(x rawIssue) (Issue, error) {
	id, author, link := strconv.Itoa(x.Number), x.User.Login, x.HTMLURL
	if a.Kind == KindGitLab {
		id, author, link = strconv.Itoa(x.IID), x.Author.Username, x.WebURL
	}
	if id == "0" || author == "" || link == "" {
		return Issue{}, &ClassifiedError{Class: ErrContractViolation, Summary: "issue missing required field"}
	}
	state := IssueOpen
	if a.Kind == KindGitHub {
		switch x.State {
		case "open":
		case "closed":
			state = IssueClosed
		default:
			return Issue{}, &ClassifiedError{Class: ErrContractViolation, Summary: "issue has unknown state"}
		}
	} else {
		switch x.State {
		case "opened":
		case "closed":
			state = IssueClosed
		default:
			return Issue{}, &ClassifiedError{Class: ErrContractViolation, Summary: "issue has unknown state"}
		}
	}
	labels := []string{}
	for _, l := range x.Labels {
		labels = append(labels, l.Name)
	}
	return Issue{ID: id, Title: x.Title, Body: x.Body, Author: author, URL: link, State: state, Labels: sortDedupe(labels)}, nil
}
func (a *Adapter) ListIssuesByLabel(ctx context.Context, p ProjectRef, label string, since Cursor) ([]Issue, Cursor, error) {
	tracker, err := newCursorTracker(since)
	if err != nil {
		return nil, "", err
	}
	path := a.base(p) + "/issues?labels=" + url.QueryEscape(label)
	if tracker.queryTime() != "" {
		key := "since"
		if a.Kind == KindGitLab {
			key = "updated_after"
		}
		path += "&" + key + "=" + url.QueryEscape(tracker.queryTime())
	}
	if a.Kind == KindGitHub {
		path += "&state=open&sort=updated&direction=asc"
	} else {
		path += "&state=opened&order_by=updated_at&sort=asc"
	}
	out := []Issue{}
	e := a.pages(ctx, p, path, func(raw []byte) error {
		var xs []rawIssue
		if json.Unmarshal(raw, &xs) != nil {
			return &ClassifiedError{Class: ErrContractViolation, Summary: "invalid issue list"}
		}
		for _, x := range xs {
			if a.Kind == KindGitHub && x.Pull != nil {
				continue
			}
			i, e := a.issue(x)
			if e != nil {
				return e
			}
			newItem, e := tracker.add(i.ID, x.UpdatedAt)
			if e != nil {
				return e
			}
			if newItem {
				out = append(out, i)
			}
		}
		return nil
	})
	return out, tracker.next(), e
}
func (a *Adapter) GetIssue(ctx context.Context, p ProjectRef, id string) (Issue, error) {
	var x rawIssue
	if e := a.call(ctx, p, a.base(p)+"/issues/"+pathPart(id), "GET", nil, &x); e != nil {
		return Issue{}, e
	}
	return a.issue(x)
}

type rawComment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Author struct {
		Username string `json:"username"`
	} `json:"author"`
}

func (a *Adapter) listComments(ctx context.Context, p ProjectRef, target TargetRef, since Cursor) ([]Comment, Cursor, error) {
	if target.ID == "" || (target.Kind != TargetIssue && target.Kind != TargetChange) {
		return nil, "", &ClassifiedError{Class: ErrContractViolation, Summary: "invalid target"}
	}
	id := target.ID
	tracker, err := newCursorTracker(since)
	if err != nil {
		return nil, "", err
	}
	path := a.base(p) + "/issues/" + pathPart(id) + "/comments"
	if a.Kind == KindGitLab {
		resource := "issues"
		if target.Kind == TargetChange {
			resource = "merge_requests"
		}
		path = a.base(p) + "/" + resource + "/" + pathPart(id) + "/notes"
	}
	if tracker.queryTime() != "" {
		key := "since"
		if a.Kind == KindGitLab {
			key = "created_after"
		}
		path += "?" + key + "=" + url.QueryEscape(tracker.queryTime())
	}
	out := []Comment{}
	e := a.pages(ctx, p, path, func(raw []byte) error {
		var xs []rawComment
		if json.Unmarshal(raw, &xs) != nil {
			return &ClassifiedError{Class: ErrContractViolation, Summary: "invalid comment list"}
		}
		for _, x := range xs {
			comment := Comment{ID: strconv.FormatInt(x.ID, 10), Body: x.Body, CreatedAt: x.CreatedAt}
			newItem, e := tracker.add(comment.ID, comment.CreatedAt)
			if e != nil {
				return e
			}
			comment.Author = x.User.Login
			if a.Kind == KindGitLab {
				comment.Author = x.Author.Username
			}
			if newItem && comment.Author != "" {
				out = append(out, comment)
			}
		}
		return nil
	})
	return out, tracker.next(), e
}
func (a *Adapter) ListIssueComments(c context.Context, p ProjectRef, id string, s Cursor) ([]Comment, Cursor, error) {
	return a.listComments(c, p, TargetRef{Kind: TargetIssue, ID: id}, s)
}
func (a *Adapter) ListChangeComments(c context.Context, p ProjectRef, id string, s Cursor) ([]Comment, Cursor, error) {
	return a.listComments(c, p, TargetRef{Kind: TargetChange, ID: id}, s)
}

type rawChange struct {
	Number int    `json:"number"`
	IID    int    `json:"iid"`
	URL    string `json:"html_url"`
	WebURL string `json:"web_url"`
	State  string `json:"state"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
	DiffRefs struct {
		HeadSHA string `json:"head_sha"`
	} `json:"diff_refs"`
	MergedAt       *time.Time `json:"merged_at"`
	Mergeable      *bool      `json:"mergeable"`
	MergeableState string     `json:"mergeable_state"`
	MergeStatus    string     `json:"merge_status"`
	Detailed       string     `json:"detailed_merge_status"`
	Draft          bool       `json:"draft"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
}

func (a *Adapter) change(x rawChange) (Change, error) {
	id, url, sha := strconv.Itoa(x.Number), x.URL, x.Head.SHA
	state := ChangeOpen
	if a.Kind == KindGitLab {
		id, url, sha = strconv.Itoa(x.IID), x.WebURL, x.DiffRefs.HeadSHA
		switch x.State {
		case "opened":
		case "merged":
			state = ChangeMerged
		case "closed":
			state = ChangeClosed
		default:
			return Change{}, &ClassifiedError{Class: ErrContractViolation, Summary: "change has unknown state"}
		}
	} else {
		switch x.State {
		case "open":
		case "closed":
			state = ChangeClosed
		default:
			return Change{}, &ClassifiedError{Class: ErrContractViolation, Summary: "change has unknown state"}
		}
	}
	if x.MergedAt != nil {
		state = ChangeMerged
	}
	if id == "0" || url == "" || sha == "" {
		return Change{}, &ClassifiedError{Class: ErrContractViolation, Summary: "change missing required field"}
	}
	merge := MergeabilityUnknown
	if x.Mergeable != nil {
		if !*x.Mergeable {
			merge = Conflicting
		} else if x.MergeableState == "clean" || x.MergeableState == "" {
			merge = Mergeable
		}
	}
	if a.Kind == KindGitLab {
		switch x.Detailed {
		case "mergeable", "mergeable_status":
			merge = Mergeable
		case "conflict", "conflicting":
			merge = Conflicting
		}
		if merge == MergeabilityUnknown {
			switch x.MergeStatus {
			case "can_be_merged":
				merge = Mergeable
			case "cannot_be_merged":
				merge = Conflicting
			}
		}
	}
	draft := x.Draft
	if a.Kind == KindGitLab {
		draft = strings.HasPrefix(strings.ToLower(strings.TrimSpace(x.Title)), "draft:") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(x.Title)), "wip:")
	}
	c := Change{ID: id, URL: url, HeadSHA: sha, State: state, Mergeability: merge, ReviewState: ReviewUnknown, IsDraft: draft}
	if x.MergedAt != nil {
		c.MergedAt = *x.MergedAt
	}
	return c, nil
}

func (a *Adapter) reviewState(ctx context.Context, p ProjectRef, id string) (ReviewState, error) {
	if a.Kind == KindGitHub {
		var reviews []struct {
			State string `json:"state"`
		}
		if err := a.call(ctx, p, a.base(p)+"/pulls/"+pathPart(id)+"/reviews", "GET", nil, &reviews); err != nil {
			return ReviewUnknown, err
		}
		for _, r := range reviews {
			if r.State == "APPROVED" {
				return Approved, nil
			}
			if r.State != "CHANGES_REQUESTED" && r.State != "COMMENTED" && r.State != "DISMISSED" && r.State != "PENDING" {
				return ReviewUnknown, &ClassifiedError{Class: ErrContractViolation, Summary: "review has unknown state"}
			}
		}
		return NotApproved, nil
	}
	var approvals struct {
		ApprovedBy    []json.RawMessage `json:"approved_by"`
		ApprovalsLeft *int              `json:"approvals_left"`
	}
	if err := a.call(ctx, p, a.base(p)+"/merge_requests/"+pathPart(id)+"/approvals", "GET", nil, &approvals); err != nil {
		return ReviewUnknown, err
	}
	if len(approvals.ApprovedBy) > 0 {
		return Approved, nil
	}
	if approvals.ApprovalsLeft != nil {
		return NotApproved, nil
	}
	return ReviewUnknown, nil
}
func (a *Adapter) GetChange(ctx context.Context, p ProjectRef, id string) (Change, error) {
	return a.getChange(ctx, p, id, true)
}

func (a *Adapter) getChange(ctx context.Context, p ProjectRef, id string, fetchReview bool) (Change, error) {
	var x rawChange
	path := a.base(p) + "/pulls/" + pathPart(id)
	if a.Kind == KindGitLab {
		path = a.base(p) + "/merge_requests/" + pathPart(id)
	}
	if e := a.call(ctx, p, path, "GET", nil, &x); e != nil {
		return Change{}, e
	}
	c, e := a.change(x)
	if e != nil {
		return Change{}, e
	}
	if fetchReview {
		if review, err := a.reviewState(ctx, p, c.ID); err == nil {
			c.ReviewState = review
		} else if !errors.Is(err, ErrAuthOrCapability) {
			return Change{}, err
		}
	}
	return c, nil
}
func (a *Adapter) CreateChange(ctx context.Context, p ProjectRef, branch, base, title, body string) (Change, error) {
	path := a.base(p) + "/pulls"
	payload := map[string]any{"head": branch, "base": base, "title": title, "body": body, "draft": false}
	if a.Kind == KindGitLab {
		path = a.base(p) + "/merge_requests"
		payload = map[string]any{"source_branch": branch, "target_branch": base, "title": title, "description": body}
	}
	in, _ := json.Marshal(payload)
	var x rawChange
	if e := a.call(ctx, p, path, "POST", in, &x); e != nil {
		return Change{}, e
	}
	// GitLab can acknowledge before diff_refs.head_sha is populated.
	sha := x.Head.SHA
	id := x.Number
	if a.Kind == KindGitLab {
		sha, id = x.DiffRefs.HeadSHA, x.IID
	}
	if id == 0 {
		return Change{}, &ClassifiedError{Class: ErrContractViolation, Summary: "created change missing id"}
	}
	if sha == "" {
		return a.getChange(ctx, p, strconv.Itoa(id), false)
	}
	return a.change(x)
}
func (a *Adapter) FindChangeForCreateOperation(ctx context.Context, p ProjectRef, opKey, branch, base string) (*Change, FindResult, error) {
	if opKey == "" || branch == "" || base == "" {
		return nil, "", &ClassifiedError{Class: ErrContractViolation, Summary: "operation key, branch, and base are required"}
	}
	path := a.base(p) + "/pulls?state=all"
	if a.Kind == KindGitLab {
		path = a.base(p) + "/merge_requests?state=all"
	}
	if a.Kind == KindGitHub {
		owner, _, ok := strings.Cut(p.ProjectKey, "/")
		if !ok || owner == "" {
			return nil, "", &ClassifiedError{Class: ErrContractViolation, Summary: "github project key must be owner/repository"}
		}
		path += "&head=" + url.QueryEscape(owner+":"+branch) + "&base=" + url.QueryEscape(base)
	} else {
		path += "&source_branch=" + url.QueryEscape(branch) + "&target_branch=" + url.QueryEscape(base)
	}
	type candidate struct {
		change Change
		body   string
	}
	var candidates []candidate
	var allErr error
	allErr = a.pages(ctx, p, path, func(raw []byte) error {
		var xs []rawChange
		if json.Unmarshal(raw, &xs) != nil {
			return &ClassifiedError{Class: ErrContractViolation, Summary: "invalid change list"}
		}
		for _, x := range xs {
			c, e := a.change(x)
			if e != nil {
				return e
			}
			candidates = append(candidates, candidate{change: c, body: x.Body})
		}
		return nil
	})
	if allErr != nil {
		return nil, "", allErr
	}
	var marked []Change
	for _, c := range candidates {
		if strings.Contains(c.body, opKey) {
			marked = append(marked, c.change)
		}
	}
	if len(marked) > 1 {
		return nil, "", &ClassifiedError{Class: ErrSemanticConflict, Summary: "operation marker matched multiple changes"}
	}
	if len(marked) == 1 {
		return &marked[0], MarkerHit, nil
	}
	if len(candidates) > 0 {
		return nil, SemanticConflict, nil
	}
	return nil, NoMatch, nil
}
func (a *Adapter) GetChangeDiff(ctx context.Context, p ProjectRef, id string) (string, error) {
	path := a.base(p) + "/pulls/" + pathPart(id)
	if a.Kind == KindGitLab {
		path = a.base(p) + "/merge_requests/" + pathPart(id) + "/changes"
	}
	if a.Kind == KindGitHub {
		if err := a.chargeAPICall(ctx, p); err != nil {
			return "", err
		}
		out, stderr, err := a.Run(ctx, a.CLI, []string{"api", path, "--hostname", p.Host, "-H", "Accept: application/vnd.github.v3.diff"}, nil)
		if err != nil {
			return "", classify(string(stderr), err)
		}
		return string(out), nil
	}
	var x struct {
		Changes []struct {
			Diff string `json:"diff"`
		} `json:"changes"`
	}
	if e := a.call(ctx, p, path, "GET", nil, &x); e != nil {
		return "", e
	}
	var b strings.Builder
	for _, d := range x.Changes {
		b.WriteString(d.Diff)
	}
	return b.String(), nil
}
func (a *Adapter) GetChecks(ctx context.Context, p ProjectRef, sha string) (CheckSuite, error) {
	if a.Kind == KindGitLab {
		var ps []struct {
			ID     int64  `json:"id"`
			WebURL string `json:"web_url"`
			Status string `json:"status"`
		}
		path := a.base(p) + "/pipelines?sha=" + url.QueryEscape(sha) + "&order_by=id&sort=desc"
		if e := a.call(ctx, p, path, "GET", nil, &ps); e != nil {
			return CheckSuite{}, e
		}
		if len(ps) == 0 {
			return CheckSuite{Conclusion: "unknown"}, nil
		}
		var jobs []struct {
			Name         string `json:"name"`
			WebURL       string `json:"web_url"`
			Status       string `json:"status"`
			AllowFailure bool   `json:"allow_failure"`
		}
		if e := a.call(ctx, p, a.base(p)+"/pipelines/"+strconv.FormatInt(ps[0].ID, 10)+"/jobs", "GET", nil, &jobs); e != nil {
			return CheckSuite{}, e
		}
		result := normalizeCI(ps[0].Status)
		suite := CheckSuite{Conclusion: result, ExternalURL: ps[0].WebURL}
		for _, j := range jobs {
			if (j.Status == "failed" || j.Status == "canceled") && !j.AllowFailure {
				suite.FailedJobs = append(suite.FailedJobs, CheckJob{Name: j.Name, WebURL: j.WebURL, AllowFailure: j.AllowFailure})
				suite.Conclusion = "failure"
			}
		}
		if suite.Conclusion == "failure" && len(suite.FailedJobs) == 0 {
			suite.Conclusion = "success"
		}
		return suite, nil
	}
	var checks struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
			HTMLURL    string `json:"html_url"`
			DetailsURL string `json:"details_url"`
		} `json:"check_runs"`
	}
	if e := a.call(ctx, p, a.base(p)+"/commits/"+pathPart(sha)+"/check-runs", "GET", nil, &checks); e != nil {
		return CheckSuite{}, e
	}
	// /status returns a combined-status object, not a bare array.
	var statuses struct {
		State    string `json:"state"`
		Statuses []struct {
			State     string `json:"state"`
			Context   string `json:"context"`
			TargetURL string `json:"target_url"`
		} `json:"statuses"`
	}
	if e := a.call(ctx, p, a.base(p)+"/commits/"+pathPart(sha)+"/status", "GET", nil, &statuses); e != nil {
		return CheckSuite{}, e
	}
	suite := CheckSuite{Conclusion: "unknown"}
	seen := false
	add := func(conclusion string) {
		if conclusion == "" {
			conclusion = "pending"
		}
		if !seen || ciWorse(conclusion, suite.Conclusion) {
			suite.Conclusion = conclusion
		}
		seen = true
	}
	for _, r := range checks.CheckRuns {
		if r.DetailsURL != "" && suite.ExternalURL == "" {
			suite.ExternalURL = r.DetailsURL
		}
		switch r.Conclusion {
		case "failure", "cancelled", "timed_out", "action_required":
			add("failure")
			suite.FailedJobs = append(suite.FailedJobs, CheckJob{Name: r.Name, WebURL: r.HTMLURL})
		case "", "queued", "in_progress", "pending":
			add("pending")
		case "success", "neutral", "skipped":
			add("success")
		default:
			add("unknown")
		}
	}
	for _, s := range statuses.Statuses {
		if suite.ExternalURL == "" {
			suite.ExternalURL = s.TargetURL
		}
		switch s.State {
		case "failure", "error":
			add("failure")
		case "pending":
			add("pending")
		case "success":
			add("success")
		default:
			add("unknown")
		}
	}
	add(normalizeGitHubStatus(statuses.State))
	return suite, nil
}

func normalizeGitHubStatus(s string) string {
	switch s {
	case "failure", "error":
		return "failure"
	case "pending":
		return "pending"
	case "success":
		return "success"
	default:
		return "unknown"
	}
}

func ciWorse(candidate, current string) bool {
	rank := map[string]int{"success": 0, "pending": 1, "unknown": 2, "failure": 3}
	return rank[candidate] > rank[current]
}
func normalizeCI(s string) string {
	switch s {
	case "success", "passed":
		return "success"
	case "failed", "failure":
		return "failure"
	case "running", "pending", "created", "queued", "in_progress":
		return "pending"
	}
	return "unknown"
}
func (a *Adapter) MergeChange(ctx context.Context, p ProjectRef, id, expected, method string) (Change, error) {
	if !a.AutoMergeSupported(p) {
		return Change{}, &ClassifiedError{Class: ErrAuthOrCapability, Summary: "capability_unsupported: expected-head CAS is unproven"}
	}
	if a.capabilities != nil {
		enabled, err := a.capabilities.AutoMergeEnabled(ctx, p)
		if err != nil || !enabled {
			return Change{}, &ClassifiedError{Class: ErrAuthOrCapability, Summary: "capability_unsupported: persisted auto_merge capability is unavailable"}
		}
	}
	if id == "" || expected == "" {
		return Change{}, &ClassifiedError{Class: ErrContractViolation, Summary: "change id and expected head sha are required"}
	}
	if method != "merge" {
		return Change{}, &ClassifiedError{Class: ErrContractViolation, Summary: "only merge method is supported"}
	}
	path := a.base(p) + "/pulls/" + pathPart(id) + "/merge"
	payload := map[string]string{"sha": expected, "merge_method": "merge"}
	if a.Kind == KindGitLab {
		path = a.base(p) + "/merge_requests/" + pathPart(id) + "/merge"
		// GitLab has no merge_method equivalent. Its project configuration
		// selects the strategy, but sha remains the required CAS field.
		payload = map[string]string{"sha": expected}
	}
	in, _ := json.Marshal(payload)
	var response json.RawMessage
	if e := a.call(ctx, p, path, "PUT", in, &response); e != nil {
		if unsupportedCAS(e) {
			a.disableAutoMerge(p)
		}
		return Change{}, e
	}
	// GitHub's merge response is not a pull-request projection. Re-read the
	// Change rather than accepting a successful transport response as merge
	// evidence; this also gives both platforms the same neutral result.
	c, e := a.getChange(ctx, p, id, false)
	if e != nil {
		return Change{}, e
	}
	if c.State != ChangeMerged {
		return Change{}, &ClassifiedError{Class: ErrSemanticConflict, Summary: "merge response did not produce a merged change"}
	}
	return c, nil
}
func targetPath(a *Adapter, p ProjectRef, t TargetRef, suffix string) (string, error) {
	if t.ID == "" || (t.Kind != TargetIssue && t.Kind != TargetChange) {
		return "", &ClassifiedError{Class: ErrContractViolation, Summary: "invalid target"}
	}
	name := "issues"
	if a.Kind == KindGitLab && t.Kind == TargetChange {
		name = "merge_requests"
	}
	return a.base(p) + "/" + name + "/" + pathPart(t.ID) + suffix, nil
}
func (a *Adapter) ListLabelEvents(ctx context.Context, p ProjectRef, t TargetRef, since Cursor) ([]LabelEvent, Cursor, error) {
	tracker, err := newCursorTracker(since)
	if err != nil {
		return nil, "", err
	}
	path, e := targetPath(a, p, t, "")
	if e != nil {
		return nil, "", e
	}
	if a.Kind == KindGitHub {
		path += "/timeline"
	} else {
		path += "/resource_label_events"
	}
	if tracker.queryTime() != "" {
		key := "since"
		if a.Kind == KindGitLab {
			key = "created_after"
		}
		path += "?" + key + "=" + url.QueryEscape(tracker.queryTime())
	}
	out := []LabelEvent{}
	e = a.pages(ctx, p, path, func(raw []byte) error {
		var xs []struct {
			ID    int64 `json:"id"`
			Label struct {
				Name string `json:"name"`
			} `json:"label"`
			Actor struct {
				Login string `json:"login"`
			} `json:"actor"`
			User struct {
				Username string `json:"username"`
			} `json:"user"`
			Event   string    `json:"event"`
			Action  string    `json:"action"`
			Created time.Time `json:"created_at"`
		}
		if json.Unmarshal(raw, &xs) != nil {
			return &ClassifiedError{Class: ErrContractViolation, Summary: "invalid label event list"}
		}
		for _, x := range xs {
			newItem, e := tracker.add(strconv.FormatInt(x.ID, 10), x.Created)
			if e != nil {
				return e
			}
			actor := x.Actor.Login
			if a.Kind == KindGitLab {
				actor = x.User.Username
			}
			if actor == "" {
				continue
			}
			act := x.Action
			if act == "" {
				act = x.Event
			}
			if act == "labeled" || act == "add" {
				act = "added"
			} else if act == "unlabeled" || act == "remove" {
				act = "removed"
			} else {
				continue
			}
			if newItem {
				out = append(out, LabelEvent{TargetID: t.ID, Label: x.Label.Name, Action: LabelAction(act), Actor: actor, ObservedAt: x.Created})
			}
		}
		return nil
	})
	return out, tracker.next(), e
}
func (a *Adapter) CommentTarget(ctx context.Context, p ProjectRef, t TargetRef, body string) (string, error) {
	path, e := targetPath(a, p, t, "/comments")
	if a.Kind == KindGitLab {
		path, e = targetPath(a, p, t, "/notes")
	}
	if e != nil {
		return "", e
	}
	in, _ := json.Marshal(map[string]string{"body": body})
	var x struct {
		ID int64 `json:"id"`
	}
	if e = a.call(ctx, p, path, "POST", in, &x); e != nil {
		return "", e
	}
	if x.ID == 0 {
		return "", &ClassifiedError{Class: ErrContractViolation, Summary: "comment missing id"}
	}
	return strconv.FormatInt(x.ID, 10), nil
}
func (a *Adapter) SetLabels(ctx context.Context, p ProjectRef, t TargetRef, add, remove []string) error {
	path, e := targetPath(a, p, t, "")
	if e != nil {
		return e
	}
	add = sortDedupe(add)
	remove = sortDedupe(remove)
	for _, x := range add {
		for _, y := range remove {
			if x == y {
				return &ClassifiedError{Class: ErrContractViolation, Summary: "label in add and remove"}
			}
		}
	}
	if a.Kind == KindGitHub {
		for _, x := range add {
			in, _ := json.Marshal(map[string]string{"labels": x})
			if e = a.call(ctx, p, path+"/labels", "POST", in, nil); e != nil {
				return e
			}
		}
		for _, x := range remove {
			if e = a.call(ctx, p, path+"/labels/"+pathPart(x), "DELETE", nil, nil); e != nil {
				return e
			}
		}
		return a.verifyLabels(ctx, p, t, add, remove)
	}
	in, _ := json.Marshal(map[string]string{"add_labels": strings.Join(add, ","), "remove_labels": strings.Join(remove, ",")})
	if e = a.call(ctx, p, path, "PUT", in, nil); e != nil {
		return e
	}
	return a.verifyLabels(ctx, p, t, add, remove)
}

func (a *Adapter) verifyLabels(ctx context.Context, p ProjectRef, t TargetRef, add, remove []string) error {
	var x struct {
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	path, err := targetPath(a, p, t, "")
	if err != nil {
		return err
	}
	if err = a.call(ctx, p, path, "GET", nil, &x); err != nil {
		return err
	}
	got := map[string]bool{}
	for _, l := range x.Labels {
		got[l.Name] = true
	}
	for _, l := range add {
		if !got[l] {
			return &ClassifiedError{Class: ErrSemanticConflict, Summary: "label add not observed: " + l}
		}
	}
	for _, l := range remove {
		if got[l] {
			return &ClassifiedError{Class: ErrSemanticConflict, Summary: "label removal not observed: " + l}
		}
	}
	return nil
}

var _ Client = (*Adapter)(nil)
