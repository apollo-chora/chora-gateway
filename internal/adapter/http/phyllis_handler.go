// Phyllis MVP route handlers — exposes the 11 BFF aggregation routes per
// docs/m13/phyllis-mvp-2026-05-08.md (8-step happy-path):
//
//	GET    /api/me                         — chora-identity
//	GET    /api/me/roles?course_id={id}    — chora-identity
//	GET    /api/tenants/me                 — chora-tenancy
//	POST   /api/courses                    — chora-creation + chora-delivery
//	GET    /api/catalog?public=true        — chora-delivery
//	POST   /api/enrollments                — chora-delivery (+ Pub/Sub event)
//	GET    /api/atoms/{id}                 — chora-creation + chora-consumption (parallel)
//	POST   /api/atoms/{id}/feedback        — chora-consumption
//	POST   /api/ai/generate                — chora-model-broker-router
//	GET    /api/companion/me                — chora-consumption
//	GET    /api/companion/daily-dose        — chora-consumption
//
// Auth model: Chora session JWT validated by RequireChoraSessionJWT (when
// wired); claims propagated downstream via servicemesh.MarshalToHeaders. Per
// `feedback_no_inline_config`, downstream URLs are configured via SVC_*_URL
// env vars.
package httpadapter

import (
	"io"
	"net/http"
	"strings"

	"github.com/apollo-chora/chora-common/auth/chorasession"
	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
	"github.com/apollo-chora/chora-gateway/internal/domain/route"
	"github.com/apollo-chora/chora-gateway/internal/domain/session"
)

// PhyllisHandler binds the aggregator to the BFF mux. It is built and wired
// once per process startup.
type PhyllisHandler struct {
	agg *phyllis.Aggregator
}

// NewPhyllisHandler constructs a PhyllisHandler.
func NewPhyllisHandler(agg *phyllis.Aggregator) *PhyllisHandler {
	return &PhyllisHandler{agg: agg}
}

// NewRouterWithPhyllis wires the BFF mux including the Phyllis MVP routes.
// Existing surface aggregation handlers (/bff/aplus/home, etc.) and the
// session/proxy endpoints are preserved.
func NewRouterWithPhyllis(routes route.Repository, sessions session.Repository, up upstream.Client, agg *phyllis.Aggregator) http.Handler {
	return NewRouterWithPhyllisAndGraphQL(routes, sessions, up, agg, nil)
}

