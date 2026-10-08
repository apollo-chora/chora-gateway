// Package gatewayproxy proxies the A6 batch of non-auth /api/* BFF routes
// through to their downstream domain services. It closes the gap reported
// in docs/m13/handoff-fe-to-be-service-2026-05-14.md §A6: FE probed each
// route with a real Bearer ChoraSession JWT and found most non-auth
// /api/* routes return GATEWAY_ROUTE_NOT_FOUND — they were not routed at
// the gateway at all.
//
// Each route maps to the REAL downstream path (verified by reading each
// downstream service's HTTP adapter — the FE-facing path differs from the
// downstream-served path):
//
//	GET  /api/feature-flags                → chora-tenancy   GET /api/tenants/{tenantID}/entitlements
//	GET  /api/tenants/{id}                 → chora-tenancy   GET /api/tenants/{id}
//	GET  /api/courses/{id}                 → chora-delivery  GET /v1/courses/{id}
//	POST /api/courses/{id}/enrol           → chora-delivery  POST /v1/courses/{id}/enrolments
//	GET  /api/me/knowledge-graph/clusters  → chora-consumption GET /v1/me/knowledge-graph/clusters
//	GET  /api/me/companions                 → chora-consumption GET /v1/me/companions
//	GET  /api/me/companions/{id}/growth     → chora-consumption GET /v1/me/companions/{id}/growth
//	GET  /api/notifications                → chora-notifications GET /api/notifications
//	GET  /api/v1/me/mana                   → chora-identity GET /api/v1/me/mana            (A17)
//	POST /api/v1/me/mana/topup             → chora-identity POST /api/v1/me/mana/topup     (A17)
//	POST /api/v1/me/mana/demo-grant       → chora-identity POST /api/v1/me/mana/demo-grant (demo only)
//
// Wire contract:
//   - Bearer JWT enforced by chora-gateway's RequireChoraSessionJWT
//     middleware before the request reaches this aggregator (the BFF
//     handler's prefixes are added to DefaultJWTGatedPrefixes). AuthCtx
//     fields are populated by the BFF handler from validated mesh claims.
//   - Outbound calls stamp Authorization, traceparent, X-Tenant-Id, the
//     lowercase `gcid` header (chora-consumption requireContext,
//     chora-notifications tenantContext, and chora-delivery's v1 enrolment
//     handler all read X-Tenant-Id + lowercase gcid — both are mandatory
//     or the downstream 4xx MISSING_CONTEXT / "gcid required"), plus the
//     canonical mesh metadata via servicemesh.MarshalToHeaders
//     (chora-gcid / chora-tenant-id / chora-role-summary) for services on
//     the mesh-claims path.
//   - 5xx → 502 GATEWAY_UPSTREAM_5XX. Timeout → 504 GATEWAY_UPSTREAM_TIMEOUT.
//     2xx + 4xx pass through verbatim — a downstream 4xx (e.g. a domain
//     404) proves the request reached the downstream handler and must
//     surface unchanged.
//
// SVC_*_URL is read at cmd/server boot per memory feedback_no_inline_config
// — never inline. The BFF handler skips registration when ALL URLs are
// empty so the routes remain 404 in unconfigured envs (clearer signal than
// a 502 fan-out failure on every request).
package gatewayproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
)

// DefaultPerCallTimeout caps each downstream call. The A6 routes are cheap
// (single DB lookup / in-memory read on the downstream); 6s is generous
// headroom over the downstream SLOs.
const DefaultPerCallTimeout = 6 * time.Second

// DefaultPaymentsStreamTimeout caps the long-lived chora-payments admin SSE
// connection. Matches the companionbridge ChatStream budget (60s) — proxies +
// GCLB tolerate a 60s idle deadline; the FE EventSource auto-reconnects on
// the configured retry interval emitted by the upstream.
const DefaultPaymentsStreamTimeout = 60 * time.Second

// DefaultPostCreateTimeout caps the POST /api/posts → chora-sharing call. The
// downstream Moderation gate (P6 reflection) runs ~16s warm; 50s covers a cold
// model-fallback hop while staying under the 60s httproute/GCLB ceiling.
const DefaultPostCreateTimeout = 50 * time.Second

// Config wires the downstream service base URLs + per-call timeout. URLs
// are sourced from env via LoadConfigFromEnv per feedback_no_inline_config.
type Config struct {
	// TenancyURL is the chora-tenancy service base (feature-flags + tenant
	// detail). Empty disables those routes.
	TenancyURL string

	// DeliveryURL is the chora-delivery service base (course detail +
	// enrol). Empty disables those routes.
	DeliveryURL string

	// ConsumptionURL is the chora-consumption service base (KG clusters +
	// companions roster + companion growth). Empty disables those routes.
	ConsumptionURL string

	// NotificationsURL is the chora-notifications service base. Empty
	// disables the notifications route.
	NotificationsURL string

	// IdentityURL is the chora-identity service base (A17 — mana wallet +
	// top-up: /api/v1/me/mana and /api/v1/me/mana/topup). Empty disables
	// those routes.
	IdentityURL string

	// CreationURL is the chora-creation service base (P7 — manual question
	// authoring: /api/atoms/{atom_id}/questions[...]). Empty disables those
	// routes. chora-creation serves the routes at the SAME path the FE
	// hits — pure verbatim proxy, no rewrite.
	CreationURL string

	// PaymentsURL is the chora-payments service base (H+ admin tx-history —
	// /api/v1/admin/payments/{purchases,export,stream,{purchase_id}/refund}).
	// Empty disables those routes. Per ADR-164 chora-payments serves the admin
	// REST surface at the SAME paths the FE hits — pure verbatim proxy.
	// SSE-streaming endpoint lives in payments_stream.go (separate file because
	// it bypasses the call/classify helpers to flush each frame to the FE).
	PaymentsURL string

	// SharingURL is the chora-sharing service base (C+ ChoraCircle post-create:
	// POST /api/posts → the synchronous Moderation gate). Empty disables the
	// route. The FE composer sends {content, hashtags}; the bridge translates
	// to chora-sharing's {body, tags, visibility} createPostReq shape.
	SharingURL string

	// GovernanceURL is the chora-governance service base (ADR-225 — learner-self
	// AI Transparency Notice: GET /api/v1/me/ai-transparency read +
	// POST .../acknowledge). Empty disables those routes. chora-governance
	// serves them at /v1/me/ai-transparency[/acknowledge] (the /api prefix is
	// stripped, mirroring ProxyGoals).
	GovernanceURL string

	// PerCallTimeout caps each downstream call. Defaults to DefaultPerCallTimeout.
	PerCallTimeout time.Duration

	// PaymentsStreamTimeout caps the SSE proxy upstream connection.
	// Defaults to DefaultPaymentsStreamTimeout (60s) which matches the
	// companionbridge ChatStream budget.
	PaymentsStreamTimeout time.Duration

	// PostCreateTimeout caps the POST /api/posts → chora-sharing call. The
	// downstream runs a SYNCHRONOUS Moderation crew gate (P6 reflection,
	// 2 LLM rounds ≈ 16s warm, longer on cold model fallback), so the default
	// 6s PerCallTimeout would 504 every post. Defaults to
	// DefaultPostCreateTimeout (50s) — covers the gate + headroom while staying
	// under the 60s httproute/GCLB ceiling and the 45s server WriteTimeout is
	// matched on chora-sharing.
	PostCreateTimeout time.Duration
}

// ApplyDefaults sets the default per-call timeout when zero.
func (c *Config) ApplyDefaults() {
	if c.PerCallTimeout == 0 {
		c.PerCallTimeout = DefaultPerCallTimeout
	}
	if c.PaymentsStreamTimeout == 0 {
		c.PaymentsStreamTimeout = DefaultPaymentsStreamTimeout
	}
	if c.PostCreateTimeout == 0 {
		c.PostCreateTimeout = DefaultPostCreateTimeout
	}
}

// LoadConfigFromEnv reads the SVC_*_URL env vars per feedback_no_inline_config.
// These are the SAME env vars the existing aggregators already read — no new
// deployment config is introduced. SVC_CREATION_URL is the same var the
// existing chora-creation upstream client + HTTPUpstream config reads.
func LoadConfigFromEnv() Config {
	c := Config{
		TenancyURL:       os.Getenv("SVC_TENANCY_URL"),
		DeliveryURL:      os.Getenv("SVC_DELIVERY_URL"),
		ConsumptionURL:   os.Getenv("SVC_CONSUMPTION_URL"),
		NotificationsURL: os.Getenv("SVC_NOTIFICATIONS_URL"),
		IdentityURL:      os.Getenv("SVC_IDENTITY_URL"),
		CreationURL:      os.Getenv("SVC_CREATION_URL"),
		// C+ ChoraCircle post-create (POST /api/posts → chora-sharing Moderation
		// gate). Same SVC_SHARING_URL the social aggregator already reads — no new
		// deployment config (feedback_no_inline_config).
		SharingURL: os.Getenv("SVC_SHARING_URL"),
		// ADR-225 AI Transparency Notice — chora-governance learner-self routes.
		// SVC_GOVERNANCE_URL is the SAME var the governance upstream already
		// reads (no new deployment config; feedback_no_inline_config).
		GovernanceURL: os.Getenv("SVC_GOVERNANCE_URL"),
		// H+ tx-history (Phase 2 Agent A2, 2026-05-26) — chora-payments admin
		// REST surface for ListPurchaseHistory / IssuePaymentRefund /
		// ExportPurchases / StreamPaymentEvents. Reuses CHORA_PAYMENTS_HTTP_ADDR
		// (already plumbed for the Stripe webhook passthrough in
		// chora-infra/k8s/services/chora-gateway/deployment.yaml). Defaults to
		// the in-cluster K8s DNS name when unset so dev pods boot without
		// explicit wiring.
		PaymentsURL: paymentsURLOrDefault(os.Getenv("CHORA_PAYMENTS_HTTP_ADDR")),
	}
	c.ApplyDefaults()
	return c
}

// paymentsURLOrDefault falls back to the canonical in-cluster K8s DNS name
// when CHORA_PAYMENTS_HTTP_ADDR is unset. Mirrors the K8s ns/service convention.
func paymentsURLOrDefault(envVal string) string {
	if envVal != "" {
		return envVal
	}
	return "http://chora-payments.payments.svc.cluster.local"
}

// AuthCtx carries the per-request mesh-trust + tracing values stamped on
// outbound calls. Populated by the BFF handler from validated JWT claims.
//
// IdempotencyKey (A17) carries the FE-supplied Idempotency-Key header for
// POST routes that the contract marks idempotent (e.g. /api/v1/me/mana/topup
// per learner-economy.yaml:482). Optional — stamped only when non-empty so
// the downstream's idempotency-fallback synthesis remains the default path
// when the FE omits the header.
type AuthCtx struct {
	Bearer         string
	Traceparent    string
	TenantID       string
	GCID           string
	RoleSummary    map[string]any
	IdempotencyKey string

	// Roles (Bucket 4, 2026-05-14) carries the user's typed role set in the
	// active tenant (e.g. ["TENANT_ADMIN", "INSTRUCTOR"]). Propagated to
	// downstream services via the canonical `x-mesh-user-roles` mTLS-bound
	// header so role gates (e.g. searchTenantMembers TRAINING_ADMIN /
	// TENANT_ADMIN) can fast-fail without parsing the legacy RoleSummary
	// JSON envelope. Empty/nil when the session JWT carries no typed roles.
	Roles []string
}

// Response is the normalised aggregator output forwarded to the FE.
type Response struct {
	Status  int
	Headers http.Header
	Body    []byte
}

// Aggregator is the stateless A6 BFF proxy.
type Aggregator struct {
	cfg    Config
	client *http.Client
}

// New constructs an Aggregator. Returns nil when ALL downstream URLs are
// unset so callers can route-skip in unconfigured envs.
func New(cfg Config) *Aggregator {
	cfg.ApplyDefaults()
	if cfg.TenancyURL == "" && cfg.DeliveryURL == "" &&
		cfg.ConsumptionURL == "" && cfg.NotificationsURL == "" &&
		cfg.IdentityURL == "" && cfg.CreationURL == "" &&
		cfg.PaymentsURL == "" && cfg.SharingURL == "" &&
		cfg.GovernanceURL == "" {
		return nil
	}
	// http.Client.Timeout is the hard upper bound across all routes. Short A6
	// routes still cancel at their per-call CONTEXT deadline (PerCallTimeout)
	// inside callWithTimeout, so sizing the client to the LONGEST non-stream
	// route (POST /api/posts Moderation gate) is safe — it never relaxes the
	// short-call budget. The SSE proxy path bypasses this client
	// (payments_stream.go builds its own request with its own context deadline).
	clientTimeout := cfg.PerCallTimeout
	if cfg.PostCreateTimeout > clientTimeout {
		clientTimeout = cfg.PostCreateTimeout
	}
	return &Aggregator{
		cfg:    cfg,
		client: &http.Client{Timeout: clientTimeout + 1*time.Second},
	}
}

// -----------------------------------------------------------------------------
// HTTP plumbing
// -----------------------------------------------------------------------------

type callResult struct {
	status int
	body   []byte
	header http.Header
	err    error
}

// call performs one outbound request with mesh-trust headers stamped,
// honouring PerCallTimeout.
func (a *Aggregator) call(ctx context.Context, method, urlStr string, body []byte, auth AuthCtx) callResult {
	return a.callWithTimeout(ctx, method, urlStr, body, auth, a.cfg.PerCallTimeout)
}

