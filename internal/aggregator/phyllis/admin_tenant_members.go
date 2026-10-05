// admin_tenant_members.go — L1 Tenant lane BFF proxies (CHO-1708).
//
// Two surfaces:
//
//  1. Identity tenant-members admin (identity-admin.yaml v1.4.0):
//     GET/POST /api/v1/admin/tenant-members + PATCH .../{gcid}/role are
//     forwarded VERBATIM to chora-identity (same path) — query string and
//     body untouched. The identity handlers gate on the Bucket 4
//     `x-mesh-user-roles` mesh header, which `call()` stamps from
//     AuthCtx.Roles.
//
//  2. Tenancy mana-pool me-style alias (tenancy-admin.yaml parametric):
//     /api/v1/admin/tenants/me/mana-pool[:topup] rewrites `me` to the
//     JWT tenant — the ListAdminMyAddons (CHO-1698) precedent; no
//     tenantId in the URL leaks into FE state.
//
// Per the CHO-1632 path-stripping bug class the canonical backend mount
// paths are hard-coded here and pinned by admin_tenant_members_test.go.
package phyllis

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// requireTenant guards the me-style + admin routes: a missing tenant short-
// circuits to 400 before any upstream call (ListAdminMyAddons precedent).
func requireTenant(auth AuthCtx) (Response, bool) {
	if auth.TenantID == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_TENANT_NOT_RESOLVED",
			"tenant_id missing from auth context"), false
	}
	return Response{}, true
}

// authIsOperator reports whether the auth context carries PLATFORM_OPERATOR
// (case-insensitive) — the sole cross-tenant principal (ADR-165).
func authIsOperator(auth AuthCtx) bool {
	for _, r := range auth.Roles {
		if strings.EqualFold(strings.TrimSpace(r), "PLATFORM_OPERATOR") {
			return true
		}
	}
	return false
}

// queryHasFranchisee reports whether the raw query carries a non-empty
// managed_tenant_id — the operator cross-tenant learner-directory selector.
func queryHasFranchisee(rawQuery string) bool {
	v, err := url.ParseQuery(rawQuery)
	if err != nil {
		return false
	}
	return strings.TrimSpace(v.Get("managed_tenant_id")) != ""
}

// SearchAdminTenantMembers — GET /api/v1/admin/tenant-members →
// chora-identity, query string forwarded verbatim.
//
// S4 (CHO-2000): a PLATFORM_OPERATOR targeting ?managed_tenant_id=<uuid>
// performs a cross-tenant learner-directory read. Like GrantTenantMembership it
// does NOT requireTenant — the operator holds no session tenant; the target
// rides the query and chora-identity is the authority (operator gate +
// fail-closed IMDA-D1 audit). The skip is operator-only so a tenant-less admin
// cannot reach identity via the param; all other callers keep the guard.
func (a *Aggregator) SearchAdminTenantMembers(ctx context.Context, auth AuthCtx, rawQuery string) (Response, error) {
	if !(authIsOperator(auth) && queryHasFranchisee(rawQuery)) {
		if resp, ok := requireTenant(auth); !ok {
			return resp, nil
		}
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.IdentityURL + "/api/v1/admin/tenant-members"
		if rawQuery != "" {
			u += "?" + rawQuery
		}
		return classify(a.call(c, http.MethodGet, u, nil, auth))
	}), nil
}

// AddAdminTenantMember — POST /api/v1/admin/tenant-members →
// chora-identity addTenantMemberByEmail; body forwarded verbatim. The
// tenant rides ONLY the stamped X-Tenant-Id / chora-tenant-id headers —
// never the body.
func (a *Aggregator) AddAdminTenantMember(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	if resp, ok := requireTenant(auth); !ok {
		return resp, nil
	}
	return a.withWriteBudget(ctx, func(c context.Context) Response {
		u := a.cfg.IdentityURL + "/api/v1/admin/tenant-members"
		return classify(a.callWrite(c, http.MethodPost, u, body, auth))
	}), nil
}

