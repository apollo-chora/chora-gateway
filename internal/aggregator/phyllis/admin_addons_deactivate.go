// admin_addons_deactivate.go — H+ Add-on Lifecycle deactivation modal
// BFF proxy (CHO-1731, STITCH-H-ADD-2).
//
// Sibling of admin_addons.go (CHO-1698, STITCH-H-ADD-1). The FE at
// `/h/addons` opens the deactivation modal and POSTs to the `me`-style
// alias `POST /api/v1/admin/tenants/me/addons/{addonPlanId}:deactivate`.
// This aggregator method rewrites `me` to AuthCtx.TenantID and forwards
// to chora-tenancy's parametric backend
//
//	POST /api/v1/admin/tenants/{tenantId}/addons/{addonPlanId}:deactivate
//
// preserving the deactivation request body (reason / reason_text /
// effective_at per the OpenAPI DeactivateAddonRequest schema) and
// stamping the auth headers (X-Tenant-Id / gcid / traceparent /
// Authorization) via the shared a.call() helper.
//
// Status mapping (BE → BFF):
//
//	200 OK         — immediate deactivation succeeded.
//	202 Accepted   — deactivation scheduled (effective_at in the future).
//	404 Not Found  — subscription not found / already deactivated.
//	423 Locked     — compliance-locked add-on; FE renders lock state.
//	5xx            — normalised to 502 via classify() (cascade-safe).
package phyllis

import (
	"context"
	"net/http"
	"net/url"
)

// DeactivateAdminMyAddon — POST /api/v1/admin/tenants/me/addons/{addonPlanId}:deactivate →
// chora-tenancy POST /api/v1/admin/tenants/{tenantId}/addons/{addonPlanId}:deactivate.
// tenantId comes from the validated JWT (AuthCtx.TenantID); a missing
// tenant short-circuits to 400 before the upstream call. addonPlanID is
// PathEscape'd defensively so traversal-style segments stay contained
// in the last URL component.
func (a *Aggregator) DeactivateAdminMyAddon(ctx context.Context, addonPlanID string, body []byte, auth AuthCtx) (Response, error) {
	if auth.TenantID == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_TENANT_NOT_RESOLVED",
			"tenant_id missing from auth context"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		// Build the canonical backend URL. The `:deactivate` action suffix
		// must remain on the last segment — net/url.PathEscape leaves the
		// `:` alone (it's a valid path char), so concatenation is safe.
		u := a.cfg.TenancyURL + "/api/v1/admin/tenants/" +
			url.PathEscape(auth.TenantID) + "/addons/" +
			url.PathEscape(addonPlanID) + ":deactivate"
		return classify(a.call(c, http.MethodPost, u, body, auth))
	}), nil
}
