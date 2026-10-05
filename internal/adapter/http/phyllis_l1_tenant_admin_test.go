// phyllis_l1_tenant_admin_test.go — L1 Tenant lane (CHO-1708) mux dispatch
// tests: /api/v1/admin/tenant-members[/{gcid}/role] → chora-identity and
// /api/v1/admin/tenants/me/mana-pool[:topup] → chora-tenancy parametric.
// The production JWT gate (RequireChoraSessionJWT, main.go) is outside this
// router; these tests assert dispatch + path rewrite + verbatim forwarding.
package httpadapter_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

const (
	l1tTenant = "01970000-0000-7000-8000-000000000001"
	l1tGcid   = "01935f12-0000-7000-8000-0000000000ff"
)

func l1Do(t *testing.T, srv string, method, path, body string) *http.Response {
	t.Helper()
	var req *http.Request
	if body == "" {
		req, _ = http.NewRequestWithContext(context.Background(), method, srv+path, nil)
	} else {
		req, _ = http.NewRequestWithContext(context.Background(), method, srv+path, strings.NewReader(body))
	}
	req.Header.Set("Authorization", "Bearer phyllis")
	req.Header.Set("X-Tenant-Id", l1tTenant)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestL1_TenantMembers_GetDispatchesToIdentitySearch(t *testing.T) {
	identity := newDownstream(t, http.StatusOK, `{"items":[],"next_page_token":null}`)
	srv := newPhyllisServer(t, phyllis.Config{IdentityURL: identity.URL}, nil)

	resp := l1Do(t, srv.URL, http.MethodGet, "/api/v1/admin/tenant-members?q=ani&role=ADMIN", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	if identity.lastMethod != http.MethodGet || identity.lastPath != "/api/v1/admin/tenant-members" {
		t.Errorf("downstream = %s %s", identity.lastMethod, identity.lastPath)
	}
	if identity.lastQuery != "q=ani&role=ADMIN" {
		t.Errorf("query = %q; want forwarded verbatim", identity.lastQuery)
	}
}

func TestL1_TenantMembers_PostDispatchesAdd(t *testing.T) {
	identity := newDownstream(t, http.StatusCreated, `{"gcid":"x","roles":["INSTRUCTOR"]}`)
	srv := newPhyllisServer(t, phyllis.Config{IdentityURL: identity.URL}, nil)

	body := `{"email":"anika@mtm.sg","role":"INSTRUCTOR"}`
	resp := l1Do(t, srv.URL, http.MethodPost, "/api/v1/admin/tenant-members", body)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d; want 201", resp.StatusCode)
	}
	if identity.lastMethod != http.MethodPost {
		t.Errorf("method = %s; want POST", identity.lastMethod)
	}
	if identity.lastBody != body {
		t.Errorf("body = %q; want verbatim", identity.lastBody)
	}
}

func TestL1_TenantMembers_PatchRoleItemPath(t *testing.T) {
	identity := newDownstream(t, http.StatusOK, `{"gcid":"x","roles":["ADMIN"]}`)
	srv := newPhyllisServer(t, phyllis.Config{IdentityURL: identity.URL}, nil)

	resp := l1Do(t, srv.URL, http.MethodPatch,
		"/api/v1/admin/tenant-members/"+l1tGcid+"/role", `{"role":"ADMIN"}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	wantPath := "/api/v1/admin/tenant-members/" + l1tGcid + "/role"
	if identity.lastMethod != http.MethodPatch || identity.lastPath != wantPath {
		t.Errorf("downstream = %s %s; want PATCH %s", identity.lastMethod, identity.lastPath, wantPath)
	}
}

func TestL1_TenantMembers_BadItemPath400_AndCollection405(t *testing.T) {
	identity := newDownstream(t, http.StatusOK, `{}`)
	srv := newPhyllisServer(t, phyllis.Config{IdentityURL: identity.URL}, nil)

	if resp := l1Do(t, srv.URL, http.MethodPatch,
		"/api/v1/admin/tenant-members/"+l1tGcid+"/banana", `{}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad sub-resource status = %d; want 400", resp.StatusCode)
	}
	if resp := l1Do(t, srv.URL, http.MethodDelete,
		"/api/v1/admin/tenant-members", ""); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("DELETE collection status = %d; want 405", resp.StatusCode)
	}
	if resp := l1Do(t, srv.URL, http.MethodGet,
		"/api/v1/admin/tenant-members/"+l1tGcid+"/role", ""); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET item status = %d; want 405", resp.StatusCode)
	}
	if identity.calls.Load() != 0 {
		t.Errorf("downstream must not be called on dispatch errors (calls=%d)", identity.calls.Load())
	}
}