// ChangeAdminTenantMemberRole — PATCH /api/v1/admin/tenant-members/{gcid}/role
// → chora-identity changeTenantMemberRole; body forwarded verbatim.
func (a *Aggregator) ChangeAdminTenantMemberRole(ctx context.Context, auth AuthCtx, gcid string, body []byte) (Response, error) {
	if resp, ok := requireTenant(auth); !ok {
		return resp, nil
	}
	return a.withWriteBudget(ctx, func(c context.Context) Response {
		u := a.cfg.IdentityURL + "/api/v1/admin/tenant-members/" +
			url.PathEscape(gcid) + "/role"
		return classify(a.callWrite(c, http.MethodPatch, u, body, auth))
	}), nil
}

// SetAdminTenantMemberRoles — PUT /api/v1/admin/tenant-members/{gcid}/roles
// → chora-identity setTenantMemberRoles (CHO-1809 multi-role REPLACE).
// Body forwarded verbatim — `{roles: ['LEARNER', 'INSTRUCTOR']}`.
func (a *Aggregator) SetAdminTenantMemberRoles(ctx context.Context, auth AuthCtx, gcid string, body []byte) (Response, error) {
	if resp, ok := requireTenant(auth); !ok {
		return resp, nil
	}
	return a.withWriteBudget(ctx, func(c context.Context) Response {
		u := a.cfg.IdentityURL + "/api/v1/admin/tenant-members/" +
			url.PathEscape(gcid) + "/roles"
		return classify(a.callWrite(c, http.MethodPut, u, body, auth))
	}), nil
}

// SetAdminTenantMemberDisplayName — PATCH /api/v1/admin/tenant-members/{gcid}/display-name
// → chora-identity setTenantMemberDisplayName (CHO-1817 follow-up).
// Body forwarded verbatim — `{display_name: '...'}`.
func (a *Aggregator) SetAdminTenantMemberDisplayName(ctx context.Context, auth AuthCtx, gcid string, body []byte) (Response, error) {
	if resp, ok := requireTenant(auth); !ok {
		return resp, nil
	}
	return a.withWriteBudget(ctx, func(c context.Context) Response {
		u := a.cfg.IdentityURL + "/api/v1/admin/tenant-members/" +
			url.PathEscape(gcid) + "/display-name"
		return classify(a.callWrite(c, http.MethodPatch, u, body, auth))
	}), nil
}

// GrantTenantMembership — POST /api/v1/admin/tenant-memberships →
// chora-identity grantTenantMembership (ADR-194 D1, WS2 / CHO-1872). The
// operator cross-tenant grant: body {gcid, tenant_id, roles[]} forwarded
// VERBATIM. Unlike the tenant-members proxies above this does NOT
// requireTenant — the target tenant comes from the BODY, not the caller's
// session. The PLATFORM_OPERATOR role rides x-mesh-user-roles (call() stamps
// AuthCtx.Roles); the identity handler enforces the operator gate.
func (a *Aggregator) GrantTenantMembership(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	return a.withWriteBudget(ctx, func(c context.Context) Response {
		u := a.cfg.IdentityURL + "/api/v1/admin/tenant-memberships"
		return classify(a.callWrite(c, http.MethodPost, u, body, auth))
	}), nil
}

// RemoveAdminTenantMember — DELETE /api/v1/admin/tenant-members/{gcid} →
// chora-identity removeTenantMember (WS2b / CHO-1869). Member-centric revoke:
// soft-deletes all of a member's roles in the caller's tenant. requireTenant;
// the tenant rides the stamped X-Tenant-Id. Mutating → WriteCallTimeout.
func (a *Aggregator) RemoveAdminTenantMember(ctx context.Context, auth AuthCtx, gcid string) (Response, error) {
	if resp, ok := requireTenant(auth); !ok {
		return resp, nil
	}
	return a.withWriteBudget(ctx, func(c context.Context) Response {
		u := a.cfg.IdentityURL + "/api/v1/admin/tenant-members/" + url.PathEscape(gcid)
		return classify(a.callWrite(c, http.MethodDelete, u, nil, auth))
	}), nil
}

