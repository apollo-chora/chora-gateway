// notifications_handler_test.go — HTTP route binding tests for the BFF
// /api/v1/notifications/* proxy that fans out to chora-notifications.
//
// SS dress-rehearsal v2 paydown (route was 404 prior). Tests cover the
// 200/401/502/504 acceptance gates from the task scope plus the
// route-not-mounted (404 passthrough) edge.
package httpadapter_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/notifications"
)

func newNotificationsStub(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func newNotificationsBridgeHandler(t *testing.T, stub *httptest.Server, timeout time.Duration) http.Handler {
	t.Helper()
	if timeout == 0 {
		timeout = 1 * time.Second
	}
	agg := notifications.New(notifications.Config{
		NotificationsURL: stub.URL,
		PerCallTimeout:   timeout,
	})
	if agg == nil {
		t.Fatal("aggregator nil — stub URL should have wired it")
	}
	return httpadapter.NewNotificationsMux(agg)
}

func doNotifReq(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequestWithContext(context.Background(), method, path, nil)
	}
	r.Header.Set("Authorization", "Bearer testtoken")
	r.Header.Set("X-Tenant-Id", "tenant-001")
	// Stamp MeshClaims into context the way RequireChoraSessionJWT would.
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// -----------------------------------------------------------------------------
// 200 — happy path GET /api/v1/notifications (list)
// -----------------------------------------------------------------------------

func TestNotifications_List_200_Passthrough(t *testing.T) {
	stub := newNotificationsStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/notifications" {
			t.Errorf("stub path=%s want /api/notifications", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("stub method=%s want GET", r.Method)
		}
		// Mesh-claims must reach the downstream.
		if r.Header.Get("chora-gcid") != "gcid-001" && r.Header.Get("X-Chora-GCID") != "gcid-001" {
			t.Errorf("missing GCID header: %v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"id":"n1"}],"total":1}`))
	})
	h := newNotificationsBridgeHandler(t, stub, 0)

	w := doNotifReq(t, h, http.MethodGet, "/api/v1/notifications?recipient_gcid=gcid-001", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"items"`) {
		t.Errorf("body=%s missing items", w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type=%s want application/json", got)
	}
}

// -----------------------------------------------------------------------------
// 200 — POST /api/v1/notifications (enqueue)
// -----------------------------------------------------------------------------

func TestNotifications_Enqueue_201_BodyPassthrough(t *testing.T) {
	stub := newNotificationsStub(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if !strings.Contains(string(body), `"recipient_gcid"`) {
			t.Errorf("downstream body=%s missing recipient_gcid", string(body))
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"n1","status":"queued"}`))
	})
	h := newNotificationsBridgeHandler(t, stub, 0)

	w := doNotifReq(t, h, http.MethodPost, "/api/v1/notifications",
		`{"recipient_gcid":"gcid-001","channel":"email","template_id":"welcome","payload":{}}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d want 201", w.Code)
	}
}

// -----------------------------------------------------------------------------
// 200 — POST /api/v1/notifications/mark-read
// -----------------------------------------------------------------------------

func TestNotifications_MarkRead_200(t *testing.T) {
	stub := newNotificationsStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/notifications/mark-read" {
			t.Errorf("stub path=%s want /api/notifications/mark-read", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("stub method=%s want POST", r.Method)
		}
		_, _ = w.Write([]byte(`{"updated":2}`))
	})
	h := newNotificationsBridgeHandler(t, stub, 0)

	w := doNotifReq(t, h, http.MethodPost, "/api/v1/notifications/mark-read",
		`{"notification_ids":["n1","n2"]}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------------------------
// 502 — upstream 5xx
// -----------------------------------------------------------------------------

func TestNotifications_Upstream5xx_Becomes502(t *testing.T) {
	stub := newNotificationsStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	})
	h := newNotificationsBridgeHandler(t, stub, 0)

	w := doNotifReq(t, h, http.MethodGet, "/api/v1/notifications", "")

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want 502 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "GATEWAY_UPSTREAM_5XX") {
		t.Errorf("body=%s missing GATEWAY_UPSTREAM_5XX", w.Body.String())
	}
}

// -----------------------------------------------------------------------------
// 504 — upstream timeout
// -----------------------------------------------------------------------------

