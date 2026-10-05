// upstream_auth_ctx.go — bridges the BFF request context to the
// upstream.AuthCtx surface.
//
// Why: HTTPUpstream (services/chora-gateway/internal/adapter/upstream/
// http_upstream.go) stamps W3C traceparent + mesh metadata (chora-gcid,
// chora-tenant-id, chora-role-summary) on every outbound HTTP call by
// reading upstream.AuthCtxFromContext(ctx). If the BFF handlers don't
// first call upstream.WithAuthCtx(ctx, AuthCtx{...}), the adapter sees
// zero AuthCtx and the outbound call has no traceparent + only partial
// mesh metadata.
//
// What this file does: returns a child context that carries an
// upstream.AuthCtx populated from the existing chora-gateway request
// context (traceparent from traceContext middleware, tenant_id + gcid
// from authMiddleware session lookup or RequireChoraSessionJWT
// mesh-claims middleware, RoleSummary from mesh claims when present).
// All five BFF
// surface handlers (aplus/cplus/hplus/oplus/rplus) call it before
// invoking the upstream client, closing the Gap 1 surfaced by the
// 2026-05-13 HTTPUpstream prod rollout.
//
// Trust boundary: this helper deliberately does NOT proxy the inbound
// Authorization header. Per S3.6 the BFF is the trust boundary and
// downstream services trust mesh-asserted identity via Cloud Service
// Mesh mTLS — not the session JWT. The Bearer field on AuthCtx is left
// empty for /bff/* surface routes; Phyllis MVP /api/* routes have their
// own authCtxFromRequest path that populates it because those handlers
// re-use the Bearer for downstream identity assertions that haven't
// migrated to the mesh-claims path yet.
package httpadapter

import (
	"context"
	"net/http"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

// withUpstreamAuthCtx returns a child context carrying an upstream.AuthCtx
// populated from the inbound request. Safe to call on requests that arrived
// without auth (guest views) — TenantID + GCID + RoleSummary will be empty
// but Traceparent is always populated (the BFF traceContext middleware
// guarantees one exists on the request context).
func withUpstreamAuthCtx(r *http.Request) context.Context {
	ctx := r.Context()
	auth := upstream.AuthCtx{
		Traceparent: traceparentFromContext(ctx),
		TenantID:    tenantFromContext(ctx),
		GCID:        gcidFromContext(ctx),
	}
	// When the Chora session JWT middleware (RequireChoraSessionJWT) ran
	// upstream of the surface handler, the mesh claims are stashed under
	// meshClaimsCtxKey with the role_summary copied from the validated JWT.
	// Pull that out so downstream services see the role gate via the
	// chora-role-summary mesh header.
	if mc, ok := MeshClaimsFromContext(ctx); ok {
		if mc.TenantID != "" {
			auth.TenantID = mc.TenantID
		}
		if mc.GCID != "" {
			auth.GCID = mc.GCID
		}
		if len(mc.RoleSummary) > 0 {
			auth.RoleSummary = mc.RoleSummary
		}
		// Typed roles → the x-mesh-user-roles header. Downstream role gates fail
		// CLOSED on it, so omitting this denies 100% of role-gated calls (CHO-2148).
		if len(mc.Roles) > 0 {
			auth.Roles = append([]string(nil), mc.Roles...)
		}
	}
	// Fallback to the raw validated ChoraSession claims when only those landed on
	// the context (route-ordering). Still VALIDATED — never a client header.
	if len(auth.Roles) == 0 {
		if cs, ok := ChoraSessionClaimsFromContext(ctx); ok && cs != nil && len(cs.Roles) > 0 {
			auth.Roles = append([]string(nil), cs.Roles...)
		}
	}
	return upstream.WithAuthCtx(ctx, auth)
}

// IdentityFromContextWithAuthCtx returns (tenantID, gcid) from the
// upstream.AuthCtx if present, falling back to the legacy
// tenant_id / gcid context keys. Used by GraphQL resolvers that want to
// continue using the IdentityFromContext signature exposed by the
// httpadapter package.
//
// NOTE: kept here (not in middleware.go) so the upstream.AuthCtx
// extraction co-locates with WithAuthCtx-stamping logic.
func IdentityFromContextWithAuthCtx(ctx context.Context) (string, string) {
	if auth := upstream.AuthCtxFromContext(ctx); auth.TenantID != "" || auth.GCID != "" {
		return auth.TenantID, auth.GCID
	}
	return tenantFromContext(ctx), gcidFromContext(ctx)
}
