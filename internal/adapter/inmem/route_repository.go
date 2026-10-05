package inmem

import (
	"context"
	"sync"

	"github.com/apollo-chora/chora-gateway/internal/domain/route"
)

// RouteRepository is the in-memory route table. It's seeded at startup with
// the deterministic skeleton routes; callers Match against the table on
// every incoming request.
type RouteRepository struct {
	mu     sync.RWMutex
	routes []*route.BFFRoute
}

// NewRouteRepository constructs a repository containing the M10 skeleton
// route table — one entry per surface composition endpoint plus the auth +
// proxy entries.
//
// Surface visibility note: /bff/aplus/home defaults to AuthMode=Public so
// guest preview works (per the briefing's "200 on public — guest preview"
// case). Other surface aggregates require an authenticated session.
func NewRouteRepository() *RouteRepository {
	r := &RouteRepository{}
	for _, p := range defaultRoutes() {
		br, err := route.New(p)
		if err != nil {
			// Defensive: misconfigured default = panic at startup, never run a
			// service with an inconsistent route table.
			panic("invalid skeleton route: " + err.Error())
		}
		r.routes = append(r.routes, br)
	}
	return r
}

// Match returns the first matching route or ErrNotFound.
func (r *RouteRepository) Match(_ context.Context, path string) (*route.BFFRoute, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, br := range r.routes {
		if br.Matches(path) {
			clone := *br
			return &clone, nil
		}
	}
	return nil, route.ErrNotFound
}

// All returns the full route table (for /readyz diagnostics).
func (r *RouteRepository) All(_ context.Context) ([]*route.BFFRoute, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*route.BFFRoute, 0, len(r.routes))
	for _, br := range r.routes {
		clone := *br
		out = append(out, &clone)
	}
	return out, nil
}

