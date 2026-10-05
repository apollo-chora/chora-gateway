// admin_tenant_members_test.go — TDD RED for the L1 Tenant lane
// (CHO-1708): gateway proxies for the identity tenant-members admin
// surface + the me-style tenancy mana-pool alias.
//
// Contracts: bff-gateway.yaml v0.6.0 (proxy surfaces) over
// identity-admin.yaml v1.4.0 + tenancy-admin.yaml (parametric mana-pool).
// Per feedback_bff_aggregator_path_test the canonical backend mount paths
// are hard-coded in assertions (CHO-1632 path-stripping bug class).
package phyllis_test

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

const (
	l1Tenant = "01970000-0000-7000-8000-000000000001"
	l1Gcid   = "01935f12-0000-7000-8000-0000000000ff"
)

func l1Auth() phyllis.AuthCtx {
	return phyllis.AuthCtx{
		Bearer:      "phyllis",
		GCID:        l1Gcid,
		TenantID:    l1Tenant,
		Traceparent: "00-trace-id-01",
		Roles:       []string{"admin", "instructor"},
	}
}

// --- identity: tenant-members ------------------------------------------------

func TestSearchAdminTenantMembers_forwardsQueryAndRoles(t *testing.T) {
	var seenPath, seenMethod, seenQuery, seenRoles, seenTenant string
	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath, seenMethod = r.URL.Path, r.Method
		seenQuery = r.URL.RawQuery
		seenRoles = r.Header.Get("x-mesh-user-roles")
		seenTenant = r.Header.Get("X-Tenant-Id")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[],"next_page_token":null}`))
	})

	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: time.Second}, nil)
	res, _ := a.SearchAdminTenantMembers(context.Background(), l1Auth(), "q=ani&role=ADMIN&page_size=20")

	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200 (body=%s)", res.Status, res.Body)
	}
	if seenMethod != http.MethodGet || seenPath != "/api/v1/admin/tenant-members" {
		t.Errorf("upstream = %s %s; want GET /api/v1/admin/tenant-members", seenMethod, seenPath)
	}
	if seenQuery != "q=ani&role=ADMIN&page_size=20" {
		t.Errorf("query = %q; must forward verbatim", seenQuery)
	}
	// The identity admin gate (callerHoldsAdminRole) reads x-mesh-user-roles —
	// the aggregator MUST stamp AuthCtx.Roles onto the mesh headers.
	if seenRoles != "admin,instructor" {
		t.Errorf("x-mesh-user-roles = %q; want \"admin,instructor\"", seenRoles)
	}
	if seenTenant != l1Tenant {
		t.Errorf("X-Tenant-Id = %q", seenTenant)
	}
}

// S4 (CHO-2000): a PLATFORM_OPERATOR targeting ?managed_tenant_id=<uuid> is a
// cross-tenant learner-directory read — like GrantTenantMembership it does NOT
// requireTenant (the operator carries no session tenant; the target rides the
// query, and chora-identity enforces the operator gate + fail-closed IMDA-D1
// audit). The query + roles must forward verbatim.
func TestSearchAdminTenantMembers_operatorFranchisee_noTenantRequired(t *testing.T) {
	var seenPath, seenMethod, seenQuery, seenRoles string
	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath, seenMethod = r.URL.Path, r.Method
		seenQuery = r.URL.RawQuery
		seenRoles = r.Header.Get("x-mesh-user-roles")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[],"next_page_token":null}`))
	})

	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: time.Second}, nil)
	// Operator session has NO tenant — the read must still forward.
	auth := phyllis.AuthCtx{Bearer: "phyllis", GCID: l1Gcid, TenantID: "", Roles: []string{"platform_operator"}}
	rawQuery := "role=learner&q=dale&managed_tenant_id=" + l1Tenant
	res, _ := a.SearchAdminTenantMembers(context.Background(), auth, rawQuery)

	if res.Status != http.StatusOK {
		t.Fatalf("status = %d; want 200 (operator franchisee read must NOT 400; body=%s)", res.Status, res.Body)
	}
	if seenMethod != http.MethodGet || seenPath != "/api/v1/admin/tenant-members" {
		t.Errorf("upstream = %s %s; want GET /api/v1/admin/tenant-members", seenMethod, seenPath)
	}
	if seenQuery != rawQuery {
		t.Errorf("query = %q; must forward verbatim (incl. managed_tenant_id)", seenQuery)
	}
	if seenRoles != "platform_operator" {
		t.Errorf("x-mesh-user-roles = %q; want platform_operator (identity operator gate + audit)", seenRoles)
	}
}

