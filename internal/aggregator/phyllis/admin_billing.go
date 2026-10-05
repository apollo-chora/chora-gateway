// admin_billing.go — H+ Billing BFF proxies (CHO-1759 followup):
//
//	GET  /api/v1/admin/tenants/me/invoices       → ListAdminMyInvoices
//	POST /api/v1/admin/tenants/me/billing-portal → CreateAdminMyBillingPortalSession
//
// Both rewrite the `me` alias to AuthCtx.TenantID and forward to
// chora-payments at the canonical
//
//	GET  /api/v1/admin/tenants/{tenantId}/invoices?{rawQuery}
//	POST /api/v1/admin/tenants/{tenantId}/billing-portal
//
// preserving the query string + body verbatim and stamping the auth
// headers (Authorization / gcid / X-Tenant-Id / traceparent).
package phyllis

import (
	"context"
	"net/http"
	"net/url"
)

// ListAdminMyInvoices — GET /api/v1/admin/tenants/me/invoices →
// chora-payments GET /api/v1/admin/tenants/{tenantId}/invoices.
// rawQuery forwarded verbatim so limit / starting_after reach Stripe.
func (a *Aggregator) ListAdminMyInvoices(ctx context.Context, rawQuery string, auth AuthCtx) (Response, error) {
	if auth.TenantID == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_TENANT_NOT_RESOLVED",
			"tenant_id missing from auth context"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.PaymentsURL + "/api/v1/admin/tenants/" +
			url.PathEscape(auth.TenantID) + "/invoices"
		if rawQuery != "" {
			u += "?" + rawQuery
		}
		return classify(a.call(c, http.MethodGet, u, nil, auth))
	}), nil
}

// CreateAdminMyBillingPortalSession — POST /api/v1/admin/tenants/me/billing-portal
// → chora-payments POST /api/v1/admin/tenants/{tenantId}/billing-portal.
// Body forwarded verbatim (return_url).
func (a *Aggregator) CreateAdminMyBillingPortalSession(ctx context.Context, body []byte, auth AuthCtx) (Response, error) {
	if auth.TenantID == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_TENANT_NOT_RESOLVED",
			"tenant_id missing from auth context"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.PaymentsURL + "/api/v1/admin/tenants/" +
			url.PathEscape(auth.TenantID) + "/billing-portal"
		return classify(a.call(c, http.MethodPost, u, body, auth))
	}), nil
}