// callWithTimeout is call() with an explicit per-call deadline. Routes whose
// downstream is slow (e.g. POST /api/posts → the synchronous Moderation gate)
// pass a longer timeout than the default PerCallTimeout.
func (a *Aggregator) callWithTimeout(ctx context.Context, method, urlStr string, body []byte, auth AuthCtx, timeout time.Duration) callResult {
	if urlStr == "" {
		return callResult{err: fmt.Errorf("gatewayproxy: empty url for %s", method)}
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(callCtx, method, urlStr, bodyReader)
	if err != nil {
		return callResult{err: fmt.Errorf("gatewayproxy: build request: %w", err)}
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+auth.Bearer)
	}
	if auth.Traceparent != "" {
		req.Header.Set("traceparent", auth.Traceparent)
	}
	if auth.TenantID != "" {
		req.Header.Set("X-Tenant-Id", auth.TenantID)
	}
	if auth.GCID != "" {
		req.Header.Set("X-Chora-GCID", auth.GCID)
		// chora-consumption requireContext, chora-notifications tenantContext,
		// chora-delivery v1 enrolment create, and chora-tenancy's gcid reads
		// all expect the LOWERCASE `gcid` header. Stamp it explicitly so the
		// downstreams' existing header convention is preserved — without it
		// they 4xx MISSING_CONTEXT / "gcid required". Mirrors the kgexplore +
		// notifications + phyllis aggregators' established pattern.
		req.Header.Set("gcid", auth.GCID)
	}
	// A17 — forward the FE Idempotency-Key when supplied. Per
	// learner-economy.yaml:406-413 the header is optional + maxLength 64;
	// chora-identity's topupMana handler synthesises a fallback key from the
	// Stripe payment_intent_id when absent, so the gateway MUST NOT stamp an
	// empty-string header (it would override the downstream fallback path).
	if auth.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", auth.IdempotencyKey)
	}
	// Canonical mesh-trust headers (chora-gcid / chora-tenant-id /
	// chora-role-summary / x-mesh-user-roles) for services on the mesh-claims
	// path. Bucket 4 (B6.1, 2026-05-16) — Roles []string propagates the
	// typed role set so downstream role gates (e.g. chora-identity
	// searchTenantMembers TRAINING_ADMIN / TENANT_ADMIN) can fast-fail
	// without parsing the legacy RoleSummary JSON envelope.
	mesh := servicemesh.MarshalToHeaders(servicemesh.MeshClaims{
		GCID:        auth.GCID,
		TenantID:    auth.TenantID,
		Roles:       auth.Roles,
		RoleSummary: auth.RoleSummary,
	})
	for k, vs := range mesh {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return callResult{err: err}
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return callResult{status: resp.StatusCode, body: out, header: resp.Header}
}

// classify converts a callResult into a Response. 5xx → 502; timeout → 504;
// 2xx + 4xx pass through verbatim (a downstream handler 4xx proves the
// request reached the downstream).
//
// EXCEPT for 501 + 503 — both are intentional contract codes the downstream
// emits to communicate semantics the FE must render:
//   - 501 NOT_IMPLEMENTED — reserved question_type / job type per the
//     question authoring contract.
//   - 503 CREATION_JOBS_NOT_WIRED — fail-loud envelope chora-creation emits
//     when chora-identity gRPC isn't wired (mana client absent). FE renders
//     a "configure tenant" upsell rather than the generic gateway 5xx.
//
// 500 / 502 / 504 still normalise to 502 GATEWAY_UPSTREAM_5XX because those
// indicate genuine upstream failure (panic / proxy crash / read timeout).
func classify(cr callResult) Response {
	if cr.err != nil {
		if errors.Is(cr.err, context.DeadlineExceeded) {
			return errResp(http.StatusGatewayTimeout, "GATEWAY_UPSTREAM_TIMEOUT", cr.err.Error())
		}
		if errors.Is(cr.err, context.Canceled) {
			return errResp(http.StatusGatewayTimeout, "GATEWAY_REQUEST_CANCELED", cr.err.Error())
		}
		var urlErr *url.Error
		if errors.As(cr.err, &urlErr) && urlErr.Timeout() {
			return errResp(http.StatusGatewayTimeout, "GATEWAY_UPSTREAM_TIMEOUT", cr.err.Error())
		}
		return errResp(http.StatusBadGateway, "GATEWAY_UPSTREAM_ERROR", cr.err.Error())
	}
	// 501 + 503 are intentional contract codes — pass through verbatim.
	if cr.status == http.StatusNotImplemented || cr.status == http.StatusServiceUnavailable {
		return Response{Status: cr.status, Body: cr.body, Headers: cr.header}
	}
	if cr.status >= 500 {
		return errResp(http.StatusBadGateway, "GATEWAY_UPSTREAM_5XX",
			fmt.Sprintf("upstream returned %d", cr.status))
	}
	return Response{Status: cr.status, Body: cr.body, Headers: cr.header}
}

func errResp(status int, code, message string) Response {
	body := fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, code, message)
	return Response{Status: status, Body: []byte(body)}
}

// -----------------------------------------------------------------------------
// A6 routes
// -----------------------------------------------------------------------------

// GetFeatureFlags proxies GET /api/feature-flags → chora-tenancy
// GET /api/tenants/{tenantID}/entitlements. The tenant id is resolved from
// the validated JWT claims (auth.TenantID); a missing tenant short-circuits
// with a 400 because the downstream entitlements list is tenant-scoped.
//
// chora-tenancy has no literal "feature-flags" route — its TenantEntitlement
// list IS the canonical feature-flag set (each entitlement is a feature
// flag; see services/chora-tenancy/internal/domain/tenancy/tenancy.go).
func (a *Aggregator) GetFeatureFlags(ctx context.Context, auth AuthCtx) (Response, error) {
	if auth.TenantID == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_TENANT_NOT_RESOLVED",
			"tenant_id missing from auth context"), nil
	}
	u := a.cfg.TenancyURL + "/api/tenants/" + url.PathEscape(auth.TenantID) + "/entitlements"
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// GetTenant proxies GET /api/tenants/{id} → chora-tenancy
// GET /api/tenants/{id}. An empty id short-circuits with a 404 (defensive —
// the BFF handler guards too, but the aggregator must never build a bad URL).
func (a *Aggregator) GetTenant(ctx context.Context, auth AuthCtx, tenantID string) (Response, error) {
	if tenantID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_TENANT_ID_REQUIRED",
			"tenant id required in path: /api/tenants/{id}"), nil
	}
	u := a.cfg.TenancyURL + "/api/tenants/" + url.PathEscape(tenantID)
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// GetCourse proxies GET /api/courses/{id} → chora-delivery
// GET /v1/courses/{id}. chora-delivery's v1 single-course detail GET is
// self-gating (it 404s non-public cross-tenant rows) so the mesh headers
// are forwarded for tenant scoping but a missing tenant simply means only
// public rows resolve.
func (a *Aggregator) GetCourse(ctx context.Context, auth AuthCtx, courseID string) (Response, error) {
	if courseID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_COURSE_ID_REQUIRED",
			"course id required in path: /api/courses/{id}"), nil
	}
	u := a.cfg.DeliveryURL + "/v1/courses/" + url.PathEscape(courseID)
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// EnrolCourse proxies POST /api/courses/{id}/enrol → chora-delivery
// POST /v1/courses/{id}/enrolments. The body is forwarded verbatim;
// chora-delivery's v1 enrolment-create reads gcid from the body OR the
// lowercase gcid header (both are supplied — the header from the validated
// JWT claims).
func (a *Aggregator) EnrolCourse(ctx context.Context, auth AuthCtx, courseID string, body []byte) (Response, error) {
	if courseID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_COURSE_ID_REQUIRED",
			"course id required in path: /api/courses/{id}/enrol"), nil
	}
	u := a.cfg.DeliveryURL + "/v1/courses/" + url.PathEscape(courseID) + "/enrolments"
	return classify(a.call(ctx, http.MethodPost, u, body, auth)), nil
}

// GetKGClusters proxies GET /api/me/knowledge-graph/clusters →
// chora-consumption GET /v1/me/knowledge-graph/clusters. The downstream
// returns the FE-compatible {data: {clusters: [], capRemaining, capMax}}
// envelope which the FE consumes directly (passed through verbatim).
func (a *Aggregator) GetKGClusters(ctx context.Context, auth AuthCtx) (Response, error) {
	u := a.cfg.ConsumptionURL + "/v1/me/knowledge-graph/clusters"
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// ListCompanions proxies GET /api/me/companions → chora-consumption
// GET /v1/me/companions. The downstream returns {items: [...]} which the FE
// consumes directly (passed through verbatim).
func (a *Aggregator) ListCompanions(ctx context.Context, auth AuthCtx) (Response, error) {
	u := a.cfg.ConsumptionURL + "/v1/me/companions"
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// GetCompanionGrowth proxies GET /api/me/companions/{id}/growth →
// chora-consumption GET /v1/me/companions/{id}/growth (ADR-149 Companion
// Growth axis). An empty companion id short-circuits with a 404.
func (a *Aggregator) GetCompanionGrowth(ctx context.Context, auth AuthCtx, companionID string) (Response, error) {
	if companionID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_COMPANION_ID_REQUIRED",
			"companion id required in path: /api/me/companions/{id}/growth"), nil
	}
	u := a.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) + "/growth"
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// ListProofingTests proxies GET /api/v1/me/proofing-tests → chora-consumption
// GET /v1/me/proofing-tests (CHO-2040 R8-6 Virgin Proofing Test list). The raw
// query string (optional goal_id filter) is forwarded verbatim so the
// downstream filter parsing stays canonical. GET-only; the POST start-runner is
// CompanionBridge-owned at /api/v1/me/companions/{id}/proofing-test. The
// downstream returns the learner-scoped list the FE consumes directly.
func (a *Aggregator) ListProofingTests(ctx context.Context, auth AuthCtx, rawQuery string) (Response, error) {
	u := a.cfg.ConsumptionURL + "/v1/me/proofing-tests"
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// ListNotifications proxies GET /api/notifications → chora-notifications
// GET /api/notifications. The raw query string (recipient_gcid, channel,
// from, to, limit) is forwarded verbatim so the downstream filter parsing
// stays canonical.
func (a *Aggregator) ListNotifications(ctx context.Context, auth AuthCtx, rawQuery string) (Response, error) {
	u := a.cfg.NotificationsURL + "/api/notifications"
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// -----------------------------------------------------------------------------
// A6 follow-up (CHO-1545): atomic session — Phyllis step 7
// -----------------------------------------------------------------------------

// StartAtomSession proxies POST /api/atoms/{atomId}/session →
// chora-consumption POST /v1/me/atom-sessions.
//
// chora-consumption has NO /api/atoms/{id}/session endpoint — it serves
// the atom-session lifecycle under /v1/me/atom-sessions (its EXT-scope
// handler; see services/chora-consumption/internal/adapter/http/
// me_handlers.go). The downstream meStartSessionReq shape is
// {"atom_id": "..."} — the FE start request carries no body, so the
// gateway synthesises that body from the path atomId. The downstream
// response (extSessionResp) already carries the session_id the FE needs
// for the subsequent submit call, so the body is passed through verbatim.
//
// An empty atomId short-circuits 404 — the gateway must never build a
// downstream body with an empty atom_id (chora-consumption would 400
// MISSING_ATOM_ID, but a clean gateway-side 404 is the honest signal).
func (a *Aggregator) StartAtomSession(ctx context.Context, auth AuthCtx, atomID string) (Response, error) {
	if atomID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_ATOM_ID_REQUIRED",
			"atom id required in path: /api/atoms/{atomId}/session"), nil
	}
	// Synthesise the downstream meStartSessionReq body from the path id.
	body, _ := json.Marshal(map[string]string{"atom_id": atomID})
	u := a.cfg.ConsumptionURL + "/v1/me/atom-sessions"
	return classify(a.call(ctx, http.MethodPost, u, body, auth)), nil
}

// SubmitAtomSession proxies POST /api/atoms/{atomId}/session/submit →
// chora-consumption POST /v1/me/atom-sessions/{session_id}/answers (the
// server-side MCQ grade endpoint).
//
// Bridging contract: the downstream /answers endpoint is keyed by
// session_id, but the FE-facing path is keyed by atomId. The cleanest
// honest bridge — the FE obtains session_id from the StartAtomSession
// response and passes it back in the submit body. This method lifts
// session_id out of the FE body into the downstream path and forwards
// the REMAINING answer fields (answer_id, answer_index, hint_used —
// chora-consumption's meSubmitAnswerReq shape) as the downstream body.
//
// atomId is required in the path for REST symmetry with StartAtomSession
// (and so the route is /api/atoms/{atomId}/... consistently) but is not
// itself forwarded — the downstream session row already binds the atom.
//
// Failure modes that short-circuit BEFORE any fan-out (clearer signal
// than a downstream 4xx):
//   - empty atomId            → 404 GATEWAY_ATOM_ID_REQUIRED
//   - malformed FE body       → 400 GATEWAY_BAD_REQUEST_BODY
//   - body missing session_id → 400 GATEWAY_SESSION_ID_REQUIRED
func (a *Aggregator) SubmitAtomSession(ctx context.Context, auth AuthCtx, atomID string, body []byte) (Response, error) {
	if atomID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_ATOM_ID_REQUIRED",
			"atom id required in path: /api/atoms/{atomId}/session/submit"), nil
	}
	// Parse the FE body to lift session_id into the path. The remaining
	// fields are re-marshalled as the downstream answer body so session_id
	// is not duplicated (it lives in the path).
	var fe map[string]any
	if err := json.Unmarshal(body, &fe); err != nil {
		return errResp(http.StatusBadRequest, "GATEWAY_BAD_REQUEST_BODY",
			"submit body must be valid JSON: "+err.Error()), nil
	}
	sessionID, _ := fe["session_id"].(string)
	if sessionID == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_SESSION_ID_REQUIRED",
			"submit body must include session_id (from the start-session response)"), nil
	}
	delete(fe, "session_id")
	downstreamBody, _ := json.Marshal(fe)

	u := a.cfg.ConsumptionURL + "/v1/me/atom-sessions/" + url.PathEscape(sessionID) + "/answers"
	return classify(a.call(ctx, http.MethodPost, u, downstreamBody, auth)), nil
}

// -----------------------------------------------------------------------------
// A17 (2026-05-15) — mana wallet + top-up — pure passthrough to chora-identity
// -----------------------------------------------------------------------------

