// tenancy_setup_handler.go — Setup Wizard Phase C BFF handler (CHO-1682).
//
// Wraps one BFF route onto the Phyllis aggregator's SetupTenant fan-out:
//
//	POST  /api/v1/tenants/setup   — fans out to:
//	                                  chora-identity:/api/v1/tenants/me/idp-providers
//	                                  chora-tenancy:/api/v1/tenants/me/finish-setup
//
// Bearer + traceparent + gcid + tenant headers propagate via
// authCtxFromRequest as every other Phyllis route. JWT validation
// happens upstream via RequireChoraSessionJWT (gated entry added to
// DefaultJWTGatedPrefixes in jwt_auth.go).
package httpadapter

import (
	"net/http"
)

// handleTenantsSetup — POST /api/v1/tenants/setup (Setup Wizard step 4
// Apply). Forwards the request envelope to the SetupTenant aggregator
// after the Chora session JWT gate has validated the caller and
// populated MeshClaims with the caller's TenantID.
func (p *PhyllisHandler) handleTenantsSetup(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	body := readBody(r)
	resp, _ := p.agg.SetupTenant(r.Context(), authCtxFromRequest(r), body)
	writePhyllisResp(w, resp)
}
