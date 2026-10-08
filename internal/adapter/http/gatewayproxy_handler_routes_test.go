// gatewayproxy_handler_routes_test.go — route-level coverage for the
// gatewayproxy handler leaves the wire-up suite never asserted: courses
// collection + subpath, mana wallet + topup, enrolments, learning paths,
// growth edges + subpath, maps + subpath, collections collection/search,
// list-my-collections, daily-dose AI, admin payments subpath, me-course
// content + admin tenant leaves, plus the tenancy-bootstrap / me-leaves.
//
// Each test wires the real NewGatewayProxyMux with all downstreams pointed
// at ONE recording stub; the stub records the path + method so the route →
// downstream translation is pinned exactly.
package httpadapter_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

type gwRouteRecorder struct {
	*httptest.Server
	lastPath       string
	lastMethod     string
	lastQuery      string
	lastIdempotKey string
	lastBody       string
}

func newGwRouteRecorder(t *testing.T, status int, body string) *gwRouteRecorder {
	t.Helper()
	r := &gwRouteRecorder{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.lastPath = req.URL.Path
		r.lastMethod = req.Method
		r.lastQuery = req.URL.RawQuery
		r.lastIdempotKey = req.Header.Get("Idempotency-Key")
		b, _ := io.ReadAll(req.Body)
		r.lastBody = string(b)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(r.Server.Close)
	return r
}

func newGwRouteMux(t *testing.T, rec *gwRouteRecorder) http.Handler {
	t.Helper()
	agg := gatewayproxy.New(gatewayproxy.Config{
		TenancyURL:     rec.URL,
		DeliveryURL:    rec.URL,
		ConsumptionURL: rec.URL,
		IdentityURL:    rec.URL,
		CreationURL:    rec.URL,
		PaymentsURL:    rec.URL,
		GovernanceURL:  rec.URL,
		PerCallTimeout: 1 * time.Second,
	})
	if agg == nil {
		t.Fatal("aggregator nil")
	}
	return httpadapter.NewGatewayProxyMux(agg)
}

func TestGwRoutes_CoursesCollectionAndSubpath(t *testing.T) {
	rec := newGwRouteRecorder(t, http.StatusOK, `{"items":[]}`)
	mux := newGwRouteMux(t, rec)

	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{"collection GET", http.MethodGet, "/api/v1/courses"},
		{"collection POST", http.MethodPost, "/api/v1/courses"},
		{"subpath GET", http.MethodGet, "/api/v1/courses/c-1"},
		{"subpath PATCH", http.MethodPatch, "/api/v1/courses/c-1"},
		{"subpath DELETE", http.MethodDelete, "/api/v1/courses/c-1"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			w := doGwProxyReq(t, mux, tc.method, tc.path, `{}`)
			if w.Code != http.StatusOK {
				t.Fatalf("%s %s → %d; want 200", tc.method, tc.path, w.Code)
			}
		})
	}
}

func TestGwRoutes_ManaWalletAndTopup(t *testing.T) {
	rec := newGwRouteRecorder(t, http.StatusOK, `{"balance":100}`)
	mux := newGwRouteMux(t, rec)

	// Wallet GET → proxied 200.
	w := doGwProxyReq(t, mux, http.MethodGet, "/api/v1/me/mana", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/me/mana → %d; want 200", w.Code)
	}

	// Topup POST → retired 410 (use /api/v1/checkout/user-mana instead).
	w = doGwProxyReq(t, mux, http.MethodPost, "/api/v1/me/mana/topup", `{}`)
	if w.Code != http.StatusGone {
		t.Fatalf("POST /api/v1/me/mana/topup → %d; want 410 (retired)", w.Code)
	}
}

func TestGwRoutes_ManaDemoGrant(t *testing.T) {
	rec := newGwRouteRecorder(t, http.StatusOK,
		`{"granted_units":100000,"balance_units":100000,"replayed":false,"reason":"demo_grant"}`)
	mux := newGwRouteMux(t, rec)

	// Demo grant POST → proxied 200 to chora-identity, same path.
	w := doGwProxyReq(t, mux, http.MethodPost, "/api/v1/me/mana/demo-grant", "")
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/me/mana/demo-grant → %d; want 200", w.Code)
	}
	if rec.lastPath != "/api/v1/me/mana/demo-grant" {
		t.Fatalf("downstream path = %q; want /api/v1/me/mana/demo-grant", rec.lastPath)
	}
	if rec.lastMethod != http.MethodPost {
		t.Fatalf("downstream method = %q; want POST", rec.lastMethod)
	}

	// The amount is server-configured: the gateway must forward NO body, so a
	// client cannot smuggle in an amount.
	if rec.lastBody != "" {
		t.Fatalf("downstream body = %q; want empty (amount is server-configured)", rec.lastBody)
	}

	// Idempotency-Key is REQUIRED by the contract → forwarded verbatim.
	r := httptest.NewRequest(http.MethodPost, "/api/v1/me/mana/demo-grant", nil)
	r.Header.Set("Authorization", "Bearer testtoken")
	r.Header.Set("X-Tenant-Id", "tenant-001")
	r.Header.Set("Idempotency-Key", "demo-key-001")
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/me/mana/demo-grant (with key) → %d; want 200", w.Code)
	}
	if rec.lastIdempotKey != "demo-key-001" {
		t.Fatalf("forwarded Idempotency-Key = %q; want demo-key-001", rec.lastIdempotKey)
	}

	// Non-POST → 405.
	w = doGwProxyReq(t, mux, http.MethodGet, "/api/v1/me/mana/demo-grant", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/v1/me/mana/demo-grant → %d; want 405", w.Code)
	}
}

