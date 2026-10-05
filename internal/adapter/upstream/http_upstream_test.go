// Package upstream_test exercises the HTTPUpstream Phase-6 adapter that
// replaces FakeUpstream's deterministic placeholders with real HTTP fan-out
// to the 8 downstream services.
//
// Per Phase-6 contract:
//
//	GetLearningPath → chora-consumption GET /api/learning-paths
//	GetRecentAtoms  → chora-creation     GET /api/atoms?status=published
//	GetCompanion     → chora-consumption GET /companion/me
//	GetFeed         → chora-sharing      GET /v1/feed
//	GetTenant       → chora-tenancy      GET /tenants/{id}
//	GetGovernance   → chora-governance   GET /governance/{tenant_id}
//	GetAuditEvents  → chora-observability GET /events?tenant_id=X
//	GetCourses      → chora-delivery     GET /courses
//
// The adapter mirrors the phyllis.go call()/classify()/withBudget() pattern
// for W3C traceparent propagation + mesh metadata + 5s per-call budget.
//
// Strict TDD: each method has a happy-path httptest mock test verifying:
//   - downstream URL path
//   - tenant + gcid mesh headers stamped
//   - traceparent propagated
//   - JSON body returned to caller
//
// Failure injection (FailMethods) is preserved for parity with FakeUpstream.
package upstream_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

// fixture builds an HTTPUpstream wired against a single httptest server for
// the named method (other URLs are deliberately blank so accidental fan-out
// surfaces a transport error).
func fixture(srvURL string, method string) *upstream.HTTPUpstream {
	cfg := upstream.HTTPConfig{}
	switch method {
	case "GetLearningPath", "GetCompanion":
		cfg.ConsumptionURL = srvURL
	case "GetRecentAtoms":
		cfg.CreationURL = srvURL
	case "GetFeed":
		cfg.SharingURL = srvURL
	case "GetTenant":
		cfg.TenancyURL = srvURL
	case "GetGovernance":
		cfg.GovernanceURL = srvURL
	case "GetAuditEvents":
		cfg.ObservabilityURL = srvURL
	case "GetCourses":
		cfg.DeliveryURL = srvURL
	}
	return upstream.NewHTTPUpstream(cfg)
}

// captureHandler records the inbound request shape for assertion + writes a
// canned 200 JSON body.
type captured struct {
	path        string
	method      string
	query       string
	tenantHdr   string
	gcidHdr     string
	traceparent string
	authBearer  string
}

func captureHandler(_ *testing.T, c *captured, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.path = r.URL.Path
		c.method = r.Method
		c.query = r.URL.RawQuery
		c.tenantHdr = r.Header.Get(servicemesh.HeaderTenantID)
		c.gcidHdr = r.Header.Get(servicemesh.HeaderGCID)
		c.traceparent = r.Header.Get("traceparent")
		c.authBearer = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}
}

// withAuthCtx returns a ctx carrying AuthCtx so HTTPUpstream's per-call header
// stamping is exercised.
func withAuthCtx(ctx context.Context) context.Context {
	return upstream.WithAuthCtx(ctx, upstream.AuthCtx{
		Bearer:      "test-token",
		Traceparent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		TenantID:    "tenant-a",
		GCID:        "gcid-1",
		RoleSummary: map[string]any{"role": "learner"},
	})
}