// GetMyMana proxies GET /api/v1/me/mana → chora-identity GET /api/v1/me/mana
// (same path; pure pass). The downstream returns the canonical MeMana shape
// per chora-contracts/openapi/learner-economy.yaml:172-183 — the body passes
// through verbatim. A downstream 402 (e.g. saga-state lock) passes through
// unchanged so the FE can render the upsell UI.
func (a *Aggregator) GetMyMana(ctx context.Context, auth AuthCtx) (Response, error) {
	u := a.cfg.IdentityURL + "/api/v1/me/mana"
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// TopupMana proxies POST /api/v1/me/mana/topup → chora-identity POST
// /api/v1/me/mana/topup (same path; pure pass). Honours the FE
// Idempotency-Key header per learner-economy.yaml:482 — auth.IdempotencyKey
// is stamped in the call() helper, NOT lifted into the body. The downstream
// returns the canonical ManaTopupResponse (201) or 402 InsufficientManaUpsell
// (contract-locked) — both pass through verbatim. The body is forwarded
// verbatim (no reshape) — the FE-facing ManaTopupRequest equals the
// downstream request shape.
func (a *Aggregator) TopupMana(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	u := a.cfg.IdentityURL + "/api/v1/me/mana/topup"
	return classify(a.call(ctx, http.MethodPost, u, body, auth)), nil
}

// GrantDemoMana proxies POST /api/v1/me/mana/demo-grant → chora-identity
// POST /api/v1/me/mana/demo-grant (same path; pure pass). The demo grant is
// a no-body operation — the credited amount is server-configured per
// learner-economy.yaml grantDemoManaToSelf, so the gateway forwards NO body
// rather than an empty JSON object (a `{}` would be a client-supplied
// amount, which the contract forbids).
//
// The Idempotency-Key header is REQUIRED by the contract, so unlike
// TopupMana the gateway does not rely on the downstream's fallback key
// synthesis: auth.IdempotencyKey is stamped by the call() helper whenever
// the FE supplied one, and a missing one is a downstream 400 that must pass
// through verbatim rather than be masked by a gateway-generated key.
//
// The downstream returns the canonical ManaDemoGrantResponse (200), 400
// (missing Idempotency-Key), 404 (demo surface not enabled in this
// deployment) or 429 (demo quota exhausted) — all pass through verbatim so
// the FE can distinguish "demo is off here" from "you have no budget left".
func (a *Aggregator) GrantDemoMana(ctx context.Context, auth AuthCtx) (Response, error) {
	u := a.cfg.IdentityURL + "/api/v1/me/mana/demo-grant"
	return classify(a.call(ctx, http.MethodPost, u, nil, auth)), nil
}

// ListMyManaLedger proxies GET /api/v1/me/mana/ledger → chora-identity GET
// /api/v1/me/mana/ledger (same path; pure passthrough, rawQuery forwarded
// verbatim like ListMyEnrolments).
//
// CHO-1883 (2026-06-26) — chora-identity's listManaLedger (me_economy_handlers.go)
// RLS-scopes the per-row ledger to the JWT gcid and returns {items:[...]}. The
// gateway never owned this leaf — the dispatcher claimed only /api/v1/me/mana
// (+ /topup) as exact paths, so the ledger 404'd at the edge (2026-06-25
// mana-parity handoff §2.4) and the A+ Wallet could only show a balance. The
// rawQuery (from/to/direction/reason/page_size) is forwarded so the FE can
// filter/paginate without a gateway redeploy.
func (a *Aggregator) ListMyManaLedger(ctx context.Context, auth AuthCtx, rawQuery string) (Response, error) {
	u := a.cfg.IdentityURL + "/api/v1/me/mana/ledger"
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// -----------------------------------------------------------------------------
// CJ#2 (2026-05-26) — GET /api/v1/me/enrolments?course_id=... — pure
// passthrough to chora-delivery.
//
// FE polling endpoint hit by the post-Stripe-checkout success screen
// (chora-web/src/app/features/surfaces/aplus/course-enrolment-success/
// course-enrolment-success.component.ts) until the row appears.
//
// The endpoint was silently missing from the gateway route table after the
// CJ#2 path migrated to /api/v1/*; all 13+ polls returned 404 even though
// chora-delivery's /v1/me/enrolments handler was healthy + the
// course_enrollments DB row existed (per the 2026-05-26 smoke).
//
// chora-delivery ignores the course_id query param and returns ALL enrolments
// for the JWT-identified gcid + tenant; the FE filters client-side. We still
// forward the rawQuery verbatim so a future server-side filter is a one-line
// chora-delivery change without a gateway redeploy.
// -----------------------------------------------------------------------------

// ListMyEnrolments proxies GET /api/v1/me/enrolments → chora-delivery GET
// /v1/me/enrolments (path translated; rawQuery forwarded verbatim).
//
// The downstream returns 200 with {items, total} where items[] each carries
// course_id + enrolled_at; the FE poller matches by course_id and surfaces
// the "Enrolled — view course" CTA on first match. Empty items + 200 = "not
// yet"; the poller keeps ticking up to the timeout in the component.
func (a *Aggregator) ListMyEnrolments(ctx context.Context, auth AuthCtx, rawQuery string) (Response, error) {
	u := a.cfg.DeliveryURL + "/v1/me/enrolments"
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// -----------------------------------------------------------------------------
// Debt #4 / A6 — GET /api/v1/instructors/{instructor_gcid}/courses
// (FE instructor-roster surface) — pure passthrough to chora-delivery.
// -----------------------------------------------------------------------------

// ListInstructorCourses proxies GET /api/v1/instructors/{instructor_gcid}/courses
// → chora-delivery GET /api/v1/instructors/{instructor_gcid}/courses (same
// path; pure passthrough). chora-delivery owns the by-instructor read
// + does its own authz (self-or-instructor-or-admin via x-mesh-user-roles)
// + RLS-scopes the read to the caller's tenant. The downstream returns
// {items[], total, page, per}.
//
// An empty instructorGCID short-circuits with a 404 — the gateway must
// never build a downstream path with an empty segment.
//
// rawQuery is forwarded verbatim so pagination params (page, per) flow
// to the downstream's parser.
func (a *Aggregator) ListInstructorCourses(ctx context.Context, auth AuthCtx, instructorGCID, rawQuery string) (Response, error) {
	if instructorGCID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_INSTRUCTOR_ID_REQUIRED",
			"instructor id required in path: /api/v1/instructors/{instructor_gcid}/courses"), nil
	}
	u := a.cfg.DeliveryURL + "/api/v1/instructors/" + url.PathEscape(instructorGCID) + "/courses"
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// -----------------------------------------------------------------------------
// B6.1 (2026-05-16) — searchTenantMembers picker — pure passthrough to
// chora-identity. The FE picker chip-search hits this endpoint after the
// instructor types >= 3 chars; the downstream returns a tenant-scoped
// (RLS-filtered) list of {gcid, email, display_name, avatar_url, roles[],
// last_active_at} entries.
//
// Contract: chora-contracts/openapi/identity-admin.yaml::searchTenantMembers
//
//	GET /api/v1/admin/tenant-members
//	  ?q=<2..64 chars>
//	  &role=<LEARNER|INSTRUCTOR|ADMIN|AUDITOR|OWNER|SUPPORT_AGENT>
//	  &page_size=<10|20|50|100>
//	  &page_token=<base64 cursor>
//
// Status passthrough:
//   - 200 — list of TenantMemberSummary entries (verbatim).
//   - 401 — gateway should have caught (JWT gate); downstream defensive 401.
//   - 403 — caller lacks TRAINING_ADMIN / TENANT_ADMIN role (verbatim).
//   - 422 — handler-level validation failure (verbatim).
//   - 5xx → 502 GATEWAY_UPSTREAM_5XX (the only status normalisation).
//
// Query string is forwarded verbatim — chora-identity's handler parses it
// canonically. No body (GET only).
// -----------------------------------------------------------------------------

// SearchTenantMembers proxies GET /api/v1/admin/tenant-members → chora-identity
// at the same path. The rawQuery is forwarded verbatim. Mesh-trust headers
// (chora-tenant-id + chora-gcid + x-mesh-user-roles) are stamped via the
// canonical call() helper.
func (a *Aggregator) SearchTenantMembers(ctx context.Context, auth AuthCtx, rawQuery string) (Response, error) {
	u := a.cfg.IdentityURL + "/api/v1/admin/tenant-members"
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// -----------------------------------------------------------------------------
// P7 (2026-05-15) — manual question authoring — pure passthrough to chora-creation
//
// chora-creation serves the question CRUD routes at the SAME path the FE hits:
// /api/atoms/{atom_id}/questions[/{question_id}] (commit 83a769e7 /
// chora-creation :p3-b82d1e32). The gateway forwards request body + bearer +
// mesh headers verbatim — no body reshape, no path rewrite.
//
// Status passthrough (per cr-question-authoring-design-2026-05-15.md §4):
//   - 201 (POST create)              — verbatim
//   - 200 (PATCH / GET)              — verbatim
//   - 204 (DELETE — idempotent)      — verbatim, NO body
//   - 400 (bad JSON)                 — verbatim
//   - 401 (no bearer)                — verbatim (the gateway JWT gate fires first)
//   - 403 (non-author non-admin)     — verbatim
//   - 404 (atom or question not found) — verbatim
//   - 409 CREATION_QUESTION_DUPLICATE (D2: 1 atom = 1 question) — verbatim
//   - 422 (validation failed)        — verbatim
//   - 501 (reserved question_type)   — verbatim (downstream 4xx semantics)
//   - 5xx → 502 GATEWAY_UPSTREAM_5XX — the only status normalisation
//
// Empty atom_id or question_id short-circuit 404 BEFORE the outbound call
// (the gateway must never build a bad downstream URL).
// -----------------------------------------------------------------------------

// CreateQuestion proxies POST /api/atoms/{atom_id}/questions → chora-creation
// at the same path. Body forwarded verbatim (no reshape). Returns 404 BEFORE
// any outbound call when atomID is empty.
func (a *Aggregator) CreateQuestion(ctx context.Context, auth AuthCtx, atomID string, body []byte) (Response, error) {
	if atomID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_ATOM_ID_REQUIRED",
			"atom id required in path: /api/atoms/{atom_id}/questions"), nil
	}
	u := a.cfg.CreationURL + "/api/atoms/" + url.PathEscape(atomID) + "/questions"
	return classify(a.call(ctx, http.MethodPost, u, body, auth)), nil
}

// EditQuestion proxies PATCH /api/atoms/{atom_id}/questions/{question_id} →
// chora-creation at the same path. Body forwarded verbatim. Each successful
// PATCH appends a QuestionRevision (append-only invariant) and bumps the
// parent AtomRevision (handled downstream).
func (a *Aggregator) EditQuestion(ctx context.Context, auth AuthCtx, atomID, questionID string, body []byte) (Response, error) {
	if atomID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_ATOM_ID_REQUIRED",
			"atom id required in path: /api/atoms/{atom_id}/questions/{question_id}"), nil
	}
	if questionID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_QUESTION_ID_REQUIRED",
			"question id required in path: /api/atoms/{atom_id}/questions/{question_id}"), nil
	}
	u := a.cfg.CreationURL + "/api/atoms/" + url.PathEscape(atomID) + "/questions/" + url.PathEscape(questionID)
	return classify(a.call(ctx, http.MethodPatch, u, body, auth)), nil
}

// GetQuestion proxies GET /api/atoms/{atom_id}/questions/{question_id} →
// chora-creation at the same path. The downstream returns the admin AUTHOR
// projection (includes mcq_payload / essay_payload + current revision); it
// passes through verbatim.
func (a *Aggregator) GetQuestion(ctx context.Context, auth AuthCtx, atomID, questionID string) (Response, error) {
	if atomID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_ATOM_ID_REQUIRED",
			"atom id required in path: /api/atoms/{atom_id}/questions/{question_id}"), nil
	}
	if questionID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_QUESTION_ID_REQUIRED",
			"question id required in path: /api/atoms/{atom_id}/questions/{question_id}"), nil
	}
	u := a.cfg.CreationURL + "/api/atoms/" + url.PathEscape(atomID) + "/questions/" + url.PathEscape(questionID)
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// DeleteQuestion proxies DELETE /api/atoms/{atom_id}/questions/{question_id}
// → chora-creation at the same path. Soft-delete (idempotent per FE A18 —
// re-deleting an already-deleted question still returns 204). The 204 + the
// empty body pass through verbatim — the gateway MUST NOT synthesise an
// envelope.
func (a *Aggregator) DeleteQuestion(ctx context.Context, auth AuthCtx, atomID, questionID string) (Response, error) {
	if atomID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_ATOM_ID_REQUIRED",
			"atom id required in path: /api/atoms/{atom_id}/questions/{question_id}"), nil
	}
	if questionID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_QUESTION_ID_REQUIRED",
			"question id required in path: /api/atoms/{atom_id}/questions/{question_id}"), nil
	}
	u := a.cfg.CreationURL + "/api/atoms/" + url.PathEscape(atomID) + "/questions/" + url.PathEscape(questionID)
	return classify(a.call(ctx, http.MethodDelete, u, nil, auth)), nil
}

// -----------------------------------------------------------------------------
// P7.5 (2026-05-15) — AI single-question async + model-answer adhoc fill —
// pure passthrough to chora-creation. P4+P5 (commits d958890c + 6ce3a0d9)
// landed the chora-creation single-Q async path; the gateway now claims the
// new URL leaves so the FE can dispatch ai_draft + batch_source_material +
// adhoc model-answer fills:
//
//	POST   /api/atoms/{atom_id}/question-jobs                          (create)
//	GET    /api/atoms/{atom_id}/question-jobs/{job_id}                 (poll)
//	POST   /api/atoms/{atom_id}/question-jobs/{job_id}/accept          (persist)
//	POST   /api/atoms/{atom_id}/questions/{q_id}/ai-model-answer-jobs  (adhoc)
//
// All four leaves use the SAME path on chora-creation — pure verbatim proxy.
//
// Status passthrough (P5 envelope semantics — see question_jobs_handler.go):
//   - 202 (POST create / model-answer)                     — verbatim
//   - 200 (GET poll / POST accept)                          — verbatim
//   - 402 INSUFFICIENT_MANA + upsell envelope               — verbatim
//   - 404 (atom / job / question not found)                 — verbatim
//   - 422 (validation failed)                               — verbatim
//   - 503 CREATION_JOBS_NOT_WIRED (chora-identity gRPC absent) — verbatim
//   - 5xx → 502 GATEWAY_UPSTREAM_5XX (only status normalisation)
//
// Multipart pass-through (POST /question-jobs only) — the batch source-
// material upload path carries multipart/form-data with file bytes. The
// gateway MUST forward the request body untouched AND preserve Content-Type
// (boundary intact). Callers supply both via the explicit contentType +
// body parameters; the call helper is bypassed when contentType is
// non-application/json so the canonical Content-Type header is preserved.
// -----------------------------------------------------------------------------

// CreateQuestionJob proxies POST /api/atoms/{atom_id}/question-jobs →
// chora-creation at the same path. Body forwarded verbatim. contentType is
// stamped verbatim on the outbound request (application/json OR
// multipart/form-data with boundary for batch source-material upload).
// Empty atomID short-circuits 404 BEFORE any outbound call.
func (a *Aggregator) CreateQuestionJob(ctx context.Context, auth AuthCtx, atomID, contentType string, body []byte) (Response, error) {
	if atomID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_ATOM_ID_REQUIRED",
			"atom id required in path: /api/atoms/{atom_id}/question-jobs"), nil
	}
	u := a.cfg.CreationURL + "/api/atoms/" + url.PathEscape(atomID) + "/question-jobs"
	return classify(a.callWithContentType(ctx, http.MethodPost, u, contentType, body, auth)), nil
}

