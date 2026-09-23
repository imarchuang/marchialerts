package notify_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"marchialerts/am"
	"marchialerts/notify"
)

var t0 = time.Unix(1_700_000_000, 0).UTC()

func firingAlert(host string) am.PostableAlert {
	return am.PostableAlert{
		Labels:   map[string]string{"alertname": "HighCPU", "team": "o11y", "host": host},
		StartsAt: t0,
		EndsAt:   t0.Add(time.Minute), // rolling: still firing
	}
}

func resolvedAlert(host string) am.PostableAlert {
	a := firingAlert(host)
	a.EndsAt = t0.Add(-time.Second) // in the past: resolved
	return a
}

// The payload is a GROUP: one POST carries alerts[] with per-alert status,
// labels, fingerprint, startsAt/endsAt (H7).
func TestWebhookPostsGroupPayload(t *testing.T) {
	var got notify.Payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %s", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh := notify.NewWebhook("hook", srv.URL)
	now := t0.Add(30 * time.Second)
	p := notify.BuildPayload("hook", `{alertname=HighCPU,team=o11y}`,
		[]am.PostableAlert{firingAlert("a"), resolvedAlert("b")}, now)

	if _, err := wh.Notify(context.Background(), p); err != nil {
		t.Fatal(err)
	}

	if got.Status != "firing" {
		t.Errorf("status = %q, want firing (any alert firing)", got.Status)
	}
	if got.GroupKey != `{alertname=HighCPU,team=o11y}` {
		t.Errorf("groupKey = %q", got.GroupKey)
	}
	if len(got.Alerts) != 2 {
		t.Fatalf("alerts = %d, want 2 (group, not single instance)", len(got.Alerts))
	}
	f, r := got.Alerts[0], got.Alerts[1]
	if f.Status != "firing" || f.Labels["host"] != "a" {
		t.Errorf("alerts[0] = %+v", f)
	}
	if f.Fingerprint == "" {
		t.Error("firing alert missing fingerprint")
	}
	if r.Status != "resolved" || r.Labels["host"] != "b" {
		t.Errorf("alerts[1] = %+v", r)
	}
	if r.EndsAt.IsZero() {
		t.Error("resolved alert must carry endsAt")
	}
}

// 500 → retried → 200 → exactly one success, more than one attempt.
func TestRetryOn500ThenSucceed(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh := notify.NewWebhook("hook", srv.URL)
	rt := notify.NewRetry(wh)
	rt.Sleep = func(context.Context, time.Duration) error { return nil } // no real waiting

	p := notify.BuildPayload("hook", "k", []am.PostableAlert{firingAlert("a")}, t0)
	if _, err := rt.Notify(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2 (500 then 200)", got)
	}
}

// 400 is permanent: give up immediately, no retry.
func TestNoRetryOn400(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	wh := notify.NewWebhook("hook", srv.URL)
	rt := notify.NewRetry(wh)
	rt.Sleep = func(context.Context, time.Duration) error { return nil }

	p := notify.BuildPayload("hook", "k", []am.PostableAlert{firingAlert("a")}, t0)
	if _, err := rt.Notify(context.Background(), p); err == nil {
		t.Fatal("expected error on 400")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1 (4xx is permanent)", got)
	}
}

// Persistent 5xx exhausts MaxAttempts and returns retryable=true so the AM
// knows the group must be retried at the next flush.
func TestRetryExhaustion(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	wh := notify.NewWebhook("hook", srv.URL)
	rt := notify.NewRetry(wh)
	rt.MaxAttempts = 3
	rt.Sleep = func(context.Context, time.Duration) error { return nil }

	p := notify.BuildPayload("hook", "k", []am.PostableAlert{firingAlert("a")}, t0)
	retryable, err := rt.Notify(context.Background(), p)
	if err == nil {
		t.Fatal("expected error after exhaustion")
	}
	if !retryable {
		t.Error("exhausted retry should report retryable=true")
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

// Fanout: one group, two adapters — both get the same payload.
func TestFanoutDeliversToAll(t *testing.T) {
	var aCalls, bCalls int
	mk := func(name string, count *int) notify.ContactPoint {
		return contactPointFunc{name: name, fn: func(notify.Payload) (bool, error) {
			*count++
			return false, nil
		}}
	}
	fo := notify.Fanout{mk("stdout", &aCalls), mk("hook", &bCalls)}

	if err := fo.Notify("k", []am.PostableAlert{firingAlert("a")}); err != nil {
		t.Fatal(err)
	}
	if aCalls != 1 || bCalls != 1 {
		t.Fatalf("fanout = stdout:%d hook:%d, want 1 each", aCalls, bCalls)
	}
}

// A failing adapter fails the fanout so the group is retried next flush.
func TestFanoutPropagatesError(t *testing.T) {
	bad := contactPointFunc{name: "hook", fn: func(notify.Payload) (bool, error) {
		return true, errors.New("boom")
	}}
	fo := notify.Fanout{bad}
	if err := fo.Notify("k", []am.PostableAlert{firingAlert("a")}); err == nil {
		t.Fatal("expected fanout to propagate the adapter error")
	}
}

type contactPointFunc struct {
	name string
	fn   func(notify.Payload) (bool, error)
}

func (c contactPointFunc) Name() string { return c.name }
func (c contactPointFunc) Notify(_ context.Context, p notify.Payload) (bool, error) {
	return c.fn(p)
}