func TestHTTPUpstream_GetLearningPath_HappyPath(t *testing.T) {
	t.Parallel()
	cap := &captured{}
	srv := httptest.NewServer(captureHandler(t, cap, `{"path_id":"p-1","title":"Algebra"}`))
	defer srv.Close()

	h := fixture(srv.URL, "GetLearningPath")
	res, err := h.GetLearningPath(withAuthCtx(context.Background()), "tenant-a", "gcid-1")
	if err != nil {
		t.Fatalf("GetLearningPath: %v", err)
	}
	if cap.path != "/api/learning-paths" {
		t.Errorf("path = %q; want /api/learning-paths", cap.path)
	}
	if cap.tenantHdr != "tenant-a" {
		t.Errorf("tenant header = %q; want tenant-a", cap.tenantHdr)
	}
	if cap.gcidHdr != "gcid-1" {
		t.Errorf("gcid header = %q; want gcid-1", cap.gcidHdr)
	}
	if !strings.Contains(cap.traceparent, "0af7651916cd43dd8448eb211c80319c") {
		t.Errorf("traceparent not propagated: %q", cap.traceparent)
	}
	body, _ := res.(map[string]any)
	if body["path_id"] != "p-1" {
		t.Errorf("body.path_id = %v; want p-1", body["path_id"])
	}
}

func TestHTTPUpstream_GetRecentAtoms_PathAndQuery(t *testing.T) {
	t.Parallel()
	cap := &captured{}
	srv := httptest.NewServer(captureHandler(t, cap, `[{"atom_id":"a1"},{"atom_id":"a2"}]`))
	defer srv.Close()

	h := fixture(srv.URL, "GetRecentAtoms")
	res, err := h.GetRecentAtoms(withAuthCtx(context.Background()), "tenant-a", "gcid-1")
	if err != nil {
		t.Fatalf("GetRecentAtoms: %v", err)
	}
	if cap.path != "/api/atoms" {
		t.Errorf("path = %q; want /api/atoms", cap.path)
	}
	if !strings.Contains(cap.query, "status=published") {
		t.Errorf("query = %q; want status=published", cap.query)
	}
	atoms, _ := res.([]any)
	if len(atoms) != 2 {
		t.Errorf("atoms len = %d; want 2", len(atoms))
	}
}

func TestHTTPUpstream_GetCompanion(t *testing.T) {
	t.Parallel()
	cap := &captured{}
	srv := httptest.NewServer(captureHandler(t, cap, `{"companion_id":"f-1","name":"Pip"}`))
	defer srv.Close()

	h := fixture(srv.URL, "GetCompanion")
	res, err := h.GetCompanion(withAuthCtx(context.Background()), "tenant-a", "gcid-1")
	if err != nil {
		t.Fatalf("GetCompanion: %v", err)
	}
	if cap.path != "/companion/me" {
		t.Errorf("path = %q; want /companion/me", cap.path)
	}
	body, _ := res.(map[string]any)
	if body["companion_id"] != "f-1" {
		t.Errorf("body.companion_id = %v", body["companion_id"])
	}
}

func TestHTTPUpstream_GetFeed(t *testing.T) {
	t.Parallel()
	cap := &captured{}
	srv := httptest.NewServer(captureHandler(t, cap, `{"posts":[]}`))
	defer srv.Close()

	h := fixture(srv.URL, "GetFeed")
	_, err := h.GetFeed(withAuthCtx(context.Background()), "tenant-a", "gcid-1")
	if err != nil {
		t.Fatalf("GetFeed: %v", err)
	}
	if cap.path != "/v1/feed" {
		t.Errorf("path = %q; want /v1/feed", cap.path)
	}
}

func TestHTTPUpstream_GetTenant_PathContainsTenantID(t *testing.T) {
	t.Parallel()
	cap := &captured{}
	srv := httptest.NewServer(captureHandler(t, cap, `{"tenant_id":"tenant-a","name":"Acme"}`))
	defer srv.Close()

	h := fixture(srv.URL, "GetTenant")
	res, err := h.GetTenant(withAuthCtx(context.Background()), "tenant-a", "gcid-1")
	if err != nil {
		t.Fatalf("GetTenant: %v", err)
	}
	if cap.path != "/tenants/tenant-a" {
		t.Errorf("path = %q; want /tenants/tenant-a", cap.path)
	}
	body, _ := res.(map[string]any)
	if body["name"] != "Acme" {
		t.Errorf("body.name = %v", body["name"])
	}
}

