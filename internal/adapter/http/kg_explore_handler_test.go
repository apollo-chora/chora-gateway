// kg_explore_handler_test.go — HTTP route binding tests for the BFF
// GET /api/v1/consumption/kg/explore/{atom_id} proxy (D1.4).
//
// The FE Discovery KG canvas (P1.3) consumes the per-user KG hexagonal
// exploration endpoint. The route 404'd at the gateway edge because
// chora-gateway did not proxy it. These tests cover:
//   - 200 happy path proxied to chora-consumption's identical path
//   - mesh-trust headers reach the downstream
//   - 401 via the JWT prefix gate when no token is presented
//   - 405 on non-GET
//   - 502/504 normalisation
//   - WithKGExploreBridge composition (nil aggregator passthrough)
//
// Strict TDD: tests written BEFORE the handler implementation.
package httpadapter_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-common/auth/chorasession"
	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/kgexplore"
)

func newKGExploreStub(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func newKGExploreBridgeHandler(t *testing.T, stub *httptest.Server, timeout time.Duration) http.Handler {
	t.Helper()
	if timeout == 0 {
		timeout = 1 * time.Second
	}
	agg := kgexplore.New(kgexplore.Config{
		ConsumptionURL: stub.URL,
		PerCallTimeout: timeout,
	})
	if agg == nil {
		t.Fatal("aggregator nil — stub URL should have wired it")
	}
	return httpadapter.NewKGExploreMux(agg)
}

func doKGExploreReq(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), method, path, nil)
	r.Header.Set("Authorization", "Bearer testtoken")
	r.Header.Set("X-Tenant-Id", "tenant-001")
	// Stamp MeshClaims into context the way RequireChoraSessionJWT would.
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// -----------------------------------------------------------------------------
// 200 — happy path: GET /api/v1/consumption/kg/explore/{atom_id}
// -----------------------------------------------------------------------------

func TestKGExplore_HappyPath_200_ProxiesToConsumption(t *testing.T) {
	stub := newKGExploreStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/consumption/kg/explore/atom-abc" {
			t.Errorf("stub path=%s want /api/v1/consumption/kg/explore/atom-abc", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("stub method=%s want GET", r.Method)
		}
		// chora-consumption fog_handler.extRequireContext reads X-Tenant-Id +
		// lowercase gcid — both must arrive or it 400s MISSING_CONTEXT.
		if r.Header.Get("X-Tenant-Id") != "tenant-001" {
			t.Errorf("missing X-Tenant-Id: %v", r.Header)
		}
		if r.Header.Get("gcid") != "gcid-001" {
			t.Errorf("missing lowercase gcid header: %v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"focal_node":"atom-abc","neighbors":[{"atom_id":"atom-9","relation":"prerequisite","confidence":0.8,"fog_label":"hidden","is_junction":false}]}`))
	})
	h := newKGExploreBridgeHandler(t, stub, 0)

	w := doKGExploreReq(t, h, http.MethodGet, "/api/v1/consumption/kg/explore/atom-abc")

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"focal_node"`) {
		t.Errorf("body=%s missing focal_node", w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type=%s want application/json", got)
	}
}

// -----------------------------------------------------------------------------
// 405 — non-GET on the explore path
// -----------------------------------------------------------------------------

func TestKGExplore_WrongMethod_405(t *testing.T) {
	stub := newKGExploreStub(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("downstream should not be called on 405")
	})
	h := newKGExploreBridgeHandler(t, stub, 0)

	w := doKGExploreReq(t, h, http.MethodPost, "/api/v1/consumption/kg/explore/atom-abc")

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want 405", w.Code)
	}
}

// -----------------------------------------------------------------------------
// 404 — atom_id missing from the path (bare prefix)
// -----------------------------------------------------------------------------

func TestKGExplore_MissingAtomID_404(t *testing.T) {
	stub := newKGExploreStub(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("downstream should not be called when atom_id is absent")
	})
	h := newKGExploreBridgeHandler(t, stub, 0)

	w := doKGExploreReq(t, h, http.MethodGet, "/api/v1/consumption/kg/explore/")

	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 for missing atom_id", w.Code)
	}
}

// -----------------------------------------------------------------------------
// 502 — upstream 5xx
// -----------------------------------------------------------------------------

func TestKGExplore_Upstream5xx_Becomes502(t *testing.T) {
	stub := newKGExploreStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	})
	h := newKGExploreBridgeHandler(t, stub, 0)

	w := doKGExploreReq(t, h, http.MethodGet, "/api/v1/consumption/kg/explore/atom-abc")

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

func TestKGExplore_UpstreamTimeout_Becomes504(t *testing.T) {
	stub := newKGExploreStub(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	h := newKGExploreBridgeHandler(t, stub, 50*time.Millisecond)

	w := doKGExploreReq(t, h, http.MethodGet, "/api/v1/consumption/kg/explore/atom-abc")

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d want 504 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "GATEWAY_UPSTREAM_TIMEOUT") {
		t.Errorf("body=%s missing GATEWAY_UPSTREAM_TIMEOUT", w.Body.String())
	}
}

// -----------------------------------------------------------------------------
// 4xx pass-through — the not-yet-fixed fog engine returns a downstream 4xx;
// it must be forwarded verbatim (the deliverable is the ROUTE; a downstream
// 4xx proves the request reached chora-consumption's handler).
// -----------------------------------------------------------------------------

func TestKGExplore_Downstream4xx_PassThrough(t *testing.T) {
	stub := newKGExploreStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"FOG_VALIDATE_FAILED","message":"validate-node FAILED"}`))
	})
	h := newKGExploreBridgeHandler(t, stub, 0)

	w := doKGExploreReq(t, h, http.MethodGet, "/api/v1/consumption/kg/explore/atom-abc")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 pass-through body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "FOG_VALIDATE_FAILED") {
		t.Errorf("body=%s — downstream handler error should pass through verbatim", w.Body.String())
	}
}

