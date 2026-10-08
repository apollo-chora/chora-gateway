// gatewayproxy_handler.go — HTTP route bindings for the A6 batch of
// missing non-auth /api/* BFF proxy routes per
// docs/m13/handoff-fe-to-be-service-2026-05-14.md §A6.
//
// FE probed each route with a real Bearer ChoraSession JWT and found most
// non-auth /api/* routes return GATEWAY_ROUTE_NOT_FOUND — they were not
// routed at the gateway at all. This handler closes that gap.
//
// Routes mounted (all require Bearer JWT — the prefixes below are added to
// DefaultJWTGatedPrefixes so RequireChoraSessionJWT runs upstream and
// stamps the validated mesh claims onto the request context):
//
//	GET  /api/feature-flags                → chora-tenancy   /api/tenants/{tenantID}/entitlements
//	GET  /api/tenants/{id}                 → chora-tenancy   /api/tenants/{id}
//	GET  /api/courses/{id}                 → chora-delivery  /v1/courses/{id}
//	POST /api/courses/{id}/enrol           → chora-delivery  /v1/courses/{id}/enrolments
//	GET  /api/me/knowledge-graph/clusters  → chora-consumption /v1/me/knowledge-graph/clusters
//	GET  /api/me/companions                 → chora-consumption /v1/me/companions
//	GET  /api/me/companions/{id}/growth      → chora-consumption /v1/me/companions/{id}/growth
//	GET  /api/notifications                → chora-notifications /api/notifications
//	GET  /api/v1/me/mana                   → chora-identity  /api/v1/me/mana          (A17)
//	POST /api/v1/me/mana/topup             → chora-identity  /api/v1/me/mana/topup    (A17)
//	POST /api/v1/me/mana/demo-grant       → chora-identity  /api/v1/me/mana/demo-grant (demo only)
//
// Composition note: these /api/* paths are NOT in the gateway's legacy
// route table (internal/adapter/inmem/route_repository.go). Rather than
// extending that skeleton table, this handler is composed as an OUTER
// bridge (WithGatewayProxy) — the same proven pattern as
// WithCompanionBridge / WithNotificationsBridge / WithKGExploreBridge — so
// the bridge owns its exact paths and everything else falls through to the
// base router. The real trust boundary is RequireChoraSessionJWT (the
// HS256 ChoraSession validator), applied by WithChoraSessionOnPrefixes.
//
// The bridge passes downstream JSON bodies through verbatim — the
// downstreams already return the shapes the FE consumes directly.
package httpadapter

