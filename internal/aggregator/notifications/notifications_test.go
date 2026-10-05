// Package notifications_test exercises the BFF /api/v1/notifications/*
// proxy aggregator that wires chora-gateway → chora-notifications per the
// SS dress-rehearsal v2 paydown (route was 404 prior to this aggregator).
//
// The aggregator proxies 4 BFF routes, stamps mesh-trust headers on outbound
// calls, applies a 5s per-call timeout, surfaces 502 on downstream 5xx, and
// 504 on upstream timeout.
package notifications_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/notifications"
)

// stubUpstream is a minimal recording test server.
type stubUpstream struct {
	*httptest.Server
	calls      atomic.Int64
	lastPath   string
	lastQuery  string
	lastMethod string
	lastAuth   string
	lastTenant string
	lastGCID   string
	lastTP     string
	lastBody   []byte
}

func newStub(t *testing.T, h http.HandlerFunc) *stubUpstream {
	t.Helper()
	s := &stubUpstream{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		s.lastPath = r.URL.Path
		s.lastQuery = r.URL.RawQuery
		s.lastMethod = r.Method
		s.lastAuth = r.Header.Get("Authorization")
		s.lastTenant = r.Header.Get("X-Tenant-Id")
		s.lastGCID = r.Header.Get("X-Chora-GCID")
		if s.lastGCID == "" {
			s.lastGCID = r.Header.Get("chora-gcid")
		}
		s.lastTP = r.Header.Get("traceparent")
		s.lastBody, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
		h(w, r)
	}))
	t.Cleanup(s.Server.Close)
	return s
}

func basicAuth() notifications.AuthCtx {
	return notifications.AuthCtx{
		Bearer:      "fb-tok",
		Traceparent: "00-aaa-bbb-01",
		TenantID:    "tenant-001",
		GCID:        "gcid-001",
	}
}

// -----------------------------------------------------------------------------
// LoadConfigFromEnv + New
// -----------------------------------------------------------------------------

func TestNew_NilWhenURLEmpty(t *testing.T) {
	if agg := notifications.New(notifications.Config{}); agg != nil {
		t.Fatalf("expected nil aggregator when SVC_NOTIFICATIONS_URL empty, got %#v", agg)
	}
}

func TestNew_AppliesDefaultTimeout(t *testing.T) {
	cfg := notifications.Config{NotificationsURL: "http://svc"}
	cfg.ApplyDefaults()
	if cfg.PerCallTimeout != notifications.DefaultPerCallTimeout {
		t.Fatalf("default timeout = %s, want %s",
			cfg.PerCallTimeout, notifications.DefaultPerCallTimeout)
	}
}

// -----------------------------------------------------------------------------
// List — GET /api/notifications (happy path 200 + mesh headers)
// -----------------------------------------------------------------------------

func TestList_HappyPath_200_StampsMeshHeaders(t *testing.T) {
	stub := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"id":"n1"}],"total":1}`))
	})
	agg := notifications.New(notifications.Config{
		NotificationsURL: stub.URL,
		PerCallTimeout:   1 * time.Second,
	})
	if agg == nil {
		t.Fatal("aggregator nil — expected wired with stub URL")
	}

	resp, _ := agg.List(context.Background(), basicAuth(), "recipient_gcid=gcid-001&channel=email")

	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", resp.Status, string(resp.Body))
	}
	if stub.lastPath != "/api/notifications" {
		t.Errorf("path=%s want /api/notifications", stub.lastPath)
	}
	if stub.lastQuery != "recipient_gcid=gcid-001&channel=email" {
		t.Errorf("query=%s want recipient_gcid=gcid-001&channel=email", stub.lastQuery)
	}
	if stub.lastMethod != http.MethodGet {
		t.Errorf("method=%s want GET", stub.lastMethod)
	}
	if stub.lastAuth != "Bearer fb-tok" {
		t.Errorf("Authorization=%s want Bearer fb-tok", stub.lastAuth)
	}
	if stub.lastTenant != "tenant-001" {
		t.Errorf("X-Tenant-Id=%s want tenant-001", stub.lastTenant)
	}
	if stub.lastGCID != "gcid-001" {
		t.Errorf("gcid header=%s want gcid-001", stub.lastGCID)
	}
	if stub.lastTP != "00-aaa-bbb-01" {
		t.Errorf("traceparent=%s want 00-aaa-bbb-01", stub.lastTP)
	}
	if !strings.Contains(string(resp.Body), `"items"`) {
		t.Errorf("body=%s missing items field", string(resp.Body))
	}
}

// -----------------------------------------------------------------------------
// Enqueue — POST /api/notifications (body passthrough)
// -----------------------------------------------------------------------------

func TestEnqueue_PassesBodyVerbatim_201(t *testing.T) {
	stub := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"n1","status":"queued"}`))
	})
	agg := notifications.New(notifications.Config{
		NotificationsURL: stub.URL,
		PerCallTimeout:   1 * time.Second,
	})

	body := []byte(`{"recipient_gcid":"gcid-001","channel":"email","template_id":"welcome","payload":{}}`)
	resp, _ := agg.Enqueue(context.Background(), basicAuth(), body)

	if resp.Status != http.StatusCreated {
		t.Fatalf("status=%d want 201", resp.Status)
	}
	if stub.lastMethod != http.MethodPost {
		t.Errorf("method=%s want POST", stub.lastMethod)
	}
	if string(stub.lastBody) != string(body) {
		t.Errorf("body=%s want %s", string(stub.lastBody), string(body))
	}
}