// NewRouterWithPhyllisAndGraphQL extends NewRouterWithPhyllis with an
// optional /graphql federated endpoint (Phase 31). When graphqlHandler is
// non-nil it serves learner-facing reads alongside the existing REST routes
// per CLAUDE.md §4. Existing REST routes are NEVER removed.
//
// JWT validation is OFF in this constructor — call
// NewRouterWithPhyllisGraphQLAndChoraSession to enable production-ready
// Chora session JWT validation on /api/* and /graphql.
func NewRouterWithPhyllisAndGraphQL(routes route.Repository, sessions session.Repository, up upstream.Client, agg *phyllis.Aggregator, graphqlHandler http.Handler) http.Handler {
	h := NewHandler(routes, sessions, up)
	ph := NewPhyllisHandler(agg)

	mux := http.NewServeMux()
	// Health + service info (public).
	mux.HandleFunc("/healthz", healthz)
	mux.HandleFunc("/healthz/", healthz)
	mux.HandleFunc("/health", healthz)
	mux.HandleFunc("/readyz", h.readyz)
	mux.HandleFunc("/version", h.version)
	mux.HandleFunc("/", h.index)

	// Auth endpoints.
	mux.HandleFunc("/api/auth/session", h.session)

	// Existing BFF surface composition endpoints.
	mux.HandleFunc("/bff/aplus/home", h.aplusHome)
	mux.HandleFunc("/bff/cplus/feed", h.cplusFeed)
	mux.HandleFunc("/bff/hplus/tenant", h.hplusTenant)
	mux.HandleFunc("/bff/oplus/governance", h.oplusGovernance)
	mux.HandleFunc("/bff/rplus/courses", h.rplusCourses)

	// Phyllis MVP routes (Bearer pass-through; auth enforced by downstream
	// chora-identity).
	mux.HandleFunc("/api/me", ph.handleMe)
	mux.HandleFunc("/api/me/roles", ph.handleMeRoles)
	mux.HandleFunc("/api/tenants/me", ph.handleTenantsMe)
	mux.HandleFunc("/api/courses", ph.handleCourses)
	mux.HandleFunc("/api/catalog", ph.handleCatalog)
	mux.HandleFunc("/api/enrollments", ph.handleEnrollments)
	// M14.iter5 — AI Assist route MUST register BEFORE the /api/atoms/
	// prefix so exact-path match wins. Phyllis Step 4 (chora-creation → QGen
	// engine on us-central1 per ADR-148).
	mux.HandleFunc("/api/atoms/ai-assist", ph.handleAIAssist)
	// OE-AI-ASSIST Step 5 — async job status subtree prefix-match. Routes
	// GET /api/atoms/ai-assist/{job_id} through chora-creation's
	// getAiAssistJob handler. MUST register BEFORE /api/atoms/ so
	// longest-prefix-match wins (otherwise handleAtoms would mis-parse
	// `ai-assist` as an atom_id).
	mux.HandleFunc("/api/atoms/ai-assist/", ph.handleAIAssistJobStatus)
	mux.HandleFunc("/api/atoms/", ph.handleAtoms)
	mux.HandleFunc("/api/ai/generate", ph.handleAIGenerate)
	mux.HandleFunc("/api/companion/me", ph.handleCompanionMe)
	mux.HandleFunc("/api/companion/daily-dose", ph.handleCompanionDailyDose)
	// Debt #41 close (2026-05-16) — bare /companion/daily-dose alias.
	// CR v4 rehearsal row 12 probed the bare path (no /api/ prefix) and
	// got 404 because the gateway only claimed /api/companion/daily-dose.
	// chora-consumption's handler IS mounted at the bare path
	// (services/chora-consumption/.../router.go:254), so the alias is a
	// pure gateway-claim addition — same aggregator, same downstream
	// call, same response. JWT auth is gated by the /companion/ entry in
	// DefaultJWTGatedPrefixes (jwt_auth.go).
	mux.HandleFunc("/companion/daily-dose", ph.handleCompanionDailyDose)
	// ADR-254 D9 alias, drop after the SPA cut (WP-X go): the pre-rename
	// /api/familiar/me, /api/familiar/daily-dose and bare /familiar/daily-dose
	// paths bind to the SAME handlers (same aggregator, same downstream call).
	// The inmem route table (route_repository.go) and DefaultJWTGatedPrefixes
	// (jwt_auth.go) carry the matching alias entries.
	mux.HandleFunc("/api/familiar/me", ph.handleCompanionMe)
	mux.HandleFunc("/api/familiar/daily-dose", ph.handleCompanionDailyDose)
	mux.HandleFunc("/familiar/daily-dose", ph.handleCompanionDailyDose)
	// M14.iter-kg-fog — A+ Knowledge-Graph dashboard panel routes per
	// docs/m13/kg-fog-aplus-integration-backend-handoff-2026-05-13.md §1.1+§1.2.
	mux.HandleFunc("/api/v1/me/knowledge-graph/clusters", ph.handleKGClusters)

	// PE-14 — A+ GDPR consent center routes (GDPR Art. 15/20). Account
	// closure (Art. 17) moved to the canonical /api/v1/me/account/close
	// saga me-route (CHO-1719, closure_handler.go); the legacy
	// /api/me/account-closure → orchestrator:/sagas BFF route was dead
	// (no live caller; /sagas never implemented) and is removed (CHO-1790 D12).
	mux.HandleFunc("/api/me/consents", ph.handleGdprConsents)
	mux.HandleFunc("/api/me/consents/grant", ph.handleGdprConsentGrant)
	mux.HandleFunc("/api/me/data-export", ph.handleGdprDataExport)

	// CHO-1632 Phase 3 — H+ Setup-Tenant bootstrap. Forwards to
	// chora-tenancy `/v1/tenants/bootstrap` (CHO-1628). JWT-gated +
	// GCID stamped from the validated Chora session.
	mux.HandleFunc("/api/v1/tenants/bootstrap", ph.handleTenantsBootstrap)
	// Setup Wizard entry points that read the caller's active tenant, all
	// mounted through the one registrar in wizard_active_tenant.go so each is
	// behind requireActiveTenant. Exact-path discipline is preserved there:
	// they dispatch before any wildcard tenant route, and the trailing-slash
	// idp-providers entry still captures {providerType}.
	registerWizardRoutes(mux, ph)
	// Ownership handover (UX Track U, E3). The me-routes go behind the same
	// requireActiveTenant guard for the same reason; the operator override
	// deliberately does not, because PLATFORM_OPERATOR is tenant-less and
	// gating it on an active tenant would make it unreachable by the only
	// role allowed to use it.
	registerOwnershipRoutes(mux, ph)
	// Setup Wizard Phase A (CHO-1655) — PATCH only; mounted at exact
	// path so it dispatches before any wildcard tenant routes.
	// Setup Wizard Phase B (CHO-1664) — POST only. Same exact-path
	// discipline so it dispatches before any wildcard tenant routes.
	// Setup Wizard step 4 Apply (CHO-1682) — POST aggregator. Fans out
	// to chora-identity's POST /me/idp-providers + chora-tenancy's
	// PATCH /me/finish-setup. Exact-path mount so it dispatches before
	// any wildcard tenant routes; the handler lives in
	// tenancy_setup_handler.go.
	// Setup Wizard re-entry hydration (CHO-1692) — GET /api/v1/tenants/me
	// proxies to chora-tenancy's pg-backed MeTenantHandler. Exact-path
	// mount so it dispatches before any wildcard tenant routes.
	// Setup Wizard re-entry hydration (CHO-1692) — GET /api/v1/tenants/me/idp-providers
	// proxies to chora-identity's MeIdpProvidersHandler GET branch.
	// H+ IdP Federation Disconnect (CHO-1694) — DELETE on the sub-path
	// /api/v1/tenants/me/idp-providers/{providerType}. The trailing-slash
	// mux entry captures the sub-path and dispatches through the same
	// handler, which routes by method.
	// H+ Add-on Lifecycle dashboard (CHO-1698 STITCH-H-ADD-1) — GET
	// /api/v1/admin/tenants/me/addons proxies to chora-tenancy's
	// parametric `/api/v1/admin/tenants/{tenantId}/addons` with
	// tenantId from the validated JWT. The FE keeps a stable `me`-style
	// URL; backend remains parametric for cross-tenant admin paths.
	mux.HandleFunc("/api/v1/admin/tenants/me/addons", ph.handleAdminTenantsMeAddons)
	// H+ Add-on Lifecycle deactivation modal (CHO-1731 STITCH-H-ADD-2) —
	// POST /api/v1/admin/tenants/me/addons/{addonPlanId}:deactivate. The
	// trailing-slash mux mount captures the {addonPlanId}:deactivate
	// sub-path; the handler parses the planID + dispatches based on the
	// `:deactivate` suffix (only action recognised in v1).
	mux.HandleFunc("/api/v1/admin/tenants/me/addons/", ph.handleAdminTenantsMeAddonsAction)

	// L1 Tenant lane (CHO-1708) — H+ members admin + me-style mana-pool.
	// The trailing-slash mount captures PATCH
	// /api/v1/admin/tenant-members/{gcid}/role (idp-providers precedent);
	// the /api/v1/admin/ prefix is already JWT-gated (B6.1).
	mux.HandleFunc("/api/v1/admin/tenant-members", ph.handleAdminTenantMembers)
	mux.HandleFunc("/api/v1/admin/tenant-members/", ph.handleAdminTenantMembers)
	// WS2 (CHO-1872 / ADR-194 D1) — operator cross-tenant grant. Exact mount:
	// the trailing path "tenant-membership*s*" is NOT caught by the
	// "tenant-members/" subtree above. POST only; tenant is in the body.
	mux.HandleFunc("/api/v1/admin/tenant-memberships", ph.handleAdminTenantMemberships)
	// WS3 (CHO-1873 / ADR-194 D2) — cold-invite admin API. Base path (POST
	// create / GET list) + trailing-slash subtree (DELETE /{inviteId} revoke).
	// "tenant-invites" shares no `/` boundary with the tenant-members subtree.
	mux.HandleFunc("/api/v1/admin/tenant-invites", ph.handleAdminTenantInvites)
	mux.HandleFunc("/api/v1/admin/tenant-invites/", ph.handleAdminTenantInvites)
	// CHO-2103 (W4 Exam BC) — staff manual-doc KYC review. Subtree only
	// ({gcid}/verify|reject always parametric) → chora-identity AdminKycVerifyHandler.
	// JWT-gated by the broad "/api/v1/admin/" prefix; role gated DOWNSTREAM (identity
	// adminGate via x-mesh-user-roles).
	mux.HandleFunc("/api/v1/admin/kyc/", ph.handleAdminKyc)
	// CHO-2148 — the H+ tenant external web-egress entitlement (Far Sight
	// opt-in). GET reads (default-deny when the tenant never opted in);
	// PATCH writes the policy + the policy-changed event in one upstream tx.
	mux.HandleFunc("/api/v1/admin/tenants/me/external-egress", ph.handleAdminTenantsMeExternalEgress)
	mux.HandleFunc("/api/v1/admin/tenants/me/mana-pool", ph.handleAdminTenantsMeManaPool)
	mux.HandleFunc("/api/v1/admin/tenants/me/mana-pool:topup", ph.handleAdminTenantsMeManaPoolTopup)
	mux.HandleFunc("/api/v1/admin/tenants/me/mana-pool:auto-renew", ph.handleAdminTenantsMeManaPoolAutoRenew)
	// H+ Billing (CHO-1759 followup) — tenant-scoped Stripe Invoice list +
	// Customer Portal session mint. Backed by chora-payments (PaymentsURL).
	// Exact-path mounts; both rewrite `me` → AuthCtx.TenantID before the
	// outbound call.
	mux.HandleFunc("/api/v1/admin/tenants/me/invoices", ph.handleAdminTenantsMeInvoices)
	mux.HandleFunc("/api/v1/admin/tenants/me/billing-portal", ph.handleAdminTenantsMeBillingPortal)
	// H+ Marketplace catalog browse (CHO-1735) — tenant-agnostic catalog
	// reads. Exact mount for the list endpoint + trailing-slash mount for
	// the {addonPlanId} detail sub-path. Both dispatch via
	// handleAdminMarketplaceAddons (single handler, switches on the tail).
	mux.HandleFunc("/api/v1/admin/marketplace/addons", ph.handleAdminMarketplaceAddons)
	mux.HandleFunc("/api/v1/admin/marketplace/addons/", ph.handleAdminMarketplaceAddons)

	// GraphQL federated endpoint (Phase 31 — learner-facing reads).
	// The FE posts to /api/v1/graphql (chora-web graphql.service.ts); alias it
	// to the SAME federation handler so it is reachable through the /api/v1
	// edge (the bare /graphql is preserved). Fixes the shell-wide 404.
	if graphqlHandler != nil {
		mux.Handle("/graphql", graphqlHandler)
		mux.Handle("/graphql/", graphqlHandler)
		mux.Handle("/api/v1/graphql", graphqlHandler)
		mux.Handle("/api/v1/graphql/", graphqlHandler)
	}

	// Generic proxy mode.
	mux.HandleFunc("/api/proxy/", h.genericProxy)

	return traceContext(logging(h.authMiddleware(mux)))
}

