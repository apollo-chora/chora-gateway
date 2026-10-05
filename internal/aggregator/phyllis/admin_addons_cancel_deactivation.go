// admin_addons_cancel_deactivation.go — H+ Add-on lifecycle "undo a
// pending end-of-cycle deactivation" BFF proxy (CHO-1785 follow-up).
//
// Sibling of admin_addons_deactivate.go. The FE addon-management tile's
// "Undo" button posts to the me-style alias
//
//	POST /api/v1/admin/tenants/me/addons/{addonPlanId}:cancel-deactivation
//
// This aggregator method rewrites `me` to AuthCtx.TenantID and forwards
// to chora-tenancy's parametric backend
//
//	POST /api/v1/admin/tenants/{tenantId}/addons/{addonPlanId}:cancel-deactivation
//
// stamping the auth headers via the shared a.call() helper.
//
// Status mapping (BE → BFF):
//
//	200 OK         — deactivation cancelled (or idempotent on ACTIVE).
//	404 Not Found  — subscription not found.
//	409 Conflict   — already DEACTIVATED.
//	403 Forbidden  — RBAC.
//	5xx            — normalised to 502 via classify().
package phyllis

import (
	"context"
	"net/http"
	"net/url"
)

// CancelDeactivationAdminMyAddon — POST :cancel-deactivation alias.
// Empty TenantID short-circuits to 400 GATEWAY_TENANT_NOT_RESOLVED so a
// misconfigured token never hits the upstream with a malformed path.
func (a *Aggregator) CancelDeactivationAdminMyAddon(ctx context.Context, addonPlanID string, auth AuthCtx) (Response, error) {
	if auth.TenantID == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_TENANT_NOT_RESOLVED",
			"tenant_id missing from auth context"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.TenancyURL + "/api/v1/admin/tenants/" +
			url.PathEscape(auth.TenantID) + "/addons/" +
			url.PathEscape(addonPlanID) + ":cancel-deactivation"
		// No body — the BE endpoint takes an empty POST.
		return classify(a.call(c, http.MethodPost, u, nil, auth))
	}), nil
}
