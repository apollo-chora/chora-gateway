// gatewayproxy_handler_test.go — HTTP route binding tests for the A6 BFF
// proxy routes per docs/m13/handoff-fe-to-be-service-2026-05-14.md §A6.
//
// FE probed each route with a real Bearer ChoraSession JWT and found most
// non-auth /api/* routes return GATEWAY_ROUTE_NOT_FOUND. These tests cover
// the gateway route bindings that close that gap:
//   - GET  /api/feature-flags
//   - GET  /api/tenants/{id}
//   - GET  /api/courses/{id}
//   - POST /api/courses/{id}/enrol
//   - GET  /api/me/knowledge-graph/clusters
//   - GET  /api/me/companions
//   - GET  /api/me/companions/{id}/growth
//   - GET  /api/notifications
//
// Plus: mesh-trust header propagation, 405 on wrong method, path-shape
// 404s, and WithGatewayProxy composition (nil aggregator passthrough).
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

func newGwProxyStub(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// newGwProxyMux wires the gatewayproxy mux with every downstream pointed at
// the same stub server (each test asserts on the path the stub received so
// a single stub is sufficient).
func newGwProxyMux(t *testing.T, stub *httptest.Server) http.Handler {
	t.Helper()
	agg := gatewayproxy.New(gatewayproxy.Config{
		TenancyURL:       stub.URL,
		DeliveryURL:      stub.URL,
		ConsumptionURL:   stub.URL,
		NotificationsURL: stub.URL,
		// P7 — chora-creation serves the question CRUD routes at the same
		// path the FE hits. Same stub URL so per-test path assertions work.
		CreationURL: stub.URL,
		// B6.1 — chora-identity owns /api/v1/admin/tenant-members.
		IdentityURL:    stub.URL,
		PerCallTimeout: time.Second,
	})
	if agg == nil {
		t.Fatal("aggregator nil — stub URL should have wired it")
	}
	return httpadapter.NewGatewayProxyMux(agg)
}

func doGwProxyReq(t *testing.T, h http.Handler, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
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
// GET /api/feature-flags
// -----------------------------------------------------------------------------

func TestGwProxy_FeatureFlags_200_ProxiesToTenantEntitlements(t *testing.T) {
	var gotPath string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Header.Get("gcid") != "gcid-001" {
			t.Errorf("downstream lowercase gcid = %q; want gcid-001", r.Header.Get("gcid"))
		}
		if r.Header.Get("X-Tenant-Id") != "tenant-001" {
			t.Errorf("downstream X-Tenant-Id = %q; want tenant-001", r.Header.Get("X-Tenant-Id"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/feature-flags", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/api/tenants/tenant-001/entitlements" {
		t.Errorf("downstream path = %q; want /api/tenants/tenant-001/entitlements", gotPath)
	}
}

func TestGwProxy_FeatureFlags_405_OnPost(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/feature-flags", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on POST", w.Code)
	}
}

// -----------------------------------------------------------------------------
// GET /api/tenants/{id}
// -----------------------------------------------------------------------------

// Subject: the route proxies to the right downstream path. It used to ask for
// mtm-singapore from a session scoped to tenant-001 and assert 200, which was
// a cross-tenant read written down as correct behaviour. The tenant asked for
// is now the caller's own and the session carries the audience the route now
// requires; the assertions are unchanged, because neither was ever about
// reading someone else's tenant.
func TestGwProxy_TenantByID_200(t *testing.T) {
	var gotPath string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"tenant-001"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReqAs(t, h, http.MethodGet, "/api/tenants/tenant-001", "tenant-001", "owner")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/api/tenants/tenant-001" {
		t.Errorf("downstream path = %q; want /api/tenants/tenant-001", gotPath)
	}
}

func TestGwProxy_TenantByID_404_OnBareePrefix(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/tenants/", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on bare /api/tenants/", w.Code)
	}
}

// -----------------------------------------------------------------------------
// GET /api/courses/{id}
// -----------------------------------------------------------------------------

func TestGwProxy_CourseByID_200_ProxiesToDeliveryV1(t *testing.T) {
	var gotPath string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"course-1"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/courses/course-1", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/v1/courses/course-1" {
		t.Errorf("downstream path = %q; want /v1/courses/course-1", gotPath)
	}
}

// -----------------------------------------------------------------------------
// POST /api/courses/{id}/enrol
// -----------------------------------------------------------------------------

func TestGwProxy_EnrolCourse_201_ProxiesToDeliveryEnrolments(t *testing.T) {
	var gotPath, gotMethod string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"enrolment_id":"enr-1"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/courses/course-1/enrol", `{"gcid":"x"}`)
	if w.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201", w.Code)
	}
	if gotPath != "/v1/courses/course-1/enrolments" {
		t.Errorf("downstream path = %q; want /v1/courses/course-1/enrolments", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("downstream method = %s; want POST", gotMethod)
	}
}

func TestGwProxy_EnrolCourse_405_OnGet(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/courses/course-1/enrol", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on GET /enrol", w.Code)
	}
}

func TestGwProxy_Courses_404_OnUnknownSubpath(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/courses/course-1/bogus", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on unknown /api/courses/{id}/bogus subpath", w.Code)
	}
}

// -----------------------------------------------------------------------------
// GET /api/me/knowledge-graph/clusters
// -----------------------------------------------------------------------------

func TestGwProxy_KGClusters_200(t *testing.T) {
	var gotPath string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"clusters":[]}}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/me/knowledge-graph/clusters", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/v1/me/knowledge-graph/clusters" {
		t.Errorf("downstream path = %q; want /v1/me/knowledge-graph/clusters", gotPath)
	}
}