import (
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
	choraserver "github.com/apollo-chora/chora-common/http"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

// batchUploadDeadline bounds the AI-Assist batch upload request. The gateway
// buffers the whole multipart body (readQuestionJobBody -> io.ReadAll), so a
// multi-megabyte learning-material upload can exceed the 15s server-wide
// ReadTimeout on the wire alone; this route lifts its own deadline. Every other
// route keeps the tight default (slowloris protection).
const batchUploadDeadline = 120 * time.Second

// GatewayProxyPathPrefixes lists the BFF path prefixes the gatewayproxy mux
// serves. Exposed for inclusion in DefaultJWTGatedPrefixes.
//
// NOTE: order matters at the prefix-match level only for documentation —
// the WithGatewayProxy dispatcher checks exact paths + prefix membership.
// /api/me/companions and /api/me/knowledge-graph/clusters are BOTH under
// /api/me but are distinct, non-overlapping subtrees.
var GatewayProxyPathPrefixes = []string{
	"/api/feature-flags",
	"/api/tenants/",
	"/api/courses/",
	"/api/me/companions",
	// ADR-254 D9 alias, drop after the SPA cut (WP-X go)
	"/api/me/familiars",
	"/api/me/knowledge-graph/clusters",
	"/api/notifications",
	// A17 — mana wallet + top-up. /api/v1/me/mana covers both the GET leaf
	// and the /topup POST subpath as a JWT-gated prefix.
	"/api/v1/me/mana",
	// CJ#2 (2026-05-26) — FE post-Stripe-checkout polling. JWT-gated so
	// the downstream chora-delivery handler sees the stamped gcid + tenant
	// mesh claims and can RLS-scope the read.
	"/api/v1/me/enrolments",
	// B6.1 (2026-05-16) — searchTenantMembers picker. JWT-gated; the
	// downstream chora-identity handler enforces TRAINING_ADMIN /
	// TENANT_ADMIN role gate via x-mesh-user-roles.
	"/api/v1/admin/tenant-members",
	// Debt #4 / A6 (2026-05-16) — instructor-roster surface. JWT-gated; the
	// downstream chora-delivery handler enforces self-or-instructor-or-admin
	// role gate via x-mesh-user-roles.
	"/api/v1/instructors/",
	// Lane D (#60, 2026-05-16) — Tier 1 verbatim proxy claims:
	//   /api/v1/test-sets[/{...}]      → chora-delivery (Lane A)
	//   /api/v1/assessments[/{...}]    → chora-delivery (Lane B)
	//   /api/v1/me/assessments[/{...}] → chora-delivery (Lane B learner)
	// Each subtree's prefix is JWT-gated so RequireChoraSessionJWT stamps
	// mesh claims before the bridge dispatcher fires.
	"/api/v1/test-sets",
	"/api/v1/assessments",
	"/api/v1/me/assessments",
	// CJ#2 (2026-05-16) — courses authoring + R+ review + release.
	// JWT-gated so the downstream chora-delivery handler sees stamped
	// mesh claims for instructor / training-admin RBAC.
	"/api/v1/courses",
	// H+ tx-history (Phase 2 Agent A2, 2026-05-26) — chora-payments admin
	// REST surface: /api/v1/admin/payments/{purchases,export,stream,
	// {purchase_id}/refund}. JWT-gated; the downstream chora-payments
	// admin_handler enforces the PLATFORM_OPERATOR / TENANT_ADMIN / OWNER /
	// AUDITOR role gate via X-Chora-Role + x-mesh-user-roles.
	"/api/v1/admin/payments",
	// Open-course flow / ask (b) (2026-05-26) — chora-consumption course-
	// bound LearningPath read for the A+ course-learn page. JWT-gated so
	// RequireChoraSessionJWT stamps tenant_id + gcid mesh claims before
	// gatewayproxy.call() forwards them to chora-consumption's me_handlers
	// (which reads them off X-Tenant-Id + gcid headers).
	"/api/v1/me/learning-paths",
	// CHO-1612 — A+ open-course heterogeneous curriculum read subtree
	// (/api/v1/me/courses/{id}/content → chora-consumption).
	"/api/v1/me/courses/",
	// Epic-1b W8 — A+ Growth Edges (learner weakness): list leaf + /{id} +
	// /uploads[/{id}] subtree → chora-consumption.
	"/api/v1/me/growth-edges",
	"/api/v1/me/growth-edges/",
	// E2 (2026-09-02): H+ instance readiness, ONE round trip for the
	// readiness and go-live screens. Operator / tenant-admin audience,
	// enforced by the handler's own gate (the SOLE application-layer authz for
	// this endpoint). JWT-gated below as well: claiming a path here does NOT
	// gate it, the two lists are independent.
	"/api/v1/admin/readiness",
	// B6 item 3 (2026-09-02): the learner's remaining practice taps for the
	// current UTC day. JWT-gated so RequireChoraSessionJWT stamps tenant_id +
	// gcid before the proxy forwards them; the budget is per-learner and the
	// downstream handler reads the identity off the stamped mesh claims, never
	// off the query string.
	"/api/v1/me/practice-budget",
	// ADR-204 (2026-06-29) — A+ learner-owned Goal: list/create leaf +
	// /{id} subtree (PATCH) → chora-consumption. JWT-gated so RequireChoraSessionJWT
	// stamps tenant_id + gcid mesh claims before gatewayproxy.call() forwards them
	// to chora-consumption's goals_handler (extRequireContext reads X-Tenant-Id +
	// lowercase gcid). Mirrors the Growth Edges sibling above.
	"/api/v1/me/goals",
	"/api/v1/me/goals/",
	// WS-B (2026-07-02) — My Knowledge Atlas map read-model (map = Goal, ADR-214
	// D1): list leaf + /{goalId}/graph subtree → chora-consumption. JWT-gated so
	// RequireChoraSessionJWT stamps tenant_id + gcid mesh claims. Mirrors Goals.
	"/api/v1/me/maps",
	"/api/v1/me/maps/",
	// CHO-2045 (ADR-224) — A+ learner Dose Preferences leaf: GET read + PUT
	// replace → chora-consumption. JWT-gated so RequireChoraSessionJWT stamps
	// tenant_id + gcid mesh claims. Leaf-only (no /{id} subtree). Mirrors Goals.
	"/api/v1/me/dose-preferences",
	// ADR-225 — AI Transparency Notice (learner-self): GET state + POST
	// acknowledge → chora-governance. JWT-gated so RequireChoraSessionJWT stamps
	// tenant_id + gcid mesh claims. Two exact leaves (no /{id} subtree).
	"/api/v1/me/ai-transparency",
	"/api/v1/me/ai-transparency/acknowledge",
	// Learner-sovereign Discovery Graph (2026-07-01) — A+ per-user concept
	// graph: list leaf + subtree (reroot / concepts[/{id}] / edges[/{id}]) →
	// chora-consumption. JWT-gated so RequireChoraSessionJWT stamps tenant_id +
	// gcid mesh claims before gatewayproxy.call() forwards them to
	// chora-consumption's concept-graph handler (extRequireContext reads
	// X-Tenant-Id + lowercase gcid). Mirrors the ADR-204 Goals sibling above.
	"/api/v1/me/concept-graph",
	"/api/v1/me/concept-graph/",
	// CHO-2040 R8-6 (2026-07-05) — A+ Virgin Proofing Test LIST leaf
	// (GET /api/v1/me/proofing-tests?goal_id=) → chora-consumption. JWT-gated so
	// RequireChoraSessionJWT stamps tenant_id + gcid mesh claims before
	// gatewayproxy.call() forwards them (chora-consumption extRequireContext 400s
	// MISSING_CONTEXT without them). The POST start-runner is CompanionBridge-owned
	// under /api/v1/me/companions/{id}/proofing-test — distinct subtree.
	"/api/v1/me/proofing-tests",
	// CHO-1618 (2026-06-01) — Personal Collections (A+). JWT-gated so
	// RequireChoraSessionJWT stamps tenant_id + gcid mesh claims before
	// gatewayproxy.call() forwards them to chora-creation's collection_handler
	// (tenantContext middleware reads them off X-Tenant-Id + gcid headers).
	// The 7 routes split across /api/v1/collections (root + parametric subtree)
	// and /api/v1/me/collections (caller-scoped list).
	"/api/v1/collections",
	"/api/v1/me/collections",
	// W3.B.1 (2026-06-28) — QuestionBank curation (R+ Question Banks). JWT-gated
	// so RequireChoraSessionJWT stamps tenant_id + gcid mesh claims before
	// gatewayproxy.call() forwards them to chora-creation's question_bank_handler
	// (tenantContext middleware reads them off X-Tenant-Id + gcid headers).
	// The 9 routes split across /api/v1/question-banks (root + parametric
	// subtree) and /api/v1/me/question-banks (caller-scoped list).
	"/api/v1/question-banks",
	"/api/v1/me/question-banks",
	// C+ ChoraCircle post-create (2026-06-04) — POST /api/posts → chora-sharing
	// synchronous Moderation gate. JWT-gated so RequireChoraSessionJWT stamps
	// tenant_id + gcid mesh claims before the bridge forwards them (chora-sharing
	// reads X-Tenant-Id + lowercase gcid). The FE C+ Share composer hit this
	// route and got GATEWAY_ROUTE_NOT_FOUND — it was never wired at the gateway.
	"/api/posts",
	// SP2.9 (CHO-2048) — A+ dashboard-as-hub GCID-scoped UI preferences. The
	// /api/me/preferences prefix covers the GET read leaf + the PUT
	// /api/me/preferences/dashboard-layout AND /api/me/preferences/home-layout
	// leaves (HasPrefix; ADR-240 Track B adds the latter). JWT-gated so
	// RequireChoraSessionJWT stamps the caller's gcid mesh claims before the
	// bridge forwards to chora-identity /api/v1/me/preferences.
	"/api/me/preferences",
	// W6 (2026-07-09) — StudentTranscript read-model. Two exact leaves, both
	// GET-only, both verbatim proxy to chora-consumption's transcript_handler.go
	// (JWT-gated so RequireChoraSessionJWT stamps tenant_id + gcid + Roles mesh
	// claims before gatewayproxy.call() forwards X-Tenant-Id + lowercase gcid +
	// x-mesh-user-roles). /api/v1/me/transcript is self-scoped (learner-owned);
	// /api/v1/transcript/by-assessments is the tenant-wide R+ gradebook join,
	// role-gated DOWNSTREAM (instructor/admin/training-admin/tenant_admin) via
	// x-mesh-user-roles — the gateway does not enforce the role itself.
	"/api/v1/me/transcript",
	"/api/v1/transcript/by-assessments",
	// UX refactor B6 item 2 follow-up: the mark-seen WRITE subtree
	// /api/v1/me/transcript/{entryID}:seen → chora-consumption
	// /v1/me/transcript/{entryID}:seen. B6 item 2 shipped the downstream
	// write without this claim, so the A+ card could read the unseen flag
	// through the gateway and never clear it. The bridge owns the WHOLE
	// subtree and forwards verbatim: chora-consumption's
	// handleMeTranscriptEntryAction 404s an unknown action and 405s a
	// non-POST, and duplicating that suffix rule here would be a second
	// place to keep in step.
	"/api/v1/me/transcript/",
	// CHO-2261 — A+ atom read (Daily Dose AI-pick resolution). The bridge
	// TRANSLATES /api/v1/atoms[/{id}] onto chora-creation /api/atoms[/{id}] (the
	// mesh-allowlisted downstream). The exact leaf + the /{id} subtree.
	"/api/v1/atoms",
	"/api/v1/atoms/",
	// CHO-2276 — topic-tree Sub-phase B. The bridge TRANSLATES /api/v1/topics
	// [/{id}[/move|/atoms]] onto chora-creation /api/topics[...] (the
	// mesh-allowlisted downstream). The exact collection leaf + the /{id} subtree.
	"/api/v1/topics",
	"/api/v1/topics/",
}

// GatewayProxyHandler binds the gatewayproxy aggregator to the BFF mux.
type GatewayProxyHandler struct {
	agg *gatewayproxy.Aggregator
}

// NewGatewayProxyHandler constructs the handler.
func NewGatewayProxyHandler(agg *gatewayproxy.Aggregator) *GatewayProxyHandler {
	return &GatewayProxyHandler{agg: agg}
}

// NewGatewayProxyMux returns a mux that serves the A6 proxy routes. Combine
// with WithGatewayProxy to compose with a base handler that owns non-bridge
// paths.
func NewGatewayProxyMux(agg *gatewayproxy.Aggregator) http.Handler {
	h := NewGatewayProxyHandler(agg)
	mux := http.NewServeMux()
	// Exact-path routes.
	mux.HandleFunc("/api/feature-flags", h.handleFeatureFlags)
	mux.HandleFunc("/api/notifications", h.handleNotifications)
	mux.HandleFunc("/api/me/companions", h.handleListCompanions)
	mux.HandleFunc("/api/me/knowledge-graph/clusters", h.handleKGClusters)
	// SP2.9 (CHO-2048) — A+ dashboard-as-hub UI preferences: GET read leaf +
	// PUT dashboard-layout upsert → chora-identity /api/v1/me/preferences.
	// ADR-240 Track B adds the PUT home-layout leaf (shell /home launcher).
	mux.HandleFunc("/api/me/preferences", h.handleMePreferences)
	mux.HandleFunc("/api/me/preferences/dashboard-layout", h.handleDashboardLayout)
	mux.HandleFunc("/api/me/preferences/home-layout", h.handleHomeLayout)
	// A17 — mana wallet + top-up exact-path routes on chora-identity.
	mux.HandleFunc("/api/v1/me/mana", h.handleMeMana)
	mux.HandleFunc("/api/v1/me/mana/topup", h.handleMeManaTopup)
	// Demo-only free mana mint → chora-identity. Separate exact path from the
	// retired /topup above; the JWT-gated /api/v1/me/mana prefix covers it.
	mux.HandleFunc("/api/v1/me/mana/demo-grant", h.handleMeManaDemoGrant)
	// CHO-1883 (2026-06-26) — A+ Wallet ledger leaf (per-row transaction list).
	mux.HandleFunc("/api/v1/me/mana/ledger", h.handleMeManaLedger)
	// CHO-1883 — A+ Mana Pool ledger CSV/NDJSON export (streaming download).
	mux.HandleFunc("/api/v1/me/mana/ledger/export", h.handleMeManaLedgerExport)
	// CJ#2 (2026-05-26) — post-Stripe-checkout polling. Static exact path
	// (no parametric sub-resources). Routed to chora-delivery
	// /v1/me/enrolments — see ListMyEnrolments in gatewayproxy.go.
	mux.HandleFunc("/api/v1/me/enrolments", h.handleMeEnrolments)
	// Debt #4 / A6 (2026-05-16) — instructor-roster subtree dispatcher on
	// chora-delivery. Net/http subtree matching needs the trailing slash;
	// the handler parses {instructor_gcid} + verifies the /courses leaf.
	mux.HandleFunc("/api/v1/instructors/", h.handleInstructorSubpath)
	// Lane D (#60, 2026-05-16) — Tier 1 verbatim proxy claims for the routes
	// Lanes A (test-sets), B (assessments + me/assessments), and C
	// (atoms/questions/search) just shipped. Each subtree dispatcher
	// forwards the request unchanged — verbatim method + path + body + query
	// + content-type. Static exact-path routes are registered BEFORE the
	// trailing-slash subtree dispatch so net/http's most-specific-match wins.
	//
	// Lane C — static exact path on chora-creation. MUST register BEFORE the
	// /api/atoms/ subtree so net/http does not 307-redirect or treat
	// "questions" as an atom_id.
	mux.HandleFunc("/api/atoms/questions/search", h.handleQuestionSearch)
	// CJ#2 (2026-05-16) — courses exact + subtree on chora-delivery.
	// 6 endpoints per chora-contracts/openapi/delivery-courses.yaml.
	mux.HandleFunc("/api/v1/courses", h.handleCoursesCollection)
	mux.HandleFunc("/api/v1/courses/", h.handleCoursesSubpath)
	// Lane A — test-sets exact + subtree on chora-delivery.
	mux.HandleFunc("/api/v1/test-sets", h.handleTestSetsCollection)
	mux.HandleFunc("/api/v1/test-sets/", h.handleTestSetsSubpath)
	// Lane B — assessments exact + subtree on chora-delivery (instructor side).
	mux.HandleFunc("/api/v1/assessments", h.handleAssessmentsCollection)
	mux.HandleFunc("/api/v1/assessments/", h.handleAssessmentsSubpath)
	// Lane B — me/assessments exact + subtree on chora-delivery (learner side).
	mux.HandleFunc("/api/v1/me/assessments", h.handleMeAssessmentsCollection)
	mux.HandleFunc("/api/v1/me/assessments/", h.handleMeAssessmentsSubpath)
	// QuestionTypes registry (2026-05-15) — static exact-path route MUST be
	// registered BEFORE the /api/atoms/ subtree dispatch so net/http's most-
	// specific-match wins (otherwise handleAtomSubpath would treat the
	// literal "question-types" as an atom_id). The handler forwards the body
	// byte-identical to chora-creation per chora-contracts/openapi/
	// creation-questions.yaml::listQuestionTypes.
	mux.HandleFunc("/api/atoms/question-types", h.handleQuestionTypes)
	// A20 (2026-05-16) — POST /api/atoms collection-level createAtom. Static
	// exact path (no trailing slash). Routed BEFORE the /api/atoms/ subtree
	// dispatcher so net/http's mux does NOT 307-redirect to /api/atoms/
	// (where handleAtomSubpath would 404 on a sub-resource-less path).
	mux.HandleFunc("/api/atoms", h.handleAtomsCollection)
	// Subtree dispatchers — net/http subtree matching requires the trailing
	// slash; the handlers parse the {id} segment + reject deeper paths.
	mux.HandleFunc("/api/tenants/", h.handleTenantByID)
	mux.HandleFunc("/api/courses/", h.handleCourseSubpath)
	mux.HandleFunc("/api/me/companions/", h.handleCompanionSubpath)
	// A6 follow-up (CHO-1545) — atomic session subtree. ONLY the
	// /api/atoms/{id}/session + .../session/submit leaves are owned here;
	// handleAtomSubpath 404s any other /api/atoms/* path so the
	// WithGatewayProxy dispatcher's matchesGatewayProxyPath (which already
	// excludes non-session atom paths) keeps GET /api/atoms/{id} +
	// POST /api/atoms/{id}/feedback + /api/atoms/ai-assist Phyllis-owned.
	mux.HandleFunc("/api/atoms/", h.handleAtomSubpath)
	// chora-payments admin REST surface. The purchases-list / export / SSE
	// leaves were RETIRED at the Contextual Transaction History cutover
	// (ADR-205 / CHO-1947) — the unified /api/v1/admin/transactions surface
	// (chora-tenancy projection) supersedes them. Only the refund leaf remains
	// (refund UX re-introduction tracked as CHO-1951):
	//   POST /api/v1/admin/payments/{purchase_id}/refund    (refund)
	mux.HandleFunc("/api/v1/admin/payments/", h.handleAdminPaymentsSubpath)
	// Open-course flow / ask (b) (2026-05-26) — static exact path for the
	// course-bound LearningPath read on chora-consumption. Query string
	// carries the optional course_id (FE poll-until-ready); the leaf is the
	// course-learn page bootstrap endpoint.
	mux.HandleFunc("/api/v1/me/learning-paths", h.handleMeLearningPaths)
	// W6 (2026-07-09) — StudentTranscript read-model. Two exact GET-only
	// leaves, verbatim proxy to chora-consumption (bare /v1/... path, no
	// subtree). See handleMeTranscript / handleTranscriptByAssessments below.
	mux.HandleFunc("/api/v1/me/transcript", h.handleMeTranscript)
	mux.HandleFunc("/api/v1/transcript/by-assessments", h.handleTranscriptByAssessments)
	// UX refactor B6 item 2 follow-up: the mark-seen write subtree. Registered
	// alongside the exact leaf above: ServeMux prefers the exact pattern, so
	// the GET read keeps its own GET-only handler and only deeper paths reach
	// the entry-action bridge.
	mux.HandleFunc("/api/v1/me/transcript/", h.handleMeTranscriptEntryAction)
	// CHO-1612 — heterogeneous course curriculum read (subtree; {id}/content).
	mux.HandleFunc("/api/v1/me/courses/", h.handleMeCourseContent)
	// Epic-1b W8 — A+ Growth Edges (learner weakness) on chora-consumption.
	// Exact list leaf + parametric subtree (by-id read/dismiss + uploads producer/poll).
	// B6 item 3: remaining practice taps. Exact leaf, GET only; the handler
	// downstream owns the method check.
	mux.HandleFunc("/api/v1/me/practice-budget", h.handlePracticeBudget)
	mux.HandleFunc("/api/v1/me/growth-edges", h.handleGrowthEdges)
	mux.HandleFunc("/api/v1/me/growth-edges/", h.handleGrowthEdgesSubpath)
	// ADR-204 (2026-06-29) — A+ learner-owned Goal. Exact list/create leaf +
	// parametric subtree (PATCH {id}). Verbatim proxy to chora-consumption with
	// the /api prefix stripped, mirroring the Epic-1b W8 Growth Edges sibling.
	mux.HandleFunc("/api/v1/me/goals", h.handleGoals)
	mux.HandleFunc("/api/v1/me/goals/", h.handleGoalsSubpath)
	// WS-B (2026-07-02) — My Knowledge Atlas: map read-model (map = Goal,
	// ADR-214 D1). Exact list leaf + parametric subtree (/{goalId}/graph).
	// GET-only verbatim proxy to chora-consumption /v1/me/maps, mirroring Goals.
	mux.HandleFunc("/api/v1/me/maps", h.handleMaps)
	mux.HandleFunc("/api/v1/me/maps/", h.handleMapsSubpath)
	// CHO-2045 (ADR-224) — A+ Dose Preferences leaf. Exact GET read + PUT replace.
	// Verbatim proxy to chora-consumption /v1/me/dose-preferences (/api stripped).
	// Leaf-only (no subtree).
	mux.HandleFunc("/api/v1/me/dose-preferences", h.handleDosePreferences)
	// ADR-225 — AI Transparency Notice (learner-self). Two exact leaves, verbatim
	// proxy to chora-governance /v1/me/ai-transparency[/acknowledge].
	mux.HandleFunc("/api/v1/me/ai-transparency", h.handleAITransparency)
	mux.HandleFunc("/api/v1/me/ai-transparency/acknowledge", h.handleAITransparencyAcknowledge)
	// Learner-sovereign Discovery Graph (2026-07-01) — A+ per-user concept
	// graph. Exact list leaf + parametric subtree (reroot / concepts[/{id}] /
	// edges[/{id}]). Verbatim proxy to chora-consumption with the /api prefix
	// stripped — the subtree handler forwards ALL methods (the downstream router
	// does the leaf dispatch), mirroring the ADR-204 Goals sibling.
	mux.HandleFunc("/api/v1/me/concept-graph", h.handleConceptGraph)
	mux.HandleFunc("/api/v1/me/concept-graph/", h.handleConceptGraphSubpath)
	// CHO-2040 R8-6 (2026-07-05) — Virgin Proofing Test LIST leaf (GET only,
	// optional ?goal_id=) → chora-consumption /v1/me/proofing-tests.
	mux.HandleFunc("/api/v1/me/proofing-tests", h.handleProofingTests)
	// CHO-1618 (2026-06-01) — Personal Collections. 3 mux entries cover the 7
	// routes: the collection root (POST create), the parametric subtree
	// (GET / PATCH / DELETE {id} + POST {id}/atoms + DELETE {id}/atoms/{atomId}),
	// and the caller-scoped list. Verbatim proxy to chora-creation at the SAME
	// path. The exact `/api/v1/me/collections` leaf is registered alongside the
	// `/api/v1/me/courses/` + `/api/v1/me/learning-paths` siblings (distinct,
	// non-overlapping subtrees under /api/v1/me).
	mux.HandleFunc("/api/v1/collections", h.handleCollectionsCollection)
	// Exact-pattern route — takes precedence over the /api/v1/collections/
	// subtree dispatcher so "search" is NOT parsed as a {collectionId}.
	mux.HandleFunc("/api/v1/collections/search", h.handleCollectionsSearch)
	mux.HandleFunc("/api/v1/collections/", h.handleCollectionsSubpath)
	mux.HandleFunc("/api/v1/me/collections", h.handleListMyCollections)
	// W3.B.1 (2026-06-28) — QuestionBank curation. 3 mux entries cover the 9
	// routes: the collection root (POST create), the parametric subtree
	// (GET / PATCH / DELETE {id} + POST/GET {id}/questions + DELETE
	// {id}/questions/{qid} + POST {id}/assemble-test-set), and the caller-scoped
	// list. Verbatim proxy to chora-creation at the SAME path. Mirrors the
	// CHO-1618 Collections wiring above.
	mux.HandleFunc("/api/v1/question-banks", h.handleQuestionBanksCollection)
	mux.HandleFunc("/api/v1/question-banks/", h.handleQuestionBanksSubpath)
	mux.HandleFunc("/api/v1/me/question-banks", h.handleListMyQuestionBanks)
	// Async daily-dose AI enrichment (2026-06-05) — GET only; long timeout
	// (downstream runs the Recommender + Companion LLM calls to completion). The
	// fast deterministic dose stays phyllis-owned at /api/companion/daily-dose.
	mux.HandleFunc("/api/companion/daily-dose/ai", h.handleDailyDoseAI)
	// CHO-2261 — A+ atom read on the /v1/ prefix (atom.service.ts). The exact
	// collection leaf + the /{id} subtree. Both TRANSLATE to chora-creation
	// /api/atoms[/{id}] (the mesh-allowlisted downstream). GET-only; the
	// handlers 405 other methods (fail loud, never a silent 404).
	mux.HandleFunc("/api/v1/atoms", h.handleV1AtomsCollection)
	mux.HandleFunc("/api/v1/atoms/", h.handleV1AtomsItem)
	// CHO-2276 — topic-tree Sub-phase B on the /v1/ prefix. The exact collection
	// leaf (GET list / POST create) + the /{id} subtree (GET/PUT/DELETE +
	// /{id}/move + /{id}/atoms POST). Both TRANSLATE to chora-creation
	// /api/topics[...]. Unsupported methods 405 (fail loud, never a silent 404).
	mux.HandleFunc("/api/v1/topics", h.handleV1TopicsCollection)
	mux.HandleFunc("/api/v1/topics/", h.handleV1TopicsItem)
	// ADR-254 D9 alias, drop after the SPA cut (WP-X go): the pre-rename
	// /api/me/familiars* + /api/familiar/daily-dose/ai paths are re-pathed onto
	// the companion routes above (same handler, same upstream).
	return withAliasPrefixes(mux, gatewayProxyAliasPrefixes)
}

// gatewayProxyAliasPrefixes maps each pre-rename public prefix the gatewayproxy
// mux served onto its companion-named prefix (see adr254_alias.go).
// ADR-254 D9 alias, drop after the SPA cut (WP-X go)
var gatewayProxyAliasPrefixes = map[string]string{
	"/api/me/familiars":           "/api/me/companions",
	"/api/familiar/daily-dose/ai": "/api/companion/daily-dose/ai",
}

// WithGatewayProxy composes a gatewayproxy mux with a base handler: routes
// the gatewayproxy mux owns are served by the bridge; everything else
// falls through to `base`. Passes through when `agg` is nil so
// cmd/server/main.go can opt out in unconfigured envs.
//
// matchesGatewayProxyPath decides ownership: an exact match on a leaf
// route, or membership under one of the subtree prefixes. /api/me/companions
// (exact) and /api/me/companions/ (subtree) are both owned; /api/me/anything
// else falls through to the base router (e.g. the Phyllis /api/me handler).
func WithGatewayProxy(base http.Handler, agg *gatewayproxy.Aggregator) http.Handler {
	if agg == nil {
		return base
	}
	bridgeMux := NewGatewayProxyMux(agg)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if matchesGatewayProxyPath(r.URL.Path) {
			bridgeMux.ServeHTTP(w, r)
			return
		}
		base.ServeHTTP(w, r)
	})
}

