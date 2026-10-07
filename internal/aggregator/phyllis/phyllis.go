// Package phyllis aggregates the Phyllis MVP happy-path routes per
// docs/m13/phyllis-mvp-2026-05-08.md.
//
// This is the BFF aggregation layer: each public method fans out to one or
// more downstream services (chora-identity, chora-tenancy, chora-creation,
// chora-consumption, chora-delivery, chora-model-broker-router), propagates
// W3C traceparent + Authorization headers, honours a 5s per-call + 15s
// per-aggregation budget, and returns a normalised Response struct. Auth
// errors (401/403) and 404s pass through untouched; 5xx is normalised to
// 502 Bad Gateway; per-call timeouts surface as 504 Gateway Timeout.
//
// All service URLs come from environment variables (SVC_*_URL) per the
// project's no-inline-config rule (.claude/rules/development-execution.md /
// memory feedback_no_inline_config). Real wiring is set up by the BFF main()
// entrypoint via LoadConfigFromEnv().
//
// The aggregator is hexagonal: it has no transitive dependency on the chi or
// upstream packages — it is a pure domain orchestrator over an http.Client.
// Tests inject httptest.NewServer mocks for each downstream.
package phyllis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
)

// Default budgets per the briefing: 5s per-call, 15s per aggregation.
const (
	DefaultPerCallTimeout    = 5 * time.Second
	DefaultAggregationBudget = 15 * time.Second

	// CHO-1826 durable fix — admin mutation routes (tenant-member role
	// writes, member add, operator grant) drive a SYNCHRONOUS
	// identity->tenancy->pg chain that exceeds the 5s read per-call budget on
	// a cold start (Cloud SQL resume from cost-pause / idle gRPC subchannel
	// re-dial), surfacing a 504 GATEWAY_UPSTREAM_TIMEOUT facade even though
	// the write commits. These routes get a longer write budget, kept under
	// the 45s server WriteTimeout (cmd/server/main.go:591). Reads keep the
	// tight 5s/15s fast-fail so a hung downstream still surfaces quickly.
	DefaultWriteCallTimeout       = 30 * time.Second
	DefaultWriteAggregationBudget = 40 * time.Second
)

// Config wires the downstream service URLs and timing budgets. URLs are
// sourced from env vars to satisfy the no-inline-config rule.
type Config struct {
	IdentityURL    string
	TenancyURL     string
	CreationURL    string
	DeliveryURL    string
	ConsumptionURL string
	// PaymentsURL is the chora-payments HTTP base. Populated for the H+
	// Billing aggregators (ListAdminMyInvoices + CreateAdminMyBillingPortalSession);
	// other phyllis routes still talk to tenancy/identity/etc.
	PaymentsURL string

	// ModelBrokerGatewayURL is the new (Tier 2 D6) LLM-execution endpoint —
	// the BFF posts to {ModelBrokerGatewayURL}/llm/generate. Per the S3.1
	// audit fix, this REPLACES the legacy ModelBrokerRouterURL/generate
	// path; ModelBrokerRouterURL is retained as a fallback when this URL
	// is unset OR the Gateway returns 5xx.
	ModelBrokerGatewayURL string

	// ModelBrokerRouterURL is the legacy /generate path on
	// chora-model-broker-router. Kept for backwards compatibility while
	// the canned generator is being relocated to the Gateway. Once the
	// Router strips its generation package this URL will be removed.
	ModelBrokerRouterURL string

	PerCallTimeout    time.Duration
	AggregationBudget time.Duration

	// WriteCallTimeout / WriteAggregationBudget bound the admin MUTATION
	// routes (see admin_tenant_members.go). Larger than the read budget to
	// absorb cold-start latency in the identity->tenancy->pg write chain
	// (CHO-1826). Overridable via CHORA_PHYLLIS_WRITE_PERCALL_TIMEOUT_SECONDS /
	// CHORA_PHYLLIS_WRITE_BUDGET_SECONDS.
	WriteCallTimeout       time.Duration
	WriteAggregationBudget time.Duration
}

// ApplyDefaults fills any zero-valued timing budget with its default and keeps
// the write budget coherent (budget >= per-call).
func (c *Config) ApplyDefaults() {
	if c.PerCallTimeout == 0 {
		c.PerCallTimeout = DefaultPerCallTimeout
	}
	if c.AggregationBudget == 0 {
		c.AggregationBudget = DefaultAggregationBudget
	}
	if c.WriteCallTimeout == 0 {
		c.WriteCallTimeout = DefaultWriteCallTimeout
	}
	if c.WriteAggregationBudget == 0 {
		c.WriteAggregationBudget = DefaultWriteAggregationBudget
	}
	// A write aggregation budget shorter than its per-call timeout would cut
	// the single hop off early — clamp up.
	if c.WriteAggregationBudget < c.WriteCallTimeout {
		c.WriteAggregationBudget = c.WriteCallTimeout
	}
}