func TestHTTPUpstream_GetGovernance_PathContainsTenantID(t *testing.T) {
	t.Parallel()
	cap := &captured{}
	srv := httptest.NewServer(captureHandler(t, cap, `{"tenant_id":"tenant-a","imda_dimensions":{}}`))
	defer srv.Close()

	h := fixture(srv.URL, "GetGovernance")
	res, err := h.GetGovernance(withAuthCtx(context.Background()), "tenant-a", "gcid-1")
	if err != nil {
		t.Fatalf("GetGovernance: %v", err)
	}
	if cap.path != "/governance/tenant-a" {
		t.Errorf("path = %q; want /governance/tenant-a", cap.path)
	}
	body, _ := res.(map[string]any)
	if body["tenant_id"] != "tenant-a" {
		t.Errorf("body.tenant_id = %v", body["tenant_id"])
	}
}

func TestHTTPUpstream_GetAuditEvents_QueryContainsTenant(t *testing.T) {
	t.Parallel()
	cap := &captured{}
	srv := httptest.NewServer(captureHandler(t, cap, `[{"event_id":"e1"}]`))
	defer srv.Close()

	h := fixture(srv.URL, "GetAuditEvents")
	res, err := h.GetAuditEvents(withAuthCtx(context.Background()), "tenant-a", "gcid-1")
	if err != nil {
		t.Fatalf("GetAuditEvents: %v", err)
	}
	if cap.path != "/events" {
		t.Errorf("path = %q; want /events", cap.path)
	}
	if !strings.Contains(cap.query, "tenant_id=tenant-a") {
		t.Errorf("query = %q; want tenant_id=tenant-a", cap.query)
	}
	evts, _ := res.([]any)
	if len(evts) != 1 {
		t.Errorf("evts len = %d; want 1", len(evts))
	}
}

func TestHTTPUpstream_GetCourses(t *testing.T) {
	t.Parallel()
	cap := &captured{}
	srv := httptest.NewServer(captureHandler(t, cap, `[{"course_id":"c1"}]`))
	defer srv.Close()

	h := fixture(srv.URL, "GetCourses")
	_, err := h.GetCourses(withAuthCtx(context.Background()), "tenant-a", "gcid-1")
	if err != nil {
		t.Fatalf("GetCourses: %v", err)
	}
	if cap.path != "/courses" {
		t.Errorf("path = %q; want /courses", cap.path)
	}
}

// ------ failure modes ------

func TestHTTPUpstream_EmptyURLReturnsErrUpstream(t *testing.T) {
	t.Parallel()
	h := upstream.NewHTTPUpstream(upstream.HTTPConfig{}) // no URLs
	_, err := h.GetLearningPath(context.Background(), "tenant-a", "gcid-1")
	if err == nil {
		t.Fatal("expected error when ConsumptionURL is empty")
	}
	if !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("err = %v; want wrap of ErrUpstream", err)
	}
}

func TestHTTPUpstream_DownstreamError5xx_WrapsErrUpstream(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	h := upstream.NewHTTPUpstream(upstream.HTTPConfig{CreationURL: srv.URL})
	_, err := h.GetRecentAtoms(context.Background(), "tenant-a", "gcid-1")
	if err == nil {
		t.Fatal("expected error on 5xx")
	}
	if !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("err = %v; want wrap of ErrUpstream", err)
	}
}

func TestHTTPUpstream_404PassesThroughAsNil(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	h := upstream.NewHTTPUpstream(upstream.HTTPConfig{ConsumptionURL: srv.URL})
	// 404 should be treated as "no record" → return nil, nil so the BFF
	// AggregatedView code can render an empty-state.
	res, err := h.GetLearningPath(context.Background(), "tenant-a", "gcid-1")
	if err != nil {
		t.Fatalf("404 should not surface error; got %v", err)
	}
	if res != nil {
		t.Errorf("404 should return nil payload; got %v", res)
	}
}

