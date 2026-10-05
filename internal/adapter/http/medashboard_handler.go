// medashboard_handler.go — HTTP route binding for the A+ multi-role
// dashboard composing aggregator route per
// docs/m13/handoff-fe-to-be-service-2026-05-14.md §A6 GAP #2 (A6
// follow-up CHO-1545; Phyllis steps 1b/6).
//
// Route mounted (requires Bearer JWT — /api/me is already in
// DefaultJWTGatedPrefixes so RequireChoraSessionJWT runs upstream and
// stamps the validated mesh claims onto the request context):
//
//	GET /api/me/dashboard → medashboard composing aggregator
//	                        (fans out to chora-identity /me +
//	                        chora-consumption /v1/me/streak +
//	                        /v1/me/learning-paths)
//
// Composition note: /api/me/dashboard is NOT in the gateway's legacy
// route table. Rather than extending that skeleton table, this handler
// is composed as an OUTER bridge (WithMeDashboard) — the same proven
// pattern as WithGatewayProxy / WithCompanionBridge / WithKGExploreBridge
// — so the bridge owns its exact path and everything else (including the
// Phyllis-owned /api/me + the gatewayproxy-owned /api/me/companions)
// falls through to the base router.
package httpadapter

import (
	"net/http"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/medashboard"
)

// MeDashboardHandler binds the medashboard aggregator to the BFF mux.
type MeDashboardHandler struct {
	agg *medashboard.Aggregator
}

// NewMeDashboardHandler constructs the handler.
func NewMeDashboardHandler(agg *medashboard.Aggregator) *MeDashboardHandler {
	return &MeDashboardHandler{agg: agg}
}

// NewMeDashboardMux returns a mux that serves GET /api/me/dashboard.
// Combine with WithMeDashboard to compose with a base handler that owns
// non-bridge paths.
func NewMeDashboardMux(agg *medashboard.Aggregator) http.Handler {
	h := NewMeDashboardHandler(agg)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/me/dashboard", h.handleDashboard)
	return mux
}

// WithMeDashboard composes a medashboard mux with a base handler: the
// exact path /api/me/dashboard is served by the bridge; everything else
// falls through to `base`. Passes through when `agg` is nil so
// cmd/server/main.go can opt out in unconfigured envs.
func WithMeDashboard(base http.Handler, agg *medashboard.Aggregator) http.Handler {
	if agg == nil {
		return base
	}
	bridgeMux := NewMeDashboardMux(agg)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/me/dashboard" {
			bridgeMux.ServeHTTP(w, r)
			return
		}
		base.ServeHTTP(w, r)
	})
}

// handleDashboard serves GET /api/me/dashboard. The aggregator never
// returns a 5xx (the dashboard is the A+ landing surface — it degrades
// gracefully on partial downstream failure), so this handler simply
// relays the composed DTO.
func (h *MeDashboardHandler) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/me/dashboard")
		return
	}
	// The viewer's IANA zone rides ?tz=, because the home's due-today band is
	// "inside the VIEWER's local day" and the aggregator cannot know that. It
	// is a display hint and never an authorisation input, so it comes off the
	// query string rather than a verified claim; the aggregator echoes back the
	// zone it actually used as bands_tz, and refuses an unknown one to UTC.
	auth := meDashboardAuthFromRequest(r)
	auth.TimeZone = r.URL.Query().Get("tz")
	resp, _ := h.agg.GetDashboard(r.Context(), auth)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if resp.Status == 0 {
		resp.Status = http.StatusInternalServerError
	}
	w.WriteHeader(resp.Status)
	if len(resp.Body) > 0 {
		_, _ = w.Write(resp.Body)
	}
}

// meDashboardAuthFromRequest mirrors gatewayProxyAuthFromRequest /
// authCtxFromRequest for the medashboard aggregator's AuthCtx shape.
// Pulls Bearer + traceparent + tenant + mesh-claims onto the outbound
// fan-out so chora-identity (/me bearer-auth) + chora-consumption
// (requireContext: X-Tenant-Id + lowercase gcid) both see the context
// they require.
func meDashboardAuthFromRequest(r *http.Request) medashboard.AuthCtx {
	tp, _ := r.Context().Value(ctxKeyTraceparent).(string)
	ac := medashboard.AuthCtx{
		Bearer:      bearerToken(r),
		Traceparent: tp,
		TenantID:    r.Header.Get("X-Tenant-Id"),
	}
	if mc, ok := MeshClaimsFromContext(r.Context()); ok {
		ac.GCID = mc.GCID
		if mc.TenantID != "" {
			ac.TenantID = mc.TenantID
		}
		ac.RoleSummary = mc.RoleSummary
		// Typed roles → x-mesh-user-roles downstream. Every Chora role gate fails
		// CLOSED on that header, so dropping it denies 100% of role-gated calls
		// (CHO-2148). Validated claims only — never a client-supplied header.
		if len(mc.Roles) > 0 {
			ac.Roles = append([]string(nil), mc.Roles...)
		}
	}
	// D1.5-style defensive fallback: the dashboard fan-out MUST carry a
	// GCID + TenantID downstream. RequireChoraSessionJWT stamps BOTH
	// MeshClaims and the raw ChoraSession claims; if for any route-ordering
	// reason only the raw claims landed, recover from them.
	if ac.GCID == "" || ac.TenantID == "" {
		if cs, ok := ChoraSessionClaimsFromContext(r.Context()); ok {
			if ac.GCID == "" {
				ac.GCID = cs.GCID
			}
			if ac.TenantID == "" {
				ac.TenantID = cs.TenantID
			}
		}
	}
	return ac
}
