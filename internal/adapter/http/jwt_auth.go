// jwt_auth.go — production-ready Chora session JWT validation middleware
// for chora-gateway.
//
// Per S3.6 spec, the BFF is the trust boundary for all inbound user traffic.
// The /api/* trust boundary validates **Chora session JWTs minted by
// POST /api/v1/auth/session/mint**.
//
// Flow:
//
//  1. chora-web POSTs {username, password} to /api/v1/auth/session/mint; the
//     gateway verifies them at chora-identity and signs a Chora session JWT
//     (HS256; claims iss, aud, sub, gcid, tenant_id, email, roles, iat, exp).
//  2. chora-web stores the session JWT and sends it as
//     `Authorization: Bearer <session>` on every /api/* call.
//  3. **This middleware** validates that Chora session JWT via
//     chorasession.Validator (HS256, signer from CHORA_SESSION_SIGNER) and
//     stamps servicemesh.MeshClaims onto the request context.
//
// On any rejection writes 401 + WWW-Authenticate. Health/version paths bypass
// this middleware (wired upstream of the path matcher).
//
// Composition: chained AFTER traceContext + logging, BEFORE handler. Public
// paths (`/healthz`, `/readyz`, `/version`, `/`) MUST bypass via the upstream
// router (the middleware itself is not path-aware).
package httpadapter

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/apollo-chora/chora-common/auth/chorasession"
	"github.com/apollo-chora/chora-common/auth/servicemesh"
)

// meshClaimsCtxKey is a private type for stashing servicemesh.MeshClaims in
// the request context. Distinct from the existing ctxKey enum so we don't
// conflict with the in-process session-based auth path.
type meshClaimsCtxKey struct{}

// chsClaimsCtxKey stashes the raw chorasession.Claims so handlers that need
// email / sub / role_summary can read them without re-parsing.
type chsClaimsCtxKey struct{}

// MeshClaimsFromContext retrieves the propagated MeshClaims (gcid, tenant_id,
// role_summary) from the request context. Returns ok=false when claims are
// absent.
func MeshClaimsFromContext(ctx context.Context) (*servicemesh.MeshClaims, bool) {
	c, ok := ctx.Value(meshClaimsCtxKey{}).(*servicemesh.MeshClaims)
	return c, ok && c != nil
}

// ChoraSessionClaimsFromContext retrieves the validated Chora session claims
// (gcid, tenant_id, email, sub, role_summary, etc.) from the request context.
func ChoraSessionClaimsFromContext(ctx context.Context) (*chorasession.Claims, bool) {
	c, ok := ctx.Value(chsClaimsCtxKey{}).(*chorasession.Claims)
	return c, ok && c != nil
}

// withMeshClaims attaches MeshClaims to the supplied context.
//
// It stashes them under BOTH this package's private key AND the shared
// servicemesh key. The shared one is what lets consumers OUTSIDE httpadapter —
// notably the GraphQL resolvers, which cannot import this package without an
// import cycle — read the caller's validated roles and propagate them as
// x-mesh-user-roles. Without it a resolver's upstream.AuthCtx carries no Roles,
// and every fail-closed downstream role gate denies 100% of GraphQL-originated
// calls (the CHO-2148 class of bug).
func withMeshClaims(ctx context.Context, c *servicemesh.MeshClaims) context.Context {
	ctx = servicemesh.WithClaims(ctx, c)
	return context.WithValue(ctx, meshClaimsCtxKey{}, c)
}

// withChsClaims attaches the raw Chora session claims.
func withChsClaims(ctx context.Context, c *chorasession.Claims) context.Context {
	return context.WithValue(ctx, chsClaimsCtxKey{}, c)
}

// errMissingCustomClaim is returned when a JWT has a valid signature but
// the gcid identity claim is absent. Post-CHO-1653 (Phase 5 hot-fix) the
// tenant_id claim MAY be empty for bootstrap-mode JWTs — the
// chorasession.Validator accepts that shape and route-level handlers
// enforce non-empty tenant_id where they need it.
var errMissingCustomClaim = errors.New("chorasession: missing custom claim (gcid)")