// LoadConfigFromEnv reads service URLs from SVC_*_URL env vars per
// memory feedback_no_inline_config. Returns a Config with defaults applied.
//
// Per S3.1 broker rework: SVC_MODEL_BROKER_GATEWAY_URL is the new primary
// LLM-execution endpoint; SVC_MODEL_BROKER_ROUTER_URL is the legacy
// fallback while the canned generator relocates from Router to Gateway.
//
// CHO-1799: PaymentsURL reads SVC_PAYMENTS_URL first, falling back to
// CHORA_PAYMENTS_HTTP_ADDR (the var the sibling gatewayproxy aggregator +
// the Stripe webhook passthrough have read since CHO-1759). Production
// deployment.yaml only sets the latter — without this fallback the H+
// Billing surface (/me/invoices + /me/billing-portal) silently 502'd
// because phyllis built "" + "/api/v1/admin/tenants/{tid}/billing-portal".
func LoadConfigFromEnv() Config {
	paymentsURL := os.Getenv("SVC_PAYMENTS_URL")
	if paymentsURL == "" {
		paymentsURL = os.Getenv("CHORA_PAYMENTS_HTTP_ADDR")
	}
	c := Config{
		IdentityURL:           os.Getenv("SVC_IDENTITY_URL"),
		TenancyURL:            os.Getenv("SVC_TENANCY_URL"),
		CreationURL:           os.Getenv("SVC_CREATION_URL"),
		DeliveryURL:           os.Getenv("SVC_DELIVERY_URL"),
		ConsumptionURL:        os.Getenv("SVC_CONSUMPTION_URL"),
		PaymentsURL:           paymentsURL,
		ModelBrokerGatewayURL: os.Getenv("SVC_MODEL_BROKER_GATEWAY_URL"),
		ModelBrokerRouterURL:  os.Getenv("SVC_MODEL_BROKER_ROUTER_URL"),
		// CHO-1826 — admin write-budget overrides (no-inline-config). Unset or
		// invalid leaves the field zero so ApplyDefaults fills the default.
		WriteCallTimeout:       envSeconds("CHORA_PHYLLIS_WRITE_PERCALL_TIMEOUT_SECONDS"),
		WriteAggregationBudget: envSeconds("CHORA_PHYLLIS_WRITE_BUDGET_SECONDS"),
	}
	c.ApplyDefaults()
	return c
}

