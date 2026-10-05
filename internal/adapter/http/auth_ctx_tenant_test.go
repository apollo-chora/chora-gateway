// auth_ctx_tenant_test.go: where the caller's tenant comes from.
//
// authCtxFromRequest used to seed the tenant from the inbound X-Tenant-Id
// header and then let validated mesh claims override it, but only when those
// claims carried a non-empty tenant. Two consequences followed from the
// ORDERING rather than from any single line.
//
//  1. A validated session whose mesh claims carry no tenant fell back to the
//     header. PLATFORM_OPERATOR is tenant-less by design (ADR-165), so that is
//     an ordinary state, not an edge case.
//  2. The D1.5 fallback to the raw ChoraSession claims is guarded on
//     `ac.TenantID == ""`, and the header had already filled it. So a
//     caller-supplied header PRE-EMPTED the validated session's own tenant,
//     which is the opposite of what that fallback exists to do.
//
// The tenant now comes from validated claims only: mesh claims first, then the
// raw ChoraSession claims. The header is consulted only when no validated
// session of either kind is on the context, which is the pre-existing
// behaviour for routes that legitimately have no session.
package httpadapter

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
)

const (
	headerTenant    = "01931f2a-0000-7000-8000-00000000hhhh"
	validatedTenant = "01931f2a-0000-7000-8000-00000000vvvv"
)

func reqWithTenantHeader() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/catalog", nil)
	r.Header.Set("X-Tenant-Id", headerTenant)
	return r
}

// Case 1: the validated claims win, which they already did.
func TestAuthCtxFromRequest_ValidatedTenantBeatsTheHeader(t *testing.T) {
	r := reqWithTenantHeader()
	r = r.WithContext(withMeshClaims(r.Context(),
		&servicemesh.MeshClaims{GCID: "g-1", TenantID: validatedTenant}))

	if got := authCtxFromRequest(r).TenantID; got != validatedTenant {
		t.Fatalf("tenant = %q, want the validated %q", got, validatedTenant)
	}
}

// Case 2, the one that was wrong. A validated session with no tenant in either
// claim set keeps an EMPTY tenant, so the downstream refusal or the wizard
// guard names it. It must never take the header's value.
func TestAuthCtxFromRequest_ValidatedSessionWithNoTenantDoesNotTakeTheHeader(t *testing.T) {
	r := reqWithTenantHeader()
	r = r.WithContext(withMeshClaims(r.Context(),
		&servicemesh.MeshClaims{GCID: "g-op", TenantID: "", Roles: []string{"platform_operator"}}))

	ac := authCtxFromRequest(r)
	if ac.TenantID != "" {
		t.Fatalf("tenant = %q, want empty: a caller-supplied header stood in for a tenant the session does not have", ac.TenantID)
	}
	if ac.GCID != "g-op" {
		t.Fatalf("gcid = %q, want g-op: the identity still comes from the claims", ac.GCID)
	}
}

// Case 3: no validated session of either kind. The header stands, which is the
// pre-existing behaviour for routes that legitimately have no session.
func TestAuthCtxFromRequest_NoSessionKeepsTheHeader(t *testing.T) {
	if got := authCtxFromRequest(reqWithTenantHeader()).TenantID; got != headerTenant {
		t.Fatalf("tenant = %q, want the header %q", got, headerTenant)
	}
}

// An anonymous caller with no header at all has no tenant, which is what pins
// the public catalogue to visibility=public.
func TestAuthCtxFromRequest_NoSessionNoHeaderIsEmpty(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/catalog", nil)
	if got := authCtxFromRequest(r).TenantID; got != "" {
		t.Fatalf("tenant = %q, want empty", got)
	}
}