// RequireChoraSessionJWT returns an HTTP middleware that validates the
// inbound Chora session JWT and attaches the parsed claims (both raw +
// mesh-normalised) to the request context. Mount upstream of the BFF mux
// but downstream of the public-paths router to skip /healthz etc.
//
// On success: chains to next; chorasession.Claims and servicemesh.MeshClaims
// both available via ChoraSessionClaimsFromContext / MeshClaimsFromContext.
// On failure: 401 with WWW-Authenticate=Bearer; chain short-circuited.
func RequireChoraSessionJWT(v *chorasession.Validator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok, ok := extractSessionToken(r)
			if !ok {
				writeJWT401(w, "missing_bearer", "Authorization: Bearer required")
				return
			}
			claims, err := v.Validate(tok)
			if err != nil {
				writeJWT401(w, "invalid_token", err.Error())
				return
			}
			// Defence in depth: chorasession.Validator already rejects empty
			// gcid (returns ErrMissingClaim) so the validator surface keeps
			// the identity boundary single-sourced. We still check here so
			// a future validator-config bug can't silently elevate a non-
			// Chora token. tenant_id is intentionally NOT checked — Phase 5
			// bootstrap-mode JWTs (CHO-1648) carry tenant_id="" and the
			// validator accepts that shape per CHO-1653. Tenant-scoped
			// routes that require an active tenant enforce it themselves
			// downstream.
			if strings.TrimSpace(claims.GCID) == "" {
				writeJWT401(w, "invalid_token", errMissingCustomClaim.Error())
				return
			}
			// Normalise into mesh claims for downstream propagation.
			//
			// Bucket 4 (2026-05-14 multi-tenant identity): Roles []string
			// from the JWT is forwarded to downstream services via the
			// HeaderUserRoles (X-Mesh-User-Roles) mTLS-bound header. The
			// legacy chora-role-summary map[string]any stays nil here
			// because the session JWT no longer carries per-course role
			// nuance — role assignment is now flat per (gcid, tenant_id).
			mc := &servicemesh.MeshClaims{
				GCID:     claims.GCID,
				TenantID: claims.TenantID,
				Roles:    append([]string(nil), claims.Roles...),
			}
			ctx := r.Context()
			ctx = withChsClaims(ctx, claims)
			ctx = withMeshClaims(ctx, mc)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// writeJWT401 emits a 401 with a uniform WWW-Authenticate + JSON envelope.
// Body matches the existing writeError envelope so SPA error handling stays
// uniform across BFF gates.
func writeJWT401(w http.ResponseWriter, errCode, message string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="chora", error="`+errCode+`"`)
	writeError(w, http.StatusUnauthorized, "GATEWAY_UNAUTHENTICATED", message)
}

// extractSessionToken pulls the Chora session JWT from a request. The
// Authorization: Bearer header is the canonical transport for every REST
// route. For WebSocket-UPGRADE requests ONLY it additionally accepts the
// token via the `access_token` query param: browsers cannot set the
// Authorization header on a native WebSocket handshake (the WebSocket
// constructor takes only a URL + subprotocols per RFC 6455). Restricting the
// query-param path to upgrades keeps REST tokens out of URLs / access logs.
//
// ADR-168 R+ classroom realtime — the /api/v1/live-quizzes/{id}/ws and
// /api/v1/live-polls/{id}/ws fan-out endpoints are the only WS upgrades the
// gateway gates today; this is the browser-compatible auth path for them.
func extractSessionToken(r *http.Request) (string, bool) {
	if tok, ok := extractBearerToken(r.Header.Get("Authorization")); ok {
		return tok, true
	}
	if isWebSocketUpgrade(r) {
		if tok := strings.TrimSpace(r.URL.Query().Get("access_token")); tok != "" {
			return tok, true
		}
	}
	return "", false
}

// extractBearerToken pulls the bearer token from an Authorization header
// value. Returns ok=false on missing header, wrong scheme, or empty token.
func extractBearerToken(authz string) (string, bool) {
	authz = strings.TrimSpace(authz)
	if authz == "" {
		return "", false
	}
	parts := strings.SplitN(authz, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	tok := strings.TrimSpace(parts[1])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// choraSessionOnPathPrefix wraps next with Chora session JWT validation only
// for the supplied path prefixes. Other routes (e.g. /healthz, /version,
// /api/v1/auth/session/mint) pass through to next without authentication.
// This composes the JWT auth gate with the existing route table without
// breaking unauthenticated public paths or the legacy /bff/* surface
// aggregation routes (those keep their session-based auth via
// authMiddleware).
func choraSessionOnPathPrefix(v *chorasession.Validator, prefixes []string, next http.Handler) http.Handler {
	gated := RequireChoraSessionJWT(v)(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, p := range prefixes {
			if strings.HasPrefix(r.URL.Path, p) {
				gated.ServeHTTP(w, r)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// DefaultJWTGatedPrefixes is the canonical list of path prefixes that
// require Chora session JWT validation. /api/auth/session is intentionally
// absent (legacy session-mint endpoint), as is
// /api/v1/auth/session/mint (the session exchange that ESTABLISHES the
// session JWT). Health, version, readyz are public.
//
// /api/catalog is intentionally ABSENT — it is a public cross-tenant
// discovery endpoint (Phyllis demo step 5). The inner route table already
// advertises it as auth_mode: public; removing it from the prefix gate
// honours that declaration and allows anonymous browse without JWT.
// See D0.3 in docs/m14/RESUME_PROMPT_PHYLLIS_BACKEND_DEMO_READY_2026-05-14.md.
var DefaultJWTGatedPrefixes = []string{
	// Phase A4 (ADR-181 D2, CHO-1718) — passkey REGISTRATION ceremonies
	// require an authenticated session (a passkey is added to an EXISTING
	// account post-first-login): RequireChoraSessionJWT must stamp the
	// claims so webauthn_handler.go can enforce body-gcid == session-gcid.
	// The login/{begin,finish} siblings are intentionally ABSENT — like
	// /api/v1/auth/session/mint they ESTABLISH the session.
	"/api/v1/auth/webauthn/register/",
	// W0 (rt) realtime SSE channel (ADR-183, CHO-1661) — the stream-ticket
	// minter reads MeshClaims to bind the ticket to the session identity;
	// without this entry the bridge sees a claims-less request and 401s
	// every mint (the CHO-1652 class of gap). The /stream sibling is
	// intentionally ABSENT: it bypasses the gateway entirely via its own
	// 3600s HTTPRoute and authenticates with the ticket, not the Bearer.
	RealtimeTicketPath,
	"/api/me",
	"/api/tenants/",
	// CHO-1652 — H+ Setup-Tenant Phase 3 BFF route. Without this entry
	// RequireChoraSessionJWT skips the request, MeshClaims is never
	// stamped on the context, authCtxFromRequest returns
	// AuthCtx{GCID: ""}, and phyllis.call() forwards to chora-tenancy
	// without the `gcid` header. chora-tenancy then rejects with
	// 401 gateway_unauthenticated ("X-GCID header required"). The route
	// is registered on the mux in phyllis_handler.go (Phase 3 CHO-1632)
	// but was missed from this list — the bootstrap endpoint is the
	// first JWT-gated route under /api/v1/tenants/, all other entries
	// under that prefix are public (e.g., admin reads, marketplace).
	"/api/v1/tenants/bootstrap",
	// CHO-1655 — Setup Wizard Phase A. /me/branding is JWT-gated so
	// RequireChoraSessionJWT stamps MeshClaims; phyllis.call() then
	// forwards `X-Tenant-Id` to chora-tenancy's PATCH /me/branding
	// handler (which 401s without it).
	"/api/v1/tenants/me/branding",
	// CHO-1664 — Setup Wizard Phase B. /me/addons is JWT-gated for the
	// same reason as /me/branding above: phyllis.call() needs MeshClaims
	// stamped so it can forward X-Tenant-Id + gcid headers to
	// chora-tenancy's handler.
	"/api/v1/tenants/me/addons",
	// CHO-1682 — Setup Wizard step 4 Apply aggregator. /tenants/setup
	// fans out to chora-identity's POST /me/idp-providers + chora-tenancy's
	// PATCH /me/finish-setup; both backends require X-Tenant-Id + gcid
	// stamped after JWT verify by the BFF.
	"/api/v1/tenants/setup",
	// CHO-1692 — Setup Wizard re-entry hydration. /api/v1/tenants/me
	// proxies to chora-tenancy's pg-backed MeTenantHandler which 401s
	// without X-Tenant-Id stamped from the validated JWT.
	"/api/v1/tenants/me",
	// CHO-1698 STITCH-H-ADD-1 — H+ Add-on Lifecycle dashboard alias.
	// GET /api/v1/admin/tenants/me/addons proxies to chora-tenancy's
	// parametric admin endpoint with tenantId stamped from validated JWT.
	"/api/v1/admin/tenants/me/addons",
	// H+ Billing (CHO-1759 followup) — /h/billing surfaces tenant-scoped
	// Stripe Invoice list + Customer Portal session mint. Both subresources
	// are JWT-gated so the aggregator can rewrite the `me` alias to the
	// resolved TenantID from MeshClaims.
	"/api/v1/admin/tenants/me/invoices",
	"/api/v1/admin/tenants/me/billing-portal",
	"/api/courses",
	"/api/enrollments",
	"/api/atoms",
	// CHO-2261 — GET /api/v1/atoms[/{id}] atom read (A+ Daily Dose AI-pick
	// resolution). JWT-gated so RequireChoraSessionJWT stamps X-Tenant-Id +
	// gcid; chora-creation getAtom is RLS/tenant-scoped (tenantFromContext) and
	// 404s an empty tenant. The /api/atoms entry above does NOT prefix-cover the
	// /v1/ variant — it needs its own entry. Prefix covers collection + /{id}.
	"/api/v1/atoms",
	// CHO-2276 — topic-tree Sub-phase B. JWT-gated so RequireChoraSessionJWT
	// stamps X-Tenant-Id + gcid (RLS) AND the typed Roles → x-mesh-user-roles
	// that chora-creation's topic write gate (requireAdmin, admin/owner) reads
	// fail-closed. Reads are learner-safe; the write admin gate is DOWNSTREAM.
	// The single prefix covers the collection + /{id} + /{id}/{move,atoms}.
	"/api/v1/topics",
	// C+ ChoraCircle post-create (2026-06-04) — POST /api/posts → chora-sharing
	// Moderation gate. JWT-gated so RequireChoraSessionJWT stamps MeshClaims;
	// the gatewayproxy bridge then forwards X-Tenant-Id + gcid to chora-sharing
	// (which 4xx MISSING_CONTEXT without them). Mirrors the /api/atoms entry.
	"/api/posts",
	"/api/ai/",
	"/api/companion/",
	// Debt #41 close (2026-05-16) — bare `/companion/` prefix alias for
	// the daily-dose-style routes (CR v4 rehearsal row 12). The gateway
	// claim at /companion/daily-dose (phyllis_handler.go) delegates to the
	// same aggregator as /api/companion/daily-dose; this prefix ensures
	// RequireChoraSessionJWT validates the Bearer + stamps MeshClaims
	// before the alias handler runs, so the BFF outbound call still
	// stamps the gcid + X-Tenant-Id headers chora-consumption requires.
	"/companion/",
	// ADR-254 D9 alias, drop after the SPA cut (WP-X go): the pre-rename
	// Phyllis prefixes stay gated so the alias handlers run behind the same
	// session validation + mesh-claim stamping as the companion routes.
	"/api/familiar/",
	"/familiar/",
	// PROD-D (m14.iter5g) Companion Growth + Egg purchase BFF routes per
	// the PROD-D FE growth handoff (2026-05-13) §2.1. Bearer JWT is
	// enforced on every route, including the tenant-scoped catalog/odds reads.
	"/api/v1/me/companions",
	"/api/v1/companion-eggs",
	// ADR-254 D9 alias, drop after the SPA cut (WP-X go)
	"/api/v1/me/familiars",
	"/api/v1/familiar-eggs",
	// CHO-1719 (ADR-181 D5, 2026-06-12) — account-closure saga trigger
	// routes (closure_handler.go): /api/v1/me/account/close[,/cancel] +
	// /api/v1/me/account/closure. JWT-gated because the self-close routes
	// derive gcid + tenant_id EXCLUSIVELY from the validated session claims
	// (never the body) and the "own saga only" gate compares the saga's
	// gcid against the session gcid. The operator route
	// /api/v1/admin/accounts/{gcid}/close is already covered by the
	// /api/v1/admin/ prefix below (B6.1).
	"/api/v1/me/account",
	// A17 (2026-05-15) — mana wallet + top-up BFF proxy → chora-identity per
	// docs/m13/handoff-fe-to-be-service-2026-05-14.md §A17. Authed because
	// the wallet is user-scoped (gcid) and the top-up POST charges the
	// user's Stripe payment method. Prefix covers both /api/v1/me/mana (GET)
	// and /api/v1/me/mana/topup (POST).
	"/api/v1/me/mana",
	// ADR-205 / CHO-1940 (Wave B4) — learner-scoped contextual Transaction
	// History. Authed so RequireChoraSessionJWT stamps MeshClaims (gcid +
	// tenant_id) before transaction_history_handler.go binds the LEARNER
	// scope from them. Prefix covers /api/v1/me/transactions[/summary|/{id}].
	// (The /admin/transactions sibling is already covered by the blanket
	// "/api/v1/admin/" prefix below.)
	"/api/v1/me/transactions",
	// CJ#2 (2026-05-26) — FE post-Stripe-checkout polling. Authed so
	// RequireChoraSessionJWT validates the Bearer + stamps MeshClaims
	// (tenant_id + gcid) into request context; gatewayProxyAuthFromRequest
	// reads them, and gatewayproxy.call() stamps X-Tenant-Id + lowercase
	// `gcid` on the outbound chora-delivery /v1/me/enrolments call so
	// the downstream tenantRequired middleware + handler-side gcid header
	// read both succeed. Without this entry the JWT middleware skips the
	// path, MeshClaims is empty, and chora-delivery 400s with "X-Tenant-Id
	// header required".
	"/api/v1/me/enrolments",
	// In-app notifications BFF proxy → chora-notifications. SS dress-rehearsal
	// v2 paydown — every notification list/enqueue/mark-read must be authed
	// (user-scoped + tenant-scoped) so unauthenticated callers cannot read
	// other users' notifications.
	"/api/v1/notifications",
	// Per-user Knowledge-Graph hexagonal exploration (D1.4) — BFF proxy →
	// chora-consumption fog_handler (ADR-143 §7). Authed because the
	// exploration is per-user (each learner owns disconnected MapCluster
	// aggregates with per-user fog); an unauthenticated caller has no KG.
	"/api/v1/consumption/kg/explore/",
	// Stripe finalization (Iter G.5) — publishable key + checkout URLs.
	// Authed because callers should be logged-in learners; anonymous
	// readers don't need to know the publishable key.
	"/api/v1/stripe-config",
	// A6 (2026-05-14) — the missing non-auth /api/* gateway routes.
	// FE token-probed each and found GATEWAY_ROUTE_NOT_FOUND — they were
	// not routed at the gateway at all. The gatewayproxy bridge
	// (WithGatewayProxy) proxies each to its real downstream path. All
	// authed: feature-flags is tenant-scoped; notifications is
	// user/tenant-scoped. The downstreams (chora-tenancy /
	// chora-notifications) require the X-Tenant-Id + lowercase gcid context
	// the validated ChoraSession JWT supplies via mesh claims.
	//
	// NOTE: the rest of the gatewayproxy bridge's prefixes are ALREADY
	// covered by the entries above — /api/tenants/ (line ~196),
	// /api/courses (prefix-covers /api/courses/{id}), /api/me
	// (prefix-covers /api/me/companions + /api/me/knowledge-graph/clusters).
	// Only /api/feature-flags + /api/notifications (the non-/v1 form) are
	// genuinely new prefixes. See gatewayproxy_handler.go + handoff §A6.
	"/api/feature-flags",
	"/api/notifications",
	// B6.1 (2026-05-16) — searchTenantMembers picker. Authed because the
	// search is tenant-scoped (RLS) and the role gate (TRAINING_ADMIN /
	// TENANT_ADMIN) reads the validated mesh claims. /api/v1/admin/* prefix
	// covers this leaf + any future admin routes the gateway proxies.
	"/api/v1/admin/",
	// Debt #4 / A6 (2026-05-16) — instructor-roster surface. Authed because
	// the by-instructor read is identity-bearing (the downstream chora-delivery
	// handler enforces self-or-instructor-or-admin via the validated mesh
	// claims). Prefix-covers /api/v1/instructors/{instructor_gcid}/courses.
	"/api/v1/instructors/",
	// Lane D (#60, 2026-05-16) — Tier 1 routes Lanes A/B/C just shipped.
	// All three subtrees are JWT-gated because the downstream handlers
	// require tenant + caller GCID for cohort + role enforcement.
	// /api/atoms/questions/search (Lane C) is already covered by the
	// existing /api/atoms prefix above.
	"/api/v1/test-sets",
	"/api/v1/assessments",
	"/api/v1/me/assessments",
	// E2E-BE-CJ2 (2026-05-16) — Customer Journey #2 course authoring +
	// review + release. JWT-gated because the downstream chora-delivery
	// handler enforces instructor / training-admin RBAC via the mesh-
	// claim x-mesh-user-roles + needs gcid for author-self gate.
	"/api/v1/courses",
	// ADR-164 Stage B (2026-05-24) — chora-payments REST proxy.
	// /api/v1/checkout/{course|application|companion-egg|mana-topup|subscription}
	// mints Stripe Checkout Sessions via the chora-payments gRPC PaymentService.
	// JWT-gated so RequireChoraSessionJWT stamps the validated tenant_id +
	// learner_gcid mesh claims before the payments_handler reads them.
	"/api/v1/checkout",
	// H+ tx-history (Phase 2 Agent A2, 2026-05-26) — chora-payments admin
	// REST surface for the H+ Transaction History page. Strictly speaking
	// the existing /api/v1/admin/ prefix (B6.1) already JWT-gates this, but
	// the explicit per-feature prefix entry keeps the audit trail readable
	// and survives a future re-narrowing of /api/v1/admin/. The downstream
	// chora-payments admin_handler reads X-Chora-Role + x-mesh-user-roles
	// stamped by gatewayproxy.callWithRoleHeader to enforce the
	// PLATFORM_OPERATOR / TENANT_ADMIN / OWNER / AUDITOR role gate.
	//
	// PLATFORM_OPERATOR is a NEW role per ADR-165 (PROPOSED). The session JWT
	// validator already passes claims.Roles []string through verbatim — no
	// enum check exists in this middleware so the new role name simply flows
	// downstream where chora-payments + chora-identity recognise it.
	"/api/v1/admin/payments",
	"/v1/feed",
	"/v1/me/",
	"/v1/atoms/",
	"/v1/posts/",
	"/v1/connections",
	"/v1/leaderboard",
	"/v1/duels",
	"/v1/discovery/",
	"/graphql",
	// /api/v1/graphql is the FE's GraphQL path (aliased to the same handler in
	// phyllis_handler.go); gate it like /graphql so learner-facing reads carry
	// validated Chora session claims.
	"/api/v1/graphql",
	// Phase C (2026-05-26) — O+ Observability+ surface. The 5 /bff/oplus/*
	// routes (dashboard / dimensions / agents / governance / a2a) need the
	// chora-session JWT validator to stamp ChoraSessionClaims onto the
	// request context BEFORE the outer middleware.AuditorGate inspects the
	// `auditor` role claim. Without this prefix, AuditorGate would read empty
	// roles and reject every OPlus request with 403 auditor_role_required.
	// Per imda-governance-4-dimensions three-audience explainability model.
	"/bff/oplus/",
	// R+ Rhythm+ delivery proxy bridge (2026-05-26) — verbatim passthrough
	// to chora-delivery for resource groups whose downstream handler EXISTS
	// today (bookings + certifications + campus). /v1/me/applications is
	// covered by the existing /v1/me/ prefix above; the three entries here
	// are the genuinely new ones the bridge needs. See
	// rplus_delivery_proxy_handler.go + the R+ build-out plan at
	// ~/.claude/plans/jaunty-strolling-key.md M1.
	//
	// JWT-gated because the downstream chora-delivery handlers all use
	// tenantRequired middleware that reads X-Tenant-Id from validated mesh
	// claims; without this gate, an unauthenticated caller would reach the
	// downstream and 4xx with "X-Tenant-Id header required" — confusing
	// real auth failures with missing-context errors. Gate here = explicit
	// 401 at the gateway.
	"/api/bookings",
	"/api/certifications",
	// R+ certifications + applications admin queues (2026-06-02) — the FE
	// calls the /api/v1/* forms (certifications.service.ts + applications-
	// admin), but only the legacy no-v1 /api/certifications was gated, so the
	// gateway never stamped X-Tenant-Id and chora-delivery 400'd "X-Tenant-Id
	// header required". list #1 (proxy-prefix) + list #3 (delivery mesh authz)
	// already cover these; this closes the list #2 (gateway JWT-gate) gap.
	// Subtree prefix covers the /{id} detail routes. /api/v1/me/applications
	// is unaffected (matched by the /api/v1/me/ leaves above, not this prefix).
	"/api/v1/certifications",
	"/api/v1/applications",
	"/v1/campus",
	// R+ scheduling week-view + create/reschedule/cancel (CHO-1626, 2026-06-01)
	// — proxied to chora-delivery (RplusDeliveryProxyPathPrefixes) but
	// previously UNGATED, so the gateway never stamped the X-Tenant-Id/gcid
	// mesh claims the downstream tenantRequired needs (the delivery mesh authz
	// also denied it — fixed in chora-infra/.../chora-delivery/
	// authz-allow-gateway.yaml). Same gap as the CHO-1580 verticals below.
	// REST only (no WS). Subtree prefix covers /v1/scheduling/classes[/{id}/...].
	"/v1/scheduling",
	// Open-course flow / ask (b) (2026-05-26) — chora-consumption course-
	// bound LearningPath read for the A+ course-learn page. JWT-gated so
	// RequireChoraSessionJWT stamps tenant_id + gcid mesh claims before the
	// gatewayproxy bridge forwards to chora-consumption (its me_handlers
	// reads X-Tenant-Id + lowercase gcid headers off the validated mesh
	// claims). Static prefix; the WithGatewayProxy bridge owns the leaf —
	// no /api/v1/me prefix conflict because the existing /api/me prefix
	// (line above) is /api/me (no /v1/) and the existing /api/v1/me/...
	// entries claim specific leaves (/mana, /enrolments) only.
	"/api/v1/me/learning-paths",
	// CHO-1612 (2026-06-01) — A+ open-course heterogeneous curriculum read
	// /api/v1/me/courses/{id}/content → chora-consumption. JWT-gated so
	// RequireChoraSessionJWT stamps tenant_id + gcid mesh claims;
	// gatewayProxyAuthFromRequest reads them and gatewayproxy.call() forwards
	// X-Tenant-Id + lowercase gcid to chora-consumption's me_handlers
	// (extRequireContext 400s MISSING_CONTEXT without them). The route was in
	// GatewayProxyPathPrefixes + bridge-owned but MISSING here — the gate
	// skipped it, so mesh claims were empty. Trailing-slash subtree prefix.
	"/api/v1/me/courses/",
	// Epic-1b W8 — A+ Growth Edges read + upload, proxied to chora-consumption.
	// JWT-gated so RequireChoraSessionJWT stamps tenant_id + gcid mesh claims
	// before the gatewayproxy bridge forwards X-Tenant-Id + lowercase gcid
	// (chora-consumption extRequireContext 400s MISSING_CONTEXT without them).
	// This single prefix covers the list leaf + /{id} + /uploads[/{id}] subtree.
	"/api/v1/me/growth-edges",
	// E2 (2026-09-02): H+ instance readiness. JWT-gated so
	// RequireChoraSessionJWT stamps the mesh claims the handler's operator /
	// tenant-admin gate reads: sessionRoles reads the VALIDATED claims off the
	// context, never a caller-supplied header, so without this entry the gate
	// sees no roles and refuses every legitimate caller.
	"/api/v1/admin/readiness",
	// B6 item 3 (2026-09-02): the learner's remaining practice taps, proxied
	// to chora-consumption. JWT-gated for the same reason as its siblings: the
	// budget is per-learner and the downstream handler derives the identity
	// from the stamped mesh claims, so an ungated request would arrive with no
	// gcid and 400 MISSING_CONTEXT. Claiming a route in
	// GatewayProxyPathPrefixes does NOT gate it; these are two separate lists,
	// and a route in one but not the other is reachable unauthenticated.
	"/api/v1/me/practice-budget",
	// ADR-204 (2026-06-29) — A+ learner-owned Goal read/create/update, proxied to
	// chora-consumption. JWT-gated so RequireChoraSessionJWT stamps tenant_id +
	// gcid mesh claims before the gatewayproxy bridge forwards X-Tenant-Id +
	// lowercase gcid (chora-consumption extRequireContext 400s MISSING_CONTEXT
	// without them; the goals_handler also derives the "own goal only" gate + the
	// primaryLens from the session gcid). This single prefix covers the list/create
	// leaf + the /{id} subtree (HasPrefix). Mirrors the Growth Edges sibling.
	"/api/v1/me/goals",
	// CHO-2040 R8-6 (2026-07-05) — A+ Virgin Proofing Test list route, proxied to
	// chora-consumption. JWT-gated so RequireChoraSessionJWT stamps tenant_id +
	// gcid mesh claims before the gatewayproxy bridge forwards X-Tenant-Id +
	// lowercase gcid — WITHOUT this entry the route is proxied but never gated, so
	// chora-consumption's extRequireContext 400s MISSING_CONTEXT. Mirrors the
	// Goals / Growth Edges siblings (guarded by TestDefaultJWTGatedPrefixes_CoversProofingTests).
	"/api/v1/me/proofing-tests",
	// WS-B (2026-07-02) — My Knowledge Atlas map read-model (map = Goal, ADR-214
	// D1), proxied to chora-consumption. JWT-gated so RequireChoraSessionJWT stamps
	// tenant_id + gcid mesh claims before the gatewayproxy bridge forwards
	// X-Tenant-Id + lowercase gcid — WITHOUT this entry the maps route was proxied
	// but never JWT-gated, so chora-consumption maps_handler extRequireContext 400s
	// MISSING_CONTEXT ("X-Tenant-Id required"). This single prefix covers the list
	// leaf + the /{goalId}/graph subtree (HasPrefix). Mirrors the Goals sibling.
	"/api/v1/me/maps",
	// CHO-2045 (ADR-224) — A+ learner Dose Preferences GET/PUT, proxied to
	// chora-consumption. JWT-gated so RequireChoraSessionJWT stamps tenant_id +
	// gcid mesh claims before the gatewayproxy bridge forwards X-Tenant-Id +
	// lowercase gcid (chora-consumption requireContext 401s MISSING_CONTEXT
	// without them). Single prefix (leaf). Mirrors the Goals sibling.
	"/api/v1/me/dose-preferences",
	// ADR-225 — AI Transparency Notice (learner-self), proxied to
	// chora-governance. JWT-gated so RequireChoraSessionJWT stamps tenant_id +
	// lowercase gcid mesh claims (chora-governance tenantContext 400s without
	// them). Single prefix covers the GET leaf + the POST /acknowledge leaf.
	"/api/v1/me/ai-transparency",
	// Learner-sovereign Discovery Graph (2026-07-01) — A+ per-user concept
	// graph read/mutate, proxied to chora-consumption. JWT-gated so
	// RequireChoraSessionJWT stamps tenant_id + gcid mesh claims before the
	// gatewayproxy bridge forwards X-Tenant-Id + lowercase gcid (chora-consumption
	// extRequireContext 400s MISSING_CONTEXT without them; the concept-graph
	// handler also scopes the per-user graph off the session gcid). This single
	// prefix covers the list leaf + the /reroot + /concepts[/{id}] + /edges[/{id}]
	// subtree (HasPrefix). Mirrors the ADR-204 Goals sibling above.
	"/api/v1/me/concept-graph",
	// ADR-168 R+ classroom realtime (CHO-1616, 2026-06-01) — the live-quizzes
	// / live-polls / classroom-sessions subtrees carry BOTH REST CRUD and the
	// WebSocket fan-out (/{id}/ws). Both MUST be JWT-gated: the REST handlers
	// downstream require X-Tenant-Id + gcid mesh claims, and the WS handlers
	// tenant-scope the broker subscribe against the validated claims. Before
	// these entries the subtrees were UNGATED — the WS fan-out (snapshot
	// carries tenant_id + instructor_gcid + the learner leaderboard) was
	// reachable by any caller who knew a session UUID, ACROSS tenant
	// boundaries. Browsers reach the /ws upgrade with the JWT in the
	// access_token query param (RequireChoraSessionJWT's extractSessionToken
	// honours it for WS upgrades only). The WS-hijack proxy strips
	// access_token before forwarding the upgrade to chora-delivery.
	"/api/v1/live-quizzes",
	"/api/v1/live-polls",
	"/api/v1/classroom-sessions",
	// R+ Stage C-lite (CHO-1580) training-admin verticals — proxied to
	// chora-delivery but previously UNGATED, so the gateway never stamped the
	// X-Tenant-Id/gcid mesh claims the downstream tenantRequired needs (the
	// delivery mesh authz also denied them — fixed in
	// chora-infra/k8s/services/chora-delivery/authz-allow-gateway.yaml). REST
	// only (no WS), so no access_token query-param path. Gating turns an
	// unauthenticated call into a clean 401 instead of a downstream mesh 403.
	"/api/v1/surveys",
	"/api/v1/wbl-placements",
	"/api/v1/rosters",
	"/api/v1/exams",
	// R+ four-mode (ADR-190) — Offering CRUD (W1, CHO-1849) + universal-finder
	// search (W2.A, CHO-1850). JWT-gated so RequireChoraSessionJWT stamps
	// tenant_id + gcid mesh claims before the bridge forwards to chora-delivery
	// (else its tenantRequired 400s). W1 added the proxy prefix but omitted this
	// gate — TestDefaultJWTGatedPrefixes_CoversRplusDeliveryProxy caught it.
	"/api/v1/offerings",
	"/api/v1/search/offerings",
	// R+ four-mode SHORT (ADR-237, CHO-2191) — durable Room catalogue; JWT-gated
	// so RequireChoraSessionJWT stamps tenant_id + gcid before the bridge
	// forwards to chora-delivery (else its tenantRequired 400s).
	"/api/v1/rooms",
	// R+ Phase-2 W7 (CHO-2074) — A+ learner course-only module-progress read.
	// JWT-gated so RequireChoraSessionJWT stamps tenant_id + gcid before the
	// bridge forwards to chora-delivery (else its tenantRequired 400s).
	"/api/v1/me/module-progress",
	"/api/v1/project-groups",
	"/api/v1/skillsfutures-claims",
	// CHO-1618 (2026-06-01) — A+ Personal Collections proxied verbatim to
	// chora-creation. JWT-gated so RequireChoraSessionJWT stamps tenant_id +
	// gcid mesh claims before the gatewayproxy bridge forwards them; the
	// downstream collection_handler's tenantContext middleware reads
	// X-Tenant-Id + lowercase gcid off the validated claims (without the gate
	// every route 4xxs with missing-context). The /api/v1/collections prefix
	// covers the root POST + the parametric /{id}[/atoms[/{atomId}]] subtree;
	// /api/v1/me/collections is the distinct caller-scoped list leaf (no
	// broad /api/v1/me prefix exists, so it MUST be listed explicitly).
	"/api/v1/collections",
	"/api/v1/me/collections",
	// W3.B.1 (2026-06-28) — R+ Question Banks proxied verbatim to chora-creation.
	// JWT-gated so RequireChoraSessionJWT stamps tenant_id + gcid mesh claims
	// before the gatewayproxy bridge forwards them; the downstream
	// question_bank_handler's tenantContext middleware reads X-Tenant-Id +
	// lowercase gcid off the validated claims (without the gate every route 4xxs
	// with missing-context). The /api/v1/question-banks prefix covers the root
	// POST + the parametric /{id}[/questions[/{qid}]|/assemble-test-set] subtree;
	// /api/v1/me/question-banks is the distinct caller-scoped list leaf (no broad
	// /api/v1/me prefix exists, so it MUST be listed explicitly). Mirrors the
	// CHO-1618 Collections entries above.
	"/api/v1/question-banks",
	"/api/v1/me/question-banks",
	// ADR-143 KG hexagon canvas (2026-06-10) — the A+ Discovery canvas BFF
	// routes (routes_kg_canvas.go bridge): cluster list/create (sibling
	// §1.1+§1.2) + the clusters/ + junctions/ canvas subtrees. JWT-gated so
	// RequireChoraSessionJWT stamps tenant_id + gcid mesh claims before the
	// bridge forwards X-Tenant-Id + lowercase gcid to chora-consumption
	// (extRequireContext 400s MISSING_CONTEXT without them). The KG is
	// per-user (each learner owns disconnected MapCluster aggregates), so an
	// anonymous caller has no canvas.
	KGCanvasJWTGatedPrefix,
	// ADR-143 §8 H+ tenant KG-config dials — the parametric
	// /api/v1/tenants/{tid}/knowledge-graph/config GET+PATCH pair. Prefix
	// gating cannot express the mid-path {tid} wildcard, so the whole
	// /api/v1/tenants/ namespace is gated. Every REGISTERED route under the
	// prefix (bootstrap / me / me/branding / me/addons / setup /
	// me/idp-providers — see the individual entries above) was ALREADY
	// gated, so the only observable change is 401-instead-of-404 for
	// tokenless calls to unrouted paths under the namespace — information
	// hiding consistent with the blanket non-v1 /api/tenants/ entry. Any
	// future PUBLIC route under /api/v1/tenants/ must carve itself out of
	// this entry explicitly.
	KGCanvasTenantsJWTGatedPrefix,
	// ADR-217 Phase 2.2 (CHO-2008) — operator franchise sub-tenant create.
	// JWT-gated so RequireChoraSessionJWT stamps MeshClaims (incl. Roles) before
	// tenancy_admin_handler.go enforces the platform_operator gate — the SOLE
	// app-layer authz for the sub-tenant create-path (chora-tenancy does no role
	// check; it trusts the Istio allow-list + this gateway gate).
	"/api/v1/tenancy/sub-tenants",
	// ADR-217 Phase 2.3b (CHO-2010) — operator/tenant-admin hierarchy read.
	// JWT-gated so RequireChoraSessionJWT stamps MeshClaims (incl. GCID +
	// TenantID + Roles) before tenancy_admin_handler.go resolves the current
	// tenant + enforces the operator/tenant-admin gate.
	"/api/v1/tenancy/tenants/current/hierarchy",
	// W6 (2026-07-09) — StudentTranscript read-model, proxied to
	// chora-consumption. JWT-gated so RequireChoraSessionJWT stamps tenant_id +
	// gcid + Roles mesh claims before the gatewayproxy bridge forwards
	// X-Tenant-Id + lowercase gcid + x-mesh-user-roles (chora-consumption's
	// extRequireContext 400s MISSING_CONTEXT without the former; its
	// hasTranscriptAdminRole gate 403s every caller without the latter). Two
	// distinct exact leaves — no broad /api/v1/me prefix exists (mirrors the
	// Goals / Growth Edges siblings), and by-assessments sits OUTSIDE
	// /api/v1/me entirely so it needs its own explicit entry too.
	"/api/v1/me/transcript",
	"/api/v1/transcript/by-assessments",
	// C+ social routes (CHO-1457) — /v1/* paths handled by SocialHandler.
	// JWT-gated so RequireChoraSessionJWT stamps MeshClaims (GCID +
	// TenantID) before the aggregator fans out to chora-sharing —
	// otherwise the downstream 400s on missing gcid + X-Tenant-Id
	// headers. /v1/discovery/courses/public is intentionally absent
	// (public cross-tenant catalogue, no auth required).
	"/v1/duels",
	"/v1/duels/",
	"/v1/feed",
	"/v1/feed/",
	"/v1/me/social",
	"/v1/me/bookmarks",
	"/v1/me/profile",
	"/v1/me/profile/",
	"/v1/connections",
	"/v1/connections/",
	"/v1/leaderboard",
	"/v1/atoms/",
	"/v1/posts/",
}

// WithChoraSessionOnPrefixes wraps `next` with Chora session JWT validation
// on `DefaultJWTGatedPrefixes`. When `v` is nil the function returns `next`
// unchanged so dev callers can boot without a validator (NOT supported in
// production — main.go fails loud when the validator cannot be built). All
// other paths pass through unauthenticated; the inner mux is responsible
// for applying its own auth (session-based middleware on `/bff/*`).
func WithChoraSessionOnPrefixes(next http.Handler, v *chorasession.Validator) http.Handler {
	if v == nil {
		return next
	}
	return choraSessionOnPathPrefix(v, DefaultJWTGatedPrefixes, next)
}
