// Package notifications_test — supplementary coverage for the routes the
// original suite left untouched: LoadConfigFromEnv, PushSubscriptions
// (POST + DELETE + raw query forwarding), and the classify error branch that
// maps a plain upstream failure (connection refused, DNS) to
// GATEWAY_UPSTREAM_ERROR rather than the timeout / 5xx envelopes.
package notifications_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/notifications"
)

func TestLoadConfigFromEnv_ReadsEnvVar(t *testing.T) {
	t.Setenv("SVC_NOTIFICATIONS_URL", "http://notifications.internal:8080")
	cfg := notifications.LoadConfigFromEnv()
	if cfg.NotificationsURL != "http://notifications.internal:8080" {
		t.Errorf("NotificationsURL = %q; want http://notifications.internal:8080", cfg.NotificationsURL)
	}
	if cfg.PerCallTimeout != notifications.DefaultPerCallTimeout {
		t.Errorf("PerCallTimeout = %s; want default %s", cfg.PerCallTimeout, notifications.DefaultPerCallTimeout)
	}
}

func TestLoadConfigFromEnv_EmptyWhenUnset(t *testing.T) {
	t.Setenv("SVC_NOTIFICATIONS_URL", "")
	cfg := notifications.LoadConfigFromEnv()
	if cfg.NotificationsURL != "" {
		t.Errorf("NotificationsURL = %q; want empty when unset", cfg.NotificationsURL)
	}
}

func TestPushSubscriptions_POSTForwardsBodyAndQuery(t *testing.T) {
	stub := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"registered":true}`))
	})
	agg := notifications.New(notifications.Config{
		NotificationsURL: stub.URL,
		PerCallTimeout:   1 * time.Second,
	})
	if agg == nil {
		t.Fatal("aggregator nil — expected wired with stub URL")
	}

	body := []byte(`{"token":"push-tok-1","platform":"web"}`)
	resp, err := agg.PushSubscriptions(context.Background(), basicAuth(), http.MethodPost, "", body)
	if err != nil {
		t.Fatalf("PushSubscriptions: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Fatalf("status=%d want 201", resp.Status)
	}
	if stub.lastPath != "/api/notifications/push-subscriptions" {
		t.Errorf("path=%s want /api/notifications/push-subscriptions", stub.lastPath)
	}
	if stub.lastMethod != http.MethodPost {
		t.Errorf("method=%s want POST", stub.lastMethod)
	}
	if string(stub.lastBody) != string(body) {
		t.Errorf("body=%s want %s", string(stub.lastBody), string(body))
	}
}

func TestPushSubscriptions_DELETEForwardsRawQueryNoBody(t *testing.T) {
	stub := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	agg := notifications.New(notifications.Config{
		NotificationsURL: stub.URL,
		PerCallTimeout:   1 * time.Second,
	})

	resp, err := agg.PushSubscriptions(context.Background(), basicAuth(), http.MethodDelete, "token=push-tok-1", []byte(`{"ignored":true}`))
	if err != nil {
		t.Fatalf("PushSubscriptions: %v", err)
	}
	if resp.Status != http.StatusNoContent {
		t.Fatalf("status=%d want 204", resp.Status)
	}
	if stub.lastMethod != http.MethodDelete {
		t.Errorf("method=%s want DELETE", stub.lastMethod)
	}
	if stub.lastQuery != "token=push-tok-1" {
		t.Errorf("query=%s want token=push-tok-1", stub.lastQuery)
	}
	// DELETE must not forward the POST body.
	if string(stub.lastBody) != "" {
		t.Errorf("body=%q; want empty for DELETE", string(stub.lastBody))
	}
}

func TestMarkRead_ForwardsBodyVerbatim(t *testing.T) {
	stub := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"updated":3}`))
	})
	agg := notifications.New(notifications.Config{
		NotificationsURL: stub.URL,
		PerCallTimeout:   1 * time.Second,
	})

	body := []byte(`{"notification_ids":["n1","n2","n3"]}`)
	resp, err := agg.MarkRead(context.Background(), basicAuth(), body)
	if err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.Status)
	}
	if string(stub.lastBody) != string(body) {
		t.Errorf("body=%s want %s", string(stub.lastBody), string(body))
	}
}

// TestList_UpstreamConnectionRefused_Becomes502 exercises the
// GATEWAY_UPSTREAM_ERROR classify branch — a plain network failure that is
// neither a deadline nor a 5xx response.
func TestList_UpstreamConnectionRefused_Becomes502(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	deadURL := dead.URL
	dead.Close()

	agg := notifications.New(notifications.Config{
		NotificationsURL: deadURL,
		PerCallTimeout:   1 * time.Second,
	})
	if agg == nil {
		t.Fatal("aggregator nil")
	}

	resp, err := agg.List(context.Background(), basicAuth(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if resp.Status != http.StatusBadGateway {
		t.Fatalf("status=%d want 502 body=%s", resp.Status, string(resp.Body))
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_UPSTREAM_ERROR") {
		t.Errorf("body=%s missing GATEWAY_UPSTREAM_ERROR", string(resp.Body))
	}
}

// TestEnqueue_4xxPassthrough documents that a downstream 4xx — here 422 —
// is forwarded verbatim (classify only rewrites errs + 5xx).
func TestEnqueue_4xxPassthrough(t *testing.T) {
	stub := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"code":"SUPPRESSED","message":"no channel"}}`))
	})
	agg := notifications.New(notifications.Config{
		NotificationsURL: stub.URL,
		PerCallTimeout:   1 * time.Second,
	})

	resp, err := agg.Enqueue(context.Background(), basicAuth(), []byte(`{"channel":"none"}`))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if resp.Status != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want 422 (verbatim passthrough)", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "SUPPRESSED") {
		t.Errorf("body=%s should preserve downstream error code", string(resp.Body))
	}
}

// TestList_2xxPassthroughPreservesHeaders ensures a downstream header set
// (e.g. X-RateLimit-Remaining) survives the proxy hop.
func TestList_2xxPassthroughPreservesHeaders(t *testing.T) {
	stub := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "42")
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	agg := notifications.New(notifications.Config{
		NotificationsURL: stub.URL,
		PerCallTimeout:   1 * time.Second,
	})

	resp, err := agg.List(context.Background(), basicAuth(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.Status)
	}
	if got := resp.Headers.Get("X-RateLimit-Remaining"); got != "42" {
		t.Errorf("header = %q; want 42", got)
	}
}

// TestList_CanceledParentContext_Becomes504RequestCanceled maps the
// context.Canceled classify branch: when the caller abandons the request the
// downstream call fails with `context canceled` and the aggregator must
// surface 504 GATEWAY_REQUEST_CANCELED rather than a generic 502.
func TestList_CanceledParentContext_Becomes504RequestCanceled(t *testing.T) {
	stub := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		// Sleep so the cancellation actually beats the response.
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	agg := notifications.New(notifications.Config{
		NotificationsURL: stub.URL,
		PerCallTimeout:   1 * time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	resp, err := agg.List(ctx, basicAuth(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if resp.Status != http.StatusGatewayTimeout {
		t.Fatalf("status=%d want 504 body=%s", resp.Status, string(resp.Body))
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_REQUEST_CANCELED") {
		t.Errorf("body=%s missing GATEWAY_REQUEST_CANCELED", string(resp.Body))
	}
}
