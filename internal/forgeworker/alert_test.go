package forgeworker

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xsift/sift/internal/forge"
	"github.com/xsift/sift/internal/storage"
)

const alertNow = int64(1_700_000_000_000)

func enqueueChannelAlert(t *testing.T, db *storage.DB, targetKind string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"forge_kind": "github", "forge_host": "github.com", "forge_project_key": "org/repo",
		"target_kind": targetKind, "target_id": "42", "purpose": "channel_failure", "markdown": "[sift alert:channel_failure:delivery-1:1]",
	})
	if err != nil {
		t.Fatal(err)
	}
	key := "alert:channel_failure:delivery-1:1"
	if _, err := db.EnqueueOperation(context.Background(), storage.Operation{Key: key, Kind: storage.OperationForgeAlert, Payload: payload}, alertNow); err != nil {
		t.Fatal(err)
	}
	return key
}

type alertRecordingCharger struct {
	mu   sync.Mutex
	keys []string
}

func (c *alertRecordingCharger) Charge(_ context.Context, _ forge.ProjectRef, key string) (forge.ChargeResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keys = append(c.keys, key)
	return forge.ChargeResult{Charged: true, Limit: 10}, nil
}

func TestAlertWorkerProductionAdapterChargesStableAttemptKey(t *testing.T) {
	for _, targetKind := range []string{"issue", "change"} {
		t.Run(targetKind, func(t *testing.T) {
			db := openWorkerDB(t)
			enqueueChannelAlert(t, db, targetKind)
			charger := &alertRecordingCharger{}
			adapter, err := forge.NewProductionAdapter(forge.KindGitHub, "gh", func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
				if strings.Contains(strings.Join(args, " "), "--method POST") {
					return []byte(`{"id":1}`), nil, nil
				}
				return []byte(`[]`), nil, nil
			}, charger)
			if err != nil {
				t.Fatal(err)
			}
			worker := AlertWorker{DB: db, Client: adapter, WorkerID: "alert", Lease: time.Second, Now: func() time.Time { return time.UnixMilli(alertNow) }}
			if err := worker.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(charger.keys) != 2 {
				t.Fatalf("charged calls = %#v, want lookup and comment", charger.keys)
			}
			base := strings.TrimSuffix(charger.keys[0], ":1")
			if !regexp.MustCompile(`^forge-call:[0-9a-f]{32}$`).MatchString(base) || charger.keys[1] != base+":2" {
				t.Fatalf("charge keys = %#v, want one stable attempt base", charger.keys)
			}
		})
	}
}

type alertErrorClient struct {
	*forge.Fake
	err error
}

func (c alertErrorClient) ListIssueComments(context.Context, forge.ProjectRef, string, forge.Cursor) ([]forge.Comment, forge.Cursor, error) {
	return nil, "", c.err
}

func TestAlertWorkerClassifiesForgeFailures(t *testing.T) {
	now := time.UnixMilli(alertNow)
	for _, tc := range []struct {
		name  string
		err   error
		state storage.OperationState
		class storage.ErrorClass
	}{
		{"auth", &forge.ClassifiedError{Class: forge.ErrAuthOrCapability, Summary: "denied"}, storage.OperationFailed, storage.ErrorAuthCapability},
		{"contract", &forge.ClassifiedError{Class: forge.ErrContractViolation, Summary: "bad response"}, storage.OperationFailed, storage.ErrorContract},
		{"conflict", &forge.ClassifiedError{Class: forge.ErrSemanticConflict, Summary: "ambiguous marker"}, storage.OperationConflict, storage.ErrorSemanticConflict},
		{"rate limited", &forge.ClassifiedError{Class: forge.ErrRateLimited, Summary: "slow down", RetryAt: now.Add(time.Minute)}, storage.OperationRetryable, storage.ErrorRateLimited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openWorkerDB(t)
			enqueueChannelAlert(t, db, "issue")
			var outcome storage.CompleteOutcome
			worker := AlertWorker{DB: db, Client: alertErrorClient{Fake: forge.NewFake(), err: tc.err}, WorkerID: "alert", Lease: time.Second, Now: func() time.Time { return now }, Complete: func(_ context.Context, _ storage.ClaimedOperation, got storage.CompleteOutcome) error {
				outcome = got
				return nil
			}}
			if err := worker.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if outcome.State != tc.state || outcome.ErrorClass != tc.class {
				t.Fatalf("outcome = %#v, want %s/%s", outcome, tc.state, tc.class)
			}
			if tc.class == storage.ErrorRateLimited && outcome.RetryAfterMS != time.Minute.Milliseconds() {
				t.Fatalf("retry after = %d, want %d", outcome.RetryAfterMS, time.Minute.Milliseconds())
			}
		})
	}
}