// envSeconds reads an integer-seconds env var into a Duration. Returns 0 when
// unset, non-numeric, or non-positive so the caller's ApplyDefaults supplies
// the default (fail-soft on a malformed override rather than booting with a
// zero budget).
func envSeconds(name string) time.Duration {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// AuthCtx is the per-request authentication + tracing context. Bearer is the
// caller's Authorization header value (sans "Bearer " prefix); Traceparent is
// the W3C trace context to propagate; TenantID is resolved from session/JWT.
//
// GCID + RoleSummary are populated by the JWT validation middleware (per S3.6
// Stage B) so outbound calls can stamp the canonical mesh-metadata headers
// `chora-gcid`, `chora-tenant-id`, `chora-role-summary`. Empty values are
// omitted (defensive default — no spoofing risk if the BFF is unauthenticated).
type AuthCtx struct {
	Bearer      string
	Traceparent string
	TenantID    string
	GCID        string
	RoleSummary map[string]any
	// Roles is the typed role list resolved from the JWT (Bucket 4 — 2026-05-14
	// per servicemesh.MeshClaims.Roles). Populated by the JWT validation
	// middleware. Used by aggregators that gate field visibility on role
	// (e.g. GetAtom strips correct_option_id when the caller's roles lack
	// `author` / `instructor`).
	Roles []string
	// IdempotencyKey is the inbound Idempotency-Key header (RFC 7807-style
	// client-supplied dedup token), carried on AuthCtx so write-led aggregators
	// (e.g. social.ShareAtomToFeed) can forward it to downstreams that require
	// it — chora-sharing POST /v1/atoms/{id}/share 400s without it per §7.1
	// step 5 (ADR-196 D4). Empty for read paths; never synthesised.
	IdempotencyKey string
}

// HasAuthorRole reports whether the resolved auth context carries an
// authoring/instructor role on the active tenant. Used to gate the WS-0b
// A16 author-mode fields (e.g. correct_option_id on question_payload).
//
// The tenant-administrative roles (admin / owner / tenant_admin) are admitted
// alongside author / instructor. chora-creation gates its ENTIRE /questions
// authoring subtree on the same predicate, so an admin excluded here can mint a
// question through the (ungated) accept route and then 403 on reading it back —
// the atom editor renders empty. Keep this list and chora-creation's
// hasAuthorRole in lockstep: they are the two doors to one secret.
//
// Matching is case-insensitive so a canonical token ("AUTHOR", "ADMIN") and a
// lowercase mesh value both land.
func (a AuthCtx) HasAuthorRole() bool {
	for _, r := range a.Roles {
		switch strings.ToLower(strings.TrimSpace(r)) {
		case "author", "instructor", "admin", "owner", "tenant_admin":
			return true
		}
	}
	return false
}

// Response is the normalised aggregator return: either pass-through body +
// status, or a synthesised error envelope. Body is always JSON-serialisable.
type Response struct {
	Status  int
	Headers http.Header
	Body    []byte
}

// EventEmitter is a callback the BFF main() supplies for emitting Pub/Sub
// events after a successful aggregation. nil = events disabled (used in unit
// tests that don't care about event emission).
type EventEmitter func(ctx context.Context, topic string, payload []byte) error

// Aggregator is the public surface area for the Phyllis MVP routes. It is
// safe for concurrent use across HTTP request goroutines.
type Aggregator struct {
	cfg    Config
	client *http.Client
	emit   EventEmitter
}

// New constructs an Aggregator with defaults applied to cfg. emit may be nil.
func New(cfg Config, emit EventEmitter) *Aggregator {
	cfg.ApplyDefaults()
	// The shared http.Client.Timeout is a hard ceiling on EVERY request, so it
	// must accommodate the largest per-route budget — otherwise the admin
	// write routes' longer WriteAggregationBudget would be silently truncated
	// to the read AggregationBudget. Per-route context deadlines (set in
	// callWithTimeout / withBudgetTimeout) do the actual per-route enforcement.
	clientTimeout := cfg.AggregationBudget
	if cfg.WriteAggregationBudget > clientTimeout {
		clientTimeout = cfg.WriteAggregationBudget
	}
	return &Aggregator{
		cfg:    cfg,
		client: &http.Client{Timeout: clientTimeout},
		emit:   emit,
	}
}

// IsAuthStatus reports whether status is a pass-through auth error (401/403).
func IsAuthStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// isPassThrough returns true for statuses we forward verbatim (auth + not-found).
func isPassThrough(status int) bool {
	return IsAuthStatus(status) || status == http.StatusNotFound
}

// -----------------------------------------------------------------------------
// HTTP plumbing
// -----------------------------------------------------------------------------

// callResult is the internal shape returned by call().
type callResult struct {
	status int
	body   []byte
	header http.Header
	err    error
}

// Idempotent-GET retry tuning. Package vars so tests can tighten them.
//   - Reads to chora-consumption (daily-dose, companion/me, atoms, KG) can hit a
//     transient mesh-level 503 ("no healthy upstream" during a single-replica
//     rollout / cost-resume window) or a connection reset (the app's HTTP
//     IdleTimeout closing an Envoy-reused idle conn first). Both mean the
//     request NEVER reached the app, so one retry on a fresh connection is safe
//     and makes these windows invisible to the learner (fixes the daily-dose
//     502/503 flakiness, CHO debug 2026-06-29). Writes (callWrite) + any non-GET
//     via call() are NEVER retried.
var (
	phyllisMaxGetAttempts  = 2
	phyllisGetRetryBackoff = 50 * time.Millisecond
)

// call performs an outbound request honouring the read PerCallTimeout. Used by
// every read + non-mutating route. For idempotent GETs it retries ONCE on a
// transient, request-never-processed failure (mesh 503 / connection reset);
// non-GET methods are passed straight through (no retry — they must use
// callWrite or accept the single attempt).
func (a *Aggregator) call(ctx context.Context, method, urlStr string, body []byte, auth AuthCtx) callResult {
	cr := a.callWithTimeout(ctx, a.cfg.PerCallTimeout, method, urlStr, body, auth)
	if method != http.MethodGet {
		return cr
	}
	for attempt := 1; attempt < phyllisMaxGetAttempts && retriableCallResult(cr); attempt++ {
		select {
		case <-ctx.Done():
			return cr
		case <-time.After(phyllisGetRetryBackoff):
		}
		cr = a.callWithTimeout(ctx, a.cfg.PerCallTimeout, method, urlStr, body, auth)
	}
	return cr
}

// retriableCallResult reports whether a GET callResult is a transient,
// request-never-processed failure worth one more attempt: a mesh-level 503
// (zero-Ready rollout window / "no healthy upstream") or a non-deadline
// transport error (idle-connection reset / connect-failure). A real upstream
// timeout (deadline/cancel) is NOT retried — the budget is spent and the
// upstream is slow, not absent. A 5xx other than 503 is NOT retried — the app
// returns 500 for genuine errors (the mesh, not the app, emits 503 here).
func retriableCallResult(cr callResult) bool {
	if cr.err != nil {
		if errors.Is(cr.err, context.DeadlineExceeded) || errors.Is(cr.err, context.Canceled) {
			return false
		}
		var urlErr *url.Error
		if errors.As(cr.err, &urlErr) && urlErr.Timeout() {
			return false
		}
		return true
	}
	return cr.status == http.StatusServiceUnavailable
}

// callWrite performs one outbound request honouring the longer WriteCallTimeout.
// Used by the admin mutation routes whose identity->tenancy->pg chain can
// exceed the read budget on a cold start (CHO-1826).
func (a *Aggregator) callWrite(ctx context.Context, method, urlStr string, body []byte, auth AuthCtx) callResult {
	return a.callWithTimeout(ctx, a.cfg.WriteCallTimeout, method, urlStr, body, auth)
}

// callWithTimeout performs one outbound request with traceparent + Authorization
// headers stamped, bounding the hop with the supplied timeout. Returns a
// callResult; err is non-nil only on transport failures (timeouts surface via
// err == context.DeadlineExceeded).
func (a *Aggregator) callWithTimeout(ctx context.Context, timeout time.Duration, method, urlStr string, body []byte, auth AuthCtx) callResult {
	if urlStr == "" {
		return callResult{err: fmt.Errorf("phyllis: empty url for %s", method)}
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(callCtx, method, urlStr, bodyReader)
	if err != nil {
		return callResult{err: fmt.Errorf("phyllis: build request: %w", err)}
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
		// D1.5 fix — chora-consumption's requireContext / extRequireContext
		// read the LOWERCASE `gcid` header (returns "gcid required" without
		// it — the Phyllis step 8 daily-dose blocker). Stamp it explicitly
		// for every chora-consumption-bound route (daily-dose, companion/me,
		// atoms/{id}/feedback, knowledge-graph/clusters). Mirrors the
		// social + notifications aggregators' established pattern. Purely
		// additive — carries the same value as the canonical chora-gcid
		// mesh header under the header name the downstream actually reads.
		req.Header.Set("gcid", auth.GCID)
	}
	// Stamp Cloud Service Mesh-bound metadata so backend services can decode
	// the trusted caller identity without re-validating the JWT (per S3.6
	// Stage B + libs/chora-go-common/auth/servicemesh contract).
	mesh := servicemesh.MarshalToHeaders(servicemesh.MeshClaims{
		GCID:        auth.GCID,
		TenantID:    auth.TenantID,
		RoleSummary: auth.RoleSummary,
		// L1 (CHO-1708): Bucket 4 typed roles were resolved into AuthCtx
		// but never marshalled — downstream role gates that read
		// x-mesh-user-roles (identity callerHoldsAdminRole, delivery RBAC)
		// saw an empty header on every phyllis-proxied call. Stamp them.
		Roles: auth.Roles,
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

// classify converts a callResult into a Response, normalising errors:
//   - context deadline / Timeout / Cancellation -> 504 Gateway Timeout
//   - other transport errors -> 502 Bad Gateway
//   - 5xx upstream -> 502 Bad Gateway (cascade-safe)
//   - 401/403/404 -> pass-through
//   - 2xx -> pass-through
func classify(cr callResult) Response {
	if cr.err != nil {
		if errors.Is(cr.err, context.DeadlineExceeded) {
			return errResp(http.StatusGatewayTimeout, "GATEWAY_UPSTREAM_TIMEOUT", cr.err.Error())
		}
		if errors.Is(cr.err, context.Canceled) {
			return errResp(http.StatusGatewayTimeout, "GATEWAY_REQUEST_CANCELED", cr.err.Error())
		}
		// urlError unwrap may carry deadline / canceled
		var urlErr *url.Error
		if errors.As(cr.err, &urlErr) && urlErr.Timeout() {
			return errResp(http.StatusGatewayTimeout, "GATEWAY_UPSTREAM_TIMEOUT", cr.err.Error())
		}
		return errResp(http.StatusBadGateway, "GATEWAY_UPSTREAM_ERROR", cr.err.Error())
	}
	if cr.status >= 500 {
		return errResp(http.StatusBadGateway, "GATEWAY_UPSTREAM_5XX",
			fmt.Sprintf("upstream returned %d", cr.status))
	}
	if isPassThrough(cr.status) {
		return Response{Status: cr.status, Body: cr.body, Headers: cr.header}
	}
	return Response{Status: cr.status, Body: cr.body, Headers: cr.header}
}

// errResp builds a JSON error envelope.
func errResp(status int, code, message string) Response {
	body := fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, code, message)
	return Response{Status: status, Body: []byte(body)}
}

// withBudget wraps the aggregation in the read AggregationBudget. Used by every
// read + non-mutating route.
func (a *Aggregator) withBudget(parent context.Context, fn func(context.Context) Response) Response {
	return a.withBudgetTimeout(parent, a.cfg.AggregationBudget, fn)
}

// withWriteBudget wraps the aggregation in the longer WriteAggregationBudget for
// admin mutation routes (CHO-1826).
func (a *Aggregator) withWriteBudget(parent context.Context, fn func(context.Context) Response) Response {
	return a.withBudgetTimeout(parent, a.cfg.WriteAggregationBudget, fn)
}

// withBudgetTimeout wraps the aggregation in the supplied budget. If exceeded,
// returns a 504. The first non-2xx pass-through status from any sub-call
// short-circuits without consuming the full budget.
func (a *Aggregator) withBudgetTimeout(parent context.Context, budget time.Duration, fn func(context.Context) Response) Response {
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()
	done := make(chan Response, 1)
	go func() {
		done <- fn(ctx)
	}()
	select {
	case r := <-done:
		return r
	case <-ctx.Done():
		return errResp(http.StatusGatewayTimeout, "GATEWAY_AGGREGATION_BUDGET_EXCEEDED", ctx.Err().Error())
	}
}

// -----------------------------------------------------------------------------
// Phyllis MVP routes
// -----------------------------------------------------------------------------

// GetMe — GET /api/me — fans out to chora-identity:/me.
func (a *Aggregator) GetMe(ctx context.Context, auth AuthCtx) (Response, error) {
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodGet, a.cfg.IdentityURL+"/me", nil, auth))
	}), nil
}

