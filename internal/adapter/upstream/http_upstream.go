// Package upstream — HTTPUpstream Phase-6 adapter.
//
// Replaces FakeUpstream's deterministic placeholders with real HTTP fan-out
// to the 8 downstream domain services per the Phase-6 contract:
//
//	GetLearningPath → chora-consumption  GET /api/learning-paths
//	GetRecentAtoms  → chora-creation     GET /api/atoms?status=published
//	GetCompanion     → chora-consumption  GET /companion/me
//	GetStreak       → chora-consumption  GET /v1/me/streak
//	GetFeed         → chora-sharing      GET /v1/feed/shared-atoms
//	GetTenant       → chora-tenancy      GET /api/tenants/{id}
//	GetGovernance   → chora-governance   GET /governance/{tenant_id}
//	GetAuditEvents  → chora-observability GET /events?tenant_id=X
//	GetCourses      → chora-delivery     GET /courses
//
// Mirrors the phyllis.Aggregator call()/classify() pattern:
//   - W3C traceparent propagation
//   - servicemesh.MarshalToHeaders for mTLS-aware mesh metadata
//   - Per-call 5s timeout
//   - 5xx normalised to ErrUpstream wrap
//   - 404 returns (nil, nil) so AggregatedView callers can render empty-state
//
// AuthCtx is carried via context.Context — callers (BFF handlers / GraphQL
// resolvers) use WithAuthCtx() before invoking; the adapter falls back to
// zero AuthCtx when none present.
//
// Tracing: every method emits an OpenTelemetry span named
// "upstream.{Method}" so HTTPUpstream calls show up alongside phyllis.go
// spans in Cloud Trace (per Tier 3 D13 / ai-observability-cloud-trace).
//
// Gating: cmd/server/main.go selects FakeUpstream (default) or
// HTTPUpstream based on BFF_HTTPUPSTREAM_ENABLED=true. This preserves the
// zero-regression rollback path for current deploys.
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
)

// DefaultPerCallTimeout matches phyllis.DefaultPerCallTimeout.
const DefaultPerCallTimeout = 5 * time.Second

// HTTPConfig wires the 7 downstream service base URLs. URLs are sourced from
// SVC_*_URL env vars per memory feedback_no_inline_config.
//
// Note: only 7 base URLs are needed (Consumption serves BOTH LearningPath +
// Companion) — that mapping mirrors the domain ownership in
// docs/architecture-review-inputs-2026-05-07.md Tier 1.
type HTTPConfig struct {
	ConsumptionURL   string // chora-consumption: GetLearningPath, GetCompanion
	CreationURL      string // chora-creation:    GetRecentAtoms
	SharingURL       string // chora-sharing:     GetFeed
	TenancyURL       string // chora-tenancy:     GetTenant
	GovernanceURL    string // chora-governance:  GetGovernance
	ObservabilityURL string // chora-observability: GetAuditEvents
	DeliveryURL      string // chora-delivery:    GetCourses

	PerCallTimeout time.Duration
}

// LoadHTTPConfigFromEnv reads the 7 SVC_*_URL env vars per
// feedback_no_inline_config and applies a 5s per-call default.
func LoadHTTPConfigFromEnv() HTTPConfig {
	c := HTTPConfig{
		ConsumptionURL:   os.Getenv("SVC_CONSUMPTION_URL"),
		CreationURL:      os.Getenv("SVC_CREATION_URL"),
		SharingURL:       os.Getenv("SVC_SHARING_URL"),
		TenancyURL:       os.Getenv("SVC_TENANCY_URL"),
		GovernanceURL:    os.Getenv("SVC_GOVERNANCE_URL"),
		ObservabilityURL: os.Getenv("SVC_OBSERVABILITY_URL"),
		DeliveryURL:      os.Getenv("SVC_DELIVERY_URL"),
	}
	if c.PerCallTimeout == 0 {
		c.PerCallTimeout = DefaultPerCallTimeout
	}
	return c
}

