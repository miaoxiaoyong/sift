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
func pathPart(s string) string {
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
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