// GetMyRoles — GET /api/me/roles?course_id={id} — chora-identity:/me/roles.
func (a *Aggregator) GetMyRoles(ctx context.Context, auth AuthCtx, courseID string) (Response, error) {
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.IdentityURL + "/me/roles"
		if courseID != "" {
			u += "?course_id=" + url.QueryEscape(courseID)
		}
		return classify(a.call(c, http.MethodGet, u, nil, auth))
	}), nil
}

// GetMyTenant — GET /api/tenants/me — resolves tenant_id from auth context
// and fans out to chora-tenancy:/api/tenants/{id}.
//
// A6 follow-up (CHO-1545): the downstream path is /api/tenants/{id}, NOT
// /tenants/{id}. chora-tenancy's HTTP adapter mounts the tenant detail
// handler under /api/tenants/ — the legacy /tenants/{id} forward 404'd.
// Same bug class A6 already fixed for GetAtom; mirrors the verified
// gatewayproxy.GetTenant path.
func (a *Aggregator) GetMyTenant(ctx context.Context, auth AuthCtx) (Response, error) {
	if auth.TenantID == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_TENANT_NOT_RESOLVED",
			"tenant_id missing from auth context"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.TenancyURL + "/api/tenants/" + url.PathEscape(auth.TenantID)
		return classify(a.call(c, http.MethodGet, u, nil, auth))
	}), nil
}

