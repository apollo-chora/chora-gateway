// admin_marketplace.go — H+ Marketplace catalog browse BFF proxies
// (CHO-1735).
//
// Two endpoints, both tenant-agnostic catalog reads (no `me` rewrite —
// the catalog is global, not per-tenant). The FE-facing path is
// identical to the BE-facing path:
//
//	GET /api/v1/admin/marketplace/addons?<query>      — paginated list
//	GET /api/v1/admin/marketplace/addons/{addonPlanId} — detail
//
// AuthCtx still propagates (Bearer + gcid + X-Tenant-Id + traceparent)
// because the detail endpoint resolves the `is_installed` flag against
// the calling tenant.
package phyllis

import (
	"context"
	"net/http"
	"net/url"
)

// ListMarketplaceAddons — GET /api/v1/admin/marketplace/addons?<query>.
// Query forwards verbatim (category / tier / cursor / limit per the
// OpenAPI listMarketplaceAddons op).
func (a *Aggregator) ListMarketplaceAddons(ctx context.Context, rawQuery string, auth AuthCtx) (Response, error) {
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.TenancyURL + "/api/v1/admin/marketplace/addons"
		if rawQuery != "" {
			u += "?" + rawQuery
		}
		return classify(a.call(c, http.MethodGet, u, nil, auth))
	}), nil
}

// GetMarketplaceAddonDetail — GET /api/v1/admin/marketplace/addons/{addonPlanId}.
// Response includes is_installed which the BE resolves against the
// X-Tenant-Id header (stamped from AuthCtx.TenantID by a.call).
func (a *Aggregator) GetMarketplaceAddonDetail(ctx context.Context, addonPlanID string, auth AuthCtx) (Response, error) {
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.TenancyURL + "/api/v1/admin/marketplace/addons/" +
			url.PathEscape(addonPlanID)
		return classify(a.call(c, http.MethodGet, u, nil, auth))
	}), nil
}