// AuthCtx is the per-request authentication + tracing context HTTPUpstream
// stamps on outbound calls. Mirrors phyllis.AuthCtx but lives here to keep
// the adapter package independent of the aggregator.
type AuthCtx struct {
	Bearer      string
	Traceparent string
	TenantID    string
	GCID        string
	RoleSummary map[string]any
	// Roles is the typed role list from the VALIDATED session / mesh claims —
	// never a client-supplied header. It becomes the `x-mesh-user-roles` mesh
	// header (servicemesh.HeaderUserRoles).
	//
	// It is NOT optional. Every downstream role gate in Chora reads that header
	// and FAILS CLOSED (the fail-OPEN `rolesAllowed(X-Role)` was deleted in
	// CHO-2072), so an AuthCtx without Roles does not degrade gracefully — the
	// downstream denies 100% of the calls. Until 2026-07-14 this field did not
	// exist at all, which made HTTPUpstream and ObservabilityClient unable to
	// reach ANY role-gated endpoint; the O+ egress kill-switch had to hand-roll
	// its own proxy purely to work around it (CHO-2148). Guarded by
	// mesh_roles_guard_test.go.
	Roles []string
}

type authCtxKey struct{}

// WithAuthCtx attaches AuthCtx so HTTPUpstream call methods can stamp the
// outbound HTTP headers. Callers that do not attach AuthCtx receive an
// unauthenticated outbound call (mesh metadata absent).
func WithAuthCtx(ctx context.Context, a AuthCtx) context.Context {
	return context.WithValue(ctx, authCtxKey{}, a)
}

// AuthCtxFromContext extracts AuthCtx if present.
func AuthCtxFromContext(ctx context.Context) AuthCtx {
	if v, ok := ctx.Value(authCtxKey{}).(AuthCtx); ok {
		return v
	}
	return AuthCtx{}
}

// HTTPUpstream implements the Client interface against real HTTP downstream
// services. Construct via NewHTTPUpstream. Safe for concurrent use.
type HTTPUpstream struct {
	cfg    HTTPConfig
	client *http.Client

	// FailMethods preserves parity with FakeUpstream — primarily used by
	// chaos tests + the existing graceful-degradation contract.
	FailMethods map[string]bool

	tracer trace.Tracer
}

// NewHTTPUpstream constructs an HTTPUpstream with sensible defaults applied.
func NewHTTPUpstream(cfg HTTPConfig) *HTTPUpstream {
	if cfg.PerCallTimeout == 0 {
		cfg.PerCallTimeout = DefaultPerCallTimeout
	}
	return &HTTPUpstream{
		cfg:         cfg,
		client:      &http.Client{Timeout: cfg.PerCallTimeout},
		FailMethods: map[string]bool{},
		tracer:      otel.Tracer("chora-gateway/upstream/http"),
	}
}

// shouldFail returns true when the method is in FailMethods.
func (h *HTTPUpstream) shouldFail(method string) bool {
	if h == nil || h.FailMethods == nil {
		return false
	}
	return h.FailMethods[method]
}

// -----------------------------------------------------------------------------
// HTTP plumbing (mirrors phyllis.go call/classify)
// -----------------------------------------------------------------------------

type callResult struct {
	status int
	body   []byte
	header http.Header
	err    error
}

// call performs one outbound request with traceparent + Authorization headers
// stamped, honouring PerCallTimeout. Emits an OpenTelemetry span. Mirrors
// phyllis.Aggregator.call() exactly to preserve the Cloud Trace shape.
func (h *HTTPUpstream) call(ctx context.Context, spanName, method, urlStr string, auth AuthCtx) callResult {
	ctx, span := h.tracer.Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("http.request.method", method),
			attribute.String("server.address", urlHost(urlStr)),
			attribute.String("chora.tenant_id", auth.TenantID),
		),
	)
	defer span.End()

	if urlStr == "" {
		err := fmt.Errorf("%w: empty url for %s", ErrUpstream, spanName)
		span.SetStatus(codes.Error, "empty url")
		span.RecordError(err)
		return callResult{err: err}
	}

	callCtx, cancel := context.WithTimeout(ctx, h.cfg.PerCallTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, method, urlStr, nil)
	if err != nil {
		span.SetStatus(codes.Error, "build request")
		span.RecordError(err)
		return callResult{err: fmt.Errorf("upstream: build request: %w", err)}
	}
	req.Header.Set("Accept", "application/json")
	if auth.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+auth.Bearer)
	}
	if auth.Traceparent != "" {
		req.Header.Set("traceparent", auth.Traceparent)
	}
	if auth.TenantID != "" {
		req.Header.Set("X-Tenant-Id", auth.TenantID)
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

	resp, err := h.client.Do(req)
	if err != nil {
		span.SetStatus(codes.Error, "transport error")
		span.RecordError(err)
		return callResult{err: err}
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	if resp.StatusCode >= 500 {
		span.SetStatus(codes.Error, fmt.Sprintf("upstream %d", resp.StatusCode))
	}
	return callResult{status: resp.StatusCode, body: out, header: resp.Header}
}

// classify converts a callResult into either a parsed JSON payload + nil,
// (nil, nil) on 404, or (nil, ErrUpstream-wrap) on 5xx / transport error.
//
// 404 is treated as "no record" → callers (AggregatedView composers) render
// empty-state. 401/403 are treated as upstream errors here because the BFF
// is the trust boundary; if a downstream rejects the mesh metadata that's
// a misconfiguration, not a UX outcome.
func classifyJSON(method string, cr callResult) (any, error) {
	if cr.err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrUpstream, method, cr.err)
	}
	if cr.status == http.StatusNotFound {
		return nil, nil
	}
	if cr.status >= 400 {
		return nil, fmt.Errorf("%w: %s returned %d", ErrUpstream, method, cr.status)
	}
	if len(cr.body) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(cr.body, &v); err != nil {
		return nil, fmt.Errorf("%w: %s: malformed JSON: %v", ErrUpstream, method, err)
	}
	return v, nil
}

