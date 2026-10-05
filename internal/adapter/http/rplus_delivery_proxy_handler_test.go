// rplus_delivery_proxy_handler_test.go — HTTP route binding tests for the
// R+ (Rhythm+) surface BFF proxy bridge. Add-only file; the existing
// gatewayproxy_handler_test.go is not touched.
//
// Coverage:
//   - One happy-path per resource group (bookings / certifications /
//     campus / me-applications) asserts that the handler forwards method
//   - path + body verbatim to chora-delivery.
//   - WithRplusDeliveryProxy composition: bridge-owned paths route through
//     the bridge; non-bridge paths fall through to the base handler.
//   - matchesRplusDeliveryProxyPath ownership matrix.
//   - DefaultJWTGatedPrefixes covers every R+ delivery proxy prefix.
//
// Strict TDD per feedback_strict_tdd.
package httpadapter_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

func newRplusMux(t *testing.T, stub *httptest.Server) http.Handler {
	t.Helper()
	agg := gatewayproxy.New(gatewayproxy.Config{
		DeliveryURL:    stub.URL,
		PerCallTimeout: time.Second,
	})
	if agg == nil {
		t.Fatal("aggregator nil — DeliveryURL should have wired it")
	}
	return httpadapter.NewRplusDeliveryProxyMux(agg)
}

func doRplusReq(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequestWithContext(context.Background(), method, path, nil)
	}
	r.Header.Set("Authorization", "Bearer testtoken")
	r.Header.Set("X-Tenant-Id", "tenant-001")
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// -----------------------------------------------------------------------------
// /api/bookings
// -----------------------------------------------------------------------------

func TestRplusProxy_Bookings_GET_List(t *testing.T) {
	var gotPath, gotMethod string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	}))
	defer stub.Close()
	h := newRplusMux(t, stub)

	w := doRplusReq(t, h, http.MethodGet, "/api/bookings", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", w.Code, w.Body.String())
	}
	if gotMethod != http.MethodGet || gotPath != "/api/bookings" {
		t.Errorf("downstream method/path = %s %q; want GET /api/bookings", gotMethod, gotPath)
	}
}

func TestRplusProxy_Bookings_POST_PreservesBody(t *testing.T) {
	var gotBody string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
		}
		gotBody = string(buf)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"booking_id":"b-1"}`))
	}))
	defer stub.Close()
	h := newRplusMux(t, stub)

	w := doRplusReq(t, h, http.MethodPost, "/api/bookings", `{"course_id":"c-1"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201", w.Code)
	}
	if !strings.Contains(gotBody, "c-1") {
		t.Errorf("body = %q; want includes c-1", gotBody)
	}
}

// -----------------------------------------------------------------------------
// /api/certifications
// -----------------------------------------------------------------------------

