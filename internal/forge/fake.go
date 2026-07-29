package forge

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type Fake struct {
	mu           sync.Mutex
	issues       map[string][]Issue
	issueTimes   map[string]time.Time
	events       map[string][]LabelEvent
	changes      map[string]map[string]Change
	changeBodies map[string]map[string]string
	comments     map[string][]Comment
	labels       map[string]map[string]map[string]bool
	diffs        map[string]string
}

func NewFake() *Fake {
	return &Fake{issues: map[string][]Issue{}, issueTimes: map[string]time.Time{}, events: map[string][]LabelEvent{}, changes: map[string]map[string]Change{}, changeBodies: map[string]map[string]string{}, comments: map[string][]Comment{}, labels: map[string]map[string]map[string]bool{}, diffs: map[string]string{}}
}
func projectKey(p ProjectRef) string { return string(p.Kind) + ":" + p.Host + ":" + p.ProjectKey }
func (f *Fake) AddIssue(p ProjectRef, i Issue) Issue {
	if i.ID == "" || i.Author == "" || i.URL == "" {
		panic("forge: fake issue missing required field")
	}
	i.Labels = sortDedupe(i.Labels)
	f.mu.Lock()
	k := projectKey(p)
	f.issues[k] = append(f.issues[k], i)
	f.issueTimes[k+"\x00"+i.ID] = time.Unix(int64(len(f.issues[k])), 0)
	f.mu.Unlock()
	return i
}
func (f *Fake) AddLabelEvent(p ProjectRef, e LabelEvent) {
	if e.Actor == "" || e.TargetID == "" || e.Label == "" {
		panic("forge: fake label event missing required field")
	}
	if e.ObservedAt.IsZero() {
		e.ObservedAt = time.Now()
	}
	f.mu.Lock()
	f.events[projectKey(p)+"\x00"+e.TargetID] = append(f.events[projectKey(p)+"\x00"+e.TargetID], e)
	f.mu.Unlock()
}
func (f *Fake) AddChange(p ProjectRef, id, sha string) Change {
	return f.AddChangeWithBody(p, id, sha, "")
}

