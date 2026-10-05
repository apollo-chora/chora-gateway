// admin_addons_detail.go — H+ Add-on Lifecycle marketplace tile detail
// BFF proxy (CHO-1734, STITCH-H-ADD-5 — the FINAL sub-story closing
// epic CHO-1697).
//
// Sibling of admin_addons.go (CHO-1698) + admin_addons_deactivate.go
// (CHO-1731) + admin_addons_usage.go (CHO-1732) +
// admin_addons_change_tier.go (CHO-1733). The FE at
// `/h/addons/{addonPlanId}/detail` reads the full `AddOnDetail` from
// the `me`-style alias
//
//	GET /api/v1/admin/tenants/me/addons/{addonPlanId}
//
// This aggregator rewrites `me` to AuthCtx.TenantID and forwards to
//
//	GET /api/v1/admin/tenants/{tenantId}/addons/{addonPlanId}
//
// The response carries entitlements + integrations + compliance pills +
// pricing_tiers + an optional `current_subscription` snapshot (present
// iff the tenant has an active subscription — drives the FE's
// `Manage` vs `Install` CTA).
//
// Status mapping (BE → BFF, via classify()):
//
//	200 OK         — detail returned (current_subscription may or may
//	                 not be present)
//	404 Not Found  — subscription not found / tenant not subscribed →
//	                 FE renders the "Not currently subscribed" empty
//	                 state.
//	5xx            — normalised to 502 via classify() (cascade-safe).
package phyllis

import (
	"context"
	"net/http"
	"net/url"
)

// GetAdminMyAddonDetail — GET /api/v1/admin/tenants/me/addons/{addonPlanId}
// → chora-tenancy GET /api/v1/admin/tenants/{tenantId}/addons/{addonPlanId}.
// tenantId comes from the validated JWT (AuthCtx.TenantID); a missing
// tenant short-circuits to 400 before the upstream call.
func (a *Aggregator) GetAdminMyAddonDetail(ctx context.Context, addonPlanID string, auth AuthCtx) (Response, error) {
	if auth.TenantID == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_TENANT_NOT_RESOLVED",
			"tenant_id missing from auth context"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.TenancyURL + "/api/v1/admin/tenants/" +
			url.PathEscape(auth.TenantID) + "/addons/" +
			url.PathEscape(addonPlanID)
		return classify(a.call(c, http.MethodGet, u, nil, auth))
	}), nil
}