// -----------------------------------------------------------------------------
// Auth + tracing extraction
// -----------------------------------------------------------------------------

// authCtxFromRequest extracts the Phyllis AuthCtx from the request (Bearer
// token, traceparent context value, X-Tenant-Id header).
//
// When the JWT validation middleware (RequireChoraSessionJWT) ran upstream
// the GCID, TenantID, RoleSummary fields are populated from the validated
// claims so outbound calls stamp the canonical chora-gcid / chora-tenant-id
// / chora-role-summary mesh metadata headers. When the middleware did NOT
// run (e.g. session-token auth path), GCID + RoleSummary remain empty and
// only X-Tenant-Id falls back from the inbound header.
func authCtxFromRequest(r *http.Request) phyllis.AuthCtx {
	tp, _ := r.Context().Value(ctxKeyTraceparent).(string)
	ac := phyllis.AuthCtx{
		Bearer:         bearerToken(r),
		Traceparent:    tp,
		IdempotencyKey: strings.TrimSpace(r.Header.Get("Idempotency-Key")),
	}
	mc, hasMeshClaims := MeshClaimsFromContext(r.Context())
	if hasMeshClaims {
		ac.GCID = mc.GCID
		ac.TenantID = mc.TenantID
		ac.RoleSummary = mc.RoleSummary
		// WS-0b A16 — propagate typed Roles[] so role-gated aggregators
		// (e.g. GetAtom author-mode correct_option_id) can fast-check
		// without parsing the legacy RoleSummary JSON.
		if len(mc.Roles) > 0 {
			ac.Roles = append([]string(nil), mc.Roles...)
		}
	}
	// D1.5 fix — defensive fallback to the raw validated ChoraSession claims.
	// Identity-needing routes (e.g. /api/companion/daily-dose, which is
	// per-user) MUST carry a GCID downstream. RequireChoraSessionJWT stamps
	// BOTH MeshClaims and the raw ChoraSession claims; if for any route-
	// ordering reason only the raw claims landed on the context, recover
	// GCID + TenantID from them so the outbound call still stamps the gcid +
	// X-Tenant-Id headers chora-consumption's requireContext requires.
	cs, hasSessionClaims := ChoraSessionClaimsFromContext(r.Context())
	if hasSessionClaims {
		if ac.GCID == "" {
			ac.GCID = cs.GCID
		}
		if ac.TenantID == "" {
			ac.TenantID = cs.TenantID
		}
	}

	// The inbound X-Tenant-Id is consulted ONLY when no validated session of
	// either kind is on the context, which is the pre-existing behaviour for
	// routes that legitimately have no session (the public catalogue).
	//
	// It used to seed the field before the claims were read, and the ordering
	// was the defect rather than any single line. A validated session whose
	// mesh claims carry no tenant fell through to the header, and since
	// PLATFORM_OPERATOR is tenant-less by design that is an ordinary state.
	// Worse, the fallback above is guarded on an empty tenant, so a header had
	// already filled the field and PRE-EMPTED the validated session's own
	// tenant, which is the opposite of what that fallback exists to do.
	//
	// A validated session with no tenant in either claim set now keeps an
	// EMPTY tenant, so the downstream refusal or the wizard's named 409 says
	// so, and GetCatalog's anonymous caller stays pinned to visibility=public
	// exactly as its comment intends.
	if !hasMeshClaims && !hasSessionClaims {
		ac.TenantID = r.Header.Get("X-Tenant-Id")
	}
	return ac
}