// -----------------------------------------------------------------------------
// GET /api/me/companions  +  GET /api/me/companions/{id}/growth
// -----------------------------------------------------------------------------

func TestGwProxy_ListCompanions_200(t *testing.T) {
	var gotPath string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/me/companions", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/v1/me/companions" {
		t.Errorf("downstream path = %q; want /v1/me/companions", gotPath)
	}
}

func TestGwProxy_CompanionGrowth_200(t *testing.T) {
	var gotPath string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"companion_id":"fam-1"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/me/companions/fam-1/growth", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/v1/me/companions/fam-1/growth" {
		t.Errorf("downstream path = %q; want /v1/me/companions/fam-1/growth", gotPath)
	}
}

func TestGwProxy_CompanionSubpath_404_OnUnknownLeaf(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/me/companions/fam-1/bogus", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on unknown companion subpath leaf", w.Code)
	}
}

// -----------------------------------------------------------------------------
// GET /api/notifications
// -----------------------------------------------------------------------------

func TestGwProxy_Notifications_200_ForwardsQuery(t *testing.T) {
	var gotPath, gotQuery string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/notifications?limit=20", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/api/notifications" {
		t.Errorf("downstream path = %q; want /api/notifications", gotPath)
	}
	if gotQuery != "limit=20" {
		t.Errorf("downstream query = %q; want limit=20", gotQuery)
	}
}

func TestGwProxy_Notifications_405_OnDelete(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodDelete, "/api/notifications", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on DELETE", w.Code)
	}
}

// -----------------------------------------------------------------------------
// GET /api/v1/me/proofing-tests — Virgin Proofing Test list (CHO-2040 R8-6).
// The POST runner is CompanionBridge-owned; this is the learner-scoped LIST
// leaf (optional ?goal_id= filter) proxied verbatim to chora-consumption.
// -----------------------------------------------------------------------------

func TestGwProxy_ProofingTests_200_ForwardsQuery(t *testing.T) {
	var gotPath, gotQuery string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		if r.Header.Get("gcid") != "gcid-001" {
			t.Errorf("downstream lowercase gcid = %q; want gcid-001", r.Header.Get("gcid"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/v1/me/proofing-tests?goal_id=goal-9", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/v1/me/proofing-tests" {
		t.Errorf("downstream path = %q; want /v1/me/proofing-tests", gotPath)
	}
	if gotQuery != "goal_id=goal-9" {
		t.Errorf("downstream query = %q; want goal_id=goal-9", gotQuery)
	}
}

func TestGwProxy_ProofingTests_405_OnPost(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/v1/me/proofing-tests", `{}`)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on POST", w.Code)
	}
}

// -----------------------------------------------------------------------------
// wrong-method 405s on the remaining GET-only routes + downstream 4xx/5xx
// pass-through
// -----------------------------------------------------------------------------

func TestGwProxy_KGClusters_405_OnPost(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/me/knowledge-graph/clusters", `{}`)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on POST /api/me/knowledge-graph/clusters", w.Code)
	}
}

func TestGwProxy_ListCompanions_405_OnPost(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/me/companions", `{}`)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on POST /api/me/companions", w.Code)
	}
}

func TestGwProxy_CompanionGrowth_405_OnPost(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/me/companions/fam-1/growth", `{}`)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on POST /api/me/companions/{id}/growth", w.Code)
	}
}

func TestGwProxy_TenantByID_405_OnPost(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/tenants/mtm-singapore", `{}`)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on POST /api/tenants/{id}", w.Code)
	}
}

func TestGwProxy_CourseByID_405_OnDelete(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodDelete, "/api/courses/course-1", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on DELETE /api/courses/{id}", w.Code)
	}
}

// A downstream domain 4xx (e.g. a real 404) must pass through verbatim — it
// proves the request reached the downstream handler (NOT a gateway-edge 404).
func TestGwProxy_DownstreamDomain404_PassesThrough(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"tenant not found"}`))
	})
	h := newGwProxyMux(t, stub)
	// The tenant asked for is the caller's own: this test is about a
	// downstream 4xx reaching the client verbatim, and "ghost" was an
	// incidental choice that also happened to be a cross-tenant read.
	w := doGwProxyReqAs(t, h, http.MethodGet, "/api/tenants/tenant-001", "tenant-001", "owner")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 passed through verbatim", w.Code)
	}
	if !strings.Contains(w.Body.String(), "tenant not found") {
		t.Errorf("body = %q; want downstream body passed through", w.Body.String())
	}
}

// A downstream 5xx must normalise to 502 GATEWAY_UPSTREAM_5XX.
func TestGwProxy_Downstream5xx_Normalises502(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/notifications", "")
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 on downstream 5xx", w.Code)
	}
	if !strings.Contains(w.Body.String(), "GATEWAY_UPSTREAM_5XX") {
		t.Errorf("body = %q; want GATEWAY_UPSTREAM_5XX envelope", w.Body.String())
	}
}

// -----------------------------------------------------------------------------
// A6 follow-up (CHO-1545): POST /api/atoms/{id}/session + .../session/submit
// -----------------------------------------------------------------------------

func TestGwProxy_StartAtomSession_201_ProxiesToConsumption(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"session_id":"sess-1","atom_id":"atom-cspo-1"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/atoms/atom-cspo-1/session", "")
	if w.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201", w.Code)
	}
	if gotPath != "/v1/me/atom-sessions" {
		t.Errorf("downstream path = %q; want /v1/me/atom-sessions", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("downstream method = %s; want POST", gotMethod)
	}
	if !strings.Contains(gotBody, `"atom_id":"atom-cspo-1"`) {
		t.Errorf("downstream body = %q; want synthesised atom_id", gotBody)
	}
}

func TestGwProxy_StartAtomSession_405_OnGet(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/atoms/atom-1/session", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on GET /api/atoms/{id}/session", w.Code)
	}
}

func TestGwProxy_SubmitAtomSession_200_ProxiesToConsumptionAnswers(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"session_id":"sess-1","status":"completed","is_correct":true}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/atoms/atom-cspo-1/session/submit",
		`{"session_id":"sess-1","answer_id":"ans-1","answer_index":1}`)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/v1/me/atom-sessions/sess-1/answers" {
		t.Errorf("downstream path = %q; want /v1/me/atom-sessions/sess-1/answers", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("downstream method = %s; want POST", gotMethod)
	}
	if strings.Contains(gotBody, "session_id") {
		t.Errorf("downstream body = %q; session_id must be stripped (in path)", gotBody)
	}
	if !strings.Contains(gotBody, `"answer_id":"ans-1"`) {
		t.Errorf("downstream body = %q; want answer fields forwarded", gotBody)
	}
}

func TestGwProxy_SubmitAtomSession_400_OnMissingSessionID(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/atoms/atom-1/session/submit", `{"answer_index":1}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 when session_id missing from body", w.Code)
	}
}

