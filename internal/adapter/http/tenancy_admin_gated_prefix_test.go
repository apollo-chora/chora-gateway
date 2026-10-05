// tenancy_admin_gated_prefix_test.go: the create-org route must stay JWT gated
// (UX Track U, package E1).
//
// The H+ /h/tenants/new screen posts here, and this handler's operator gate is
// the SOLE application-layer authz on the path: chora-tenancy's CreateSubTenant
// server does no role check of its own. hasPlatformOperatorRole and sessionRoles
// both read the VALIDATED mesh claims off the request context, never a
// caller-supplied header, so a request that skips RequireChoraSessionJWT
// arrives with no roles at all.
//
// The failure is quiet in the wrong direction to leave unguarded, and the two
// route lists do not imply each other: claiming a path in
// GatewayProxyPathPrefixes does NOT gate it, and this path is deliberately in
// neither the proxy list nor bridge-owned, because the tenancy-admin handler
// serves it directly.
package httpadapter_test

import (
	"strings"
	"testing"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
)

func TestDefaultJWTGatedPrefixes_CoversSubTenantCreate(t *testing.T) {
	t.Parallel()
	const path = "/api/v1/tenancy/sub-tenants"
	covered := false
	for _, p := range httpadapter.DefaultJWTGatedPrefixes {
		if strings.HasPrefix(path, p) {
			covered = true
			break
		}
	}
	if !covered {
		t.Fatalf("DefaultJWTGatedPrefixes does not cover %q, so RequireChoraSessionJWT would skip it and the handler's operator gate would see no roles on a request that never proved who sent it. This handler is the only application-layer authz on the path: chora-tenancy's CreateSubTenant does no role check. Add the prefix back to jwt_auth.go.", path)
	}
}

// The create route is served by the tenancy-admin handler directly, not by the
// gatewayproxy bridge. Pinning that keeps someone from "fixing" the gate by
// adding it to the proxy list, which would route it somewhere that cannot serve
// it and would not gate it either.
func TestGatewayProxyPathPrefixes_DoesNotClaimSubTenantCreate(t *testing.T) {
	t.Parallel()
	const path = "/api/v1/tenancy/sub-tenants"
	for _, p := range httpadapter.GatewayProxyPathPrefixes {
		if strings.HasPrefix(path, p) {
			t.Fatalf("GatewayProxyPathPrefixes claims %q via %q, but the tenancy-admin handler owns this path. The proxy bridge has no upstream for it, and membership in that list does not gate the route.", path, p)
		}
	}
}