func TestRplusProxy_Certifications_GET_ByID(t *testing.T) {
	var gotPath string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"cert_id":"cert-9"}`))
	}))
	defer stub.Close()
	h := newRplusMux(t, stub)

	w := doRplusReq(t, h, http.MethodGet, "/api/certifications/cert-9", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if gotPath != "/api/certifications/cert-9" {
		t.Errorf("downstream path = %q; want /api/certifications/cert-9", gotPath)
	}
}

// -----------------------------------------------------------------------------
// /v1/campus
// -----------------------------------------------------------------------------

func TestRplusProxy_Campus_GET_List(t *testing.T) {
	var gotPath, gotQuery string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer stub.Close()
	h := newRplusMux(t, stub)

	w := doRplusReq(t, h, http.MethodGet, "/v1/campus?type=ROOM&page=1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if gotPath != "/v1/campus" {
		t.Errorf("path = %q; want /v1/campus", gotPath)
	}
	if gotQuery != "type=ROOM&page=1" {
		t.Errorf("query = %q; want type=ROOM&page=1", gotQuery)
	}
}

// -----------------------------------------------------------------------------
// /v1/me/applications
// -----------------------------------------------------------------------------

func TestRplusProxy_MeApplications_GET_List(t *testing.T) {
	var gotPath string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	}))
	defer stub.Close()
	h := newRplusMux(t, stub)

	w := doRplusReq(t, h, http.MethodGet, "/v1/me/applications", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if gotPath != "/v1/me/applications" {
		t.Errorf("path = %q; want /v1/me/applications", gotPath)
	}
}

func TestRplusProxy_MeApplications_POST_SubPath(t *testing.T) {
	var gotPath, gotMethod string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"checkout_url":"https://stripe.example/cs_xxx"}`))
	}))
	defer stub.Close()
	h := newRplusMux(t, stub)

	w := doRplusReq(t, h, http.MethodPost, "/v1/me/applications/app-1/accept-offer", `{}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", w.Code)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/me/applications/app-1/accept-offer" {
		t.Errorf("downstream = %s %q; want POST /v1/me/applications/app-1/accept-offer", gotMethod, gotPath)
	}
}

// -----------------------------------------------------------------------------
// WithRplusDeliveryProxy composition
// -----------------------------------------------------------------------------

func TestWithRplusDeliveryProxy_NilAggregator_PassesThrough(t *testing.T) {
	baseHit := false
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		baseHit = true
		w.WriteHeader(http.StatusTeapot)
	})

	composed := httpadapter.WithRplusDeliveryProxy(base, nil)
	r := httptest.NewRequest(http.MethodGet, "/api/bookings", nil)
	w := httptest.NewRecorder()
	composed.ServeHTTP(w, r)

	if !baseHit {
		t.Fatal("nil aggregator should fall through to base handler")
	}
	if w.Code != http.StatusTeapot {
		t.Errorf("status = %d; want 418 (passthrough)", w.Code)
	}
}

func TestWithRplusDeliveryProxy_NonBridgePath_FallsThroughToBase(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("bridge should NOT be called for non-bridge path; got %s", r.URL.Path)
	}))
	defer stub.Close()
	agg := gatewayproxy.New(gatewayproxy.Config{
		DeliveryURL: stub.URL, PerCallTimeout: time.Second,
	})

	baseHit := false
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		baseHit = true
		w.WriteHeader(http.StatusOK)
	})

	composed := httpadapter.WithRplusDeliveryProxy(base, agg)
	r := httptest.NewRequest(http.MethodGet, "/unrelated/path", nil)
	w := httptest.NewRecorder()
	composed.ServeHTTP(w, r)

	if !baseHit {
		t.Fatal("non-bridge path should fall through to base handler")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
}

// -----------------------------------------------------------------------------
// /api/v1/rooms — R+ four-mode SHORT (ADR-237, CHO-2191): durable Room
// catalogue backing the Schedule & Rooms room picker + room_id double-book
// gate. The chora-delivery handler + FE picker shipped, but the gateway route
// was omitted, so the browser got GATEWAY_ROUTE_NOT_FOUND (404) — the picker
// rendered "Couldn't load rooms" and the double-book gate was inert in the UI.
// -----------------------------------------------------------------------------

func TestRplusProxy_Rooms_GET_List(t *testing.T) {
	var gotPath, gotMethod string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"rooms":[]}`))
	}))
	defer stub.Close()
	h := newRplusMux(t, stub)

	w := doRplusReq(t, h, http.MethodGet, "/api/v1/rooms", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", w.Code, w.Body.String())
	}
	if gotMethod != http.MethodGet || gotPath != "/api/v1/rooms" {
		t.Errorf("downstream method/path = %s %q; want GET /api/v1/rooms", gotMethod, gotPath)
	}
}

func TestRplusProxy_Rooms_POST_PreservesBody(t *testing.T) {
	var gotBody, gotMethod string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
		}
		gotBody = string(buf)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"room-1","name":"Alpha","capacity":30}`))
	}))
	defer stub.Close()
	h := newRplusMux(t, stub)

	w := doRplusReq(t, h, http.MethodPost, "/api/v1/rooms", `{"name":"Alpha","capacity":30}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201; body=%s", w.Code, w.Body.String())
	}
	if gotMethod != http.MethodPost {
		t.Errorf("downstream method = %s; want POST", gotMethod)
	}
	if !strings.Contains(gotBody, "Alpha") {
		t.Errorf("body = %q; want includes Alpha", gotBody)
	}
}

// -----------------------------------------------------------------------------
// DefaultJWTGatedPrefixes coverage
// -----------------------------------------------------------------------------

func TestDefaultJWTGatedPrefixes_CoversRplusDeliveryProxy(t *testing.T) {
	// Every R+ delivery proxy prefix (list #1) MUST be covered by a
	// DefaultJWTGatedPrefixes entry (list #2) so RequireChoraSessionJWT
	// validates the Bearer + stamps mesh claims (tenant_id + gcid) BEFORE the
	// bridge forwards to chora-delivery — otherwise the downstream
	// tenantRequired middleware 400s "X-Tenant-Id header required".
	//
	// Iterates the REAL RplusDeliveryProxyPathPrefixes (not a hardcoded
	// subset) so a future list-#1 addition that forgets the list-#2 gate fails
	// HERE at build time instead of as a live 400. This was the latent gap
	// that let /api/v1/certifications + /api/v1/applications ship ungated
	// (the old hardcoded mustCover only checked bookings/certifications/campus).
	for _, want := range httpadapter.RplusDeliveryProxyPathPrefixes {
		covered := false
		for _, prefix := range httpadapter.DefaultJWTGatedPrefixes {
			if want == prefix || strings.HasPrefix(want, strings.TrimSuffix(prefix, "/")+"/") {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("R+ proxy prefix %q is NOT covered by DefaultJWTGatedPrefixes — the gateway would skip JWT validation and chora-delivery would 400 \"X-Tenant-Id header required\"", want)
		}
	}
}