func TestGwProxy_SubmitAtomSession_405_OnGet(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/atoms/atom-1/session/submit", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on GET /api/atoms/{id}/session/submit", w.Code)
	}
}

func TestGwProxy_AtomsUnknownSubpath_404(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/atoms/atom-1/session/bogus", `{}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on unknown /api/atoms/{id}/session/bogus subpath", w.Code)
	}
}

// matchesGatewayProxyPath must own ONLY the /session leaves under
// /api/atoms/ — GET /api/atoms/{id} + POST /api/atoms/{id}/feedback +
// /api/atoms/ai-assist stay Phyllis-owned and must fall through to the
// base router.
func TestWithGatewayProxy_AtomNonSessionPath_FallsThroughToPhyllis(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	agg := gatewayproxy.New(gatewayproxy.Config{
		ConsumptionURL: stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)
	for _, p := range []string{
		"/api/atoms/atom-1",          // GET atom detail — Phyllis
		"/api/atoms/atom-1/feedback", // POST feedback — Phyllis
		"/api/atoms/ai-assist",       // QGen — Phyllis
	} {
		r := httptest.NewRequest(http.MethodGet, p, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusTeapot {
			t.Errorf("path %q: status = %d; want 418 — non-session atom path must fall through to Phyllis", p, w.Code)
		}
	}
}

// CHO-1692 hot-fix — the gatewayproxy subtree prefix "/api/tenants/" was
// silently capturing the auth-context-aware /api/tenants/me endpoint and
// treating "me" as a literal tenant_id, which 404s on chora-tenancy. The
// carve-out in matchesGatewayProxyPath must let /api/tenants/me fall
// through to the inner phyllis_handler so the URL gets rewritten with
// the caller's real tenant_id from the validated JWT.
func TestWithGatewayProxy_TenantsMePath_FallsThroughToPhyllis(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	agg := gatewayproxy.New(gatewayproxy.Config{
		TenancyURL:     stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)
	r := httptest.NewRequest(http.MethodGet, "/api/tenants/me", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTeapot {
		t.Errorf("status = %d; want 418 — /api/tenants/me must fall through to Phyllis (got captured by the gatewayproxy /api/tenants/ subtree)", w.Code)
	}
}

// Belt-and-braces: an actual {uuid} path under /api/tenants/ is STILL
// owned by the gatewayproxy bridge (handleTenantByID). The carve-out
// must be limited to the literal "me" segment.
func TestWithGatewayProxy_TenantsByID_StillOwnedByBridge(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"tenant_id":"019e95f6-1b70-7175-8238-470092d85314"}`))
	})
	agg := gatewayproxy.New(gatewayproxy.Config{
		TenancyURL:     stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)
	r := httptest.NewRequest(http.MethodGet, "/api/tenants/019e95f6-1b70-7175-8238-470092d85314", nil)
	r.Header.Set("Authorization", "Bearer t")
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusTeapot {
		t.Error("/api/tenants/{uuid} leaked to base — bridge must still own the parametric subtree")
	}
}

// CHO-1883 (2026-06-26) — the A+ Wallet mana ledger leaf. The dispatcher
// claimed /api/v1/me/mana (+ /topup) as EXACT paths but NOT
// /api/v1/me/mana/ledger, so the ledger leaked to Phyllis → 404 at the edge
// (mana-parity handoff §2.4). The bridge must own the leaf and proxy it to
// chora-identity (listManaLedger), restoring the per-row transaction read.
func TestWithGatewayProxy_MeManaLedger_OwnedByBridge(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	var gotPath string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	agg := gatewayproxy.New(gatewayproxy.Config{
		IdentityURL:    stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me/mana/ledger?page_size=20", nil)
	r.Header.Set("Authorization", "Bearer t")
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusTeapot {
		t.Fatal("/api/v1/me/mana/ledger leaked to base — bridge must own the ledger leaf (was 404 at the edge)")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 proxied from chora-identity", w.Code)
	}
	if gotPath != "/api/v1/me/mana/ledger" {
		t.Errorf("downstream path = %q; want /api/v1/me/mana/ledger", gotPath)
	}
}

func TestGwProxy_MeManaLedger_405OnNonGET(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/v1/me/mana/ledger", `{}`)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on POST /api/v1/me/mana/ledger", w.Code)
	}
}

// CHO-1883 — the Mana Pool ledger CSV/NDJSON export is a STREAMING download:
// the bridge must own /ledger/export and preserve Content-Type +
// Content-Disposition (the generic JSON passthrough would force application/json).
func TestWithGatewayProxy_MeManaLedgerExport_StreamsDownload(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="mana-ledger-x.csv"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("recorded_at,direction,units\n2026-06-26T00:00:00Z,debit,10\n"))
	})
	agg := gatewayproxy.New(gatewayproxy.Config{IdentityURL: stub.URL, PerCallTimeout: time.Second})
	h := httpadapter.WithGatewayProxy(base, agg)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me/mana/ledger/export?format=csv", nil)
	r.Header.Set("Authorization", "Bearer t")
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusTeapot {
		t.Fatal("/api/v1/me/mana/ledger/export leaked to base — bridge must own the export leaf")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 streamed from chora-identity", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type = %q; want text/csv preserved (NOT JSON-forced)", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q; want attachment preserved", cd)
	}
	if !strings.Contains(w.Body.String(), "debit,10") {
		t.Error("csv body was not streamed through verbatim")
	}
}

func TestGwProxy_MeManaLedgerExport_405OnNonGET(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/v1/me/mana/ledger/export", `{}`)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on POST /api/v1/me/mana/ledger/export", w.Code)
	}
}

func TestWithGatewayProxy_AtomSessionPath_HandledByBridge(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"session_id":"sess-1"}`))
	})
	agg := gatewayproxy.New(gatewayproxy.Config{
		ConsumptionURL: stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/atoms/atom-1/session", nil)
	r.Header.Set("Authorization", "Bearer t")
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusTeapot {
		t.Error("atom session path leaked to base — bridge must own /api/atoms/{id}/session")
	}
	if w.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201 from the bridge", w.Code)
	}
}

