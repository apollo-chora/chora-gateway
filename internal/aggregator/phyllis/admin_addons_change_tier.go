// admin_addons_change_tier.go — H+ Add-on Lifecycle change-tier BFF
// proxy (CHO-1733, STITCH-H-ADD-4).
//
// Sibling of admin_addons.go (CHO-1698, STITCH-H-ADD-1) +
// admin_addons_deactivate.go (CHO-1731, STITCH-H-ADD-2) +
// admin_addons_usage.go (CHO-1732, STITCH-H-ADD-3). The FE at
// `/h/addons/{addonPlanId}/change-tier` PATCHes the `me`-style alias
//
//	PATCH /api/v1/admin/tenants/me/addons/{addonPlanId}
//
// This aggregator rewrites `me` to AuthCtx.TenantID and forwards to
//
//	PATCH /api/v1/admin/tenants/{tenantId}/addons/{addonPlanId}
//
// preserving the `ChangeAddonTierRequest` body (target_plan_id /
// proration_mode / effective_at per the OpenAPI) and stamping the
// auth headers (X-Tenant-Id / gcid / traceparent / Authorization).
//
// Status mapping (BE → BFF, via classify()):
//
//	200 OK         — tier change applied immediately, response carries
//	                 billing_delta_cents + from_tier + to_tier.
//	202 Accepted   — change scheduled (effective_at in the future),
//	                 response carries schedule_id.
//	404 Not Found  — subscription not found.
//	409 Conflict   — not upgrade-eligible (pending cancellation /
//	                 dunning / terminal).
//	422 Unproc.    — validation (invalid target_plan_id / plan-family
//	                 incompatible).
//	5xx            — normalised to 502 via classify() (cascade-safe).
package phyllis

import (
	"context"
	"net/http"
	"net/url"
)

// ChangeAdminMyAddonTier — PATCH /api/v1/admin/tenants/me/addons/{addonPlanId}
// → chora-tenancy PATCH /api/v1/admin/tenants/{tenantId}/addons/{addonPlanId}.
// tenantId comes from the validated JWT (AuthCtx.TenantID); a missing
// tenant short-circuits to 400 before the upstream call.
func (a *Aggregator) ChangeAdminMyAddonTier(ctx context.Context, addonPlanID string, body []byte, auth AuthCtx) (Response, error) {
	if auth.TenantID == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_TENANT_NOT_RESOLVED",
			"tenant_id missing from auth context"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.TenancyURL + "/api/v1/admin/tenants/" +
			url.PathEscape(auth.TenantID) + "/addons/" +
			url.PathEscape(addonPlanID)
		return classify(a.call(c, http.MethodPatch, u, body, auth))
	}), nil
}