// GetMyTenantV1 — GET /api/v1/tenants/me — proxies to chora-tenancy's
// pg-backed MeTenantHandler at the SAME path. chora-tenancy resolves the
// "me" tenant from the X-Tenant-Id header `a.call()` stamps for us, so
// no URL rewrite is needed. Response carries `branding` +
// `wizard_completed_at` — the canonical shape the wizard's CHO-1692
// re-entry hydration consumes.
func (a *Aggregator) GetMyTenantV1(ctx context.Context, auth AuthCtx) (Response, error) {
	if auth.TenantID == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_TENANT_NOT_RESOLVED",
			"tenant_id missing from auth context"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.TenancyURL + "/api/v1/tenants/me"
		return classify(a.call(c, http.MethodGet, u, nil, auth))
	}), nil
}

// ListMyIdpProviders — GET /api/v1/tenants/me/idp-providers — proxies
// to chora-identity's MeIdpProvidersHandler (CHO-1692 GET). chora-identity
// resolves the calling tenant from the X-Tenant-Id header stamped by
// a.call(). Response is `{"items":[...]}` (empty for fresh tenants —
// 200, NOT 404). Distinct from the POST aggregator at /api/v1/tenants/setup
// which fans out IdP upsert + finish-setup.
func (a *Aggregator) ListMyIdpProviders(ctx context.Context, auth AuthCtx) (Response, error) {
	if auth.TenantID == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_TENANT_NOT_RESOLVED",
			"tenant_id missing from auth context"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.IdentityURL + "/api/v1/tenants/me/idp-providers"
		return classify(a.call(c, http.MethodGet, u, nil, auth))
	}), nil
}

// DeleteMyIdpProvider — DELETE /api/v1/tenants/me/idp-providers/{providerType}
// (CHO-1694). Proxies the soft-delete to chora-identity. Forwards the
// caller-supplied providerType verbatim in the path; chora-identity
// validates it server-side (unknown values → 400). Distinct from the
// admin-side delete which lives in tenancy-admin under a different
// route.
func (a *Aggregator) DeleteMyIdpProvider(ctx context.Context, auth AuthCtx, providerType string) (Response, error) {
	if auth.TenantID == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_TENANT_NOT_RESOLVED",
			"tenant_id missing from auth context"), nil
	}
	if providerType == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_INVALID_PROVIDER_TYPE",
			"provider_type segment required"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.IdentityURL + "/api/v1/tenants/me/idp-providers/" + url.PathEscape(providerType)
		return classify(a.call(c, http.MethodDelete, u, nil, auth))
	}), nil
}

// CreateCourse — POST /api/courses — composite: chora-creation:/api/atoms
// (root atom) + chora-delivery:/courses (course skeleton). Sequential: if
// creation fails the delivery call is skipped and a 502 returned.
//
// chora-creation mounts the atom collection at /api/atoms (its legacy
// AtomHandler mux — internal/adapter/http/handler.go:479), NOT /atoms.
// Mirrors the GetAtom / DeleteAtom / PatchAtom / AIAssist fan-outs in this
// file, which all target /api/atoms.
func (a *Aggregator) CreateCourse(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	return a.withBudget(ctx, func(c context.Context) Response {
		atomCR := a.call(c, http.MethodPost, a.cfg.CreationURL+"/api/atoms", body, auth)
		atomResp := classify(atomCR)
		if atomResp.Status >= 400 {
			return atomResp
		}
		courseCR := a.call(c, http.MethodPost, a.cfg.DeliveryURL+"/courses", body, auth)
		courseResp := classify(courseCR)
		if courseResp.Status >= 400 {
			return courseResp
		}
		// Merge: { course_id, root_atom_id, ...delivery_body }
		merged := mergeJSON(courseResp.Body, map[string]any{
			"root_atom_id": stringField(atomCR.body, "atom_id"),
		})
		return Response{
			Status: http.StatusCreated,
			Body:   merged,
		}
	}), nil
}

// GetCatalog — GET /api/catalog[?public=true] — Phyllis Step 5 (PublicDiscovery).
//
// Targets chora-delivery's consolidated /v1/courses route with a `visibility`
// query param. The legacy /courses?public=true route is NOT used: it is
// tenantRequired-gated and 400s an unauthed request, which broke the demo's
// UNAUTHED public-discovery curl (`GET api.chora.site/api/catalog`).
//
// Visibility selection:
//   - public == true            → ?visibility=public
//   - no tenant context (anon)  → ?visibility=public
//   - authed (tenant present)   → ?visibility=tenant_or_public
//
// An anonymous caller is ALWAYS pinned to visibility=public — chora-delivery's
// tenant_or_public filter returns every tenant_only row when the tenant scope
// is empty, so sending tenant_or_public without a tenant would leak other
// tenants' non-public courses. visibility=public is strictly public-visibility
// rows and is safe to serve cross-tenant + unauthenticated.
func (a *Aggregator) GetCatalog(ctx context.Context, auth AuthCtx, public bool, params url.Values) (Response, error) {
	return a.withBudget(ctx, func(c context.Context) Response {
		visibility := "public"
		if !public && auth.TenantID != "" {
			visibility = "tenant_or_public"
		}
		dq := url.Values{}
		dq.Set("visibility", visibility)
		// CR2-C2: forward the learner-facing search (`q`) + cursor pagination
		// (`first`, `after`) + paid filter to chora-delivery /v1/courses, which
		// already honours them. `visibility` stays server-derived (never
		// client-supplied) to avoid the cross-tenant leak documented above.
		for _, k := range []string{"q", "first", "after", "paid"} {
			if v := params.Get(k); v != "" {
				dq.Set(k, v)
			}
		}
		u := a.cfg.DeliveryURL + "/v1/courses?" + dq.Encode()
		return classify(a.call(c, http.MethodGet, u, nil, auth))
	}), nil
}

