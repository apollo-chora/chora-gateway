// gatewayproxy_goals_test.go — HTTP route binding tests for the ADR-204
// learner-owned Goal BFF proxy routes (A+ Discovery). These mirror the Epic-1b
// W8 Growth Edges handler bindings: the gateway claims the route shapes and
// proxies method + body + query verbatim to chora-consumption with the /api
// prefix STRIPPED (the downstream serves the EXT-scope routes at /v1/me/...).
//
// Routes:
//   - GET   /api/v1/me/goals        → chora-consumption GET   /v1/me/goals
//   - POST  /api/v1/me/goals        → chora-consumption POST  /v1/me/goals
//   - PATCH /api/v1/me/goals/{id}   → chora-consumption PATCH /v1/me/goals/{id}
//
// Plus: mesh-trust header propagation, WithGatewayProxy composition (bridge
// ownership), and JWT-gate coverage.
//
// Reuses newGwProxyStub / newGwProxyMux / doGwProxyReq from
// gatewayproxy_handler_test.go (same package).
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

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

// -----------------------------------------------------------------------------
// GET /api/v1/me/goals (list → {items, primaryLens})
// -----------------------------------------------------------------------------

func TestGwProxy_GoalsList_200_ProxiesToConsumption(t *testing.T) {
	var gotPath, gotMethod, gotQuery string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotQuery = r.URL.RawQuery
		if r.Header.Get("gcid") != "gcid-001" {
			t.Errorf("downstream lowercase gcid = %q; want gcid-001", r.Header.Get("gcid"))
		}
		if r.Header.Get("X-Tenant-Id") != "tenant-001" {
			t.Errorf("downstream X-Tenant-Id = %q; want tenant-001", r.Header.Get("X-Tenant-Id"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[],"primaryLens":"curiosity"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/v1/me/goals?status=active", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/v1/me/goals" {
		t.Errorf("downstream path = %q; want /v1/me/goals (/api stripped)", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("downstream method = %s; want GET", gotMethod)
	}
	if gotQuery != "status=active" {
		t.Errorf("downstream query = %q; want status=active forwarded verbatim", gotQuery)
	}
}

// -----------------------------------------------------------------------------
// POST /api/v1/me/goals (create)
// -----------------------------------------------------------------------------

func TestGwProxy_GoalsCreate_201_ProxiesToConsumption(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
		}
		gotBody = string(buf)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"goalId":"g-1"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/v1/me/goals", `{"kind":"curiosity"}`)
	if w.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201", w.Code)
	}
	if gotPath != "/v1/me/goals" {
		t.Errorf("downstream path = %q; want /v1/me/goals (/api stripped)", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("downstream method = %s; want POST", gotMethod)
	}
	if gotBody != `{"kind":"curiosity"}` {
		t.Errorf("downstream body = %q; want the JSON GoalDTO forwarded verbatim", gotBody)
	}
}

// -----------------------------------------------------------------------------
// PATCH /api/v1/me/goals/{id} (update — via the /api/v1/me/goals/ subpath)
// -----------------------------------------------------------------------------

func TestGwProxy_GoalsUpdate_200_ProxiesToConsumption(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
		}
		gotBody = string(buf)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"goalId":"g-1","status":"achieved"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPatch, "/api/v1/me/goals/g-1", `{"status":"achieved"}`)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/v1/me/goals/g-1" {
		t.Errorf("downstream path = %q; want /v1/me/goals/g-1 (/api stripped)", gotPath)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("downstream method = %s; want PATCH", gotMethod)
	}
	if gotBody != `{"status":"achieved"}` {
		t.Errorf("downstream body = %q; want the JSON patch forwarded verbatim", gotBody)
	}
}

// -----------------------------------------------------------------------------
// GET /api/v1/me/goals/{id}/knowledge — the Companion's per-goal reflection
// (CHO-2118). A goal SUB-RESOURCE, and the only thing that makes it reachable is
// that the bridge proxies the `/api/v1/me/goals/` subtree VERBATIM: it appears in
// no route registry, no per-leaf allowlist, and no method gate.
//
// That is a deliberate, cheap design — and it is completely unpinned. This test
// is the pin. If it ever fails, the learner does not see a broken reflection;
// they see a 404 from the edge that no chora-consumption log will explain,
// because the request never arrives.
// -----------------------------------------------------------------------------

func TestGwProxy_GoalKnowledge_200_ProxiesToConsumption(t *testing.T) {
	var gotPath, gotMethod, gotQuery string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"goalId":"g-1","reflection":{"text":"I remember you.","status":"fresh"}}`))
	})
	h := newGwProxyMux(t, stub)

	w := doGwProxyReq(t, h, http.MethodGet, "/api/v1/me/goals/g-1/knowledge?x=1", "")

	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/v1/me/goals/g-1/knowledge" {
		t.Errorf("downstream path = %q; want /v1/me/goals/g-1/knowledge (/api stripped, sub-resource preserved)", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("downstream method = %s; want GET", gotMethod)
	}
	if gotQuery != "x=1" {
		t.Errorf("downstream query = %q; want it forwarded verbatim", gotQuery)
	}
	if !strings.Contains(w.Body.String(), `"status":"fresh"`) {
		t.Errorf("body not returned to the caller: %s", w.Body.String())
	}
}

// -----------------------------------------------------------------------------
// WithGatewayProxy composition — the bridge MUST claim the goals paths (they
// must NOT fall through to the base router).
// -----------------------------------------------------------------------------

func TestWithGatewayProxy_GoalsPath_HandledByBridge(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	agg := gatewayproxy.New(gatewayproxy.Config{
		ConsumptionURL: stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/me/goals"},
		{http.MethodPost, "/api/v1/me/goals"},
		{http.MethodPatch, "/api/v1/me/goals/g-1"},
		// The goal-knowledge sub-resource (CHO-2118). It has NO entry of its own
		// in any gateway list — it is reachable ONLY because the bridge claims the
		// whole `/api/v1/me/goals/` SUBTREE, and it does so BEFORE the route
		// registry (which lists no goals route at all) can 404 it. Narrow that
		// subtree match to exact leaves and the Companion's reflection dies with a
		// GATEWAY_ROUTE_NOT_FOUND that no consumption log would ever explain.
		{http.MethodGet, "/api/v1/me/goals/g-1/knowledge"},
	} {
		r := httptest.NewRequestWithContext(context.Background(), tc.method, tc.path, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer t")
		r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusTeapot {
			t.Errorf("%s %s: leaked to base — bridge must own the goals routes", tc.method, tc.path)
		}
	}
}

// -----------------------------------------------------------------------------
// JWT-gate prefix coverage — DefaultJWTGatedPrefixes MUST cover the goals routes
// so RequireChoraSessionJWT validates + stamps mesh claims before the handler
// runs (else the downstream chora-consumption extRequireContext 4xxs).
// -----------------------------------------------------------------------------

func TestDefaultJWTGatedPrefixes_CoversGoals(t *testing.T) {
	wantCovered := []string{
		"/api/v1/me/goals",
		"/api/v1/me/goals/g-1",
		"/api/v1/me/goals/g-1/knowledge", // CHO-2118 sub-resource
	}
	for _, p := range wantCovered {
		covered := false
		for _, prefix := range httpadapter.DefaultJWTGatedPrefixes {
			if strings.HasPrefix(p, prefix) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("path %q is NOT covered by DefaultJWTGatedPrefixes — JWT gate would skip, mesh claims would be empty, chora-consumption would 4xx", p)
		}
	}
}