func TestAlertWorkerRetryReclaimUsesDistinctStableChargeKeys(t *testing.T) {
	ctx := context.Background()
	db := openWorkerDB(t)
	enqueueChannelAlert(t, db, "change")
	charger := &alertRecordingCharger{}
	adapter, err := forge.NewProductionAdapter(forge.KindGitHub, "gh", func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		if strings.Contains(strings.Join(args, " "), "--method POST") {
			return []byte(`{"id":1}`), nil, nil
		}
		return []byte(`[]`), nil, nil
	}, charger)
	if err != nil {
		t.Fatal(err)
	}
	first := AlertWorker{DB: db, Client: adapter, WorkerID: "alert-1", Lease: time.Second, Now: func() time.Time { return time.UnixMilli(alertNow) }, Complete: func(context.Context, storage.ClaimedOperation, storage.CompleteOutcome) error {
		return errors.New("crash after remote delivery")
	}}
	if err := first.RunOnce(ctx); err == nil {
		t.Fatal("first completion must leave the attempt leased")
	}
	if len(charger.keys) != 2 {
		t.Fatalf("first attempt charge keys = %#v, want lookup and comment", charger.keys)
	}
	firstBase := strings.TrimSuffix(charger.keys[0], ":1")
	if charger.keys[1] != firstBase+":2" {
		t.Fatalf("first attempt charge keys = %#v, want stable sequence", charger.keys)
	}

	second := AlertWorker{DB: db, Client: adapter, WorkerID: "alert-2", Lease: time.Second, Now: func() time.Time { return time.UnixMilli(alertNow + 2_000) }}
	if err := second.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(charger.keys) != 4 {
		t.Fatalf("charge keys after reclaim = %#v, want lookup and comment", charger.keys)
	}
	secondBase := strings.TrimSuffix(charger.keys[2], ":1")
	if !regexp.MustCompile(`^forge-call:[0-9a-f]{32}$`).MatchString(secondBase) || secondBase == firstBase || charger.keys[3] != secondBase+":2" {
		t.Fatalf("reclaimed attempt charge key = %#v, want distinct stable attempt base", charger.keys)
	}
}

func TestAlertWorkerMarkerReplayDoesNotResend(t *testing.T) {
	ctx := context.Background()
	db := openWorkerDB(t)
	enqueueChannelAlert(t, db, "issue")
	ref := forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo"}
	client := &countingFake{Fake: forge.NewFake()}
	client.AddIssue(ref, forge.Issue{ID: "42", Title: "title", Author: "author", URL: "https://example.test/42"})
	crashed := false
	worker := AlertWorker{DB: db, Client: client, WorkerID: "alert", Lease: time.Second, Now: func() time.Time { return time.UnixMilli(alertNow) }, Complete: func(ctx context.Context, claim storage.ClaimedOperation, outcome storage.CompleteOutcome) error {
		if !crashed {
			crashed = true
			return errors.New("crash before local completion")
		}
		return db.CompleteOutboxAttempt(ctx, claim, outcome)
	}}
	if err := worker.RunOnce(ctx); err == nil {
		t.Fatal("first completion must simulate a crash")
	}
	if client.sentCount() != 1 {
		t.Fatalf("sends after crash = %d, want 1", client.sentCount())
	}
	worker.Now = func() time.Time { return time.UnixMilli(alertNow + 2_000) }
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if client.sentCount() != 1 {
		t.Fatalf("sends after marker replay = %d, want 1", client.sentCount())
	}
}