// matchesGatewayProxyPath reports whether the gatewayproxy bridge owns the
// path. Exact leaf matches OR subtree membership (prefix + "/").
//
// CRITICAL — order of dispatch under /api/atoms/:
//  1. EXACT static /api/atoms/question-types  (registry passthrough)
//  2. PARAMETRIC /api/atoms/{id}/session[...] (atomic session leaves)
//  3. PARAMETRIC /api/atoms/{id}/questions[...] + /question-jobs[...] (P7/P7.5)
//  4. EVERYTHING ELSE under /api/atoms/* falls through to Phyllis (atom
//     fetch / feedback / ai-assist).
//
// The static-path claim MUST precede the parametric predicates so the literal
// "question-types" segment isn't shadowed by the {atom_id} interpretation.
func matchesGatewayProxyPath(p string) bool {
	// Carve-out: /api/tenants/me is the phyllis-owned auth-context-aware
	// endpoint that rewrites the URL with the caller's tenant_id from the
	// validated JWT before forwarding to chora-tenancy. The subtree prefix
	// "/api/tenants/" matched below would otherwise capture it as a
	// literal {id}="me" lookup, which 404s on chora-tenancy. Falling
	// through here lets the inner phyllis_handler.go mount at
	// "/api/tenants/me" take the request. (CHO-1692 hydration depends
	// on this — it reads branding + wizard_completed_at via this path.)
	if p == "/api/tenants/me" {
		return false
	}
	// Carve-out (L1 CHO-1705/CHO-1708): the tenant-members admin family is
	// phyllis-owned — GET roster search + POST add-by-email + PATCH
	// {gcid}/role all dispatch in phyllis_handler.handleAdminTenantMembers.
	// The bridge's old B6.1 GET-only claim shadowed the POST/PATCH mounts
	// (composed-router 405, walk-caught 2026-06-10). Falling through keeps
	// ONE owner for the family; phyllis forwards RawQuery verbatim so the
	// R+ assessment-cohort picker keeps its behavior.
	if p == "/api/v1/admin/tenant-members" ||
		strings.HasPrefix(p, "/api/v1/admin/tenant-members/") {
		return false
	}
	switch p {
	case "/api/feature-flags",
		"/api/notifications",
		"/api/me/companions",
		// ADR-254 D9 alias, drop after the SPA cut (WP-X go)
		"/api/me/familiars",
		"/api/me/knowledge-graph/clusters",
		// SP2.9 (CHO-2048) - A+ dashboard-as-hub UI preferences. All three leaves
		// are exact-path owned; the /api/me/preferences JWT-gate prefix covers
		// them all (HasPrefix). GET read + PUT dashboard-layout → chora-identity.
		// ADR-240 Track B adds the PUT home-layout leaf (shell /home launcher).
		"/api/me/preferences",
		"/api/me/preferences/dashboard-layout",
		"/api/me/preferences/home-layout",
		// A17 — mana wallet + top-up. Both leaves are exact-path owned.
		"/api/v1/me/mana",
		"/api/v1/me/mana/topup",
		// Demo-only mana mint (grantDemoManaToSelf). EXACT-path owned; the
		// /api/v1/me/mana JWT-gate prefix covers it. NOT the retired
		// /topup route — the two must stay distinct so the paid path can
		// never silently become a free mint.
		"/api/v1/me/mana/demo-grant",
		// CHO-1883 (2026-06-26) — A+ Wallet ledger leaf + CSV/NDJSON export.
		// EXACT-path owned; the /api/v1/me/mana JWT-gate prefix covers both
		// (HasPrefix).
		"/api/v1/me/mana/ledger",
		"/api/v1/me/mana/ledger/export",
		// CJ#2 (2026-05-26) — FE post-Stripe-checkout polling. Static exact
		// path; query string carries course_id but the path itself is bare.
		"/api/v1/me/enrolments",
		// QuestionTypes registry — static exact path. MUST be matched here
		// (before the isAtomQuestionPath parametric predicate) so the literal
		// "question-types" segment isn't interpreted as an atom_id by the
		// Phyllis atom-fetch route.
		"/api/atoms/question-types",
		// A20 (2026-05-16) — POST /api/atoms collection-level creation. The
		// gateway claims the EXACT static path (no trailing slash) so
		// net/http's mux does NOT 307-redirect to /api/atoms/ (where
		// handleAtomSubpath would then 404 on a sub-resource-less path).
		// Routed to chora-creation:/api/atoms which is the canonical
		// createAtom handler per chora-contracts/openapi/creation-admin.yaml.
		"/api/atoms",
		// CJ#2 (2026-05-16) — courses exact-path. Subtree below.
		"/api/v1/courses",
		// Lane D (#60, 2026-05-16) — Tier 1 verbatim proxy leaves.
		// Static EXACT paths claimed here; the subtree-trailing-slash forms
		// are claimed via the subtree loop below.
		"/api/v1/test-sets",
		"/api/v1/assessments",
		"/api/v1/me/assessments",
		// Lane D / Lane C — chora-creation question search. Static exact
		// path on /api/atoms/. MUST be matched here BEFORE the parametric
		// isAtomQuestionPath / isAtomSessionPath / isAtomPublishPath
		// predicates so the literal "questions/search" path doesn't get
		// shadowed by an {atom_id} interpretation.
		"/api/atoms/questions/search",
		// chora-payments admin REST surface — the purchases/export/stream exact
		// leaves were retired (ADR-205 / CHO-1947); only the parametric refund
		// leaf remains, matched via the /api/v1/admin/payments/ subtree below.
		// Open-course flow / ask (b) (2026-05-26) — chora-consumption course-
		// bound LearningPath read. Static exact path; query string carries the
		// optional course_id. No subtree owned here (GET-by-id leaf is not
		// part of the A+ open-course flow).
		"/api/v1/me/learning-paths",
		// Epic-1b W8 — A+ Growth Edges list leaf (filter/sort/page query).
		"/api/v1/me/growth-edges",
		// ADR-204 (2026-06-29) — A+ learner-owned Goal list/create leaf. The
		// parametric /{id} subtree (PATCH) is claimed via the loop below.
		"/api/v1/me/goals",
		// WS-B (2026-07-02) — My Knowledge Atlas map list leaf (GET). The
		// /{goalId}/graph subtree is claimed via the loop below.
		"/api/v1/me/maps",
		// CHO-2045 (ADR-224) — A+ Dose Preferences leaf (GET + PUT). Leaf-only; no subtree.
		"/api/v1/me/dose-preferences",
		// ADR-225 — AI Transparency Notice leaves (GET state + POST acknowledge).
		"/api/v1/me/ai-transparency",
		"/api/v1/me/ai-transparency/acknowledge",
		// Learner-sovereign Discovery Graph (2026-07-01) — A+ concept-graph list
		// leaf (GET). The /reroot + /concepts[/{id}] + /edges[/{id}] subtree is
		// claimed via the loop below.
		"/api/v1/me/concept-graph",
		// CHO-2040 R8-6 (2026-07-05) — Virgin Proofing Test LIST leaf. Static
		// exact path (no parametric subtree; ?goal_id= rides the query). The POST
		// start-runner is CompanionBridge-owned under /api/v1/me/companions/.
		"/api/v1/me/proofing-tests",
		// CHO-1618 (2026-06-01) — Personal Collections. The collection root
		// (POST create) + the caller-scoped list are static exact paths; the
		// parametric /{id}[/atoms[/{atomId}]] subtree is claimed below.
		"/api/v1/collections",
		"/api/v1/me/collections",
		// W3.B.1 (2026-06-28) — QuestionBank curation. The collection root
		// (POST create) + the caller-scoped list are static exact paths; the
		// parametric /{id}[/questions[/{qid}]] + /{id}/assemble-test-set subtree
		// is claimed below.
		"/api/v1/question-banks",
		"/api/v1/me/question-banks",
		// Async daily-dose AI enrichment — exact path. The fast deterministic
		// /api/companion/daily-dose stays phyllis-owned (distinct exact path).
		"/api/companion/daily-dose/ai",
		// ADR-254 D9 alias, drop after the SPA cut (WP-X go)
		"/api/familiar/daily-dose/ai",
		// CHO-2261 — A+ atom collection leaf (GET /api/v1/atoms). The /{id}
		// subtree is claimed via the loop below. Translated to chora-creation
		// /api/atoms. Owned here so it does NOT fall through to the Phyllis base.
		"/api/v1/atoms",
		// CHO-2276 — topic-tree collection leaf (GET list / POST create on
		// /api/v1/topics). The /{id}[/move|/atoms] subtree is claimed via the
		// loop below. Translated to chora-creation /api/topics. Owned here so it
		// does NOT fall through to the Phyllis base.
		"/api/v1/topics",
		// W6 (2026-07-09) — StudentTranscript read-model. Two exact GET-only
		// leaves (no subtree — chora-consumption registers both as bare
		// mux.Handle exact paths, no parametric segment).
		"/api/v1/me/transcript",
		"/api/v1/transcript/by-assessments":
		return true
	}
	// Subtree prefixes — only the trailing-slash forms, so a bare
	// "/api/tenants" or "/api/courses" (Phyllis-owned exact paths) is NOT
	// captured here.
	for _, sub := range []string{
		"/api/tenants/",
		"/api/courses/",
		"/api/me/companions/",
		// ADR-254 D9 alias, drop after the SPA cut (WP-X go)
		"/api/me/familiars/",
		// Debt #4 / A6 (2026-05-16) — instructor-roster surface. The bridge
		// owns the WHOLE /api/v1/instructors/ subtree so a missing /courses
		// leaf falls through to the handler's 404 rather than the base router.
		"/api/v1/instructors/",
		// CJ#2 (2026-05-16) — courses subtree. /api/v1/courses/{id}[/{action}]
		// (incl. /content authoring routes → chora-delivery, CHO-1612).
		"/api/v1/courses/",
		// CHO-1612 — learner course curriculum read subtree
		// /api/v1/me/courses/{id}/content → chora-consumption.
		"/api/v1/me/courses/",
		// UX refactor B6 item 2 follow-up: transcript entry-action subtree:
		// /api/v1/me/transcript/{entryID}:seen (POST) → chora-consumption. The
		// bridge owns the WHOLE subtree and forwards verbatim so an unknown
		// action falls through to the downstream's 404 rather than the base
		// router. The bare /api/v1/me/transcript read leaf is matched by the
		// exact-path switch above and keeps its own GET-only handler.
		"/api/v1/me/transcript/",
		// Epic-1b W8 — A+ Growth Edges subtree: /{id} (read/dismiss) +
		// /uploads (multipart producer) + /uploads/{id} (poll) → chora-consumption.
		"/api/v1/me/growth-edges/",
		// ADR-204 (2026-06-29) — A+ learner-owned Goal subtree: /{id} (PATCH
		// update) → chora-consumption. The bridge owns the WHOLE subtree so a
		// malformed sub-path falls through to the downstream's 404 rather than the
		// base router.
		"/api/v1/me/goals/",
		// WS-B (2026-07-02) — My Knowledge Atlas map subtree: /{goalId}/graph
		// (GET) → chora-consumption. The bridge owns the WHOLE subtree.
		"/api/v1/me/maps/",
		// Learner-sovereign Discovery Graph (2026-07-01) — A+ concept-graph
		// subtree: /reroot, /concepts, /concepts/{id}, /edges, /edges/{id}. The
		// bridge owns the WHOLE subtree so a malformed sub-path falls through to
		// the downstream's 404 rather than the base router. Forwarded VERBATIM
		// (all methods) — chora-consumption's router does the leaf dispatch.
		"/api/v1/me/concept-graph/",
		// CHO-1618 (2026-06-01) — Personal Collections parametric subtree:
		// /api/v1/collections/{id}, /{id}/atoms, /{id}/atoms/{atomId}. The
		// bridge owns the WHOLE subtree so a malformed sub-path falls through
		// to the handler's 404 rather than the base router.
		"/api/v1/collections/",
		// W3.B.1 (2026-06-28) — QuestionBank parametric subtree:
		// /api/v1/question-banks/{id}, /{id}/questions, /{id}/questions/{qid},
		// /{id}/assemble-test-set. The bridge owns the WHOLE subtree so a
		// malformed sub-path falls through to the handler's 404 rather than the
		// base router.
		"/api/v1/question-banks/",
		// Lane D (#60, 2026-05-16) — Tier 1 subtree dispatch. The bridge
		// owns the WHOLE subtree for each — every sub-path is forwarded to
		// the downstream service verbatim (the downstream's own handler
		// 404s unknown paths so the FE sees the real semantic error).
		"/api/v1/test-sets/",
		"/api/v1/assessments/",
		"/api/v1/me/assessments/",
		// H+ tx-history (Phase 2 Agent A2, 2026-05-26) — chora-payments admin
		// REST surface subtree. Covers the parametric refund leaf
		// /api/v1/admin/payments/{purchase_id}/refund. The static leaves
		// (purchases / export / stream) are matched via the exact-path switch
		// above; the subtree predicate is reached only for parametric paths.
		"/api/v1/admin/payments/",
		// CHO-2261 — A+ atom-detail subtree (/api/v1/atoms/{id}). The bridge
		// owns the WHOLE subtree; handleV1AtomsItem 405s non-GET and 404s any
		// deeper sub-path (no /v1/ atom sub-resources are proxied — those live
		// on the /api/atoms prefix). Translated to chora-creation /api/atoms/{id}.
		"/api/v1/atoms/",
		// CHO-2276 — topic-tree item subtree (/api/v1/topics/{id}[/move|/atoms]).
		// The bridge owns the WHOLE subtree; handleV1TopicsItem enforces the
		// per-leaf method matrix (405) and 404s an empty id / unknown sub-path.
		// Translated to chora-creation /api/topics/{id}[...].
		"/api/v1/topics/",
	} {
		if strings.HasPrefix(p, sub) {
			return true
		}
	}
	// A6 follow-up (CHO-1545) — atomic session leaves under /api/atoms/.
	// The /api/atoms/ subtree is OTHERWISE Phyllis-owned (GET atom detail,
	// POST feedback, ai-assist), so the bridge claims ONLY the /session
	// and /session/submit leaves — everything else under /api/atoms/ falls
	// through to the base (Phyllis) router.
	if isAtomSessionPath(p) {
		return true
	}
	// P7 (2026-05-15) — manual question authoring leaves under /api/atoms/.
	// The bridge claims /api/atoms/{atom_id}/questions and
	// /api/atoms/{atom_id}/questions/{question_id} — proxies them verbatim
	// to chora-creation. All other /api/atoms/* paths stay Phyllis-owned.
	if isAtomQuestionPath(p) {
		return true
	}
	// A22 (2026-05-16) — POST /api/atoms/{atom_id}/publish (Phase K). The
	// bridge proxies the publish CTA verbatim to chora-creation. Response is
	// a bare LearningAtom (no {atom} wrap) so the FE unwrap is a no-op.
	if isAtomPublishPath(p) {
		return true
	}
	// WS-7b (2026-05-26) — GET /api/atoms/{atomId}/revisions — revision
	// history page. The bridge owns this leaf and proxies it verbatim to
	// chora-creation (same path). All other /api/atoms/* paths stay
	// Phyllis-owned or are claimed by the predicates above.
	if isAtomRevisionsPath(p) {
		return true
	}
	// Phase-A.2 Wave 2 (2026-06-28) — POST /api/atoms/{atom_id}/clone
	// (clone-as-variant). Proxied verbatim to chora-creation; response is the
	// new LearningAtom. All other /api/atoms/* paths stay Phyllis-owned.
	if isAtomClonePath(p) {
		return true
	}
	// ADR-229 WS-1 (CHO-2127) — PATCH /api/atoms/{atom_id}/reuse-visibility
	// (author-only reuse-consent audience). Proxied verbatim to
	// chora-creation. All other /api/atoms/* paths stay Phyllis-owned.
	if isAtomReuseVisibilityPath(p) {
		return true
	}
	return false
}

// isAtomReuseVisibilityPath reports whether p is the ADR-229 WS-1 reuse-
// consent leaf the gatewayproxy bridge owns:
//
//	/api/atoms/{atomId}/reuse-visibility
//
// {atomId} must be a single non-empty path segment. Any other /api/atoms/*
// shape returns false so it falls through to the other predicates / Phyllis.
func isAtomReuseVisibilityPath(p string) bool {
	rest := strings.TrimPrefix(p, "/api/atoms/")
	if rest == p { // no prefix match
		return false
	}
	rest = strings.TrimSuffix(rest, "/")
	parts := strings.Split(rest, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] == "reuse-visibility"
}

// isAtomSessionPath reports whether p is one of the two atomic-session
// leaves the gatewayproxy bridge owns:
//
//	/api/atoms/{atomId}/session
//	/api/atoms/{atomId}/session/submit
//
// {atomId} must be a single non-empty path segment with no embedded
// slash. Any other /api/atoms/* shape returns false so it falls through
// to the Phyllis router.
func isAtomSessionPath(p string) bool {
	rest := strings.TrimPrefix(p, "/api/atoms/")
	if rest == p { // no prefix match
		return false
	}
	rest = strings.TrimSuffix(rest, "/")
	parts := strings.Split(rest, "/")
	// {atomId}/session  →  2 parts;  {atomId}/session/submit  →  3 parts.
	if len(parts) == 2 {
		return parts[0] != "" && parts[1] == "session"
	}
	if len(parts) == 3 {
		return parts[0] != "" && parts[1] == "session" && parts[2] == "submit"
	}
	return false
}

