// addons.go — Setup Wizard Phase B BFF aggregator wrapper (CHO-1664).
//
// Surfaces `POST /api/v1/tenants/me/addons` for the H+ Setup-Tenant
// wizard's step 2. Proxies the request body verbatim to chora-tenancy
// `/api/v1/tenants/me/addons` (canonical mount — see
// services/chora-tenancy/cmd/server/main.go) with the canonical AuthCtx
// propagation (Authorization, traceparent, gcid, X-Tenant-Id).
//
// Sibling stories:
//   - CHO-1405 — parent Setup Wizard
//   - CHO-1655 — Phase A, branding (sibling: branding.go)
//   - CHO-1664 — Phase B, this file (Add-Ons step BFF route)
//   - CHO-1657 — Phase C planned (Wizard shell + Review + Identity stub)
package phyllis

import (
	"context"
	"net/http"
)

// SubscribeMeAddOns — POST /api/v1/tenants/me/addons →
// chora-tenancy `/api/v1/tenants/me/addons`. The caller's tenant_id is
// taken from the validated Chora session JWT via AuthCtx — never
// trusted from the request body. chora-tenancy's handler reads the
// `X-Tenant-Id` + `gcid` headers that `phyllis.call()` stamps from
// AuthCtx; the request body shape is `{"add_on_codes": ["code1", ...]}`
// and the response carries the full set of pending_activation
// subscriptions for the tenant.
//
// Status codes pass through classify():
//   - 200 OK           — subscriptions persisted (newly created or pre-existing)
//   - 400 Bad Request  — unknown add-on code / empty list / malformed JSON
//   - 401              — JWT gate or X-Tenant-Id missing
//
// Upstream 5xx normalises to 502 via classify(); upstream timeout to 504.
func (a *Aggregator) SubscribeMeAddOns(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodPost, a.cfg.TenancyURL+"/api/v1/tenants/me/addons", body, auth))
	}), nil
}

// ListMeAddOns — GET /api/v1/tenants/me/addons (CHO-1692). Reads the
// persisted Setup Wizard add-on selections so the FE's re-entry
// hydration can pre-fill step 2's `selectedAddOns` Set. Empty list is
// 200 with `{"subscriptions":[]}` — a fresh tenant is normal pre-config.
func (a *Aggregator) ListMeAddOns(ctx context.Context, auth AuthCtx) (Response, error) {
	if auth.TenantID == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_TENANT_NOT_RESOLVED",
			"tenant_id missing from auth context"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodGet, a.cfg.TenancyURL+"/api/v1/tenants/me/addons", nil, auth))
	}), nil
}
