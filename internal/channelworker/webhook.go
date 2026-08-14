// Package channelworker contains the side-effecting consumers for Channel operations.
package channelworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/xsift/sift/internal/storage"
)

var (
	ErrAuthOrCapability  = errors.New("channel: auth or capability")
	ErrContractViolation = errors.New("channel: contract violation")
	ErrTransient         = errors.New("channel: transient")
	ErrRateLimited       = errors.New("channel: rate limited")
)

type RateLimitedError struct{ RetryAfterMS int64 }

func (e RateLimitedError) Error() string { return ErrRateLimited.Error() }
func (e RateLimitedError) Unwrap() error { return ErrRateLimited }

type SecretResolver interface {
	Resolve(context.Context, string) (string, error)
}
type WebhookSender interface {
	Send(context.Context, string, string) (string, error)
}

type webhookPayload struct {
	DeliveryKind     string `json:"delivery_kind"`
	DeliveryID       string `json:"delivery_id"`
	InterruptID      string `json:"interrupt_id"`
	EscalationNo     int    `json:"escalation_no"`
	Priority         string `json:"priority"`
	InterruptVersion int    `json:"interrupt_version"`
	Nonce            string `json:"nonce"`
	BatchID          string `json:"batch_id"`
	BatchKind        string `json:"batch_kind"`
	ProjectID        string `json:"project_id"`
	Scope            string `json:"scope"`
	ScopeID          string `json:"scope_id"`
	DueAtMS          int64  `json:"due_at_ms"`
	Channel          struct {
		ID           string   `json:"id"`
		Type         string   `json:"type"`
		TargetRef    string   `json:"target_ref"`
		Renderer     string   `json:"renderer"`
		Capabilities []string `json:"capabilities"`
	} `json:"channel"`
	ForgeAlertTarget struct {
		ForgeKind       string `json:"forge_kind"`
		ForgeHost       string `json:"forge_host"`
		ForgeProjectKey string `json:"forge_project_key"`
		TargetKind      string `json:"target_kind"`
		TargetID        string `json:"target_id"`
	} `json:"forge_alert_target"`
	RenderedText string            `json:"rendered_text"`
	Members      []json.RawMessage `json:"members"`
}

type batchMember struct {
	DeliveryID       string            `json:"delivery_id"`
	InterruptID      string            `json:"interrupt_id"`
	InterruptVersion int               `json:"interrupt_version"`
	Nonce            string            `json:"nonce"`
	Headline         string            `json:"headline"`
	Reason           string            `json:"reason"`
	Severity         string            `json:"severity"`
	Links            []json.RawMessage `json:"links"`
	Options          []json.RawMessage `json:"options"`
	CommandLines     []string          `json:"command_lines"`
}

func decodeClosed(raw []byte, dst any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func required(raw []byte, names ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	for _, name := range names {
		if value, ok := fields[name]; !ok || string(value) == "null" {
			return false
		}
	}
	return true
}

func only(raw []byte, names ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	allowed := make(map[string]bool, len(names))
	for _, name := range names {
		allowed[name] = true
	}
	for name := range fields {
		if !allowed[name] {
			return false
		}
	}
	return true
}

func validChannel(p webhookPayload) bool {
	if p.DeliveryID == "" || p.Channel.ID == "" || p.Channel.Type != "webhook" || p.Channel.Renderer != "plain-v1" || len(p.Channel.Capabilities) == 0 {
		return false
	}
	const prefix = "secret_ref:"
	return strings.HasPrefix(p.Channel.TargetRef, prefix) && len(p.Channel.TargetRef) > len(prefix) && !strings.ContainsAny(p.Channel.TargetRef[len(prefix):], "\r\n")
}

// EnvironmentSecretResolver is the small production resolver used by the V0
// daemon. The payload contains only the handle; the resolved value never
// crosses the storage boundary.
type EnvironmentSecretResolver struct{}

func (EnvironmentSecretResolver) Resolve(_ context.Context, name string) (string, error) {
	if value, ok := os.LookupEnv(name); ok {
		return value, nil
	}
	if value, ok := os.LookupEnv("SIFT_SECRET_" + name); ok {
		return value, nil
	}
	return "", ErrAuthOrCapability
}

type HTTPWebhookSender struct {
	Client *http.Client
	// Now is injectable so HTTP-date Retry-After remains deterministic in tests.
	Now func() time.Time
}

func (s HTTPWebhookSender) Send(ctx context.Context, endpoint, body string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return "", ErrContractViolation
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", ErrTransient
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		var retryAfter int64
		if value := resp.Header.Get("Retry-After"); value != "" {
			value = strings.TrimSpace(value)
			if seconds, parseErr := strconv.ParseInt(value, 10, 64); parseErr == nil && seconds >= 0 {
				retryAfter = seconds * 1000
			} else {
				now := time.Now()
				if s.Now != nil {
					now = s.Now()
				}
				if when, parseErr := http.ParseTime(value); parseErr == nil && when.After(now) {
					retryAfter = when.Sub(now).Milliseconds()
				}
			}
		}
		return "", RateLimitedError{RetryAfterMS: retryAfter}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", ErrAuthOrCapability
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", ErrTransient
	}
	return resp.Status, nil
}

