// Package upstream — Phase C governance client.
//
// governance_client.go is the real-wire client for chora-governance, replacing
// the FakeUpstream.GetGovernance deterministic placeholder. Per the O+
// hydration plan (`/Users/daleleung/.claude/plans/atomic-napping-spring.md`
// Phase C1), this client supports two transports:
//
//  1. gRPC (canonical inter-service per ADR-140 / Wave-1 G-FULL) for
//     GetIMDADashboard + QueryAuditEvents
//  2. HTTP for two Phase-B endpoints that don't have gRPC counterparts yet:
//     GET /api/hitl/pending + GET /api/imda/dimensions/{name}/rubric
//
// Both transports go through the Cloud Service Mesh sidecar, so mTLS is
// handled by the mesh CA (no manual cert plumbing per the
// an infra skill — mesh-internal hops automatically use
// PeerAuthentication=PERMISSIVE which lifts to STRICT under the chora-prod
// mesh policy).
//
// Endpoint resolution: per [[secrets-and-env]] — `CHORA_GOVERNANCE_GRPC_ADDR`
// + `CHORA_GOVERNANCE_HTTP_ADDR` come from env vars sourced from Terraform.
// Nothing inline. Boot fails loud if both are empty (caller's responsibility
// — the constructor wraps both in nil-safe handling for tests).
//
// Per `feedback_no_stubs_real_wiring`: there is NO in-process stub fallback
// here. The Fake / mock paths exist only as `FakeUpstream` in upstream.go
// (and ONLY behind `CHORA_GATEWAY_UPSTREAM_FAKE=true`). This client is
// strictly real-wire.
package upstream

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
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"

	governancev1 "github.com/apollo-chora/chora-contracts/gen/go/chora/services/governance/v1"
)

// DefaultGovernanceCallTimeout bounds each governance call. The IMDA
// dashboard + audit query are hot reads; HITL pending is a small list. 5 s is
// generous; mesh-internal p99 should be well under 1 s.
const DefaultGovernanceCallTimeout = 5 * time.Second

// HITL claim/release sentinel errors. The BFF handler uses `errors.Is` against
// these to translate upstream chora-governance responses to FE-facing HTTP
// statuses per the N7 contract:
//
//	ErrHITLNotFound   → upstream 404 (decision_id not found OR cross-tenant)
//	ErrHITLConflict   → upstream 409 (already claimed or terminal verdict)
//	ErrHITLForbidden  → upstream 403 (release by non-assignee)
//	ErrHITLInvalid    → upstream 422 (domain validation; blank gcid, etc.)
//
// 5xx upstream responses (incl. 503) fall through to the broader ErrUpstream
// sentinel, which the BFF handler maps to 503 for the FE.
var (
	ErrHITLNotFound  = errors.New("hitl decision not found")
	ErrHITLConflict  = errors.New("hitl decision conflict (claimed/terminal)")
	ErrHITLForbidden = errors.New("hitl decision forbidden (non-assignee)")
	ErrHITLInvalid   = errors.New("hitl decision invalid (validation)")
)

// GovernanceConfig wires the chora-governance endpoint addresses.
type GovernanceConfig struct {
	// GRPCAddr is the chora-governance gRPC endpoint (host:port). Mesh-internal
	// — TLS terminated by the sidecar. Empty disables gRPC calls (caller
	// receives ErrUpstream-wrapped error).
	GRPCAddr string
	// HTTPAddr is the chora-governance HTTP endpoint base URL (e.g.
	// `http://chora-governance.governance.svc.cluster.local:8080`). Empty
	// disables HTTP calls (HITL queue + rubric drilldown).
	HTTPAddr string
	// PerCallTimeout bounds each call. Defaults to DefaultGovernanceCallTimeout.
	PerCallTimeout time.Duration
}

// LoadGovernanceConfigFromEnv reads the canonical env vars per
// `feedback_no_inline_config`. Both addresses are optional at load time;
// callers fail-loud at boot if they require real wiring.
func LoadGovernanceConfigFromEnv() GovernanceConfig {
	c := GovernanceConfig{
		GRPCAddr: os.Getenv("CHORA_GOVERNANCE_GRPC_ADDR"),
		HTTPAddr: os.Getenv("CHORA_GOVERNANCE_HTTP_ADDR"),
	}
	if c.PerCallTimeout == 0 {
		c.PerCallTimeout = DefaultGovernanceCallTimeout
	}
	return c
}