// urlHost returns the host portion of urlStr for span attribute; empty on
// parse error.
func urlHost(urlStr string) string {
	if urlStr == "" {
		return ""
	}
	if u, err := url.Parse(urlStr); err == nil {
		return u.Host
	}
	return ""
}

// -----------------------------------------------------------------------------
// Client interface methods
// -----------------------------------------------------------------------------

// GetLearningPath → chora-consumption GET /api/learning-paths.
func (h *HTTPUpstream) GetLearningPath(ctx context.Context, tenantID, gcid string) (any, error) {
	if h.shouldFail("GetLearningPath") {
		return nil, fmt.Errorf("%w: failure injection", ErrUpstream)
	}
	u := h.cfg.ConsumptionURL + "/api/learning-paths"
	auth := stampAuthFromContext(ctx, tenantID, gcid)
	return classifyJSON("GetLearningPath", h.call(ctx, "upstream.GetLearningPath", http.MethodGet, u, auth))
}

// GetRecentAtoms → chora-creation GET /api/atoms?status=published.
func (h *HTTPUpstream) GetRecentAtoms(ctx context.Context, tenantID, gcid string) (any, error) {
	if h.shouldFail("GetRecentAtoms") {
		return nil, fmt.Errorf("%w: failure injection", ErrUpstream)
	}
	u := h.cfg.CreationURL + "/api/atoms?status=published"
	auth := stampAuthFromContext(ctx, tenantID, gcid)
	return classifyJSON("GetRecentAtoms", h.call(ctx, "upstream.GetRecentAtoms", http.MethodGet, u, auth))
}

// GetCompanion → chora-consumption GET /companion/me.
func (h *HTTPUpstream) GetCompanion(ctx context.Context, tenantID, gcid string) (any, error) {
	if h.shouldFail("GetCompanion") {
		return nil, fmt.Errorf("%w: failure injection", ErrUpstream)
	}
	u := h.cfg.ConsumptionURL + "/companion/me"
	auth := stampAuthFromContext(ctx, tenantID, gcid)
	return classifyJSON("GetCompanion", h.call(ctx, "upstream.GetCompanion", http.MethodGet, u, auth))
}

// GetStreak → chora-consumption GET /v1/me/streak.
//
// Backs the GraphQL `myStreak` query (a live Daily Dose read). Callee:
// chora-consumption handleMeStreak (internal/adapter/http/router.go:637),
// which returns streakResp {learner_gcid, count, last_activity_at}.
func (h *HTTPUpstream) GetStreak(ctx context.Context, tenantID, gcid string) (any, error) {
	if h.shouldFail("GetStreak") {
		return nil, fmt.Errorf("%w: failure injection", ErrUpstream)
	}
	u := h.cfg.ConsumptionURL + "/v1/me/streak"
	auth := stampAuthFromContext(ctx, tenantID, gcid)
	return classifyJSON("GetStreak", h.call(ctx, "upstream.GetStreak", http.MethodGet, u, auth))
}

// GetFeed → chora-sharing GET /v1/feed/shared-atoms.
//
// chora-sharing mounts the learner feed at /v1/feed/shared-atoms
// (internal/adapter/http/handlers.go:387); there is no bare /v1/feed route,
// so the old path 404'd and classifyJSON turned that into a silent empty feed.
func (h *HTTPUpstream) GetFeed(ctx context.Context, tenantID, gcid string) (any, error) {
	if h.shouldFail("GetFeed") {
		return nil, fmt.Errorf("%w: failure injection", ErrUpstream)
	}
	u := h.cfg.SharingURL + "/v1/feed/shared-atoms"
	auth := stampAuthFromContext(ctx, tenantID, gcid)
	return classifyJSON("GetFeed", h.call(ctx, "upstream.GetFeed", http.MethodGet, u, auth))
}