// defaultRoutes is the seed list for the M10 skeleton route table. Extended
// at M12+ when real backend wiring lands.
func defaultRoutes() []route.NewParams {
	return []route.NewParams{
		// Auth (public — must be reachable without a session).
		{Surface: route.SurfaceAPI, PathPattern: "/api/auth/session", BackendService: "chora-identity", AuthMode: route.AuthModePublic},

		// A+ home — guest-previewable (Public). When a session is present the
		// BFF stamps gcid/tenant headers; when absent it composes a guest view.
		{Surface: route.SurfaceAPlus, PathPattern: "/bff/aplus/home", BackendService: "chora-consumption", AuthMode: route.AuthModePublic},

		// C+ feed — authenticated (social context requires identity).
		{Surface: route.SurfaceCPlus, PathPattern: "/bff/cplus/feed", BackendService: "chora-sharing", AuthMode: route.AuthModeAuthenticated},

		// H+ tenant — admin-only (tenant config).
		{Surface: route.SurfaceHPlus, PathPattern: "/bff/hplus/tenant", BackendService: "chora-tenancy", AuthMode: route.AuthModeAdmin},

		// O+ governance — admin-only (audit + IMDA).
		{Surface: route.SurfaceOPlus, PathPattern: "/bff/oplus/governance", BackendService: "chora-governance", AuthMode: route.AuthModeAdmin},

		// R+ courses — authenticated (instructor + admin both visit; tenant
		// scoping enforced upstream).
		{Surface: route.SurfaceRPlus, PathPattern: "/bff/rplus/courses", BackendService: "chora-delivery", AuthMode: route.AuthModeAuthenticated},

		// Generic proxy — authenticated. The handler stamps headers and
		// forwards to the named backend.
		{Surface: route.SurfaceAPI, PathPattern: "/api/proxy/*", BackendService: "dynamic", AuthMode: route.AuthModeAuthenticated},

		// Phyllis MVP routes (M13.A) — public from the BFF's perspective: the
		// caller's Authorization: Bearer {gcid} is forwarded to chora-identity
		// (and the rest of the constellation), and downstream services enforce
		// tenant scoping + role context. Per docs/m13/phyllis-mvp-2026-05-08.md.
		{Surface: route.SurfaceAPI, PathPattern: "/api/me", BackendService: "chora-identity", AuthMode: route.AuthModePublic},
		{Surface: route.SurfaceAPI, PathPattern: "/api/me/roles", BackendService: "chora-identity", AuthMode: route.AuthModePublic},
		{Surface: route.SurfaceAPI, PathPattern: "/api/tenants/me", BackendService: "chora-tenancy", AuthMode: route.AuthModePublic},
		{Surface: route.SurfaceAPI, PathPattern: "/api/courses", BackendService: "chora-delivery", AuthMode: route.AuthModePublic},
		{Surface: route.SurfaceAPI, PathPattern: "/api/catalog", BackendService: "chora-delivery", AuthMode: route.AuthModePublic},
		{Surface: route.SurfaceAPI, PathPattern: "/api/enrollments", BackendService: "chora-delivery", AuthMode: route.AuthModePublic},
		{Surface: route.SurfaceAPI, PathPattern: "/api/atoms/*", BackendService: "chora-creation", AuthMode: route.AuthModePublic},
		// CHO-2261 — the /v1/ atom prefix the A+ atom services hit. authMiddleware
		// Match()es EVERY inbound path against this table BEFORE the mux runs; the
		// missing entry is why GET /api/v1/atoms/{id} returned GATEWAY_ROUTE_NOT_FOUND
		// (404) and the Daily Dose silently dropped every AI-picked atom. The
		// gatewayproxy bridge TRANSLATES this onto chora-creation /api/atoms/{id}
		// (the /*-suffix pattern Matches() both the bare collection and /{id}).
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/atoms/*", BackendService: "chora-creation", AuthMode: route.AuthModePublic},
		// CHO-2276 (topic-tree Sub-phase B) — the /api/v1/topics prefix the A+/R+
		// topic-tree UI hits. authMiddleware Match()es EVERY inbound path against
		// this table BEFORE the mux runs, so without this entry every topic call
		// 404s GATEWAY_ROUTE_NOT_FOUND (the CHO-1655/CHO-2261 bug class). The
		// gatewayproxy bridge TRANSLATES this onto chora-creation /api/topics
		// (the /*-suffix pattern Matches() the bare collection AND /{id}[/move|/atoms]).
		// AuthMode is Public because JWT enforcement happens upstream via
		// WithChoraSessionOnPrefixes; the admin/owner write gate is DOWNSTREAM
		// (chora-creation requireAdmin via x-mesh-user-roles). /api/internal/topics/*
		// is deliberately absent — the operator backfill is never exposed here.
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/topics/*", BackendService: "chora-creation", AuthMode: route.AuthModePublic},
		{Surface: route.SurfaceAPI, PathPattern: "/api/ai/generate", BackendService: "chora-model-broker-router", AuthMode: route.AuthModePublic},
		{Surface: route.SurfaceAPI, PathPattern: "/api/companion/me", BackendService: "chora-consumption", AuthMode: route.AuthModePublic},
		{Surface: route.SurfaceAPI, PathPattern: "/api/companion/daily-dose", BackendService: "chora-consumption", AuthMode: route.AuthModePublic},
		// Debt #41 close (2026-05-16) — bare-path alias for the CR v4 rehearsal
		// smoke that probed /companion/daily-dose (no /api/ prefix) and got 404.
		// Handler in phyllis_handler.go delegates to the same aggregator.
		{Surface: route.SurfaceAPI, PathPattern: "/companion/daily-dose", BackendService: "chora-consumption", AuthMode: route.AuthModePublic},
		// ADR-254 D9 alias, drop after the SPA cut (WP-X go): the pre-rename
		// Phyllis paths must stay in this table (it is matched BEFORE the mux
		// runs) or the alias handlers 404 GATEWAY_ROUTE_NOT_FOUND.
		{Surface: route.SurfaceAPI, PathPattern: "/api/familiar/me", BackendService: "chora-consumption", AuthMode: route.AuthModePublic},
		{Surface: route.SurfaceAPI, PathPattern: "/api/familiar/daily-dose", BackendService: "chora-consumption", AuthMode: route.AuthModePublic},
		{Surface: route.SurfaceAPI, PathPattern: "/familiar/daily-dose", BackendService: "chora-consumption", AuthMode: route.AuthModePublic},

		// PE-14 — A+ GDPR consent center routes (GDPR Art. 15/20). Account
		// closure (Art. 17) is served by the canonical /api/v1/me/account/close
		// saga me-route (CHO-1719); the legacy /api/me/account-closure →
		// chora-closure-orchestrator:/sagas route was dead and is removed
		// (CHO-1790 D12). Bearer pass-through: caller's Authorization header is
		// forwarded to the downstream identity service.
		{Surface: route.SurfaceAPI, PathPattern: "/api/me/consents", BackendService: "chora-identity", AuthMode: route.AuthModePublic},
		{Surface: route.SurfaceAPI, PathPattern: "/api/me/consents/grant", BackendService: "chora-identity", AuthMode: route.AuthModePublic},
		{Surface: route.SurfaceAPI, PathPattern: "/api/me/data-export", BackendService: "chora-identity", AuthMode: route.AuthModePublic},

		// CHO-1632 Phase 3 — H+ Setup-Tenant bootstrap. Forwards to
		// chora-tenancy `/v1/tenants/bootstrap` (CHO-1628). Caller's
		// GCID is taken from the validated Chora session JWT, NEVER
		// from the body.
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/tenants/bootstrap", BackendService: "chora-tenancy", AuthMode: route.AuthModePublic},

		// CHO-1655 Setup Wizard Phase A — H+ branding PATCH. Mux registration
		// (phyllis_handler.go) + DefaultJWTGatedPrefixes entry (jwt_auth.go)
		// are not enough on their own — authMiddleware calls routes.Match
		// BEFORE dispatching to the mux, so the path MUST be advertised
		// here or the auth layer returns 404 GATEWAY_ROUTE_NOT_FOUND for
		// every authenticated request. AuthMode is Public because JWT
		// enforcement happens upstream via WithChoraSessionOnPrefixes.
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/tenants/me/branding", BackendService: "chora-tenancy", AuthMode: route.AuthModePublic},
		// CHO-1664 Setup Wizard Phase B — H+ add-ons subscription POST.
		// Same 3-layer wiring discipline as branding above (mux + JWT
		// prefix + this entry). See feedback_gateway_three_layer_route_wiring.
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/tenants/me/addons", BackendService: "chora-tenancy", AuthMode: route.AuthModePublic},
		// CHO-1682 Setup Wizard step 4 Apply — aggregator POST. Backend is
		// chora-gateway itself (the route is served by the BFF; it fans
		// out to identity + tenancy). BackendService is "chora-gateway"
		// so the route-table reflects "no upstream proxy" — the mux
		// handler at /api/v1/tenants/setup answers locally.
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/tenants/setup", BackendService: "chora-gateway", AuthMode: route.AuthModePublic},
		// CHO-1692 Setup Wizard re-entry hydration — GET /api/v1/tenants/me
		// proxies to chora-tenancy's pg-backed MeTenantHandler.
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/tenants/me", BackendService: "chora-tenancy", AuthMode: route.AuthModePublic},
		// CHO-1692 Setup Wizard re-entry hydration — GET /api/v1/tenants/me/idp-providers
		// proxies to chora-identity's MeIdpProvidersHandler.
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/tenants/me/idp-providers", BackendService: "chora-identity", AuthMode: route.AuthModePublic},
		// CHO-1698 STITCH-H-ADD-1 — H+ Add-on Lifecycle dashboard alias.
		// GET /api/v1/admin/tenants/me/addons proxies to chora-tenancy.
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/admin/tenants/me/addons", BackendService: "chora-tenancy", AuthMode: route.AuthModePublic},
		// CHO-1731 STITCH-H-ADD-2 — H+ Add-on Lifecycle deactivation modal.
		// POST /api/v1/admin/tenants/me/addons/{addonPlanId}:deactivate proxies
		// to chora-tenancy's parametric admin endpoint. The wildcard glob
		// covers the {addonPlanId}:deactivate sub-path so Match() against
		// the canonical FE URL resolves without GATEWAY_ROUTE_NOT_FOUND.
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/admin/tenants/me/addons/*", BackendService: "chora-tenancy", AuthMode: route.AuthModePublic},
		// CHO-2148 — H+ tenant external web-egress entitlement (Far Sight opt-in).
		// GET + PATCH /api/v1/admin/tenants/me/external-egress -> chora-tenancy.
		//
		// THIS ENTRY IS LOAD-BEARING. authMiddleware (middleware.go) matches EVERY
		// path against this registry and 404s anything absent BEFORE the mux runs,
		// so an aggregator method + a mux registration are NOT enough on their own —
		// the route is simply invisible without a line here. See memory
		// [[project_gateway_route_three_list_sync]]; route_registry_admin_test.go
		// now fails loudly if a phyllis admin mount is missing from this list.
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/admin/tenants/me/external-egress", BackendService: "chora-tenancy", AuthMode: route.AuthModePublic},
		// CHO-1708 L1 Tenant — H+ members admin (roster search + add-by-email
		// + {gcid}/role change) proxies to chora-identity; me-style
		// TenantManaPool read/create/topup proxies to chora-tenancy's
		// parametric admin endpoints. JWT-gated via the /api/v1/admin/ prefix.
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/admin/tenant-members/*", BackendService: "chora-identity", AuthMode: route.AuthModePublic},
		// WS2 (CHO-1872 / ADR-194 D1) — operator cross-tenant grant. Separate
		// exact entry: "tenant-memberships" is NOT covered by the
		// "tenant-members/*" prefix above (no shared `/` boundary).
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/admin/tenant-memberships", BackendService: "chora-identity", AuthMode: route.AuthModePublic},
		// WS3 (CHO-1873 / ADR-194 D2) — cold-invite create/list/revoke. The base
		// path (POST create / GET list) + a wildcard for the /{inviteId} revoke
		// item path. "tenant-invites" shares no `/` boundary with the
		// tenant-members(hips) entries above, so it needs its own entries.
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/admin/tenant-invites", BackendService: "chora-identity", AuthMode: route.AuthModePublic},
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/admin/tenant-invites/*", BackendService: "chora-identity", AuthMode: route.AuthModePublic},
		// CHO-2103 (W4 Exam BC) — staff manual-doc KYC review. Wildcard for the
		// /{gcid}/verify|reject item paths → chora-identity AdminKycVerifyHandler.
		// JWT-gated via the /api/v1/admin/ prefix; the role gate is DOWNSTREAM
		// (identity adminGate via x-mesh-user-roles). "kyc" shares no `/` boundary
		// with the tenant-* entries, so it needs its own entry.
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/admin/kyc/*", BackendService: "chora-identity", AuthMode: route.AuthModePublic},
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/admin/tenants/me/mana-pool", BackendService: "chora-tenancy", AuthMode: route.AuthModePublic},
		// `:topup` / `:auto-renew` are NOT covered by the mana-pool entry
		// above (Matches() is exact-or-`/*`-prefix; the colon segment shares
		// no `/` boundary) — each colon verb needs its own explicit entry.
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/admin/tenants/me/mana-pool:topup", BackendService: "chora-tenancy", AuthMode: route.AuthModePublic},
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/admin/tenants/me/mana-pool:auto-renew", BackendService: "chora-tenancy", AuthMode: route.AuthModePublic},
		// H+ Billing (CHO-1759 followup) — tenant-scoped Stripe Invoice
		// list + Customer Portal session mint. Backed by chora-payments
		// (NOT chora-tenancy). Both paths JWT-gated; the phyllis aggregator
		// rewrites `me` → AuthCtx.TenantID before forwarding.
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/admin/tenants/me/invoices", BackendService: "chora-payments", AuthMode: route.AuthModePublic},
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/admin/tenants/me/billing-portal", BackendService: "chora-payments", AuthMode: route.AuthModePublic},
		// CHO-1735 H+ Marketplace catalog browse — tenant-agnostic catalog
		// reads. List endpoint at the exact path, detail endpoint covered
		// by the wildcard. authMiddleware calls routes.Match BEFORE the
		// inner mux; without these entries every GET would 404 at the
		// gateway (the CHO-1655 bug class).
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/admin/marketplace/addons", BackendService: "chora-tenancy", AuthMode: route.AuthModePublic},
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/admin/marketplace/addons/*", BackendService: "chora-tenancy", AuthMode: route.AuthModePublic},

		// GraphQL federated endpoint (Phase 31 — learner-facing reads). Public
		// from the BFF's perspective; downstream resolvers stamp identity + RLS
		// based on the JWT. Per CLAUDE.md §4 protocol strategy.
		{Surface: route.SurfaceAPI, PathPattern: "/graphql", BackendService: "chora-bff-graphql", AuthMode: route.AuthModePublic},
		{Surface: route.SurfaceAPI, PathPattern: "/graphql/*", BackendService: "chora-bff-graphql", AuthMode: route.AuthModePublic},
		// FE posts learner GraphQL to /api/v1/graphql (chora-web graphql.service.ts);
		// alias it to the SAME BFF federation handler (mounted in phyllis_handler.go).
		// JWT-gated via the /api/v1/graphql entry in jwt_auth.go DefaultJWTGatedPrefixes.
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/graphql", BackendService: "chora-bff-graphql", AuthMode: route.AuthModePublic},
		{Surface: route.SurfaceAPI, PathPattern: "/api/v1/graphql/*", BackendService: "chora-bff-graphql", AuthMode: route.AuthModePublic},
	}
}