// GetQuestionJob proxies GET /api/atoms/{atom_id}/question-jobs/{job_id} →
// chora-creation at the same path. Returns 404 BEFORE any outbound call when
// atomID or jobID is empty.
func (a *Aggregator) GetQuestionJob(ctx context.Context, auth AuthCtx, atomID, jobID string) (Response, error) {
	if atomID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_ATOM_ID_REQUIRED",
			"atom id required in path: /api/atoms/{atom_id}/question-jobs/{job_id}"), nil
	}
	if jobID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_JOB_ID_REQUIRED",
			"job id required in path: /api/atoms/{atom_id}/question-jobs/{job_id}"), nil
	}
	u := a.cfg.CreationURL + "/api/atoms/" + url.PathEscape(atomID) + "/question-jobs/" + url.PathEscape(jobID)
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// AcceptQuestionJob proxies POST /api/atoms/{atom_id}/question-jobs/{job_id}/accept →
// chora-creation at the same path. Body forwarded verbatim (the accepted_candidates
// array with optional overrides). 404 BEFORE any outbound call when atomID or jobID empty.
func (a *Aggregator) AcceptQuestionJob(ctx context.Context, auth AuthCtx, atomID, jobID string, body []byte) (Response, error) {
	if atomID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_ATOM_ID_REQUIRED",
			"atom id required in path: /api/atoms/{atom_id}/question-jobs/{job_id}/accept"), nil
	}
	if jobID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_JOB_ID_REQUIRED",
			"job id required in path: /api/atoms/{atom_id}/question-jobs/{job_id}/accept"), nil
	}
	u := a.cfg.CreationURL + "/api/atoms/" + url.PathEscape(atomID) + "/question-jobs/" + url.PathEscape(jobID) + "/accept"
	return classify(a.call(ctx, http.MethodPost, u, body, auth)), nil
}

// RegenerateQuestionJobImage proxies POST
// /api/atoms/{atom_id}/question-jobs/{job_id}/regenerate-image → chora-creation
// at the same path (CHO-1822 review-stage image regenerate). Body forwarded
// verbatim ({draft_id, placement, prompt}); the FE Idempotency-Key rides via
// auth. 404 BEFORE any outbound call when atomID or jobID is empty.
func (a *Aggregator) RegenerateQuestionJobImage(ctx context.Context, auth AuthCtx, atomID, jobID string, body []byte) (Response, error) {
	if atomID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_ATOM_ID_REQUIRED",
			"atom id required in path: /api/atoms/{atom_id}/question-jobs/{job_id}/regenerate-image"), nil
	}
	if jobID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_JOB_ID_REQUIRED",
			"job id required in path: /api/atoms/{atom_id}/question-jobs/{job_id}/regenerate-image"), nil
	}
	u := a.cfg.CreationURL + "/api/atoms/" + url.PathEscape(atomID) + "/question-jobs/" + url.PathEscape(jobID) + "/regenerate-image"
	return classify(a.call(ctx, http.MethodPost, u, body, auth)), nil
}

// CreateModelAnswerJob proxies POST /api/atoms/{atom_id}/questions/{q_id}/ai-model-answer-jobs →
// chora-creation at the same path. Body forwarded verbatim (optional tone_hint).
// 404 BEFORE any outbound call when atomID or questionID empty.
func (a *Aggregator) CreateModelAnswerJob(ctx context.Context, auth AuthCtx, atomID, questionID string, body []byte) (Response, error) {
	if atomID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_ATOM_ID_REQUIRED",
			"atom id required in path: /api/atoms/{atom_id}/questions/{question_id}/ai-model-answer-jobs"), nil
	}
	if questionID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_QUESTION_ID_REQUIRED",
			"question id required in path: /api/atoms/{atom_id}/questions/{question_id}/ai-model-answer-jobs"), nil
	}
	u := a.cfg.CreationURL + "/api/atoms/" + url.PathEscape(atomID) + "/questions/" + url.PathEscape(questionID) + "/ai-model-answer-jobs"
	return classify(a.call(ctx, http.MethodPost, u, body, auth)), nil
}

// -----------------------------------------------------------------------------
// QuestionTypes registry (2026-05-15) — pure passthrough to chora-creation.
//
// Bug fixed: the legacy Phyllis /api/atoms/{id} read-side route claims the
// static /api/atoms/question-types path because the parametric {id} pattern
// treats "question-types" as an atom_id — and Phyllis's GetAtom wraps the
// body in {"atom": ...}. That wrap is correct for the atom-fetch contract
// but WRONG for the registry which per chora-contracts/openapi/
// creation-questions.yaml::listQuestionTypes returns {"items": [...]} at
// the top level.
//
// Fix shape: claim the EXACT static path /api/atoms/question-types in the
// gatewayproxy bridge (before any parametric atom dispatch) and forward
// the chora-creation body BYTE-IDENTICAL — no wrap, no reshape.
//
// Status passthrough (same envelope semantics as the rest of this
// aggregator): 5xx → 502, timeout → 504, 4xx + 2xx pass through verbatim.
// -----------------------------------------------------------------------------

// GetQuestionTypes proxies GET /api/atoms/question-types → chora-creation
// GET /api/atoms/question-types (same path; pure pass). The downstream
// returns the canonical {"items": [QuestionTypeOption]} envelope per
// chora-contracts/openapi/creation-questions.yaml — the body is forwarded
// verbatim. Tenant-agnostic registry (same for every caller) but the bearer
// + mesh headers are still stamped so chora-creation's standard auth gate
// is satisfied.
func (a *Aggregator) GetQuestionTypes(ctx context.Context, auth AuthCtx) (Response, error) {
	u := a.cfg.CreationURL + "/api/atoms/question-types"
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// CreateAtom proxies POST /api/atoms → chora-creation at the same path.
// Body forwarded verbatim per the chora-creation createAtom contract.
// Closes FE A20 (2026-05-16): the previous routing 307-redirected POST
// /api/atoms → /api/atoms/ which then 404'd on handleAtomSubpath. By
// claiming the EXACT static path the redirect loop is broken.
//
// Status passthrough:
//
//	201 — atom created (LearningAtom envelope).
//	400 — CREATION_INVALID_BODY / CREATION_INVALID_ATOM.
//	401 / 403 — bearer / role gate.
//	5xx → 502 GATEWAY_UPSTREAM_5XX.
func (a *Aggregator) CreateAtom(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	u := a.cfg.CreationURL + "/api/atoms"
	return classify(a.call(ctx, http.MethodPost, u, body, auth)), nil
}

// GetDailyDoseAI proxies the async daily-dose AI enrichment
// (GET /api/companion/daily-dose/ai) to chora-consumption. The downstream runs
// the Companion greeting + Recommender picks to completion (~16-30s LLM
// round-trips, gateway-metered), so it uses the long PostCreateTimeout rather
// than the default 6s PerCallTimeout (which would 504 the enrichment). The FE
// fetches this AFTER the fast deterministic /companion/daily-dose renders.
func (a *Aggregator) GetDailyDoseAI(ctx context.Context, auth AuthCtx) (Response, error) {
	if a.cfg.ConsumptionURL == "" {
		return errResp(http.StatusBadGateway, "GATEWAY_NOT_CONFIGURED",
			"SVC_CONSUMPTION_URL must be set for /api/companion/daily-dose/ai"), nil
	}
	u := a.cfg.ConsumptionURL + "/companion/daily-dose/ai"
	return classify(a.callWithTimeout(ctx, http.MethodGet, u, nil, auth, a.cfg.PostCreateTimeout)), nil
}

// ListAtoms proxies GET /api/atoms?status=... → chora-creation listAtoms.
// Query string forwarded verbatim so the downstream status filter parses
// canonically.
func (a *Aggregator) ListAtoms(ctx context.Context, auth AuthCtx, rawQuery string) (Response, error) {
	u := a.cfg.CreationURL + "/api/atoms"
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// GetAtomByID proxies GET /api/v1/atoms/{id} → chora-creation /api/atoms/{id}
// (CHO-2261). The gateway-facing path carries the /v1/ prefix the A+ atom
// services (atom.service.ts loadAtom, driving the Daily Dose AI-pick
// resolution per ADR-196) use; chora-creation serves the atom's authored
// detail at /api/atoms/{id} — NO /v1/. That downstream path is the one the
// live mesh AuthorizationPolicy creation/allow-from-gateway ALREADY allowlists
// (/api/atoms + /api/atoms/*) and the one the working PLAY page hits, so the
// prefix is TRANSLATED here rather than passed through: no /api/v1/atoms/*
// mesh entry is required.
//
// Response forwarded VERBATIM (unwrapped LearningAtom — atom.service.ts
// consumes the atom directly, with NO {atom:...} envelope; chora-creation's
// getAtom projection is already learner-safe, stripping is_correct /
// model_answer). Empty atomID short-circuits 404 BEFORE any outbound call (the
// gateway must never build a bad downstream URL). Upstream 404 passes through;
// 5xx → 502 GATEWAY_UPSTREAM_5XX via classify().
func (a *Aggregator) GetAtomByID(ctx context.Context, auth AuthCtx, atomID string) (Response, error) {
	if atomID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_ATOM_ID_REQUIRED",
			"atom id required in path: /api/v1/atoms/{id}"), nil
	}
	u := a.cfg.CreationURL + "/api/atoms/" + url.PathEscape(atomID)
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// ProxyTopicTree proxies the content topic-tree routes (CHO-2276, Sub-phase B)
// onto chora-creation, TRANSLATING the gateway-facing /api/v1/topics prefix
// onto the downstream /api/topics prefix — the path chora-creation's topic
// handler serves (Sub-phase A, CHO-2275) and the one the live mesh
// AuthorizationPolicy creation/allow-from-gateway allowlists (/api/topics +
// /api/topics/*, added in this sub-phase). Method + body + query are forwarded
// VERBATIM:
//
//	GET    /api/v1/topics[?parent_id]     → GET    /api/topics[?parent_id]   (list)
//	GET    /api/v1/topics/{id}            → GET    /api/topics/{id}          (node)
//	POST   /api/v1/topics                 → POST   /api/topics               (create, admin)
//	PUT    /api/v1/topics/{id}            → PUT    /api/topics/{id}          (rename/reorder, admin)
//	DELETE /api/v1/topics/{id}            → DELETE /api/topics/{id}          (soft-delete, admin)
//	POST   /api/v1/topics/{id}/move       → POST   /api/topics/{id}/move     (reparent, admin)
//	POST   /api/v1/topics/{id}/atoms      → POST   /api/topics/{id}/atoms    (attach, admin)
//
// topicID=="" selects the collection; a non-empty topicID selects the item, and
// subResource ("move" | "atoms") appends the write leaf. The id is PathEscaped
// so the gateway never builds a malformed downstream URL (mirrors GetAtomByID /
// GetCollection). The operator-only POST /api/internal/topics/backfill is NOT
// proxied here — it bypasses the tenant-header gate and stays in-cluster.
//
// Reads are learner-safe (JWT + RLS, like atoms CHO-2261). Writes are
// admin-gated DOWNSTREAM: call() stamps x-mesh-user-roles from the validated
// JWT (the authoritative role assertion) and chora-creation's topic handler
// enforces admin/owner (requireAdmin, defence-in-depth). Its 403
// CREATION_TOPIC_ADMIN_REQUIRED passes through verbatim; upstream 404 passes
// through; 5xx → 502 GATEWAY_UPSTREAM_5XX via classify().
func (a *Aggregator) ProxyTopicTree(ctx context.Context, auth AuthCtx, method, topicID, subResource, rawQuery string, body []byte) (Response, error) {
	u := a.cfg.CreationURL + "/api/topics"
	if topicID != "" {
		u += "/" + url.PathEscape(topicID)
		if subResource != "" {
			u += "/" + subResource
		}
	}
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.call(ctx, method, u, body, auth)), nil
}

// PublishAtom proxies POST /api/atoms/{atom_id}/publish → chora-creation at
// the same path. Body forwarded verbatim (no reshape — the publish handler
// takes no body). Response is a bare LearningAtom envelope (no {atom} wrap)
// per A22.Q2 — chora-creation's publishAtomResponse() helper merges
// status + current_revision_id + current_revision_number directly onto the
// LearningAtom shape per A22.Q4.
//
// Status passthrough (per A22 ack):
//
//	200 — publish success OR idempotent re-publish.
//	404 — atom not found (or soft-deleted; in-memory repo filters).
//	409 — discriminated sub-codes per ADR-141 D4:
//	      CREATION_ATOM_NO_PUBLISHED_REVISION — no Question/Revision exists yet.
//	      CREATION_ATOM_ARCHIVED              — atom is soft-deleted.
//	405 — non-POST methods (the gateway dispatcher already 405s, but
//	      chora-creation enforces this defensively too).
//	5xx → 502 GATEWAY_UPSTREAM_5XX (the only status normalisation).
//
// Empty atomID short-circuits 404 BEFORE any outbound call (the gateway must
// never build a bad downstream URL).
func (a *Aggregator) PublishAtom(ctx context.Context, auth AuthCtx, atomID string) (Response, error) {
	if atomID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_ATOM_ID_REQUIRED",
			"atom id required in path: /api/atoms/{atom_id}/publish"), nil
	}
	u := a.cfg.CreationURL + "/api/atoms/" + url.PathEscape(atomID) + "/publish"
	return classify(a.call(ctx, http.MethodPost, u, nil, auth)), nil
}

// CloneAtom proxies POST /api/atoms/{atom_id}/clone → chora-creation at the
// same path (Phase-A.2 Wave 2, 2026-06-28). Body forwarded verbatim (optional
// {title} override). The downstream mints a NEW draft atom + question copying
// the source payload + stamps cloned_from_atom_id provenance (ADR-199) and
// returns 201 + the new atom. Empty atomID short-circuits 404.
func (a *Aggregator) CloneAtom(ctx context.Context, auth AuthCtx, atomID string, body []byte) (Response, error) {
	if atomID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_ATOM_ID_REQUIRED",
			"atom id required in path: /api/atoms/{atom_id}/clone"), nil
	}
	u := a.cfg.CreationURL + "/api/atoms/" + url.PathEscape(atomID) + "/clone"
	return classify(a.call(ctx, http.MethodPost, u, body, auth)), nil
}

