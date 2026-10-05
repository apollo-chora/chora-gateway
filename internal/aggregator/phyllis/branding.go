// branding.go — Setup Wizard Phase A BFF aggregator wrapper
// (CHO-1655).
//
// Surfaces `PATCH /api/v1/tenants/me/branding` for the H+ Setup-Tenant
// wizard's step 1. Proxies the request body verbatim to chora-tenancy
// `/api/v1/tenants/me/branding` (canonical mount — see
// services/chora-tenancy/cmd/server/main.go) with the canonical AuthCtx
// propagation (Authorization, traceparent, gcid, X-Tenant-Id).
//
// Sibling stories:
//   - CHO-1405 — parent Setup Wizard
//   - CHO-1655 — Phase A, this file (Branding step BFF route)
//   - CHO-1664 — Phase B planned (Add-ons step)
//   - CHO-1657 — Phase C planned (Wizard shell + Review + Identity stub)
package phyllis

import (
	"context"
	"net/http"
)

// UpdateMeBranding — PATCH /api/v1/tenants/me/branding →
// chora-tenancy `/api/v1/tenants/me/branding`. The caller's tenant_id
// is taken from the validated Chora session JWT via AuthCtx — never
// trusted from the request body. chora-tenancy's handler reads the
// `X-Tenant-Id` header that `phyllis.call()` stamps from
// `AuthCtx.TenantID` and PATCH-merges into the current branding row.
//
// Body shape forwarded verbatim:
//
//	{
//	  "primary_color_hex": "#RRGGBB",
//	  "logo_url":          "https://...",
//	  "custom_domain":     "..."
//	}
//
// Status codes pass through classify():
//   - 200 OK         — branding persisted; body is the canonical
//     BrandingConfig payload
//   - 400 Bad Request — invalid hex / non-http(s) logo URL / bad JSON
//   - 401            — JWT gate or X-Tenant-Id missing
//   - 404 Not Found   — caller's tenant_id no longer exists
//
// Upstream 5xx normalises to 502 via classify(); upstream timeout to 504.
func (a *Aggregator) UpdateMeBranding(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodPatch, a.cfg.TenancyURL+"/api/v1/tenants/me/branding", body, auth))
	}), nil
}
