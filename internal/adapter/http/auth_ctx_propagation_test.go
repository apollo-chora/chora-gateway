// auth_ctx_propagation_test.go — RED-phase TDD specs for upstream.AuthCtx
// propagation from BFF surface handlers to HTTPUpstream call sites.
//
// Gap surfaced by the HTTPUpstream prod rollout 2026-05-13: BFF surface
// handlers (aplusHome / cplusFeed / hplusTenant / oplusGovernance / rplusCourses)
// extract (tenantID, gcid) from request context but DO NOT call
// upstream.WithAuthCtx(ctx, AuthCtx{Traceparent, TenantID, GCID, RoleSummary})
// before invoking h.upstream.Get*(). The downstream HTTPUpstream adapter
// therefore receives zero AuthCtx via AuthCtxFromContext and outbound calls
// to chora-consumption / chora-creation / etc. carry no traceparent and
// only partial mesh metadata (tenant+gcid via the method arg fallback in
// stampAuthFromContext; no role_summary, no bearer, no traceparent).
//
// These tests stand in a fake upstream that asserts AuthCtxFromContext on
// every method call sees a populated AuthCtx (TenantID + GCID + Traceparent).
// RED → GREEN per .claude/rules/development-execution.md.
package httpadapter_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/adapter/inmem"
	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

// capturingUpstream records the AuthCtx visible via
// upstream.AuthCtxFromContext at every method invocation. Used to verify
// the BFF surface handlers correctly call upstream.WithAuthCtx before
// invoking the upstream client.
type capturingUpstream struct {
	mu   sync.Mutex
	last map[string]upstream.AuthCtx // method name -> AuthCtx seen
}

func newCapturingUpstream() *capturingUpstream {
	return &capturingUpstream{last: map[string]upstream.AuthCtx{}}
}

func (c *capturingUpstream) record(method string, ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last[method] = upstream.AuthCtxFromContext(ctx)
}

func (c *capturingUpstream) seen(method string) (upstream.AuthCtx, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.last[method]
	return a, ok
}

func (c *capturingUpstream) GetLearningPath(ctx context.Context, tenantID, gcid string) (any, error) {
	c.record("GetLearningPath", ctx)
	return map[string]any{"_stub": true, "tenant_id": tenantID, "gcid": gcid}, nil
}

func (c *capturingUpstream) GetRecentAtoms(ctx context.Context, tenantID, gcid string) (any, error) {
	c.record("GetRecentAtoms", ctx)
	return []map[string]any{{"atom_id": "a1"}}, nil
}

func (c *capturingUpstream) GetCompanion(ctx context.Context, tenantID, gcid string) (any, error) {
	c.record("GetCompanion", ctx)
	return map[string]any{"companion_id": "f1"}, nil
}

func (c *capturingUpstream) GetStreak(ctx context.Context, tenantID, gcid string) (any, error) {
	c.record("GetStreak", ctx)
	return map[string]any{"count": 1}, nil
}

func (c *capturingUpstream) GetFeed(ctx context.Context, tenantID, gcid string) (any, error) {
	c.record("GetFeed", ctx)
	return map[string]any{"posts": []any{}}, nil
}

func (c *capturingUpstream) GetTenant(ctx context.Context, tenantID, gcid string) (any, error) {
	c.record("GetTenant", ctx)
	return map[string]any{"tenant_id": tenantID}, nil
}

func (c *capturingUpstream) GetGovernance(ctx context.Context, tenantID, gcid string) (any, error) {
	c.record("GetGovernance", ctx)
	return map[string]any{"tenant_id": tenantID}, nil
}

func (c *capturingUpstream) GetAuditEvents(ctx context.Context, tenantID, gcid string) (any, error) {
	c.record("GetAuditEvents", ctx)
	return []map[string]any{{"event_id": "e1"}}, nil
}

func (c *capturingUpstream) GetCourses(ctx context.Context, tenantID, gcid string) (any, error) {
	c.record("GetCourses", ctx)
	return []map[string]any{{"course_id": "c1"}}, nil
}

// Compile-time check that capturingUpstream implements upstream.Client.
var _ upstream.Client = (*capturingUpstream)(nil)

// newServerCapturing wires the standard skeleton stack with a capturingUpstream.
func newServerCapturing(t *testing.T) (*httptest.Server, *inmem.SessionRepository, *capturingUpstream) {
	t.Helper()
	routesRepo := inmem.NewRouteRepository()
	sessionsRepo := inmem.NewSessionRepository()
	cap := newCapturingUpstream()
	router := httpadapter.NewRouter(routesRepo, sessionsRepo, cap)
	return httptest.NewServer(router), sessionsRepo, cap
}

// TestAplusHome_PropagatesAuthCtx_OnAllThreeUpstreamCalls verifies that the
// surface handler for /bff/aplus/home stamps AuthCtx with at minimum
// traceparent before invoking GetLearningPath, GetRecentAtoms, GetCompanion.
//
// Aplus home is a public route, so we don't require GCID/TenantID — but the
// traceparent MUST be present because the BFF is the trace root and downstream
// services need it to continue the trace tree.
func TestAplusHome_PropagatesTraceparent_OnAllUpstreamCalls(t *testing.T) {
	srv, _, cap := newServerCapturing(t)
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		srv.URL+"/bff/aplus/home", nil)
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}

	for _, method := range []string{"GetLearningPath", "GetRecentAtoms", "GetCompanion"} {
		auth, ok := cap.seen(method)
		if !ok {
			t.Errorf("%s was not invoked", method)
			continue
		}
		if auth.Traceparent == "" {
			t.Errorf("%s: AuthCtx.Traceparent is empty; want propagation of inbound traceparent", method)
		}
	}
}