// GetTenant → chora-tenancy GET /api/tenants/{id}.
//
// chora-tenancy mounts the tenant detail handler under /api/tenants/
// (internal/adapter/http/handlers.go:76); the legacy /tenants/{id} path 404'd.
// Mirrors the verified phyllis.GetMyTenant + gatewayproxy.GetTenant path.
func (h *HTTPUpstream) GetTenant(ctx context.Context, tenantID, gcid string) (any, error) {
	if h.shouldFail("GetTenant") {
		return nil, fmt.Errorf("%w: failure injection", ErrUpstream)
	}
	if h.cfg.TenancyURL == "" {
		return nil, fmt.Errorf("%w: GetTenant: empty TenancyURL", ErrUpstream)
	}
	u := h.cfg.TenancyURL + "/api/tenants/" + url.PathEscape(tenantID)
	auth := stampAuthFromContext(ctx, tenantID, gcid)
	return classifyJSON("GetTenant", h.call(ctx, "upstream.GetTenant", http.MethodGet, u, auth))
}

// GetGovernance → chora-governance GET /governance/{tenant_id}.
func (h *HTTPUpstream) GetGovernance(ctx context.Context, tenantID, gcid string) (any, error) {
	if h.shouldFail("GetGovernance") {
		return nil, fmt.Errorf("%w: failure injection", ErrUpstream)
	}
	if h.cfg.GovernanceURL == "" {
		return nil, fmt.Errorf("%w: GetGovernance: empty GovernanceURL", ErrUpstream)
	}
	u := h.cfg.GovernanceURL + "/governance/" + url.PathEscape(tenantID)
	auth := stampAuthFromContext(ctx, tenantID, gcid)
	return classifyJSON("GetGovernance", h.call(ctx, "upstream.GetGovernance", http.MethodGet, u, auth))
}

// GetAuditEvents → chora-observability GET /events?tenant_id=X.
func (h *HTTPUpstream) GetAuditEvents(ctx context.Context, tenantID, gcid string) (any, error) {
	if h.shouldFail("GetAuditEvents") {
		return nil, fmt.Errorf("%w: failure injection", ErrUpstream)
	}
	if h.cfg.ObservabilityURL == "" {
		return nil, fmt.Errorf("%w: GetAuditEvents: empty ObservabilityURL", ErrUpstream)
	}
	u := h.cfg.ObservabilityURL + "/events?tenant_id=" + url.QueryEscape(tenantID)
	auth := stampAuthFromContext(ctx, tenantID, gcid)
	return classifyJSON("GetAuditEvents", h.call(ctx, "upstream.GetAuditEvents", http.MethodGet, u, auth))
}

// GetCourses → chora-delivery GET /courses.
func (h *HTTPUpstream) GetCourses(ctx context.Context, tenantID, gcid string) (any, error) {
	if h.shouldFail("GetCourses") {
		return nil, fmt.Errorf("%w: failure injection", ErrUpstream)
	}
	if h.cfg.DeliveryURL == "" {
		return nil, fmt.Errorf("%w: GetCourses: empty DeliveryURL", ErrUpstream)
	}
	u := h.cfg.DeliveryURL + "/courses"
	auth := stampAuthFromContext(ctx, tenantID, gcid)
	return classifyJSON("GetCourses", h.call(ctx, "upstream.GetCourses", http.MethodGet, u, auth))
}

// stampAuthFromContext pulls AuthCtx from context and falls back to a
// minimally-populated AuthCtx so the mesh metadata is at least stamped with
// tenant + gcid even if the BFF handler forgot to call WithAuthCtx.
func stampAuthFromContext(ctx context.Context, tenantID, gcid string) AuthCtx {
	auth := AuthCtxFromContext(ctx)
	if auth.TenantID == "" {
		auth.TenantID = tenantID
	}
	if auth.GCID == "" {
		auth.GCID = gcid
	}
	return auth
}

// -----------------------------------------------------------------------------
// Compile-time interface check
// -----------------------------------------------------------------------------

var _ Client = (*HTTPUpstream)(nil)