// CreateEnrollment — POST /api/enrollments — chora-delivery:/enrollments.
// On success, emits chora.delivery.enrollment.created.v1 via the configured
// EventEmitter (best-effort; emitter errors are logged, not surfaced).
func (a *Aggregator) CreateEnrollment(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	resp := a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodPost, a.cfg.DeliveryURL+"/enrollments", body, auth))
	})
	if resp.Status >= 200 && resp.Status < 300 && a.emit != nil {
		_ = a.emit(ctx, "chora.delivery.enrollment.created.v1", resp.Body)
	}
	return resp, nil
}

// GetAtom — GET /api/atoms/{id} — proxies chora-creation:/api/atoms/{id}
// (the atom's authored content) through the WS-0b A16 question_payload gate.
//
// chora-creation serves the atom under /api/atoms/{id} (its legacy
// AtomHandler mux — see services/chora-creation/internal/adapter/http/
// handler.go), NOT /atoms/{id}. The gateway-facing path is /api/atoms/{id};
// the BFF forwards to the SAME path. This also covers GET /api/atoms/new —
// chora-creation's /api/atoms/{id} handler treats the literal "new" segment
// as the empty-AtomDraft template request.
//
// If the atom call returns 404, surface 404 directly. If it fails (5xx /
// transport), surface 502.
//
// WS-0b A16 — `mode` controls question_payload visibility:
//   - mode == "author" AND auth.HasAuthorRole() → preserve correct_option_id
//     if upstream emits it (currently chora-creation does not — defense-in-depth).
//   - Otherwise → strip question_payload.correct_option_id from the atom JSON.
//
// Fail loud per `feedback_no_stubs_real_wiring`: if chora-creation returns a
// question_payload with type=mcq but empty/missing options[], surface 502
// rather than the silent empty payload that would render nothing in the FE.
//
// The atom-session lifecycle is NOT loaded here: the player starts a session
// via POST /v1/me/atom-sessions (chora-consumption). A prior implementation
// side-loaded GET /atoms/{id}/session, but chora-consumption has no such
// endpoint — it always 404'd and fabricated a spurious "session_error:
// upstream_unavailable" degraded banner. That dead fan-out was removed
// (CHO-1968).
func (a *Aggregator) GetAtom(ctx context.Context, auth AuthCtx, atomID string, mode string) (Response, error) {
	return a.withBudget(ctx, func(c context.Context) Response {
		// Single synchronous proxy to chora-creation. Propagate mode downstream
		// as a query param so future chora-creation evolutions can vary the
		// projection per mode (chora-creation currently ignores it).
		upstreamURL := a.cfg.CreationURL + "/api/atoms/" + url.PathEscape(atomID)
		if mode == "author" {
			upstreamURL += "?mode=author"
		}
		atomCR := a.call(c, http.MethodGet, upstreamURL, nil, auth)

		atomResp := classify(atomCR)
		if atomResp.Status == http.StatusNotFound || atomResp.Status >= 500 || atomCR.err != nil {
			return atomResp
		}
		if atomResp.Status >= 400 {
			return atomResp
		}

		// Apply WS-0b A16 question_payload BFF gates:
		//   1. Validate non-empty options[] when question_payload.type == "mcq"
		//      (fail loud — 502 GATEWAY_UPSTREAM_5XX on malformed upstream).
		//   2. Strip correct_option_id when caller is not in author mode
		//      OR lacks the author/instructor role (defense in depth).
		authorMode := mode == "author" && auth.HasAuthorRole()
		atomJSON, perr := processQuestionPayload(atomCR.body, authorMode)
		if perr != nil {
			return errResp(http.StatusBadGateway, "GATEWAY_UPSTREAM_5XX", perr.Error())
		}

		return Response{
			Status: http.StatusOK,
			Body:   marshalJSON(map[string]any{"atom": atomJSON}),
		}
	}), nil
}

// processQuestionPayload applies BFF-side gates to the upstream atom JSON
// per WS-0b A16:
//
//  1. Fail loud — a question_payload with type=mcq but no options[] (empty
//     or missing) is a contract violation; surface 502 so the FE renders an
//     explicit error rather than a blank question.
//  2. Strip correct_option_id when authorMode is false (defense in depth —
//     chora-creation currently never emits it, but the strip prevents any
//     future upstream regression from leaking it).
//
// Returns the (possibly modified) parsed JSON value ready to embed under
// the envelope's "atom" key (the outer marshalJSON will re-serialise it).
// On contract violation returns a non-nil error so the caller can map to
// 502 GATEWAY_UPSTREAM_5XX.
func processQuestionPayload(atomBytes []byte, authorMode bool) (any, error) {
	if len(atomBytes) == 0 {
		return nil, nil
	}
	var atomMap map[string]any
	if err := json.Unmarshal(atomBytes, &atomMap); err != nil {
		// Not a JSON object — pass through as raw (preserves existing
		// behaviour for unusual upstream shapes; the FE will surface the
		// type mismatch).
		return rawJSONOrNull(atomBytes), nil
	}
	qpRaw, present := atomMap["question_payload"]
	if !present || qpRaw == nil {
		// No question_payload — nothing to gate. Return the parsed map so
		// the envelope marshals cleanly.
		return atomMap, nil
	}
	qp, ok := qpRaw.(map[string]any)
	if !ok {
		// Malformed (not an object) — refuse.
		return nil, fmt.Errorf("question_payload is not a JSON object")
	}
	qType, _ := qp["type"].(string)
	if qType == "mcq" {
		opts, _ := qp["options"].([]any)
		if len(opts) == 0 {
			return nil, fmt.Errorf("question_payload type=mcq but options[] is empty or missing")
		}
	}
	if !authorMode {
		// Defense-in-depth strip. chora-creation currently does NOT emit
		// correct_option_id but if a future upstream regression leaks it,
		// the BFF guarantees learners never see it.
		delete(qp, "correct_option_id")
		atomMap["question_payload"] = qp
	}
	return atomMap, nil
}