// WebhookAdapter executes exactly one sealed attempt. Errors crossing this
// boundary are classifications only: sender text can contain endpoint secrets.
type WebhookAdapter struct {
	Resolver SecretResolver
	Sender   WebhookSender
}

func (a WebhookAdapter) Publish(ctx context.Context, payload []byte, operationKey string) (json.RawMessage, error) {
	var p webhookPayload
	if err := decodeClosed(payload, &p); err != nil || !required(payload, "delivery_kind", "delivery_id", "channel", "rendered_text") || !validChannel(p) {
		return nil, fmt.Errorf("%w: closed channel_publish payload", ErrContractViolation)
	}
	if p.DeliveryKind == "interrupt" {
		if !only(payload, "delivery_kind", "delivery_id", "interrupt_id", "escalation_no", "priority", "interrupt_version", "nonce", "channel", "rendered_text") || !required(payload, "interrupt_id", "escalation_no", "interrupt_version", "nonce") || p.InterruptID == "" || p.Nonce == "" || p.DeliveryID != fmt.Sprintf("interrupt:%s:%d:%s", p.InterruptID, p.EscalationNo, p.Channel.ID) {
			return nil, fmt.Errorf("%w: interrupt payload", ErrContractViolation)
		}
	} else if p.DeliveryKind == "attention_batch" {
		if !only(payload, "delivery_kind", "delivery_id", "batch_id", "batch_kind", "channel", "project_id", "forge_alert_target", "scope", "scope_id", "due_at_ms", "members", "rendered_text") || !required(payload, "batch_id", "batch_kind", "project_id", "scope", "scope_id", "due_at_ms", "forge_alert_target", "members") || p.BatchID == "" || (p.BatchKind != "daily_summary" && p.BatchKind != "critical_fused") || (p.Scope != "day" && p.Scope != "global" && p.Scope != "run") || p.ProjectID == "" || p.ForgeAlertTarget.ForgeKind == "" || (p.ForgeAlertTarget.ForgeKind != "github" && p.ForgeAlertTarget.ForgeKind != "gitlab") || p.ForgeAlertTarget.ForgeHost == "" || p.ForgeAlertTarget.ForgeProjectKey == "" || (p.ForgeAlertTarget.TargetKind != "issue" && p.ForgeAlertTarget.TargetKind != "change") || p.ForgeAlertTarget.TargetID == "" || len(p.Members) == 0 {
			return nil, fmt.Errorf("%w: batch payload", ErrContractViolation)
		}
		last := ""
		if p.DeliveryID != p.BatchID+":publish:1" {
			return nil, fmt.Errorf("%w: batch delivery identity", ErrContractViolation)
		}
		for _, raw := range p.Members {
			var member batchMember
			if err := decodeClosed(raw, &member); err != nil || !only(raw, "delivery_id", "interrupt_id", "interrupt_version", "nonce", "headline", "reason", "severity", "links", "options", "command_lines") || !required(raw, "delivery_id", "interrupt_id", "interrupt_version", "nonce", "headline", "reason", "severity", "links", "options", "command_lines") || member.DeliveryID != p.BatchID+":"+member.InterruptID || member.InterruptID == "" || member.Nonce == "" || member.InterruptID <= last || (member.Severity != "low" && member.Severity != "normal" && member.Severity != "high" && member.Severity != "critical") || (member.Reason != "agent_blocked" && member.Reason != "code_review" && member.Reason != "failure_review" && member.Reason != "startup_stall" && member.Reason != "merge_conflict") {
				return nil, fmt.Errorf("%w: batch member", ErrContractViolation)
			}
			last = member.InterruptID
		}
	} else {
		return nil, fmt.Errorf("%w: delivery kind", ErrContractViolation)
	}
	if a.Resolver == nil {
		return nil, ErrAuthOrCapability
	}
	endpoint, err := a.Resolver.Resolve(ctx, strings.TrimPrefix(p.Channel.TargetRef, "secret_ref:"))
	if err != nil {
		return nil, fmt.Errorf("%w: resolver rejected handle", ErrAuthOrCapability)
	}
	parsed, err := url.Parse(endpoint)
	if endpoint == "" || strings.ContainsAny(endpoint, "\r\n") || err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("%w: resolved endpoint", ErrContractViolation)
	}
	if a.Sender == nil {
		return nil, ErrAuthOrCapability
	}
	remote, err := a.Sender.Send(ctx, endpoint, p.RenderedText+"\n[sift "+operationKey+"]")
	if err != nil {
		switch {
		case errors.Is(err, ErrRateLimited):
			return nil, err
		case errors.Is(err, ErrAuthOrCapability):
			return nil, ErrAuthOrCapability
		case errors.Is(err, ErrContractViolation):
			return nil, ErrContractViolation
		}
		return nil, ErrTransient
	}
	return json.RawMessage(fmt.Sprintf(`{"remote_ref":%q}`, remote)), nil
}