// isAtomQuestionPath reports whether p is one of the P7 manual-question
// authoring OR P7.5 AI single-question async / model-answer adhoc leaves the
// gatewayproxy bridge owns:
//
//	/api/atoms/{atomId}/questions                                    (P7)
//	/api/atoms/{atomId}/questions/{questionId}                       (P7)
//	/api/atoms/{atomId}/question-jobs                                (P7.5)
//	/api/atoms/{atomId}/question-jobs/{jobId}                        (P7.5)
//	/api/atoms/{atomId}/question-jobs/{jobId}/accept                 (P7.5)
//	/api/atoms/{atomId}/questions/{questionId}/ai-model-answer-jobs  (P7.5)
//
// All segments must be single non-empty path segments. Any other /api/atoms/*
// shape (e.g. GET /api/atoms/{id} read-side which is Phyllis-owned, or the
// /session leaves owned by isAtomSessionPath) returns false.
func isAtomQuestionPath(p string) bool {
	rest := strings.TrimPrefix(p, "/api/atoms/")
	if rest == p { // no prefix match
		return false
	}
	rest = strings.TrimSuffix(rest, "/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[0] == "" {
		return false
	}
	// -------------------------------------------------------------------
	// P7 — /questions[...]
	// -------------------------------------------------------------------
	if parts[1] == "questions" {
		// /questions               (2)
		// /questions/{questionId}  (3)
		// /questions/{questionId}/ai-model-answer-jobs (4)  ← P7.5
		switch len(parts) {
		case 2:
			return true
		case 3:
			return parts[2] != ""
		case 4:
			return parts[2] != "" && parts[3] == "ai-model-answer-jobs"
		}
		return false
	}
	// -------------------------------------------------------------------
	// P7.5 — /question-jobs[...]
	// -------------------------------------------------------------------
	if parts[1] == "question-jobs" {
		// /question-jobs                          (2)
		// /question-jobs/{jobId}                  (3)
		// /question-jobs/{jobId}/accept           (4)
		// /question-jobs/{jobId}/regenerate-image (4)  ← CHO-1822
		switch len(parts) {
		case 2:
			return true
		case 3:
			return parts[2] != ""
		case 4:
			return parts[2] != "" && (parts[3] == "accept" || parts[3] == "regenerate-image")
		}
		return false
	}
	return false
}

// isAtomPublishPath reports whether p is the A22 Phase K publishAtom leaf:
//
//	/api/atoms/{atomId}/publish
//
// {atomId} must be a single non-empty path segment with no embedded slash.
// Any other /api/atoms/* shape returns false so it falls through to the base
// router (Phyllis-owned for GET / feedback / ai-assist).
func isAtomPublishPath(p string) bool {
	rest := strings.TrimPrefix(p, "/api/atoms/")
	if rest == p { // no prefix match
		return false
	}
	rest = strings.TrimSuffix(rest, "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" {
		return false
	}
	return parts[1] == "publish"
}

// isAtomClonePath reports whether p is the Phase-A.2 Wave 2 clone-as-variant
// leaf:
//
//	/api/atoms/{atomId}/clone
//
// {atomId} must be a single non-empty path segment with no embedded slash.
// Any other /api/atoms/* shape returns false so it falls through to the base
// (Phyllis) router.
func isAtomClonePath(p string) bool {
	rest := strings.TrimPrefix(p, "/api/atoms/")
	if rest == p { // no prefix match
		return false
	}
	rest = strings.TrimSuffix(rest, "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" {
		return false
	}
	return parts[1] == "clone"
}

// isAtomRevisionsPath reports whether p is the WS-7b revision-history leaf:
//
//	/api/atoms/{atomId}/revisions
//
// {atomId} must be a single non-empty path segment with no embedded slash.
// Any other /api/atoms/* shape returns false so it falls through to the base
// router (Phyllis-owned for GET / feedback / ai-assist). The revisions leaf
// is GET-only; the handler enforces 405 on other methods.
func isAtomRevisionsPath(p string) bool {
	rest := strings.TrimPrefix(p, "/api/atoms/")
	if rest == p { // no prefix match
		return false
	}
	rest = strings.TrimSuffix(rest, "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" {
		return false
	}
	return parts[1] == "revisions"
}

// -----------------------------------------------------------------------------
// GET /api/feature-flags
// -----------------------------------------------------------------------------

func (h *GatewayProxyHandler) handleFeatureFlags(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/feature-flags")
		return
	}
	resp, _ := h.agg.GetFeatureFlags(r.Context(), gatewayProxyAuthFromRequest(r))
	writeGatewayProxyResp(w, resp)
}

// -----------------------------------------------------------------------------
// GET /api/tenants/{id}
// -----------------------------------------------------------------------------

func (h *GatewayProxyHandler) handleTenantByID(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(
		strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/tenants/"), "/"),
	)
	if tenantID == "" || strings.Contains(tenantID, "/") {
		writeError(w, http.StatusNotFound, "GATEWAY_TENANT_ID_REQUIRED",
			"path must be /api/tenants/{id}")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/tenants/{id}")
		return
	}
	// Audience, then scope, mirroring the readiness endpoint.
	//
	// Until 2026-09-06 this route had NEITHER. It read an arbitrary tenant by
	// id from the path, so any authenticated user of any tenant could read any
	// other tenant's record, and five consecutive layers left the join to the
	// next one: GetTenant forwards the id unchanged, chora-tenancy's
	// handleTenantByID does not compare it, its tenantRequired asserts only
	// that X-Tenant-Id is PRESENT, and LegacyTenantStore.Get runs on
	// context.Background() against a tenants table that carries no RLS policy
	// while 23 of its siblings do.
	//
	// Audience is the H+ Tenant Admin set: every frontend caller of this route
	// is an H+ surface (hplus/tenant-overview, hplus/setup), so this is the
	// audience the product already assumes.
	if !hasPlatformOperatorRole(r) && !hasTenantAdminRole(r) {
		writeError(w, http.StatusForbidden, "GATEWAY_FORBIDDEN",
			"reading a tenant record requires the platform_operator or tenant_admin role")
		return
	}
	if !callerMayReadTenant(r, tenantID) {
		log.Printf("gatewayproxy: refused cross-tenant tenant read: gcid=%s session_tenant=%q requested_tenant=%q",
			strings.TrimSpace(r.Header.Get("gcid")), sessionTenantID(r), tenantID)
		writeError(w, http.StatusForbidden, "GATEWAY_FORBIDDEN",
			"a tenant record is readable only by its own organisation")
		return
	}
	resp, _ := h.agg.GetTenant(r.Context(), gatewayProxyAuthFromRequest(r), tenantID)
	writeGatewayProxyResp(w, resp)
}

// -----------------------------------------------------------------------------
// GET /api/courses/{id}  +  POST /api/courses/{id}/enrol
// -----------------------------------------------------------------------------

func (h *GatewayProxyHandler) handleCourseSubpath(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/courses/"), "/")
	if rest == "" {
		writeError(w, http.StatusNotFound, "GATEWAY_COURSE_ID_REQUIRED",
			"path must be /api/courses/{id}")
		return
	}
	parts := strings.Split(rest, "/")
	courseID := parts[0]
	if courseID == "" {
		writeError(w, http.StatusNotFound, "GATEWAY_COURSE_ID_REQUIRED",
			"path must be /api/courses/{id}")
		return
	}

	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		// GET /api/courses/{id}
		resp, _ := h.agg.GetCourse(r.Context(), gatewayProxyAuthFromRequest(r), courseID)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 1:
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/courses/{id}")
	case len(parts) == 2 && parts[1] == "enrol" && r.Method == http.MethodPost:
		// POST /api/courses/{id}/enrol
		body := readBody(r)
		resp, _ := h.agg.EnrolCourse(r.Context(), gatewayProxyAuthFromRequest(r), courseID, body)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 2 && parts[1] == "enrol":
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only on /api/courses/{id}/enrol")
	default:
		writeError(w, http.StatusNotFound, "GATEWAY_ROUTE_NOT_FOUND",
			"unknown sub-path on /api/courses/{id}")
	}
}

// -----------------------------------------------------------------------------
// GET /api/me/knowledge-graph/clusters
// -----------------------------------------------------------------------------

func (h *GatewayProxyHandler) handleKGClusters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/me/knowledge-graph/clusters")
		return
	}
	resp, _ := h.agg.GetKGClusters(r.Context(), gatewayProxyAuthFromRequest(r))
	writeGatewayProxyResp(w, resp)
}

// -----------------------------------------------------------------------------
// GET /api/me/companions  +  GET /api/me/companions/{id}/growth
// -----------------------------------------------------------------------------

func (h *GatewayProxyHandler) handleListCompanions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/me/companions")
		return
	}
	resp, _ := h.agg.ListCompanions(r.Context(), gatewayProxyAuthFromRequest(r))
	writeGatewayProxyResp(w, resp)
}

func (h *GatewayProxyHandler) handleCompanionSubpath(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/me/companions/"), "/")
	parts := strings.Split(rest, "/")
	// Only /{id}/growth is wired in this bridge (the other ADR-149 subpaths
	// — hatch / kg-neighbors / source-revelation / growth-events — are
	// owned by the existing CompanionBridge under /api/v1/me/companions).
	if len(parts) != 2 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "GATEWAY_ROUTE_NOT_FOUND",
			"path must be /api/me/companions/{id}/growth")
		return
	}
	companionID, leaf := parts[0], parts[1]
	if leaf != "growth" {
		writeError(w, http.StatusNotFound, "GATEWAY_ROUTE_NOT_FOUND",
			"only /api/me/companions/{id}/growth is served here")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/me/companions/{id}/growth")
		return
	}
	resp, _ := h.agg.GetCompanionGrowth(r.Context(), gatewayProxyAuthFromRequest(r), companionID)
	writeGatewayProxyResp(w, resp)
}

// -----------------------------------------------------------------------------
// GET /api/notifications
// -----------------------------------------------------------------------------

func (h *GatewayProxyHandler) handleNotifications(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/notifications")
		return
	}
	resp, _ := h.agg.ListNotifications(r.Context(), gatewayProxyAuthFromRequest(r), r.URL.RawQuery)
	writeGatewayProxyResp(w, resp)
}

// -----------------------------------------------------------------------------
// GET /api/v1/me/proofing-tests — Virgin Proofing Test list (CHO-2040 R8-6).
//
// The POST start-runner is CompanionBridge-owned at
// /api/v1/me/companions/{id}/proofing-test; this is the learner-scoped LIST leaf
// with an optional ?goal_id= filter, proxied verbatim to chora-consumption
// /v1/me/proofing-tests. GET only; the downstream returns the learner's
// ProofingTest list the FE consumes directly (passed through verbatim).
// -----------------------------------------------------------------------------

func (h *GatewayProxyHandler) handleProofingTests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/v1/me/proofing-tests")
		return
	}
	resp, _ := h.agg.ListProofingTests(r.Context(), gatewayProxyAuthFromRequest(r), r.URL.RawQuery)
	writeGatewayProxyResp(w, resp)
}

// -----------------------------------------------------------------------------
// GET /api/atoms/question-types — question-type registry (passthrough)
//
// Bug fixed: pre-fix the Phyllis /api/atoms/{id} read-side route claimed this
// static path (treating "question-types" as an atom_id) and wrapped the
// chora-creation body in {"atom": ...}. The bridge now owns the exact static
// path and forwards the body byte-identical per chora-contracts/openapi/
// creation-questions.yaml::listQuestionTypes which returns {"items": [...]}
// at the top level.
// -----------------------------------------------------------------------------

func (h *GatewayProxyHandler) handleQuestionTypes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/atoms/question-types")
		return
	}
	resp, _ := h.agg.GetQuestionTypes(r.Context(), gatewayProxyAuthFromRequest(r))
	writeGatewayProxyResp(w, resp)
}

// -----------------------------------------------------------------------------
// A20 (2026-05-16) — POST /api/atoms — collection-level createAtom proxy.
//
// chora-creation registers its createAtom handler on /api/atoms (no trailing
// slash) per services/chora-creation/internal/adapter/http/handler.go:161.
// The previous gateway routing only claimed /api/atoms/ (trailing slash) on
// handleAtomSubpath, so net/http would 307-redirect POST /api/atoms →
// /api/atoms/ and handleAtomSubpath would 404 ("path must be
// /api/atoms/{id}/..."). FE Phase I AI-assist mint blocked on this loop.
//
// Fix: claim the EXACT static path /api/atoms (no slash) on the gateway and
// forward verbatim to chora-creation. Body, method, headers all passed
// through; response status + body passed through verbatim per the existing
// classify() helper.
// -----------------------------------------------------------------------------

func (h *GatewayProxyHandler) handleAtomsCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		body := readBody(r)
		resp, _ := h.agg.CreateAtom(r.Context(), gatewayProxyAuthFromRequest(r), body)
		writeGatewayProxyResp(w, resp)
	case http.MethodGet:
		// GET /api/atoms — list atoms (collection-level read). Forward query
		// string verbatim so chora-creation's listAtoms can parse status etc.
		resp, _ := h.agg.ListAtoms(r.Context(), gatewayProxyAuthFromRequest(r), r.URL.RawQuery)
		writeGatewayProxyResp(w, resp)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST and GET only on /api/atoms")
	}
}

// -----------------------------------------------------------------------------
// GET /api/companion/daily-dose/ai — async daily-dose AI enrichment (2026-06-05).
//
// The fast deterministic dose is phyllis-owned at /api/companion/daily-dose; this
// route proxies the AI layer (Companion greeting + Recommender picks) to
// chora-consumption with a long timeout so the gateway-metered LLM calls run to
// completion (mana umbrella) instead of the 3s hot-path cancel. The FE fetches
// it after the dose renders.
// -----------------------------------------------------------------------------

func (h *GatewayProxyHandler) handleDailyDoseAI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/companion/daily-dose/ai")
		return
	}
	resp, _ := h.agg.GetDailyDoseAI(r.Context(), gatewayProxyAuthFromRequest(r))
	writeGatewayProxyResp(w, resp)
}

// -----------------------------------------------------------------------------
// CHO-2261 — GET /api/v1/atoms[/{id}] — A+ atom read proxy.
//
// The A+ atom services (atom.service.ts loadAtom / loadAtoms) call the /api/v1/
// prefix; the working PLAY page + author tooling call the sibling /api/atoms
// prefix (chora-creation, Phyllis-owned for the {id} GET which wraps as
// {atom:...}). The Daily Dose resolves each AI-picked atom via
// GET /api/v1/atoms/{id} — unregistered it 404'd (GATEWAY_ROUTE_NOT_FOUND) and
// the pick was fail-SOFT dropped, silently thinning the dose.
//
// These leaves TRANSLATE the /v1/ prefix onto chora-creation /api/atoms[/{id}]
// (the path the live mesh AuthorizationPolicy allowlists) and forward the
// response VERBATIM — UNWRAPPED, because atom.service.ts consumes the
// LearningAtom directly. GET-only; other methods 405 (fail loud per the AC —
// never a silent 404). The /api/atoms sibling stays Phyllis-owned + wrapped.
// -----------------------------------------------------------------------------

func (h *GatewayProxyHandler) handleV1AtomsCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/v1/atoms")
		return
	}
	resp, _ := h.agg.ListAtoms(r.Context(), gatewayProxyAuthFromRequest(r), r.URL.RawQuery)
	writeGatewayProxyResp(w, resp)
}

func (h *GatewayProxyHandler) handleV1AtomsItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/v1/atoms/{id}")
		return
	}
	// Parse {id} from /api/v1/atoms/{id}. Reject deeper sub-paths — no /v1/ atom
	// sub-resources are proxied here (they live on the /api/atoms prefix).
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/atoms/")
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" || strings.Contains(rest, "/") {
		writeError(w, http.StatusNotFound, "GATEWAY_ATOM_ID_REQUIRED",
			"path must be /api/v1/atoms/{id} (no sub-resources)")
		return
	}
	resp, _ := h.agg.GetAtomByID(r.Context(), gatewayProxyAuthFromRequest(r), rest)
	writeGatewayProxyResp(w, resp)
}

// -----------------------------------------------------------------------------
// CHO-2276 — /api/v1/topics[/{id}[/move|/atoms]] — content topic-tree proxy
// (Sub-phase B). The A+/R+ topic-tree UI hits the /v1/ prefix; chora-creation
// (Sub-phase A, CHO-2275) serves the tree at /api/topics — NO /v1/. These
// leaves TRANSLATE the /v1/ prefix onto /api/topics[...] (the mesh-allowlisted
// downstream) and forward method + body + query VERBATIM via ProxyTopicTree.
//
// Reads (GET) are learner-safe (JWT + RLS). Writes (POST/PUT/DELETE) are
// admin-gated DOWNSTREAM: call() stamps x-mesh-user-roles from the validated
// JWT and chora-creation's topic handler enforces admin/owner (requireAdmin,
// defence-in-depth). The gateway dispatchers 405 eagerly on an unsupported
// method (fail loud, never a silent 404) and 404 an empty id / unknown
// sub-path. The operator-only POST /api/internal/topics/backfill is NOT
// registered here (it bypasses the tenant-header gate; tenant is in the body).
// -----------------------------------------------------------------------------