// catalogAuthFromRequest is authCtxFromRequest with the tenant scrubbed for a
// caller who has not signed in.
//
// /api/catalog is public by design, and on every other session-less route the
// inbound X-Tenant-Id is inert. Here it is not: GetCatalog sends
// visibility=tenant_or_public whenever a tenant is present and the caller did
// not ask for public=true, and its own comment records why that must never
// happen anonymously ("sending tenant_or_public without a tenant would leak
// other tenants' non-public courses"). The SPA's search path calls this route
// without public=true, so the flip is reachable.
//
// The trusted tenant for an anonymous caller is no tenant at all. That needs
// no chora-master default and no hostname mapping: it is the honest answer,
// and it is exactly the value GetCatalog needs to pin visibility=public.
func catalogAuthFromRequest(r *http.Request) phyllis.AuthCtx {
	ac := authCtxFromRequest(r)
	if !hasValidatedSession(r) {
		ac.TenantID = ""
	}
	return ac
}

// hasValidatedSession reports whether the request carries validated claims of
// either kind. Both are stamped by RequireChoraSessionJWT; a request with
// neither was never authenticated.
func hasValidatedSession(r *http.Request) bool {
	if _, ok := MeshClaimsFromContext(r.Context()); ok {
		return true
	}
	_, ok := ChoraSessionClaimsFromContext(r.Context())
	return ok
}

// readBody reads up to 1 MiB of the request body — defensive cap for the BFF.
func readBody(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	defer r.Body.Close()
	const maxBytes = 1 << 20
	b, _ := io.ReadAll(io.LimitReader(r.Body, maxBytes))
	return b
}

// writePhyllisResp writes the aggregator Response to the wire, normalising
// headers and Content-Type.
func writePhyllisResp(w http.ResponseWriter, resp phyllis.Response) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// requireMethod returns true when the request's method matches one of the
// allowed values; otherwise it writes a 405 envelope and returns false.
func requireMethod(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	for _, m := range allowed {
		if r.Method == m {
			return true
		}
	}
	writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
		"method "+r.Method+" not allowed; want "+strings.Join(allowed, "|"))
	return false
}

// -----------------------------------------------------------------------------
// Handlers
// -----------------------------------------------------------------------------

// handleMe — GET /api/me
func (p *PhyllisHandler) handleMe(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	resp, _ := p.agg.GetMe(r.Context(), authCtxFromRequest(r))
	writePhyllisResp(w, resp)
}

// handleMeRoles — GET /api/me/roles?course_id=X
func (p *PhyllisHandler) handleMeRoles(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	courseID := r.URL.Query().Get("course_id")
	resp, _ := p.agg.GetMyRoles(r.Context(), authCtxFromRequest(r), courseID)
	writePhyllisResp(w, resp)
}

// handleTenantsMe — GET /api/tenants/me
func (p *PhyllisHandler) handleTenantsMe(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	resp, _ := p.agg.GetMyTenant(r.Context(), authCtxFromRequest(r))
	writePhyllisResp(w, resp)
}

// handleTenantsMeV1 — GET /api/v1/tenants/me (CHO-1692).
// Proxies to chora-tenancy's pg-backed MeTenantHandler at the same path
// with `X-Tenant-Id` + `gcid` stamped from the validated JWT. Distinct
// from the legacy /api/tenants/me which reads from an in-memory registry
// and returns a different DTO shape lacking wizard_completed_at.
func (p *PhyllisHandler) handleTenantsMeV1(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	resp, _ := p.agg.GetMyTenantV1(r.Context(), authCtxFromRequest(r))
	writePhyllisResp(w, resp)
}

// handleAdminTenantsMeInvoices proxies GET /api/v1/admin/tenants/me/invoices
// to chora-payments via the phyllis aggregator. CHO-1759 followup — H+
// Billing Wave 4.
func (p *PhyllisHandler) handleAdminTenantsMeInvoices(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	resp, _ := p.agg.ListAdminMyInvoices(r.Context(), r.URL.RawQuery, authCtxFromRequest(r))
	writePhyllisResp(w, resp)
}

// handleAdminTenantsMeBillingPortal proxies POST
// /api/v1/admin/tenants/me/billing-portal to chora-payments. Body
// carries `return_url`; the upstream returns `{ "url": "..." }` for the
// FE to redirect to.
func (p *PhyllisHandler) handleAdminTenantsMeBillingPortal(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	resp, _ := p.agg.CreateAdminMyBillingPortalSession(r.Context(), readBody(r), authCtxFromRequest(r))
	writePhyllisResp(w, resp)
}

// handleAdminTenantsMeAddons proxies GET /api/v1/admin/tenants/me/addons
// to chora-tenancy's parametric admin endpoint (CHO-1698 STITCH-H-ADD-1).
// GET only; everything else returns 405.
func (p *PhyllisHandler) handleAdminTenantsMeAddons(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	resp, _ := p.agg.ListAdminMyAddons(r.Context(), authCtxFromRequest(r))
	writePhyllisResp(w, resp)
}

// handleAdminMarketplaceAddons dispatches the H+ Marketplace catalog
// browse endpoints (CHO-1735). Both share a single handler because the
// only distinguishing axis is the URL tail:
//
//   - GET /api/v1/admin/marketplace/addons        — paginated list
//   - GET /api/v1/admin/marketplace/addons/{id}   — detail
//
// Anything else (non-GET, deeper sub-paths) returns 405 / 404 with a
// specific error code so a typo on the FE side surfaces immediately
// instead of silently 5xx'ing upstream.
func (p *PhyllisHandler) handleAdminMarketplaceAddons(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	const prefix = "/api/v1/admin/marketplace/addons"
	tail := strings.TrimPrefix(r.URL.Path, prefix)
	switch {
	case tail == "" || tail == "/":
		// List endpoint — forward the raw query string verbatim.
		resp, _ := p.agg.ListMarketplaceAddons(r.Context(), r.URL.RawQuery, authCtxFromRequest(r))
		writePhyllisResp(w, resp)
		return
	case strings.HasPrefix(tail, "/"):
		// Detail endpoint — extract the planId from /{addonPlanId} and
		// reject any deeper nesting (no sub-resources defined for v1).
		planID := strings.TrimPrefix(tail, "/")
		if planID == "" {
			writeError(w, http.StatusBadRequest, "GATEWAY_PLAN_ID_REQUIRED",
				"addonPlanId is empty")
			return
		}
		if strings.Contains(planID, "/") {
			writeError(w, http.StatusNotFound, "GATEWAY_SUBRESOURCE_UNKNOWN",
				"unknown sub-resource on /api/v1/admin/marketplace/addons/{addonPlanId}")
			return
		}
		resp, _ := p.agg.GetMarketplaceAddonDetail(r.Context(), planID, authCtxFromRequest(r))
		writePhyllisResp(w, resp)
		return
	default:
		writeError(w, http.StatusNotFound, "GATEWAY_ROUTE_NOT_FOUND",
			"unrecognised path under /api/v1/admin/marketplace/addons")
	}
}