// -----------------------------------------------------------------------------
// Get — GET /api/notifications/{id}
// -----------------------------------------------------------------------------

func TestGet_PathEscapesID(t *testing.T) {
	stub := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"n1"}`))
	})
	agg := notifications.New(notifications.Config{
		NotificationsURL: stub.URL,
		PerCallTimeout:   1 * time.Second,
	})

	resp, _ := agg.Get(context.Background(), basicAuth(), "01970000-0000-7000-aaaa-bbbbbbbbbbbb")

	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.Status)
	}
	if stub.lastPath != "/api/notifications/01970000-0000-7000-aaaa-bbbbbbbbbbbb" {
		t.Errorf("path=%s want /api/notifications/<uuid>", stub.lastPath)
	}
}

// -----------------------------------------------------------------------------
// MarkRead — POST /api/notifications/mark-read
// -----------------------------------------------------------------------------

func TestMarkRead_RoutesToMarkReadEndpoint(t *testing.T) {
	stub := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"updated":2}`))
	})
	agg := notifications.New(notifications.Config{
		NotificationsURL: stub.URL,
		PerCallTimeout:   1 * time.Second,
	})

	body := []byte(`{"notification_ids":["n1","n2"]}`)
	resp, _ := agg.MarkRead(context.Background(), basicAuth(), body)

	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.Status)
	}
	if stub.lastPath != "/api/notifications/mark-read" {
		t.Errorf("path=%s want /api/notifications/mark-read", stub.lastPath)
	}
	if stub.lastMethod != http.MethodPost {
		t.Errorf("method=%s want POST", stub.lastMethod)
	}
}

// -----------------------------------------------------------------------------
// Upstream 5xx → 502
// -----------------------------------------------------------------------------

func TestList_Upstream500_Becomes502(t *testing.T) {
	stub := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`upstream-boom`))
	})
	agg := notifications.New(notifications.Config{
		NotificationsURL: stub.URL,
		PerCallTimeout:   1 * time.Second,
	})

	resp, _ := agg.List(context.Background(), basicAuth(), "")

	if resp.Status != http.StatusBadGateway {
		t.Fatalf("status=%d want 502 body=%s", resp.Status, string(resp.Body))
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_UPSTREAM_5XX") {
		t.Errorf("body=%s missing GATEWAY_UPSTREAM_5XX", string(resp.Body))
	}
}

// -----------------------------------------------------------------------------
// Upstream timeout → 504
// -----------------------------------------------------------------------------

func TestEnqueue_UpstreamTimeout_Becomes504(t *testing.T) {
	stub := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		// Sleep beyond the per-call timeout so the context deadline fires.
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	agg := notifications.New(notifications.Config{
		NotificationsURL: stub.URL,
		PerCallTimeout:   50 * time.Millisecond,
	})

	resp, _ := agg.Enqueue(context.Background(), basicAuth(), []byte(`{}`))

	if resp.Status != http.StatusGatewayTimeout {
		t.Fatalf("status=%d want 504 body=%s", resp.Status, string(resp.Body))
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_UPSTREAM_TIMEOUT") {
		t.Errorf("body=%s missing GATEWAY_UPSTREAM_TIMEOUT", string(resp.Body))
	}
}

// -----------------------------------------------------------------------------
// 4xx passthrough (e.g. 404 from downstream item lookup)
// -----------------------------------------------------------------------------

func TestGet_404Passthrough(t *testing.T) {
	stub := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"NOTIFICATIONS_NOT_FOUND","message":"resource not found"}`))
	})
	agg := notifications.New(notifications.Config{
		NotificationsURL: stub.URL,
		PerCallTimeout:   1 * time.Second,
	})

	resp, _ := agg.Get(context.Background(), basicAuth(), "missing-id")

	if resp.Status != http.StatusNotFound {
		t.Fatalf("status=%d want 404 (verbatim passthrough) body=%s",
			resp.Status, string(resp.Body))
	}
	if !strings.Contains(string(resp.Body), "NOTIFICATIONS_NOT_FOUND") {
		t.Errorf("body=%s should preserve downstream code", string(resp.Body))
	}
}
