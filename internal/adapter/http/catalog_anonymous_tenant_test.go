// catalog_anonymous_tenant_test.go: an anonymous caller of the public
// catalogue carries no tenant.
//
// /api/catalog is public by design: the A+ catalogue route has no guard and
// sits beside the sign-in screen. That is fine, and it is exactly why the
// tenant matters here more than anywhere else.
//
// GetCatalog decides the visibility filter it sends to chora-delivery:
//
//	visibility := "public"
//	if !public && auth.TenantID != "" { visibility = "tenant_or_public" }
//
// and its own comment explains the stakes: "An anonymous caller is ALWAYS
// pinned to visibility=public ... sending tenant_or_public without a tenant
// would leak other tenants' non-public courses."
//
// The general rule in authCtxFromRequest keeps the inbound X-Tenant-Id for
// routes with no validated session, which is right for the routes that have
// never had one. On this route it is not, because the tenant is not inert
// here: it flips the filter. The SPA's own search path calls /api/catalog
// WITHOUT public=true (search.service.ts), so the flip is reachable.
//
// So the handler scrubs it: no validated session means no tenant, whatever the
// caller sent. That is the trusted source for an anonymous caller. No
// chora-master default and no hostname mapping is needed, because the honest
// answer for someone who has not signed in is that they belong to no tenant,
// and that is precisely the value GetCatalog needs to do the right thing.
package httpadapter

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
)

func TestCatalogAuth_AnonymousCallerCarriesNoTenantWhateverTheySend(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/catalog", nil)
	r.Header.Set("X-Tenant-Id", "01931f2a-0000-7000-8000-0000000000ff")

	if got := catalogAuthFromRequest(r).TenantID; got != "" {
		t.Fatalf("tenant = %q, want empty. A caller-supplied tenant flips GetCatalog from visibility=public to tenant_or_public, which returns another tenant's non-public courses to somebody who has not signed in", got)
	}
}

func TestCatalogAuth_SignedInCallerKeepsTheirValidatedTenant(t *testing.T) {
	const validated = "01931f2a-0000-7000-8000-0000000000aa"
	r := httptest.NewRequest(http.MethodGet, "/api/catalog", nil)
	r.Header.Set("X-Tenant-Id", "01931f2a-0000-7000-8000-0000000000ff")
	r = r.WithContext(withMeshClaims(r.Context(),
		&servicemesh.MeshClaims{GCID: "g-1", TenantID: validated}))

	// The signed-in catalogue is allowed to include this tenant's non-public
	// courses; that is the whole point of tenant_or_public.
	if got := catalogAuthFromRequest(r).TenantID; got != validated {
		t.Fatalf("tenant = %q, want the validated %q", got, validated)
	}
}

// A tenant-less operator is signed in but belongs to no tenant, so the
// catalogue they see is the public one. The scrub must not invent a tenant for
// them either.
func TestCatalogAuth_ValidatedSessionWithNoTenantStaysEmpty(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/catalog", nil)
	r.Header.Set("X-Tenant-Id", "01931f2a-0000-7000-8000-0000000000ff")
	r = r.WithContext(withMeshClaims(r.Context(),
		&servicemesh.MeshClaims{GCID: "g-op", TenantID: "", Roles: []string{"platform_operator"}}))

	if got := catalogAuthFromRequest(r).TenantID; got != "" {
		t.Fatalf("tenant = %q, want empty", got)
	}
}
