// wizard_active_tenant_test.go: the Setup Wizard must name its refusal for an
// operator who has not switched into an organisation (UX Track U, package E1).
//
// PLATFORM_OPERATOR is tenant-less by design (ADR-165): it holds no
// tenant_memberships row, so a freshly signed-in operator's session carries a
// validated identity and no active tenant. Every /me/ route in the wizard reads
// that tenant, and chora-tenancy answers a missing one with 401
// gateway_unauthenticated ("caller tenant required"). That message is wrong
// twice over: the operator IS authenticated, and the fix is not to sign in
// again, it is to create or switch into an organisation. An operator who
// navigates straight to the wizard rather than arriving through the create flow
// hits it every time.
//
// The guard fires only when the request carries VALIDATED mesh claims that hold
// no tenant. Claim-less requests are left alone deliberately: the pre-existing
// fall-back in authCtxFromRequest, where X-Tenant-Id comes from the inbound
// header when no claims are present, is out of this package's scope and is
// reported rather than changed here.
package httpadapter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

// The five wizard entry points that read the caller's tenant.
var wizardRoutes = []struct {
	name   string
	method string
	target string
	body   string
}{
	{"branding", http.MethodPatch, "/api/v1/tenants/me/branding", `{"logo_url":"x"}`},
	{"addons read", http.MethodGet, "/api/v1/tenants/me/addons", ""},
	{"addons write", http.MethodPost, "/api/v1/tenants/me/addons", `{"addon_codes":["tms"]}`},
	{"setup apply", http.MethodPost, "/api/v1/tenants/setup", `{}`},
	{"re-entry hydration", http.MethodGet, "/api/v1/tenants/me", ""},
	{"idp providers", http.MethodGet, "/api/v1/tenants/me/idp-providers", ""},
}

// wizardMux builds a mux from the PRODUCTION registrar, so these tests cannot
// pass against a parallel copy of the route table the router does not use.
func wizardMux(ph *PhyllisHandler) *http.ServeMux {
	m := http.NewServeMux()
	registerWizardRoutes(m, ph)
	return m
}

func wizardServeWith(t *testing.T, claims *servicemesh.MeshClaims, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	ph := NewPhyllisHandler(phyllis.New(phyllis.Config{}, nil))
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	if claims != nil {
		req = req.WithContext(withMeshClaims(req.Context(), claims))
	}
	rec := httptest.NewRecorder()
	wizardMux(ph).ServeHTTP(rec, req)
	return rec
}

func tenantlessOperatorClaims() *servicemesh.MeshClaims {
	return &servicemesh.MeshClaims{GCID: "g-op", TenantID: "", Roles: []string{"platform_operator"}}
}

func TestWizard_UnswitchedOperator_IsRefusedByName(t *testing.T) {
	for _, tc := range wizardRoutes {
		t.Run(tc.name, func(t *testing.T) {
			rec := wizardServeWith(t, tenantlessOperatorClaims(), tc.method, tc.target, tc.body)

			if rec.Code != http.StatusConflict {
				t.Fatalf("%s %s = %d; want 409. A bare 401 tells an authenticated operator to sign in again, which fixes nothing", tc.method, tc.target, rec.Code)
			}
			var env struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode envelope: %v (body %q)", err, rec.Body.String())
			}
			if env.Error.Code != "GATEWAY_NO_ACTIVE_TENANT" {
				t.Fatalf("code = %q; want GATEWAY_NO_ACTIVE_TENANT so the SPA can render the reason rather than a generic failure", env.Error.Code)
			}
			if !strings.Contains(strings.ToLower(env.Error.Message), "organisation") {
				t.Fatalf("message = %q; it must name the missing thing, an organisation, not the missing header", env.Error.Message)
			}
		})
	}
}

// A session that HAS a tenant must pass through untouched. Without this the
// guard could refuse everyone and still satisfy the test above.
func TestWizard_SessionWithTenant_IsNotRefused(t *testing.T) {
	claims := &servicemesh.MeshClaims{GCID: "g-admin", TenantID: "t-1", Roles: []string{"tenant_admin"}}
	for _, tc := range wizardRoutes {
		t.Run(tc.name, func(t *testing.T) {
			rec := wizardServeWith(t, claims, tc.method, tc.target, tc.body)
			if rec.Code == http.StatusConflict {
				t.Fatalf("%s %s was refused for want of a tenant it has", tc.method, tc.target)
			}
		})
	}
}

// An inbound X-Tenant-Id must not satisfy the guard when the validated claims
// carry none: the header is caller-supplied, and letting it stand in would hand
// a tenant-less operator someone else's organisation on exactly these routes.
func TestWizard_InboundHeaderDoesNotStandInForAValidatedTenant(t *testing.T) {
	ph := NewPhyllisHandler(phyllis.New(phyllis.Config{}, nil))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tenants/me/addons", nil)
	req.Header.Set("X-Tenant-Id", "someone-elses-tenant")
	req = req.WithContext(withMeshClaims(req.Context(), tenantlessOperatorClaims()))
	rec := httptest.NewRecorder()
	wizardMux(ph).ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("a caller-supplied X-Tenant-Id satisfied the guard (status %d); the validated claims are the only source", rec.Code)
	}
}

// A request with NO mesh claims at all is left alone. The permissive test
// harnesses and any non-JWT-gated caller reach the handlers exactly as before,
// so this guard adds a refusal where a validated session lacks a tenant and
// changes nothing else.
func TestWizard_NoMeshClaims_IsLeftAlone(t *testing.T) {
	rec := wizardServeWith(t, nil, http.MethodGet, "/api/v1/tenants/me/addons", "")
	if rec.Code == http.StatusConflict {
		t.Fatalf("a claim-less request was refused; the guard must key on validated claims that carry no tenant")
	}
}