// handleV1TopicsCollection serves the collection root /api/v1/topics (no path
// suffix): GET list (optional ?parent_id) + POST create (admin). Other methods
// 405. Translated to chora-creation /api/topics.
func (h *GatewayProxyHandler) handleV1TopicsCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		resp, _ := h.agg.ProxyTopicTree(r.Context(), gatewayProxyAuthFromRequest(r), http.MethodGet, "", "", r.URL.RawQuery, nil)
		writeGatewayProxyResp(w, resp)
	case http.MethodPost:
		body := readBody(r)
		resp, _ := h.agg.ProxyTopicTree(r.Context(), gatewayProxyAuthFromRequest(r), http.MethodPost, "", "", "", body)
		writeGatewayProxyResp(w, resp)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET and POST only on /api/v1/topics")
	}
}

// handleV1TopicsItem dispatches the parametric leaves under /api/v1/topics/:
//
//	GET    /api/v1/topics/{id}            get node
//	PUT    /api/v1/topics/{id}            rename / reorder      (admin)
//	DELETE /api/v1/topics/{id}            soft-delete           (admin)
//	POST   /api/v1/topics/{id}/move       reparent             (admin)
//	POST   /api/v1/topics/{id}/atoms      attach an atom        (admin)
//
// {id} is parsed exactly like handleCollectionsSubpath. Any other sub-path is
// 404. Method preservation is the verbatim contract — the downstream
// chora-creation topic handler 405s unsupported methods, but the gateway
// dispatcher 405s eagerly so a misbehaving call doesn't waste a round-trip.
func (h *GatewayProxyHandler) handleV1TopicsItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/topics/"), "/")
	if rest == "" {
		writeError(w, http.StatusNotFound, "GATEWAY_TOPIC_ID_REQUIRED",
			"path must be /api/v1/topics/{id}[/move|/atoms]")
		return
	}
	parts := strings.Split(rest, "/")
	topicID := parts[0]
	if topicID == "" {
		writeError(w, http.StatusNotFound, "GATEWAY_TOPIC_ID_REQUIRED",
			"path must be /api/v1/topics/{id}[/move|/atoms]")
		return
	}
	auth := gatewayProxyAuthFromRequest(r)

	switch {
	// /api/v1/topics/{id} — GET / PUT / DELETE.
	case len(parts) == 1 && r.Method == http.MethodGet:
		resp, _ := h.agg.ProxyTopicTree(r.Context(), auth, http.MethodGet, topicID, "", "", nil)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 1 && r.Method == http.MethodPut:
		body := readBody(r)
		resp, _ := h.agg.ProxyTopicTree(r.Context(), auth, http.MethodPut, topicID, "", "", body)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 1 && r.Method == http.MethodDelete:
		resp, _ := h.agg.ProxyTopicTree(r.Context(), auth, http.MethodDelete, topicID, "", "", nil)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 1:
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET / PUT / DELETE only on /api/v1/topics/{id}")

	// /api/v1/topics/{id}/move — POST reparent.
	case len(parts) == 2 && parts[1] == "move" && r.Method == http.MethodPost:
		body := readBody(r)
		resp, _ := h.agg.ProxyTopicTree(r.Context(), auth, http.MethodPost, topicID, "move", "", body)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 2 && parts[1] == "move":
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only on /api/v1/topics/{id}/move")

	// /api/v1/topics/{id}/atoms — POST attach.
	case len(parts) == 2 && parts[1] == "atoms" && r.Method == http.MethodPost:
		body := readBody(r)
		resp, _ := h.agg.ProxyTopicTree(r.Context(), auth, http.MethodPost, topicID, "atoms", "", body)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 2 && parts[1] == "atoms":
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only on /api/v1/topics/{id}/atoms")

	default:
		writeError(w, http.StatusNotFound, "GATEWAY_ROUTE_NOT_FOUND",
			"only /api/v1/topics/{id}, /api/v1/topics/{id}/move, and /api/v1/topics/{id}/atoms are served here")
	}
}

// -----------------------------------------------------------------------------
// POST /api/atoms/{id}/session  +  POST /api/atoms/{id}/session/submit
// (A6 follow-up — CHO-1545; atomic session, Phyllis step 7)
// -----------------------------------------------------------------------------

// handleAtomSubpath dispatches the bridge-owned leaves under /api/atoms/.
// It is registered for the WHOLE /api/atoms/ subtree on the gatewayproxy
// mux, but the WithGatewayProxy dispatcher's matchesGatewayProxyPath only
// routes the bridge-owned leaves here:
//
//	A6 follow-up (CHO-1545)        — /session + /session/submit
//	P7 (2026-05-15)                — /questions + /questions/{question_id}
//
// Every other /api/atoms/* path stays Phyllis-owned and never reaches this
// handler. The defensive 404 covers the direct-mux test path
// (NewGatewayProxyMux exercised without the WithGatewayProxy wrapper).
func (h *GatewayProxyHandler) handleAtomSubpath(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/atoms/"), "/")
	parts := strings.Split(rest, "/")
	atomID := parts[0]
	if atomID == "" {
		writeError(w, http.StatusNotFound, "GATEWAY_ATOM_ID_REQUIRED",
			"path must be /api/atoms/{id}/{session|questions}[...]")
		return
	}

	switch {
	// -------------------------------------------------------------------
	// A6 follow-up (CHO-1545) — atomic session leaves.
	// -------------------------------------------------------------------
	case len(parts) == 2 && parts[1] == "session" && r.Method == http.MethodPost:
		// POST /api/atoms/{id}/session - start an AtomAttempt.
		resp, _ := h.agg.StartAtomSession(r.Context(), gatewayProxyAuthFromRequest(r), atomID)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 2 && parts[1] == "session":
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only on /api/atoms/{id}/session")
	case len(parts) == 3 && parts[1] == "session" && parts[2] == "submit" && r.Method == http.MethodPost:
		// POST /api/atoms/{id}/session/submit — submit answers + grade.
		body := readBody(r)
		resp, _ := h.agg.SubmitAtomSession(r.Context(), gatewayProxyAuthFromRequest(r), atomID, body)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 3 && parts[1] == "session" && parts[2] == "submit":
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only on /api/atoms/{id}/session/submit")

	// -------------------------------------------------------------------
	// P7 (2026-05-15) — manual question authoring leaves. Pure verbatim
	// proxy to chora-creation (commit 83a769e7 / :p3-b82d1e32). 4 methods:
	// POST collection-level; GET / PATCH / DELETE on the question_id leaf.
	// -------------------------------------------------------------------
	case len(parts) == 2 && parts[1] == "questions" && r.Method == http.MethodPost:
		// POST /api/atoms/{id}/questions — create a Question (FE Phase C).
		body := readBody(r)
		resp, _ := h.agg.CreateQuestion(r.Context(), gatewayProxyAuthFromRequest(r), atomID, body)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 2 && parts[1] == "questions":
		// Collection-level path supports POST only (no GET — the canonical
		// author read is GET by question_id; the FE A16 read-side uses
		// GET /api/atoms/{id} via Phyllis).
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only on /api/atoms/{id}/questions")
	case len(parts) == 3 && parts[1] == "questions" && r.Method == http.MethodGet:
		// GET /api/atoms/{id}/questions/{question_id} — admin author projection.
		resp, _ := h.agg.GetQuestion(r.Context(), gatewayProxyAuthFromRequest(r), atomID, parts[2])
		writeGatewayProxyResp(w, resp)
	case len(parts) == 3 && parts[1] == "questions" && r.Method == http.MethodPatch:
		// PATCH /api/atoms/{id}/questions/{question_id} — edit (appends a
		// QuestionRevision; bumps AtomRevision downstream).
		body := readBody(r)
		resp, _ := h.agg.EditQuestion(r.Context(), gatewayProxyAuthFromRequest(r), atomID, parts[2], body)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 3 && parts[1] == "questions" && r.Method == http.MethodDelete:
		// DELETE /api/atoms/{id}/questions/{question_id} — soft-delete (FE A18).
		// Idempotent — re-deleting still returns 204.
		resp, _ := h.agg.DeleteQuestion(r.Context(), gatewayProxyAuthFromRequest(r), atomID, parts[2])
		writeGatewayProxyResp(w, resp)
	case len(parts) == 3 && parts[1] == "questions":
		// Leaf path supports GET / PATCH / DELETE only.
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET / PATCH / DELETE only on /api/atoms/{id}/questions/{question_id}")

	// -------------------------------------------------------------------
	// P7.5 (2026-05-15) — /questions/{question_id}/ai-model-answer-jobs
	// adhoc model-answer fill.
	// -------------------------------------------------------------------
	case len(parts) == 4 && parts[1] == "questions" && parts[3] == "ai-model-answer-jobs" && r.Method == http.MethodPost:
		// POST /api/atoms/{id}/questions/{question_id}/ai-model-answer-jobs.
		// Body forwarded verbatim (optional tone_hint).
		body := readBody(r)
		resp, _ := h.agg.CreateModelAnswerJob(r.Context(), gatewayProxyAuthFromRequest(r), atomID, parts[2], body)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 4 && parts[1] == "questions" && parts[3] == "ai-model-answer-jobs":
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only on /api/atoms/{id}/questions/{question_id}/ai-model-answer-jobs")

	// -------------------------------------------------------------------
	// P7.5 (2026-05-15) — /question-jobs[...]
	// AI single-question async (ai_draft / batch_source_material).
	// -------------------------------------------------------------------
	case len(parts) == 2 && parts[1] == "question-jobs" && r.Method == http.MethodPost:
		// POST /api/atoms/{id}/question-jobs — create job. May carry
		// multipart/form-data for the batch path; forward Content-Type
		// verbatim so the boundary is preserved.
		// Lift the read/write deadline for THIS request only: the batch
		// upload buffers a multi-MB body and would otherwise be severed at
		// the 15s server-wide timeout (the 504 that read as a forever-spinner).
		if err := choraserver.ExtendRequestDeadlines(w, batchUploadDeadline); err != nil {
			log.Printf("gatewayproxy: question-jobs upload deadline extension unsupported (falling back to server timeout): %v", err)
		}
		contentType := r.Header.Get("Content-Type")
		body := readQuestionJobBody(r)
		resp, _ := h.agg.CreateQuestionJob(r.Context(), gatewayProxyAuthFromRequest(r), atomID, contentType, body)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 2 && parts[1] == "question-jobs":
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only on /api/atoms/{id}/question-jobs")
	case len(parts) == 3 && parts[1] == "question-jobs" && r.Method == http.MethodGet:
		// GET /api/atoms/{id}/question-jobs/{job_id} — poll job status.
		resp, _ := h.agg.GetQuestionJob(r.Context(), gatewayProxyAuthFromRequest(r), atomID, parts[2])
		writeGatewayProxyResp(w, resp)
	case len(parts) == 3 && parts[1] == "question-jobs":
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/atoms/{id}/question-jobs/{job_id}")
	case len(parts) == 4 && parts[1] == "question-jobs" && parts[3] == "accept" && r.Method == http.MethodPost:
		// POST /api/atoms/{id}/question-jobs/{job_id}/accept — persist
		// the accepted candidates.
		body := readBody(r)
		resp, _ := h.agg.AcceptQuestionJob(r.Context(), gatewayProxyAuthFromRequest(r), atomID, parts[2], body)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 4 && parts[1] == "question-jobs" && parts[3] == "accept":
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only on /api/atoms/{id}/question-jobs/{job_id}/accept")
	case len(parts) == 4 && parts[1] == "question-jobs" && parts[3] == "regenerate-image" && r.Method == http.MethodPost:
		// POST /api/atoms/{id}/question-jobs/{job_id}/regenerate-image — CHO-1822
		// review-stage image regenerate. Body {draft_id, placement, prompt};
		// forwarded verbatim (+ the FE Idempotency-Key via auth).
		body := readBody(r)
		resp, _ := h.agg.RegenerateQuestionJobImage(r.Context(), gatewayProxyAuthFromRequest(r), atomID, parts[2], body)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 4 && parts[1] == "question-jobs" && parts[3] == "regenerate-image":
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only on /api/atoms/{id}/question-jobs/{job_id}/regenerate-image")

	// -------------------------------------------------------------------
	// A22 (2026-05-16) — POST /api/atoms/{id}/publish (Phase K). Bare
	// LearningAtom response (no {atom} wrap) per A22.Q2.
	// -------------------------------------------------------------------
	case len(parts) == 2 && parts[1] == "publish" && r.Method == http.MethodPost:
		resp, _ := h.agg.PublishAtom(r.Context(), gatewayProxyAuthFromRequest(r), atomID)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 2 && parts[1] == "publish":
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only on /api/atoms/{id}/publish")

	// -------------------------------------------------------------------
	// Phase-A.2 Wave 2 (2026-06-28) — POST /api/atoms/{id}/clone
	// (clone-as-variant → NEW draft atom + question; provenance stamped).
	// -------------------------------------------------------------------
	case len(parts) == 2 && parts[1] == "clone" && r.Method == http.MethodPost:
		body := readBody(r)
		resp, _ := h.agg.CloneAtom(r.Context(), gatewayProxyAuthFromRequest(r), atomID, body)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 2 && parts[1] == "clone":
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only on /api/atoms/{id}/clone")

	// -------------------------------------------------------------------
	// ADR-229 WS-1 (CHO-2127) — PATCH /api/atoms/{id}/reuse-visibility
	// (author-only reuse-consent audience). Verbatim proxy to
	// chora-creation; 4xx (403 CREATION_NOT_AUTHOR / 400 / 404 / 409) pass
	// through unchanged as the FE's semantic signal.
	// -------------------------------------------------------------------
	case len(parts) == 2 && parts[1] == "reuse-visibility" && r.Method == http.MethodPatch:
		body := readBody(r)
		resp, _ := h.agg.ChangeAtomReuseVisibility(r.Context(), gatewayProxyAuthFromRequest(r), atomID, body)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 2 && parts[1] == "reuse-visibility":
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"PATCH only on /api/atoms/{id}/reuse-visibility")

	// -------------------------------------------------------------------
	// WS-7b (2026-05-26) — GET /api/atoms/{id}/revisions. Verbatim proxy
	// to chora-creation (same path). query string forwarded verbatim so
	// page_size + page_token cursor pagination flows through unchanged.
	// GET-only — the append-only POST /revisions is an author-admin
	// operation served directly on chora-creation (not surfaced via the
	// BFF revision-history view).
	// -------------------------------------------------------------------
	case len(parts) == 2 && parts[1] == "revisions" && r.Method == http.MethodGet:
		resp, _ := h.agg.ListAtomRevisions(r.Context(), gatewayProxyAuthFromRequest(r), atomID, r.URL.RawQuery)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 2 && parts[1] == "revisions":
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/atoms/{id}/revisions")

	default:
		writeError(w, http.StatusNotFound, "GATEWAY_ROUTE_NOT_FOUND",
			"only /api/atoms/{id}/session[/submit], /api/atoms/{id}/questions[/{question_id}[/ai-model-answer-jobs]], /api/atoms/{id}/question-jobs[/{job_id}[/accept]], /api/atoms/{id}/publish, /api/atoms/{id}/reuse-visibility, and /api/atoms/{id}/revisions are served here")
	}
}

// readQuestionJobBody reads the question-jobs POST body with a larger cap
// than readBody so the multipart batch source-material upload path can
// flow through. The chora-creation upstream handler enforces its own
// upload-size policy via its multipart parser.
//
// 50 MB cap — same order of magnitude as Cloud Run's default upload limit
// + comfortable for a single learning-material file. Multipart streaming
// would be cleaner long-term but would require a deeper refactor (the
// aggregator currently takes a []byte body so the multipart body is
// already materialised in memory anyway).
func readQuestionJobBody(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	defer r.Body.Close()
	const maxBytes = 50 << 20 // 50 MiB
	b, _ := io.ReadAll(io.LimitReader(r.Body, maxBytes))
	return b
}

