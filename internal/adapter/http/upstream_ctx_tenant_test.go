// upstream_ctx_tenant_test.go — regression guard for the /bff/oplus/costs 503.
//
// Root cause (2026-06-01): RequireChoraSessionJWT stamps the validated
// tenant_id + gcid into MeshClaims → upstream.AuthCtx (via withUpstreamAuthCtx),
// NOT the legacy ctxKeyTenantID context key. upstreamCtx previously read only
// the legacy key, so every JWT-gated /bff/oplus/* handler passed an empty
// tenantID to the observability/governance clients. chora-observability's
// tenantContext middleware then 400'd /api/token-usage/aggregate
// (OBS_TENANT_REQUIRED), and the costs handler short-circuited to a 503.
//
// These are internal (package httpadapter) tests because upstreamCtx is
// unexported.
package httpadapter

import (
	"context"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

// TestUpstreamCtx_PrefersAuthCtxIdentity locks the fix: when withUpstreamAuthCtx
// has stamped the session-JWT tenant/gcid into the upstream.AuthCtx, upstreamCtx
// MUST return those so the observability/governance clients forward a non-empty
// X-Tenant-Id.
func TestUpstreamCtx_PrefersAuthCtxIdentity(t *testing.T) {
	const wantTenant = "11111111-1111-7111-8111-111111111111"
	const wantGCID = "gcid-oplus-admin"

	ctx := upstream.WithAuthCtx(context.Background(), upstream.AuthCtx{
		TenantID: wantTenant,
		GCID:     wantGCID,
	})

	gotTenant, gotGCID := upstreamCtx(ctx)
	if gotTenant != wantTenant {
		t.Errorf("tenant = %q; want %q (the AuthCtx tenant, not the empty legacy key)", gotTenant, wantTenant)
	}
	if gotGCID != wantGCID {
		t.Errorf("gcid = %q; want %q (the AuthCtx gcid)", gotGCID, wantGCID)
	}
}

// TestUpstreamCtx_FallsBackToLegacyKeys verifies behaviour is preserved for
// callers that never stamped an AuthCtx (e.g. genericProxy reads r.Context()
// directly): the legacy tenant_id / gcid context keys still resolve.
func TestUpstreamCtx_FallsBackToLegacyKeys(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxKeyTenantID, "legacy-tenant")
	ctx = context.WithValue(ctx, ctxKeyGcid, "legacy-gcid")

	gotTenant, gotGCID := upstreamCtx(ctx)
	if gotTenant != "legacy-tenant" {
		t.Errorf("tenant = %q; want legacy fallback %q", gotTenant, "legacy-tenant")
	}
	if gotGCID != "legacy-gcid" {
		t.Errorf("gcid = %q; want legacy fallback %q", gotGCID, "legacy-gcid")
	}
}

// TestUpstreamCtx_EmptyContext returns empty strings for a guest (no AuthCtx,
// no legacy keys) — the documented guest-view behaviour.
func TestUpstreamCtx_EmptyContext(t *testing.T) {
	gotTenant, gotGCID := upstreamCtx(context.Background())
	if gotTenant != "" || gotGCID != "" {
		t.Errorf("upstreamCtx(empty) = (%q,%q); want empty", gotTenant, gotGCID)
	}
}
