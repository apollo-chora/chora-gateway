// auth_ctx.go — bridges the GraphQL resolver context to upstream.AuthCtx.
//
// Why: HTTPUpstream stamps W3C traceparent + mesh metadata (chora-gcid,
// chora-tenant-id, chora-role-summary) on every outbound HTTP call by
// reading upstream.AuthCtxFromContext(ctx). If resolvers don't first
// call upstream.WithAuthCtx(ctx, AuthCtx{...}), the adapter sees zero
// AuthCtx and the outbound call has no traceparent + only partial mesh
// metadata.
//
// What this file does: each resolver method calls
// withResolverAuthCtx(ctx, tenantID, gcid) before invoking r.Upstream.*,
// closing the Gap 1 surfaced by the 2026-05-13 HTTPUpstream prod rollout.
package resolvers

import (
	"context"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
	"github.com/apollo-chora/chora-common/tracing"
	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

// withResolverAuthCtx returns a child context carrying an upstream.AuthCtx
// populated from the inbound resolver context. Traceparent is pulled from
// the tracing package (set by the BFF traceContext middleware upstream of
// the GraphQL handler).
//
// The BFF is the trust boundary per S3.6 — we do NOT proxy the upstream
// identity JWT; downstream services trust mesh-asserted identity via Cloud Service
// Mesh mTLS, so AuthCtx.Bearer stays empty.
//
// Roles come from the VALIDATED mesh claims the JWT middleware attached (via
// servicemesh.WithClaims — the shared key, readable from here without importing
// httpadapter and creating a cycle). They become x-mesh-user-roles downstream.
// Omitting them is not a degradation: every downstream role gate fails CLOSED,
// so a role-less AuthCtx is denied 100% of the time (CHO-2148).
func withResolverAuthCtx(ctx context.Context, tenantID, gcid string) context.Context {
	auth := upstream.AuthCtx{
		Traceparent: tracing.TraceparentFromContext(ctx),
		TenantID:    tenantID,
		GCID:        gcid,
	}
	if mc, ok := servicemesh.ClaimsFromContext(ctx); ok && mc != nil {
		if len(mc.Roles) > 0 {
			auth.Roles = append([]string(nil), mc.Roles...)
		}
		if len(mc.RoleSummary) > 0 {
			auth.RoleSummary = mc.RoleSummary
		}
	}
	return upstream.WithAuthCtx(ctx, auth)
}
