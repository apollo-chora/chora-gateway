// gatewayproxy_tenant_scope_test.go: RED-first guard for the unjoined
// cross-tenant read on GET /api/tenants/{id}.
//
// THE DEFECT
// ----------
// The route read an arbitrary tenant by id from the URL path with no role gate
// and no join to the caller's session, so any authenticated user of any tenant
// could read any other tenant's record. Five consecutive layers had a place
// where the join belonged and none of them made it:
//
//  1. this handler took the tenant from the path
//  2. gatewayproxy GetTenant put it straight into the upstream path
//  3. chora-tenancy handleTenantByID did Tenants.Get(id) with no comparison
//  4. chora-tenancy tenantRequired asserted X-Tenant-Id was PRESENT, never
//     that it matched anything
//  5. LegacyTenantStore.Get used context.Background(), so no request-scoped
//     RLS GUC could apply, and the tenants table carries no RLS policy at all
//     (23 sibling tables in chora_tenancy do)
//
// Layer 4 is the shape worth naming: a check that asserts presence and never
// compares cannot fail, and it sits exactly where a reader expects the join.
//
// THE TEST SUITE ENCODED THE DEFECT AS THE EXPECTATION
// ----------------------------------------------------
// doGwProxyReq stamps a session for tenant-001. TestGwProxy_TenantByID_200
// then requested mtm-singapore and asserted 200, and
// TestGwProxy_DownstreamDomain404_PassesThrough requested a foreign "ghost".
// Both were green, and both were cross-tenant reads written down as correct
// behaviour. They are updated alongside this file: their subjects (does the
// route proxy to the right downstream path; does a downstream 4xx pass
// through) are unchanged, and only the tenant they ask for moved to the
// caller's own, because neither test was ever about reading someone else's.
//
// THE FIX SHAPE IS THE ONE READINESS TOOK
// ---------------------------------------
// Audience gate then session join, platform_operator excepted as the sole
// tenant-less cross-tenant role (ADR-165). Same shape, same service, so it is
// the pattern rather than a one-off. Every frontend caller of this route is an
// H+ surface (hplus/tenant-overview and hplus/setup in both chora-web and
// chora-spa-v2), which is the Tenant Admin audience, so the role gate matches
// what the product already asks of it and locks nobody out.
package httpadapter_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
)

const (
	scopeOwnTenant     = "tenant-001" // what doGwProxyReq's session carries
	scopeForeignTenant = "mtm-singapore"
	scopeCallerGCID    = "gcid-001"
)