// A NON-operator with no tenant still gets the requireTenant 400 even if a
// managed_tenant_id rides the query — the skip is operator-only, so a
// tenant-less admin cannot reach identity via the cross-tenant param.
func TestSearchAdminTenantMembers_nonOperatorFranchisee_stillRequiresTenant(t *testing.T) {
	a := phyllis.New(phyllis.Config{IdentityURL: "http://unused", PerCallTimeout: time.Second}, nil)
	auth := phyllis.AuthCtx{Bearer: "phyllis", GCID: l1Gcid, TenantID: "", Roles: []string{"admin"}}
	res, _ := a.SearchAdminTenantMembers(context.Background(), auth, "managed_tenant_id="+l1Tenant)
	if res.Status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (non-operator must not bypass requireTenant via franchisee param)", res.Status)
	}
}

func TestAddAdminTenantMember_postsBodyVerbatim(t *testing.T) {
	var seenPath, seenMethod, seenBody, seenRoles string
	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath, seenMethod = r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		seenRoles = r.Header.Get("x-mesh-user-roles")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"gcid":"x"}`))
	})

	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: time.Second}, nil)
	body := []byte(`{"email":"anika@mtm.sg","role":"INSTRUCTOR"}`)
	res, _ := a.AddAdminTenantMember(context.Background(), l1Auth(), body)

	if res.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", res.Status)
	}
	if seenMethod != http.MethodPost || seenPath != "/api/v1/admin/tenant-members" {
		t.Errorf("upstream = %s %s", seenMethod, seenPath)
	}
	if seenBody != string(body) {
		t.Errorf("body = %q; must forward verbatim", seenBody)
	}
	if seenRoles == "" {
		t.Error("x-mesh-user-roles missing — identity admin gate would 403")
	}
}

func TestAddAdminTenantMember_passesThrough404And409(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusConflict} {
		identity := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":"X","message":"y"}`))
		})
		a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: time.Second}, nil)
		res, _ := a.AddAdminTenantMember(context.Background(), l1Auth(), []byte(`{}`))
		if res.Status != status {
			t.Errorf("status = %d; want %d pass-through", res.Status, status)
		}
	}
}

func TestChangeAdminTenantMemberRole_patchesItemPath(t *testing.T) {
	var seenPath, seenMethod string
	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath, seenMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"gcid":"x","roles":["ADMIN"]}`))
	})

	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: time.Second}, nil)
	res, _ := a.ChangeAdminTenantMemberRole(context.Background(), l1Auth(), l1Gcid, []byte(`{"role":"ADMIN"}`))

	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", res.Status)
	}
	if seenMethod != http.MethodPatch {
		t.Errorf("method = %q; want PATCH", seenMethod)
	}
	wantPath := "/api/v1/admin/tenant-members/" + l1Gcid + "/role"
	if seenPath != wantPath {
		t.Errorf("path = %q; want %q", seenPath, wantPath)
	}
}

// --- identity: tenant-memberships (operator cross-tenant grant, WS2) ----------

// The grant forwards the body verbatim AND — unlike the tenant-members proxies
// — does NOT requireTenant: the operator carries no session tenant (the target
// is in the body). x-mesh-user-roles must still be stamped so the identity
// PLATFORM_OPERATOR gate sees it. ADR-194 D1 / CHO-1872.
func TestGrantTenantMembership_postsBodyVerbatim_noTenantRequired(t *testing.T) {
	var seenPath, seenMethod, seenBody, seenRoles string
	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath, seenMethod = r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		seenRoles = r.Header.Get("x-mesh-user-roles")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"membership_id":"m","roles":["INSTRUCTOR"]}`))
	})

	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: time.Second}, nil)
	// Operator session has NO tenant — the grant must still forward.
	auth := phyllis.AuthCtx{Bearer: "phyllis", GCID: l1Gcid, TenantID: "", Roles: []string{"platform_operator"}}
	body := []byte(`{"gcid":"` + l1Gcid + `","tenant_id":"` + l1Tenant + `","roles":["INSTRUCTOR"]}`)
	res, _ := a.GrantTenantMembership(context.Background(), auth, body)

	if res.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201 (body=%s)", res.Status, res.Body)
	}
	if seenMethod != http.MethodPost || seenPath != "/api/v1/admin/tenant-memberships" {
		t.Errorf("upstream = %s %s; want POST /api/v1/admin/tenant-memberships", seenMethod, seenPath)
	}
	if seenBody != string(body) {
		t.Errorf("body = %q; must forward verbatim", seenBody)
	}
	if seenRoles != "platform_operator" {
		t.Errorf("x-mesh-user-roles = %q; want platform_operator", seenRoles)
	}
}

