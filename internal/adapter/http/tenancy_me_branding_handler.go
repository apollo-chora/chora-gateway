// tenancy_me_branding_handler.go — Setup Wizard Phase A BFF handler
// (CHO-1655).
//
// Wraps one BFF route onto the Phyllis aggregator's UpdateMeBranding
// fan-out:
//
//	PATCH  /api/v1/tenants/me/branding   — chora-tenancy:/api/v1/tenants/me/branding
//
// Bearer + traceparent + gcid + tenant headers propagate via
// authCtxFromRequest as every other Phyllis route. JWT validation
// happens upstream via RequireChoraSessionJWT (gated entry added to
// DefaultJWTGatedPrefixes in jwt_auth.go).
package httpadapter

import (
	"net/http"
)

// handleTenantsMeBranding — PATCH /api/v1/tenants/me/branding (Setup
// Wizard step 1). Forwards the body to chora-tenancy after the Chora
// session JWT gate has validated the caller and populated MeshClaims
// with the caller's TenantID.
func (p *PhyllisHandler) handleTenantsMeBranding(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPatch) {
		return
	}
	body := readBody(r)
	resp, _ := p.agg.UpdateMeBranding(r.Context(), authCtxFromRequest(r), body)
	writePhyllisResp(w, resp)
}