// IMDADimensionSummary is the per-dimension shape the BFF surfaces to the FE
// in /bff/oplus/dashboard. Composed from GetIMDADashboard gRPC plus enriched
// fields (rubric pass/partial/fail counts, indicator ids).
type IMDADimensionSummary struct {
	ID                 string   `json:"id"` // accountability | transparency | safety_and_robustness | fairness_and_human_oversight
	Title              string   `json:"title"`
	ScorePct           int32    `json:"score_pct"`
	RubricPassCount    int      `json:"rubric_pass_count"`
	RubricPartialCount int      `json:"rubric_partial_count"`
	RubricFailCount    int      `json:"rubric_fail_count"`
	IndicatorIDs       []string `json:"indicator_ids"`
}

// HITLPendingItem is the read-only HITL queue row (Phase C: buttons are
// disabled; this is purely a display fetch). Shape mirrors the
// chora_governance.hitl_decision_log projection emitted by Phase B B4 +
// chora-governance/internal/adapter/http handler.
//
// Phase-D enrichment (audit finding #2):
//   - CorrelationID → FE `workflow_id`
//   - DecisionKind → FE `gate` (mapped to question_review/report_review/material_decision)
//   - Summary (critic_notes) → FE `summary`
//   - AutonomyLevel → FE `autonomy_level`
//   - CreatedAt → FE `waiting_since`
//   - AssigneeGCID is now a real nullable field (migration 0008 added
//     hitl_decision_log.assignee_gcid). NULL = unassigned; the BFF passes
//     through and the FE renders an "unassigned" localised string.
type HITLPendingItem struct {
	DecisionID  string    `json:"decision_id"`
	TenantID    string    `json:"tenant_id"`
	GCID        string    `json:"gcid"`
	AgentID     string    `json:"agent_id"`
	CrewName    string    `json:"crew_name,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	TraceID     string    `json:"trace_id,omitempty"`
	ResourceURL string    `json:"resource_url,omitempty"`

	// --- Phase-D enrichment (audit finding #2) ----------------------------
	// CorrelationID is the workflow correlation id. Maps to FE `workflow_id`.
	CorrelationID string `json:"correlation_id,omitempty"`
	// DecisionKind is the projector's decision_kind (e.g. `question_review`,
	// `report_review`, `material_decision`). Maps to FE `gate` after
	// normalisation.
	DecisionKind string `json:"decision_kind,omitempty"`
	// Summary is the critic / classifier note describing why HITL is needed.
	// Maps to FE `summary`. Falls back to Reason when empty.
	Summary string `json:"summary,omitempty"`
	// AutonomyLevel is the autonomy-level tag (HOOTL / HOTL / HITL-L0..L2).
	// Maps to FE `autonomy_level`.
	AutonomyLevel string `json:"autonomy_level,omitempty"`
	// AssigneeGCID is the reviewer GCID currently responsible for the gate.
	// `nil` = unassigned (default at creation; rows sit in the shared queue
	// until a reviewer claims via the deferred-to-wave-N+1 claim endpoint).
	// Pass-through from chora-governance hitl_decision_log.assignee_gcid;
	// distinct from operator_gcid which is the post-verdict "who decided"
	// provenance. Per [[feedback-no-stubs-real-wiring]] the BFF never
	// substitutes a synthetic placeholder.
	AssigneeGCID *string `json:"assignee_gcid"`
}

// RubricItem is the per-rubric-item drilldown row. Shape mirrors the Phase B
// B3 resolver output (auto | static derivation_mode). The original
// chora-governance producer fields are `id`, `title`, `evidence_source`,
// `status`, `evidence_source_url`. The BFF transformer maps these to the
// FE-canonical shape (`ref`, `requirement`, `tools[]`, `tool_coverage`,
// `evidence_source_url`) — see RubricItemView in handlers_oplus.go.
type RubricItem struct {
	ID                string `json:"id"`
	Ref               string `json:"ref,omitempty"` // short rubric ref ("1.1") — preferred over the id slug for display
	Title             string `json:"title"`
	EvidenceSource    string `json:"evidence_source"`
	Status            string `json:"status"` // PASS | PARTIAL | FAIL
	EvidenceSourceURL string `json:"evidence_source_url,omitempty"`
	// Priority is the remediation priority (P1 | P2 | P3) emitted by the
	// chora-governance rubric resolver. Drives the O+ dimensions traffic-light
	// (P1 FAIL → red; ≥2 P2 FAILs → amber) + the per-item badge. May be empty
	// for legacy/unscored items.
	Priority string `json:"priority,omitempty"`
}

// DimensionRubric is the full rubric expand for one dimension.
type DimensionRubric struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// ScorePct is the 0..100 dimension score. chora-governance's rubric
	// endpoint returns a 0..1 `pass_rate` (NOT `score_pct`); GetDimensionRubric
	// derives ScorePct = PassRate*100 post-parse.
	ScorePct    int32   `json:"score_pct"`
	PassRate    float64 `json:"pass_rate"`
	Description string  `json:"description,omitempty"`
	// RubricItems maps governance's `items` array (the upstream field is
	// `items`, NOT `rubric_items`).
	RubricItems []RubricItem `json:"items"`
}

// AuditEvent is the BFF-facing slice of governance.v1.AuditEvent — only the
// fields O+ Decision Traces tab + Cloud Trace deep-link rendering need.
type AuditEvent struct {
	EventID     string    `json:"event_id"`
	TenantID    string    `json:"tenant_id"`
	GCID        string    `json:"gcid,omitempty"`
	AGID        string    `json:"agid,omitempty"`
	Action      string    `json:"action"`
	Resource    string    `json:"resource,omitempty"`
	Decision    string    `json:"decision"` // permitted | denied | unspecified
	Reason      string    `json:"reason,omitempty"`
	Traceparent string    `json:"traceparent,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// EgressAuditAction is the audit_log.action discriminator the external-egress
// audit rows carry (matches events.ActionExternalEgress in chora-governance).
// The O+ egress-audit read filters /api/audit on this action.
const EgressAuditAction = "external_egress"

// DefaultEgressAuditLimit bounds the O+ egress-audit slice.
const DefaultEgressAuditLimit = 100

// EgressAuditEvent is the O+ slice of an external_egress audit_log row. The
// base audit fields plus the full ExternalEgressAudited payload in After — the
// egress-audit panel drills into agent_id / denial_reason / Armor verdicts /
// citation_count / web_search_queries, all preserved in that raw JSON.
type EgressAuditEvent struct {
	EventID     string          `json:"event_id"`
	TenantID    string          `json:"tenant_id"`
	ActorGCID   string          `json:"actor_gcid,omitempty"`
	Action      string          `json:"action"`
	Decision    string          `json:"decision"` // permitted | denied
	Reason      string          `json:"reason,omitempty"`
	SubjectType string          `json:"subject_type,omitempty"`
	SubjectID   string          `json:"subject_id,omitempty"`
	Traceparent string          `json:"traceparent,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	After       json.RawMessage `json:"after,omitempty"`
}

// GovernanceReadError carries an upstream chora-governance non-2xx HTTP status
// so the BFF surfaces a governance DENY as a 4xx rather than a masked 5xx (a
// 502'd deny erases its own reason). It is returned ONLY for 4xx upstream
// responses; 5xx / transport failures return an ErrUpstream-wrapped error.
type GovernanceReadError struct {
	StatusCode int
	Body       string
}

func (e *GovernanceReadError) Error() string {
	return fmt.Sprintf("governance read: upstream %d", e.StatusCode)
}

// GovernanceGRPCClient is the minimal slice of governancev1.GovernanceClient
// that chora-gateway calls. Tests inject a fake; production wires the real
// `governancev1.NewGovernanceClient(conn)`.
type GovernanceGRPCClient interface {
	GetIMDADashboard(ctx context.Context, in *governancev1.GetIMDADashboardRequest, opts ...grpc.CallOption) (*governancev1.GetIMDADashboardResponse, error)
	QueryAuditEvents(ctx context.Context, in *governancev1.QueryAuditEventsRequest, opts ...grpc.CallOption) (*governancev1.QueryAuditEventsResponse, error)
}

// GovernanceClient is the real client implementation. It does NOT implement
// upstream.Client (the legacy 8-method interface) — instead it exposes the
// narrow O+ surface methods the new handlers_oplus.go handlers call.
//
// Construct via NewGovernanceClient. The gRPC + HTTP clients can be wired
// independently (tests typically wire only one).
type GovernanceClient struct {
	cfg    GovernanceConfig
	rpc    GovernanceGRPCClient // nil-safe; methods that need it fail with ErrUpstream when nil
	http   *http.Client
	tracer trace.Tracer
}

// GovernanceClientConfig is the constructor input.
type GovernanceClientConfig struct {
	Endpoint GovernanceConfig
	RPC      GovernanceGRPCClient // production: governancev1.NewGovernanceClient(conn)
	HTTP     *http.Client         // production: &http.Client{Timeout: Endpoint.PerCallTimeout}
}

// NewGovernanceClient constructs a GovernanceClient. Both RPC + HTTP can be
// nil; methods that need them return ErrUpstream-wrapped errors so callers
// fail loud rather than silently degrading to mock data.
func NewGovernanceClient(cfg GovernanceClientConfig) *GovernanceClient {
	endpoint := cfg.Endpoint
	if endpoint.PerCallTimeout <= 0 {
		endpoint.PerCallTimeout = DefaultGovernanceCallTimeout
	}
	httpClient := cfg.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: endpoint.PerCallTimeout}
	}
	return &GovernanceClient{
		cfg:    endpoint,
		rpc:    cfg.RPC,
		http:   httpClient,
		tracer: otel.Tracer("chora-gateway/upstream/governance"),
	}
}

// GetIMDADashboard fetches the 4-dimension IMDA scoring for a tenant via
// gRPC. The result is normalised into IMDADimensionSummary form (rubric
// counts are left zero; the handler enriches via GetDimensionRubric).
func (g *GovernanceClient) GetIMDADashboard(ctx context.Context, tenantID string) ([]IMDADimensionSummary, error) {
	if g == nil || g.rpc == nil {
		return nil, fmt.Errorf("%w: governance: gRPC client not wired", ErrUpstream)
	}
	ctx, span := g.tracer.Start(ctx, "upstream.governance.GetIMDADashboard",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("chora.tenant_id", tenantID),
		),
	)
	defer span.End()

	callCtx, cancel := context.WithTimeout(ctx, g.cfg.PerCallTimeout)
	defer cancel()
	resp, err := g.rpc.GetIMDADashboard(callCtx, &governancev1.GetIMDADashboardRequest{
		TenantId: tenantID,
	})
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return nil, fmt.Errorf("%w: GetIMDADashboard: %v", ErrUpstream, err)
	}
	if resp == nil {
		return []IMDADimensionSummary{}, nil
	}
	out := make([]IMDADimensionSummary, 0, len(resp.GetDimensions()))
	for _, d := range resp.GetDimensions() {
		out = append(out, IMDADimensionSummary{
			ID:           dimensionEnumToID(d.GetDimension()),
			Title:        dimensionTitle(d.GetDimension()),
			ScorePct:     d.GetScore(),
			IndicatorIDs: append([]string(nil), d.GetIndicators()...),
		})
	}
	return out, nil
}

// QueryAuditEvents pulls a paginated slice of audit events for the Decision
// Traces tab. Limit defaults to 50.
func (g *GovernanceClient) QueryAuditEvents(ctx context.Context, tenantID string, limit int32) ([]AuditEvent, error) {
	if g == nil || g.rpc == nil {
		return nil, fmt.Errorf("%w: governance: gRPC client not wired", ErrUpstream)
	}
	if limit <= 0 {
		limit = 50
	}
	ctx, span := g.tracer.Start(ctx, "upstream.governance.QueryAuditEvents",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("chora.tenant_id", tenantID),
			attribute.Int("chora.audit.limit", int(limit)),
		),
	)
	defer span.End()

	callCtx, cancel := context.WithTimeout(ctx, g.cfg.PerCallTimeout)
	defer cancel()
	resp, err := g.rpc.QueryAuditEvents(callCtx, &governancev1.QueryAuditEventsRequest{
		TenantId: tenantID,
		Limit:    limit,
	})
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return nil, fmt.Errorf("%w: QueryAuditEvents: %v", ErrUpstream, err)
	}
	if resp == nil {
		return []AuditEvent{}, nil
	}
	out := make([]AuditEvent, 0, len(resp.GetEvents()))
	for _, e := range resp.GetEvents() {
		ev := AuditEvent{
			EventID:     e.GetEventId(),
			TenantID:    e.GetTenantId(),
			GCID:        e.GetGcid(),
			AGID:        e.GetAgid(),
			Action:      e.GetAction(),
			Resource:    e.GetResource(),
			Decision:    decisionEnumToString(e.GetDecision()),
			Reason:      e.GetReason(),
			Traceparent: e.GetTraceparent(),
		}
		if ts := e.GetCreatedAt(); ts != nil {
			ev.CreatedAt = ts.AsTime()
		}
		out = append(out, ev)
	}
	return out, nil
}

// QueryEgressAudit reads the external-egress audit slice from
// `GET /api/audit?action=external_egress` on chora-governance (HTTP; the route
// is already gateway-allow-listed in the mesh, so no mesh change). Limit
// defaults to DefaultEgressAuditLimit.
//
// Status mapping is honest so a governance DENY is not masked:
//   - 2xx            → parsed []EgressAuditEvent
//   - 4xx            → *GovernanceReadError (the BFF returns the same 4xx)
//   - 5xx / transport → ErrUpstream-wrapped error (the BFF returns 503)
func (g *GovernanceClient) QueryEgressAudit(ctx context.Context, tenantID string, limit int) ([]EgressAuditEvent, error) {
	if g == nil || g.cfg.HTTPAddr == "" {
		return nil, fmt.Errorf("%w: governance: HTTP addr not wired", ErrUpstream)
	}
	if limit <= 0 {
		limit = DefaultEgressAuditLimit
	}
	q := url.Values{}
	if tenantID != "" {
		q.Set("tenant_id", tenantID)
	}
	q.Set("action", EgressAuditAction)
	q.Set("limit", fmt.Sprintf("%d", limit))
	u := strings.TrimRight(g.cfg.HTTPAddr, "/") + "/api/audit?" + q.Encode()

	body, status, err := g.httpGetJSONStatus(ctx, "upstream.governance.QueryEgressAudit", u, tenantID)
	if err != nil {
		return nil, fmt.Errorf("%w: QueryEgressAudit: %v", ErrUpstream, err)
	}
	if status >= 400 && status < 500 {
		// Deny / client error — surface the real status so the BFF returns 4xx,
		// never a masked 5xx (a 404 here is likewise a genuine 4xx).
		return nil, &GovernanceReadError{StatusCode: status, Body: string(body)}
	}
	if status >= 500 {
		return nil, fmt.Errorf("%w: QueryEgressAudit: upstream %d", ErrUpstream, status)
	}
	if len(body) == 0 {
		return []EgressAuditEvent{}, nil
	}
	// chora-governance /api/audit returns the canonical listResponse envelope
	// (`{items:[...], total}`); accept a bare array too for robustness.
	var envelope struct {
		Items []EgressAuditEvent `json:"items"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Items != nil {
		return envelope.Items, nil
	}
	var bare []EgressAuditEvent
	if err := json.Unmarshal(body, &bare); err != nil {
		return nil, fmt.Errorf("%w: QueryEgressAudit: malformed JSON: %v", ErrUpstream, err)
	}
	if bare == nil {
		return []EgressAuditEvent{}, nil
	}
	return bare, nil
}

// GetHITLPending fetches the read-only HITL queue from
// `GET /api/hitl/pending` on chora-governance. Limit defaults to 50.
func (g *GovernanceClient) GetHITLPending(ctx context.Context, tenantID string, limit int) ([]HITLPendingItem, error) {
	if g == nil || g.cfg.HTTPAddr == "" {
		return nil, fmt.Errorf("%w: governance: HTTP addr not wired", ErrUpstream)
	}
	if limit <= 0 {
		limit = 50
	}
	q := url.Values{}
	if tenantID != "" {
		q.Set("tenant_id", tenantID)
	}
	q.Set("limit", fmt.Sprintf("%d", limit))
	u := strings.TrimRight(g.cfg.HTTPAddr, "/") + "/api/hitl/pending"
	if encoded := q.Encode(); encoded != "" {
		u += "?" + encoded
	}

	body, err := g.httpGetJSON(ctx, "upstream.governance.GetHITLPending", u, tenantID)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return []HITLPendingItem{}, nil
	}
	// Accept both bare-array and `{items:[...]}` shapes; chora-governance
	// canonical envelope is the latter but bare-array reads are safer for
	// migration windows.
	var envelope struct {
		Items []HITLPendingItem `json:"items"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Items != nil {
		return envelope.Items, nil
	}
	var out []HITLPendingItem
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("%w: GetHITLPending: malformed JSON: %v", ErrUpstream, err)
	}
	return out, nil
}

// GetDimensionRubric fetches the full rubric expand for one dimension from
// `GET /api/imda/dimensions/{name}/rubric`. `dimension` is one of the 4
// canonical labels per ADR-141.
func (g *GovernanceClient) GetDimensionRubric(ctx context.Context, tenantID, dimension string) (*DimensionRubric, error) {
	if g == nil || g.cfg.HTTPAddr == "" {
		return nil, fmt.Errorf("%w: governance: HTTP addr not wired", ErrUpstream)
	}
	name := strings.TrimSpace(dimension)
	if name == "" {
		return nil, fmt.Errorf("%w: dimension required", ErrUpstream)
	}
	q := url.Values{}
	if tenantID != "" {
		q.Set("tenant_id", tenantID)
	}
	u := strings.TrimRight(g.cfg.HTTPAddr, "/") + "/api/imda/dimensions/" + url.PathEscape(name) + "/rubric"
	if encoded := q.Encode(); encoded != "" {
		u += "?" + encoded
	}

	body, err := g.httpGetJSON(ctx, "upstream.governance.GetDimensionRubric", u, tenantID)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("%w: GetDimensionRubric: empty body", ErrUpstream)
	}
	var out DimensionRubric
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("%w: GetDimensionRubric: malformed JSON: %v", ErrUpstream, err)
	}
	if out.ID == "" {
		out.ID = name
	}
	if out.Title == "" {
		out.Title = dimensionTitleByID(name)
	}
	// chora-governance returns a 0..1 `pass_rate`; the FE + deriveDimensionStatus
	// expect a 0..100 ScorePct. Derive it (governance never sends score_pct).
	if out.ScorePct == 0 && out.PassRate > 0 {
		out.ScorePct = int32(out.PassRate * 100)
	}
	return &out, nil
}

// HITLDecisionItem is the BFF projection of the chora-governance
// `hitlPendingItem` shape, returned as the body of POST
// /api/hitl/decisions/{id}/{claim|release}. Mirrors the upstream JSON one-to-one
// so the BFF can return it verbatim to the FE.
type HITLDecisionItem struct {
	DecisionID     string  `json:"decision_id"`
	RunID          string  `json:"run_id"`
	OperatorGcid   string  `json:"operator_gcid,omitempty"`
	AssigneeGcid   *string `json:"assignee_gcid"`
	Decision       string  `json:"decision"`
	AutonomyLevel  string  `json:"autonomy_level"`
	LifecycleStage string  `json:"lifecycle_stage"`
	DecidedAt      string  `json:"decided_at,omitempty"`
}

// ClaimHITLDecision POSTs to chora-governance
// `/api/hitl/decisions/{id}/claim` with `{"operator_gcid":"..."}`. Per N7
// contract:
//
//	200 → decoded *HITLDecisionItem, nil
//	404 → nil, ErrHITLNotFound
//	409 → nil, ErrHITLConflict (already claimed / terminal)
//	403 → nil, ErrHITLForbidden (release-only — included for symmetry)
//	422 → nil, ErrHITLInvalid (domain validation)
//	5xx / network → nil, ErrUpstream wrap
func (g *GovernanceClient) ClaimHITLDecision(ctx context.Context, tenantID, decisionID, operatorGcid string) (*HITLDecisionItem, error) {
	return g.postHITLAction(ctx, "upstream.governance.ClaimHITLDecision",
		tenantID, decisionID, "claim", operatorGcid, "")
}

// ReleaseHITLDecision POSTs to chora-governance
// `/api/hitl/decisions/{id}/release` with `{"operator_gcid":"..."}`. Same
// contract as ClaimHITLDecision; 403 is meaningful here (non-assignee can
// not release).
func (g *GovernanceClient) ReleaseHITLDecision(ctx context.Context, tenantID, decisionID, operatorGcid string) (*HITLDecisionItem, error) {
	return g.postHITLAction(ctx, "upstream.governance.ReleaseHITLDecision",
		tenantID, decisionID, "release", operatorGcid, "")
}

// ApproveHITLDecision POSTs to chora-governance
// `/api/hitl/decisions/{id}/approve` with `{"operator_gcid":"...","note":"..."}`.
// `note` is optional (empty omits the JSON field). Same status contract as
// ClaimHITLDecision; 403 is meaningful (only the current assignee may record a
// verdict), 409 for a terminal decision already recorded.
func (g *GovernanceClient) ApproveHITLDecision(ctx context.Context, tenantID, decisionID, operatorGcid, note string) (*HITLDecisionItem, error) {
	return g.postHITLAction(ctx, "upstream.governance.ApproveHITLDecision",
		tenantID, decisionID, "approve", operatorGcid, note)
}

// RejectHITLDecision POSTs to chora-governance
// `/api/hitl/decisions/{id}/reject` with `{"operator_gcid":"...","note":"..."}`.
// `note` is optional. Same status contract as ApproveHITLDecision.
func (g *GovernanceClient) RejectHITLDecision(ctx context.Context, tenantID, decisionID, operatorGcid, note string) (*HITLDecisionItem, error) {
	return g.postHITLAction(ctx, "upstream.governance.RejectHITLDecision",
		tenantID, decisionID, "reject", operatorGcid, note)
}

// postHITLAction is the shared POST /api/hitl/decisions/{id}/{action} call.
// `note` is optional — when non-empty it is included in the JSON body
// alongside operator_gcid (used by the approve/reject verdict endpoints);
// claim/release pass "" and the field is omitted.
func (g *GovernanceClient) postHITLAction(ctx context.Context, spanName, tenantID, decisionID, action, operatorGcid, note string) (*HITLDecisionItem, error) {
	if g == nil || g.cfg.HTTPAddr == "" {
		return nil, fmt.Errorf("%w: governance: HTTP addr not wired", ErrUpstream)
	}
	if strings.TrimSpace(decisionID) == "" {
		return nil, fmt.Errorf("%w: decision id required", ErrHITLInvalid)
	}
	bodyFields := map[string]string{"operator_gcid": operatorGcid}
	if note != "" {
		bodyFields["note"] = note
	}
	payload, err := json.Marshal(bodyFields)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal body: %v", ErrUpstream, err)
	}
	urlStr := strings.TrimRight(g.cfg.HTTPAddr, "/") +
		"/api/hitl/decisions/" + url.PathEscape(decisionID) + "/" + action

	body, status, err := g.httpPostJSON(ctx, spanName, urlStr, tenantID, payload)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		var item HITLDecisionItem
		if len(body) == 0 {
			return &item, nil
		}
		if err := json.Unmarshal(body, &item); err != nil {
			return nil, fmt.Errorf("%w: %s: malformed JSON: %v", ErrUpstream, spanName, err)
		}
		return &item, nil
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: upstream 404", ErrHITLNotFound)
	case http.StatusConflict:
		return nil, fmt.Errorf("%w: upstream 409", ErrHITLConflict)
	case http.StatusForbidden:
		return nil, fmt.Errorf("%w: upstream 403", ErrHITLForbidden)
	case http.StatusUnprocessableEntity, http.StatusBadRequest:
		return nil, fmt.Errorf("%w: upstream %d", ErrHITLInvalid, status)
	default:
		return nil, fmt.Errorf("%w: upstream %d", ErrUpstream, status)
	}
}

// httpPostJSON performs a POST with traceparent + tenant header propagation
// and returns the raw body + status. Distinct from httpGetJSON because the
// HITL claim/release contract requires inspecting 200/404/409/403/422 codes
// — httpGetJSON collapses 4xx to ErrUpstream which would erase the
// differentiation the BFF handler needs.
func (g *GovernanceClient) httpPostJSON(ctx context.Context, spanName, urlStr, tenantID string, body []byte) ([]byte, int, error) {
	ctx, span := g.tracer.Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("http.request.method", http.MethodPost),
			attribute.String("server.address", urlHost(urlStr)),
			attribute.String("chora.tenant_id", tenantID),
		),
	)
	defer span.End()

	callCtx, cancel := context.WithTimeout(ctx, g.cfg.PerCallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, urlStr, bytes.NewReader(body))
	if err != nil {
		span.SetStatus(codes.Error, "build request")
		span.RecordError(err)
		return nil, 0, fmt.Errorf("%w: build request: %v", ErrUpstream, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	auth := AuthCtxFromContext(ctx)
	if auth.Traceparent != "" {
		req.Header.Set("traceparent", auth.Traceparent)
	}
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	// chora-governance /api/* require a gcid header alongside X-Tenant-Id
	// (handler middleware GOV_GCID_REQUIRED; primary "gcid", fallback
	// "X-Chora-GCID"). Without it the O+ rubric + hitl-pending reads 400.
	if auth.GCID != "" {
		req.Header.Set("gcid", auth.GCID)
	}
	if auth.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+auth.Bearer)
	}

	resp, err := g.http.Do(req)
	if err != nil {
		span.SetStatus(codes.Error, "transport error")
		span.RecordError(err)
		return nil, 0, fmt.Errorf("%w: transport: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	if resp.StatusCode >= 500 {
		span.SetStatus(codes.Error, fmt.Sprintf("upstream %d", resp.StatusCode))
	}
	return out, resp.StatusCode, nil
}

// httpGetJSON performs a GET with traceparent + tenant header propagation
// and returns the raw body. 5xx → ErrUpstream; 4xx other than 404 →
// ErrUpstream; 404 → empty body + nil error.
func (g *GovernanceClient) httpGetJSON(ctx context.Context, spanName, urlStr, tenantID string) ([]byte, error) {
	ctx, span := g.tracer.Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("http.request.method", http.MethodGet),
			attribute.String("server.address", urlHost(urlStr)),
			attribute.String("chora.tenant_id", tenantID),
		),
	)
	defer span.End()

	callCtx, cancel := context.WithTimeout(ctx, g.cfg.PerCallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, urlStr, nil)
	if err != nil {
		span.SetStatus(codes.Error, "build request")
		span.RecordError(err)
		return nil, fmt.Errorf("%w: build request: %v", ErrUpstream, err)
	}
	req.Header.Set("Accept", "application/json")
	auth := AuthCtxFromContext(ctx)
	if auth.Traceparent != "" {
		req.Header.Set("traceparent", auth.Traceparent)
	}
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	// chora-governance /api/* require a gcid header alongside X-Tenant-Id
	// (handler middleware GOV_GCID_REQUIRED; primary "gcid", fallback
	// "X-Chora-GCID"). Without it the O+ rubric + hitl-pending reads 400.
	if auth.GCID != "" {
		req.Header.Set("gcid", auth.GCID)
	}
	if auth.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+auth.Bearer)
	}

	resp, err := g.http.Do(req)
	if err != nil {
		span.SetStatus(codes.Error, "transport error")
		span.RecordError(err)
		return nil, fmt.Errorf("%w: transport: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		span.SetStatus(codes.Error, fmt.Sprintf("upstream %d", resp.StatusCode))
		return nil, fmt.Errorf("%w: %d", ErrUpstream, resp.StatusCode)
	}
	return out, nil
}

// httpGetJSONStatus performs a GET with traceparent + tenant header
// propagation and returns the raw body + HTTP status. Unlike httpGetJSON it
// does NOT collapse 4xx/404 to ErrUpstream — the caller inspects the status so
// an upstream DENY (4xx) can be surfaced as a 4xx rather than masked as a 5xx.
// `err` is non-nil only for request-build / transport failures.
func (g *GovernanceClient) httpGetJSONStatus(ctx context.Context, spanName, urlStr, tenantID string) ([]byte, int, error) {
	ctx, span := g.tracer.Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("http.request.method", http.MethodGet),
			attribute.String("server.address", urlHost(urlStr)),
			attribute.String("chora.tenant_id", tenantID),
		),
	)
	defer span.End()

	callCtx, cancel := context.WithTimeout(ctx, g.cfg.PerCallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, urlStr, nil)
	if err != nil {
		span.SetStatus(codes.Error, "build request")
		span.RecordError(err)
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	auth := AuthCtxFromContext(ctx)
	if auth.Traceparent != "" {
		req.Header.Set("traceparent", auth.Traceparent)
	}
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	// chora-governance /api/* require a gcid header alongside X-Tenant-Id
	// (handler middleware GOV_GCID_REQUIRED). Without it the read 400s.
	if auth.GCID != "" {
		req.Header.Set("gcid", auth.GCID)
	}
	if auth.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+auth.Bearer)
	}

	resp, err := g.http.Do(req)
	if err != nil {
		span.SetStatus(codes.Error, "transport error")
		span.RecordError(err)
		return nil, 0, fmt.Errorf("transport: %w", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	if resp.StatusCode >= 500 {
		span.SetStatus(codes.Error, fmt.Sprintf("upstream %d", resp.StatusCode))
	}
	return out, resp.StatusCode, nil
}

// -----------------------------------------------------------------------------
// Helpers — IMDA dimension label canonicalisation per ADR-141
// -----------------------------------------------------------------------------

// dimensionEnumToID maps the IMDA proto enum to the lowercase canonical ID
// (accountability / transparency / safety_and_robustness /
// fairness_and_human_oversight). Per ADR-141.
func dimensionEnumToID(d governancev1.IMDADimension) string {
	switch d {
	case governancev1.IMDADimension_IMDA_DIMENSION_ACCOUNTABILITY:
		return "accountability"
	case governancev1.IMDADimension_IMDA_DIMENSION_TRANSPARENCY:
		return "transparency"
	case governancev1.IMDADimension_IMDA_DIMENSION_SAFETY_AND_ROBUSTNESS:
		return "safety_and_robustness"
	case governancev1.IMDADimension_IMDA_DIMENSION_FAIRNESS_AND_HUMAN_OVERSIGHT:
		return "fairness_and_human_oversight"
	default:
		return "unspecified"
	}
}

// dimensionTitle returns the display label for a proto-enum dimension.
func dimensionTitle(d governancev1.IMDADimension) string {
	return dimensionTitleByID(dimensionEnumToID(d))
}

// dimensionTitleByID returns the display label for a canonical-ID dimension.
func dimensionTitleByID(id string) string {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "accountability":
		return "Accountability"
	case "transparency":
		return "Transparency"
	case "safety_and_robustness":
		return "Safety & Robustness"
	case "fairness_and_human_oversight":
		return "Fairness & Human Oversight"
	default:
		return "Unspecified"
	}
}

// decisionEnumToString canonicalises the audit decision enum to lowercase
// string for FE consumption.
func decisionEnumToString(d governancev1.AuditDecision) string {
	switch d {
	case governancev1.AuditDecision_AUDIT_DECISION_PERMITTED:
		return "permitted"
	case governancev1.AuditDecision_AUDIT_DECISION_DENIED:
		return "denied"
	default:
		return "unspecified"
	}
}

// CanonicalIMDADimensions is the static list of 4 canonical dimension IDs
// per ADR-141. Handlers iterate this list to fan out the per-dimension
// rubric calls in handlers_oplus.go.
var CanonicalIMDADimensions = []string{
	"accountability",
	"transparency",
	"safety_and_robustness",
	"fairness_and_human_oversight",
}

// ensure unused-import safety when grpc.CallOption / errors are only used
// transitively.
var _ = errors.New
var _ grpc.CallOption = grpc.EmptyCallOption{}