// -----------------------------------------------------------------------------
// A17 — GET /api/v1/me/mana  +  POST /api/v1/me/mana/topup
// (mana wallet + top-up proxy to chora-identity)
// -----------------------------------------------------------------------------

func (h *GatewayProxyHandler) handleMeMana(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/v1/me/mana")
		return
	}
	resp, _ := h.agg.GetMyMana(r.Context(), gatewayProxyAuthFromRequest(r))
	writeGatewayProxyResp(w, resp)
}

// handleMeManaLedger — GET /api/v1/me/mana/ledger[?from&to&direction&reason&page_size]
//
// CHO-1883 (2026-06-26) — the A+ Wallet transaction list. chora-identity's
// listManaLedger (me_economy_handlers.go) RLS-scopes the per-row ledger to the
// JWT gcid and returns {items:[...]}. The dispatcher previously claimed only
// /api/v1/me/mana (+ /topup) as EXACT paths, so this leaf leaked to Phyllis →
// 404 at the edge (mana-parity handoff §2.4). rawQuery is forwarded verbatim so
// the FE can filter/paginate without a gateway redeploy.
func (h *GatewayProxyHandler) handleMeManaLedger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/v1/me/mana/ledger")
		return
	}
	resp, _ := h.agg.ListMyManaLedger(r.Context(), gatewayProxyAuthFromRequest(r), r.URL.RawQuery)
	writeGatewayProxyResp(w, resp)
}

// handleMeManaLedgerExport — GET /api/v1/me/mana/ledger/export?format=csv|json
//
// CHO-1883 — streaming CSV/NDJSON download of the caller's mana ledger.
// Streaming pass-through: ExportManaLedger writes directly to w (preserving
// Content-Type + Content-Disposition), unlike the JSON-only
// writeGatewayProxyResp. 405 on non-GET.
func (h *GatewayProxyHandler) handleMeManaLedgerExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/v1/me/mana/ledger/export")
		return
	}
	_ = h.agg.ExportManaLedger(r.Context(), gatewayProxyAuthFromRequest(r), r.URL.RawQuery, w)
}

// handleMeEnrolments — GET /api/v1/me/enrolments?course_id=...
//
// CJ#2 (2026-05-26) — FE post-Stripe-checkout polling endpoint. Forwards
// to chora-delivery /v1/me/enrolments verbatim with rawQuery; downstream
// resolves the gcid + tenant from the stamped mesh headers and returns
// {items, total} for that learner. Empty items + 200 is the FE's "still
// waiting" signal; non-empty items lets the poller match by course_id.
func (h *GatewayProxyHandler) handleMeEnrolments(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/v1/me/enrolments")
		return
	}
	resp, _ := h.agg.ListMyEnrolments(r.Context(), gatewayProxyAuthFromRequest(r), r.URL.RawQuery)
	writeGatewayProxyResp(w, resp)
}

// handleMeManaDemoGrant — POST /api/v1/me/mana/demo-grant
//
// Demo-only free mana mint, proxied to chora-identity. Distinct route from
// the retired /api/v1/me/mana/topup (which returned 410 and must stay
// retired): this one carries no payment semantics and MUST NOT be folded
// into the Stripe top-up path, which has to stay a single route that always
// charges. 405 on non-POST.
func (h *GatewayProxyHandler) handleMeManaDemoGrant(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only on /api/v1/me/mana/demo-grant")
		return
	}
	resp, _ := h.agg.GrantDemoMana(r.Context(), gatewayProxyAuthFromRequest(r))
	writeGatewayProxyResp(w, resp)
}

// handleMeManaTopup — RETIRED (WS-2.3, 2026-06-04). Per-user mana top-up moved
// to the Stripe-backed POST /api/v1/checkout/user-mana flow (chora-payments
// CreateUserManaTopUpSession → user_mana_topup.payment_captured.v1 →
// chora-identity CreditMana source=topup). The old endpoint proxied to
// chora-identity /api/v1/me/mana/topup, itself retired to 404 when the Stripe
// SDK moved to chora-payments (ADR-164). Return 410 Gone with a pointer rather
// than silently proxying a dead upstream.
func (h *GatewayProxyHandler) handleMeManaTopup(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusGone, "GATEWAY_ENDPOINT_RETIRED",
		"POST /api/v1/me/mana/topup is retired; use POST /api/v1/checkout/user-mana (Stripe mana top-up)")
}

// -----------------------------------------------------------------------------
// Debt #4 / A6 — GET /api/v1/instructors/{instructor_gcid}/courses
// (instructor-roster surface) — pure passthrough to chora-delivery.
// -----------------------------------------------------------------------------

// handleInstructorSubpath dispatches the /api/v1/instructors/ subtree.
//
// Supported leaf:
//
//	GET /api/v1/instructors/{instructor_gcid}/courses
//
// Anything else under the subtree is 404 — there is no other resource the
// bridge owns here. The downstream chora-delivery handler enforces
// authorization + RLS; the bridge just forwards the request verbatim.
func (h *GatewayProxyHandler) handleInstructorSubpath(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/instructors/"), "/")
	if rest == "" {
		writeError(w, http.StatusNotFound, "GATEWAY_INSTRUCTOR_ID_REQUIRED",
			"path must be /api/v1/instructors/{instructor_gcid}/courses")
		return
	}
	parts := strings.Split(rest, "/")
	instructorGCID := parts[0]
	if instructorGCID == "" {
		writeError(w, http.StatusNotFound, "GATEWAY_INSTRUCTOR_ID_REQUIRED",
			"path must be /api/v1/instructors/{instructor_gcid}/courses")
		return
	}
	// Only /{instructor_gcid}/courses is owned here.
	if len(parts) != 2 || parts[1] != "courses" {
		writeError(w, http.StatusNotFound, "GATEWAY_ROUTE_NOT_FOUND",
			"only /api/v1/instructors/{instructor_gcid}/courses is served here")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/v1/instructors/{instructor_gcid}/courses")
		return
	}
	resp, _ := h.agg.ListInstructorCourses(r.Context(), gatewayProxyAuthFromRequest(r), instructorGCID, r.URL.RawQuery)
	writeGatewayProxyResp(w, resp)
}

// -----------------------------------------------------------------------------
// Lane D (#60, 2026-05-16) — Tier 1 verbatim proxy handlers for the routes
// Lanes A (test-sets), B (assessments + me/assessments), and C
// (atoms/questions/search) just shipped. Each handler is a thin dispatcher:
//   - reads the inbound method + path + raw query + body + content-type
//   - delegates to the aggregator's ProxyTestSets / ProxyAssessments /
//     ProxyMeAssessments / ProxyQuestionSearch verbatim passthrough
//   - writes the aggregator response (status + body + content-type) to the
//     FE via writeGatewayProxyResp
//
// The /api/v1/* test-sets and assessments routes accept the full set of
// HTTP methods declared by chora-contracts/openapi/delivery-test-sets.yaml +
// delivery-assessments.yaml: GET / POST / PATCH / DELETE. The downstream
// chora-delivery handler 405s any other method, so the gateway forwards
// every method through; method preservation is the verbatim contract.
//
// /api/atoms/questions/search is GET-only per
// chora-contracts/openapi/creation-questions.yaml#searchQuestions — the
// gateway 405s any non-GET method here so a misbehaving FE call doesn't
// waste a round-trip on chora-creation.
// -----------------------------------------------------------------------------

// handleTestSetsCollection serves the collection-level routes on
// /api/v1/test-sets (no path suffix): GET (list) + POST (create). Other
// methods are forwarded to chora-delivery which 405s — the gateway is a
// verbatim passthrough so it does NOT intercept the 405.
func (h *GatewayProxyHandler) handleTestSetsCollection(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	resp, _ := h.agg.ProxyTestSets(r.Context(), gatewayProxyAuthFromRequest(r),
		r.Method, r.URL.Path, r.URL.RawQuery, body, r.Header.Get("Content-Type"))
	writeGatewayProxyResp(w, resp)
}

// handleTestSetsSubpath serves every path under /api/v1/test-sets/ — the
// downstream owns the route table (per-question CRUD, /publish, /archive,
// etc). The gateway forwards verbatim.
func (h *GatewayProxyHandler) handleTestSetsSubpath(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	resp, _ := h.agg.ProxyTestSets(r.Context(), gatewayProxyAuthFromRequest(r),
		r.Method, r.URL.Path, r.URL.RawQuery, body, r.Header.Get("Content-Type"))
	writeGatewayProxyResp(w, resp)
}

// handleCoursesCollection serves the collection-level routes on
// /api/v1/courses (no path suffix): GET (list?state=) + POST (create DRAFT).
// Verbatim passthrough to chora-delivery per E2E-BE-CJ2 directive row at
// `docs/m13/e2e-fe-coord-directive-2026-05-16.md` §3.
func (h *GatewayProxyHandler) handleCoursesCollection(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	resp, _ := h.agg.ProxyCourses(r.Context(), gatewayProxyAuthFromRequest(r),
		r.Method, r.URL.Path, r.URL.RawQuery, body, r.Header.Get("Content-Type"))
	writeGatewayProxyResp(w, resp)
}

// handleCoursesSubpath serves every path under /api/v1/courses/ — the
// downstream owns the route table: GET /{id}, PATCH /{id},
// POST /{id}/publish, POST /{id}/release, POST /{id}/reject,
// POST /{id}/checkout (CJ#2 Stripe extension 2026-05-24).
// Verbatim passthrough; RBAC + payment-state checks enforced downstream.
func (h *GatewayProxyHandler) handleCoursesSubpath(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	resp, _ := h.agg.ProxyCourses(r.Context(), gatewayProxyAuthFromRequest(r),
		r.Method, r.URL.Path, r.URL.RawQuery, body, r.Header.Get("Content-Type"))
	writeGatewayProxyResp(w, resp)
}

// handleAssessmentsCollection serves the collection-level routes on
// /api/v1/assessments: POST createAssessment + GET listAssessments (TBD).
// Verbatim passthrough to chora-delivery.
func (h *GatewayProxyHandler) handleAssessmentsCollection(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	resp, _ := h.agg.ProxyAssessments(r.Context(), gatewayProxyAuthFromRequest(r),
		r.Method, r.URL.Path, r.URL.RawQuery, body, r.Header.Get("Content-Type"))
	writeGatewayProxyResp(w, resp)
}

// handleAssessmentsSubpath serves every path under /api/v1/assessments/ —
// /{id}, /{id}/monitor, /{id}/submissions, /{id}/release-results, etc.
// Verbatim passthrough to chora-delivery; the downstream enforces the
// instructor / training-admin role gate via x-mesh-user-roles.
func (h *GatewayProxyHandler) handleAssessmentsSubpath(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	resp, _ := h.agg.ProxyAssessments(r.Context(), gatewayProxyAuthFromRequest(r),
		r.Method, r.URL.Path, r.URL.RawQuery, body, r.Header.Get("Content-Type"))
	writeGatewayProxyResp(w, resp)
}

// handleMeAssessmentsCollection serves the learner-side collection-level
// route on /api/v1/me/assessments: GET listMyAssessments. Self-cohort
// enforcement is delegated to the downstream — gateway stamps the caller
// GCID via the mesh-claim headers; chora-delivery filters to that GCID.
func (h *GatewayProxyHandler) handleMeAssessmentsCollection(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	resp, _ := h.agg.ProxyMeAssessments(r.Context(), gatewayProxyAuthFromRequest(r),
		r.Method, r.URL.Path, r.URL.RawQuery, body, r.Header.Get("Content-Type"))
	writeGatewayProxyResp(w, resp)
}

// handleMeAssessmentsSubpath serves every path under /api/v1/me/assessments/:
// /{id}, /{id}/submissions, /{id}/submissions/{subId}/autosave,
// /{id}/submissions/{subId}/submit, /{id}/submissions/{subId}/result.
// Verbatim passthrough; method preservation is critical for PATCH autosave
// vs POST submit semantics.
func (h *GatewayProxyHandler) handleMeAssessmentsSubpath(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	resp, _ := h.agg.ProxyMeAssessments(r.Context(), gatewayProxyAuthFromRequest(r),
		r.Method, r.URL.Path, r.URL.RawQuery, body, r.Header.Get("Content-Type"))
	writeGatewayProxyResp(w, resp)
}

// handleQuestionSearch serves GET /api/atoms/questions/search — the
// cross-atom question picker for the A+ test-set editor (per ADR-155 D1).
// chora-creation serves the route at the SAME path; the gateway forwards
// the query string verbatim. GET-only (the contract has no POST search).
func (h *GatewayProxyHandler) handleQuestionSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/atoms/questions/search")
		return
	}
	resp, _ := h.agg.ProxyQuestionSearch(r.Context(), gatewayProxyAuthFromRequest(r), r.URL.RawQuery)
	writeGatewayProxyResp(w, resp)
}

// -----------------------------------------------------------------------------
// chora-payments admin REST — refund leaf only.
//
// The purchases-list / export / SSE leaves were RETIRED at the Contextual
// Transaction History cutover (ADR-205 / CHO-1947); the unified
// /api/v1/admin/transactions surface (chora-tenancy projection) supersedes
// them. The refund leaf remains (parametric, in the subtree dispatcher):
//
//	POST /api/v1/admin/payments/{purchase_id}/refund    → handleAdminPaymentsSubpath
//
// JWT-gated (DefaultJWTGatedPrefixes carries the /api/v1/admin/payments
// prefix); the downstream chora-payments admin_handler enforces the role gate
// (PLATFORM_OPERATOR / TENANT_ADMIN / OWNER) via X-Chora-Role + x-mesh-user-roles.
// AUDITOR is read-only and cannot REFUND — the downstream returns 403, passed
// through verbatim. Refund UX re-introduction is tracked as CHO-1951.
// -----------------------------------------------------------------------------

// handleAdminPaymentsSubpath serves the parametric leaves under
// /api/v1/admin/payments/:
//
//	POST /api/v1/admin/payments/{purchase_id}/refund    (V1 scope)
//
// Any other sub-path is 404 — chora-payments admin_handler defines no other
// parametric route under this prefix. The static leaves (purchases / export /
// stream) are claimed by exact-path handlers BEFORE the subtree dispatcher
// in NewGatewayProxyMux so net/http's most-specific-match wins.
func (h *GatewayProxyHandler) handleAdminPaymentsSubpath(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/admin/payments/"), "/")
	if rest == "" {
		writeError(w, http.StatusNotFound, "GATEWAY_PURCHASE_ID_REQUIRED",
			"path must be /api/v1/admin/payments/{purchase_id}/refund")
		return
	}
	parts := strings.Split(rest, "/")
	purchaseID := parts[0]
	if purchaseID == "" {
		writeError(w, http.StatusNotFound, "GATEWAY_PURCHASE_ID_REQUIRED",
			"path must be /api/v1/admin/payments/{purchase_id}/refund")
		return
	}
	// Only /{purchase_id}/refund is owned here.
	if len(parts) != 2 || parts[1] != "refund" {
		writeError(w, http.StatusNotFound, "GATEWAY_ROUTE_NOT_FOUND",
			"only /api/v1/admin/payments/{purchase_id}/refund is served here")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only on /api/v1/admin/payments/{purchase_id}/refund")
		return
	}
	body := readBody(r)
	resp, _ := h.agg.IssuePaymentRefund(r.Context(), gatewayProxyAuthFromRequest(r),
		purchaseID, r.URL.RawQuery, body)
	writeGatewayProxyResp(w, resp)
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// gatewayProxyAuthFromRequest mirrors notificationsAuthFromRequest /
// kgExploreAuthFromRequest for the gatewayproxy aggregator's AuthCtx shape.
// Pulls Bearer + traceparent + tenant + mesh-claims onto the outbound call
// so the downstreams see the X-Tenant-Id + lowercase gcid context they
// require.
//
// A17 — also captures the FE Idempotency-Key header so the gatewayproxy.call
// helper can stamp it on the outbound POST /api/v1/me/mana/topup call per
// learner-economy.yaml:482. The header propagates verbatim — gateway does
// NOT synthesise one when absent (chora-identity's topupMana handler
// synthesises a Stripe-pi-derived fallback in that case).
func gatewayProxyAuthFromRequest(r *http.Request) gatewayproxy.AuthCtx {
	tp, _ := r.Context().Value(ctxKeyTraceparent).(string)
	ac := gatewayproxy.AuthCtx{
		Bearer:         bearerToken(r),
		Traceparent:    tp,
		TenantID:       r.Header.Get("X-Tenant-Id"),
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
	}
	if mc, ok := MeshClaimsFromContext(r.Context()); ok {
		ac.GCID = mc.GCID
		if mc.TenantID != "" {
			ac.TenantID = mc.TenantID
		}
		ac.RoleSummary = mc.RoleSummary
		// Bucket 4 (B6.1, 2026-05-16) — typed Roles[] from the validated JWT
		// propagate to downstream services via `x-mesh-user-roles`. chora-
		// identity's searchTenantMembers role gate (TRAINING_ADMIN /
		// TENANT_ADMIN) reads this header directly.
		if len(mc.Roles) > 0 {
			ac.Roles = append([]string(nil), mc.Roles...)
		}
	}
	// D1.5-style defensive fallback: identity-needing routes MUST carry a
	// GCID + TenantID downstream. RequireChoraSessionJWT stamps BOTH
	// MeshClaims and the raw ChoraSession claims; if for any route-ordering
	// reason only the raw claims landed, recover from them.
	if ac.GCID == "" || ac.TenantID == "" {
		if cs, ok := ChoraSessionClaimsFromContext(r.Context()); ok {
			if ac.GCID == "" {
				ac.GCID = cs.GCID
			}
			if ac.TenantID == "" {
				ac.TenantID = cs.TenantID
			}
			// Same fallback for Roles — ChoraSession claims carry the typed
			// roles list when mesh-claim middleware ordering elided them.
			if len(ac.Roles) == 0 && len(cs.Roles) > 0 {
				ac.Roles = append([]string(nil), cs.Roles...)
			}
		}
	}
	return ac
}

// writeGatewayProxyResp emits the aggregator Response with canonical
// Content-Type / Cache-Control headers.
func writeGatewayProxyResp(w http.ResponseWriter, resp gatewayproxy.Response) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if resp.Status == 0 {
		resp.Status = http.StatusInternalServerError
	}
	w.WriteHeader(resp.Status)
	if len(resp.Body) > 0 {
		_, _ = w.Write(resp.Body)
	}
}

