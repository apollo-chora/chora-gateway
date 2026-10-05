// wizard_active_tenant.go: name the Setup Wizard's refusal for a caller whose
// validated session carries no active tenant (UX Track U, package E1).
//
// PLATFORM_OPERATOR is tenant-less by design (ADR-165): it holds no
// tenant_memberships row, so a freshly signed-in operator has a validated
// identity and no active organisation. Every wizard /me/ route reads that
// tenant, and chora-tenancy answers a missing one with 401
// gateway_unauthenticated, "caller tenant required (X-Tenant-Id header)". That
// is wrong in both halves: the operator IS authenticated, and the remedy is not
// to sign in again but to create or switch into an organisation. The create
// flow switches the session on its 201, so the gap belongs to the operator who
// navigates to the wizard directly.
//
// The refusal is 409 rather than 401 or 403. The request is well formed and the
// caller is who they say they are and may well hold the right role; it is the
// session's state that does not fit the request, and it is fixable by the
// caller without new credentials.
//
// SCOPE. The guard fires only when the request carries validated mesh claims
// whose tenant is empty. A request with NO claims reaches the handlers exactly
// as before, so the permissive harnesses and any non-gated caller are
// unaffected. That deliberately leaves the pre-existing fall-back in
// authCtxFromRequest alone, where X-Tenant-Id is read from the inbound header
// when no claims are present; changing that is a wider decision than this
// package's, and it is reported rather than quietly altered here. What the
// guard does close is the narrower case it owns: on these routes a validated
// session with no tenant can no longer have a caller-supplied header stand in
// for one.
package httpadapter

import "net/http"

// noActiveTenantCode is the envelope code the SPA keys on to render "you have
// no organisation yet" instead of a generic failure.
const noActiveTenantCode = "GATEWAY_NO_ACTIVE_TENANT"

const noActiveTenantMessage = "this session has no active organisation; " +
	"create one or switch into an existing one before running setup"

// requireActiveTenant wraps a wizard handler so a validated session without a
// tenant is refused by name rather than proxied into a downstream 401.
func requireActiveTenant(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if mc, ok := MeshClaimsFromContext(r.Context()); ok && mc.TenantID == "" {
			writeError(w, http.StatusConflict, noActiveTenantCode, noActiveTenantMessage)
			return
		}
		next(w, r)
	}
}

// registerWizardRoutes mounts every Setup Wizard entry point that reads the
// caller's active tenant, each behind requireActiveTenant.
//
// This is the ONE registration site: the production router calls it, and the
// tests build their mux by calling it too, so a route added here is covered and
// a route added elsewhere is visibly not part of the wizard.
//
// Exact-path mounts throughout, matching the discipline the surrounding router
// uses so these dispatch before any wildcard tenant route. The trailing-slash
// idp-providers entry captures the {providerType} sub-path.
func registerWizardRoutes(mux *http.ServeMux, ph *PhyllisHandler) {
	mux.HandleFunc("/api/v1/tenants/me/branding", requireActiveTenant(ph.handleTenantsMeBranding))
	mux.HandleFunc("/api/v1/tenants/me/addons", requireActiveTenant(ph.handleTenantsMeAddons))
	mux.HandleFunc("/api/v1/tenants/setup", requireActiveTenant(ph.handleTenantsSetup))
	mux.HandleFunc("/api/v1/tenants/me", requireActiveTenant(ph.handleTenantsMeV1))
	mux.HandleFunc("/api/v1/tenants/me/idp-providers", requireActiveTenant(ph.handleTenantsMeIdpProviders))
	mux.HandleFunc("/api/v1/tenants/me/idp-providers/", requireActiveTenant(ph.handleTenantsMeIdpProviders))
}