// SubmitFeedback — POST /api/atoms/{id}/feedback — chora-consumption.
func (a *Aggregator) SubmitFeedback(ctx context.Context, auth AuthCtx, atomID string, body []byte) (Response, error) {
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.ConsumptionURL + "/atoms/" + url.PathEscape(atomID) + "/feedback"
		return classify(a.call(c, http.MethodPost, u, body, auth))
	}), nil
}

// GenerateAI — POST /api/ai/generate — chora-model-broker-gateway:/llm/generate.
//
// Per Tier 2 D6 + S3.1 audit fix the Gateway is the SOLE LLM-execution arm
// of the 3-service Model Broker. The aggregator preference order:
//
//  1. ModelBrokerGatewayURL set → POST {gateway}/llm/generate (primary).
//  2. Gateway returns 5xx + ModelBrokerRouterURL set → fall back to
//     {router}/generate (legacy, while the Router still ships a canned
//     generator behind a feature flag).
//  3. Only ModelBrokerRouterURL set → POST {router}/generate directly
//     (legacy path; will be removed once the Router strips generation).
//  4. Neither set → 502 GATEWAY_UPSTREAM_5XX.
//
// Guardrail screening orchestration is performed upstream of this BFF call
// by the LangGraph orchestrator (A-AIK-Orchestrator wave) — the Gateway
// itself does NOT call Guardrail. For S3.1, the Gateway returns the raw
// Completion and the orchestration layer screens it separately.
func (a *Aggregator) GenerateAI(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	gatewayURL := a.cfg.ModelBrokerGatewayURL
	routerURL := a.cfg.ModelBrokerRouterURL
	return a.withBudget(ctx, func(c context.Context) Response {
		// Path 1+2: Gateway preferred.
		if gatewayURL != "" {
			res := classify(a.call(c, http.MethodPost, gatewayURL+"/llm/generate", body, auth))
			// On success or pass-through (4xx, 401/403/404), return.
			if res.Status < 500 {
				return res
			}
			// 5xx — try legacy fallback if configured.
			if routerURL != "" {
				return classify(a.call(c, http.MethodPost, routerURL+"/generate", body, auth))
			}
			return res
		}
		// Path 3: legacy Router only.
		if routerURL != "" {
			return classify(a.call(c, http.MethodPost, routerURL+"/generate", body, auth))
		}
		// Path 4: misconfigured.
		return errResp(http.StatusBadGateway, "GATEWAY_UPSTREAM_5XX",
			"neither SVC_MODEL_BROKER_GATEWAY_URL nor SVC_MODEL_BROKER_ROUTER_URL configured")
	}), nil
}

// GetKGClusters — GET /api/v1/me/knowledge-graph/clusters
// → chora-consumption:/v1/me/knowledge-graph/clusters.
//
// Phyllis A+ KG-fog dashboard panel per
// docs/m13/kg-fog-aplus-integration-backend-handoff-2026-05-13.md §1.1.
// Returns the FE-compatible {data: {clusters: [], capRemaining, capMax}}
// envelope; FE renders empty-state seed input when clusters: [].
func (a *Aggregator) GetKGClusters(ctx context.Context, auth AuthCtx) (Response, error) {
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodGet,
			a.cfg.ConsumptionURL+"/v1/me/knowledge-graph/clusters", nil, auth))
	}), nil
}

// CreateKGCluster — POST /api/v1/me/knowledge-graph/clusters
// → chora-consumption:/v1/me/knowledge-graph/clusters.
//
// Per §1.2: creates a new cluster + first hex (orchestrator P50 < 2.5s SLO).
// 402 on insufficient mana, 409 on cap reached, 503 on engine rate-limit.
func (a *Aggregator) CreateKGCluster(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodPost,
			a.cfg.ConsumptionURL+"/v1/me/knowledge-graph/clusters", body, auth))
	}), nil
}

// AIAssist — POST /api/atoms/ai-assist — chora-creation:/api/atoms/ai-assist.
//
// Phyllis demo Step 4 dramatic moment: chora-creation dispatches a QGen
// 6-step pipeline run against the Vertex AI Agent Engine
// (us-central1 per ADR-148) and streams the BatchReport back. Currently
// the chora-creation handler 503s qgen_engine_not_configured until 5.D
// wires the engine client. The BFF fans out unconditionally — when the
// downstream 503s, that surfaces through unchanged.
//
// Distinct from GenerateAI (which targets the Model Broker Gateway for
// generic single-LLM-call generation). AI Assist invokes the canonical
// 6-step pipeline; GenerateAI invokes a single LLM round.
func (a *Aggregator) AIAssist(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodPost,
			a.cfg.CreationURL+"/api/atoms/ai-assist", body, auth))
	}), nil
}