// handleAdminTenantsMeAddonsAction dispatches the sub-paths under
// /api/v1/admin/tenants/me/addons/{addonPlanId}... in three shapes:
//
//   - Action style: /{addonPlanId}:{action} — e.g. `:deactivate`
//     (CHO-1731 STITCH-H-ADD-2). The colon separates plan ID from verb.
//   - Sub-resource style: /{addonPlanId}/{subresource} — e.g.
//     `/usage` (CHO-1732 STITCH-H-ADD-3). The slash separates plan ID
//     from the sub-resource name.
//   - Bare item style: /{addonPlanId} — dispatched by HTTP method.
//     PATCH → change-tier (CHO-1733 STITCH-H-ADD-4).
//
// Unknown actions / sub-resources / methods return 404 / 405 with a
// specific error code so a typo on the FE side surfaces immediately
// instead of silently 5xx'ing upstream.
//
// The trailing-slash mux mount also catches the bare exact path (when no
// exact entry exists), so we guard the empty-tail case explicitly.
func (p *PhyllisHandler) handleAdminTenantsMeAddonsAction(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/v1/admin/tenants/me/addons/"
	tail := strings.TrimPrefix(r.URL.Path, prefix)
	if tail == "" {
		// Defensive: the exact path is handled by handleAdminTenantsMeAddons.
		// If somehow we got here with an empty tail, it's a bad URL.
		writeError(w, http.StatusNotFound, "GATEWAY_ROUTE_NOT_FOUND",
			"empty sub-path under /api/v1/admin/tenants/me/addons/")
		return
	}
	// Sub-resource style (slash) takes precedence — the addonPlanId is
	// a UUID and contains no '/', so the first slash splits cleanly into
	// {planId, subresource[/...]}.
	if slashIdx := strings.IndexByte(tail, '/'); slashIdx >= 0 {
		addonPlanID := tail[:slashIdx]
		subresource := tail[slashIdx+1:]
		if addonPlanID == "" {
			writeError(w, http.StatusBadRequest, "GATEWAY_PLAN_ID_REQUIRED",
				"addonPlanId is empty")
			return
		}
		switch subresource {
		case "usage":
			if !requireMethod(w, r, http.MethodGet) {
				return
			}
			resp, _ := p.agg.GetAdminMyAddonUsage(r.Context(), addonPlanID, r.URL.RawQuery, authCtxFromRequest(r))
			writePhyllisResp(w, resp)
		case "preview-tier-change":
			// CHO-1768 — preview Stripe proration delta before commit.
			// Filed as post-CHO-1759 epic gap (CHO-1765 + CHO-1767 BE shipped without the gateway dispatch).
			if !requireMethod(w, r, http.MethodPost) {
				return
			}
			resp, _ := p.agg.PreviewAdminMyAddonTier(r.Context(), addonPlanID, readBody(r), authCtxFromRequest(r))
			writePhyllisResp(w, resp)
		default:
			writeError(w, http.StatusNotFound, "GATEWAY_SUBRESOURCE_UNKNOWN",
				"unrecognised sub-resource '/"+subresource+"' on /api/v1/admin/tenants/me/addons/{addonPlanId}")
		}
		return
	}
	// Action style (colon).
	if colonIdx := strings.LastIndexByte(tail, ':'); colonIdx >= 0 {
		addonPlanID, action := tail[:colonIdx], tail[colonIdx+1:]
		if addonPlanID == "" {
			writeError(w, http.StatusBadRequest, "GATEWAY_PLAN_ID_REQUIRED",
				"addonPlanId is empty")
			return
		}
		switch action {
		case "deactivate":
			if !requireMethod(w, r, http.MethodPost) {
				return
			}
			resp, _ := p.agg.DeactivateAdminMyAddon(r.Context(), addonPlanID, readBody(r), authCtxFromRequest(r))
			writePhyllisResp(w, resp)
		case "activate":
			// CHO-1742 — free ($0) addon Activate-Free path.
			if !requireMethod(w, r, http.MethodPost) {
				return
			}
			resp, _ := p.agg.ActivateAdminMyAddon(r.Context(), addonPlanID, readBody(r), authCtxFromRequest(r))
			writePhyllisResp(w, resp)
		case "cancel-deactivation":
			// CHO-1785 — undo a pending end-of-cycle deactivation.
			if !requireMethod(w, r, http.MethodPost) {
				return
			}
			resp, _ := p.agg.CancelDeactivationAdminMyAddon(r.Context(), addonPlanID, authCtxFromRequest(r))
			writePhyllisResp(w, resp)
		default:
			writeError(w, http.StatusNotFound, "GATEWAY_ACTION_UNKNOWN",
				"unrecognised action ':"+action+"' on /api/v1/admin/tenants/me/addons/{addonPlanId}")
		}
		return
	}
	// Bare item style: /{addonPlanId} — dispatch by HTTP method.
	// CHO-1733 (PATCH = change-tier). Future stories may add GET (detail).
	addonPlanID := tail
	switch r.Method {
	case http.MethodGet:
		// CHO-1734 (STITCH-H-ADD-5) — marketplace tile detail.
		resp, _ := p.agg.GetAdminMyAddonDetail(r.Context(), addonPlanID, authCtxFromRequest(r))
		writePhyllisResp(w, resp)
	case http.MethodPatch:
		resp, _ := p.agg.ChangeAdminMyAddonTier(r.Context(), addonPlanID, readBody(r), authCtxFromRequest(r))
		writePhyllisResp(w, resp)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"method "+r.Method+" not allowed on /api/v1/admin/tenants/me/addons/{addonPlanId}; want GET or PATCH")
	}
}