func TestGrantTenantMembership_passesThrough403And409(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusConflict} {
		identity := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":"X","message":"y"}`))
		})
		a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: time.Second}, nil)
		res, _ := a.GrantTenantMembership(context.Background(), l1Auth(), []byte(`{}`))
		if res.Status != status {
			t.Errorf("status = %d; want %d pass-through", res.Status, status)
		}
	}
}

func TestAdminTenantMembers_missingTenant400(t *testing.T) {
	a := phyllis.New(phyllis.Config{IdentityURL: "http://unused", PerCallTimeout: time.Second}, nil)
	auth := l1Auth()
	auth.TenantID = ""
	if res, _ := a.SearchAdminTenantMembers(context.Background(), auth, ""); res.Status != http.StatusBadRequest {
		t.Errorf("search status = %d; want 400", res.Status)
	}
	if res, _ := a.AddAdminTenantMember(context.Background(), auth, []byte(`{}`)); res.Status != http.StatusBadRequest {
		t.Errorf("add status = %d; want 400", res.Status)
	}
	if res, _ := a.ChangeAdminTenantMemberRole(context.Background(), auth, l1Gcid, []byte(`{}`)); res.Status != http.StatusBadRequest {
		t.Errorf("change status = %d; want 400", res.Status)
	}
	if res, _ := a.RemoveAdminTenantMember(context.Background(), auth, l1Gcid); res.Status != http.StatusBadRequest {
		t.Errorf("remove status = %d; want 400", res.Status)
	}
}

// RemoveAdminTenantMember — DELETE /api/v1/admin/tenant-members/{gcid} (WS2b).
func TestRemoveAdminTenantMember_forwardsDelete(t *testing.T) {
	var seenPath, seenMethod, seenTenant, seenRoles string
	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath, seenMethod = r.URL.Path, r.Method
		seenTenant = r.Header.Get("X-Tenant-Id")
		seenRoles = r.Header.Get("x-mesh-user-roles")
		w.WriteHeader(http.StatusNoContent)
	})
	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: time.Second}, nil)
	res, _ := a.RemoveAdminTenantMember(context.Background(), l1Auth(), l1Gcid)

	if res.Status != http.StatusNoContent {
		t.Errorf("status = %d; want 204 (body=%s)", res.Status, res.Body)
	}
	if seenMethod != http.MethodDelete || seenPath != "/api/v1/admin/tenant-members/"+l1Gcid {
		t.Errorf("upstream = %s %s; want DELETE /api/v1/admin/tenant-members/%s", seenMethod, seenPath, l1Gcid)
	}
	if seenTenant != l1Tenant {
		t.Errorf("X-Tenant-Id = %q; want %q", seenTenant, l1Tenant)
	}
	if seenRoles == "" {
		t.Errorf("x-mesh-user-roles must be stamped")
	}
}

