package forge

import (
	"context"
	"net/url"
	"strconv"
)

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
			ID           int64  `json:"id"`
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
				suite.FailedJobs = append(suite.FailedJobs, CheckJob{ID: strconv.FormatInt(j.ID, 10), Name: j.Name, WebURL: j.WebURL, AllowFailure: j.AllowFailure})
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
			ID         int64  `json:"id"`
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
			suite.FailedJobs = append(suite.FailedJobs, CheckJob{ID: strconv.FormatInt(r.ID, 10), Name: r.Name, WebURL: r.HTMLURL})
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

// RerunCheck verifies that the immutable remote check/job ID belongs to the
// frozen head, then invokes only that target's rerun endpoint. IDs are stable,
// so the mutation remains bound to the object whose head was just verified;
// there is no rerun-all fallback.
func (a *Adapter) RerunCheck(ctx context.Context, p ProjectRef, checkRunID, expectedHeadSHA string) error {
	if checkRunID == "" || expectedHeadSHA == "" {
		return &ClassifiedError{Class: ErrContractViolation, Summary: "check id and expected head sha are required"}
	}
	if a.Kind == KindGitHub {
		var check struct {
			ID      int64  `json:"id"`
			HeadSHA string `json:"head_sha"`
		}
		path := a.base(p) + "/check-runs/" + pathPart(checkRunID)
		if err := a.call(ctx, p, path, "GET", nil, &check); err != nil {
			return err
		}
		if strconv.FormatInt(check.ID, 10) != checkRunID || check.HeadSHA != expectedHeadSHA {
			return &ClassifiedError{Class: ErrSemanticConflict, Summary: "check run does not match expected head"}
		}
		return a.call(ctx, p, path+"/rerequest", "POST", []byte(`{}`), nil)
	}
	var job struct {
		ID       int64 `json:"id"`
		Pipeline struct {
			SHA string `json:"sha"`
		} `json:"pipeline"`
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	path := a.base(p) + "/jobs/" + pathPart(checkRunID)
	if err := a.call(ctx, p, path, "GET", nil, &job); err != nil {
		return err
	}
	head := job.Pipeline.SHA
	if head == "" {
		head = job.Commit.ID
	}
	if strconv.FormatInt(job.ID, 10) != checkRunID || head != expectedHeadSHA {
		return &ClassifiedError{Class: ErrSemanticConflict, Summary: "job does not match expected head"}
	}
	var retried struct {
		ID       int64 `json:"id"`
		Pipeline struct {
			SHA string `json:"sha"`
		} `json:"pipeline"`
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if err := a.call(ctx, p, path+"/retry", "POST", []byte(`{}`), &retried); err != nil {
		return err
	}
	retriedHead := retried.Pipeline.SHA
	if retriedHead == "" {
		retriedHead = retried.Commit.ID
	}
	if retriedHead != expectedHeadSHA {
		return &ClassifiedError{Class: ErrSemanticConflict, Summary: "retried job changed expected head"}
	}
	return nil
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
