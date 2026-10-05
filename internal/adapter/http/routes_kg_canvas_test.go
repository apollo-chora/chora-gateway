// routes_kg_canvas_test.go — RED-phase TDD specs for the per-user
// Knowledge-Graph hexagon-fog canvas BFF routes (ADR-143, contract
// chora-contracts/openapi/learner-knowledge-graph.yaml v1.1
// E2E-BE-KG-CANVAS block).
//
// The A+ FE (chora-web kg-fog.service.ts) calls 8 canvas/management
// endpoints under /api/v1/me/knowledge-graph/* + /api/v1/tenants/* that
// chora-consumption already serves (kg_canvas_handler.go via the EXT mux
// mounts /v1/me/knowledge-graph/clusters/ + /v1/me/knowledge-graph/junctions/
// + /v1/tenants/) but the gateway never proxied — every call 404'd at the
// BFF edge. These tests cover:
//
//   - (a) each new BFF route proxies to the right consumption path + method
//     (table-driven; the focal:move colon segment asserted verbatim)
//   - (b) identity headers forwarded: Authorization bearer + X-Tenant-Id +
//     lowercase gcid + canonical mesh chora-gcid/chora-tenant-id + traceparent
//   - (c) downstream status/body pass-through: 200 + 404 FOG_CACHE_MISS body
//     verbatim + 422 verbatim; downstream 503 → 502 GATEWAY_UPSTREAM_5XX and
//     timeout → 504 per the phyllis classify conventions the sibling KG
//     cluster list/create route uses
//   - (d) unauthenticated / missing-identity requests are NOT rejected by the
//     bridge — they forward downstream with no identity headers stamped,
//     mirroring the sibling /api/v1/me/knowledge-graph/clusters handler
//     (handleKGClusters → authCtxFromRequest: identity enforcement is the
//     downstream's job); at the production edge the same JWT prefix gate
//     (WithChoraSessionOnPrefixes) 401s both sibling + canvas routes
//   - claim precision: non-canvas paths (e.g. /api/v1/tenants/me) fall
//     through to the base handler untouched; nil aggregator = passthrough
//
// Strict TDD: written BEFORE routes_kg_canvas.go exists (RED = compile fail).
package httpadapter_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apollo-chora/chora-common/auth/chorasession"
	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

const (
	kgcTenant   = "01970000-0000-7000-8000-0000000000aa"
	kgcGCID     = "01935f12-0000-7000-8000-0000000000bb"
	kgcCluster  = "01970000-0000-7000-8000-00000000c111"
	kgcExplore  = "01970000-0000-7000-8000-00000000e111"
	kgcJunction = "01970000-0000-7000-8000-00000000d111"
	kgcTrace    = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
)

// kgCanvasStub is a recordable downstream fake that captures the full header
// set (the shared downstreamStub only records Authorization + traceparent —
// the canvas specs assert the lowercase gcid + canonical mesh headers too).
type kgCanvasStub struct {
	*httptest.Server
	calls      atomic.Int64
	lastMethod string
	lastPath   string
	lastQuery  string
	lastBody   string
	lastHeader http.Header
}

func newKGCanvasStub(t *testing.T, status int, body string) *kgCanvasStub {
	t.Helper()
	s := &kgCanvasStub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		s.lastMethod = r.Method
		s.lastPath = r.URL.Path
		s.lastQuery = r.URL.RawQuery
		s.lastHeader = r.Header.Clone()
		if r.Body != nil {
			b := make([]byte, 1<<20)
			n, _ := r.Body.Read(b)
			for n > 0 {
				s.lastBody += string(b[:n])
				n, _ = r.Body.Read(b)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Server.Close)
	return s
}

// newKGCanvasBridge composes WithKGCanvasRoutes over a teapot base so claim
// precision is observable (non-claimed paths → 418). The aggregator is a real
// phyllis.Aggregator pointed at the stub — same downstream-call helper +
// identity-header forwarding + timeouts the sibling KG list route uses.
func newKGCanvasBridge(t *testing.T, consumptionURL string, timeout time.Duration) http.Handler {
	t.Helper()
	if timeout == 0 {
		timeout = 1 * time.Second
	}
	agg := phyllis.New(phyllis.Config{
		ConsumptionURL:    consumptionURL,
		PerCallTimeout:    timeout,
		AggregationBudget: 5 * time.Second,
	}, nil)
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	return httpadapter.WithKGCanvasRoutes(base, agg)
}

// doKGCanvas issues a request with the full authed posture: Bearer +
// X-Tenant-Id + traceparent headers, MeshClaims stamped onto the context the
// way RequireChoraSessionJWT would (InjectMeshClaimsForTest).
func doKGCanvas(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequestWithContext(context.Background(), method, path, nil)
	} else {
		r = httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer kg-canvas-token")
	r.Header.Set("X-Tenant-Id", kgcTenant)
	r.Header.Set("traceparent", kgcTrace)
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), kgcGCID, kgcTenant))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// -----------------------------------------------------------------------------
// (a)+(b) — table-driven: BFF route → downstream path/method + identity headers
// -----------------------------------------------------------------------------

