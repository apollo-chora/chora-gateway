// admin_external_egress.go — the H+ tenant external web-egress entitlement
// (CHO-2148; ADR-220 D4, ADR-231 D6, PLAN.md §4.2.4).
//
//	GET   /api/v1/admin/tenants/me/external-egress
//	PATCH /api/v1/admin/tenants/me/external-egress
//
// Proxies to chora-tenancy, which OWNS the entitlement. A write there persists
// the policy row and publishes chora.tenancy.external_egress_policy.updated.v1
// in one transaction; chora-observability projects that event into the read-copy
// the model-gateway consults fail-closed on every grounded call.
//
// The `me` segment is resolved from the validated auth context, never from the
// path — a caller cannot name someone else's tenant.
//
// AUTHZ is enforced UPSTREAM, in chora-tenancy (callerHoldsHPlusAdminRole, on the
// mesh roles header, fail-closed). The gateway does no per-route admin gating for
// phyllis routes; it only stamps the mesh headers the upstream reads. Do NOT add
// a permissive check here and assume it is the gate.
package phyllis

import (
	"context"
	"net/http"
	"net/url"
)

// externalEgressPath builds the upstream chora-tenancy URL for the caller's own
// tenant. auth.TenantID comes from the validated session, so `me` can only ever
// resolve to the caller's tenant.
func (a *Aggregator) externalEgressPath(auth AuthCtx) string {
	return a.cfg.TenancyURL + "/api/v1/admin/tenants/" +
		url.PathEscape(auth.TenantID) + "/external-egress"
}

// GetAdminMyExternalEgress reads the caller's tenant external-egress policy.
// A tenant that has never opted in reads back the fail-closed default
// (egress_enabled=false, opted_in=false) — chora-tenancy fabricates no row.
func (a *Aggregator) GetAdminMyExternalEgress(ctx context.Context, auth AuthCtx) (Response, error) {
	if resp, ok := requireTenant(auth); !ok {
		return resp, nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodGet, a.externalEgressPath(auth), nil, auth))
	}), nil
}

// SetAdminMyExternalEgress changes the caller's tenant external-egress policy.
//
// Uses the WRITE budget (30s/40s, CHO-1826): the upstream write commits a row
// AND enqueues the policy-changed event in one transaction, so it is slower than
// a read and must not be cut short by the read budget — a timeout mid-commit
// would leave the admin unsure whether egress is on.
func (a *Aggregator) SetAdminMyExternalEgress(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	if resp, ok := requireTenant(auth); !ok {
		return resp, nil
	}
	return a.withWriteBudget(ctx, func(c context.Context) Response {
		return classify(a.callWrite(c, http.MethodPatch, a.externalEgressPath(auth), body, auth))
	}), nil
}