// -----------------------------------------------------------------------------
// Open-course flow / ask (b) (2026-05-26) — chora-consumption course-bound
// LearningPath read for the A+ course-learn page.
//
// GET /api/v1/me/learning-paths[?course_id={id}]
//
// Forward path + raw query verbatim to the aggregator's MeLearningPaths
// which targets chora-consumption /v1/me/learning-paths (path translated).
// The downstream me_handlers branches on ?course_id and returns either the
// hydrated single-path shape (200 with atoms[]) or 404 PATH_NOT_BOOTSTRAPPED
// when the enrollment.created.v1 subscriber has not yet bootstrapped the
// path (FE poller retries until MAX_POLL_ATTEMPTS exhausts).
// -----------------------------------------------------------------------------

func (h *GatewayProxyHandler) handleMeLearningPaths(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/v1/me/learning-paths")
		return
	}
	resp, _ := h.agg.MeLearningPaths(r.Context(), gatewayProxyAuthFromRequest(r), r.URL.RawQuery)
	writeGatewayProxyResp(w, resp)
}

// -----------------------------------------------------------------------------
// W6 (2026-07-09) — StudentTranscript read-model (Four-Mode plan "Outcome
// spine"). Two exact GET-only leaves, verbatim proxy to chora-consumption's
// transcript_handler.go:
//
//	GET /api/v1/me/transcript[?limit=]
//	GET /api/v1/transcript/by-assessments?assessment_ids=a,b,c
//
// Self-scoped vs role-gated enforcement is delegated entirely to the
// downstream — the gateway forwards the mesh-trust headers (X-Tenant-Id +
// lowercase gcid + x-mesh-user-roles) gatewayProxyAuthFromRequest already
// resolves; it does not re-derive or re-check identity/role here.
// -----------------------------------------------------------------------------

func (h *GatewayProxyHandler) handleMeTranscript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/v1/me/transcript")
		return
	}
	resp, _ := h.agg.MeTranscript(r.Context(), gatewayProxyAuthFromRequest(r), r.URL.RawQuery)
	writeGatewayProxyResp(w, resp)
}

// handleMeTranscriptEntryAction serves the transcript entry-action subtree,
// today just POST /api/v1/me/transcript/{entryID}:seen (UX refactor B6 item 2
// follow-up). Verbatim proxy to chora-consumption /v1/me/transcript/{...}.
//
// No method check and no suffix check here on purpose. chora-consumption's
// handleMeTranscriptEntryAction already 404s an unknown action and 405s a
// non-POST, and it is the only place the ":seen" rule is written down. A
// second copy in the bridge would be one more thing to keep in step, and the
// failure mode of drift is a silent 404 on a write the learner can see did
// not take effect.
//
// The learner is NOT derived here: the downstream reads the verified gcid off
// the mesh headers call() stamps, so the entry id in the path cannot be used
// to mark somebody else's result as read.
func (h *GatewayProxyHandler) handleMeTranscriptEntryAction(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	resp, _ := h.agg.MeTranscriptEntryAction(
		r.Context(), gatewayProxyAuthFromRequest(r), r.Method, r.URL.Path, r.URL.RawQuery, body)
	writeGatewayProxyResp(w, resp)
}

func (h *GatewayProxyHandler) handleTranscriptByAssessments(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/v1/transcript/by-assessments")
		return
	}
	resp, _ := h.agg.TranscriptByAssessments(r.Context(), gatewayProxyAuthFromRequest(r), r.URL.RawQuery)
	writeGatewayProxyResp(w, resp)
}

// CHO-1612 — A+ open-course heterogeneous curriculum read.
//
// GET /api/v1/me/courses/{course_id}/content → chora-consumption
// /v1/me/courses/{course_id}/content (path translated). Returns the ordered
// {course_id, items[]} curriculum projected from
// chora.delivery.course.content_composed.v1.
func (h *GatewayProxyHandler) handleMeCourseContent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/v1/me/courses/{id}/content")
		return
	}
	resp, _ := h.agg.MeCourseContent(r.Context(), gatewayProxyAuthFromRequest(r), r.URL.Path)
	writeGatewayProxyResp(w, resp)
}

// handleGrowthEdges serves GET /api/v1/me/growth-edges (list; filter/sort/page
// query forwarded verbatim) — Epic-1b W8 A+ Growth Edges. Verbatim proxy to
// chora-consumption /v1/me/growth-edges.
func (h *GatewayProxyHandler) handleGrowthEdges(w http.ResponseWriter, r *http.Request) {
	resp, _ := h.agg.ProxyGrowthEdges(r.Context(), gatewayProxyAuthFromRequest(r),
		r.Method, r.URL.Path, r.URL.RawQuery, nil, r.Header.Get("Content-Type"))
	writeGatewayProxyResp(w, resp)
}

// handleGrowthEdgesSubpath serves the /api/v1/me/growth-edges/ subtree:
//
//	GET/DELETE /api/v1/me/growth-edges/{id}          read / dismiss one edge
//	POST       /api/v1/me/growth-edges/uploads       multipart upload (202)
//	GET        /api/v1/me/growth-edges/uploads/{id}  poll the async analysis job
//
// Body read with the 50 MiB readQuestionJobBody cap so the <=32 MiB multipart
// upload flows through (readBody's 1 MiB cap would truncate it). Content-Type
// forwarded verbatim so the multipart boundary survives. The downstream
// chora-consumption handler owns the route table + validates upload_kind/size.
// handlePracticeBudget serves GET /api/v1/me/practice-budget (B6 item 3):
// the learner's remaining practice taps for the current UTC day. Verbatim
// proxy to chora-consumption /v1/me/practice-budget; no body on a GET, and
// the learner identity travels only as stamped mesh claims.
func (h *GatewayProxyHandler) handlePracticeBudget(w http.ResponseWriter, r *http.Request) {
	resp, _ := h.agg.ProxyPracticeBudget(r.Context(), gatewayProxyAuthFromRequest(r),
		r.Method, r.URL.Path, r.URL.RawQuery)
	writeGatewayProxyResp(w, resp)
}

func (h *GatewayProxyHandler) handleGrowthEdgesSubpath(w http.ResponseWriter, r *http.Request) {
	body := readQuestionJobBody(r)
	resp, _ := h.agg.ProxyGrowthEdges(r.Context(), gatewayProxyAuthFromRequest(r),
		r.Method, r.URL.Path, r.URL.RawQuery, body, r.Header.Get("Content-Type"))
	writeGatewayProxyResp(w, resp)
}

// handleGoals serves the exact /api/v1/me/goals leaf — ADR-204 learner-owned
// Goal (A+ Discovery). Verbatim proxy to chora-consumption /v1/me/goals:
//
//	GET  /api/v1/me/goals   list {items, primaryLens}  (filter query forwarded)
//	POST /api/v1/me/goals   create (JSON GoalDTO body)
//
// The GET list carries no body (filter query only); the POST create carries a
// small JSON GoalDTO. The body is read only for the body-bearing methods so a
// GET mirrors the growth-edges leaf (nil body) exactly. readBody's 1 MiB cap is
// the right bound for the small JSON GoalDTO (this is JSON, not a multipart
// upload — the growth-edges subpath's 50 MiB readQuestionJobBody is for files).
func (h *GatewayProxyHandler) handleGoals(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Method != http.MethodGet {
		body = readBody(r)
	}
	resp, _ := h.agg.ProxyGoals(r.Context(), gatewayProxyAuthFromRequest(r),
		r.Method, r.URL.Path, r.URL.RawQuery, body, r.Header.Get("Content-Type"))
	writeGatewayProxyResp(w, resp)
}

// handleGoalsSubpath serves the /api/v1/me/goals/ subtree:
//
//	PATCH /api/v1/me/goals/{id}   update (status change / companion attach-detach)
//
// Verbatim proxy to chora-consumption /v1/me/goals/{id} (path translated). The
// JSON patch body is forwarded verbatim; the downstream goals_handler owns the
// route table + the "own goal only" gate (gcid derived from the stamped mesh
// claims, never the body). Mirrors the growth-edges subpath, with readBody's
// 1 MiB JSON cap (goals carry small JSON patches, not multipart uploads).
func (h *GatewayProxyHandler) handleGoalsSubpath(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	resp, _ := h.agg.ProxyGoals(r.Context(), gatewayProxyAuthFromRequest(r),
		r.Method, r.URL.Path, r.URL.RawQuery, body, r.Header.Get("Content-Type"))
	writeGatewayProxyResp(w, resp)
}

// handleDosePreferences serves the exact /api/v1/me/dose-preferences leaf —
// A+ learner Dose Preferences (CHO-2045 / ADR-224). Verbatim proxy to
// chora-consumption /v1/me/dose-preferences:
//
//	GET /api/v1/me/dose-preferences   read the excluded-map set
//	PUT /api/v1/me/dose-preferences   set one map's include/exclude (JSON body)
//
// GET carries nil body (mirrors handleGoals); PUT's small JSON payload fits
// readBody's 1 MiB cap. Learner scoping is delegated downstream — chora-consumption
// reads X-Tenant-Id + lowercase gcid off the stamped mesh claims.
func (h *GatewayProxyHandler) handleDosePreferences(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Method != http.MethodGet {
		body = readBody(r)
	}
	resp, _ := h.agg.ProxyDosePreferences(r.Context(), gatewayProxyAuthFromRequest(r),
		r.Method, r.URL.Path, r.URL.RawQuery, body, r.Header.Get("Content-Type"))
	writeGatewayProxyResp(w, resp)
}

// handleAITransparency serves the exact /api/v1/me/ai-transparency leaf —
// ADR-225 AI Transparency Notice. GET-only; verbatim proxy to chora-governance
// /v1/me/ai-transparency (returns must_acknowledge + the versioned, age- and
// locale-appropriate disclosure copy). Learner scoping is delegated downstream —
// chora-governance reads X-Tenant-Id + lowercase gcid off the stamped mesh claims.
func (h *GatewayProxyHandler) handleAITransparency(w http.ResponseWriter, r *http.Request) {
	resp, _ := h.agg.ProxyAITransparency(r.Context(), gatewayProxyAuthFromRequest(r),
		r.Method, r.URL.Path, r.URL.RawQuery, nil, r.Header.Get("Content-Type"))
	writeGatewayProxyResp(w, resp)
}

// handleAITransparencyAcknowledge serves the exact
// /api/v1/me/ai-transparency/acknowledge leaf — ADR-225. POST only; the small
// JSON body ({disclosureVersion?, surface, scope, firstShownAt?, locale?}) fits
// readBody's 1 MiB cap. Verbatim proxy to chora-governance
// /v1/me/ai-transparency/acknowledge (records the evidence row + emits the
// acknowledged event). Learner scoping is delegated downstream.
func (h *GatewayProxyHandler) handleAITransparencyAcknowledge(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Method != http.MethodGet {
		body = readBody(r)
	}
	resp, _ := h.agg.ProxyAITransparency(r.Context(), gatewayProxyAuthFromRequest(r),
		r.Method, r.URL.Path, r.URL.RawQuery, body, r.Header.Get("Content-Type"))
	writeGatewayProxyResp(w, resp)
}

// handleMaps serves the exact /api/v1/me/maps leaf — the My Knowledge Atlas
// (WS-B, map = Goal). GET-only verbatim proxy to chora-consumption /v1/me/maps
// (lists the learner's maps + per-map concept/shaky/mastered counts). Non-GET
// carries no body; the downstream 405s anything but GET.
func (h *GatewayProxyHandler) handleMaps(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Method != http.MethodGet {
		body = readBody(r)
	}
	resp, _ := h.agg.ProxyMaps(r.Context(), gatewayProxyAuthFromRequest(r),
		r.Method, r.URL.Path, r.URL.RawQuery, body, r.Header.Get("Content-Type"))
	writeGatewayProxyResp(w, resp)
}

// handleMapsSubpath serves the /api/v1/me/maps/ subtree — GET
// /api/v1/me/maps/{goalId}/graph (the map's root-scoped painted subgraph).
// GET-only verbatim proxy; the downstream router does the leaf dispatch.
func (h *GatewayProxyHandler) handleMapsSubpath(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Method != http.MethodGet {
		body = readBody(r)
	}
	resp, _ := h.agg.ProxyMaps(r.Context(), gatewayProxyAuthFromRequest(r),
		r.Method, r.URL.Path, r.URL.RawQuery, body, r.Header.Get("Content-Type"))
	writeGatewayProxyResp(w, resp)
}

// handleConceptGraph serves the exact /api/v1/me/concept-graph leaf — the
// learner-sovereign Discovery Knowledge Graph (A+ Discovery). Verbatim proxy to
// chora-consumption /v1/me/concept-graph:
//
//	GET /api/v1/me/concept-graph   list {concepts, edges, rootConceptId}  (query forwarded)
//
// The body is read only for body-bearing methods so a GET mirrors the goals
// leaf (nil body) exactly. readBody's 1 MiB cap is the right bound for the small
// JSON payloads the concept-graph carries (JSON, not multipart uploads). The
// collection-level mutations (reroot / concepts / edges) live under the
// /api/v1/me/concept-graph/ subtree; this exact leaf only serves the GET list,
// but forwarding verbatim keeps the handler symmetric with handleGoals (the
// downstream 405s any non-GET on the bare leaf).
func (h *GatewayProxyHandler) handleConceptGraph(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Method != http.MethodGet {
		body = readBody(r)
	}
	resp, _ := h.agg.ProxyConceptGraph(r.Context(), gatewayProxyAuthFromRequest(r),
		r.Method, r.URL.Path, r.URL.RawQuery, body, r.Header.Get("Content-Type"))
	writeGatewayProxyResp(w, resp)
}

