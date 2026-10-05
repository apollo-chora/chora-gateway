// phyllis_tenancy_setup_test.go — Setup Wizard handler coverage:
// /api/v1/tenants/setup (POST), /api/v1/tenants/me/branding (PATCH),
// /api/v1/tenants/me/addons (GET read + POST subscribe + 405). Exercised
// through the permissive-route Phyllis server.
package httpadapter_test

import (
	"net/http"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

func TestPhyllisTenancy_SetupPost(t *testing.T) {
	down := newDownstream(t, http.StatusOK, `{"setup_complete":true}`)
	srv := newPermissivePhyllisServer(t, phyllis.Config{
		IdentityURL: down.URL,
		TenancyURL:  down.URL,
	})

	resp := doPhyllisReq(t, srv.URL, http.MethodPost, "/api/v1/tenants/setup",
		`{"identity":[{"provider_type":"oidc","client_id":"acme","client_secret":"shhh","discovery_url":"https://issuer.example/openid"}],"finish_setup":{}}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST setup → %d; want 200", resp.StatusCode)
	}

	// GET → 405.
	resp2 := doPhyllisReq(t, srv.URL, http.MethodGet, "/api/v1/tenants/setup", "")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET setup → %d; want 405", resp2.StatusCode)
	}
}

func TestPhyllisTenancy_MeBrandingPatch(t *testing.T) {
	down := newDownstream(t, http.StatusOK, `{"branding":{"logo":"x"}}`)
	srv := newPermissivePhyllisServer(t, phyllis.Config{TenancyURL: down.URL})

	resp := doPhyllisReq(t, srv.URL, http.MethodPatch, "/api/v1/tenants/me/branding", `{"logo_url":"x"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("PATCH branding → %d; want 200", resp.StatusCode)
	}

	// GET → 405.
	resp2 := doPhyllisReq(t, srv.URL, http.MethodGet, "/api/v1/tenants/me/branding", "")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET branding → %d; want 405", resp2.StatusCode)
	}
}

func TestPhyllisTenancy_MeAddonsGetPostAnd405(t *testing.T) {
	down := newDownstream(t, http.StatusOK, `{"addons":[]}`)
	srv := newPermissivePhyllisServer(t, phyllis.Config{TenancyURL: down.URL})

	// GET (re-entry hydration).
	resp := doPhyllisReq(t, srv.URL, http.MethodGet, "/api/v1/tenants/me/addons", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET addons → %d; want 200", resp.StatusCode)
	}

	// POST (subscribe).
	resp = doPhyllisReq(t, srv.URL, http.MethodPost, "/api/v1/tenants/me/addons", `{"addon_id":"a1"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST addons → %d; want 200", resp.StatusCode)
	}

	// PUT → 405.
	resp = doPhyllisReq(t, srv.URL, http.MethodPut, "/api/v1/tenants/me/addons", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT addons → %d; want 405", resp.StatusCode)
	}
}