func TestL1_ManaPool_GetAndPostRewriteMeAlias(t *testing.T) {
	tenancy := newDownstream(t, http.StatusOK, `{"balance_units":1000}`)
	srv := newPhyllisServer(t, phyllis.Config{TenancyURL: tenancy.URL}, nil)

	wantPath := "/api/v1/admin/tenants/" + l1tTenant + "/mana-pool"

	if resp := l1Do(t, srv.URL, http.MethodGet, "/api/v1/admin/tenants/me/mana-pool", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("GET status = %d; want 200", resp.StatusCode)
	}
	if tenancy.lastMethod != http.MethodGet || tenancy.lastPath != wantPath {
		t.Errorf("downstream = %s %s; want GET %s (me rewritten)", tenancy.lastMethod, tenancy.lastPath, wantPath)
	}

	body := `{"initial_topup_units":500}`
	if resp := l1Do(t, srv.URL, http.MethodPost, "/api/v1/admin/tenants/me/mana-pool", body); resp.StatusCode != http.StatusOK {
		t.Errorf("POST status = %d; want 200 (stub returns 200)", resp.StatusCode)
	}
	if tenancy.lastMethod != http.MethodPost || tenancy.lastPath != wantPath {
		t.Errorf("downstream = %s %s; want POST %s", tenancy.lastMethod, tenancy.lastPath, wantPath)
	}
	if tenancy.lastBody != body {
		t.Errorf("body = %q; want verbatim", tenancy.lastBody)
	}

	if resp := l1Do(t, srv.URL, http.MethodDelete, "/api/v1/admin/tenants/me/mana-pool", ""); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("DELETE status = %d; want 405", resp.StatusCode)
	}
}

func TestL1_ManaPoolTopup_PostOnly(t *testing.T) {
	tenancy := newDownstream(t, http.StatusOK, `{"balance_units":1500}`)
	srv := newPhyllisServer(t, phyllis.Config{TenancyURL: tenancy.URL}, nil)

	body := `{"units":1000}`
	if resp := l1Do(t, srv.URL, http.MethodPost, "/api/v1/admin/tenants/me/mana-pool:topup", body); resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	wantPath := "/api/v1/admin/tenants/" + l1tTenant + "/mana-pool:topup"
	if tenancy.lastMethod != http.MethodPost || tenancy.lastPath != wantPath {
		t.Errorf("downstream = %s %s; want POST %s", tenancy.lastMethod, tenancy.lastPath, wantPath)
	}
	if resp := l1Do(t, srv.URL, http.MethodGet, "/api/v1/admin/tenants/me/mana-pool:topup", ""); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET topup status = %d; want 405", resp.StatusCode)
	}
}

// WP-4 (CHO-1708) — `:auto-renew` colon-suffix path needs its own mux mount
// (like `:topup` — the bare mana-pool entry never matches a colon suffix).
// POST only; body forwarded verbatim; `me` rewritten to the JWT tenant.
func TestL1_ManaPoolAutoRenew_PostOnly(t *testing.T) {
	tenancy := newDownstream(t, http.StatusOK, `{"balance_units":1500,"monthly_topup_units":500}`)
	srv := newPhyllisServer(t, phyllis.Config{TenancyURL: tenancy.URL}, nil)

	body := `{"monthly_topup_units":500}`
	if resp := l1Do(t, srv.URL, http.MethodPost, "/api/v1/admin/tenants/me/mana-pool:auto-renew", body); resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	wantPath := "/api/v1/admin/tenants/" + l1tTenant + "/mana-pool:auto-renew"
	if tenancy.lastMethod != http.MethodPost || tenancy.lastPath != wantPath {
		t.Errorf("downstream = %s %s; want POST %s", tenancy.lastMethod, tenancy.lastPath, wantPath)
	}
	if tenancy.lastBody != body {
		t.Errorf("body = %q; want verbatim", tenancy.lastBody)
	}
	if resp := l1Do(t, srv.URL, http.MethodGet, "/api/v1/admin/tenants/me/mana-pool:auto-renew", ""); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET auto-renew status = %d; want 405", resp.StatusCode)
	}
}