// GetCompanionMe — GET /api/companion/me — chora-consumption:/companion/me.
func (a *Aggregator) GetCompanionMe(ctx context.Context, auth AuthCtx) (Response, error) {
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodGet, a.cfg.ConsumptionURL+"/companion/me", nil, auth))
	}), nil
}

// GetDailyDose — GET /api/companion/daily-dose — chora-consumption:/companion/daily-dose.
//
// Phase 2A — the optional growthEdgeID (from ?growth_edge_id={uuid}) scopes the
// dose to drilling ONE Growth Edge (focused practice session).
//
// WS-3: the optional goalID (from ?goal_id={uuid}) scopes the dose to the goal
// the learner is browsing, so that goal's material leads the served cards.
//
// Each is forwarded ONLY when non-empty, so the unscoped call stays
// byte-identical to the prior behaviour. NOTE this function REBUILDS the
// upstream URL rather than proxying the inbound query string: a scope param that
// is not named here is silently dropped and never reaches chora-consumption, so
// any future dose scope must be added in this list as well as downstream.
func (a *Aggregator) GetDailyDose(ctx context.Context, auth AuthCtx, growthEdgeID, goalID string) (Response, error) {
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.ConsumptionURL + "/companion/daily-dose"
		q := url.Values{}
		if growthEdgeID != "" {
			q.Set("growth_edge_id", growthEdgeID)
		}
		if goalID != "" {
			q.Set("goal_id", goalID)
		}
		if len(q) > 0 {
			u += "?" + q.Encode()
		}
		return classify(a.call(c, http.MethodGet, u, nil, auth))
	}), nil
}

// DeleteAtom — DELETE /api/atoms/{id} — verbatim passthrough to
// chora-creation. Pairs with GetAtom (which is a parallel fan-out for the
// learner-side read) but skips the consumption fan-out because deletion is
// authoring-side only.
//
// Per a508f184 + ddd-enforcement #8 the chora-creation handler also
// cascade-soft-deletes the attached Question (see commit 8e57a70b).
// Cloud Armor priority 996 (Infra commit 1052db87) now allows DELETE on
// this path family at the edge.
func (a *Aggregator) DeleteAtom(ctx context.Context, auth AuthCtx, atomID string) (Response, error) {
	if atomID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_ATOM_ID_REQUIRED",
			"atom id required in path: /api/atoms/{atom_id}"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodDelete, a.cfg.CreationURL+"/api/atoms/"+url.PathEscape(atomID), nil, auth))
	}), nil
}

// PatchAtom — PATCH /api/atoms/{id} — verbatim passthrough to
// chora-creation. Pairs with the atom-authoring PATCH that bumps the
// AtomRevision (append-only per ddd-enforcement #4).
//
// Cloud Armor priority 996 (Infra commit 1052db87) now allows PATCH on
// this path family at the edge.
func (a *Aggregator) PatchAtom(ctx context.Context, auth AuthCtx, atomID string, body []byte) (Response, error) {
	if atomID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_ATOM_ID_REQUIRED",
			"atom id required in path: /api/atoms/{atom_id}"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodPatch, a.cfg.CreationURL+"/api/atoms/"+url.PathEscape(atomID), body, auth))
	}), nil
}

// MintAtomMediaSignedUrl — POST /api/atoms/{id}/media — verbatim
// passthrough to chora-creation. ADR-156 Phase 1 atom-media support
// (E2E-BE-ATOM-GW / E2E-INFRA-ATOM-3). chora-creation handler mints a V4
// signed URL pointing at the `chora-atom-media-dev` bucket (provisioned
// by Infra at 4df66f51). FE PUTs the image directly to GCS using the
// signed URL — the BFF never proxies the 2MB image, only the small JSON
// signed-URL response.
//
// Cloud Armor priority 996 (Infra commit 1052db87) covers the
// /api/atoms/{id}/media path via the prefix-match regex from the
// atom-CRUD allow.
//
// 502 GATEWAY_UPSTREAM_5XX is acceptable until E2E-BE-ATOM-1 ships the
// chora-creation handler.
func (a *Aggregator) MintAtomMediaSignedUrl(ctx context.Context, auth AuthCtx, atomID string, body []byte) (Response, error) {
	if atomID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_ATOM_ID_REQUIRED",
			"atom id required in path: /api/atoms/{atom_id}/media"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodPost, a.cfg.CreationURL+"/api/atoms/"+url.PathEscape(atomID)+"/media", body, auth))
	}), nil
}

// GetAIAssistJob — GET /api/atoms/ai-assist/{job_id} — verbatim
// passthrough to chora-creation. Status poll for the qgen 2-agent
// crew async surface per docs/m13/ack-oe-ai-assist-plan-2026-05-17.md
// Step 4c. Returns AiAssistJob envelope per OpenAPI
// chora-contracts/openapi/creation-questions.yaml §AiAssistJob.
//
// FE poll cadence: ~2s until status ∈ {COMPLETED, REFUSED, FAILED}.
// Tenant-scoped at the chora-creation side (RLS) — cross-tenant
// returns 404.
func (a *Aggregator) GetAIAssistJob(ctx context.Context, auth AuthCtx, jobID string) (Response, error) {
	if jobID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_JOB_ID_REQUIRED",
			"job_id required in path: /api/atoms/ai-assist/{job_id}"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodGet,
			a.cfg.CreationURL+"/api/atoms/ai-assist/"+url.PathEscape(jobID), nil, auth))
	}), nil
}
