// Package forge defines the platform-neutral Forge port.
package forge

import (
	"context"
	"errors"
	"time"
)

type Kind string

const (
	KindGitHub Kind = "github"
	KindGitLab Kind = "gitlab"
)

type ProjectRef struct {
	Kind       Kind
	Host       string
	ProjectKey string
}
type TargetKind string

const (
	TargetIssue  TargetKind = "issue"
	TargetChange TargetKind = "change"
)

type TargetRef struct {
	Kind TargetKind
	ID   string
}
type IssueState string

const (
	IssueOpen   IssueState = "open"
	IssueClosed IssueState = "closed"
)

type Issue struct {
	ID, Title, Body, Author, URL string
	State                        IssueState
	Labels                       []string
}
type LabelAction string

const (
	LabelAdded   LabelAction = "added"
	LabelRemoved LabelAction = "removed"
)

type LabelEvent struct {
	TargetID, Label string
	Action          LabelAction
	Actor           string
	ObservedAt      time.Time
}
type ChangeState string

const (
	ChangeOpen   ChangeState = "open"
	ChangeMerged ChangeState = "merged"
	ChangeClosed ChangeState = "closed"
)

type Mergeability string

const (
	Mergeable           Mergeability = "mergeable"
	Conflicting         Mergeability = "conflicting"
	MergeabilityUnknown Mergeability = "unknown"
	ChangeMergeable     Mergeability = Mergeable
	ChangeConflicting   Mergeability = Conflicting
	ChangeMergeUnknown  Mergeability = MergeabilityUnknown
)

type ReviewState string

const (
	Approved          ReviewState = "approved"
	NotApproved       ReviewState = "not_approved"
	ReviewUnknown     ReviewState = "unknown"
	ReviewApproved    ReviewState = Approved
	ReviewNotApproved ReviewState = NotApproved
)

type Change struct {
	ID, URL, HeadSHA, MergeSHA string
	State                      ChangeState
	Mergeability               Mergeability
	ReviewState                ReviewState
	IsDraft                    bool
	MergedAt                   time.Time
}
type Comment struct {
	ID, Author, Body string
	CreatedAt        time.Time
}
type CheckJob struct {
	ID, Name, WebURL string
	AllowFailure     bool
}
type CheckSuite struct {
	Conclusion  string
	FailedJobs  []CheckJob
	ExternalURL string
}
type FindResult string

const (
	MarkerHit        FindResult = "marker_hit"
	NoMatch          FindResult = "no_match"
	SemanticConflict FindResult = "semantic_conflict"
)

// AutoMergeCapabilityReader supplies the persisted, per-project capability
// projection. It lets the merge boundary fail closed even if a caller forgot
// to carry a startup-probe result through its own policy code.
type AutoMergeCapabilityReader interface {
	AutoMergeEnabled(context.Context, ProjectRef) (bool, error)
}

// AutoMergeCapabilityRecorder is the startup write port for the durable
// capability projection. The caller supplies its configured project id.
type AutoMergeCapabilityRecorder interface {
	UpdateProjectAutoMergeCapability(context.Context, string, bool, string, int64) error
}

type Client interface {
	ListIssuesByLabel(context.Context, ProjectRef, string, Cursor) ([]Issue, Cursor, error)
	GetIssue(context.Context, ProjectRef, string) (Issue, error)
	ListIssueComments(context.Context, ProjectRef, string, Cursor) ([]Comment, Cursor, error)
	ListLabelEvents(context.Context, ProjectRef, TargetRef, Cursor) ([]LabelEvent, Cursor, error)
	CommentTarget(context.Context, ProjectRef, TargetRef, string) (string, error)
	SetLabels(context.Context, ProjectRef, TargetRef, []string, []string) error
	CreateChange(context.Context, ProjectRef, string, string, string, string) (Change, error)
	FindChangeForCreateOperation(context.Context, ProjectRef, string, string, string, string) (*Change, FindResult, error)
	GetChange(context.Context, ProjectRef, string) (Change, error)
	GetChangeDiff(context.Context, ProjectRef, string) (string, error)
	ListChangeComments(context.Context, ProjectRef, string, Cursor) ([]Comment, Cursor, error)
	GetChecks(context.Context, ProjectRef, string) (CheckSuite, error)
	MergeChange(context.Context, ProjectRef, string, string, string) (Change, error)
}

var (
	ErrTransient         = errors.New("forge: transient")
	ErrRateLimited       = errors.New("forge: rate limited")
	ErrAuthOrCapability  = errors.New("forge: auth or capability")
	ErrContractViolation = errors.New("forge: contract violation")
	ErrSemanticConflict  = errors.New("forge: semantic conflict")
)

type ClassifiedError struct {
	Class   error
	Summary string
	RetryAt time.Time
}

func (e *ClassifiedError) Error() string { return e.Class.Error() + ": " + e.Summary }
func (e *ClassifiedError) Unwrap() error { return e.Class }
