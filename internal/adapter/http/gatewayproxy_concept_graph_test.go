// gatewayproxy_concept_graph_test.go — HTTP route binding tests for the
// learner-sovereign Discovery Knowledge Graph BFF proxy routes (A+ Discovery).
// These mirror the ADR-204 Goals handler bindings: the gateway claims the route
// shapes and proxies method + path + body + query VERBATIM to chora-consumption
// with the /api prefix STRIPPED (the downstream serves the EXT-scope routes at
// /v1/me/concept-graph[/...]).
//
// The exact leaf serves the GET list; the /api/v1/me/concept-graph/ subtree
// forwards ALL methods verbatim (POST reroot / POST concepts / PATCH+DELETE
// concepts/{id} / POST edges / DELETE edges/{id}) — the gateway does NOT
// enumerate the leaves; chora-consumption's own router does the sub-route
// dispatch.
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
// GET /api/v1/me/concept-graph (list → {concepts, edges, rootConceptId})
// -----------------------------------------------------------------------------

func TestGwProxy_ConceptGraphList_200_ProxiesToConsumption(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"concepts":[],"edges":[],"rootConceptId":null}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/v1/me/concept-graph?include=edges", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/v1/me/concept-graph" {
		t.Errorf("downstream path = %q; want /v1/me/concept-graph (/api stripped)", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("downstream method = %s; want GET", gotMethod)
	}
	if gotQuery != "include=edges" {
		t.Errorf("downstream query = %q; want include=edges forwarded verbatim", gotQuery)
	}
}

// -----------------------------------------------------------------------------
// Subtree — ALL methods forwarded VERBATIM (no leaf enumeration at the gateway).
// A table over the 6 subtree route shapes asserts method + path + body reach
// chora-consumption unchanged, proving the forward is truly verbatim.
// -----------------------------------------------------------------------------

func TestGwProxy_ConceptGraphSubtree_ForwardsAllMethodsVerbatim(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		inPath     string
		wantPath   string
		body       string
		wantStatus int
		respStatus int
		respBody   string
	}{
		{
			name:       "POST reroot",
			method:     http.MethodPost,
			inPath:     "/api/v1/me/concept-graph/reroot",
			wantPath:   "/v1/me/concept-graph/reroot",
			body:       `{"conceptId":"c-9"}`,
			respStatus: http.StatusOK,
			wantStatus: http.StatusOK,
			respBody:   `{"rootConceptId":"c-9"}`,
		},
		{
			name:       "POST concepts create",
			method:     http.MethodPost,
			inPath:     "/api/v1/me/concept-graph/concepts",
			wantPath:   "/v1/me/concept-graph/concepts",
			body:       `{"label":"Photosynthesis"}`,
			respStatus: http.StatusCreated,
			wantStatus: http.StatusCreated,
			respBody:   `{"conceptId":"c-1"}`,
		},
		{
			name:       "PATCH concepts/{id} update",
			method:     http.MethodPatch,
			inPath:     "/api/v1/me/concept-graph/concepts/c-1",
			wantPath:   "/v1/me/concept-graph/concepts/c-1",
			body:       `{"label":"Cell biology"}`,
			respStatus: http.StatusOK,
			wantStatus: http.StatusOK,
			respBody:   `{"conceptId":"c-1"}`,
		},
		{
			name:       "DELETE concepts/{id}",
			method:     http.MethodDelete,
			inPath:     "/api/v1/me/concept-graph/concepts/c-1",
			wantPath:   "/v1/me/concept-graph/concepts/c-1",
			body:       "",
			respStatus: http.StatusNoContent,
			wantStatus: http.StatusNoContent,
			respBody:   "",
		},
		{
			name:       "POST edges create",
			method:     http.MethodPost,
			inPath:     "/api/v1/me/concept-graph/edges",
			wantPath:   "/v1/me/concept-graph/edges",
			body:       `{"fromConceptId":"c-1","toConceptId":"c-2"}`,
			respStatus: http.StatusCreated,
			wantStatus: http.StatusCreated,
			respBody:   `{"edgeId":"e-1"}`,
		},
		{
			name:       "DELETE edges/{id}",
			method:     http.MethodDelete,
			inPath:     "/api/v1/me/concept-graph/edges/e-1",
			wantPath:   "/v1/me/concept-graph/edges/e-1",
			body:       "",
			respStatus: http.StatusNoContent,
			wantStatus: http.StatusNoContent,
			respBody:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotMethod, gotBody string
			stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotMethod = r.Method
				buf := make([]byte, r.ContentLength)
				if r.ContentLength > 0 {
					_, _ = r.Body.Read(buf)
				}
				gotBody = string(buf)
				w.WriteHeader(tc.respStatus)
				if tc.respBody != "" {
					_, _ = w.Write([]byte(tc.respBody))
				}
			})
			h := newGwProxyMux(t, stub)
			w := doGwProxyReq(t, h, tc.method, tc.inPath, tc.body)
			if w.Code != tc.wantStatus {
				t.Errorf("status = %d; want %d", w.Code, tc.wantStatus)
			}
			if gotPath != tc.wantPath {
				t.Errorf("downstream path = %q; want %q (/api stripped)", gotPath, tc.wantPath)
			}
			if gotMethod != tc.method {
				t.Errorf("downstream method = %s; want %s forwarded verbatim", gotMethod, tc.method)
			}
			if gotBody != tc.body {
				t.Errorf("downstream body = %q; want %q forwarded verbatim", gotBody, tc.body)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// WithGatewayProxy composition — the bridge MUST claim the concept-graph paths
// (they must NOT fall through to the base router).
// -----------------------------------------------------------------------------

func TestWithGatewayProxy_ConceptGraphPath_HandledByBridge(t *testing.T) {
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
		{http.MethodGet, "/api/v1/me/concept-graph"},
		{http.MethodPost, "/api/v1/me/concept-graph/reroot"},
		{http.MethodPost, "/api/v1/me/concept-graph/concepts"},
		{http.MethodPatch, "/api/v1/me/concept-graph/concepts/c-1"},
		{http.MethodDelete, "/api/v1/me/concept-graph/concepts/c-1"},
		{http.MethodPost, "/api/v1/me/concept-graph/edges"},
		{http.MethodDelete, "/api/v1/me/concept-graph/edges/e-1"},
	} {
		r := httptest.NewRequestWithContext(context.Background(), tc.method, tc.path, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer t")
		r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusTeapot {
			t.Errorf("%s %s: leaked to base — bridge must own the concept-graph routes", tc.method, tc.path)
		}
	}
}

// -----------------------------------------------------------------------------
// JWT-gate prefix coverage — DefaultJWTGatedPrefixes MUST cover the
// concept-graph routes so RequireChoraSessionJWT validates + stamps mesh claims
// before the handler runs (else the downstream chora-consumption
// extRequireContext 4xxs on the missing X-Tenant-Id + gcid).
// -----------------------------------------------------------------------------

func TestDefaultJWTGatedPrefixes_CoversConceptGraph(t *testing.T) {
	wantCovered := []string{
		"/api/v1/me/concept-graph",
		"/api/v1/me/concept-graph/reroot",
		"/api/v1/me/concept-graph/concepts",
		"/api/v1/me/concept-graph/concepts/c-1",
		"/api/v1/me/concept-graph/edges",
		"/api/v1/me/concept-graph/edges/e-1",
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
