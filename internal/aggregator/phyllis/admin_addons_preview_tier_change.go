// admin_addons_preview_tier_change.go — H+ Add-on Lifecycle
// preview-tier-change BFF proxy (CHO-1768).
//
// Sibling of admin_addons_change_tier.go (CHO-1733). The FE at
// `/h/addons/{addonPlanId}/change-tier` POSTs the `me`-style alias
//
//	POST /api/v1/admin/tenants/me/addons/{addonPlanId}/preview-tier-change
//
// 300ms after each tier-card click. This aggregator rewrites `me` to
// AuthCtx.TenantID and forwards to the canonical chora-tenancy mount
//
//	POST /api/v1/admin/tenants/{tenantId}/addons/{addonPlanId}/preview-tier-change
//
// preserving the PreviewAddonTierRequest body verbatim (target_tier_code
// / proration_mode per the OpenAPI) and stamping the auth headers
// (X-Tenant-Id / gcid / traceparent / Authorization).
//
// Filed as the post-merge gap from epic CHO-1759: CHO-1765 added the
// chora-payments preview endpoint + CHO-1767 added the chora-tenancy
// dispatcher, but neither wired the gateway aggregator. Surfaced during
// the local pre-flight smoke when the FE preview path 404'd at the
// gateway dispatcher's "unrecognised sub-resource" branch.
package phyllis

import (
	"context"
	"net/http"
	"net/url"
)

// PreviewAdminMyAddonTier — POST /api/v1/admin/tenants/me/addons/{addonPlanId}/preview-tier-change
// → chora-tenancy POST /api/v1/admin/tenants/{tenantId}/addons/{addonPlanId}/preview-tier-change.
// tenantId comes from the validated JWT (AuthCtx.TenantID); a missing
// tenant short-circuits to 400 before the upstream call.
func (a *Aggregator) PreviewAdminMyAddonTier(ctx context.Context, addonPlanID string, body []byte, auth AuthCtx) (Response, error) {
	if auth.TenantID == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_TENANT_NOT_RESOLVED",
			"tenant_id missing from auth context"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.TenancyURL + "/api/v1/admin/tenants/" +
			url.PathEscape(auth.TenantID) + "/addons/" +
			url.PathEscape(addonPlanID) + "/preview-tier-change"
		return classify(a.call(c, http.MethodPost, u, body, auth))
	}), nil
}