// -----------------------------------------------------------------------------
// WithGatewayProxy composition
// -----------------------------------------------------------------------------

func TestWithGatewayProxy_NilAggregator_PassesThrough(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := httpadapter.WithGatewayProxy(base, nil)
	r := httptest.NewRequest(http.MethodGet, "/api/feature-flags", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTeapot {
		t.Errorf("status = %d; want 418 — nil aggregator must pass through to base", w.Code)
	}
}

func TestWithGatewayProxy_NonProxyPath_FallsThroughToBase(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	agg := gatewayproxy.New(gatewayproxy.Config{
		TenancyURL:     stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)
	// /api/catalog is owned by the Phyllis aggregator — must fall through.
	r := httptest.NewRequest(http.MethodGet, "/api/catalog", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTeapot {
		t.Errorf("status = %d; want 418 — non-proxy path must fall through to base", w.Code)
	}
}

func TestWithGatewayProxy_ProxyPath_HandledByBridge(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	agg := gatewayproxy.New(gatewayproxy.Config{
		TenancyURL:     stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/feature-flags", nil)
	r.Header.Set("Authorization", "Bearer t")
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusTeapot {
		t.Error("proxy path leaked to base — bridge must own /api/feature-flags")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 from the bridge", w.Code)
	}
}

// -----------------------------------------------------------------------------
// P7 (2026-05-15) — manual question authoring routes
//
// POST   /api/atoms/{atom_id}/questions                       (FE Phase C wiring)
// PATCH  /api/atoms/{atom_id}/questions/{question_id}         (FE Phase B wiring)
// GET    /api/atoms/{atom_id}/questions/{question_id}         (FE author projection)
// DELETE /api/atoms/{atom_id}/questions/{question_id}         (FE A18 wiring)
//
// All routes forward to chora-creation at the SAME path. Bridge claims these
// /api/atoms/* shapes via matchesGatewayProxyPath (new isAtomQuestionPath
// predicate); other /api/atoms/* shapes (GET /api/atoms/{id} +
// /api/atoms/{id}/feedback + /api/atoms/ai-assist) stay Phyllis-owned.
// -----------------------------------------------------------------------------

func TestGwProxy_QuestionsCreate_201_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
		}
		gotBody = string(buf)
		if r.Header.Get("gcid") != "gcid-001" {
			t.Errorf("downstream lowercase gcid = %q; want gcid-001", r.Header.Get("gcid"))
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"question":{"question_id":"01970000-0000-7000-8000-0000000000q1","type":"mcq"}}`))
	})
	h := newGwProxyMux(t, stub)
	body := `{"type":"mcq","prompt":"P7 smoke","mcq":{"options":[]}}`
	w := doGwProxyReq(t, h, http.MethodPost, "/api/atoms/00000000-0000-7000-8000-00000000a0a2/questions", body)
	if w.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201", w.Code)
	}
	if gotPath != "/api/atoms/00000000-0000-7000-8000-00000000a0a2/questions" {
		t.Errorf("downstream path = %q; want verbatim chora-creation path", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("downstream method = %s; want POST", gotMethod)
	}
	if gotBody != body {
		t.Errorf("downstream body = %q; want forwarded verbatim", gotBody)
	}
}

func TestGwProxy_QuestionsEdit_200_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"question":{"question_id":"01970000-0000-7000-8000-0000000000q1","prompt":"P7 v2"}}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPatch,
		"/api/atoms/00000000-0000-7000-8000-00000000a0a2/questions/01970000-0000-7000-8000-0000000000q1",
		`{"prompt":"P7 v2"}`)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/api/atoms/00000000-0000-7000-8000-00000000a0a2/questions/01970000-0000-7000-8000-0000000000q1" {
		t.Errorf("downstream path = %q; want verbatim chora-creation path", gotPath)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("downstream method = %s; want PATCH", gotMethod)
	}
}

func TestGwProxy_QuestionsGet_200_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"question":{"question_id":"01970000-0000-7000-8000-0000000000q1","mcq_payload":{"options":[]}}}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet,
		"/api/atoms/00000000-0000-7000-8000-00000000a0a2/questions/01970000-0000-7000-8000-0000000000q1", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/api/atoms/00000000-0000-7000-8000-00000000a0a2/questions/01970000-0000-7000-8000-0000000000q1" {
		t.Errorf("downstream path = %q; want verbatim chora-creation path", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("downstream method = %s; want GET", gotMethod)
	}
	if !strings.Contains(w.Body.String(), "mcq_payload") {
		t.Errorf("response body = %q; want author projection passthrough", w.Body.String())
	}
}

func TestGwProxy_QuestionsDelete_204_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusNoContent)
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodDelete,
		"/api/atoms/00000000-0000-7000-8000-00000000a0a2/questions/01970000-0000-7000-8000-0000000000q1", "")
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d; want 204", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("body = %q; want empty 204 body passed through", w.Body.String())
	}
	if gotPath != "/api/atoms/00000000-0000-7000-8000-00000000a0a2/questions/01970000-0000-7000-8000-0000000000q1" {
		t.Errorf("downstream path = %q; want verbatim chora-creation path", gotPath)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("downstream method = %s; want DELETE", gotMethod)
	}
}

func TestGwProxy_Questions_409_PassesThrough_DuplicateBody(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"CREATION_QUESTION_DUPLICATE","message":"atom already has a question"}}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost,
		"/api/atoms/00000000-0000-7000-8000-00000000a0a2/questions", `{"type":"mcq"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d; want 409 passed through", w.Code)
	}
	if !strings.Contains(w.Body.String(), "CREATION_QUESTION_DUPLICATE") {
		t.Errorf("body = %q; want CREATION_QUESTION_DUPLICATE passed through verbatim", w.Body.String())
	}
}

func TestGwProxy_QuestionsCreate_405_OnGet(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	// GET on the collection-level /questions path is not a valid route (the
	// canonical author read is GET by question_id). Expect 405.
	w := doGwProxyReq(t, h, http.MethodGet,
		"/api/atoms/00000000-0000-7000-8000-00000000a0a2/questions", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on GET /questions (collection-level)", w.Code)
	}
}

func TestGwProxy_QuestionsLeaf_405_OnPost(t *testing.T) {
	// POST on /questions/{q_id} is not a valid route. Expect 405.
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost,
		"/api/atoms/00000000-0000-7000-8000-00000000a0a2/questions/01970000-0000-7000-8000-0000000000q1",
		`{"prompt":"x"}`)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on POST /questions/{id}", w.Code)
	}
}

// WithGatewayProxy must claim the new /api/atoms/{id}/questions[/{qid}] paths
// — they must NOT fall through to the base (Phyllis) router. Same composition
// test as TestWithGatewayProxy_AtomSessionPath_HandledByBridge but for the
// P7 question routes.
func TestWithGatewayProxy_AtomQuestionsPath_HandledByBridge(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"question":{"question_id":"q1"}}`))
	})
	agg := gatewayproxy.New(gatewayproxy.Config{
		CreationURL:    stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPost, "/api/atoms/atom-1/questions"},
		{http.MethodPatch, "/api/atoms/atom-1/questions/q-1"},
		{http.MethodGet, "/api/atoms/atom-1/questions/q-1"},
		{http.MethodDelete, "/api/atoms/atom-1/questions/q-1"},
	} {
		r := httptest.NewRequestWithContext(context.Background(), tc.method, tc.path, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer t")
		r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusTeapot {
			t.Errorf("%s %s: leaked to base — bridge must own /api/atoms/{id}/questions[/qid]", tc.method, tc.path)
		}
	}
}

// Composition discipline: the existing Phyllis-owned /api/atoms/* paths
// (GET /api/atoms/{id} for the FE A16 read-side; POST /api/atoms/{id}/feedback;
// /api/atoms/ai-assist) MUST continue to fall through to the base router.
// This test repeats TestWithGatewayProxy_AtomNonSessionPath_FallsThroughToPhyllis
// for the P7 era — adding /questions to the bridge must NOT regress those.
func TestWithGatewayProxy_AtomNonQuestionsPath_StillFallsThroughToPhyllis(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	agg := gatewayproxy.New(gatewayproxy.Config{
		ConsumptionURL: stub.URL,
		CreationURL:    stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)
	for _, p := range []string{
		"/api/atoms/atom-1",          // GET atom detail — Phyllis (FE A16 read-side)
		"/api/atoms/atom-1/feedback", // POST feedback — Phyllis
		"/api/atoms/ai-assist",       // QGen — Phyllis
	} {
		r := httptest.NewRequest(http.MethodGet, p, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusTeapot {
			t.Errorf("path %q: status = %d; want 418 — non-questions/non-session atom path must fall through to Phyllis", p, w.Code)
		}
	}
}

// -----------------------------------------------------------------------------
// P7.5 (2026-05-15) — AI single-question async + model-answer adhoc fill
//
// POST /api/atoms/{atom_id}/question-jobs                            (create)
// GET  /api/atoms/{atom_id}/question-jobs/{job_id}                   (poll)
// POST /api/atoms/{atom_id}/question-jobs/{job_id}/accept            (persist)
// POST /api/atoms/{atom_id}/questions/{q_id}/ai-model-answer-jobs    (adhoc fill)
//
// All four leaves forward to chora-creation at the same path. The bridge
// claims these via the isAtomQuestionPath predicate (extended for P7.5) so
// other /api/atoms/* paths stay Phyllis-owned.
// -----------------------------------------------------------------------------

func TestGwProxy_QuestionJobsPOST_202_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod, gotBody, gotCT string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
		}
		gotBody = string(buf)
		if r.Header.Get("gcid") != "gcid-001" {
			t.Errorf("downstream lowercase gcid = %q; want gcid-001", r.Header.Get("gcid"))
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":"j1","status":"queued","poll_url":"/api/atoms/a/question-jobs/j1"}`))
	})
	h := newGwProxyMux(t, stub)
	body := `{"type":"ai_draft","question_type":"mcq","prompt":"x","count":1}`
	w := doGwProxyReq(t, h, http.MethodPost, "/api/atoms/00000000-0000-7000-8000-00000000a0a2/question-jobs", body)
	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d; want 202", w.Code)
	}
	if gotPath != "/api/atoms/00000000-0000-7000-8000-00000000a0a2/question-jobs" {
		t.Errorf("downstream path = %q; want verbatim", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s; want POST", gotMethod)
	}
	if gotBody != body {
		t.Errorf("body = %q; want forwarded verbatim", gotBody)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q; want application/json", gotCT)
	}
}

