package forge

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"
)

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