// AddChangeWithBody scripts a remotely existing Change for marker-recovery
// tests. The body is remote evidence, not part of Change's neutral projection.
func (f *Fake) AddChangeWithBody(p ProjectRef, id, sha, body string) Change {
	c := Change{ID: id, HeadSHA: sha, State: ChangeOpen, Mergeability: MergeabilityUnknown, ReviewState: ReviewUnknown}
	f.mu.Lock()
	k := projectKey(p)
	if f.changes[k] == nil {
		f.changes[k] = map[string]Change{}
		f.changeBodies[k] = map[string]string{}
	}
	f.changes[k][id] = c
	f.changeBodies[k][id] = body
	f.mu.Unlock()
	return c
}
func (f *Fake) InjectMerged(p ProjectRef, id string, at time.Time) (Change, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, e := f.changeLocked(p, id)
	if e != nil {
		return Change{}, e
	}
	c.State = ChangeMerged
	c.MergeSHA = "merge-" + c.HeadSHA
	c.MergedAt = at
	f.changes[projectKey(p)][id] = c
	return c, nil
}
func (f *Fake) changeLocked(p ProjectRef, id string) (Change, error) {
	c, ok := f.changes[projectKey(p)][id]
	if !ok {
		return Change{}, &ClassifiedError{Class: ErrSemanticConflict, Summary: "unknown change " + id}
	}
	return c, nil
}
func (f *Fake) ListIssuesByLabel(_ context.Context, p ProjectRef, label string, since Cursor) ([]Issue, Cursor, error) {
	tracker, err := newCursorTracker(since)
	if err != nil {
		return nil, "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []Issue{}
	for _, i := range f.issues[projectKey(p)] {
		if !hasLabel(i.Labels, label) {
			continue
		}
		newItem, err := tracker.add(i.ID, f.issueTimes[projectKey(p)+"\x00"+i.ID])
		if err != nil {
			return nil, "", err
		}
		if newItem {
			out = append(out, i)
		}
	}
	return out, tracker.next(), nil
}
func fakeNumber(i int) string { return fmt.Sprintf("%d", i) }
func (f *Fake) GetIssue(_ context.Context, p ProjectRef, id string) (Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, i := range f.issues[projectKey(p)] {
		if i.ID == id {
			return i, nil
		}
	}
	return Issue{}, &ClassifiedError{Class: ErrSemanticConflict, Summary: "unknown issue " + id}
}
func (f *Fake) ListIssueComments(_ context.Context, p ProjectRef, id string, since Cursor) ([]Comment, Cursor, error) {
	return f.listComments(p, id, since)
}
func (f *Fake) ListChangeComments(_ context.Context, p ProjectRef, id string, since Cursor) ([]Comment, Cursor, error) {
	return f.listComments(p, id, since)
}
func (f *Fake) listComments(p ProjectRef, id string, since Cursor) ([]Comment, Cursor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tracker, err := newCursorTracker(since)
	if err != nil {
		return nil, "", err
	}
	out := []Comment{}
	for _, comment := range f.comments[projectKey(p)+"\x00"+id] {
		newItem, err := tracker.add(comment.ID, comment.CreatedAt)
		if err != nil {
			return nil, "", err
		}
		if newItem {
			out = append(out, comment)
		}
	}
	return out, tracker.next(), nil
}
func (f *Fake) ListLabelEvents(_ context.Context, p ProjectRef, t TargetRef, since Cursor) ([]LabelEvent, Cursor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tracker, err := newCursorTracker(since)
	if err != nil {
		return nil, "", err
	}
	out := []LabelEvent{}
	for n, event := range f.events[projectKey(p)+"\x00"+t.ID] {
		newItem, err := tracker.add(fakeNumber(n+1), event.ObservedAt)
		if err != nil {
			return nil, "", err
		}
		if newItem {
			out = append(out, event)
		}
	}
	return out, tracker.next(), nil
}
func (f *Fake) CommentTarget(_ context.Context, p ProjectRef, t TargetRef, body string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := projectKey(p) + "\x00" + t.ID
	id := fakeNumber(len(f.comments[k]) + 1)
	f.comments[k] = append(f.comments[k], Comment{ID: id, Author: "fake", Body: body, CreatedAt: time.Now()})
	return id, nil
}
func (f *Fake) SetLabels(_ context.Context, p ProjectRef, t TargetRef, add, remove []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := projectKey(p)
	if f.labels[k] == nil {
		f.labels[k] = map[string]map[string]bool{}
	}
	if f.labels[k][t.ID] == nil {
		f.labels[k][t.ID] = map[string]bool{}
	}
	for _, x := range add {
		f.labels[k][t.ID][x] = true
	}
	for _, x := range remove {
		delete(f.labels[k][t.ID], x)
	}
	return nil
}
func (f *Fake) CreateChange(_ context.Context, p ProjectRef, branch, base, title, body string) (Change, error) {
	id := fakeNumber(len(f.changes[projectKey(p)]) + 1)
	return f.AddChangeWithBody(p, id, branch, body), nil
}
func (f *Fake) FindChangeForCreateOperation(_ context.Context, p ProjectRef, op, branch, base string) (*Change, FindResult, error) {
	if op == "" || branch == "" || base == "" {
		return nil, "", &ClassifiedError{Class: ErrContractViolation, Summary: "operation key, branch, and base are required"}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	changes := f.changes[projectKey(p)]
	var marked []Change
	for id, c := range changes {
		if strings.Contains(f.changeBodies[projectKey(p)][id], op) {
			marked = append(marked, c)
		}
	}
	if len(marked) > 1 {
		return nil, "", &ClassifiedError{Class: ErrSemanticConflict, Summary: "operation marker matched multiple changes"}
	}
	if len(marked) == 1 {
		return &marked[0], MarkerHit, nil
	}
	if len(changes) > 0 {
		return nil, SemanticConflict, nil
	}
	return nil, NoMatch, nil
}
func (f *Fake) GetChange(_ context.Context, p ProjectRef, id string) (Change, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.changeLocked(p, id)
}
func (f *Fake) GetChangeDiff(_ context.Context, p ProjectRef, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.diffs[projectKey(p)+"\x00"+id], nil
}
func (f *Fake) GetChecks(context.Context, ProjectRef, string) (CheckSuite, error) {
	return CheckSuite{Conclusion: "unknown"}, nil
}
func (f *Fake) MergeChange(_ context.Context, p ProjectRef, id, expected, method string) (Change, error) {
	if method != "merge" {
		return Change{}, &ClassifiedError{Class: ErrContractViolation, Summary: "only merge supported"}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, e := f.changeLocked(p, id)
	if e != nil {
		return Change{}, e
	}
	if expected != "" && c.HeadSHA != expected {
		return Change{}, &ClassifiedError{Class: ErrSemanticConflict, Summary: "stale head"}
	}
	c.State = ChangeMerged
	c.MergeSHA = "merge-" + c.HeadSHA
	c.MergedAt = time.Now()
	f.changes[projectKey(p)][id] = c
	return c, nil
}
func sortDedupe(in []string) []string {
	m := map[string]bool{}
	for _, x := range in {
		m[x] = true
	}
	o := make([]string, 0, len(m))
	for x := range m {
		o = append(o, x)
	}
	sort.Strings(o)
	return o
}
func hasLabel(xs []string, w string) bool {
	for _, x := range xs {
		if x == w {
			return true
		}
	}
	return false
}

var _ Client = (*Fake)(nil)
