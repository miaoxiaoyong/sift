package forge

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

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
	MergeCommitSHA string     `json:"merge_commit_sha"`
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
	c := Change{ID: id, URL: url, HeadSHA: sha, MergeSHA: x.MergeCommitSHA, State: state, Mergeability: merge, ReviewState: ReviewUnknown, IsDraft: draft}
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
func (a *Adapter) FindChangeForCreateOperation(ctx context.Context, p ProjectRef, opKey, payloadDigest, branch, base string) (*Change, FindResult, error) {
	if opKey == "" || len(payloadDigest) != 64 || branch == "" || base == "" {
		return nil, "", &ClassifiedError{Class: ErrContractViolation, Summary: "operation key, payload digest, branch, and base are required"}
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
	allErr := a.pages(ctx, p, path, func(raw []byte) error {
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
		if FindOperationMarker(c.body, opKey, payloadDigest) {
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
