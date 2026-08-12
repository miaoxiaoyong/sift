package forge

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"time"
)

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
	Labels    []json.RawMessage `json:"labels"`
	UpdatedAt time.Time         `json:"updated_at"`
	Pull      *json.RawMessage  `json:"pull_request"`
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
	for _, raw := range x.Labels {
		var name string
		if json.Unmarshal(raw, &name) == nil {
			labels = append(labels, name)
		} else {
			var obj struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(raw, &obj) == nil {
				labels = append(labels, obj.Name)
			}
		}
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