// handleAdminTenantMembers proxies the L1 tenant-members admin surface to
// chora-identity (CHO-1708). Dispatches:
//
//   - GET   /api/v1/admin/tenant-members               (B6.1 roster search)
//   - POST  /api/v1/admin/tenant-members               (add-member-by-email)
//   - PATCH /api/v1/admin/tenant-members/{gcid}/role   (role change)
//
// Everything else returns 405 (collection) / 400 (malformed item path).
func (p *PhyllisHandler) handleAdminTenantMembers(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/admin/tenant-members"), "/")
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			resp, _ := p.agg.SearchAdminTenantMembers(r.Context(), authCtxFromRequest(r), r.URL.RawQuery)
			writePhyllisResp(w, resp)
		case http.MethodPost:
			resp, _ := p.agg.AddAdminTenantMember(r.Context(), authCtxFromRequest(r), readBody(r))
			writePhyllisResp(w, resp)
		default:
			writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
				"GET or POST only on /api/v1/admin/tenant-members")
		}
		return
	}
	parts := strings.Split(rest, "/")
	if len(parts) == 1 {
		// Bare /{gcid} — member-centric remove (WS2b / CHO-1869). DELETE only.
		if !requireMethod(w, r, http.MethodDelete) {
			return
		}
		resp, _ := p.agg.RemoveAdminTenantMember(r.Context(), authCtxFromRequest(r), parts[0])
		writePhyllisResp(w, resp)
		return
	}
	if len(parts) != 2 {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_PATH",
			"path must be /api/v1/admin/tenant-members/{gcid} or /{gcid}/role|roles|display-name")
		return
	}
	switch parts[1] {
	case "role":
		if !requireMethod(w, r, http.MethodPatch) {
			return
		}
		resp, _ := p.agg.ChangeAdminTenantMemberRole(r.Context(), authCtxFromRequest(r), parts[0], readBody(r))
		writePhyllisResp(w, resp)
	case "roles":
		// CHO-1809 follow-up — multi-role REPLACE editor. PUT with the
		// full role-set body. Mirrors the chora-identity dispatcher.
		if !requireMethod(w, r, http.MethodPut) {
			return
		}
		resp, _ := p.agg.SetAdminTenantMemberRoles(r.Context(), authCtxFromRequest(r), parts[0], readBody(r))
		writePhyllisResp(w, resp)
	case "display-name":
		// CHO-1817 follow-up — admin patches a member's display name.
		if !requireMethod(w, r, http.MethodPatch) {
			return
		}
		resp, _ := p.agg.SetAdminTenantMemberDisplayName(r.Context(), authCtxFromRequest(r), parts[0], readBody(r))
		writePhyllisResp(w, resp)
	default:
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_PATH",
			"path must be /api/v1/admin/tenant-members/{gcid}/role, /roles or /display-name")
	}
}

// handleAdminTenantMemberships proxies POST /api/v1/admin/tenant-memberships
// to chora-identity grantTenantMembership (ADR-194 D1, WS2 / CHO-1872) — the
// operator cross-tenant grant. POST only; the target tenant is in the body, so
// there are no {membershipId} subpaths on this leaf (revoke is WS2b).
func (p *PhyllisHandler) handleAdminTenantMemberships(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	resp, _ := p.agg.GrantTenantMembership(r.Context(), authCtxFromRequest(r), readBody(r))
	writePhyllisResp(w, resp)
}

// handleAdminKyc dispatches the staff manual-doc KYC review subtree (CHO-2103):
//
//	POST /api/v1/admin/kyc/{gcid}/verify  → chora-identity adminKycVerify
//	POST /api/v1/admin/kyc/{gcid}/reject  → chora-identity adminKycReject
//
// {gcid} is the SUBJECT of the verification; body forwarded verbatim. Malformed
// paths 400 and non-POST 405 BEFORE any upstream call (identity's adminGate +
// FSM own the real authz + state gating).
func (p *PhyllisHandler) handleAdminKyc(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/admin/kyc"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_PATH",
			"path must be /api/v1/admin/kyc/{gcid}/verify or /reject")
		return
	}
	switch parts[1] {
	case "verify":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		resp, _ := p.agg.VerifyAdminKyc(r.Context(), authCtxFromRequest(r), parts[0], readBody(r))
		writePhyllisResp(w, resp)
	case "reject":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		resp, _ := p.agg.RejectAdminKyc(r.Context(), authCtxFromRequest(r), parts[0], readBody(r))
		writePhyllisResp(w, resp)
	default:
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_PATH",
			"path must be /api/v1/admin/kyc/{gcid}/verify or /reject")
	}
}

// handleAdminTenantInvites proxies the cold-invite admin surface to
// chora-identity (ADR-194 D2, WS3 / CHO-1873). Dispatches:
//
//   - POST   /api/v1/admin/tenant-invites              (create — grant existing
//     user now OR persist a pending cold invite)
//   - GET    /api/v1/admin/tenant-invites              (list pending invites)
//   - DELETE /api/v1/admin/tenant-invites/{inviteId}   (revoke)
//
// The base path carries no tenant on the operator create (body-tenant); list +
// revoke are tenant-scoped (the aggregator requireTenant-gates them).
func (p *PhyllisHandler) handleAdminTenantInvites(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/admin/tenant-invites"), "/")
	if rest == "" {
		switch r.Method {
		case http.MethodPost:
			resp, _ := p.agg.CreateTenantInvite(r.Context(), authCtxFromRequest(r), readBody(r))
			writePhyllisResp(w, resp)
		case http.MethodGet:
			resp, _ := p.agg.ListTenantInvites(r.Context(), authCtxFromRequest(r))
			writePhyllisResp(w, resp)
		default:
			writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
				"GET or POST only on /api/v1/admin/tenant-invites")
		}
		return
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 1 {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_PATH",
			"path must be /api/v1/admin/tenant-invites/{inviteId}")
		return
	}
	if !requireMethod(w, r, http.MethodDelete) {
		return
	}
	resp, _ := p.agg.RevokeTenantInvite(r.Context(), authCtxFromRequest(r), parts[0])
	writePhyllisResp(w, resp)
}

// handleAdminTenantsMeManaPool proxies the me-style TenantManaPool read +
// create to chora-tenancy's parametric admin endpoint (CHO-1708).
// handleAdminTenantsMeExternalEgress proxies the H+ tenant external web-egress
// entitlement to chora-tenancy (CHO-2148). GET reads; PATCH writes.
//
// PATCH (not PUT): BffClientService on the FE has no put(), chora-tenancy has no
// PUT handlers, and the upstream is a merge-upsert. Same semantics, no new verb.
func (p *PhyllisHandler) handleAdminTenantsMeExternalEgress(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		resp, _ := p.agg.GetAdminMyExternalEgress(r.Context(), authCtxFromRequest(r))
		writePhyllisResp(w, resp)
	case http.MethodPatch:
		resp, _ := p.agg.SetAdminMyExternalEgress(r.Context(), authCtxFromRequest(r), readBody(r))
		writePhyllisResp(w, resp)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET or PATCH only on /api/v1/admin/tenants/me/external-egress")
	}
}

