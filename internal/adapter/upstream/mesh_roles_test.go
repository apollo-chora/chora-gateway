// mesh_roles_test.go — the upstream package must propagate the caller's ROLES.
//
// The bug (CHO-2148 flagged it latent on ObservabilityClient; the audit found it
// is the whole package): upstream.AuthCtx had no Roles field at all, so
//   - HTTPUpstream stamped MeshClaims{GCID, TenantID, RoleSummary} — never
//     x-mesh-user-roles; and
//   - ObservabilityClient stamped NO mesh headers whatsoever (only Accept /
//     traceparent / X-Tenant-Id / Authorization).
//
// Every Chora role gate reads x-mesh-user-roles and FAILS CLOSED (the fail-OPEN
// `rolesAllowed(X-Role)` was deleted in CHO-2072). So the first role gate on ANY
// service reached through these two clients would deny 100% of calls — a working
// route dying with no code change on either side. The O+ kill-switch had to
// hand-roll its own proxy purely to dodge this.
package upstream_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

func TestObservabilityClient_StampsMeshRolesHeader(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agents":[]}`))
	}))
	defer srv.Close()

	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: srv.URL, PerCallTimeout: 2 * time.Second,
	}, nil)

	ctx := upstream.WithAuthCtx(context.Background(), upstream.AuthCtx{
		GCID:     "00000000-0000-7000-8000-000000001999",
		TenantID: "11111111-1111-7111-8111-111111111111",
		Roles:    []string{"platform_operator", "admin"},
	})
	if _, err := oc.GetAgents(ctx, "11111111-1111-7111-8111-111111111111"); err != nil {
		t.Fatalf("GetAgents: %v", err)
	}

	if v := got.Get(servicemesh.HeaderUserRoles); v != "platform_operator,admin" {
		t.Errorf("%s = %q, want %q — without it EVERY fail-closed obs role gate denies "+
			"100%% of the calls made through this client",
			servicemesh.HeaderUserRoles, v, "platform_operator,admin")
	}
	if v := got.Get(servicemesh.HeaderGCID); v != "00000000-0000-7000-8000-000000001999" {
		t.Errorf("%s = %q, want the caller gcid", servicemesh.HeaderGCID, v)
	}
	if v := got.Get(servicemesh.HeaderTenantID); v != "11111111-1111-7111-8111-111111111111" {
		t.Errorf("%s = %q, want the caller tenant", servicemesh.HeaderTenantID, v)
	}
	// The pre-existing header must survive: obs's tenantContext middleware demands
	// X-Tenant-Id on EVERY request (it 400s OBS_TENANT_REQUIRED without it).
	if v := got.Get("X-Tenant-Id"); v != "11111111-1111-7111-8111-111111111111" {
		t.Errorf("X-Tenant-Id = %q — obs tenantContext 400s without it", v)
	}
}

// A role-less caller must send NO roles header — not an empty one. An empty
// header is a distinct value a downstream could mis-parse as a role.
func TestObservabilityClient_NoRolesSendsNoRolesHeader(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agents":[]}`))
	}))
	defer srv.Close()

	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: srv.URL, PerCallTimeout: 2 * time.Second,
	}, nil)
	ctx := upstream.WithAuthCtx(context.Background(), upstream.AuthCtx{GCID: "g", TenantID: "t"})
	if _, err := oc.GetAgents(ctx, "t"); err != nil {
		t.Fatalf("GetAgents: %v", err)
	}
	if _, ok := got[http.CanonicalHeaderKey(servicemesh.HeaderUserRoles)]; ok {
		t.Errorf("%s present for a role-less caller; want absent", servicemesh.HeaderUserRoles)
	}
}

func TestHTTPUpstream_StampsMeshRolesHeader(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	h := upstream.NewHTTPUpstream(upstream.HTTPConfig{
		CreationURL:    srv.URL,
		PerCallTimeout: 2 * time.Second,
	})

	ctx := upstream.WithAuthCtx(context.Background(), upstream.AuthCtx{
		GCID:     "gcid-1",
		TenantID: "tenant-1",
		Roles:    []string{"instructor", "author"},
	})
	if _, err := h.GetRecentAtoms(ctx, "tenant-1", "gcid-1"); err != nil {
		t.Fatalf("GetRecentAtoms: %v", err)
	}

	if got == nil {
		t.Fatal("upstream was never called")
	}
	if v := got.Get(servicemesh.HeaderUserRoles); v != "instructor,author" {
		t.Errorf("%s = %q, want %q — HTTPUpstream serves all 5 BFF surfaces; without roles "+
			"any fail-closed downstream role gate denies 100%% of them",
			servicemesh.HeaderUserRoles, v, "instructor,author")
	}
}
