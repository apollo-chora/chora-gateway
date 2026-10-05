// admin_addons_usage.go — H+ Add-on Lifecycle usage analytics BFF
// proxy (CHO-1732, STITCH-H-ADD-3).
//
// Sibling of admin_addons.go (CHO-1698, STITCH-H-ADD-1) +
// admin_addons_deactivate.go (CHO-1731, STITCH-H-ADD-2). The FE at
// `/h/addons/{addonPlanId}/usage` reads from the `me`-style alias
//
//	GET /api/v1/admin/tenants/me/addons/{addonPlanId}/usage?<query>
//
// This aggregator method rewrites `me` to AuthCtx.TenantID and
// forwards to chora-tenancy's parametric backend
//
//	GET /api/v1/admin/tenants/{tenantId}/addons/{addonPlanId}/usage?<query>
//
// preserving the query string (from / to / granularity per the OpenAPI
// getAddonUsage op) and stamping the auth headers (X-Tenant-Id / gcid /
// traceparent / Authorization) via the shared a.call() helper.
package phyllis

import (
	"context"
	"net/http"
	"net/url"
)

// GetAdminMyAddonUsage — GET /api/v1/admin/tenants/me/addons/{addonPlanId}/usage?<query> →
// chora-tenancy GET /api/v1/admin/tenants/{tenantId}/addons/{addonPlanId}/usage?<query>.
// tenantId comes from the validated JWT (AuthCtx.TenantID); a missing
// tenant short-circuits to 400 before the upstream call. rawQuery is
// the request URL's RawQuery field — pass through verbatim so the BE
// can apply its own defaults when empty.
func (a *Aggregator) GetAdminMyAddonUsage(ctx context.Context, addonPlanID, rawQuery string, auth AuthCtx) (Response, error) {
	if auth.TenantID == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_TENANT_NOT_RESOLVED",
			"tenant_id missing from auth context"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.TenancyURL + "/api/v1/admin/tenants/" +
			url.PathEscape(auth.TenantID) + "/addons/" +
			url.PathEscape(addonPlanID) + "/usage"
		if rawQuery != "" {
			u += "?" + rawQuery
		}
		return classify(a.call(c, http.MethodGet, u, nil, auth))
	}), nil
}
