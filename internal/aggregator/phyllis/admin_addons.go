// admin_addons.go — H+ Add-on Lifecycle dashboard BFF proxy
// (CHO-1698, STITCH-H-ADD-1).
//
// The FE at `/h/addons` calls `GET /api/v1/admin/tenants/me/addons` —
// a `me`-style alias kept stable across tenants (no tenantId in the
// URL leaks into FE state). This aggregator method rewrites the path
// to the parametric backend mount
// `/api/v1/admin/tenants/{tenantId}/addons` with tenantId pulled from
// the validated JWT (AuthCtx.TenantID), then forwards to chora-tenancy
// via the shared `a.call()` which stamps X-Tenant-Id + gcid + traceparent.
//
// The POST + PATCH + DELETE half of the admin-addon lifecycle (activate
// / change tier / deactivate / usage_record / usage_flush) stays on its
// existing parametric routes — only the FE's read-side list needs the
// `me` alias to keep the URL hygienic.
package phyllis

import (
	"context"
	"net/http"
	"net/url"
)

// ListAdminMyAddons — GET /api/v1/admin/tenants/me/addons →
// chora-tenancy GET /api/v1/admin/tenants/{tenantId}/addons. tenantId
// comes from the validated JWT (AuthCtx.TenantID); a missing tenant
// short-circuits to 400 before the upstream call.
func (a *Aggregator) ListAdminMyAddons(ctx context.Context, auth AuthCtx) (Response, error) {
	if auth.TenantID == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_TENANT_NOT_RESOLVED",
			"tenant_id missing from auth context"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.TenancyURL + "/api/v1/admin/tenants/" +
			url.PathEscape(auth.TenantID) + "/addons"
		return classify(a.call(c, http.MethodGet, u, nil, auth))
	}), nil
}
