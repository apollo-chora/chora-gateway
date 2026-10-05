// phyllis_admin_routes_test.go — route-level coverage for the H+ admin
// tenant leaves on the Phyllis router: tenants-me addons (GET), invoices
// (GET), billing portal (POST), marketplace addons (list + detail), tenant
// memberships (POST), tenant invites (POST/GET/DELETE + malformed-shape
// guards), external egress (GET/PATCH/405). Uses the permissive route repo
// so requests pass authMiddleware and reach the mux handlers.
package httpadapter_test

import (
	"net/http"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

func TestPhyllisAdmin_TenantsMeAddons_Get(t *testing.T) {
	down := newDownstream(t, http.StatusOK, `{"addons":[]}`)
	srv := newPermissivePhyllisServer(t, phyllis.Config{TenancyURL: down.URL})

	resp := doPhyllisReq(t, srv.URL, http.MethodGet, "/api/v1/admin/tenants/me/addons", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET addons → %d; want 200", resp.StatusCode)
	}
}

func TestPhyllisAdmin_InvoicesAndBillingPortal(t *testing.T) {
	down := newDownstream(t, http.StatusOK, `{"items":[]}`)
	srv := newPermissivePhyllisServer(t, phyllis.Config{PaymentsURL: down.URL})

	resp := doPhyllisReq(t, srv.URL, http.MethodGet, "/api/v1/admin/tenants/me/invoices", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET invoices → %d; want 200", resp.StatusCode)
	}

	resp2 := doPhyllisReq(t, srv.URL, http.MethodPost, "/api/v1/admin/tenants/me/billing-portal", `{"return_url":"https://x"}`)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("POST billing-portal → %d; want 200", resp2.StatusCode)
	}
}

func TestPhyllisAdmin_MarketplaceAddons(t *testing.T) {
	down := newDownstream(t, http.StatusOK, `{"items":[]}`)
	srv := newPermissivePhyllisServer(t, phyllis.Config{TenancyURL: down.URL})

	// List.
	resp := doPhyllisReq(t, srv.URL, http.MethodGet, "/api/v1/admin/marketplace/addons", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET marketplace list → %d; want 200", resp.StatusCode)
	}

	// Detail.
	resp2 := doPhyllisReq(t, srv.URL, http.MethodGet, "/api/v1/admin/marketplace/addons/plan-1", "")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("GET marketplace detail → %d; want 200", resp2.StatusCode)
	}

	// Wrong method → 405.
	resp3 := doPhyllisReq(t, srv.URL, http.MethodPost, "/api/v1/admin/marketplace/addons", `{}`)
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST marketplace → %d; want 405", resp3.StatusCode)
	}
}

func TestPhyllisAdmin_TenantMembershipsAndInvites(t *testing.T) {
	down := newDownstream(t, http.StatusOK, `{"granted":true}`)
	srv := newPermissivePhyllisServer(t, phyllis.Config{IdentityURL: down.URL})

	// POST /api/v1/admin/tenant-memberships.
	resp := doPhyllisReq(t, srv.URL, http.MethodPost, "/api/v1/admin/tenant-memberships", `{"gcid":"x","tenant_id":"t"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST tenant-memberships → %d; want 200", resp.StatusCode)
	}

	// Tenant invites: POST create, GET list, DELETE revoke, malformed shape 400, wrong method 405.
	resp = doPhyllisReq(t, srv.URL, http.MethodPost, "/api/v1/admin/tenant-invites", `{"email":"a@b.c"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST tenant-invites → %d; want 200", resp.StatusCode)
	}

	resp = doPhyllisReq(t, srv.URL, http.MethodGet, "/api/v1/admin/tenant-invites", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET tenant-invites → %d; want 200", resp.StatusCode)
	}

	resp = doPhyllisReq(t, srv.URL, http.MethodDelete, "/api/v1/admin/tenant-invites/inv-1", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("DELETE tenant-invites/inv-1 → %d; want 200", resp.StatusCode)
	}

	resp = doPhyllisReq(t, srv.URL, http.MethodDelete, "/api/v1/admin/tenant-invites/a/b", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("DELETE tenant-invites/a/b → %d; want 400", resp.StatusCode)
	}

	resp = doPhyllisReq(t, srv.URL, http.MethodPut, "/api/v1/admin/tenant-invites", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT tenant-invites → %d; want 405", resp.StatusCode)
	}
}

func TestPhyllisAdmin_ExternalEgress(t *testing.T) {
	down := newDownstream(t, http.StatusOK, `{"egress_enabled":false}`)
	srv := newPermissivePhyllisServer(t, phyllis.Config{TenancyURL: down.URL})

	// GET.
	resp := doPhyllisReq(t, srv.URL, http.MethodGet, "/api/v1/admin/tenants/me/external-egress", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET external-egress → %d; want 200", resp.StatusCode)
	}

	// PATCH.
	resp = doPhyllisReq(t, srv.URL, http.MethodPatch, "/api/v1/admin/tenants/me/external-egress", `{"enabled":true}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("PATCH external-egress → %d; want 200", resp.StatusCode)
	}

	// POST → 405.
	resp = doPhyllisReq(t, srv.URL, http.MethodPost, "/api/v1/admin/tenants/me/external-egress", `{}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST external-egress → %d; want 405", resp.StatusCode)
	}
}
