// preferences.go - SP2.9 / ADR-240: GCID-scoped UI-preference proxy to
// chora-identity. The layouts are GCID-scoped account preferences owned by the
// Identity domain (users.ui_preferences JSONB); the gateway is a stateless
// proxy - it holds NO state.
//
//	GET /api/me/preferences                   → chora-identity GET /api/v1/me/preferences
//	PUT /api/me/preferences/dashboard-layout  → chora-identity PUT /api/v1/me/preferences/dashboard-layout
//	PUT /api/me/preferences/home-layout       → chora-identity PUT /api/v1/me/preferences/home-layout
//
// Like the A17 mana routes (GetMyMana), the downstream path is built
// explicitly (chora-identity serves under /api/v1/me/*, not the FE-facing
// /api/me/*). The learner scope is delegated downstream — chora-identity's
// bearerAuth reads the gcid off the stamped mesh claims, NEVER the body
// (project_gateway_bff_tenant_from_authctx).
//
// The dashboard-layout PUT carries a fast edge validation of the layout order
// (unknown/duplicate/empty wrapper key → 422 WITHOUT a downstream round-trip),
// because that order is a CLOSED wrapper enum. The home-layout PUT deliberately
// does the OPPOSITE (ADR-240 D11): it FORWARDS the body verbatim and validates
// NOTHING - the FE pin registry is the single id authority and chora-identity is
// the sole structural authority, so replicating an id check here would recreate
// the hand-mirrored-enum drift that broke dashboard_layout twice (CHO-2274).
// A downstream 422 passes through verbatim on both legs.
package gatewayproxy

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/apollo-chora/chora-common/uiprefs"
)

// knownDashboardWrapperKeys is the closed set the dashboard layout order must be
// a subset/permutation of. It DERIVES from the single canonical source
// (uiprefs.DashboardWrapperKeys) that chora-identity's validator also derives
// from, so the two backend validators can never drift apart by hand. Before
// CHO-2274 this was a hand-maintained copy that fell behind the FE when "study"
// (CHO-2226) and "transcript" (CHO-2237) shipped, so every save 422'd here.
// chora-identity remains the authoritative validator; this is a fast edge guard.
var knownDashboardWrapperKeys = uiprefs.DashboardWrapperKeySet()

// GetPreferences proxies GET /api/me/preferences → chora-identity
// GET /api/v1/me/preferences. Pure passthrough — the snake_case
// {dashboard_layout:{order,updated_at}} body (dashboard_layout omitted when
// unset) passes through verbatim for the FE adapter.
func (a *Aggregator) GetPreferences(ctx context.Context, auth AuthCtx) (Response, error) {
	u := a.cfg.IdentityURL + "/api/v1/me/preferences"
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// SaveDashboardLayout proxies PUT /api/me/preferences/dashboard-layout →
// chora-identity PUT /api/v1/me/preferences/dashboard-layout after an edge
// validation of the order. The FE body {order,updated_at} is forwarded
// verbatim once the order clears the edge guard.
func (a *Aggregator) SaveDashboardLayout(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	if resp, ok := validateDashboardLayoutBody(body); !ok {
		return resp, nil
	}
	u := a.cfg.IdentityURL + "/api/v1/me/preferences/dashboard-layout"
	return classify(a.call(ctx, http.MethodPut, u, body, auth)), nil
}

// SaveHomeLayout proxies PUT /api/me/preferences/home-layout → chora-identity
// PUT /api/v1/me/preferences/home-layout. Per ADR-240 D11 the gateway does NOT
// edge-validate the body at all - NOT the pin-id vocabulary (the FE registry is
// the single authority; an unknown id is inert in a GCID-scoped blob and grants
// nothing) and NOT the structure (chora-identity is the sole structural
// authority). The FE body is forwarded VERBATIM; the downstream's 200/422/… is
// passed through unchanged.
func (a *Aggregator) SaveHomeLayout(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	u := a.cfg.IdentityURL + "/api/v1/me/preferences/home-layout"
	return classify(a.call(ctx, http.MethodPut, u, body, auth)), nil
}

// validateDashboardLayoutBody edge-validates the PUT body: it must be JSON with
// a non-empty `order` that is a subset/permutation of knownDashboardWrapperKeys
// with no duplicates. Returns (errResponse, false) to short-circuit; (_, true)
// to proceed. updated_at format is left to the authoritative downstream.
func validateDashboardLayoutBody(body []byte) (Response, bool) {
	var req struct {
		Order []string `json:"order"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return errResp(http.StatusBadRequest, "GATEWAY_BAD_JSON",
			"request body must be JSON {order,updated_at}"), false
	}
	if len(req.Order) == 0 {
		return errResp(http.StatusUnprocessableEntity, "GATEWAY_INVALID_DASHBOARD_LAYOUT",
			"order must be a non-empty subset of the known dashboard wrappers"), false
	}
	seen := make(map[string]struct{}, len(req.Order))
	for _, k := range req.Order {
		if _, ok := knownDashboardWrapperKeys[k]; !ok {
			return errResp(http.StatusUnprocessableEntity, "GATEWAY_INVALID_DASHBOARD_LAYOUT",
				"unknown dashboard wrapper key: "+k), false
		}
		if _, dup := seen[k]; dup {
			return errResp(http.StatusUnprocessableEntity, "GATEWAY_INVALID_DASHBOARD_LAYOUT",
				"duplicate dashboard wrapper key: "+k), false
		}
		seen[k] = struct{}{}
	}
	return Response{}, true
}
