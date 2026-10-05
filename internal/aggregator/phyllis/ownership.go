// ownership.go: BFF wrappers for the tenant ownership handover (UX Track U,
// E3 slice 6, first-launch spec 13.3, 13.4 and 13.8).
//
// Every call proxies verbatim to chora-tenancy, which owns the handover rules.
// What this layer contributes is the identity: call() stamps X-Tenant-Id, gcid
// and the mesh roles header from the VALIDATED session, and chora-tenancy gates
// on exactly those. The body carries the nominee and, on the override, the
// reason; it never carries an identity, because the server resolves both sides
// of a handover itself.
package phyllis

import (
	"context"
	"net/http"
	"net/url"
)

// ownershipBase is the me-subtree on chora-tenancy. The tenant is implied by
// the stamped header rather than the path, which is why the caller's session
// has to carry one.
const ownershipBase = "/api/v1/tenants/me/ownership"

// CreateMeOwnershipOffer, POST /api/v1/tenants/me/ownership/offers.
// chora-tenancy refuses a caller who does not hold the live owner row.
func (a *Aggregator) CreateMeOwnershipOffer(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	if auth.TenantID == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_TENANT_NOT_RESOLVED",
			"tenant_id missing from auth context"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodPost, a.cfg.TenancyURL+ownershipBase+"/offers", body, auth))
	}), nil
}

// GetMeOwnershipOffer, GET /api/v1/tenants/me/ownership/offer. Both sides of a
// handover read this one row; a 404 means no offer is open.
func (a *Aggregator) GetMeOwnershipOffer(ctx context.Context, auth AuthCtx) (Response, error) {
	if auth.TenantID == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_TENANT_NOT_RESOLVED",
			"tenant_id missing from auth context"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodGet, a.cfg.TenancyURL+ownershipBase+"/offer", nil, auth))
	}), nil
}

// SettleMeOwnershipOffer answers one offer: verb is accept, decline or revoke.
//
// offerID is PathEscaped even though the handler validates it as a UUID first.
// The validation is the gate; the escaping is the guarantee that this function
// cannot build a path outside the subtree if a future caller skips it.
func (a *Aggregator) SettleMeOwnershipOffer(ctx context.Context, auth AuthCtx, offerID, verb string) (Response, error) {
	if auth.TenantID == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_TENANT_NOT_RESOLVED",
			"tenant_id missing from auth context"), nil
	}
	u := a.cfg.TenancyURL + ownershipBase + "/offers/" + url.PathEscape(offerID) + "/" + url.PathEscape(verb)
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodPost, u, nil, auth))
	}), nil
}

// AssignTenantOwner, POST /api/v1/admin/tenants/{tenantId}/ownership/offers,
// the operator override.
//
// The tenant is the PATH, not the auth context: PLATFORM_OPERATOR is tenant-less
// by design (ADR-165), so an operator's session carries no tenant and there is
// nothing to stamp. chora-tenancy authorises this one on the roles header,
// which call() stamps, and scopes the write to the path tenant inside
// RunInTenantTx with RLS still enforcing.
func (a *Aggregator) AssignTenantOwner(ctx context.Context, auth AuthCtx, tenantID string, body []byte) (Response, error) {
	u := a.cfg.TenancyURL + "/api/v1/admin/tenants/" + url.PathEscape(tenantID) + "/ownership/offers"
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodPost, u, body, auth))
	}), nil
}