// -----------------------------------------------------------------------------
// 401 — the JWT prefix gate rejects an unauthenticated caller on the
// /api/v1/consumption/kg/explore/ prefix BEFORE the bridge runs.
// -----------------------------------------------------------------------------

// TestKGExplore_DefaultJWTGate_IncludesPrefix asserts the explore prefix is in
// DefaultJWTGatedPrefixes so WithChoraSessionOnPrefixes gates it.
func TestKGExplore_DefaultJWTGate_IncludesPrefix(t *testing.T) {
	found := false
	for _, p := range httpadapter.DefaultJWTGatedPrefixes {
		if p == httpadapter.KGExploreBridgePathPrefix {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("DefaultJWTGatedPrefixes does not include %q — JWT gate would let unauthenticated traffic through to kg/explore",
			httpadapter.KGExploreBridgePathPrefix)
	}
}

// TestKGExplore_UnauthenticatedRequest_401 wires the canonical
// WithChoraSessionOnPrefixes wrapper (the same one main.go uses) over the
// bridge and asserts an unauthenticated GET is rejected 401 at the edge.
func TestKGExplore_UnauthenticatedRequest_401(t *testing.T) {
	v, err := chorasession.NewValidator(
		[]byte("test-chora-session-signer-key-must-be-at-least-32-bytes-long"),
		"https://api.chora.site", "chora-489812")
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	stub := newKGExploreStub(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("downstream MUST NOT be called for an unauthenticated request")
	})
	agg := kgexplore.New(kgexplore.Config{ConsumptionURL: stub.URL, PerCallTimeout: time.Second})
	bridge := httpadapter.NewKGExploreMux(agg)
	wrapped := httpadapter.WithChoraSessionOnPrefixes(bridge, v)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/consumption/kg/explore/atom-abc", nil)
	// No Authorization header — anonymous request.
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET status=%d want 401", w.Code)
	}
}

// -----------------------------------------------------------------------------
// WithKGExploreBridge composition
// -----------------------------------------------------------------------------

func TestWithKGExploreBridge_NilAggregator_Passthrough(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	composed := httpadapter.WithKGExploreBridge(base, nil)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/consumption/kg/explore/atom-abc", nil)
	w := httptest.NewRecorder()
	composed.ServeHTTP(w, r)

	if w.Code != http.StatusTeapot {
		t.Errorf("nil aggregator should pass through to base — got %d", w.Code)
	}
}

func TestWithKGExploreBridge_RoutesBridgePaths(t *testing.T) {
	stub := newKGExploreStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"focal_node":"a","neighbors":[]}`))
	})
	agg := kgexplore.New(kgexplore.Config{ConsumptionURL: stub.URL, PerCallTimeout: time.Second})

	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Should ONLY be invoked for non-bridge paths.
		w.WriteHeader(http.StatusTeapot)
	})
	composed := httpadapter.WithKGExploreBridge(base, agg)

	// Bridge path — handled by mux, expect 200.
	r1 := httptest.NewRequest(http.MethodGet, "/api/v1/consumption/kg/explore/atom-abc", nil)
	r1 = r1.WithContext(httpadapter.InjectMeshClaimsForTest(r1.Context(), "gcid-001", "tenant-001"))
	w1 := httptest.NewRecorder()
	composed.ServeHTTP(w1, r1)
	if w1.Code != http.StatusOK {
		t.Errorf("bridge path: status=%d want 200 body=%s", w1.Code, w1.Body.String())
	}

	// Non-bridge path — passes through to base.
	r2 := httptest.NewRequest(http.MethodGet, "/api/v1/something-else", nil)
	w2 := httptest.NewRecorder()
	composed.ServeHTTP(w2, r2)
	if w2.Code != http.StatusTeapot {
		t.Errorf("non-bridge path: status=%d want 418 (base)", w2.Code)
	}
}