func TestRemoveAdminTenantMember_passesThrough404(t *testing.T) {
	identity := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"IDENTITY_MEMBERSHIP_NOT_FOUND","message":"x"}`))
	})
	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: time.Second}, nil)
	if res, _ := a.RemoveAdminTenantMember(context.Background(), l1Auth(), l1Gcid); res.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 pass-through", res.Status)
	}
}

// --- tenancy: me-style mana-pool ----------------------------------------------

func TestGetAdminMyManaPool_rewritesMeToParametric(t *testing.T) {
	var seenPath, seenMethod string
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath, seenMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"balance_units":1000}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: time.Second}, nil)
	res, _ := a.GetAdminMyManaPool(context.Background(), l1Auth())

	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", res.Status)
	}
	wantPath := "/api/v1/admin/tenants/" + l1Tenant + "/mana-pool"
	if seenMethod != http.MethodGet || seenPath != wantPath {
		t.Errorf("upstream = %s %s; want GET %s (me alias rewritten)", seenMethod, seenPath, wantPath)
	}
}

func TestCreateAdminMyManaPool_postsToParametric(t *testing.T) {
	var seenPath, seenMethod, seenBody string
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath, seenMethod = r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"balance_units":500}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: time.Second}, nil)
	body := []byte(`{"initial_topup_units":500}`)
	res, _ := a.CreateAdminMyManaPool(context.Background(), l1Auth(), body)

	if res.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", res.Status)
	}
	wantPath := "/api/v1/admin/tenants/" + l1Tenant + "/mana-pool"
	if seenMethod != http.MethodPost || seenPath != wantPath {
		t.Errorf("upstream = %s %s; want POST %s", seenMethod, seenPath, wantPath)
	}
	if seenBody != string(body) {
		t.Errorf("body = %q; must forward verbatim", seenBody)
	}
}

func TestTopupAdminMyManaPool_postsToColonTopup(t *testing.T) {
	var seenPath, seenMethod string
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath, seenMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"balance_units":1500}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: time.Second}, nil)
	res, _ := a.TopupAdminMyManaPool(context.Background(), l1Auth(), []byte(`{"units":1000}`))

	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", res.Status)
	}
	wantPath := "/api/v1/admin/tenants/" + l1Tenant + "/mana-pool:topup"
	if seenMethod != http.MethodPost || seenPath != wantPath {
		t.Errorf("upstream = %s %s; want POST %s", seenMethod, seenPath, wantPath)
	}
}

// WP-4 (CHO-1708) — the `:auto-renew` me-style proxy mirrors `:topup`:
// rewrite `me` → JWT tenant against the chora-tenancy parametric
// `POST /api/v1/admin/tenants/{tenantId}/mana-pool:auto-renew`
// (tenancy-admin.yaml setTenantManaPoolAutoRenew); body
// {"monthly_topup_units": int64 >= 0} forwarded verbatim.
func TestSetAutoRenewAdminMyManaPool_postsToColonAutoRenew(t *testing.T) {
	var seenPath, seenMethod, seenBody string
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath, seenMethod = r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"balance_units":1500,"monthly_topup_units":500}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: time.Second}, nil)
	body := []byte(`{"monthly_topup_units":500}`)
	res, _ := a.SetAutoRenewAdminMyManaPool(context.Background(), l1Auth(), body)

	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", res.Status)
	}
	wantPath := "/api/v1/admin/tenants/" + l1Tenant + "/mana-pool:auto-renew"
	if seenMethod != http.MethodPost || seenPath != wantPath {
		t.Errorf("upstream = %s %s; want POST %s (me alias rewritten)", seenMethod, seenPath, wantPath)
	}
	if seenBody != string(body) {
		t.Errorf("body = %q; must forward verbatim", seenBody)
	}
}

func TestSetAutoRenewAdminMyManaPool_passesThrough404And422(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusUnprocessableEntity} {
		tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":"X","message":"y"}`))
		})
		a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: time.Second}, nil)
		res, _ := a.SetAutoRenewAdminMyManaPool(context.Background(), l1Auth(), []byte(`{"monthly_topup_units":0}`))
		if res.Status != status {
			t.Errorf("status = %d; want %d pass-through", res.Status, status)
		}
	}
}

func TestAdminMyManaPool_missingTenant400(t *testing.T) {
	a := phyllis.New(phyllis.Config{TenancyURL: "http://unused", PerCallTimeout: time.Second}, nil)
	auth := l1Auth()
	auth.TenantID = ""
	if res, _ := a.GetAdminMyManaPool(context.Background(), auth); res.Status != http.StatusBadRequest {
		t.Errorf("get status = %d; want 400", res.Status)
	}
	if res, _ := a.CreateAdminMyManaPool(context.Background(), auth, nil); res.Status != http.StatusBadRequest {
		t.Errorf("create status = %d; want 400", res.Status)
	}
	if res, _ := a.TopupAdminMyManaPool(context.Background(), auth, nil); res.Status != http.StatusBadRequest {
		t.Errorf("topup status = %d; want 400", res.Status)
	}
	if res, _ := a.SetAutoRenewAdminMyManaPool(context.Background(), auth, nil); res.Status != http.StatusBadRequest {
		t.Errorf("auto-renew status = %d; want 400", res.Status)
	}
}

func TestManaPool_upstream5xxNormalisesTo502(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: time.Second}, nil)
	if res, _ := a.GetAdminMyManaPool(context.Background(), l1Auth()); res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", res.Status)
	}
}