// ChangeAtomReuseVisibility proxies PATCH /api/atoms/{atom_id}/reuse-visibility
// → chora-creation at the same path (ADR-229 WS-1, CHO-2127). Body
// ({reuse_visibility}) forwarded verbatim. The downstream enforces the
// author-only guard — its 403 CREATION_NOT_AUTHOR (and 400/404/409) pass
// through unchanged as the FE's semantic signal. Empty atomID
// short-circuits 404.
func (a *Aggregator) ChangeAtomReuseVisibility(ctx context.Context, auth AuthCtx, atomID string, body []byte) (Response, error) {
	if atomID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_ATOM_ID_REQUIRED",
			"atom id required in path: /api/atoms/{atom_id}/reuse-visibility"), nil
	}
	u := a.cfg.CreationURL + "/api/atoms/" + url.PathEscape(atomID) + "/reuse-visibility"
	return classify(a.call(ctx, http.MethodPatch, u, body, auth)), nil
}

// -----------------------------------------------------------------------------
// Lane D (#60, 2026-05-16) — Tier 1 verbatim proxy claims for the routes
// Lanes A (test-sets), B (assessments + me/assessments), and C
// (atoms/questions/search) just shipped. The downstream services serve at the
// SAME path the FE hits — pure passthrough with mesh-trust header stamping.
//
// Verbatim contract:
//   - method preserved (GET / POST / PATCH / DELETE)
//   - path forwarded verbatim — the gateway does NOT rewrite
//   - request body forwarded byte-for-byte (Content-Type honoured via
//     callWithContentType when supplied; defaults to application/json)
//   - query string forwarded verbatim
//   - mesh-trust headers stamped via the canonical call helper
//     (X-Tenant-Id + lowercase gcid + chora-* + x-mesh-user-roles)
//   - 5xx → 502 GATEWAY_UPSTREAM_5XX (the only normalisation)
//   - 2xx + 4xx pass through verbatim — a downstream 4xx is a semantic signal
//     the FE must render unchanged (e.g. 403 from the role gate, 404 from
//     missing test set, 422 from validation).
//
// Self-cohort enforcement on /api/v1/me/assessments/* — delegated to the
// downstream chora-delivery handler which reads the gcid header and filters
// to caller-only rows. The gateway just stamps the mesh-claim gcid.
// -----------------------------------------------------------------------------

// ProxyTestSets is a verbatim passthrough for /api/v1/test-sets[/{...}] →
// chora-delivery at the same path. Supports the full CRUD + per-question
// add/update/remove + /publish + /archive lifecycle per
// chora-contracts/openapi/delivery-test-sets.yaml.
//
// path MUST start with /api/v1/test-sets — the caller is responsible for
// preserving the exact path the FE sent. rawQuery + body are forwarded
// unchanged. contentType honours the inbound Content-Type so the downstream
// handler's body parser sees the original payload shape.
func (a *Aggregator) ProxyTestSets(ctx context.Context, auth AuthCtx, method, path, rawQuery string, body []byte, contentType string) (Response, error) {
	u := a.cfg.DeliveryURL + path
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.callWithContentType(ctx, method, u, contentType, body, auth)), nil
}

// ProxyAssessments is a verbatim passthrough for /api/v1/assessments[/{...}]
// → chora-delivery at the same path. Covers the 5 instructor-side operations
// per chora-contracts/openapi/delivery-assessments.yaml:
//
//	POST   /api/v1/assessments                              createAssessment
//	GET    /api/v1/assessments                              listAssessments (TBD)
//	GET    /api/v1/assessments/{id}                         getAssessment
//	GET    /api/v1/assessments/{id}/monitor                 getAssessmentMonitor
//	GET    /api/v1/assessments/{id}/submissions             listAssessmentSubmissions
//	POST   /api/v1/assessments/{id}/release-results         releaseAssessmentResults
//	POST   /api/v1/assessments/{id}/publish                 publishAssessment (TBD)
//	POST   /api/v1/assessments/{id}/force-close             forceCloseAssessment (TBD)
//	POST   /api/v1/assessments/{id}/archive                 archiveAssessment (TBD)
//
// Authorization is enforced downstream — the chora-delivery handler reads
// x-mesh-user-roles for the instructor / training-admin role gate.
func (a *Aggregator) ProxyAssessments(ctx context.Context, auth AuthCtx, method, path, rawQuery string, body []byte, contentType string) (Response, error) {
	u := a.cfg.DeliveryURL + path
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.callWithContentType(ctx, method, u, contentType, body, auth)), nil
}

// ProxyMeAssessments is a verbatim passthrough for /api/v1/me/assessments[/{...}]
// → chora-delivery at the same path. Covers the 6 learner-side operations:
//
//	GET    /api/v1/me/assessments                                           listMyAssessments
//	GET    /api/v1/me/assessments/{id}                                      getMyAssessment
//	POST   /api/v1/me/assessments/{id}/submissions                          startMySubmission
//	PATCH  /api/v1/me/assessments/{id}/submissions/{subId}/autosave         autosaveMySubmission
//	POST   /api/v1/me/assessments/{id}/submissions/{subId}/submit           submitMySubmission
//	GET    /api/v1/me/assessments/{id}/submissions/{subId}/result           getMySubmissionResult
//
// Self-cohort enforcement is delegated to the downstream — chora-delivery
// reads the gcid header (stamped by the canonical call helper from auth.GCID)
// and scopes results to the caller. The gateway does NOT attempt to filter
// by gcid locally; the trust boundary is the validated JWT → mesh-claim gcid.
func (a *Aggregator) ProxyMeAssessments(ctx context.Context, auth AuthCtx, method, path, rawQuery string, body []byte, contentType string) (Response, error) {
	u := a.cfg.DeliveryURL + path
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.callWithContentType(ctx, method, u, contentType, body, auth)), nil
}

// ProxyCourses is a verbatim passthrough for /api/v1/courses[/{...}] →
// chora-delivery at the same path. Covers the 6 CJ#2 endpoints per
// chora-contracts/openapi/delivery-courses.yaml:
//
//	POST   /api/v1/courses                — create DRAFT (instructor)
//	GET    /api/v1/courses?state=...      — list (R+ admin queue)
//	GET    /api/v1/courses/{id}           — fetch one
//	PATCH  /api/v1/courses/{id}           — update DRAFT
//	POST   /api/v1/courses/{id}/publish   — DRAFT → AWAITING_REVIEW
//	POST   /api/v1/courses/{id}/release   — AWAITING_REVIEW → PUBLISHED
//	POST   /api/v1/courses/{id}/reject    — AWAITING_REVIEW → DRAFT
//
// L3 (CHO-1793/1795) additions — same verbatim subtree, no new route needed:
//
//	POST   /api/v1/courses/{id}/content/upload-url — mint a signed PUT URL for
//	       a video/PDF/image upload (the FE then PUTs bytes DIRECT to GCS).
//	cert-definition rides the existing create (POST) + update (PATCH) bodies.
//
// Authorization is enforced downstream — the chora-delivery handler reads
// x-mesh-user-roles for the instructor / training-admin role gate.
//
// Per E2E-BE-CJ2 directive row at
// `docs/m13/e2e-fe-coord-directive-2026-05-16.md` §3.
func (a *Aggregator) ProxyCourses(ctx context.Context, auth AuthCtx, method, path, rawQuery string, body []byte, contentType string) (Response, error) {
	u := a.cfg.DeliveryURL + path
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.callWithContentType(ctx, method, u, contentType, body, auth)), nil
}

// ProxyQuestionSearch is a verbatim passthrough for GET /api/atoms/questions/search
// → chora-creation at the same path. The downstream returns the canonical
// paginated envelope { items, page, per, total } per
// chora-contracts/openapi/creation-questions.yaml#searchQuestions.
//
// Tenant scoping is handled downstream — chora-creation reads tenant_id from
// the mesh-claim headers (or falls back to the optional ?tenant_id= query
// param for cross-tenant search by a privileged caller).
func (a *Aggregator) ProxyQuestionSearch(ctx context.Context, auth AuthCtx, rawQuery string) (Response, error) {
	u := a.cfg.CreationURL + "/api/atoms/questions/search"
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// -----------------------------------------------------------------------------
// chora-payments admin REST — refund proxy.
//
// The purchases-list (ListPurchaseHistory) + export (ExportPurchases) + SSE
// (StreamPaymentEvents) proxies were RETIRED at the Contextual Transaction
// History cutover (ADR-205 / CHO-1947) — the unified /api/v1/admin/transactions
// surface (chora-tenancy projection) supersedes them. Only the refund proxy
// remains (refund UX re-introduction tracked as CHO-1951).
//
// Role propagation: the JWT-stamped Roles []string is rendered as a comma-
// joined X-Chora-Role header (additive to the canonical x-mesh-user-roles
// stamped by call()) so chora-payments admin_handler's role gate can fast-fail.
// chora-payments owns the policy (PLATFORM_OPERATOR / TENANT_ADMIN / OWNER may
// refund; AUDITOR is read-only → 403 verbatim). 2xx + 4xx pass through verbatim;
// 5xx → 502 GATEWAY_UPSTREAM_5XX; timeout → 504.
// -----------------------------------------------------------------------------

// IssuePaymentRefund proxies POST /api/v1/admin/payments/{purchase_id}/refund
// → chora-payments at the same path. Body forwarded verbatim (the contract's
// IssueRefundRequest carries aggregate_type + optional reason; the downstream
// reads both). rawQuery is forwarded too in case future query params land
// (e.g. ?dry_run=true) — empty in V1.
//
// Empty purchaseID short-circuits 404 BEFORE any outbound call — the gateway
// must never build a downstream URL with an empty path segment.
//
// Status passthrough:
//   - 200 OK                         — refund issued (IssueRefundResponse).
//   - 401 / 403                      — auth gate (verbatim).
//   - 404                            — purchase not found (verbatim).
//   - 409                            — already refunded (verbatim).
//   - 5xx → 502 GATEWAY_UPSTREAM_5XX — the only normalisation.
func (a *Aggregator) IssuePaymentRefund(ctx context.Context, auth AuthCtx, purchaseID, rawQuery string, body []byte) (Response, error) {
	if purchaseID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_PURCHASE_ID_REQUIRED",
			"purchase id required in path: /api/v1/admin/payments/{purchase_id}/refund"), nil
	}
	u := a.cfg.PaymentsURL + "/api/v1/admin/payments/" + url.PathEscape(purchaseID) + "/refund"
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.callWithRoleHeader(ctx, http.MethodPost, u, body, auth)), nil
}

// ExportManaLedger proxies GET /api/v1/me/mana/ledger/export →
// chora-identity GET /api/v1/me/mana/ledger/export, streaming the CSV/NDJSON
// download with Content-Type + Content-Disposition preserved (CHO-1883). The
// generic gatewayproxy passthrough hardcodes JSON + drops downstream headers,
// so a download needs this streaming variant. RLS-scoped to the JWT gcid
// downstream. (Mirrors ExportPurchases — a future refactor could share one
// streaming engine; kept separate here to leave the live payments path
// untouched.)
func (a *Aggregator) ExportManaLedger(ctx context.Context, auth AuthCtx, rawQuery string, w http.ResponseWriter) error {
	if a.cfg.IdentityURL == "" {
		writeStreamingErr(w, http.StatusBadGateway, "GATEWAY_UPSTREAM_NOT_CONFIGURED",
			"identity service URL empty")
		return nil
	}
	upstreamURL := a.cfg.IdentityURL + "/api/v1/me/mana/ledger/export"
	if rawQuery != "" {
		upstreamURL += "?" + rawQuery
	}

	callCtx, cancel := context.WithTimeout(ctx, a.cfg.PaymentsStreamTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, upstreamURL, nil)
	if err != nil {
		writeStreamingErr(w, http.StatusBadGateway, "GATEWAY_UPSTREAM_ERROR",
			fmt.Sprintf("build request: %v", err))
		return nil
	}
	stampAuthHeaders(req, auth)
	stampRoleHeader(req, auth)

	client := &http.Client{Timeout: a.cfg.PaymentsStreamTimeout + 5*time.Second}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			writeStreamingErr(w, http.StatusGatewayTimeout, "GATEWAY_UPSTREAM_TIMEOUT", err.Error())
			return nil
		}
		var urlErr *url.Error
		if errors.As(err, &urlErr) && urlErr.Timeout() {
			writeStreamingErr(w, http.StatusGatewayTimeout, "GATEWAY_UPSTREAM_TIMEOUT", err.Error())
			return nil
		}
		writeStreamingErr(w, http.StatusBadGateway, "GATEWAY_UPSTREAM_ERROR", err.Error())
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		writeStreamingErr(w, http.StatusBadGateway, "GATEWAY_UPSTREAM_5XX",
			fmt.Sprintf("upstream returned %d", resp.StatusCode))
		return nil
	}
	if resp.StatusCode >= 400 {
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		} else {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return nil
	}
	for _, h := range []string{"Content-Type", "Content-Disposition", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return nil
}

// -----------------------------------------------------------------------------
// Open-course flow (ask (b), 2026-05-26) — chora-consumption course-bound
// LearningPath read for the A+ course-learn page.
//
// FE polls this endpoint after a successful enrolment until the
// chora-delivery enrollment.created.v1 → chora-consumption
// EnrollmentCreatedSubscriber has bootstrapped the LearningPath
// (eventual-consistency window typically 5-15s, up to 30s cold-start).
// Downstream 404 = "not yet ready, retry"; the FE poller treats 404 as
// non-fatal until MAX_POLL_ATTEMPTS exhausts.
// -----------------------------------------------------------------------------

// -----------------------------------------------------------------------------
// WS-7b (2026-05-26) — GET /api/atoms/{atomId}/revisions — revision history
// page for the A+ atom-revisions view — pure passthrough to chora-creation.
//
// chora-creation serves listAtomRevisions at the SAME path the FE hits:
// GET /api/atoms/{atom_id}/revisions (per chora-contracts/openapi/
// creation-admin.yaml::listAtomRevisions). The gateway forwards query params
// page_size + page_token verbatim so the downstream cursor-based paging
// parser stays canonical. Body absent (GET only).
//
// Status passthrough (mirrors the rest of this aggregator):
//   - 200 AtomRevisionPage { items, next_page_token }      — verbatim
//   - 401 / 403                                            — verbatim
//   - 404 CREATION_ATOM_NOT_FOUND                          — verbatim
//   - 5xx → 502 GATEWAY_UPSTREAM_5XX                       — only normalisation
//
// An empty atomID short-circuits 404 BEFORE any outbound call — the gateway
// must never build a downstream URL with an empty path segment.
// -----------------------------------------------------------------------------