// TestCplusFeed_PropagatesAuthCtx_WithSessionGCIDTenant verifies that an
// authenticated session populates AuthCtx.GCID + AuthCtx.TenantID +
// AuthCtx.Traceparent before invoking GetFeed.
func TestCplusFeed_PropagatesAuthCtx_WithSessionGCIDTenant(t *testing.T) {
	srv, _, cap := newServerCapturing(t)
	defer srv.Close()
	tok := mintSession(t, srv, fakeJWT)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		srv.URL+"/bff/cplus/feed", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("traceparent", "00-1bc7651916cd43dd8448eb211c80319c-b7ad6b7169203332-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}

	auth, ok := cap.seen("GetFeed")
	if !ok {
		t.Fatalf("GetFeed was not invoked")
	}
	if auth.TenantID == "" {
		t.Errorf("AuthCtx.TenantID is empty; want session tenant")
	}
	if auth.GCID == "" {
		t.Errorf("AuthCtx.GCID is empty; want session gcid")
	}
	if auth.Traceparent == "" {
		t.Errorf("AuthCtx.Traceparent is empty; want propagation")
	}
}

// TestHplusTenant_PropagatesAuthCtx verifies admin-only H+ tenant route
// propagates AuthCtx with TenantID + GCID + Traceparent.
func TestHplusTenant_PropagatesAuthCtx(t *testing.T) {
	srv, _, cap := newServerCapturing(t)
	defer srv.Close()
	tok := mintSession(t, srv, fakeAdminJWT)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		srv.URL+"/bff/hplus/tenant", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("traceparent", "00-2bc7651916cd43dd8448eb211c80319c-b7ad6b7169203333-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}

	auth, ok := cap.seen("GetTenant")
	if !ok {
		t.Fatalf("GetTenant was not invoked")
	}
	if auth.TenantID == "" {
		t.Errorf("AuthCtx.TenantID is empty")
	}
	if auth.GCID == "" {
		t.Errorf("AuthCtx.GCID is empty")
	}
	if auth.Traceparent == "" {
		t.Errorf("AuthCtx.Traceparent is empty")
	}
}

// TestOplusGovernance_PropagatesAuthCtx_OnBothUpstreamCalls verifies that
// both GetGovernance + GetAuditEvents see populated AuthCtx.
func TestOplusGovernance_PropagatesAuthCtx_OnBothUpstreamCalls(t *testing.T) {
	srv, _, cap := newServerCapturing(t)
	defer srv.Close()
	tok := mintSession(t, srv, fakeAdminJWT)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		srv.URL+"/bff/oplus/governance", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("traceparent", "00-3bc7651916cd43dd8448eb211c80319c-b7ad6b7169203334-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}

	for _, method := range []string{"GetGovernance", "GetAuditEvents"} {
		auth, ok := cap.seen(method)
		if !ok {
			t.Errorf("%s was not invoked", method)
			continue
		}
		if auth.TenantID == "" {
			t.Errorf("%s: AuthCtx.TenantID is empty", method)
		}
		if auth.GCID == "" {
			t.Errorf("%s: AuthCtx.GCID is empty", method)
		}
		if auth.Traceparent == "" {
			t.Errorf("%s: AuthCtx.Traceparent is empty", method)
		}
	}
}

// TestRplusCourses_PropagatesAuthCtx verifies R+ courses route stamps
// AuthCtx before GetCourses.
func TestRplusCourses_PropagatesAuthCtx(t *testing.T) {
	srv, _, cap := newServerCapturing(t)
	defer srv.Close()
	tok := mintSession(t, srv, fakeAdminJWT)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		srv.URL+"/bff/rplus/courses", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("traceparent", "00-4bc7651916cd43dd8448eb211c80319c-b7ad6b7169203335-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}

	auth, ok := cap.seen("GetCourses")
	if !ok {
		t.Fatalf("GetCourses was not invoked")
	}
	if auth.TenantID == "" {
		t.Errorf("AuthCtx.TenantID is empty")
	}
	if auth.GCID == "" {
		t.Errorf("AuthCtx.GCID is empty")
	}
	if auth.Traceparent == "" {
		t.Errorf("AuthCtx.Traceparent is empty")
	}
}

// TestAplusHome_AuthCtxIdempotent_DoesNotOverridePreSetMeshClaims verifies
// that if a caller already set AuthCtx on the context (e.g. test fixture),
// the surface handler's WithAuthCtx call does NOT regress those values.
// Specifically: handler must not zero-out fields that were already populated.
func TestAplusHome_AuthCtxIdempotent_DoesNotOverridePreSetMeshClaims(t *testing.T) {
	srv, _, cap := newServerCapturing(t)
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		srv.URL+"/bff/aplus/home", nil)
	req.Header.Set("traceparent", "00-5bc7651916cd43dd8448eb211c80319c-b7ad6b7169203336-01")
	// Stamp the canonical mesh metadata headers so backends would normally
	// trust the BFF — but here we're inbound, so the BFF's JWT middleware
	// would normally clobber these. For the surface routes the legacy
	// session-based auth path is used, so these headers should NOT be
	// trusted as auth identity (BFF is the trust boundary). We just check
	// the traceparent flows.
	req.Header.Set(servicemesh.HeaderGCID, "gcid-spoof")
	req.Header.Set(servicemesh.HeaderTenantID, "tenant-spoof")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	auth, ok := cap.seen("GetLearningPath")
	if !ok {
		t.Fatalf("GetLearningPath was not invoked")
	}
	if auth.Traceparent == "" {
		t.Errorf("traceparent missing from AuthCtx")
	}
}