// Multipart POST — the gateway must forward the request body untouched AND
// preserve the multipart Content-Type (boundary intact). This is the batch
// source-material upload path.
func TestGwProxy_QuestionJobsPOST_Multipart_StreamsBodyAndContentType(t *testing.T) {
	var gotCT, gotBody string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
		}
		gotBody = string(buf)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":"j2","status":"queued"}`))
	})
	h := newGwProxyMux(t, stub)

	multipartBody := "--boundary123\r\nContent-Disposition: form-data; name=\"file\"; filename=\"src.pdf\"\r\nContent-Type: application/pdf\r\n\r\n<pdf bytes>\r\n--boundary123--\r\n"
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/atoms/00000000-0000-7000-8000-00000000a0a2/question-jobs", strings.NewReader(multipartBody))
	r.Header.Set("Authorization", "Bearer testtoken")
	r.Header.Set("X-Tenant-Id", "tenant-001")
	r.Header.Set("Content-Type", "multipart/form-data; boundary=boundary123")
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d; want 202", w.Code)
	}
	if !strings.HasPrefix(gotCT, "multipart/form-data") {
		t.Errorf("Content-Type = %q; want multipart/form-data prefix", gotCT)
	}
	if !strings.Contains(gotCT, "boundary=boundary123") {
		t.Errorf("Content-Type = %q; want boundary preserved", gotCT)
	}
	if gotBody != multipartBody {
		t.Errorf("multipart body forwarded = %q; want verbatim", gotBody)
	}
}