func TestNotifications_UpstreamTimeout_Becomes504(t *testing.T) {
	stub := newNotificationsStub(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	h := newNotificationsBridgeHandler(t, stub, 50*time.Millisecond)

	w := doNotifReq(t, h, http.MethodPost, "/api/v1/notifications", `{}`)

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d want 504 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "GATEWAY_UPSTREAM_TIMEOUT") {
		t.Errorf("body=%s missing GATEWAY_UPSTREAM_TIMEOUT", w.Body.String())
	}
}

// -----------------------------------------------------------------------------
// 401 — JWT prefix gate rejects missing token on /api/v1/notifications
// -----------------------------------------------------------------------------

// /api/v1/notifications is listed in DefaultJWTGatedPrefixes; an unauthenticated
// caller is rejected by WithChoraSessionOnPrefixes before reaching the bridge.
func TestNotifications_DefaultJWTGate_IncludesPrefix(t *testing.T) {
	found := false
	for _, p := range httpadapter.DefaultJWTGatedPrefixes {
		if p == "/api/v1/notifications" || p == httpadapter.NotificationsBridgePathPrefix {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("DefaultJWTGatedPrefixes does not include /api/v1/notifications — JWT gate would let unauthenticated traffic through")
	}
}

// -----------------------------------------------------------------------------
// 405 — wrong method on collection
// -----------------------------------------------------------------------------

func TestNotifications_Collection_WrongMethod_405(t *testing.T) {
	stub := newNotificationsStub(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("downstream should not be called on 405")
	})
	h := newNotificationsBridgeHandler(t, stub, 0)

	w := doNotifReq(t, h, http.MethodDelete, "/api/v1/notifications", "")

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want 405", w.Code)
	}
}

// -----------------------------------------------------------------------------
// GET /api/v1/notifications/{id} — proxies item lookup
// -----------------------------------------------------------------------------

func TestNotifications_GetByID_200(t *testing.T) {
	stub := newNotificationsStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/notifications/01970000-0000-7000-aaaa-bbbbbbbbbbbb" {
			t.Errorf("stub path=%s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":"01970000-0000-7000-aaaa-bbbbbbbbbbbb"}`))
	})
	h := newNotificationsBridgeHandler(t, stub, 0)

	w := doNotifReq(t, h, http.MethodGet,
		"/api/v1/notifications/01970000-0000-7000-aaaa-bbbbbbbbbbbb", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}
}

// -----------------------------------------------------------------------------
// WithNotificationsBridge composition: passthrough when aggregator nil
// -----------------------------------------------------------------------------

func TestWithNotificationsBridge_NilAggregator_Passthrough(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	composed := httpadapter.WithNotificationsBridge(base, nil)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	w := httptest.NewRecorder()
	composed.ServeHTTP(w, r)

	if w.Code != http.StatusTeapot {
		t.Errorf("nil aggregator should pass through to base — got %d", w.Code)
	}
}

// -----------------------------------------------------------------------------
// WithNotificationsBridge composition: routes /api/v1/notifications/* to mux,
// non-bridge paths fall through to base
// -----------------------------------------------------------------------------

func TestWithNotificationsBridge_RoutesBridgePaths(t *testing.T) {
	stub := newNotificationsStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	})
	agg := notifications.New(notifications.Config{
		NotificationsURL: stub.URL,
		PerCallTimeout:   1 * time.Second,
	})

	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Should ONLY be invoked for non-bridge paths.
		w.WriteHeader(http.StatusTeapot)
	})
	composed := httpadapter.WithNotificationsBridge(base, agg)

	// Bridge path — handled by mux, expect 200.
	r1 := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	r1 = r1.WithContext(httpadapter.InjectMeshClaimsForTest(r1.Context(), "gcid-001", "tenant-001"))
	w1 := httptest.NewRecorder()
	composed.ServeHTTP(w1, r1)
	if w1.Code != http.StatusOK {
		t.Errorf("bridge path: status=%d want 200", w1.Code)
	}

	// Non-bridge path — passes through to base.
	r2 := httptest.NewRequest(http.MethodGet, "/api/v1/something-else", nil)
	w2 := httptest.NewRecorder()
	composed.ServeHTTP(w2, r2)
	if w2.Code != http.StatusTeapot {
		t.Errorf("non-bridge path: status=%d want 418 (base)", w2.Code)
	}
}