func (p *PhyllisHandler) handleAdminTenantsMeManaPool(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		resp, _ := p.agg.GetAdminMyManaPool(r.Context(), authCtxFromRequest(r))
		writePhyllisResp(w, resp)
	case http.MethodPost:
		resp, _ := p.agg.CreateAdminMyManaPool(r.Context(), authCtxFromRequest(r), readBody(r))
		writePhyllisResp(w, resp)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET or POST only on /api/v1/admin/tenants/me/mana-pool")
	}
}

// handleAdminTenantsMeManaPoolTopup proxies the me-style TenantManaPool
// top-up to chora-tenancy (CHO-1708). POST only.
func (p *PhyllisHandler) handleAdminTenantsMeManaPoolTopup(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	resp, _ := p.agg.TopupAdminMyManaPool(r.Context(), authCtxFromRequest(r), readBody(r))
	writePhyllisResp(w, resp)
}

// handleAdminTenantsMeManaPoolAutoRenew proxies the me-style TenantManaPool
// auto-renew config to chora-tenancy (CHO-1708 WP-4). POST only.
func (p *PhyllisHandler) handleAdminTenantsMeManaPoolAutoRenew(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	resp, _ := p.agg.SetAutoRenewAdminMyManaPool(r.Context(), authCtxFromRequest(r), readBody(r))
	writePhyllisResp(w, resp)
}

// handleTenantsMeIdpProviders proxies the IdP provider read + delete
// surface to chora-identity. Dispatches:
//
//   - GET    /api/v1/tenants/me/idp-providers                       (CHO-1692 list)
//   - DELETE /api/v1/tenants/me/idp-providers/{providerType}        (CHO-1694 disconnect)
//
// The POST half (step 4 Apply) goes through the aggregator at
// /api/v1/tenants/setup, not this handler. Everything else returns 405.
func (p *PhyllisHandler) handleTenantsMeIdpProviders(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		resp, _ := p.agg.ListMyIdpProviders(r.Context(), authCtxFromRequest(r))
		writePhyllisResp(w, resp)
	case http.MethodDelete:
		// Parse {providerType} from the sub-path so we can forward it
		// in the URL (chora-identity's handler also parses on its own
		// side — we send the same path through).
		segment := strings.TrimSuffix(
			strings.TrimPrefix(r.URL.Path, "/api/v1/tenants/me/idp-providers/"),
			"/",
		)
		if segment == "" || strings.Contains(segment, "/") {
			writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_PROVIDER_TYPE",
				"path must be /api/v1/tenants/me/idp-providers/{providerType}")
			return
		}
		resp, _ := p.agg.DeleteMyIdpProvider(r.Context(), authCtxFromRequest(r), segment)
		writePhyllisResp(w, resp)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET or DELETE only on /api/v1/tenants/me/idp-providers[/{providerType}]")
	}
}

// handleCourses — POST /api/courses
func (p *PhyllisHandler) handleCourses(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	body := readBody(r)
	resp, _ := p.agg.CreateCourse(r.Context(), authCtxFromRequest(r), body)
	writePhyllisResp(w, resp)
}

// handleCatalog — GET /api/catalog?public=true
func (p *PhyllisHandler) handleCatalog(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	q := r.URL.Query()
	public := q.Get("public") == "true"
	resp, _ := p.agg.GetCatalog(r.Context(), catalogAuthFromRequest(r), public, q)
	writePhyllisResp(w, resp)
}

// handleEnrollments — POST /api/enrollments
func (p *PhyllisHandler) handleEnrollments(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	body := readBody(r)
	resp, _ := p.agg.CreateEnrollment(r.Context(), authCtxFromRequest(r), body)
	writePhyllisResp(w, resp)
}

// handleAtoms — GET /api/atoms/{id}, POST /api/atoms/{id}/feedback
func (p *PhyllisHandler) handleAtoms(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/atoms/")
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" {
		writeError(w, http.StatusNotFound, "GATEWAY_ATOM_PATH_REQUIRED",
			"path must include the atom id: /api/atoms/{id}")
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	atomID := parts[0]

	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		// WS-0b A16 — `?mode=author` requests the author-mode projection
		// (preserves correct_option_id when caller also holds the
		// author/instructor role). Default is learner mode.
		mode := r.URL.Query().Get("mode")
		resp, _ := p.agg.GetAtom(r.Context(), authCtxFromRequest(r), atomID, mode)
		writePhyllisResp(w, resp)
	case len(parts) == 1 && r.Method == http.MethodPatch:
		// PATCH /api/atoms/{id} — verbatim proxy to chora-creation per
		// a508f184 atom-CRUD gap close + Infra Cloud Armor priority 996
		// unblock at commit 1052db87.
		body := readBody(r)
		resp, _ := p.agg.PatchAtom(r.Context(), authCtxFromRequest(r), atomID, body)
		writePhyllisResp(w, resp)
	case len(parts) == 1 && r.Method == http.MethodDelete:
		// DELETE /api/atoms/{id} — verbatim proxy to chora-creation per
		// a508f184 atom-CRUD gap close + Infra Cloud Armor priority 996
		// unblock at commit 1052db87. chora-creation cascade-soft-deletes
		// the attached Question per ddd-enforcement #8 (commit 8e57a70b).
		resp, _ := p.agg.DeleteAtom(r.Context(), authCtxFromRequest(r), atomID)
		writePhyllisResp(w, resp)
	case len(parts) == 2 && parts[1] == "feedback" && r.Method == http.MethodPost:
		body := readBody(r)
		resp, _ := p.agg.SubmitFeedback(r.Context(), authCtxFromRequest(r), atomID, body)
		writePhyllisResp(w, resp)
	case len(parts) == 2 && parts[1] == "media" && r.Method == http.MethodPost:
		// POST /api/atoms/{id}/media — verbatim proxy to chora-creation
		// per E2E-BE-ATOM-GW / E2E-INFRA-ATOM-3 (ADR-156 Phase 1). Returns
		// a V4 signed URL JSON envelope; FE then PUTs the image directly
		// to GCS via the signed URL (NOT through the BFF). 502 until
		// E2E-BE-ATOM-1 chora-creation handler ships per FE acceptance.
		body := readBody(r)
		resp, _ := p.agg.MintAtomMediaSignedUrl(r.Context(), authCtxFromRequest(r), atomID, body)
		writePhyllisResp(w, resp)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"unsupported method or sub-path on /api/atoms/")
	}
}

