// phyllis_handler_routes_test.go — route-level coverage for the Phyllis
// handler leaves the TDD suite never hit: KG clusters (GET/POST/405), AI
// assist + job status (with 404 guards), tenants/me V1 + idp-providers
// subtree, tenants bootstrap, and the admin-tenants-me leaves. Each route
// is exercised through the full NewRouterWithPhyllis mux against a
// downstreamStub so path translation + passthrough are pinned.
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
	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
	"github.com/apollo-chora/chora-gateway/internal/domain/route"
)

// permissiveRouteRepo accepts every path with Public auth so route-level
// tests can exercise mux handlers that the default route table does not
// advertise (e.g. /api/v1/me/knowledge-graph/clusters or the
// idp-providers/{type} sub-path). authMiddleware Match()es every inbound
// path before the mux runs, so a missing entry would 404 GATEWAY_ROUTE_NOT_FOUND.
type permissiveRouteRepo struct {
	base route.Repository
}

func (p *permissiveRouteRepo) Match(ctx context.Context, path string) (*route.BFFRoute, error) {
	if a, err := p.base.Match(ctx, path); err == nil {
		return a, nil
	}
	return &route.BFFRoute{Surface: route.SurfaceAPI, PathPattern: path, BackendService: "test", AuthMode: route.AuthModePublic}, nil
}

func (p *permissiveRouteRepo) All(ctx context.Context) ([]*route.BFFRoute, error) {
	return p.base.All(ctx)
}

// newPermissivePhyllisServer wires NewRouterWithPhyllis with a
// permissiveRouteRepo so every mux-registered path passes the auth gate.
func newPermissivePhyllisServer(t *testing.T, cfg phyllis.Config) *httptest.Server {
	t.Helper()
	cfg.PerCallTimeout = 1 * time.Second
	cfg.AggregationBudget = 5 * time.Second

	base := inmem.NewRouteRepository()
	// inmem has no route-mutation API, but NewRouterWithPhyllisAndGraphQL
	// only depends on the Repository interface — wrap it.
	pr := &permissiveRouteRepo{base: base}
	sess := inmem.NewSessionRepository()
	up := upstream.NewFakeUpstream()
	agg := phyllis.New(cfg, nil)
	router := httpadapter.NewRouterWithPhyllis(pr, sess, up, agg)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

func doPhyllisReq(t *testing.T, srvURL, method, path, body string) *http.Response {
	t.Helper()
	var req *http.Request
	var err error
	if body != "" {
		req, err = http.NewRequest(method, srvURL+path, strings.NewReader(body))
	} else {
		req, err = http.NewRequest(method, srvURL+path, nil)
	}
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer phyllis")
	req.Header.Set("X-Tenant-Id", l1tTenant)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func TestPhyllisRoutes_KGClusters_GetPostAnd405(t *testing.T) {
	down := newDownstream(t, http.StatusOK, `{"clusters":[]}`)
	srv := newPermissivePhyllisServer(t, phyllis.Config{
		ConsumptionURL: down.URL,
		TenancyURL:     down.URL,
		IdentityURL:    down.URL,
		CreationURL:    down.URL,
	})

	// GET /api/v1/me/knowledge-graph/clusters
	resp := doPhyllisReq(t, srv.URL, http.MethodGet, "/api/v1/me/knowledge-graph/clusters", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET clusters → %d; want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// POST → CreateKGCluster
	resp = doPhyllisReq(t, srv.URL, http.MethodPost, "/api/v1/me/knowledge-graph/clusters", `{"name":"x"}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST clusters → %d; want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// PUT → 405
	resp = doPhyllisReq(t, srv.URL, http.MethodPut, "/api/v1/me/knowledge-graph/clusters", "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT clusters → %d; want 405", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestPhyllisRoutes_AIAssistAndJobStatus(t *testing.T) {
	down := newDownstream(t, http.StatusOK, `{"job_id":"job-9"}`)
	srv := newPermissivePhyllisServer(t, phyllis.Config{CreationURL: down.URL})

	// POST /api/atoms/ai-assist
	resp := doPhyllisReq(t, srv.URL, http.MethodPost, "/api/atoms/ai-assist", `{"topic":"x"}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST ai-assist → %d; want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// GET /api/atoms/ai-assist/job-9
	resp = doPhyllisReq(t, srv.URL, http.MethodGet, "/api/atoms/ai-assist/job-9", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET ai-assist/job-9 → %d; want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// GET with empty/multi-segment path → 404.
	resp = doPhyllisReq(t, srv.URL, http.MethodGet, "/api/atoms/ai-assist/", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET ai-assist/ → %d; want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()
	resp = doPhyllisReq(t, srv.URL, http.MethodGet, "/api/atoms/ai-assist/a/b", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET ai-assist/a/b → %d; want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// POST on job-status → 405.
	resp = doPhyllisReq(t, srv.URL, http.MethodPost, "/api/atoms/ai-assist/job-9", `{}`)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST job status → %d; want 405", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// GET on ai-assist → 405 (POST only).
	resp = doPhyllisReq(t, srv.URL, http.MethodGet, "/api/atoms/ai-assist", "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET ai-assist → %d; want 405", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestPhyllisRoutes_TenantsMeV1AndIdpProviders(t *testing.T) {
	down := newDownstream(t, http.StatusOK, `{"branding":{}}`)
	srv := newPermissivePhyllisServer(t, phyllis.Config{
		TenancyURL:  down.URL,
		IdentityURL: down.URL,
	})

	// GET /api/v1/tenants/me
	resp := doPhyllisReq(t, srv.URL, http.MethodGet, "/api/v1/tenants/me", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET tenants/me → %d; want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// GET /api/v1/tenants/me/idp-providers
	resp = doPhyllisReq(t, srv.URL, http.MethodGet, "/api/v1/tenants/me/idp-providers", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET idp-providers → %d; want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// DELETE /api/v1/tenants/me/idp-providers/google
	resp = doPhyllisReq(t, srv.URL, http.MethodDelete, "/api/v1/tenants/me/idp-providers/google", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("DELETE idp-providers/google → %d; want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// DELETE with empty segment → 400.
	resp = doPhyllisReq(t, srv.URL, http.MethodDelete, "/api/v1/tenants/me/idp-providers/", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("DELETE idp-providers/ → %d; want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// PATCH → 405.
	resp = doPhyllisReq(t, srv.URL, http.MethodPatch, "/api/v1/tenants/me/idp-providers", "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PATCH idp-providers → %d; want 405", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestPhyllisRoutes_TenantsBootstrap(t *testing.T) {
	down := newDownstream(t, http.StatusOK, `{"tenant_id":"t-1"}`)
	srv := newPermissivePhyllisServer(t, phyllis.Config{TenancyURL: down.URL})

	resp := doPhyllisReq(t, srv.URL, http.MethodPost, "/api/v1/tenants/bootstrap", `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST bootstrap → %d; want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestPhyllisRoutes_AdminTenantsMeLeaves(t *testing.T) {
	down := newDownstream(t, http.StatusOK, `{"addons":[]}`)
	srv := newPermissivePhyllisServer(t, phyllis.Config{TenancyURL: down.URL})

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/admin/tenants/me/addons"},
		{http.MethodPost, "/api/v1/admin/tenants/me/addons/plan-1:deactivate"},
	} {
		tc := tc
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			resp := doPhyllisReq(t, srv.URL, tc.method, tc.path, `{}`)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("%s %s → %d; want 200", tc.method, tc.path, resp.StatusCode)
			}
			_ = resp.Body.Close()
		})
	}
}
