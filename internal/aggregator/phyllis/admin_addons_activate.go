// admin_addons_activate.go — H+ Marketplace Activate-Free BFF proxy
// (CHO-1742). Sibling of admin_addons_deactivate.go.
//
// The FE catalog detail screen detects `pricing_tiers[0].monthly_price_cents
// === 0` and renders an `Activate Free` CTA instead of Subscribe. The CTA
// POSTs to the `me`-style alias
//
//	POST /api/v1/admin/tenants/me/addons/{addonPlanId}:activate
//
// which this aggregator rewrites to AuthCtx.TenantID and forwards to
// chora-tenancy's parametric backend
//
//	POST /api/v1/admin/tenants/{tenantId}/addons/{addonPlanId}:activate
//
// preserving the (empty) request body and stamping auth headers
// (X-Tenant-Id / gcid / traceparent / Authorization) via the shared
// a.call() helper.
//
// Why not reuse the Stripe Checkout BFF alias (CHO-1739)? Stripe Checkout
// rejects $0 sessions by design — the gateway's `amount_cents must be > 0`
// gate is correct, and a separate activate path keeps the free flow
// payment-free (no tenant_addon_purchase aggregate, no Stripe webhook
// round-trip; just chora-tenancy SubscriptionRegistry.Subscribe → ACTIVE).
//
// Status mapping (BE → BFF):
//
//	200 OK         — subscription activated (or already ACTIVE — idempotent).
//	404 Not Found  — addon catalog entry missing.
//	422            — validation error from the registry.
//	5xx            — normalised to 502 via classify() (cascade-safe).
package phyllis

import (
	"context"
	"net/http"
	"net/url"
)

// ActivateAdminMyAddon — POST /api/v1/admin/tenants/me/addons/{addonPlanId}:activate →
// chora-tenancy POST /api/v1/admin/tenants/{tenantId}/addons/{addonPlanId}:activate.
// tenantId comes from the validated JWT (AuthCtx.TenantID); a missing
// tenant short-circuits to 400 before the upstream call. addonPlanID is
// PathEscape'd defensively so traversal-style segments stay contained in
// the last URL component.
func (a *Aggregator) ActivateAdminMyAddon(ctx context.Context, addonPlanID string, body []byte, auth AuthCtx) (Response, error) {
	if auth.TenantID == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_TENANT_NOT_RESOLVED",
			"tenant_id missing from auth context"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.TenancyURL + "/api/v1/admin/tenants/" +
			url.PathEscape(auth.TenantID) + "/addons/" +
			url.PathEscape(addonPlanID) + ":activate"
		return classify(a.call(c, http.MethodPost, u, body, auth))
	}), nil
}