// ListAtomRevisions proxies GET /api/atoms/{atomId}/revisions →
// chora-creation at the same path. rawQuery is forwarded verbatim so
// page_size + page_token cursor pagination flows to the downstream handler.
// Mesh-trust headers (X-Tenant-Id + gcid + chora-* + x-mesh-user-roles) are
// stamped via the canonical call() helper — chora-creation enforces RLS.
//
// Empty atomID short-circuits 404 BEFORE any outbound call.
func (a *Aggregator) ListAtomRevisions(ctx context.Context, auth AuthCtx, atomID, rawQuery string) (Response, error) {
	if atomID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_ATOM_ID_REQUIRED",
			"atom id required in path: /api/atoms/{atomId}/revisions"), nil
	}
	u := a.cfg.CreationURL + "/api/atoms/" + url.PathEscape(atomID) + "/revisions"
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// -----------------------------------------------------------------------------
// CHO-1618 (2026-06-01) — Personal Collections — pure passthrough to
// chora-creation. chora-creation serves the 7 Collection CRUD routes at the
// SAME path the FE hits (per services/chora-creation/internal/adapter/http/
// collection_handler.go mountCollectionRoutes + chora-contracts/openapi/
// creation-admin.yaml §Collections). The gateway forwards method + path +
// body verbatim — no body reshape, no path rewrite. Mesh-trust headers
// (X-Tenant-Id + lowercase gcid) are stamped via the canonical call() helper
// so chora-creation's tenantContext middleware + RLS resolve the caller.
//
// Routes:
//
//	POST   /api/v1/collections                              CreateCollection
//	GET    /api/v1/me/collections                           ListMyCollections
//	GET    /api/v1/collections/{collectionId}               GetCollection
//	PATCH  /api/v1/collections/{collectionId}               PatchCollection
//	DELETE /api/v1/collections/{collectionId}               DeleteCollection
//	POST   /api/v1/collections/{collectionId}/atoms         AddCollectionAtom
//	DELETE /api/v1/collections/{collectionId}/atoms/{atomId} RemoveCollectionAtom
//
// Status passthrough (mirrors the rest of this aggregator):
//   - 200 / 201 (read / create)                            — verbatim
//   - 204 (delete / remove — NO body)                      — verbatim
//   - 400 (CREATION_INVALID_BODY / CREATION_COLLECTION_INVALID) — verbatim
//   - 401 / 403 (auth / owner gate)                        — verbatim
//   - 404 (collection / atom not found)                    — verbatim
//   - 409 CREATION_COLLECTION_DUPLICATE_ATOM               — verbatim
//   - 422 CREATION_COLLECTION_ATOM_CAP_EXCEEDED            — verbatim
//   - 5xx → 502 GATEWAY_UPSTREAM_5XX                       — only normalisation
//
// Empty collectionId / atomId short-circuit 404 BEFORE any outbound call —
// the gateway must never build a downstream URL with an empty path segment.
// -----------------------------------------------------------------------------

// CreateCollection proxies POST /api/v1/collections → chora-creation at the
// same path. Body forwarded verbatim (CreateCollectionRequest). The owning
// gcid is resolved downstream from the stamped gcid mesh header.
func (a *Aggregator) CreateCollection(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	u := a.cfg.CreationURL + "/api/v1/collections"
	return classify(a.call(ctx, http.MethodPost, u, body, auth)), nil
}

// ListMyCollections proxies GET /api/v1/me/collections → chora-creation at the
// same path. The downstream returns {items, total} scoped to the caller's
// gcid + tenant (soft-deleted rows filtered) — passed through verbatim.
func (a *Aggregator) ListMyCollections(ctx context.Context, auth AuthCtx) (Response, error) {
	u := a.cfg.CreationURL + "/api/v1/me/collections"
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// SearchMyCollections serves GET /api/v1/collections/search?q=&limit= for the
// A+ search-hub collection tab (WS-8). It federates the caller's OWN
// Collections — reusing the same downstream GET /api/v1/me/collections read
// (gcid + tenant scoped, soft-deleted rows filtered) — and applies a
// case-insensitive title substring filter, capped at `limit`. atom_count is
// derived from the hydrated `atoms` array the list endpoint already returns.
//
// SCOPE (MVP): caller-owned collections only. Cross-tenant PUBLIC-collection
// discovery is a later Meilisearch-backed upgrade (a `collections` index fed
// by chora.creation.collection.created.v1 — WS-8 backlog). This endpoint is
// deliberately ADDITIVE: it touches no chora-creation code, so the search tab
// ships with only a gateway redeploy (no chora-creation revert risk).
//
// The FE shape is {items:[{id,title,atom_count}]} (owner_display_name is
// optional and intentionally omitted — it lives in chora_identity and cannot
// be resolved here without a cross-DB query, which is forbidden).
func (a *Aggregator) SearchMyCollections(ctx context.Context, auth AuthCtx, q string, limit int) (Response, error) {
	resp := classify(a.call(ctx, http.MethodGet, a.cfg.CreationURL+"/api/v1/me/collections", nil, auth))
	if resp.Status != http.StatusOK {
		// Propagate the upstream verdict verbatim (401/403/normalised-5xx) so
		// the FE surfaces auth/upstream errors rather than a misleading empty list.
		return resp, nil
	}

	var list struct {
		Items []struct {
			CollectionID string            `json:"collection_id"`
			Title        string            `json:"title"`
			Atoms        []json.RawMessage `json:"atoms"`
		} `json:"items"`
	}
	if err := json.Unmarshal(resp.Body, &list); err != nil {
		return errResp(http.StatusBadGateway, "GATEWAY_UPSTREAM_DECODE",
			"failed to decode collections list: "+err.Error()), nil
	}

	needle := strings.ToLower(strings.TrimSpace(q))
	type collectionHit struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		AtomCount int    `json:"atom_count"`
	}
	items := make([]collectionHit, 0, len(list.Items))
	for _, it := range list.Items {
		if needle != "" && !strings.Contains(strings.ToLower(it.Title), needle) {
			continue
		}
		items = append(items, collectionHit{
			ID:        it.CollectionID,
			Title:     it.Title,
			AtomCount: len(it.Atoms),
		})
		if limit > 0 && len(items) >= limit {
			break
		}
	}

	out, err := json.Marshal(struct {
		Items []collectionHit `json:"items"`
	}{Items: items})
	if err != nil {
		return errResp(http.StatusInternalServerError, "GATEWAY_MARSHAL_ERROR", err.Error()), nil
	}
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")
	return Response{Status: http.StatusOK, Headers: hdr, Body: out}, nil
}

// GetCollection proxies GET /api/v1/collections/{collectionId} → chora-creation
// at the same path. The downstream is visibility-gated (404s rows the caller
// cannot read). Empty collectionID short-circuits 404 BEFORE any outbound call.
func (a *Aggregator) GetCollection(ctx context.Context, auth AuthCtx, collectionID string) (Response, error) {
	if collectionID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_COLLECTION_ID_REQUIRED",
			"collection id required in path: /api/v1/collections/{collectionId}"), nil
	}
	u := a.cfg.CreationURL + "/api/v1/collections/" + url.PathEscape(collectionID)
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// PatchCollection proxies PATCH /api/v1/collections/{collectionId} →
// chora-creation at the same path. Body forwarded verbatim
// (PatchCollectionRequest). Owner gate enforced downstream (403 when
// gcid != owner_gcid). Empty collectionID short-circuits 404.
func (a *Aggregator) PatchCollection(ctx context.Context, auth AuthCtx, collectionID string, body []byte) (Response, error) {
	if collectionID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_COLLECTION_ID_REQUIRED",
			"collection id required in path: /api/v1/collections/{collectionId}"), nil
	}
	u := a.cfg.CreationURL + "/api/v1/collections/" + url.PathEscape(collectionID)
	return classify(a.call(ctx, http.MethodPatch, u, body, auth)), nil
}

// DeleteCollection proxies DELETE /api/v1/collections/{collectionId} →
// chora-creation at the same path. Soft-delete (owner gate downstream). The
// 204 + empty body pass through verbatim — the gateway MUST NOT synthesise an
// envelope. Empty collectionID short-circuits 404.
func (a *Aggregator) DeleteCollection(ctx context.Context, auth AuthCtx, collectionID string) (Response, error) {
	if collectionID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_COLLECTION_ID_REQUIRED",
			"collection id required in path: /api/v1/collections/{collectionId}"), nil
	}
	u := a.cfg.CreationURL + "/api/v1/collections/" + url.PathEscape(collectionID)
	return classify(a.call(ctx, http.MethodDelete, u, nil, auth)), nil
}

// AddCollectionAtom proxies POST /api/v1/collections/{collectionId}/atoms →
// chora-creation at the same path. Body forwarded verbatim
// (AddCollectionAtomRequest). The downstream validates atom existence + the
// 500-atom cap + cross-tenant visibility rules. Empty collectionID
// short-circuits 404.
func (a *Aggregator) AddCollectionAtom(ctx context.Context, auth AuthCtx, collectionID string, body []byte) (Response, error) {
	if collectionID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_COLLECTION_ID_REQUIRED",
			"collection id required in path: /api/v1/collections/{collectionId}/atoms"), nil
	}
	u := a.cfg.CreationURL + "/api/v1/collections/" + url.PathEscape(collectionID) + "/atoms"
	return classify(a.call(ctx, http.MethodPost, u, body, auth)), nil
}

// RemoveCollectionAtom proxies DELETE
// /api/v1/collections/{collectionId}/atoms/{atomId} → chora-creation at the
// same path. The 204 + empty body pass through verbatim. Empty collectionID
// or atomID short-circuits 404 BEFORE any outbound call.
func (a *Aggregator) RemoveCollectionAtom(ctx context.Context, auth AuthCtx, collectionID, atomID string) (Response, error) {
	if collectionID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_COLLECTION_ID_REQUIRED",
			"collection id required in path: /api/v1/collections/{collectionId}/atoms/{atomId}"), nil
	}
	if atomID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_ATOM_ID_REQUIRED",
			"atom id required in path: /api/v1/collections/{collectionId}/atoms/{atomId}"), nil
	}
	u := a.cfg.CreationURL + "/api/v1/collections/" + url.PathEscape(collectionID) + "/atoms/" + url.PathEscape(atomID)
	return classify(a.call(ctx, http.MethodDelete, u, nil, auth)), nil
}

// ConvertCollectionToStudyList proxies POST
// /api/v1/collections/{collectionId}/convert-to-study-list → chora-creation at
// the SAME path (WS-4, ADR-233). Verbatim passthrough — no body reshape, no
// path rewrite. Mirrors AssembleQuestionBankTestSet exactly (same single-verb
// leaf shape under an already-claimed parametric subtree).
//
// chora-creation owns the whole transition: it re-evaluates the ADR-229 reuse
// disjunct per atom against the CALLER's gcid (stamped by the canonical call()
// helper as the lowercase `gcid` mesh header), mints the GRANT_SCOPE_COLLECTION
// grants on the tenant-visible leg, and emits the study-list event. The gateway
// adds nothing but the mesh-trust headers.
//
// Status passthrough (ADR-233 D11) — every verdict is a DOMAIN verdict and must
// survive byte-for-byte:
//   - 201 → {study_list_event_id, atom_count, excluded[{atom_id, reason}]}
//     PARTIAL SUCCESS: entitled atoms convert; atoms the author has since
//     restricted are dropped and NAMED. The excluded[] list is the learner's
//     only account of what was left behind — the bridge MUST NOT reshape or
//     drop it (contrast CompanionBridge, whose classify() camelises whole
//     bodies; this aggregator's classify() is a verbatim passthrough).
//   - 409 CREATION_COLLECTION_NO_ENTITLED_ATOMS → zero survivors. Never an
//     empty study list — that would be fabricating a success.
//   - 404 CREATION_COLLECTION_NOT_FOUND → the actor may not VIEW the collection
//     (CHO-2165 landed ADR-233 D9's reserved policy: a non-owner who CAN see a
//     collection may now FORK it, so the refusal is for the ones they cannot).
//     404, never 403 — a 403 would confirm the existence of a collection the
//     caller may not see. Do not "helpfully" restore a 403 here.
//   - 5xx → 502 GATEWAY_UPSTREAM_5XX (the only normalisation).
//
// The Collection is NOT mutated downstream, so this route is safely retryable.
// Empty collectionID short-circuits 404 BEFORE any outbound call.
func (a *Aggregator) ConvertCollectionToStudyList(ctx context.Context, auth AuthCtx, collectionID string, body []byte) (Response, error) {
	if collectionID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_COLLECTION_ID_REQUIRED",
			"collection id required in path: /api/v1/collections/{collectionId}/convert-to-study-list"), nil
	}
	u := a.cfg.CreationURL + "/api/v1/collections/" + url.PathEscape(collectionID) + "/convert-to-study-list"
	return classify(a.call(ctx, http.MethodPost, u, body, auth)), nil
}

// -----------------------------------------------------------------------------
// W3.B.1 (2026-06-28) — QuestionBank curation — pure passthrough to
// chora-creation. chora-creation serves the QuestionBank CRUD + assemble routes
// at the SAME path the FE hits (per services/chora-creation/internal/adapter/
// http/question_bank_handler.go mountQuestionBankRoutes). The gateway forwards
// method + path + body verbatim — no body reshape, no path rewrite. Mesh-trust
// headers (X-Tenant-Id + lowercase gcid) are stamped via the canonical call()
// helper so chora-creation's tenantContext middleware + RLS resolve the caller.
// Mirrors the CHO-1618 Collections passthrough EXACTLY.
//
// Routes:
//
//	POST   /api/v1/question-banks                            CreateQuestionBank
//	GET    /api/v1/me/question-banks                         ListMyQuestionBanks
//	GET    /api/v1/question-banks/{id}                       GetQuestionBank
//	PATCH  /api/v1/question-banks/{id}                       PatchQuestionBank
//	DELETE /api/v1/question-banks/{id}                       DeleteQuestionBank
//	POST   /api/v1/question-banks/{id}/questions             AddQuestionBankQuestion
//	GET    /api/v1/question-banks/{id}/questions             ListQuestionBankQuestions
//	DELETE /api/v1/question-banks/{id}/questions/{qid}       RemoveQuestionBankQuestion
//	POST   /api/v1/question-banks/{id}/assemble-test-set     AssembleQuestionBankTestSet
//
// Status passthrough (mirrors the rest of this aggregator):
//   - 200 / 201 / 202 (read / create / assemble)            — verbatim
//   - 204 (delete / remove — NO body)                       — verbatim
//   - 400 / 401 / 403 / 404 / 409 / 422 (domain verdicts)   — verbatim
//   - 5xx → 502 GATEWAY_UPSTREAM_5XX                        — only normalisation
//
// Empty {id} / {qid} short-circuit 404 BEFORE any outbound call — the gateway
// must never build a downstream URL with an empty path segment.
// -----------------------------------------------------------------------------

