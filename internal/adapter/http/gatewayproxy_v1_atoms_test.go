// gatewayproxy_v1_atoms_test.go — CHO-2261 route-binding + guard specs.
//
// The /api/v1/atoms prefix was unregistered at the gateway → the A+ Daily Dose
// silently dropped every AI-picked atom whose detail it fetched via
// GET /api/v1/atoms/{id}. Wiring a bridge route needs FIVE things in sync:
//
//  1. inmem route registry     — authMiddleware 404s GATEWAY_ROUTE_NOT_FOUND
//     against this BEFORE the mux ever runs.
//  2. DefaultJWTGatedPrefixes   — stamps X-Tenant-Id + gcid mesh claims
//     (chora-creation getAtom is RLS/tenant-scoped).
//  3. GatewayProxyPathPrefixes  — the bridge's parallel doc list.
//  4. NewGatewayProxyMux         — serves the leaf.
//  5. matchesGatewayProxyPath   — WithGatewayProxy routes it to the bridge.
//
// Miss any one and the route is dead in production while every unit test that
// calls the handler directly still passes. TestV1AtomsRoute_RegisteredInEveryList
// is the guard that fails RED if a future refactor drops any of the five.
//
// Strict TDD: written BEFORE the handlers + list entries exist.
package httpadapter_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/adapter/inmem"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

// -----------------------------------------------------------------------------
// Mux binding — GET /api/v1/atoms/{id} → chora-creation /api/atoms/{id}
// -----------------------------------------------------------------------------

func TestGwProxy_V1AtomsItem_GET_ProxiesToCreationTranslatingV1(t *testing.T) {
	var gotPath, gotMethod string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"atom-1","title":"Road cycling","atom_type":"mcq"}`))
	})
	h := newGwProxyMux(t, stub)

	w := doGwProxyReq(t, h, http.MethodGet, "/api/v1/atoms/atom-1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", w.Code, w.Body.String())
	}
	if gotPath != "/api/atoms/atom-1" {
		t.Errorf("downstream path = %q; want /api/atoms/atom-1 (v1 prefix translated)", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("downstream method = %s; want GET", gotMethod)
	}
	if !strings.Contains(w.Body.String(), `"Road cycling"`) {
		t.Errorf("body = %q; want unwrapped atom verbatim", w.Body.String())
	}
}

func TestGwProxy_V1AtomsCollection_GET_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod, gotQuery string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotQuery = r.URL.Path, r.Method, r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	})
	h := newGwProxyMux(t, stub)

	w := doGwProxyReq(t, h, http.MethodGet, "/api/v1/atoms?limit=20", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", w.Code, w.Body.String())
	}
	if gotPath != "/api/atoms" {
		t.Errorf("downstream path = %q; want /api/atoms (collection, v1 translated)", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("downstream method = %s; want GET", gotMethod)
	}
	if gotQuery != "limit=20" {
		t.Errorf("downstream query = %q; want limit=20 forwarded verbatim", gotQuery)
	}
}

// Fail loud, not silent (AC): an unsupported method must 405 with a clear code,
// never a silent fall-through, and must NOT reach the downstream.
func TestGwProxy_V1AtomsItem_NonGET_405_NoDownstream(t *testing.T) {
	called := false
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := newGwProxyMux(t, stub)

	w := doGwProxyReq(t, h, http.MethodDelete, "/api/v1/atoms/atom-1", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /api/v1/atoms/{id} = %d; want 405", w.Code)
	}
	if called {
		t.Error("405 must not reach the downstream")
	}
}

// -----------------------------------------------------------------------------
// Ownership — WithGatewayProxy must claim /api/v1/atoms[/{id}] for the bridge,
// NOT leak to the base (Phyllis) router.
// -----------------------------------------------------------------------------

func TestWithGatewayProxy_V1AtomsPaths_HandledByBridge(t *testing.T) {
	for _, p := range []string{"/api/v1/atoms", "/api/v1/atoms/019f6baf-0a31-708e-8923-954f39602102"} {
		base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		})
		stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"x"}`))
		})
		agg := gatewayproxy.New(gatewayproxy.Config{CreationURL: stub.URL, PerCallTimeout: time.Second})
		h := httpadapter.WithGatewayProxy(base, agg)

		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, p, nil)
		r.Header.Set("Authorization", "Bearer t")
		r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)

		if w.Code == http.StatusTeapot {
			t.Errorf("%s leaked to base — the bridge must own /api/v1/atoms", p)
		}
		if w.Code != http.StatusOK {
			t.Errorf("%s status = %d; want 200 from the bridge", p, w.Code)
		}
	}
}

// Regression guard: the sibling /api/atoms/{id} (NO v1, Phyllis-owned) MUST keep
// falling through to base — the v1 claim must not capture it.
func TestWithGatewayProxy_NonV1AtomItem_StillFallsThroughToPhyllis(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	agg := gatewayproxy.New(gatewayproxy.Config{CreationURL: stub.URL, PerCallTimeout: time.Second})
	h := httpadapter.WithGatewayProxy(base, agg)

	r := httptest.NewRequest(http.MethodGet, "/api/atoms/atom-1", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTeapot {
		t.Errorf("/api/atoms/{id} = %d; want 418 — must still fall through to Phyllis", w.Code)
	}
}

// -----------------------------------------------------------------------------
// GUARD — the /api/v1/atoms route must be present in EVERY required list.
// A future refactor that drops any one goes RED here.
// -----------------------------------------------------------------------------

func TestV1AtomsRoute_RegisteredInEveryList(t *testing.T) {
	const collection = "/api/v1/atoms"
	item := "/api/v1/atoms/019f6baf-0a31-708e-8923-954f39602102"

	// (1) inmem route registry — authMiddleware Match() runs BEFORE the mux;
	// a miss here is GATEWAY_ROUTE_NOT_FOUND (the CHO-2261 signature).
	repo := inmem.NewRouteRepository()
	for _, p := range []string{collection, item} {
		matched, err := repo.Match(context.Background(), p)
		if err != nil {
			t.Errorf("route %q not in the inmem registry — authMiddleware 404s it "+
				"(GATEWAY_ROUTE_NOT_FOUND) before the mux. Add a BFFRoute in "+
				"internal/adapter/inmem/route_repository.go.", p)
			continue
		}
		if matched.BackendService != "chora-creation" {
			t.Errorf("registry BackendService for %q = %q; want chora-creation", p, matched.BackendService)
		}
	}

	// (2) DefaultJWTGatedPrefixes — must HasPrefix-cover both so mesh claims are
	// stamped (chora-creation getAtom is RLS/tenant-scoped; empty tenant → 404).
	assertPrefixCovered(t, "DefaultJWTGatedPrefixes", httpadapter.DefaultJWTGatedPrefixes, collection, item)

	// (3) GatewayProxyPathPrefixes — the bridge's parallel doc list.
	assertPrefixCovered(t, "GatewayProxyPathPrefixes", httpadapter.GatewayProxyPathPrefixes, collection, item)

	// (4)+(5) mux + matchesGatewayProxyPath — exercised via WithGatewayProxy:
	// both paths must be bridge-owned (proven non-teapot in
	// TestWithGatewayProxy_V1AtomsPaths_HandledByBridge above).
}

func assertPrefixCovered(t *testing.T, listName string, prefixes []string, paths ...string) {
	t.Helper()
	for _, path := range paths {
		covered := false
		for _, p := range prefixes {
			if strings.HasPrefix(path, p) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("%s does not HasPrefix-cover %q — the JWT/ownership gate skips it", listName, path)
		}
	}
}