// handleConceptGraphSubpath serves the /api/v1/me/concept-graph/ subtree:
//
//	POST   /api/v1/me/concept-graph/reroot        re-root the hierarchy
//	POST   /api/v1/me/concept-graph/concepts      create a concept (empty-ok)
//	PATCH  /api/v1/me/concept-graph/concepts/{id} rename / re-parent
//	DELETE /api/v1/me/concept-graph/concepts/{id} remove a concept
//	POST   /api/v1/me/concept-graph/edges         add an edge
//	DELETE /api/v1/me/concept-graph/edges/{id}    remove an edge
//
// Verbatim proxy to chora-consumption /v1/me/concept-graph/... (path
// translated). The gateway forwards method + path + body + query VERBATIM and
// does NOT enumerate the leaves — chora-consumption's own router does the real
// sub-route dispatch + the "own graph only" gate (gcid derived from the stamped
// mesh claims, never the body). Mirrors handleGoalsSubpath, with readBody's 1
// MiB JSON cap (concept-graph carries small JSON payloads, not multipart
// uploads); DELETE requests carry no body (readBody returns empty).
func (h *GatewayProxyHandler) handleConceptGraphSubpath(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	resp, _ := h.agg.ProxyConceptGraph(r.Context(), gatewayProxyAuthFromRequest(r),
		r.Method, r.URL.Path, r.URL.RawQuery, body, r.Header.Get("Content-Type"))
	writeGatewayProxyResp(w, resp)
}

// -----------------------------------------------------------------------------
// CHO-1618 (2026-06-01) — Personal Collections — verbatim proxy to
// chora-creation at the SAME path. 7 verbatim-proxy routes across 3 handlers,
// plus 1 gateway-side search route (WS-8):
//
//	POST   /api/v1/collections                              handleCollectionsCollection
//	GET    /api/v1/me/collections                           handleListMyCollections
//	GET    /api/v1/collections/search                       handleCollectionsSearch  (gateway-side filter; NOT proxied verbatim)
//	GET    /api/v1/collections/{id}                         handleCollectionsSubpath
//	PATCH  /api/v1/collections/{id}                         handleCollectionsSubpath
//	DELETE /api/v1/collections/{id}                         handleCollectionsSubpath
//	POST   /api/v1/collections/{id}/atoms                   handleCollectionsSubpath
//	DELETE /api/v1/collections/{id}/atoms/{atomId}          handleCollectionsSubpath
//
// The downstream chora-creation collection_handler enforces tenant context +
// owner gate + RLS; the gateway just stamps the mesh-claim headers and
// forwards method + path + body verbatim. The /search route is the lone
// exception — it reuses the /me/collections read and filters in the gateway
// (no chora-creation change), so it ships with only a gateway redeploy.
// -----------------------------------------------------------------------------

// handleCollectionsCollection serves the collection root /api/v1/collections
// (no path suffix): POST createCollection. Other methods 405 — the canonical
// list is GET /api/v1/me/collections (caller-scoped), served separately.
func (h *GatewayProxyHandler) handleCollectionsCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only on /api/v1/collections")
		return
	}
	body := readBody(r)
	resp, _ := h.agg.CreateCollection(r.Context(), gatewayProxyAuthFromRequest(r), body)
	writeGatewayProxyResp(w, resp)
}

// handleListMyCollections serves GET /api/v1/me/collections — the caller's
// personal Collections (gcid + tenant scoped downstream). 405 on non-GET.
func (h *GatewayProxyHandler) handleListMyCollections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/v1/me/collections")
		return
	}
	resp, _ := h.agg.ListMyCollections(r.Context(), gatewayProxyAuthFromRequest(r))
	writeGatewayProxyResp(w, resp)
}

// handleCollectionsSearch serves GET /api/v1/collections/search?q=&limit= —
// the A+ search-hub collection tab (WS-8). Registered as an EXACT mux pattern
// so it takes precedence over the /api/v1/collections/ subtree dispatcher
// (which would otherwise parse "search" as a {collectionId} and 404). GET-only.
// The aggregator filters the caller's own collections by a case-insensitive
// title match; see SearchMyCollections for the MVP scope note.
func (h *GatewayProxyHandler) handleCollectionsSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/v1/collections/search")
		return
	}
	q := r.URL.Query().Get("q")
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 50 {
				n = 50
			}
			limit = n
		}
	}
	resp, _ := h.agg.SearchMyCollections(r.Context(), gatewayProxyAuthFromRequest(r), q, limit)
	writeGatewayProxyResp(w, resp)
}

// handleCollectionsSubpath dispatches the parametric leaves under
// /api/v1/collections/:
//
//	GET    /api/v1/collections/{id}                          getCollection
//	PATCH  /api/v1/collections/{id}                          patchCollection
//	DELETE /api/v1/collections/{id}                          deleteCollection
//	POST   /api/v1/collections/{id}/atoms                    addCollectionAtom
//	DELETE /api/v1/collections/{id}/atoms/{atomId}           removeCollectionAtom
//	POST   /api/v1/collections/{id}/convert-to-study-list    convertToStudyList
//
// {collectionId} + {atomId} are parsed exactly like the atom-revisions /
// instructor-roster handlers parse their path segments. Any other sub-path
// is 404. Method preservation is the verbatim contract — the downstream
// chora-creation collection_handler 405s unsupported methods, but the gateway
// dispatcher 405s eagerly so a misbehaving FE call doesn't waste a round-trip.
func (h *GatewayProxyHandler) handleCollectionsSubpath(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/collections/"), "/")
	if rest == "" {
		writeError(w, http.StatusNotFound, "GATEWAY_COLLECTION_ID_REQUIRED",
			"path must be /api/v1/collections/{id}[/atoms[/{atomId}]]")
		return
	}
	parts := strings.Split(rest, "/")
	collectionID := parts[0]
	if collectionID == "" {
		writeError(w, http.StatusNotFound, "GATEWAY_COLLECTION_ID_REQUIRED",
			"path must be /api/v1/collections/{id}[/atoms[/{atomId}]]")
		return
	}

	switch {
	// -------------------------------------------------------------------
	// /api/v1/collections/{id} — GET / PATCH / DELETE.
	// -------------------------------------------------------------------
	case len(parts) == 1 && r.Method == http.MethodGet:
		resp, _ := h.agg.GetCollection(r.Context(), gatewayProxyAuthFromRequest(r), collectionID)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 1 && r.Method == http.MethodPatch:
		body := readBody(r)
		resp, _ := h.agg.PatchCollection(r.Context(), gatewayProxyAuthFromRequest(r), collectionID, body)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 1 && r.Method == http.MethodDelete:
		resp, _ := h.agg.DeleteCollection(r.Context(), gatewayProxyAuthFromRequest(r), collectionID)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 1:
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET / PATCH / DELETE only on /api/v1/collections/{id}")

	// -------------------------------------------------------------------
	// /api/v1/collections/{id}/atoms — POST addCollectionAtom.
	// -------------------------------------------------------------------
	case len(parts) == 2 && parts[1] == "atoms" && r.Method == http.MethodPost:
		body := readBody(r)
		resp, _ := h.agg.AddCollectionAtom(r.Context(), gatewayProxyAuthFromRequest(r), collectionID, body)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 2 && parts[1] == "atoms":
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only on /api/v1/collections/{id}/atoms")

	// -------------------------------------------------------------------
	// /api/v1/collections/{id}/atoms/{atomId} — DELETE removeCollectionAtom.
	// -------------------------------------------------------------------
	case len(parts) == 3 && parts[1] == "atoms" && r.Method == http.MethodDelete:
		resp, _ := h.agg.RemoveCollectionAtom(r.Context(), gatewayProxyAuthFromRequest(r), collectionID, parts[2])
		writeGatewayProxyResp(w, resp)
	case len(parts) == 3 && parts[1] == "atoms":
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"DELETE only on /api/v1/collections/{id}/atoms/{atomId}")

	// -------------------------------------------------------------------
	// /api/v1/collections/{id}/convert-to-study-list — POST (WS-4, ADR-233).
	//
	// Verbatim passthrough to chora-creation, which owns the transition:
	// it re-evaluates the ADR-229 reuse disjunct PER ATOM against the
	// caller's gcid, mints the GRANT_SCOPE_COLLECTION grants, and emits the
	// study-list event. The Collection is NOT mutated (D9), so the route is
	// retryable. 201 carries a PARTIAL SUCCESS envelope whose excluded[]
	// names every dropped atom + reason (D11); 409 = zero survivors; 403 =
	// actor is not the owner. All pass through untouched.
	// -------------------------------------------------------------------
	case len(parts) == 2 && parts[1] == "convert-to-study-list" && r.Method == http.MethodPost:
		body := readBody(r)
		resp, _ := h.agg.ConvertCollectionToStudyList(r.Context(), gatewayProxyAuthFromRequest(r), collectionID, body)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 2 && parts[1] == "convert-to-study-list":
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only on /api/v1/collections/{id}/convert-to-study-list")

	default:
		writeError(w, http.StatusNotFound, "GATEWAY_ROUTE_NOT_FOUND",
			"only /api/v1/collections/{id}, /api/v1/collections/{id}/atoms, /api/v1/collections/{id}/atoms/{atomId}, and /api/v1/collections/{id}/convert-to-study-list are served here")
	}
}

// -----------------------------------------------------------------------------
// W3.B.1 (2026-06-28) — QuestionBank curation — verbatim proxy to chora-creation
// at the SAME path. 9 routes across 3 handlers. Mirrors the CHO-1618 Collections
// handlers EXACTLY:
//
//	POST   /api/v1/question-banks                            handleQuestionBanksCollection
//	GET    /api/v1/me/question-banks                         handleListMyQuestionBanks
//	GET    /api/v1/question-banks/{id}                       handleQuestionBanksSubpath
//	PATCH  /api/v1/question-banks/{id}                       handleQuestionBanksSubpath
//	DELETE /api/v1/question-banks/{id}                       handleQuestionBanksSubpath
//	POST   /api/v1/question-banks/{id}/questions             handleQuestionBanksSubpath
//	GET    /api/v1/question-banks/{id}/questions             handleQuestionBanksSubpath
//	DELETE /api/v1/question-banks/{id}/questions/{qid}       handleQuestionBanksSubpath
//	POST   /api/v1/question-banks/{id}/assemble-test-set     handleQuestionBanksSubpath
//
// The downstream chora-creation question_bank_handler enforces tenant context +
// owner gate + RLS; the gateway just stamps the mesh-claim headers and forwards
// method + path + body verbatim.
// -----------------------------------------------------------------------------

// handleQuestionBanksCollection serves the collection root
// /api/v1/question-banks (no path suffix): POST createQuestionBank. Other
// methods 405 — the canonical list is GET /api/v1/me/question-banks
// (caller-scoped), served separately.
func (h *GatewayProxyHandler) handleQuestionBanksCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only on /api/v1/question-banks")
		return
	}
	body := readBody(r)
	resp, _ := h.agg.CreateQuestionBank(r.Context(), gatewayProxyAuthFromRequest(r), body)
	writeGatewayProxyResp(w, resp)
}

// handleListMyQuestionBanks serves GET /api/v1/me/question-banks — the caller's
// QuestionBanks (gcid + tenant scoped downstream). 405 on non-GET.
func (h *GatewayProxyHandler) handleListMyQuestionBanks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/v1/me/question-banks")
		return
	}
	resp, _ := h.agg.ListMyQuestionBanks(r.Context(), gatewayProxyAuthFromRequest(r))
	writeGatewayProxyResp(w, resp)
}

// handleQuestionBanksSubpath dispatches the parametric leaves under
// /api/v1/question-banks/:
//
//	GET    /api/v1/question-banks/{id}                  getQuestionBank
//	PATCH  /api/v1/question-banks/{id}                  patchQuestionBank
//	DELETE /api/v1/question-banks/{id}                  deleteQuestionBank
//	POST   /api/v1/question-banks/{id}/questions        addQuestion
//	GET    /api/v1/question-banks/{id}/questions        listQuestions
//	DELETE /api/v1/question-banks/{id}/questions/{qid}  removeQuestion
//	POST   /api/v1/question-banks/{id}/assemble-test-set assembleTestSet
//
// {id} + {qid} are parsed exactly like the collections subpath handler. Any
// other sub-path is 404. Method preservation is the verbatim contract — the
// downstream chora-creation handler 405s unsupported methods, but the gateway
// dispatcher 405s eagerly so a misbehaving FE call doesn't waste a round-trip.
func (h *GatewayProxyHandler) handleQuestionBanksSubpath(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/question-banks/"), "/")
	if rest == "" {
		writeError(w, http.StatusNotFound, "GATEWAY_QUESTION_BANK_ID_REQUIRED",
			"path must be /api/v1/question-banks/{id}[/questions[/{qid}]|/assemble-test-set]")
		return
	}
	parts := strings.Split(rest, "/")
	id := parts[0]
	if id == "" {
		writeError(w, http.StatusNotFound, "GATEWAY_QUESTION_BANK_ID_REQUIRED",
			"path must be /api/v1/question-banks/{id}[/questions[/{qid}]|/assemble-test-set]")
		return
	}

	switch {
	// -------------------------------------------------------------------
	// /api/v1/question-banks/{id} — GET / PATCH / DELETE.
	// -------------------------------------------------------------------
	case len(parts) == 1 && r.Method == http.MethodGet:
		resp, _ := h.agg.GetQuestionBank(r.Context(), gatewayProxyAuthFromRequest(r), id)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 1 && r.Method == http.MethodPatch:
		body := readBody(r)
		resp, _ := h.agg.PatchQuestionBank(r.Context(), gatewayProxyAuthFromRequest(r), id, body)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 1 && r.Method == http.MethodDelete:
		resp, _ := h.agg.DeleteQuestionBank(r.Context(), gatewayProxyAuthFromRequest(r), id)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 1:
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET / PATCH / DELETE only on /api/v1/question-banks/{id}")

	// -------------------------------------------------------------------
	// /api/v1/question-banks/{id}/questions — POST add / GET list.
	// -------------------------------------------------------------------
	case len(parts) == 2 && parts[1] == "questions" && r.Method == http.MethodPost:
		body := readBody(r)
		resp, _ := h.agg.AddQuestionBankQuestion(r.Context(), gatewayProxyAuthFromRequest(r), id, body)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 2 && parts[1] == "questions" && r.Method == http.MethodGet:
		resp, _ := h.agg.ListQuestionBankQuestions(r.Context(), gatewayProxyAuthFromRequest(r), id, r.URL.RawQuery)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 2 && parts[1] == "questions":
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST / GET only on /api/v1/question-banks/{id}/questions")

	// -------------------------------------------------------------------
	// /api/v1/question-banks/{id}/assemble-test-set — POST.
	// -------------------------------------------------------------------
	case len(parts) == 2 && parts[1] == "assemble-test-set" && r.Method == http.MethodPost:
		body := readBody(r)
		resp, _ := h.agg.AssembleQuestionBankTestSet(r.Context(), gatewayProxyAuthFromRequest(r), id, body)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 2 && parts[1] == "assemble-test-set":
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only on /api/v1/question-banks/{id}/assemble-test-set")

	// -------------------------------------------------------------------
	// /api/v1/question-banks/{id}/reorder — POST (Phase-A.2 Wave 2).
	// -------------------------------------------------------------------
	case len(parts) == 2 && parts[1] == "reorder" && r.Method == http.MethodPost:
		body := readBody(r)
		resp, _ := h.agg.ReorderQuestionBank(r.Context(), gatewayProxyAuthFromRequest(r), id, body)
		writeGatewayProxyResp(w, resp)
	case len(parts) == 2 && parts[1] == "reorder":
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"POST only on /api/v1/question-banks/{id}/reorder")

	// -------------------------------------------------------------------
	// /api/v1/question-banks/{id}/questions/{qid} — DELETE removeQuestion.
	// -------------------------------------------------------------------
	case len(parts) == 3 && parts[1] == "questions" && r.Method == http.MethodDelete:
		resp, _ := h.agg.RemoveQuestionBankQuestion(r.Context(), gatewayProxyAuthFromRequest(r), id, parts[2])
		writeGatewayProxyResp(w, resp)
	case len(parts) == 3 && parts[1] == "questions":
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"DELETE only on /api/v1/question-banks/{id}/questions/{qid}")

	default:
		writeError(w, http.StatusNotFound, "GATEWAY_ROUTE_NOT_FOUND",
			"only /api/v1/question-banks/{id}, /api/v1/question-banks/{id}/questions, /api/v1/question-banks/{id}/questions/{qid}, and /api/v1/question-banks/{id}/assemble-test-set are served here")
	}
}

// _ keeps the servicemesh import alive so future signed-mesh-claims helpers
// added to gatewayProxyAuthFromRequest do not need a separate import bump
// (mirrors notifications_handler.go + kg_explore_handler.go).
var _ = servicemesh.MarshalToHeaders