// CreateQuestionBank proxies POST /api/v1/question-banks → chora-creation at the
// same path. Body forwarded verbatim (createQuestionBankRequest). The owning
// gcid is resolved downstream from the stamped gcid mesh header.
func (a *Aggregator) CreateQuestionBank(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	u := a.cfg.CreationURL + "/api/v1/question-banks"
	return classify(a.call(ctx, http.MethodPost, u, body, auth)), nil
}

// ListMyQuestionBanks proxies GET /api/v1/me/question-banks → chora-creation at
// the same path. The downstream returns {items, total} scoped to the caller's
// gcid + tenant (soft-deleted rows filtered) — passed through verbatim.
func (a *Aggregator) ListMyQuestionBanks(ctx context.Context, auth AuthCtx) (Response, error) {
	u := a.cfg.CreationURL + "/api/v1/me/question-banks"
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// GetQuestionBank proxies GET /api/v1/question-banks/{id} → chora-creation at
// the same path. The downstream is visibility-gated (404s rows the caller
// cannot read). Empty id short-circuits 404 BEFORE any outbound call.
func (a *Aggregator) GetQuestionBank(ctx context.Context, auth AuthCtx, id string) (Response, error) {
	if id == "" {
		return errResp(http.StatusNotFound, "GATEWAY_QUESTION_BANK_ID_REQUIRED",
			"question bank id required in path: /api/v1/question-banks/{id}"), nil
	}
	u := a.cfg.CreationURL + "/api/v1/question-banks/" + url.PathEscape(id)
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// PatchQuestionBank proxies PATCH /api/v1/question-banks/{id} → chora-creation
// at the same path. Body forwarded verbatim (patchQuestionBankRequest). Owner
// gate enforced downstream (403 when gcid != owner_gcid). Empty id
// short-circuits 404.
func (a *Aggregator) PatchQuestionBank(ctx context.Context, auth AuthCtx, id string, body []byte) (Response, error) {
	if id == "" {
		return errResp(http.StatusNotFound, "GATEWAY_QUESTION_BANK_ID_REQUIRED",
			"question bank id required in path: /api/v1/question-banks/{id}"), nil
	}
	u := a.cfg.CreationURL + "/api/v1/question-banks/" + url.PathEscape(id)
	return classify(a.call(ctx, http.MethodPatch, u, body, auth)), nil
}

// DeleteQuestionBank proxies DELETE /api/v1/question-banks/{id} → chora-creation
// at the same path. Soft-delete (owner gate downstream). The 204 + empty body
// pass through verbatim — the gateway MUST NOT synthesise an envelope. Empty id
// short-circuits 404.
func (a *Aggregator) DeleteQuestionBank(ctx context.Context, auth AuthCtx, id string) (Response, error) {
	if id == "" {
		return errResp(http.StatusNotFound, "GATEWAY_QUESTION_BANK_ID_REQUIRED",
			"question bank id required in path: /api/v1/question-banks/{id}"), nil
	}
	u := a.cfg.CreationURL + "/api/v1/question-banks/" + url.PathEscape(id)
	return classify(a.call(ctx, http.MethodDelete, u, nil, auth)), nil
}

// AddQuestionBankQuestion proxies POST /api/v1/question-banks/{id}/questions →
// chora-creation at the same path. Body forwarded verbatim (addQuestionRequest).
// The downstream validates question existence + the item cap + duplicate rules.
// Empty id short-circuits 404.
func (a *Aggregator) AddQuestionBankQuestion(ctx context.Context, auth AuthCtx, id string, body []byte) (Response, error) {
	if id == "" {
		return errResp(http.StatusNotFound, "GATEWAY_QUESTION_BANK_ID_REQUIRED",
			"question bank id required in path: /api/v1/question-banks/{id}/questions"), nil
	}
	u := a.cfg.CreationURL + "/api/v1/question-banks/" + url.PathEscape(id) + "/questions"
	return classify(a.call(ctx, http.MethodPost, u, body, auth)), nil
}

// ListQuestionBankQuestions proxies GET /api/v1/question-banks/{id}/questions →
// chora-creation at the same path. The downstream returns {items, total} for the
// bank's active items (visibility-gated). Empty id short-circuits 404.
func (a *Aggregator) ListQuestionBankQuestions(ctx context.Context, auth AuthCtx, id, rawQuery string) (Response, error) {
	if id == "" {
		return errResp(http.StatusNotFound, "GATEWAY_QUESTION_BANK_ID_REQUIRED",
			"question bank id required in path: /api/v1/question-banks/{id}/questions"), nil
	}
	u := a.cfg.CreationURL + "/api/v1/question-banks/" + url.PathEscape(id) + "/questions"
	// Forward the server-side filter/sort/page params (q, question_type, sort,
	// page, page_size) verbatim — chora-creation whitelists + parameterises them
	// (CHO-1926). Without this the workbench list ignores all filters.
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// RemoveQuestionBankQuestion proxies DELETE
// /api/v1/question-banks/{id}/questions/{qid} → chora-creation at the same path.
// The 204 + empty body pass through verbatim. Empty id or qid short-circuits
// 404 BEFORE any outbound call.
func (a *Aggregator) RemoveQuestionBankQuestion(ctx context.Context, auth AuthCtx, id, questionID string) (Response, error) {
	if id == "" {
		return errResp(http.StatusNotFound, "GATEWAY_QUESTION_BANK_ID_REQUIRED",
			"question bank id required in path: /api/v1/question-banks/{id}/questions/{qid}"), nil
	}
	if questionID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_QUESTION_ID_REQUIRED",
			"question id required in path: /api/v1/question-banks/{id}/questions/{qid}"), nil
	}
	u := a.cfg.CreationURL + "/api/v1/question-banks/" + url.PathEscape(id) + "/questions/" + url.PathEscape(questionID)
	return classify(a.call(ctx, http.MethodDelete, u, nil, auth)), nil
}

// AssembleQuestionBankTestSet proxies POST
// /api/v1/question-banks/{id}/assemble-test-set → chora-creation at the same
// path. Body forwarded verbatim (assembleTestSetRequest). The downstream
// publishes the question_batch.accepted.v1 event and returns 202
// {job_id, test_set_status}. Empty id short-circuits 404.
func (a *Aggregator) AssembleQuestionBankTestSet(ctx context.Context, auth AuthCtx, id string, body []byte) (Response, error) {
	if id == "" {
		return errResp(http.StatusNotFound, "GATEWAY_QUESTION_BANK_ID_REQUIRED",
			"question bank id required in path: /api/v1/question-banks/{id}/assemble-test-set"), nil
	}
	u := a.cfg.CreationURL + "/api/v1/question-banks/" + url.PathEscape(id) + "/assemble-test-set"
	return classify(a.call(ctx, http.MethodPost, u, body, auth)), nil
}

// ReorderQuestionBank proxies POST
// /api/v1/question-banks/{id}/reorder → chora-creation at the same path
// (Phase-A.2 Wave 2, 2026-06-28). Body forwarded verbatim
// ({question_ids:[...full ordered list...]}). The downstream reassigns item
// positions and returns 200 + the updated bank. Empty id short-circuits 404.
func (a *Aggregator) ReorderQuestionBank(ctx context.Context, auth AuthCtx, id string, body []byte) (Response, error) {
	if id == "" {
		return errResp(http.StatusNotFound, "GATEWAY_QUESTION_BANK_ID_REQUIRED",
			"question bank id required in path: /api/v1/question-banks/{id}/reorder"), nil
	}
	u := a.cfg.CreationURL + "/api/v1/question-banks/" + url.PathEscape(id) + "/reorder"
	return classify(a.call(ctx, http.MethodPost, u, body, auth)), nil
}

// MeLearningPaths proxies GET /api/v1/me/learning-paths[?course_id=X] →
// chora-consumption GET /v1/me/learning-paths[?course_id=X] (path
// translated; rawQuery forwarded verbatim).
//
// When course_id is present the downstream returns the single hydrated
// path with atoms[] (titles from local atom_index) + per-atom state
// derived from CurrentIndex. Without course_id the downstream lists
// every path owned by the learner across courses.
//
// 404 PATH_NOT_BOOTSTRAPPED passes through verbatim — the FE poller
// treats it as "retry until subscriber lands".
func (a *Aggregator) MeLearningPaths(ctx context.Context, auth AuthCtx, rawQuery string) (Response, error) {
	u := a.cfg.ConsumptionURL + "/v1/me/learning-paths"
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// -----------------------------------------------------------------------------
// W6 StudentTranscript read-model (Four-Mode plan "Outcome spine") — GET-only
// passthrough for the two BFF routes chora-consumption's transcript_handler.go
// serves at the bare (no /api prefix) path, mirroring MeLearningPaths:
//
//	GET /api/v1/me/transcript[?limit=]                       → chora-consumption
//	                                                            GET /v1/me/transcript
//	GET /api/v1/transcript/by-assessments?assessment_ids=... → chora-consumption
//	                                                            GET /v1/transcript/by-assessments
// -----------------------------------------------------------------------------

// MeTranscript proxies GET /api/v1/me/transcript → chora-consumption GET
// /v1/me/transcript (self-scoped; the downstream's extRequireContext derives
// tenant_id + gcid from the X-Tenant-Id + lowercase gcid headers call()
// already stamps). rawQuery forwards the optional ?limit= verbatim. 503
// TRANSCRIPT_NOT_WIRED is an intentional fail-loud contract code — classify()
// passes it through unchanged (mirrors the CREATION_JOBS_NOT_WIRED carve-out).
func (a *Aggregator) MeTranscript(ctx context.Context, auth AuthCtx, rawQuery string) (Response, error) {
	u := a.cfg.ConsumptionURL + "/v1/me/transcript"
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// MeTranscriptEntryAction proxies the transcript entry-action subtree
// /api/v1/me/transcript/{entryID}:seen → chora-consumption
// /v1/me/transcript/{entryID}:seen (UX refactor B6 item 2 follow-up).
//
// The inbound path is translated by stripping the "/api" prefix and nothing
// else, so the entry id AND the ":seen" action suffix reach the downstream
// exactly as the caller sent them. Method and body ride along untouched:
// chora-consumption owns the action rule (unknown suffix 404, non-POST 405)
// and is the only place it is written down.
//
// The learner is not derived here. The downstream reads the verified gcid off
// the mesh headers call() stamps, which is what stops one learner marking
// another learner's result as read inside the same tenant.
func (a *Aggregator) MeTranscriptEntryAction(ctx context.Context, auth AuthCtx, method, reqPath, rawQuery string, body []byte) (Response, error) {
	u := a.cfg.ConsumptionURL + strings.TrimPrefix(reqPath, "/api")
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.call(ctx, method, u, body, auth)), nil
}

// TranscriptByAssessments proxies GET /api/v1/transcript/by-assessments →
// chora-consumption GET /v1/transcript/by-assessments (tenant-wide R+
// gradebook join). rawQuery forwards the required ?assessment_ids= CSV
// verbatim. The downstream's hasTranscriptAdminRole gate fail-closes on
// x-mesh-user-roles (instructor / admin / training-admin / tenant_admin) —
// call() already stamps that header from auth.Roles, the same mesh-trust
// propagation ProxyMeAssessments relies on, so a caller lacking the role
// gets a verbatim 403 passed through from chora-consumption.
func (a *Aggregator) TranscriptByAssessments(ctx context.Context, auth AuthCtx, rawQuery string) (Response, error) {
	u := a.cfg.ConsumptionURL + "/v1/transcript/by-assessments"
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// MeCourseContent serves the A+ open-course heterogeneous-curriculum read
// (CHO-1612). reqPath is the inbound BFF path /api/v1/me/courses/{id}/content;
// the /api prefix is stripped so the downstream /v1/me/courses/{id}/content
// route matches.
//
// ADR-185 — the curriculum is a chora-consumption projection that returns gs://
// media refs RAW (consumption cannot sign chora-delivery's bucket; cross-DB
// forbidden). When delivery is configured, this fans out IN PARALLEL to the
// chora-delivery learner-media signed-GET endpoint
// (/v1/me/courses/{id}/content/media-urls) and swaps gs://→signed `ref` by
// item_id (preserving the durable gs:// URI under `object_ref`). The curriculum
// is authoritative: on any delivery error/403/unconfigured, the media refs are
// left RAW and the curriculum is returned 200 — fail-visible, never blanked.
func (a *Aggregator) MeCourseContent(ctx context.Context, auth AuthCtx, reqPath string) (Response, error) {
	suffix := strings.TrimPrefix(reqPath, "/api") // /v1/me/courses/{id}/content
	curriculumURL := a.cfg.ConsumptionURL + suffix

	// No delivery configured → bare projection proxy (no media enrichment).
	if a.cfg.DeliveryURL == "" {
		return classify(a.call(ctx, http.MethodGet, curriculumURL, nil, auth)), nil
	}
	mediaURL := a.cfg.DeliveryURL + suffix + "/media-urls"

	// Fan out in parallel — the media call needs nothing from the curriculum.
	var (
		curRes, medRes callResult
		wg             sync.WaitGroup
	)
	wg.Add(2)
	go func() { defer wg.Done(); curRes = a.call(ctx, http.MethodGet, curriculumURL, nil, auth) }()
	go func() { defer wg.Done(); medRes = a.call(ctx, http.MethodGet, mediaURL, nil, auth) }()
	wg.Wait()

	// Curriculum is authoritative — surface its error verbatim if it failed.
	curResp := classify(curRes)
	if curResp.Status != http.StatusOK {
		return curResp, nil
	}

	// Best-effort: collect the signed-media map. Any delivery failure ⇒ leave
	// refs raw (fail-visible).
	resolved := map[string]string{}
	if medRes.err == nil && medRes.status == http.StatusOK {
		var med struct {
			Resolved map[string]string `json:"resolved"`
		}
		if err := json.Unmarshal(medRes.body, &med); err == nil && med.Resolved != nil {
			resolved = med.Resolved
		}
	} else {
		log.Printf("gatewayproxy: MeCourseContent media-urls unavailable (status=%d err=%v) — leaving refs raw",
			medRes.status, medRes.err)
	}
	if len(resolved) == 0 {
		return curResp, nil // nothing to merge
	}

	// Merge by item_id, preserving every other curriculum field (decode into a
	// generic map so unknown top-level/item fields survive the round-trip).
	var doc map[string]any
	if err := json.Unmarshal(curResp.Body, &doc); err != nil {
		return curResp, nil // unparseable — pass through unmerged
	}
	items, _ := doc["items"].([]any)
	for _, raw := range items {
		it, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := it["item_id"].(string)
		if signed, ok := resolved[id]; ok {
			it["object_ref"] = it["ref"]
			it["ref"] = signed
		}
	}
	merged, err := json.Marshal(doc)
	if err != nil {
		return curResp, nil // re-marshal failed — pass through unmerged
	}
	return Response{Status: http.StatusOK, Body: merged, Headers: curResp.Headers}, nil
}

// ProxyGrowthEdges is a verbatim passthrough for /api/v1/me/growth-edges[/...]
// → chora-consumption /v1/me/growth-edges[/...] (Epic-1b W8 A+ Growth Edges).
// The /api prefix is stripped (consumption serves the EXT-scope routes at
// /v1/me/..., mirroring MeCourseContent). Covers the 4 routes in
// chora-contracts/openapi/consumption-growth-edges.yaml:
//
//	GET    /api/v1/me/growth-edges               list (filter/sort/page query)
//	GET    /api/v1/me/growth-edges/{id}          read one
//	DELETE /api/v1/me/growth-edges/{id}          dismiss (soft-delete)
//	POST   /api/v1/me/growth-edges/uploads       multipart upload (202)
//	GET    /api/v1/me/growth-edges/uploads       ?status=awaiting_review, the
//	                                             learner's parked diagnoses (B6)
//	GET    /api/v1/me/growth-edges/uploads/{id}  poll the async analysis job
//
// method + rawQuery + body + Content-Type are forwarded verbatim;
// callWithContentType preserves the multipart boundary for the uploads POST.
// Learner scoping is delegated downstream — chora-consumption reads X-Tenant-Id
// + lowercase gcid off the stamped mesh claims.
func (a *Aggregator) ProxyGrowthEdges(ctx context.Context, auth AuthCtx, method, path, rawQuery string, body []byte, contentType string) (Response, error) {
	u := a.cfg.ConsumptionURL + strings.TrimPrefix(path, "/api")
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.callWithContentType(ctx, method, u, contentType, body, auth)), nil
}

// ProxyPracticeBudget is a verbatim GET passthrough for
// /api/v1/me/practice-budget → chora-consumption /v1/me/practice-budget
// (B6 item 3).
//
// No body parameter, deliberately: this is a read, and a proxy that accepted
// one would forward a body the downstream handler ignores on a GET and reject
// nothing that ought to be rejected. Learner scoping is delegated downstream,
// which reads the identity off the stamped mesh claims.
func (a *Aggregator) ProxyPracticeBudget(ctx context.Context, auth AuthCtx, method, path, rawQuery string) (Response, error) {
	u := a.cfg.ConsumptionURL + strings.TrimPrefix(path, "/api")
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.callWithContentType(ctx, method, u, "", nil, auth)), nil
}

// ProxyGoals is a verbatim passthrough for /api/v1/me/goals[/{id}] →
// chora-consumption /v1/me/goals[/{id}] (ADR-204 learner-owned Goal, A+
// Discovery). The /api prefix is stripped (consumption serves the EXT-scope
// routes at /v1/me/..., mirroring ProxyGrowthEdges). Covers the 3 routes in the
// Goal contract:
//
//	GET   /api/v1/me/goals        list {items, primaryLens}
//	POST  /api/v1/me/goals        create
//	PATCH /api/v1/me/goals/{id}   update (status / companion attach-detach)
//
// method + rawQuery + body + Content-Type are forwarded verbatim;
// callWithContentType stamps the canonical mesh-trust headers. Learner scoping
// is delegated downstream — chora-consumption reads X-Tenant-Id + lowercase
// gcid off the stamped mesh claims and derives the primaryLens from the
// caller's goals.
func (a *Aggregator) ProxyGoals(ctx context.Context, auth AuthCtx, method, path, rawQuery string, body []byte, contentType string) (Response, error) {
	u := a.cfg.ConsumptionURL + strings.TrimPrefix(path, "/api")
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.callWithContentType(ctx, method, u, contentType, body, auth)), nil
}

