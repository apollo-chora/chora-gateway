// tenancy_ownership_handler.go: the BFF routes for the tenant ownership
// handover (UX Track U, E3 slice 6, S7-B5).
//
// Six routes, one registrar. The me-routes sit behind requireActiveTenant for
// the same reason the wizard routes do (E1): a validated session with no active
// organisation would otherwise be proxied into chora-tenancy's 401 "caller
// tenant required", which is wrong in both halves, since the caller IS
// authenticated and the remedy is not to sign in again.
//
// The operator override is deliberately NOT behind that guard. PLATFORM_OPERATOR
// holds no tenant_memberships row (ADR-165), so requiring an active tenant would
// make the route unreachable by the only role allowed to use it. Its tenant is
// the path.
//
// # WHY THE IDS ARE VALIDATED HERE
//
// Both ids land in an upstream URL. url.PathEscape in the aggregator already
// makes traversal impossible, so this is not the escape hatch it replaces: it
// is a fail-fast so a malformed id is a named 400 from the BFF rather than a
// round trip that ends in an upstream parse error the caller cannot act on.
package httpadapter

import (
	"net/http"
	"regexp"
	"strings"
)

// Ownership BFF route paths. Exact and parametric mounts, matching the
// discipline of the surrounding router, and matching chora-tenancy's own mounts
// one for one so the proxy is a pass-through rather than a translation.
const (
	PathMeOwnershipOfferBFF  = "/api/v1/tenants/me/ownership/offer"
	PathMeOwnershipOffersBFF = "/api/v1/tenants/me/ownership/offers"

	PatternMeOwnershipSettleBFF = "/api/v1/tenants/me/ownership/offers/{offerId}/{verb}"
	PatternAdminOwnershipBFF    = "/api/v1/admin/tenants/{tenantId}/ownership/offers"
)

// uuidRe is deliberately loose about the version nibble: GCIDs and offer ids are
// UUIDv7, but a v4 id from an older row is still a legitimate id, and this check
// exists to reject a path segment that is not an id at all.
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ownershipVerbs is the closed set the settle route accepts. An open set would
// let a caller name any sub-path on chora-tenancy's offers subtree.
var ownershipVerbs = map[string]bool{"accept": true, "decline": true, "revoke": true}

// registerOwnershipRoutes is the ONE registration site, so the production router
// and the specs mount the same set and a route added elsewhere is visibly not
// part of this surface.
func registerOwnershipRoutes(mux *http.ServeMux, ph *PhyllisHandler) {
	mux.HandleFunc(PathMeOwnershipOfferBFF, requireActiveTenant(ph.handleMeOwnershipOffer))
	mux.HandleFunc(PathMeOwnershipOffersBFF, requireActiveTenant(ph.handleMeOwnershipOffers))
	mux.HandleFunc(PatternMeOwnershipSettleBFF, requireActiveTenant(ph.handleMeOwnershipSettle))
	mux.HandleFunc(PatternAdminOwnershipBFF, ph.handleAdminOwnershipOffers)
}

// handleMeOwnershipOffer, GET the tenant's open offer.
func (p *PhyllisHandler) handleMeOwnershipOffer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on "+PathMeOwnershipOfferBFF)
		return
	}
	resp, _ := p.agg.GetMeOwnershipOffer(r.Context(), authCtxFromRequest(r))
	writePhyllisResp(w, resp)
}

// handleMeOwnershipOffers, POST an owner-initiated offer.
func (p *PhyllisHandler) handleMeOwnershipOffers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only on "+PathMeOwnershipOffersBFF)
		return
	}
	resp, _ := p.agg.CreateMeOwnershipOffer(r.Context(), authCtxFromRequest(r), readBody(r))
	writePhyllisResp(w, resp)
}

// handleMeOwnershipSettle, POST accept, decline or revoke on one offer.
func (p *PhyllisHandler) handleMeOwnershipSettle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only on the ownership offer verbs")
		return
	}
	offerID := strings.TrimSpace(r.PathValue("offerId"))
	if !uuidRe.MatchString(offerID) {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST",
			"offer id must be a uuid")
		return
	}
	verb := strings.ToLower(strings.TrimSpace(r.PathValue("verb")))
	if !ownershipVerbs[verb] {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST",
			"unknown ownership action; accept, decline or revoke")
		return
	}
	resp, _ := p.agg.SettleMeOwnershipOffer(r.Context(), authCtxFromRequest(r), offerID, verb)
	writePhyllisResp(w, resp)
}

// handleAdminOwnershipOffers, POST the operator override.
//
// Gated here as well as in chora-tenancy. Two gates on the same decision is
// deliberate: the BFF one keeps a hostile request from becoming a cross-service
// call at all, and the tenancy one means the decision does not depend on this
// service being the only way in.
func (p *PhyllisHandler) handleAdminOwnershipOffers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only on the ownership override")
		return
	}
	if !hasPlatformOperatorRole(r) {
		writeError(w, http.StatusForbidden, "GATEWAY_OPERATOR_REQUIRED",
			"PLATFORM_OPERATOR role required (ADR-165)")
		return
	}
	tenantID := strings.TrimSpace(r.PathValue("tenantId"))
	if !uuidRe.MatchString(tenantID) {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST",
			"tenant id must be a uuid")
		return
	}
	resp, _ := p.agg.AssignTenantOwner(r.Context(), authCtxFromRequest(r), tenantID, readBody(r))
	writePhyllisResp(w, resp)
}
