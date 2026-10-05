// tenancy_me_addons_handler.go — Setup Wizard Phase B BFF handler
// (CHO-1664).
//
// Wraps one BFF route onto the Phyllis aggregator's SubscribeMeAddOns
// fan-out:
//
//	POST  /api/v1/tenants/me/addons   — chora-tenancy:/api/v1/tenants/me/addons
//
// Bearer + traceparent + gcid + tenant headers propagate via
// authCtxFromRequest as every other Phyllis route. JWT validation
// happens upstream via RequireChoraSessionJWT (gated entry added to
// DefaultJWTGatedPrefixes in jwt_auth.go).
package httpadapter

import (
	"net/http"
)

// handleTenantsMeAddons — POST /api/v1/tenants/me/addons (Setup
// Wizard step 2) + GET /api/v1/tenants/me/addons (CHO-1692 re-entry
// hydration). Dispatches on method; everything else returns 405.
func (p *PhyllisHandler) handleTenantsMeAddons(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		resp, _ := p.agg.ListMeAddOns(r.Context(), authCtxFromRequest(r))
		writePhyllisResp(w, resp)
	case http.MethodPost:
		body := readBody(r)
		resp, _ := p.agg.SubscribeMeAddOns(r.Context(), authCtxFromRequest(r), body)
		writePhyllisResp(w, resp)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET or POST only on /api/v1/tenants/me/addons")
	}
}