// ProxyMaps is a verbatim (GET-only) passthrough for /api/v1/me/maps[/...] →
// chora-consumption /v1/me/maps[/...] (WS-B, My Knowledge Atlas — a map = a Goal,
// ADR-214 D1). The /api prefix is stripped, mirroring ProxyGoals. Serves:
//
//	GET /api/v1/me/maps                 list the learner's maps + per-map counts
//	GET /api/v1/me/maps/{goalId}/graph  the map's root-scoped painted subgraph
//
// method + rawQuery are forwarded verbatim; callWithContentType stamps the
// canonical mesh-trust headers. Learner scoping is delegated downstream —
// chora-consumption reads X-Tenant-Id + lowercase gcid off the stamped mesh claims.
func (a *Aggregator) ProxyMaps(ctx context.Context, auth AuthCtx, method, path, rawQuery string, body []byte, contentType string) (Response, error) {
	u := a.cfg.ConsumptionURL + strings.TrimPrefix(path, "/api")
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.callWithContentType(ctx, method, u, contentType, body, auth)), nil
}

// ProxyDosePreferences is a verbatim passthrough for /api/v1/me/dose-preferences
// → chora-consumption /v1/me/dose-preferences (CHO-2045 / ADR-224 — GET read +
// PUT replace). The /api prefix is stripped, mirroring ProxyGoals; method +
// rawQuery + body + Content-Type are forwarded verbatim, and callWithContentType
// stamps the canonical mesh-trust headers. Learner scoping is delegated
// downstream — chora-consumption reads X-Tenant-Id + lowercase gcid off the
// stamped mesh claims.
func (a *Aggregator) ProxyDosePreferences(ctx context.Context, auth AuthCtx, method, path, rawQuery string, body []byte, contentType string) (Response, error) {
	u := a.cfg.ConsumptionURL + strings.TrimPrefix(path, "/api")
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.callWithContentType(ctx, method, u, contentType, body, auth)), nil
}

// ProxyAITransparency is a verbatim passthrough for
// /api/v1/me/ai-transparency[/acknowledge] → chora-governance
// /v1/me/ai-transparency[/acknowledge] (ADR-225 — GET must_acknowledge + copy;
// POST .../acknowledge writes the evidence row + emits the event). The /api
// prefix is stripped, mirroring ProxyGoals; method + rawQuery + body +
// Content-Type are forwarded verbatim, and callWithContentType stamps the
// canonical mesh-trust headers. Learner scoping is delegated downstream —
// chora-governance reads X-Tenant-Id + lowercase gcid off the stamped mesh claims.
func (a *Aggregator) ProxyAITransparency(ctx context.Context, auth AuthCtx, method, path, rawQuery string, body []byte, contentType string) (Response, error) {
	u := a.cfg.GovernanceURL + strings.TrimPrefix(path, "/api")
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.callWithContentType(ctx, method, u, contentType, body, auth)), nil
}

// ProxyConceptGraph is a verbatim passthrough for
// /api/v1/me/concept-graph[/...] → chora-consumption /v1/me/concept-graph[/...]
// (the learner-sovereign Discovery Knowledge Graph, A+ Discovery). The /api
// prefix is stripped (consumption serves the EXT-scope routes at /v1/me/...,
// mirroring ProxyGoals / ProxyGrowthEdges). ONE method serves every route shape
// — chora-consumption's own router does the sub-route dispatch:
//
//	GET    /api/v1/me/concept-graph               list {concepts, edges, rootConceptId}
//	POST   /api/v1/me/concept-graph/reroot        re-root the hierarchy
//	POST   /api/v1/me/concept-graph/concepts      create a concept (empty-ok)
//	PATCH  /api/v1/me/concept-graph/concepts/{id} rename / re-parent
//	DELETE /api/v1/me/concept-graph/concepts/{id} remove a concept
//	POST   /api/v1/me/concept-graph/edges         add an edge
//	DELETE /api/v1/me/concept-graph/edges/{id}    remove an edge
//
// method + rawQuery + body + Content-Type are forwarded verbatim;
// callWithContentType stamps the canonical mesh-trust headers. Learner
// sovereignty is delegated downstream — chora-consumption reads X-Tenant-Id +
// lowercase gcid off the stamped mesh claims and scopes the per-user graph.
func (a *Aggregator) ProxyConceptGraph(ctx context.Context, auth AuthCtx, method, path, rawQuery string, body []byte, contentType string) (Response, error) {
	u := a.cfg.ConsumptionURL + strings.TrimPrefix(path, "/api")
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.callWithContentType(ctx, method, u, contentType, body, auth)), nil
}

// callWithRoleHeader is the call() variant that ALSO stamps a comma-joined
// X-Chora-Role header on the outbound request alongside the canonical mesh
// metadata. Used by the H+ tx-history routes because chora-payments
// admin_handler reads X-Chora-Role directly per Agent A1 coordination
// (see the Phase 2 wave brief).
//
// The header is omitted when auth.Roles is empty so the downstream's
// default role gate (which falls back to the mesh RoleSummary) still fires.
func (a *Aggregator) callWithRoleHeader(ctx context.Context, method, urlStr string, body []byte, auth AuthCtx) callResult {
	if urlStr == "" {
		return callResult{err: fmt.Errorf("gatewayproxy: empty url for %s", method)}
	}
	callCtx, cancel := context.WithTimeout(ctx, a.cfg.PerCallTimeout)
	defer cancel()

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(callCtx, method, urlStr, bodyReader)
	if err != nil {
		return callResult{err: fmt.Errorf("gatewayproxy: build request: %w", err)}
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	stampAuthHeaders(req, auth)
	stampRoleHeader(req, auth)

	resp, err := a.client.Do(req)
	if err != nil {
		return callResult{err: err}
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return callResult{status: resp.StatusCode, body: out, header: resp.Header}
}

// stampAuthHeaders factors the canonical Bearer + traceparent + tenant +
// gcid + mesh-trust headers out of call() so the SSE + export streaming
// paths can reuse them without going through the call/classify helpers.
func stampAuthHeaders(req *http.Request, auth AuthCtx) {
	if auth.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+auth.Bearer)
	}
	if auth.Traceparent != "" {
		req.Header.Set("traceparent", auth.Traceparent)
	}
	if auth.TenantID != "" {
		req.Header.Set("X-Tenant-Id", auth.TenantID)
	}
	if auth.GCID != "" {
		req.Header.Set("X-Chora-GCID", auth.GCID)
		req.Header.Set("gcid", auth.GCID)
	}
	if auth.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", auth.IdempotencyKey)
	}
	mesh := servicemesh.MarshalToHeaders(servicemesh.MeshClaims{
		GCID:        auth.GCID,
		TenantID:    auth.TenantID,
		Roles:       auth.Roles,
		RoleSummary: auth.RoleSummary,
	})
	for k, vs := range mesh {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
}

// stampRoleHeader writes the comma-joined X-Chora-Role header read by
// chora-payments admin_handler. Omitted when auth.Roles is empty so the
// downstream's mesh-claims fallback path still fires.
//
// Roles are canonicalised to the uppercase forms chora-payments
// ParseAdminRole matches case-sensitively (TENANT_ADMIN / OWNER /
// AUDITOR / PLATFORM_OPERATOR). The chora-identity → gateway mint flow
// forwards JWT roles VERBATIM, and chora_tenancy.members.role is the
// lowercase enum {learner, instructor, admin, auditor}, so a real JWT
// carries lowercase "admin" — which would otherwise fail ParseAdminRole
// and 403 the H+ tx-history endpoints. Map the lowercase DB-enum roles
// to their canonical admin equivalents here, at the single stamping
// boundary, until the mint canonicalises upstream.
func stampRoleHeader(req *http.Request, auth AuthCtx) {
	if len(auth.Roles) == 0 {
		return
	}
	out := make([]string, 0, len(auth.Roles))
	for _, r := range auth.Roles {
		out = append(out, canonicaliseAdminRole(r))
	}
	req.Header.Set("X-Chora-Role", strings.Join(out, ","))
}

// canonicaliseAdminRole maps a JWT role string to the canonical
// X-Chora-Role token chora-payments recognises. Already-canonical
// uppercase tokens and unknown roles pass through unchanged (payments
// silently ignores unrecognised roles).
func canonicaliseAdminRole(role string) string {
	switch strings.ToLower(role) {
	case "admin":
		return "TENANT_ADMIN"
	case "owner":
		return "OWNER"
	case "auditor":
		return "AUDITOR"
	case "platform_operator", "platform-operator":
		return "PLATFORM_OPERATOR"
	}
	return role
}

// writeStreamingErr writes a JSON error envelope on the wire. Used by the
// export streaming path for error status codes returned BEFORE the body has
// been started (otherwise the headers can no longer be modified).
func writeStreamingErr(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	body := fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, code, message)
	_, _ = w.Write([]byte(body))
}

// callWithContentType is the call() variant that stamps a caller-supplied
// Content-Type on the outbound request. Used by CreateQuestionJob so the
// multipart/form-data boundary can be preserved verbatim through the proxy.
// When contentType is empty or "application/json", behaves identically to
// the canonical call() helper (writes application/json on non-nil body).
//
// Per the P7.5 multipart upload path: chora-creation parses the file from
// the request body — the gateway is a thin passthrough.
func (a *Aggregator) callWithContentType(ctx context.Context, method, urlStr, contentType string, body []byte, auth AuthCtx) callResult {
	if urlStr == "" {
		return callResult{err: fmt.Errorf("gatewayproxy: empty url for %s", method)}
	}
	callCtx, cancel := context.WithTimeout(ctx, a.cfg.PerCallTimeout)
	defer cancel()

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(callCtx, method, urlStr, bodyReader)
	if err != nil {
		return callResult{err: fmt.Errorf("gatewayproxy: build request: %w", err)}
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		// Prefer caller-supplied Content-Type when provided (preserves the
		// multipart/form-data boundary). Fallback to application/json for
		// JSON bodies which is the canonical chora-creation contract.
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		} else {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	if auth.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+auth.Bearer)
	}
	if auth.Traceparent != "" {
		req.Header.Set("traceparent", auth.Traceparent)
	}
	if auth.TenantID != "" {
		req.Header.Set("X-Tenant-Id", auth.TenantID)
	}
	if auth.GCID != "" {
		req.Header.Set("X-Chora-GCID", auth.GCID)
		req.Header.Set("gcid", auth.GCID)
	}
	if auth.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", auth.IdempotencyKey)
	}
	// Bucket 4 (B6.1, 2026-05-16) — Roles propagates as `x-mesh-user-roles`.
	mesh := servicemesh.MarshalToHeaders(servicemesh.MeshClaims{
		GCID:        auth.GCID,
		TenantID:    auth.TenantID,
		Roles:       auth.Roles,
		RoleSummary: auth.RoleSummary,
	})
	for k, vs := range mesh {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return callResult{err: err}
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return callResult{status: resp.StatusCode, body: out, header: resp.Header}
}
