// tenant_scope.go: the one rule that decides whether a caller may act on a
// tenant it named in a request.
//
// Two routes have now been found taking the tenant from the caller and never
// joining it to the session: GET /api/v1/admin/readiness (the header) and
// GET /api/tenants/{id} (the path). Both were the same shape, so the rule
// lives here once rather than as two copies that can drift.
//
// The rule: a validated session's tenant decides, and a client-supplied value
// is only ever a REQUEST to be checked against it. PLATFORM_OPERATOR is the
// documented exception, being the sole tenant-less cross-tenant role per
// ADR-165. This is the same principle decodeClosureBody names "the
// identity-from-session rule" and that phyllis_handler.go's authCtxFromRequest
// post-mortem arrived at independently.
package httpadapter

import (
	"net/http"
	"strings"
)

// callerMayReadTenant reports whether the caller's VALIDATED session entitles
// it to act on the tenant it requested.
//
// A session with no tenant is refused rather than allowed to fall through to
// whatever the caller sent. That state is not reachable through the mint today
// (a resolve with zero memberships is a 502 and an unresolvable active tenant
// is a 403, so every minted session carries a tenant), but a gate that is
// correct only because of an invariant in another service is exactly the shape
// phyllis_handler.go had to be repaired out of. PLATFORM_OPERATOR is exempt
// precisely because it is tenant-less by design.
//
// The comparison is against the caller's OWN tenant and never touches the
// requested one, so a refusal is not an existence oracle: a tenant that exists
// and one that does not are indistinguishable from the outside.
func callerMayReadTenant(r *http.Request, requested string) bool {
	if hasPlatformOperatorRole(r) {
		return true
	}
	session := sessionTenantID(r)
	return session != "" && strings.EqualFold(session, requested)
}

// sessionTenantID reads the caller's VALIDATED tenant: mesh claims first, raw
// Chora-session claims as the defence-in-depth fallback. Same precedence as
// sessionRoles and requireMeshIdentity, and deliberately never the inbound
// header, which is what the JWT middleware exists to replace.
func sessionTenantID(r *http.Request) string {
	if mc, ok := MeshClaimsFromContext(r.Context()); ok && mc != nil {
		if t := strings.TrimSpace(mc.TenantID); t != "" {
			return t
		}
	}
	if cs, ok := ChoraSessionClaimsFromContext(r.Context()); ok && cs != nil {
		return strings.TrimSpace(cs.TenantID)
	}
	return ""
}