func TestGwProxy_QuestionJobsGET_200_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"job_id":"j1","status":"completed","candidate_questions":[]}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet,
		"/api/atoms/00000000-0000-7000-8000-00000000a0a2/question-jobs/01970000-0000-7000-8000-0000000000j1", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/api/atoms/00000000-0000-7000-8000-00000000a0a2/question-jobs/01970000-0000-7000-8000-0000000000j1" {
		t.Errorf("downstream path = %q; want verbatim", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %s; want GET", gotMethod)
	}
}

func TestGwProxy_QuestionJobsAccept_200_ProxiesToCreation(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"persisted":[],"mana_debited":0}`))
	})
	h := newGwProxyMux(t, stub)
	body := `{"accepted_candidates":[{"draft_id":"d1"}]}`
	w := doGwProxyReq(t, h, http.MethodPost,
		"/api/atoms/00000000-0000-7000-8000-00000000a0a2/question-jobs/01970000-0000-7000-8000-0000000000j1/accept",
		body)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/api/atoms/00000000-0000-7000-8000-00000000a0a2/question-jobs/01970000-0000-7000-8000-0000000000j1/accept" {
		t.Errorf("downstream path = %q; want verbatim", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s; want POST", gotMethod)
	}
	if gotBody != body {
		t.Errorf("body forwarded = %q; want verbatim", gotBody)
	}
}

func TestGwProxy_QuestionJobsRegenerateImage_200_ProxiesToCreation(t *testing.T) {
	// CHO-1822 — the review-stage image regenerate sub-resource must route
	// through the BFF to chora-creation (was a 405 GATEWAY_METHOD_NOT_ALLOWED
	// because the outer matcher only allowed .../accept).
	var gotPath, gotMethod, gotBody string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
		}
		gotBody = string(buf)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":"regen-1","status":"queued"}`))
	})
	h := newGwProxyMux(t, stub)
	body := `{"draft_id":"d1","placement":"stem","prompt":"a clearer diagram"}`
	w := doGwProxyReq(t, h, http.MethodPost,
		"/api/atoms/00000000-0000-7000-8000-00000000a0a2/question-jobs/01970000-0000-7000-8000-0000000000j1/regenerate-image",
		body)
	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d; want 202 (not 405 — route must be wired)", w.Code)
	}
	if gotPath != "/api/atoms/00000000-0000-7000-8000-00000000a0a2/question-jobs/01970000-0000-7000-8000-0000000000j1/regenerate-image" {
		t.Errorf("downstream path = %q; want verbatim", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s; want POST", gotMethod)
	}
	if gotBody != body {
		t.Errorf("body forwarded = %q; want verbatim", gotBody)
	}
}