// handleAIGenerate — POST /api/ai/generate
func (p *PhyllisHandler) handleAIGenerate(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	body := readBody(r)
	resp, _ := p.agg.GenerateAI(r.Context(), authCtxFromRequest(r), body)
	writePhyllisResp(w, resp)
}

// handleAIAssist — POST /api/atoms/ai-assist (M14.iter5 — Phyllis Step 4).
//
// Routes to chora-creation:/api/atoms/ai-assist which dispatches the
// QGen 6-step pipeline against the Vertex AI Agent Engine us-central1
// (ADR-148). chora-creation handler currently 503s
// qgen_engine_not_configured until 5.D wires the engine client.
func (p *PhyllisHandler) handleAIAssist(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	body := readBody(r)
	resp, _ := p.agg.AIAssist(r.Context(), authCtxFromRequest(r), body)
	writePhyllisResp(w, resp)
}

// handleAIAssistJobStatus — GET /api/atoms/ai-assist/{job_id}
// (OE-AI-ASSIST Step 5 — chora-gateway proxy for the qgen 2-agent crew
// async status surface per docs/m13/ack-oe-ai-assist-plan-2026-05-17.md).
//
// Verbatim passthrough to chora-creation's getAiAssistJob handler. FE
// polls every ~2s until terminal state (COMPLETED | REFUSED | FAILED).
// Tenant-scoped at the chora-creation side (RLS) — cross-tenant returns
// 404. Returns AiAssistJob envelope per OpenAPI creation-questions.yaml.
func (p *PhyllisHandler) handleAIAssistJobStatus(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	// Parse {job_id} segment from /api/atoms/ai-assist/{job_id}.
	rest := strings.TrimPrefix(r.URL.Path, "/api/atoms/ai-assist/")
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" || strings.Contains(rest, "/") {
		writeError(w, http.StatusNotFound, "GATEWAY_JOB_ID_REQUIRED",
			"job_id required in path: /api/atoms/ai-assist/{job_id} (single UUID, no further path)")
		return
	}
	resp, _ := p.agg.GetAIAssistJob(r.Context(), authCtxFromRequest(r), rest)
	writePhyllisResp(w, resp)
}

// handleCompanionMe — GET /api/companion/me
func (p *PhyllisHandler) handleCompanionMe(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	resp, _ := p.agg.GetCompanionMe(r.Context(), authCtxFromRequest(r))
	writePhyllisResp(w, resp)
}

// handleCompanionDailyDose: GET /api/companion/daily-dose
// [?growth_edge_id={uuid}][?goal_id={uuid}]
//
// Phase 2A — the optional growth_edge_id scopes the dose to a focused
// single-Growth-Edge practice session.
// WS-3: the optional goal_id scopes the dose to the goal the learner is
// browsing, so that goal's material leads the served cards. `goalId` is accepted
// as an alias because the caller is owned by another lane and a spelling
// mismatch would ship as a silently inert feature rather than a loud failure.
//
// Both are read off the inbound query and handed to the aggregator explicitly:
// GetDailyDose rebuilds the upstream URL, so anything not named is dropped.
func (p *PhyllisHandler) handleCompanionDailyDose(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	goalID := strings.TrimSpace(r.URL.Query().Get("goal_id"))
	if goalID == "" {
		goalID = strings.TrimSpace(r.URL.Query().Get("goalId"))
	}
	resp, _ := p.agg.GetDailyDose(r.Context(), authCtxFromRequest(r),
		r.URL.Query().Get("growth_edge_id"), goalID)
	writePhyllisResp(w, resp)
}

// handleKGClusters — GET + POST /api/v1/me/knowledge-graph/clusters
//
// Phyllis A+ KG-fog dashboard panel (FE unblocker per
// docs/m13/kg-fog-aplus-integration-backend-handoff-2026-05-13.md §7
// step 2). GET returns {data: {clusters: [], capRemaining, capMax}};
// POST creates a new cluster + triggers fog-gen.
func (p *PhyllisHandler) handleKGClusters(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		resp, _ := p.agg.GetKGClusters(r.Context(), authCtxFromRequest(r))
		writePhyllisResp(w, resp)
	case http.MethodPost:
		body := readBody(r)
		resp, _ := p.agg.CreateKGCluster(r.Context(), authCtxFromRequest(r), body)
		writePhyllisResp(w, resp)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"method "+r.Method+" not allowed; want GET or POST")
	}
}

// NewRouterWithPhyllisGraphQLAndChoraSession wires the BFF mux with
// production-ready Chora session JWT validation gating /api/* + /graphql
// routes. The /bff/* surface aggregation routes keep the legacy
// session-based auth (those use the in-process /api/auth/session token,
// NOT the Chora session JWT). Health/version/readyz routes remain public.
//
// When validator is nil this delegates to NewRouterWithPhyllisAndGraphQL so
// callers in dev/test environments can opt out via env-driven config.
func NewRouterWithPhyllisGraphQLAndChoraSession(
	routes route.Repository,
	sessions session.Repository,
	up upstream.Client,
	agg *phyllis.Aggregator,
	graphqlHandler http.Handler,
	validator *chorasession.Validator,
) http.Handler {
	base := NewRouterWithPhyllisAndGraphQL(routes, sessions, up, agg, graphqlHandler)
	if validator == nil {
		return base
	}
	// Gate /api/* (Phyllis aggregator) + /graphql with the session JWT
	// validator. /api/auth/session is excluded — it's the legacy
	// session-mint endpoint that EXCHANGES credentials for an opaque
	// session, so it can't itself require a validated JWT.
	// /api/v1/auth/session/mint is also excluded for the same reason:
	// it's the session exchange that ESTABLISHES the session JWT.
	gatedPrefixes := []string{
		"/api/me",
		"/api/tenants/",
		"/api/courses",
		"/api/catalog",
		"/api/enrollments",
		"/api/atoms",
		"/api/ai/",
		"/api/companion/",
		// ADR-254 D9 alias, drop after the SPA cut (WP-X go)
		"/api/familiar/",
		"/graphql",
	}
	return choraSessionOnPathPrefix(validator, gatedPrefixes, base)
}