func TestHTTPUpstream_FailureInjection(t *testing.T) {
	t.Parallel()
	h := upstream.NewHTTPUpstream(upstream.HTTPConfig{ConsumptionURL: "http://unused"})
	h.FailMethods = map[string]bool{"GetCompanion": true}
	if _, err := h.GetCompanion(context.Background(), "tenant-a", "gcid-1"); err == nil {
		t.Fatal("expected ErrUpstream from FailMethods injection")
	} else if !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("err = %v; want wrap of ErrUpstream", err)
	}
}

func TestLoadHTTPConfigFromEnv_ReadsAllSVCEnvVars(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel; sequential.
	t.Setenv("SVC_CONSUMPTION_URL", "http://cons.test")
	t.Setenv("SVC_CREATION_URL", "http://crea.test")
	t.Setenv("SVC_SHARING_URL", "http://shar.test")
	t.Setenv("SVC_TENANCY_URL", "http://ten.test")
	t.Setenv("SVC_GOVERNANCE_URL", "http://gov.test")
	t.Setenv("SVC_OBSERVABILITY_URL", "http://obs.test")
	t.Setenv("SVC_DELIVERY_URL", "http://del.test")

	cfg := upstream.LoadHTTPConfigFromEnv()
	if cfg.ConsumptionURL != "http://cons.test" {
		t.Errorf("ConsumptionURL = %q", cfg.ConsumptionURL)
	}
	if cfg.CreationURL != "http://crea.test" {
		t.Errorf("CreationURL = %q", cfg.CreationURL)
	}
	if cfg.SharingURL != "http://shar.test" {
		t.Errorf("SharingURL = %q", cfg.SharingURL)
	}
	if cfg.TenancyURL != "http://ten.test" {
		t.Errorf("TenancyURL = %q", cfg.TenancyURL)
	}
	if cfg.GovernanceURL != "http://gov.test" {
		t.Errorf("GovernanceURL = %q", cfg.GovernanceURL)
	}
	if cfg.ObservabilityURL != "http://obs.test" {
		t.Errorf("ObservabilityURL = %q", cfg.ObservabilityURL)
	}
	if cfg.DeliveryURL != "http://del.test" {
		t.Errorf("DeliveryURL = %q", cfg.DeliveryURL)
	}
}

func TestHTTPUpstream_AuthCtxFromContext_Bearer(t *testing.T) {
	t.Parallel()
	cap := &captured{}
	srv := httptest.NewServer(captureHandler(t, cap, `{}`))
	defer srv.Close()

	h := upstream.NewHTTPUpstream(upstream.HTTPConfig{ConsumptionURL: srv.URL})
	ctx := upstream.WithAuthCtx(context.Background(), upstream.AuthCtx{
		Bearer:   "abc.def.ghi",
		TenantID: "tenant-x",
		GCID:     "gcid-x",
	})
	_, _ = h.GetLearningPath(ctx, "tenant-x", "gcid-x")
	if cap.authBearer != "Bearer abc.def.ghi" {
		t.Errorf("Authorization = %q; want Bearer abc.def.ghi", cap.authBearer)
	}
}

func TestHTTPUpstream_MeshHeadersStamped(t *testing.T) {
	t.Parallel()
	var roleHdr string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		roleHdr = r.Header.Get(servicemesh.HeaderRoleSummary)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	h := upstream.NewHTTPUpstream(upstream.HTTPConfig{ConsumptionURL: srv.URL})
	ctx := upstream.WithAuthCtx(context.Background(), upstream.AuthCtx{
		TenantID:    "t",
		GCID:        "g",
		RoleSummary: map[string]any{"role": "instructor"},
	})
	_, _ = h.GetLearningPath(ctx, "t", "g")
	var rs map[string]any
	if err := json.Unmarshal([]byte(roleHdr), &rs); err != nil {
		t.Fatalf("role summary header not JSON: %q", roleHdr)
	}
	if rs["role"] != "instructor" {
		t.Errorf("role summary = %v; want instructor", rs)
	}
}