func TestGwProxy_QuestionJobsRegenerateImage_405_OnGet(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet,
		"/api/atoms/00000000-0000-7000-8000-00000000a0a2/question-jobs/01970000-0000-7000-8000-0000000000j1/regenerate-image", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 (POST only on regenerate-image)", w.Code)
	}
}

func TestGwProxy_ModelAnswerJobsPOST_202_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":"j3","status":"queued"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost,
		"/api/atoms/00000000-0000-7000-8000-00000000a0a2/questions/01970000-0000-7000-8000-0000000000q1/ai-model-answer-jobs",
		`{"tone_hint":"formal"}`)
	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d; want 202", w.Code)
	}
	if gotPath != "/api/atoms/00000000-0000-7000-8000-00000000a0a2/questions/01970000-0000-7000-8000-0000000000q1/ai-model-answer-jobs" {
		t.Errorf("downstream path = %q; want verbatim", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s; want POST", gotMethod)
	}
}

// 503 CREATION_JOBS_NOT_WIRED passes through verbatim — the upstream wired
// envelope must surface so the FE can render the right msg.
func TestGwProxy_QuestionJobs_503_PassesThrough(t *testing.T) {
	notWired := `{"error":{"code":"CREATION_JOBS_NOT_WIRED","message":"mana client unavailable"}}`
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(notWired))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost,
		"/api/atoms/00000000-0000-7000-8000-00000000a0a2/question-jobs", `{"type":"ai_draft","question_type":"mcq","prompt":"x"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d; want 503 passed through", w.Code)
	}
	if !strings.Contains(w.Body.String(), "CREATION_JOBS_NOT_WIRED") {
		t.Errorf("body = %q; want CREATION_JOBS_NOT_WIRED passed through verbatim", w.Body.String())
	}
}

// 402 InsufficientMana passes through verbatim — FE must see the upsell envelope.
func TestGwProxy_QuestionJobs_402_PassesThrough(t *testing.T) {
	manaErr := `{"error":{"code":"INSUFFICIENT_MANA","message":"need 10 mana","upsell":{"required_units":10}}}`
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(manaErr))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost,
		"/api/atoms/00000000-0000-7000-8000-00000000a0a2/question-jobs", `{"type":"ai_draft","question_type":"mcq","prompt":"x"}`)
	if w.Code != http.StatusPaymentRequired {
		t.Errorf("status = %d; want 402 passed through", w.Code)
	}
	if !strings.Contains(w.Body.String(), "INSUFFICIENT_MANA") {
		t.Errorf("body = %q; want INSUFFICIENT_MANA envelope passed through", w.Body.String())
	}
}

// WithGatewayProxy must claim each of the four new P7.5 leaves.
func TestWithGatewayProxy_QuestionJobsPath_HandledByBridge(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":"j"}`))
	})
	agg := gatewayproxy.New(gatewayproxy.Config{
		CreationURL:    stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPost, "/api/atoms/atom-1/question-jobs"},
		{http.MethodGet, "/api/atoms/atom-1/question-jobs/job-1"},
		{http.MethodPost, "/api/atoms/atom-1/question-jobs/job-1/accept"},
		{http.MethodPost, "/api/atoms/atom-1/questions/q-1/ai-model-answer-jobs"},
	} {
		r := httptest.NewRequestWithContext(context.Background(), tc.method, tc.path, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer t")
		r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusTeapot {
			t.Errorf("%s %s: leaked to base — bridge must own /question-jobs[...] + /questions/{qid}/ai-model-answer-jobs", tc.method, tc.path)
		}
	}
}

// 405 method discrimination on the new routes.
func TestGwProxy_QuestionJobsPOST_405_OnGet(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet,
		"/api/atoms/00000000-0000-7000-8000-00000000a0a2/question-jobs", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on GET /question-jobs (collection)", w.Code)
	}
}

func TestGwProxy_QuestionJobsGET_405_OnPost(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost,
		"/api/atoms/00000000-0000-7000-8000-00000000a0a2/question-jobs/01970000-0000-7000-8000-0000000000j1",
		`{}`)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on POST /question-jobs/{id}", w.Code)
	}
}

func TestGwProxy_AcceptPOST_405_OnGet(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet,
		"/api/atoms/00000000-0000-7000-8000-00000000a0a2/question-jobs/01970000-0000-7000-8000-0000000000j1/accept", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on GET /accept", w.Code)
	}
}

func TestGwProxy_ModelAnswerPOST_405_OnGet(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet,
		"/api/atoms/00000000-0000-7000-8000-00000000a0a2/questions/01970000-0000-7000-8000-0000000000q1/ai-model-answer-jobs", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on GET /ai-model-answer-jobs", w.Code)
	}
}

// -----------------------------------------------------------------------------
// QuestionTypes registry — GET /api/atoms/question-types
//
// Bug fixed: the legacy Phyllis /api/atoms/{id} route was claiming the static
// /api/atoms/question-types path (treating "question-types" as an atom_id) and
// wrapping the chora-creation body in {"atom": ...}. The gatewayproxy bridge
// now claims this exact static path BEFORE the parametric atom routes so the
// body is forwarded byte-identical to the FE.
// -----------------------------------------------------------------------------