func TestGwRoutes_EnrolmentsLearningPathsGrowthEdges(t *testing.T) {
	rec := newGwRouteRecorder(t, http.StatusOK, `{"items":[]}`)
	mux := newGwRouteMux(t, rec)

	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{"enrolments", http.MethodGet, "/api/v1/me/enrolments"},
		{"learning paths", http.MethodGet, "/api/v1/me/learning-paths"},
		{"growth edges list", http.MethodGet, "/api/v1/me/growth-edges"},
		{"growth edges sub", http.MethodGet, "/api/v1/me/growth-edges/ge-1"},
		{"growth edges uploads", http.MethodPost, "/api/v1/me/growth-edges/ge-1/uploads"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			w := doGwProxyReq(t, mux, tc.method, tc.path, `{}`)
			if w.Code != http.StatusOK {
				t.Fatalf("%s %s → %d; want 200", tc.method, tc.path, w.Code)
			}
		})
	}
}

func TestGwRoutes_MapsCollectionsAndDailyDoseAI(t *testing.T) {
	rec := newGwRouteRecorder(t, http.StatusOK, `{"items":[]}`)
	mux := newGwRouteMux(t, rec)

	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{"maps list", http.MethodGet, "/api/v1/me/maps"},
		{"maps sub", http.MethodGet, "/api/v1/me/maps/g-1/graph"},
		{"collections collection", http.MethodPost, "/api/v1/collections"},
		{"collections search", http.MethodGet, "/api/v1/collections/search?q=x"},
		{"collections sub", http.MethodGet, "/api/v1/collections/c-1"},
		{"list my collections", http.MethodGet, "/api/v1/me/collections"},
		{"daily dose AI", http.MethodGet, "/api/companion/daily-dose/ai"},
		{"admin payments sub", http.MethodPost, "/api/v1/admin/payments/p-1/refund"},
		{"me course content", http.MethodGet, "/api/v1/me/courses/c-1/content"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			w := doGwProxyReq(t, mux, tc.method, tc.path, `{}`)
			if w.Code != http.StatusOK {
				t.Fatalf("%s %s → %d; want 200", tc.method, tc.path, w.Code)
			}
		})
	}
}

// TestGwRoutes_PassthroughPathsPinned — the raw path the downstream sees must
// be the canonical gatewayproxy fan-out target for a few representative leaves.
func TestGwRoutes_PassthroughPathsPinned(t *testing.T) {
	rec := newGwRouteRecorder(t, http.StatusOK, `{}`)
	mux := newGwRouteMux(t, rec)

	cases := []struct {
		method string
		path   string
		want   string
	}{
		{http.MethodGet, "/api/v1/me/enrolments", "/v1/me/enrolments"},
		{http.MethodGet, "/api/v1/me/learning-paths", "/v1/me/learning-paths"},
		{http.MethodGet, "/api/v1/me/growth-edges", "/v1/me/growth-edges"},
		{http.MethodGet, "/api/v1/me/maps", "/v1/me/maps"},
		{http.MethodGet, "/api/companion/daily-dose/ai", "/companion/daily-dose/ai"},
		{http.MethodPost, "/api/v1/admin/payments/p-1/refund", "/api/v1/admin/payments/p-1/refund"},
		// NOTE: /api/v1/me/courses/{id}/content is deliberately NOT pinned here.
		// It is a two-hop fan-out, not a passthrough: MeCourseContent calls
		// consumption /v1/me/courses/{id}/content AND delivery
		// /v1/me/courses/{id}/content/media-urls in parallel against the same
		// upstream, so a single-path pin would race on the shared recorder
		// (the census caught both outcomes across runs). Both hops are asserted
		// deterministically in internal/aggregator/gatewayproxy/course_content_test.go.
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := doGwProxyReq(t, mux, tc.method, tc.path, `{}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d", w.Code)
			}
			if rec.lastPath != tc.want {
				t.Errorf("downstream path = %q; want %q", rec.lastPath, tc.want)
			}
		})
	}
}

// TestGwRoutes_AdminTenantLeaves — the tenancy-admin / me leaves mounted on
// the OPlus+Phyllis router (handlers_oplus / phyllis_handler) are exercised
// through the WithGatewayProxy-composed base only when a test builds the full
// router — those live in route_registry_admin_test.go. Here we smoke the raw
// mux does not shadow them (falls through the base is out of scope).
func TestGwRoutes_AdminTenantLeaves_DoNotShadow(t *testing.T) {
	rec := newGwRouteRecorder(t, http.StatusOK, `{}`)
	mux := newGwRouteMux(t, rec)

	// These paths are NOT owned by the gatewayproxy mux; a request must
	// 404 (no handler registered in the bare mux), NOT be proxied.
	w := doGwProxyReq(t, mux, http.MethodGet, "/api/v1/tenants/me", "")
	if w.Code == http.StatusOK {
		t.Errorf("bare gatewayproxy mux must not own /api/v1/tenants/me; got %d", w.Code)
	}
	_ = strings.TrimSpace
}
