// tenancy_bootstrap_handler.go — H+ Setup-Tenant Phase 3 BFF handler
// (CHO-1632).
//
// Wraps one BFF route onto the Phyllis aggregator's BootstrapTenant
// fan-out:
//
//	POST   /api/v1/tenants/bootstrap   — chora-tenancy:/v1/tenants/bootstrap
//
// Bearer + traceparent + gcid + tenant headers propagate via
// authCtxFromRequest as every other Phyllis route.
package httpadapter

import (
	"net/http"
)

// handleTenantsBootstrap — POST /api/v1/tenants/bootstrap (H+ self-
// onboard). Forwards the body to chora-tenancy after the Chora session
// JWT gate has validated the caller and populated MeshClaims with the
// caller's GCID.
func (p *PhyllisHandler) handleTenantsBootstrap(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	body := readBody(r)
	resp, _ := p.agg.BootstrapTenant(r.Context(), authCtxFromRequest(r), body)
	writePhyllisResp(w, resp)
}