// CreateTenantInvite — POST /api/v1/admin/tenant-invites → chora-identity
// createTenantInvite (ADR-194 D2, WS3 / CHO-1873). Body
// {email, tenant_id?, roles[]} forwarded VERBATIM. Like GrantTenantMembership
// this does NOT requireTenant: the operator cold-invite carries the target
// tenant in the body (no session tenant); the same-tenant admin invite omits it
// and identity stamps the tenant from x-mesh headers. Identity gates
// operator-vs-admin by body shape; the role rides x-mesh-user-roles. Mutating →
// the longer WriteCallTimeout (identity->tenancy->pg chain).
func (a *Aggregator) CreateTenantInvite(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	return a.withWriteBudget(ctx, func(c context.Context) Response {
		u := a.cfg.IdentityURL + "/api/v1/admin/tenant-invites"
		return classify(a.callWrite(c, http.MethodPost, u, body, auth))
	}), nil
}

// ListTenantInvites — GET /api/v1/admin/tenant-invites → chora-identity
// listTenantInvites (WS3). Tenant-scoped admin listing — requireTenant; the
// tenant rides the stamped X-Tenant-Id header.
func (a *Aggregator) ListTenantInvites(ctx context.Context, auth AuthCtx) (Response, error) {
	if resp, ok := requireTenant(auth); !ok {
		return resp, nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.IdentityURL + "/api/v1/admin/tenant-invites"
		return classify(a.call(c, http.MethodGet, u, nil, auth))
	}), nil
}

// RevokeTenantInvite — DELETE /api/v1/admin/tenant-invites/{inviteId} →
// chora-identity revokeTenantInvite (WS3). Tenant-scoped admin — requireTenant.
// Mutating → the longer WriteCallTimeout.
func (a *Aggregator) RevokeTenantInvite(ctx context.Context, auth AuthCtx, inviteID string) (Response, error) {
	if resp, ok := requireTenant(auth); !ok {
		return resp, nil
	}
	return a.withWriteBudget(ctx, func(c context.Context) Response {
		u := a.cfg.IdentityURL + "/api/v1/admin/tenant-invites/" + url.PathEscape(inviteID)
		return classify(a.callWrite(c, http.MethodDelete, u, nil, auth))
	}), nil
}

// adminMyManaPoolURL builds the parametric tenancy mana-pool URL for the
// caller's tenant; suffix is "", ":topup" or ":auto-renew".
func (a *Aggregator) adminMyManaPoolURL(auth AuthCtx, suffix string) string {
	return a.cfg.TenancyURL + "/api/v1/admin/tenants/" +
		url.PathEscape(auth.TenantID) + "/mana-pool" + suffix
}

// GetAdminMyManaPool — GET /api/v1/admin/tenants/me/mana-pool →
// chora-tenancy GET /api/v1/admin/tenants/{tenantId}/mana-pool.
func (a *Aggregator) GetAdminMyManaPool(ctx context.Context, auth AuthCtx) (Response, error) {
	if resp, ok := requireTenant(auth); !ok {
		return resp, nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodGet, a.adminMyManaPoolURL(auth, ""), nil, auth))
	}), nil
}

// CreateAdminMyManaPool — POST /api/v1/admin/tenants/me/mana-pool →
// chora-tenancy POST /api/v1/admin/tenants/{tenantId}/mana-pool.
func (a *Aggregator) CreateAdminMyManaPool(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	if resp, ok := requireTenant(auth); !ok {
		return resp, nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodPost, a.adminMyManaPoolURL(auth, ""), body, auth))
	}), nil
}

// TopupAdminMyManaPool — POST /api/v1/admin/tenants/me/mana-pool:topup →
// chora-tenancy POST /api/v1/admin/tenants/{tenantId}/mana-pool:topup.
func (a *Aggregator) TopupAdminMyManaPool(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	if resp, ok := requireTenant(auth); !ok {
		return resp, nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodPost, a.adminMyManaPoolURL(auth, ":topup"), body, auth))
	}), nil
}

// SetAutoRenewAdminMyManaPool — POST
// /api/v1/admin/tenants/me/mana-pool:auto-renew → chora-tenancy POST
// /api/v1/admin/tenants/{tenantId}/mana-pool:auto-renew
// (setTenantManaPoolAutoRenew). Body {"monthly_topup_units": int64 >= 0}
// forwarded verbatim; 404 (no pool) / 422 (validation) pass through.
func (a *Aggregator) SetAutoRenewAdminMyManaPool(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	if resp, ok := requireTenant(auth); !ok {
		return resp, nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodPost, a.adminMyManaPoolURL(auth, ":auto-renew"), body, auth))
	}), nil
}
