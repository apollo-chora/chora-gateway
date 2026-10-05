// gatewayproxy_preferences_handler.go — SP2.9 HTTP route handlers for the A+
// dashboard-as-hub preference proxy → chora-identity. Methods on the existing
// GatewayProxyHandler (kept in a dedicated file so the shared 4-list route sync
// in gatewayproxy_handler.go — GatewayProxyPathPrefixes / NewGatewayProxyMux /
// matchesGatewayProxyPath / DefaultJWTGatedPrefixes — is owned by the
// integration step, not this unit; see the SP2.9 report).
//
//	GET /api/me/preferences                   → aggregator.GetPreferences
//	PUT /api/me/preferences/dashboard-layout  → aggregator.SaveDashboardLayout
//	PUT /api/me/preferences/home-layout       → aggregator.SaveHomeLayout (ADR-240)
//
// GCID comes from the validated mesh claims via gatewayProxyAuthFromRequest —
// never the body. The PUT's small JSON payload fits readBody's 1 MiB cap.
package httpadapter

import "net/http"

// handleMePreferences serves the exact /api/me/preferences leaf (GET read of the
// caller's GCID-scoped UI preferences). Verbatim proxy to chora-identity
// /api/v1/me/preferences; a non-GET is 405 (the aggregator always GETs).
func (h *GatewayProxyHandler) handleMePreferences(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	resp, _ := h.agg.GetPreferences(r.Context(), gatewayProxyAuthFromRequest(r))
	writeGatewayProxyResp(w, resp)
}

// handleDashboardLayout serves the exact /api/me/preferences/dashboard-layout
// leaf (PUT upsert of the dashboard layout). The aggregator edge-validates the
// order (unknown/duplicate/empty → 422) then proxies to chora-identity
// /api/v1/me/preferences/dashboard-layout; a non-PUT is 405.
func (h *GatewayProxyHandler) handleDashboardLayout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "PUT only")
		return
	}
	body := readBody(r)
	resp, _ := h.agg.SaveDashboardLayout(r.Context(), gatewayProxyAuthFromRequest(r), body)
	writeGatewayProxyResp(w, resp)
}

// handleHomeLayout serves the exact /api/me/preferences/home-layout leaf (PUT
// upsert of the shell /home launcher layout, ADR-240 Track B). Per D11 the
// gateway does NOT validate the body - not the pin-id vocabulary and not the
// structure - it forwards VERBATIM to chora-identity /api/v1/me/preferences/
// home-layout, which is the sole structural authority. A non-PUT is 405.
func (h *GatewayProxyHandler) handleHomeLayout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "PUT only")
		return
	}
	body := readBody(r)
	resp, _ := h.agg.SaveHomeLayout(r.Context(), gatewayProxyAuthFromRequest(r), body)
	writeGatewayProxyResp(w, resp)
}