// doGwProxyReqAs is doGwProxyReq with the session tenant and the session roles
// under the test's control. doGwProxyReq pins the session to tenant-001 and
// stamps NO roles, so it cannot express either half of this gate.
func doGwProxyReqAs(t *testing.T, h http.Handler, method, path, sessionTenant string, roles ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", "Bearer testtoken")
	// The inbound header is deliberately set to the REQUESTED tenant, which is
	// what a caller probing this route would send. It must not be what decides.
	r.Header.Set("X-Tenant-Id", scopeForeignTenant)
	r = r.WithContext(httpadapter.InjectMeshClaimsWithRolesForTest(
		r.Context(), scopeCallerGCID, sessionTenant, roles...))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// -----------------------------------------------------------------------------
// The escalation.
// -----------------------------------------------------------------------------

func TestGwProxy_TenantByID_RefusesAForeignTenant(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"mtm-singapore"}`))
	})
	h := newGwProxyMux(t, stub)
	for _, role := range []string{"tenant_admin", "admin", "owner", "super_admin"} {
		w := doGwProxyReqAs(t, h, http.MethodGet, "/api/tenants/"+scopeForeignTenant, scopeOwnTenant, role)
		if w.Code != http.StatusForbidden {
			t.Errorf("role %q: status = %d, want 403; an admin of %s must not read %s",
				role, w.Code, scopeOwnTenant, scopeForeignTenant)
		}
	}
}

// Status alone would pass if the refusal happened after the proxy call, which
// would still have read the other tenant's record.
func TestGwProxy_TenantByID_ForeignTenantNeverReachesDownstream(t *testing.T) {
	var calls int
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	})
	h := newGwProxyMux(t, stub)
	doGwProxyReqAs(t, h, http.MethodGet, "/api/tenants/"+scopeForeignTenant, scopeOwnTenant, "owner")
	if calls != 0 {
		t.Fatalf("downstream was called %d time(s) for a foreign tenant; the refusal must precede the read", calls)
	}
}

// No role gate at all was the other half. Every frontend caller is an H+
// Tenant Admin surface, so a learner reaching this route is not a supported
// case, and reading a tenant record is not a learner capability even for its
// own tenant.
func TestGwProxy_TenantByID_RefusesALearner(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	for _, roles := range [][]string{{"learner"}, {"author", "learner"}, {}} {
		w := doGwProxyReqAs(t, h, http.MethodGet, "/api/tenants/"+scopeOwnTenant, scopeOwnTenant, roles...)
		if w.Code != http.StatusForbidden {
			t.Errorf("roles %v: status = %d, want 403", roles, w.Code)
		}
	}
}

// A session with no tenant must be refused rather than falling through to the
// header. Not reachable through the mint today, but the fix must not rest on a
// mint invariant: that is how the phyllis fall-through got in.
func TestGwProxy_TenantByID_TenantlessAdminSessionCannotBorrowTheHeader(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReqAs(t, h, http.MethodGet, "/api/tenants/"+scopeForeignTenant, "", "owner")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; a session with no tenant must not take one from a header", w.Code)
	}
}

// The refusal must not leak which tenants exist. Before the fix a real foreign
// tenant answered 200 and a made-up one answered the downstream 404, which is
// exactly an existence oracle.
func TestGwProxy_TenantByID_RefusalIsNotAnExistenceOracle(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tenants/ghost" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"tenant not found"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"mtm-singapore"}`))
	})
	h := newGwProxyMux(t, stub)
	real := doGwProxyReqAs(t, h, http.MethodGet, "/api/tenants/"+scopeForeignTenant, scopeOwnTenant, "owner")
	ghost := doGwProxyReqAs(t, h, http.MethodGet, "/api/tenants/ghost", scopeOwnTenant, "owner")

	if real.Code != ghost.Code {
		t.Fatalf("status differs: existing tenant %d vs made-up tenant %d", real.Code, ghost.Code)
	}
	if real.Body.String() != ghost.Body.String() {
		t.Fatalf("body differs:\n existing: %s\n made-up:  %s", real.Body.String(), ghost.Body.String())
	}
}

// -----------------------------------------------------------------------------
// The controls. Each must be able to fail, or the refusals above are just a
// handler that says no to everything.
// -----------------------------------------------------------------------------

func TestGwProxy_TenantByID_AdmitsTenantAdminForItsOwnTenant(t *testing.T) {
	var gotPath string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"tenant-001"}`))
	})
	h := newGwProxyMux(t, stub)
	for _, role := range []string{"tenant_admin", "admin", "owner", "super_admin"} {
		w := doGwProxyReqAs(t, h, http.MethodGet, "/api/tenants/"+scopeOwnTenant, scopeOwnTenant, role)
		if w.Code != http.StatusOK {
			t.Errorf("role %q: status = %d, want 200 for the caller's OWN tenant; body=%s",
				role, w.Code, w.Body.String())
		}
	}
	if gotPath != "/api/tenants/"+scopeOwnTenant {
		t.Errorf("downstream path = %q; want the caller's own tenant", gotPath)
	}
}

// ADR-165 makes platform_operator the sole tenant-less cross-tenant role, so
// it keeps its reach here. Pinned so a later tightening cannot quietly remove
// the one principal the architecture sanctions.
func TestGwProxy_TenantByID_PlatformOperatorReadsAnyTenant(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"mtm-singapore"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReqAs(t, h, http.MethodGet, "/api/tenants/"+scopeForeignTenant, scopeOwnTenant, "platform_operator")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; platform_operator is the sanctioned cross-tenant role; body=%s",
			w.Code, w.Body.String())
	}
}

func TestGwProxy_TenantByID_TenantlessOperatorSessionStillReads(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"mtm-singapore"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReqAs(t, h, http.MethodGet, "/api/tenants/"+scopeForeignTenant, "", "platform_operator")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a tenant-less operator session; body=%s", w.Code, w.Body.String())
	}
}

// Ordering guard: the method check stays ahead of the gates, so a POST is a
// 405 rather than a 403 that hides which methods the route serves.
func TestGwProxy_TenantByID_MethodCheckStillPrecedesTheGates(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReqAs(t, h, http.MethodPost, "/api/tenants/"+scopeOwnTenant, scopeOwnTenant, "learner")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}