type Worker struct {
	DB          *storage.DB
	Adapter     WebhookAdapter
	Now         func() int64
	LeaseMS     int64
	WorkerID    string
	AlertAfter  int
	Backoff     storage.BackoffPolicy
	MaxAttempts int
}

func (w *Worker) RunOnce(ctx context.Context) error {
	now := int64(1)
	if w.Now != nil {
		now = w.Now()
	}
	claim, err := w.DB.ClaimOutboxOperationKind(ctx, w.WorkerID, storage.OperationChannelPublish, now, w.LeaseMS)
	if err != nil || claim == nil {
		return err
	}
	evidence, err := w.Adapter.Publish(ctx, claim.Payload, claim.Key)
	outcome := storage.CompleteOutcome{NowMS: now, ChannelFailureAlertAfter: w.AlertAfter, MaxAttempts: w.MaxAttempts}
	if err == nil {
		outcome.State, outcome.Evidence = storage.OperationSucceeded, evidence
		return w.DB.CompleteOutboxAttempt(ctx, *claim, outcome)
	}
	outcome.State, outcome.ErrorClass, outcome.ErrorSummary, outcome.Backoff = storage.OperationRetryable, storage.ErrorTransient, "channel publish failed", w.Backoff
	if outcome.Backoff.InitialDelayMS == 0 {
		outcome.Backoff = storage.BackoffPolicy{InitialDelayMS: 1000, MaxDelayMS: 60000, Multiplier: 2}
	}
	switch {
	case errors.Is(err, ErrAuthOrCapability):
		outcome.State, outcome.ErrorClass, outcome.ErrorSummary = storage.OperationFailed, storage.ErrorAuthCapability, "channel authentication or capability failure"
	case errors.Is(err, ErrContractViolation):
		outcome.State, outcome.ErrorClass, outcome.ErrorSummary = storage.OperationFailed, storage.ErrorContract, "channel payload or endpoint contract violation"
	case errors.Is(err, ErrRateLimited):
		outcome.ErrorClass, outcome.ErrorSummary = storage.ErrorRateLimited, "channel rate limited"
		var limited RateLimitedError
		if errors.As(err, &limited) {
			outcome.RetryAfterMS = limited.RetryAfterMS
		}
	}
	return w.DB.CompleteOutboxAttempt(ctx, *claim, outcome)
}