// TestGwProxy_QuestionTypes_GET_ProxiesToCreation_BodyVerbatim — bridge owns
// the path and forwards the body verbatim. No atom-wrap.
func TestGwProxy_QuestionTypes_GET_ProxiesToCreation_BodyVerbatim(t *testing.T) {
	downstream := `{"items":[{"code":"mcq","label_en":"Multiple Choice","label_zh":null,"enabled":true,"scope":"phyllis"}]}`
	var gotPath, gotMethod string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(downstream))
	})
	h := newGwProxyMux(t, stub)

	w := doGwProxyReq(t, h, http.MethodGet, "/api/atoms/question-types", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/api/atoms/question-types" {
		t.Errorf("downstream path = %q; want /api/atoms/question-types", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("downstream method = %s; want GET", gotMethod)
	}
	body := w.Body.String()
	if body != downstream {
		t.Errorf("body wrapped — got %q; want byte-identical %q", body, downstream)
	}
	if strings.Contains(body, `"atom"`) {
		t.Errorf("body contains atom wrap: %q", body)
	}
	if !strings.Contains(body, `"items"`) {
		t.Errorf("body missing top-level items: %q", body)
	}
}

// TestGwProxy_QuestionTypes_405_OnPost — GET-only static endpoint.
func TestGwProxy_QuestionTypes_405_OnPost(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/atoms/question-types", `{}`)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on POST /api/atoms/question-types", w.Code)
	}
}

// TestWithGatewayProxy_QuestionTypesPath_HandledByBridge — composition test:
// the static /api/atoms/question-types path MUST be claimed by the bridge and
// must NOT fall through to the base (Phyllis) router. This is the core defect
// repro — pre-fix the path leaked to Phyllis's handleAtoms which wrapped the
// body in {"atom": ...}.
func TestWithGatewayProxy_QuestionTypesPath_HandledByBridge(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	agg := gatewayproxy.New(gatewayproxy.Config{
		CreationURL:    stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/api/atoms/question-types", nil)
	r.Header.Set("Authorization", "Bearer t")
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusTeapot {
		t.Error("question-types path leaked to base — bridge must own /api/atoms/question-types")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 from bridge", w.Code)
	}
	// CRITICAL — no atom-wrap.
	if strings.Contains(w.Body.String(), `"atom"`) {
		t.Errorf("body contains atom wrap: %q", w.Body.String())
	}
}

// TestWithGatewayProxy_AtomFetchPath_StillFallsThroughToPhyllis — SANITY:
// the fix MUST NOT regress the canonical /api/atoms/{atom_id} read-side
// which is Phyllis-owned (returns the wrapped {atom: {...}, session: ...}
// composite). Only the static /api/atoms/question-types path is claimed.
func TestWithGatewayProxy_AtomFetchPath_StillFallsThroughToPhyllis(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	agg := gatewayproxy.New(gatewayproxy.Config{
		CreationURL:    stub.URL,
		ConsumptionURL: stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)
	// A real atom_id (UUID-shaped) must continue to fall through to Phyllis
	// — only the literal "question-types" static path is claimed.
	for _, p := range []string{
		"/api/atoms/00000000-0000-7000-8000-00000000a0a2",
		"/api/atoms/atom-001",
		"/api/atoms/new", // chora-creation's empty-AtomDraft template path
	} {
		r := httptest.NewRequest(http.MethodGet, p, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusTeapot {
			t.Errorf("path %q: status = %d; want 418 — atom-fetch must stay Phyllis-owned", p, w.Code)
		}
	}
}

// -----------------------------------------------------------------------------
// B6.1 searchTenantMembers — the bridge claim was REMOVED by the L1
// carve-out (aaa8f906): the bridge's GET-only claim shadowed the
// phyllis-owned POST/PATCH on the same family (walk-caught 405). The
// whole /api/v1/admin/tenant-members family is phyllis-owned now; the
// behavioural pins live in gatewayproxy_tenant_members_carveout_test.go
// (TestWithGatewayProxy_TenantMembersFamily_FallsThroughToPhyllis).
// -----------------------------------------------------------------------------

// -----------------------------------------------------------------------------
// GET /api/v1/instructors/{instructor_gcid}/courses (debt #4 / A6)
// -----------------------------------------------------------------------------

func TestGwProxy_InstructorCourses_200_ProxiesToDeliveryByInstructor(t *testing.T) {
	const instructorGCID = "00000000-0000-7000-8000-000000001999"
	var gotPath, gotRawQ, gotMethod string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRawQ = r.URL.RawQuery
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[],"total":0,"page":1,"per":20}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet,
		"/api/v1/instructors/"+instructorGCID+"/courses?page=2&per=10", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("downstream method = %s; want GET", gotMethod)
	}
	if gotPath != "/api/v1/instructors/"+instructorGCID+"/courses" {
		t.Errorf("downstream path = %q; want /api/v1/instructors/{id}/courses", gotPath)
	}
	if gotRawQ != "page=2&per=10" {
		t.Errorf("downstream rawQuery = %q; want page=2&per=10", gotRawQ)
	}
}

func TestGwProxy_InstructorCourses_404_OnBarePrefix(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/v1/instructors/", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on bare /api/v1/instructors/", w.Code)
	}
}

func TestGwProxy_InstructorCourses_404_OnMissingCoursesLeaf(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet,
		"/api/v1/instructors/00000000-0000-7000-8000-000000001999", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on missing /courses leaf", w.Code)
	}
}

func TestGwProxy_InstructorCourses_405_OnPost(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost,
		"/api/v1/instructors/00000000-0000-7000-8000-000000001999/courses",
		`{}`)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405", w.Code)
	}
}

func TestGwProxy_InstructorCourses_Downstream403_PassesThrough(t *testing.T) {
	const instructorGCID = "00000000-0000-7000-8000-000000001999"
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"Forbidden","message":"role gate"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet,
		"/api/v1/instructors/"+instructorGCID+"/courses", "")
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403 passthrough", w.Code)
	}
}