func TestKGCanvas_RouteTable_ProxiesToConsumption(t *testing.T) {
	cases := []struct {
		name         string
		method       string
		bffPath      string
		body         string
		wantDownPath string
	}{
		{
			name:         "hexagon GET",
			method:       http.MethodGet,
			bffPath:      "/api/v1/me/knowledge-graph/clusters/" + kgcCluster + "/explorations/" + kgcExplore + "/hexagon",
			wantDownPath: "/v1/me/knowledge-graph/clusters/" + kgcCluster + "/explorations/" + kgcExplore + "/hexagon",
		},
		{
			name:         "focal:move POST (colon segment verbatim)",
			method:       http.MethodPost,
			bffPath:      "/api/v1/me/knowledge-graph/clusters/" + kgcCluster + "/explorations/" + kgcExplore + "/focal:move",
			body:         `{"targetAtomId":"atom-2"}`,
			wantDownPath: "/v1/me/knowledge-graph/clusters/" + kgcCluster + "/explorations/" + kgcExplore + "/focal:move",
		},
		{
			name:         "cluster archive POST",
			method:       http.MethodPost,
			bffPath:      "/api/v1/me/knowledge-graph/clusters/" + kgcCluster + "/archive",
			body:         `{}`,
			wantDownPath: "/v1/me/knowledge-graph/clusters/" + kgcCluster + "/archive",
		},
		{
			name:         "cluster convert POST (ADR-223)",
			method:       http.MethodPost,
			bffPath:      "/api/v1/me/knowledge-graph/clusters/" + kgcCluster + "/convert",
			body:         `{}`,
			wantDownPath: "/v1/me/knowledge-graph/clusters/" + kgcCluster + "/convert",
		},
		{
			name:         "junction decide POST",
			method:       http.MethodPost,
			bffPath:      "/api/v1/me/knowledge-graph/junctions/" + kgcJunction + "/decide",
			body:         `{"decision":"accept"}`,
			wantDownPath: "/v1/me/knowledge-graph/junctions/" + kgcJunction + "/decide",
		},
		{
			name:         "management list GET",
			method:       http.MethodGet,
			bffPath:      "/api/v1/me/knowledge-graph/clusters/management",
			wantDownPath: "/v1/me/knowledge-graph/clusters/management",
		},
		{
			name:         "cluster rename PATCH",
			method:       http.MethodPatch,
			bffPath:      "/api/v1/me/knowledge-graph/clusters/" + kgcCluster,
			body:         `{"displayName":"Agile Maps"}`,
			wantDownPath: "/v1/me/knowledge-graph/clusters/" + kgcCluster,
		},
		{
			name:         "tenant kg config GET",
			method:       http.MethodGet,
			bffPath:      "/api/v1/tenants/" + kgcTenant + "/knowledge-graph/config",
			wantDownPath: "/v1/tenants/" + kgcTenant + "/knowledge-graph/config",
		},
		{
			name:         "tenant kg config PATCH",
			method:       http.MethodPatch,
			bffPath:      "/api/v1/tenants/" + kgcTenant + "/knowledge-graph/config",
			body:         `{"maxConcurrentKgClustersPerUser":5}`,
			wantDownPath: "/v1/tenants/" + kgcTenant + "/knowledge-graph/config",
		},
		// Sibling list/create resurrected through the bridge — the phyllis
		// mux registration was dead at the inner router (no route-table
		// entry → authMiddleware 404), so the bridge claims the exact path
		// and dispatches to the SAME aggregator methods.
		{
			name:         "cluster list GET (sibling)",
			method:       http.MethodGet,
			bffPath:      "/api/v1/me/knowledge-graph/clusters",
			wantDownPath: "/v1/me/knowledge-graph/clusters",
		},
		{
			name:         "cluster create POST (sibling)",
			method:       http.MethodPost,
			bffPath:      "/api/v1/me/knowledge-graph/clusters",
			body:         `{"seed_topic":"agile"}`,
			wantDownPath: "/v1/me/knowledge-graph/clusters",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newKGCanvasStub(t, http.StatusOK, `{"data":{"ok":true}}`)
			h := newKGCanvasBridge(t, stub.URL, 0)

			w := doKGCanvas(t, h, tc.method, tc.bffPath, tc.body)

			if w.Code != http.StatusOK {
				t.Fatalf("status=%d want 200 body=%s", w.Code, w.Body.String())
			}
			if got := w.Body.String(); !strings.Contains(got, `"ok":true`) {
				t.Errorf("body=%s — downstream body must pass through verbatim", got)
			}
			if stub.lastMethod != tc.method {
				t.Errorf("downstream method=%s want %s", stub.lastMethod, tc.method)
			}
			if stub.lastPath != tc.wantDownPath {
				t.Errorf("downstream path=%q want %q", stub.lastPath, tc.wantDownPath)
			}
			if tc.body != "" && stub.lastBody != tc.body {
				t.Errorf("downstream body=%q want verbatim %q", stub.lastBody, tc.body)
			}
			// (b) identity headers — the conventions the sibling KG list
			// route stamps via phyllis.call(): Authorization passthrough,
			// X-Tenant-Id, lowercase gcid (chora-consumption's
			// extRequireContext reads it or 400s MISSING_CONTEXT), the
			// canonical mesh chora-gcid/chora-tenant-id pair, traceparent.
			if got := stub.lastHeader.Get("Authorization"); got != "Bearer kg-canvas-token" {
				t.Errorf("Authorization=%q want bearer passthrough", got)
			}
			if got := stub.lastHeader.Get("X-Tenant-Id"); got != kgcTenant {
				t.Errorf("X-Tenant-Id=%q want %q", got, kgcTenant)
			}
			if got := stub.lastHeader.Get("gcid"); got != kgcGCID {
				t.Errorf("lowercase gcid=%q want %q", got, kgcGCID)
			}
			if got := stub.lastHeader.Get("chora-gcid"); got != kgcGCID {
				t.Errorf("chora-gcid=%q want %q", got, kgcGCID)
			}
			if got := stub.lastHeader.Get("chora-tenant-id"); got != kgcTenant {
				t.Errorf("chora-tenant-id=%q want %q", got, kgcTenant)
			}
			if got := stub.lastHeader.Get("traceparent"); got != kgcTrace {
				t.Errorf("traceparent=%q want inbound %q forwarded", got, kgcTrace)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// (c) — downstream status/body pass-through + 5xx/timeout translation
// -----------------------------------------------------------------------------

func TestKGCanvas_DownstreamStatusConventions(t *testing.T) {
	hexPath := "/api/v1/me/knowledge-graph/clusters/" + kgcCluster + "/explorations/" + kgcExplore + "/hexagon"

	t.Run("404 FOG_CACHE_MISS body passes through verbatim", func(t *testing.T) {
		body := `{"code":"FOG_CACHE_MISS","message":"hexagon not cached; trigger fog generation via /api/v1/consumption/kg/explore"}`
		stub := newKGCanvasStub(t, http.StatusNotFound, body)
		h := newKGCanvasBridge(t, stub.URL, 0)

		w := doKGCanvas(t, h, http.MethodGet, hexPath, "")
		if w.Code != http.StatusNotFound {
			t.Fatalf("status=%d want 404 pass-through", w.Code)
		}
		if w.Body.String() != body {
			t.Errorf("body=%s want verbatim FOG_CACHE_MISS envelope", w.Body.String())
		}
	})

	t.Run("422 validation body passes through verbatim", func(t *testing.T) {
		body := `{"code":"NEIGHBOR_NOT_IN_HEX","message":"target atom is not one of the cached 6 neighbors"}`
		stub := newKGCanvasStub(t, http.StatusUnprocessableEntity, body)
		h := newKGCanvasBridge(t, stub.URL, 0)

		w := doKGCanvas(t, h, http.MethodPost,
			"/api/v1/me/knowledge-graph/clusters/"+kgcCluster+"/explorations/"+kgcExplore+"/focal:move",
			`{"targetAtomId":"atom-9"}`)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status=%d want 422 pass-through", w.Code)
		}
		if !strings.Contains(w.Body.String(), "NEIGHBOR_NOT_IN_HEX") {
			t.Errorf("body=%s want downstream validation envelope verbatim", w.Body.String())
		}
	})

	t.Run("503 becomes 502 GATEWAY_UPSTREAM_5XX", func(t *testing.T) {
		stub := newKGCanvasStub(t, http.StatusServiceUnavailable, `{"error":"down"}`)
		h := newKGCanvasBridge(t, stub.URL, 0)

		w := doKGCanvas(t, h, http.MethodGet, hexPath, "")
		if w.Code != http.StatusBadGateway {
			t.Fatalf("status=%d want 502 (5xx normalised per sibling classify)", w.Code)
		}
		if !strings.Contains(w.Body.String(), "GATEWAY_UPSTREAM_5XX") {
			t.Errorf("body=%s missing GATEWAY_UPSTREAM_5XX", w.Body.String())
		}
	})

	t.Run("timeout becomes 504 GATEWAY_UPSTREAM_TIMEOUT", func(t *testing.T) {
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(300 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(slow.Close)
		h := newKGCanvasBridge(t, slow.URL, 50*time.Millisecond)

		w := doKGCanvas(t, h, http.MethodGet, hexPath, "")
		if w.Code != http.StatusGatewayTimeout {
			t.Fatalf("status=%d want 504 body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "GATEWAY_UPSTREAM_TIMEOUT") {
			t.Errorf("body=%s missing GATEWAY_UPSTREAM_TIMEOUT", w.Body.String())
		}
	})
}

// -----------------------------------------------------------------------------
// (d) — unauthenticated / missing-identity behavior mirrors the sibling
// -----------------------------------------------------------------------------

// TestKGCanvas_MissingIdentity_ForwardsWithoutClaims asserts the bridge does
// NOT locally reject an identity-less request — exactly like the sibling
// /api/v1/me/knowledge-graph/clusters handler (handleKGClusters →
// authCtxFromRequest → phyllis.call): the request forwards downstream with no
// gcid / X-Tenant-Id / mesh headers stamped, and the downstream's own
// MISSING_CONTEXT rejection passes through.
func TestKGCanvas_MissingIdentity_ForwardsWithoutClaims(t *testing.T) {
	paths := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/me/knowledge-graph/clusters"}, // sibling list
		{http.MethodGet, "/api/v1/me/knowledge-graph/clusters/" + kgcCluster + "/explorations/" + kgcExplore + "/hexagon"},
		{http.MethodPost, "/api/v1/me/knowledge-graph/clusters/" + kgcCluster + "/archive"},
	}
	for _, p := range paths {
		t.Run(p.method+" "+p.path, func(t *testing.T) {
			stub := newKGCanvasStub(t, http.StatusBadRequest,
				`{"code":"MISSING_CONTEXT","message":"X-Tenant-Id required"}`)
			h := newKGCanvasBridge(t, stub.URL, 0)

			// No Authorization, no X-Tenant-Id, no mesh claims on context.
			r := httptest.NewRequest(p.method, p.path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if stub.calls.Load() != 1 {
				t.Fatalf("downstream calls=%d want 1 — identity enforcement is the downstream's job (sibling parity)", stub.calls.Load())
			}
			if got := stub.lastHeader.Get("gcid"); got != "" {
				t.Errorf("gcid=%q want empty (no claims to stamp)", got)
			}
			if got := stub.lastHeader.Get("chora-gcid"); got != "" {
				t.Errorf("chora-gcid=%q want empty", got)
			}
			if got := stub.lastHeader.Get("X-Tenant-Id"); got != "" {
				t.Errorf("X-Tenant-Id=%q want empty", got)
			}
			if w.Code != http.StatusBadRequest {
				t.Errorf("status=%d want downstream 400 MISSING_CONTEXT pass-through", w.Code)
			}
			if !strings.Contains(w.Body.String(), "MISSING_CONTEXT") {
				t.Errorf("body=%s want downstream envelope verbatim", w.Body.String())
			}
		})
	}
}

// TestKGCanvas_DefaultJWTGate_IncludesPrefixes asserts the canvas prefixes are
// registered in DefaultJWTGatedPrefixes so WithChoraSessionOnPrefixes stamps
// validated mesh claims (gcid + tenant) before the bridge fires in production
// — without them every downstream call 400s MISSING_CONTEXT. Mirrors
// TestKGExplore_DefaultJWTGate_IncludesPrefix.
func TestKGCanvas_DefaultJWTGate_IncludesPrefixes(t *testing.T) {
	want := []string{
		httpadapter.KGCanvasJWTGatedPrefix,        // /api/v1/me/knowledge-graph
		httpadapter.KGCanvasTenantsJWTGatedPrefix, // /api/v1/tenants/
	}
	for _, wantPrefix := range want {
		found := false
		for _, p := range httpadapter.DefaultJWTGatedPrefixes {
			if p == wantPrefix {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("DefaultJWTGatedPrefixes missing %q — JWT gate would never stamp mesh claims for the canvas routes", wantPrefix)
		}
	}
}

// TestKGCanvas_UnauthenticatedAtEdge_401 wires the canonical
// WithChoraSessionOnPrefixes wrapper (same as main.go) over the bridge and
// asserts an anonymous canvas request is rejected 401 BEFORE the bridge —
// the same edge posture the sibling KG list path now shares via the
// /api/v1/me/knowledge-graph prefix.
func TestKGCanvas_UnauthenticatedAtEdge_401(t *testing.T) {
	v, err := chorasession.NewValidator(
		[]byte("test-chora-session-signer-key-must-be-at-least-32-bytes-long"),
		"https://api.chora.site", "chora-489812")
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	stub := newKGCanvasStub(t, http.StatusOK, `{}`)
	h := newKGCanvasBridge(t, stub.URL, 0)
	wrapped := httpadapter.WithChoraSessionOnPrefixes(h, v)

	for _, p := range []string{
		"/api/v1/me/knowledge-graph/clusters", // sibling — same gate
		"/api/v1/me/knowledge-graph/clusters/" + kgcCluster + "/explorations/" + kgcExplore + "/hexagon",
		"/api/v1/me/knowledge-graph/junctions/" + kgcJunction + "/decide",
		"/api/v1/tenants/" + kgcTenant + "/knowledge-graph/config",
	} {
		r := httptest.NewRequest(http.MethodGet, p, nil) // no Authorization
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: status=%d want 401 at the edge", p, w.Code)
		}
	}
	if stub.calls.Load() != 0 {
		t.Errorf("downstream calls=%d want 0 — gate must reject before the bridge", stub.calls.Load())
	}
}

// -----------------------------------------------------------------------------
// Method discipline + unknown sub-paths (downstream never called)
// -----------------------------------------------------------------------------

func TestKGCanvas_MethodAndShapeDiscipline(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{"GET on focal:move", http.MethodGet, "/api/v1/me/knowledge-graph/clusters/" + kgcCluster + "/explorations/" + kgcExplore + "/focal:move", http.StatusMethodNotAllowed},
		{"POST on hexagon", http.MethodPost, "/api/v1/me/knowledge-graph/clusters/" + kgcCluster + "/explorations/" + kgcExplore + "/hexagon", http.StatusMethodNotAllowed},
		{"GET on archive", http.MethodGet, "/api/v1/me/knowledge-graph/clusters/" + kgcCluster + "/archive", http.StatusMethodNotAllowed},
		{"GET on convert", http.MethodGet, "/api/v1/me/knowledge-graph/clusters/" + kgcCluster + "/convert", http.StatusMethodNotAllowed},
		{"POST on management", http.MethodPost, "/api/v1/me/knowledge-graph/clusters/management", http.StatusMethodNotAllowed},
		{"GET on junction decide", http.MethodGet, "/api/v1/me/knowledge-graph/junctions/" + kgcJunction + "/decide", http.StatusMethodNotAllowed},
		{"DELETE on clusters collection", http.MethodDelete, "/api/v1/me/knowledge-graph/clusters", http.StatusMethodNotAllowed},
		{"DELETE on tenant config", http.MethodDelete, "/api/v1/tenants/" + kgcTenant + "/knowledge-graph/config", http.StatusMethodNotAllowed},
		{"GET cluster detail (not a canvas route)", http.MethodGet, "/api/v1/me/knowledge-graph/clusters/" + kgcCluster, http.StatusNotFound},
		{"unknown cluster leaf", http.MethodPost, "/api/v1/me/knowledge-graph/clusters/" + kgcCluster + "/banana", http.StatusNotFound},
		{"unknown exploration leaf", http.MethodGet, "/api/v1/me/knowledge-graph/clusters/" + kgcCluster + "/explorations/" + kgcExplore + "/banana", http.StatusNotFound},
		{"junction without decide", http.MethodPost, "/api/v1/me/knowledge-graph/junctions/" + kgcJunction, http.StatusNotFound},
		{"empty cluster id segment", http.MethodPost, "/api/v1/me/knowledge-graph/clusters//archive", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newKGCanvasStub(t, http.StatusOK, `{}`)
			h := newKGCanvasBridge(t, stub.URL, 0)

			w := doKGCanvas(t, h, tc.method, tc.path, "")
			if w.Code != tc.wantStatus {
				t.Errorf("status=%d want %d body=%s", w.Code, tc.wantStatus, w.Body.String())
			}
			if stub.calls.Load() != 0 {
				t.Errorf("downstream calls=%d want 0 on dispatch errors", stub.calls.Load())
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Claim precision + composition
// -----------------------------------------------------------------------------

func TestWithKGCanvasRoutes_NilAggregator_Passthrough(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	composed := httpadapter.WithKGCanvasRoutes(base, nil)

	r := httptest.NewRequest(http.MethodGet,
		"/api/v1/me/knowledge-graph/clusters/"+kgcCluster+"/explorations/"+kgcExplore+"/hexagon", nil)
	w := httptest.NewRecorder()
	composed.ServeHTTP(w, r)
	if w.Code != http.StatusTeapot {
		t.Errorf("nil aggregator should pass through to base — got %d", w.Code)
	}
}

func TestKGCanvas_NonCanvasPaths_FallThroughToBase(t *testing.T) {
	stub := newKGCanvasStub(t, http.StatusOK, `{}`)
	h := newKGCanvasBridge(t, stub.URL, 0)

	for _, p := range []string{
		"/api/v1/tenants/me",                                         // phyllis-owned exact route
		"/api/v1/tenants/me/idp-providers",                           // phyllis-owned exact route
		"/api/v1/tenants/" + kgcTenant,                               // bare tenant id — not a canvas shape
		"/api/v1/tenants/" + kgcTenant + "/knowledge-graph",          // missing /config leaf
		"/api/v1/tenants/" + kgcTenant + "/knowledge-graph/config/x", // too deep
		"/api/v1/me/knowledge-graphs",                                // prefix near-miss
		"/api/v1/me/mana",                                            // other bridge's path
	} {
		w := doKGCanvas(t, h, http.MethodGet, p, "")
		if w.Code != http.StatusTeapot {
			t.Errorf("%s: status=%d want 418 fallthrough to base", p, w.Code)
		}
	}
	if stub.calls.Load() != 0 {
		t.Errorf("downstream calls=%d want 0 for fallthrough paths", stub.calls.Load())
	}
}
