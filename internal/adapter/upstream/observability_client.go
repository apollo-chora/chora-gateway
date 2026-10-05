// Package upstream — Phase C observability client.
//
// observability_client.go is the real-wire client for chora-observability,
// covering the O+ surface's needs in the BFF:
//
//   - `GET /api/v1/observability/agents`   — Crews + Agents hierarchy (B5)
//   - `GET /api/agent-decisions`           — Paginated decision list for
//     the Governance > Decision Traces tab
//   - `GET /api/correlations/{id}`         — Single-decision trace
//     correlation drilldown
//
// HTTP transport over the Cloud Service Mesh sidecar (mTLS via mesh CA). All
// addresses come from env vars per `feedback_no_inline_config`.
//
// Per `feedback_no_stubs_real_wiring`: no in-process stub fallback — failures
// surface as ErrUpstream-wrapped errors; the FE renders the
// `{state:'error'}` discriminated-union variant.
package upstream

import (
	"context"
	"encoding/json"
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

	"github.com/apollo-chora/chora-common/auth/servicemesh"
)

// DefaultObservabilityCallTimeout — same shape as the governance client.
const DefaultObservabilityCallTimeout = 5 * time.Second

// ObservabilityConfig wires the chora-observability endpoint.
type ObservabilityConfig struct {
	HTTPAddr       string
	PerCallTimeout time.Duration
}

// LoadObservabilityConfigFromEnv reads `CHORA_OBSERVABILITY_HTTP_ADDR` per
// `feedback_no_inline_config`. Falls back to the canonical `SVC_OBSERVABILITY_URL`
// shared with the HTTPUpstream legacy path when the dedicated var is unset.
func LoadObservabilityConfigFromEnv() ObservabilityConfig {
	c := ObservabilityConfig{
		HTTPAddr: os.Getenv("CHORA_OBSERVABILITY_HTTP_ADDR"),
	}
	if c.HTTPAddr == "" {
		c.HTTPAddr = os.Getenv("SVC_OBSERVABILITY_URL")
	}
	if c.PerCallTimeout == 0 {
		c.PerCallTimeout = DefaultObservabilityCallTimeout
	}
	return c
}

// AgentStats is the per-agent aggregate emitted by chora-observability's
// `/api/v1/observability/agents`. Counts are 24h-rolling per the Phase B5
// projection (chora.observability.agent_decision.logged.v1 grouped by
// (crew_name, agent_id)).
type AgentStats struct {
	Invocations24h int     `json:"invocations_24h"`
	P95LatencyMS   float64 `json:"p95_latency_ms"`
	RefusalRate    float64 `json:"refusal_rate"`
}

// Agent is one agent within a crew. cloud_trace_template_url powers the
// "View in Cloud Trace ↗" deep-link button per anchoring-decision #2
// (selective deep-link — no raw-span rendering in O+). The Vertex AI Agent
// Engine deep-link was removed — decommissioned per ADR-169.
type Agent struct {
	AgentID               string     `json:"agent_id"`
	Role                  string     `json:"role,omitempty"`
	EngineID              string     `json:"engine_id,omitempty"`
	CloudTraceTemplateURL string     `json:"cloud_trace_template_url,omitempty"`
	Stats                 AgentStats `json:"stats"`
}

// Crew groups agents (many-to-many keyed via `chora.crew_name` on every
// event per anchoring-decision #7).
type Crew struct {
	CrewName string `json:"crew_name"`
	CrewID   string `json:"crew_id,omitempty"`
	// HasRecentActivity is the crew-level rollup chora-observability emits
	// (true iff the SUM of the crew's agent invocation counts over the window
	// is > 0). The BFF re-marshals crews through this typed struct, so the
	// field MUST exist here or it is dropped before reaching the O+ FE, which
	// reads crew.has_recent_activity to gate the "no recent activity" banner.
	HasRecentActivity bool    `json:"has_recent_activity"`
	Agents            []Agent `json:"agents"`
}

// AgentsResponse is the full B5 response shape. The chora-observability HTTP
// handler returns either this envelope or a bare `{crews: [...]}` body —
// both are accepted on the unmarshal path.
type AgentsResponse struct {
	Crews []Crew `json:"crews"`
}

// AgentPromptsResponse is the CHO-2364 (ADR-197 read slice) prompt-evidence
// body from `GET /api/v1/observability/agent-prompts`. Agents is kept as RAW
// JSON on purpose: the BFF passes the five-agent evidence array through
// untouched, so a typed re-marshal cannot silently drop upstream fields (the
// Crew.has_recent_activity lesson).
type AgentPromptsResponse struct {
	Agents json.RawMessage `json:"agents"`
}

// AgentDecisionRow is the BFF-facing slice of a single AgentDecisionLog row.
// Shape mirrors the chora-observability handler's GET /api/agent-decisions
// response. Additional Phase-D fields (DecisionKind, ModelID, Confidence,
// CostUSD, AutonomyLevel, ActedUpon, CorrelationID) are emitted by the
// chora-observability projector when the source AgentDecisionLog row + the
// TokenUsageLedger correlation are available. The BFF transformer in
// handlers_oplus.go derives the FE-canonical `workflow_id`, `agent_slug`,
// `decision_type`, `hitl_status`, `model`, `confidence`, `cost` from these.
type AgentDecisionRow struct {
	LogID    string `json:"log_id"`
	TenantID string `json:"tenant_id"`
	GCID     string `json:"gcid,omitempty"`
	AGID     string `json:"agid,omitempty"`
	CrewName string `json:"crew_name,omitempty"`
	AgentID  string `json:"agent_id"`
	Decision string `json:"decision,omitempty"`
	Reason   string `json:"reason,omitempty"`
	// Reasoning is chora-observability's bounded reasoning record. Its
	// reasoning_summary carries the agent's OWN rationale (= qgen critic_notes)
	// — the IMDA D2 "why" the O+ Decision-Traces row-click reasoning panel
	// renders. input_hash/output_hash are the PII-safe citation (full content
	// is never carried). nil when the decision row has no reasoning.
	Reasoning *AgentDecisionReasoning `json:"reasoning,omitempty"`
	TraceID   string                  `json:"trace_id,omitempty"`
	SpanID    string                  `json:"span_id,omitempty"`
	// Traceparent is the W3C traceparent (`<ver>-<trace_id>-<span_id>-<flags>`)
	// emitted by chora-observability — its agent_decision projection serializes
	// `traceparent`, NOT a bare `trace_id`. enrichCloudTraceURLs parses the
	// 32-hex trace-id out of it so the Cloud Trace deep-link + FE `trace_id`
	// are populated for the now-flowing qgen/critic agent telemetry.
	Traceparent string    `json:"traceparent,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	// CloudTraceURL is computed BFF-side: a deep-link to the Cloud Trace UI
	// for this trace_id. Per anchoring-decision #2.
	CloudTraceURL string `json:"cloud_trace_url,omitempty"`

	// --- Phase-D enrichment (audit finding #1) ----------------------------
	// CorrelationID is the workflow correlation id from the event envelope.
	// Maps to FE `workflow_id`. May be empty when the projector has not yet
	// joined the envelope — BFF emits the raw log_id as a fallback so the FE
	// always has a non-empty grouping key.
	CorrelationID string `json:"correlation_id,omitempty"`
	// DecisionKind is the canonical decision type emitted by the enriched
	// projector (e.g. `qna_generate`, `critique`, `gate_check`). Maps to FE
	// `decision_type`. Part of the CHO-1560 +9-field projection.
	DecisionKind string `json:"decision_kind,omitempty"`
	// DecisionType is the decision type the chora-observability agent_decision
	// projection serializes TODAY (`decision_type`, e.g. `respond`). Preferred
	// fallback below DecisionKind so the O+ Decision Traces "type" column is
	// populated from the current-shape telemetry rather than left blank.
	DecisionType string `json:"decision_type,omitempty"`
	// Verdict is the qgen quality-gate outcome (accepted | rejected |
	// completed_with_warning | refused | retry) chora-observability now
	// serializes (`verdict`) from the proto attributes["decision"]. It is the
	// human-meaningful "what did the agent decide" — distinct from the 4-value
	// decision_type ENUM — and mapAgentDecision PREFERS it for the O+ DECISION
	// TYPE column + reasoning-panel step (CHO-1700 follow-up). Empty for
	// non-verdict decisions → the column falls back to decision_kind/type.
	Verdict string `json:"verdict,omitempty"`
	// ModelID is the canonical model name resolved by the projector (e.g.
	// `gemini-2.5-pro`). Maps to FE `model`.
	ModelID string `json:"model_id,omitempty"`
	// Confidence is the agent's self-reported confidence in [0..1]. Maps to
	// FE `confidence`. Zero is a legitimate value; null-distinction is
	// preserved via a pointer.
	Confidence *float64 `json:"confidence,omitempty"`
	// CostUSD is the per-decision cost rolled up from the TokenUsageLedger.
	// Maps to FE `cost` after USD-formatting.
	CostUSD *float64 `json:"cost_usd,omitempty"`
	// AutonomyLevel is the autonomy-level tag the projector attaches when
	// the agent emitted at HOOTL / HOTL / HITL-L0..L2. Used (with ActedUpon)
	// to derive FE `hitl_status`.
	AutonomyLevel string `json:"autonomy_level,omitempty"`
	// ActedUpon is the projector's flag for HITL gate completion. Combined
	// with AutonomyLevel to derive FE `hitl_status`.
	ActedUpon string `json:"acted_upon,omitempty"`
	// PromptConditions is the durable prompt-explainability map<string,string>
	// (ADR-197 M-A.5) chora-observability now serializes on the decision row.
	// It captures the conditions/variables bound when the agent's prompt was
	// composed (the "why this prompt" record). The BFF passes it straight
	// through to the O+ governance decision view — no transformation.
	PromptConditions map[string]string `json:"prompt_conditions,omitempty"`
}

// AgentDecisionReasoning mirrors chora-observability's bounded ReasoningSummary
// record (json `reasoning`). Summary is the agent's rationale (= qgen
// critic_notes); the hashes are the PII-safe citation (raw input/output is
// never carried — per the §9 PII discipline).
type AgentDecisionReasoning struct {
	Summary    string `json:"reasoning_summary"`
	InputHash  string `json:"input_hash,omitempty"`
	OutputHash string `json:"output_hash,omitempty"`
}

// AgentDecisionsResponse is the paginated response envelope.
type AgentDecisionsResponse struct {
	Items      []AgentDecisionRow `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
	Total      int                `json:"total,omitempty"`
}

// CorrelationResponse is the per-decision trace correlation (single-row
// drilldown for the Decision Traces tab).
type CorrelationResponse struct {
	CorrelationID string         `json:"correlation_id"`
	TraceID       string         `json:"trace_id,omitempty"`
	Spans         []any          `json:"spans,omitempty"`
	Meta          map[string]any `json:"meta,omitempty"`
}

// AgentDecisionCountResponse is the response envelope returned by the
// chora-observability `/api/v1/observability/agent-decisions/count` endpoint.
// Mirrored from the handler shape so the BFF doesn't need to re-marshal.
type AgentDecisionCountResponse struct {
	Count int64  `json:"count"`
	Since string `json:"since"`
	Until string `json:"until"`
}

// TokenUsageAggregateGroup mirrors chora-observability
// `ledger.AggregateGroup` (the row shape inside the `{groups:[...]}`
// envelope returned by `GET /api/token-usage/aggregate?group_by=...`).
// Cost is int64 MICROS (1e-6 USD) — the BFF converts to float USD by
// dividing by 1_000_000. Field names match the upstream JSON tags exactly.
type TokenUsageAggregateGroup struct {
	Group              string `json:"group"`
	TotalCostUsdMicros int64  `json:"total_cost_usd_micros"`
	PromptTokens       int    `json:"prompt_tokens"`
	CompletionTokens   int    `json:"completion_tokens"`
	EntryCount         int    `json:"entry_count"`
}

// TokenUsageAggregateResponse is the `{groups:[...]}` envelope from
// `GET /api/token-usage/aggregate`.
type TokenUsageAggregateResponse struct {
	Groups []TokenUsageAggregateGroup `json:"groups"`
}

// CostCumulativeResponse mirrors chora-observability `cost.CumulativeResult`
// — only the canonical-total fields the BFF cost rollup needs. The full
// upstream payload also carries `ticks[]`, which the BFF does not consume.
// `total_cost_usd_micros` is int64 MICROS → divide by 1_000_000 for float USD.
type CostCumulativeResponse struct {
	TotalCostUsdMicros int64  `json:"total_cost_usd_micros"`
	TotalCostUsd       string `json:"total_cost_usd"`
}

// ObservabilityClient is the real-wire HTTP client. Construct via
// NewObservabilityClient.
type ObservabilityClient struct {
	cfg    ObservabilityConfig
	http   *http.Client
	tracer trace.Tracer
}

// NewObservabilityClient constructs the client. HTTP client defaults to one
// with the configured PerCallTimeout when nil.
func NewObservabilityClient(cfg ObservabilityConfig, httpClient *http.Client) *ObservabilityClient {
	if cfg.PerCallTimeout <= 0 {
		cfg.PerCallTimeout = DefaultObservabilityCallTimeout
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: cfg.PerCallTimeout}
	}
	return &ObservabilityClient{
		cfg:    cfg,
		http:   httpClient,
		tracer: otel.Tracer("chora-gateway/upstream/observability"),
	}
}

// GetAgents fetches the crews + agents hierarchy. tenantID is forwarded so
// the downstream handler can scope counts per-tenant (or leave global if
// empty).
func (o *ObservabilityClient) GetAgents(ctx context.Context, tenantID string) (AgentsResponse, error) {
	if o == nil || o.cfg.HTTPAddr == "" {
		return AgentsResponse{}, fmt.Errorf("%w: observability: HTTP addr not wired", ErrUpstream)
	}
	q := url.Values{}
	if tenantID != "" {
		q.Set("tenant_id", tenantID)
	}
	u := strings.TrimRight(o.cfg.HTTPAddr, "/") + "/api/v1/observability/agents"
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}

	body, err := o.httpGetJSON(ctx, "upstream.observability.GetAgents", u, tenantID)
	if err != nil {
		return AgentsResponse{}, err
	}
	if len(body) == 0 {
		return AgentsResponse{Crews: []Crew{}}, nil
	}
	var out AgentsResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return AgentsResponse{}, fmt.Errorf("%w: GetAgents: malformed JSON: %v", ErrUpstream, err)
	}
	if out.Crews == nil {
		out.Crews = []Crew{}
	}
	return out, nil
}

// GetAgentPrompts fetches the CHO-2364 per-agent prompt-evidence aggregation
// (ADR-197 read slice). tenantID is forwarded so the downstream handler
// scopes the aggregation per-tenant. Mirrors GetAgents; the agents payload
// stays raw JSON for a lossless pass-through.
func (o *ObservabilityClient) GetAgentPrompts(ctx context.Context, tenantID string) (AgentPromptsResponse, error) {
	if o == nil || o.cfg.HTTPAddr == "" {
		return AgentPromptsResponse{}, fmt.Errorf("%w: observability: HTTP addr not wired", ErrUpstream)
	}
	q := url.Values{}
	if tenantID != "" {
		q.Set("tenant_id", tenantID)
	}
	u := strings.TrimRight(o.cfg.HTTPAddr, "/") + "/api/v1/observability/agent-prompts"
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}

	body, err := o.httpGetJSON(ctx, "upstream.observability.GetAgentPrompts", u, tenantID)
	if err != nil {
		return AgentPromptsResponse{}, err
	}
	if len(body) == 0 {
		return AgentPromptsResponse{Agents: json.RawMessage("[]")}, nil
	}
	var out AgentPromptsResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return AgentPromptsResponse{}, fmt.Errorf("%w: GetAgentPrompts: malformed JSON: %v", ErrUpstream, err)
	}
	if len(out.Agents) == 0 {
		out.Agents = json.RawMessage("[]")
	}
	return out, nil
}

// GetAgentDecisions fetches the paginated decision list. `since` is RFC3339
// (or empty for "last 24h"); `limit` defaults to 50; `cursor` for follow-up
// pages.
func (o *ObservabilityClient) GetAgentDecisions(ctx context.Context, tenantID, since, cursor string, limit int) (AgentDecisionsResponse, error) {
	if o == nil || o.cfg.HTTPAddr == "" {
		return AgentDecisionsResponse{}, fmt.Errorf("%w: observability: HTTP addr not wired", ErrUpstream)
	}
	if limit <= 0 {
		limit = 50
	}
	q := url.Values{}
	if tenantID != "" {
		q.Set("tenant_id", tenantID)
	}
	if since != "" {
		q.Set("since", since)
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	q.Set("limit", fmt.Sprintf("%d", limit))
	u := strings.TrimRight(o.cfg.HTTPAddr, "/") + "/api/agent-decisions?" + q.Encode()

	body, err := o.httpGetJSON(ctx, "upstream.observability.GetAgentDecisions", u, tenantID)
	if err != nil {
		return AgentDecisionsResponse{}, err
	}
	if len(body) == 0 {
		return AgentDecisionsResponse{Items: []AgentDecisionRow{}}, nil
	}
	var out AgentDecisionsResponse
	if err := json.Unmarshal(body, &out); err == nil && out.Items != nil {
		enrichCloudTraceURLs(out.Items)
		return out, nil
	}
	// fall back to bare array
	var rows []AgentDecisionRow
	if err := json.Unmarshal(body, &rows); err != nil {
		return AgentDecisionsResponse{}, fmt.Errorf("%w: GetAgentDecisions: malformed JSON: %v", ErrUpstream, err)
	}
	enrichCloudTraceURLs(rows)
	return AgentDecisionsResponse{Items: rows}, nil
}

// CountAgentDecisions fetches the count of agent_decision_log rows for the
// tenant in the half-open window [since, until). Backs the O+ Dashboard
// `recent_decisions_24h` rollup.
//
// Zero-value since/until are forwarded as empty query params — the downstream
// handler defaults to (now-24h, now). Per [[feedback-no-stubs-real-wiring]]
// errors are surfaced loudly via ErrUpstream wrap; callers (the BFF Dashboard
// handler) degrade gracefully by omitting the recent_decisions_24h field
// rather than failing the whole dashboard envelope.
func (o *ObservabilityClient) CountAgentDecisions(ctx context.Context, tenantID string, since, until time.Time) (AgentDecisionCountResponse, error) {
	if o == nil || o.cfg.HTTPAddr == "" {
		return AgentDecisionCountResponse{}, fmt.Errorf("%w: observability: HTTP addr not wired", ErrUpstream)
	}
	q := url.Values{}
	if tenantID != "" {
		q.Set("tenant_id", tenantID)
	}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	if !until.IsZero() {
		q.Set("until", until.UTC().Format(time.RFC3339))
	}
	u := strings.TrimRight(o.cfg.HTTPAddr, "/") + "/api/v1/observability/agent-decisions/count"
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	body, err := o.httpGetJSON(ctx, "upstream.observability.CountAgentDecisions", u, tenantID)
	if err != nil {
		return AgentDecisionCountResponse{}, err
	}
	if len(body) == 0 {
		return AgentDecisionCountResponse{}, nil
	}
	var out AgentDecisionCountResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return AgentDecisionCountResponse{}, fmt.Errorf("%w: CountAgentDecisions: malformed JSON: %v", ErrUpstream, err)
	}
	return out, nil
}

// GetCorrelation fetches a single TraceCorrelation row for the Decision
// Traces tab drilldown.
func (o *ObservabilityClient) GetCorrelation(ctx context.Context, tenantID, id string) (*CorrelationResponse, error) {
	if o == nil || o.cfg.HTTPAddr == "" {
		return nil, fmt.Errorf("%w: observability: HTTP addr not wired", ErrUpstream)
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("%w: correlation id required", ErrUpstream)
	}
	u := strings.TrimRight(o.cfg.HTTPAddr, "/") + "/api/correlations/" + url.PathEscape(id)

	body, err := o.httpGetJSON(ctx, "upstream.observability.GetCorrelation", u, tenantID)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, nil
	}
	var out CorrelationResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("%w: GetCorrelation: malformed JSON: %v", ErrUpstream, err)
	}
	return &out, nil
}

// AggregateTokenUsage fetches the per-group token + cost rollup from
// `GET /api/token-usage/aggregate?group_by={groupBy}`. groupBy is one of
// `model` | `agent` | `gcid` (unknown falls back to `model` upstream).
// Backs the O+ `/bff/oplus/costs` `by_model` + `by_agent` arrays — the BFF
// calls this once per group dimension.
func (o *ObservabilityClient) AggregateTokenUsage(ctx context.Context, tenantID, groupBy string, from, to time.Time) (TokenUsageAggregateResponse, error) {
	if o == nil || o.cfg.HTTPAddr == "" {
		return TokenUsageAggregateResponse{}, fmt.Errorf("%w: observability: HTTP addr not wired", ErrUpstream)
	}
	q := url.Values{}
	if tenantID != "" {
		q.Set("tenant_id", tenantID)
	}
	if groupBy != "" {
		q.Set("group_by", groupBy)
	}
	if !from.IsZero() {
		q.Set("from", from.UTC().Format(time.RFC3339))
	}
	if !to.IsZero() {
		q.Set("to", to.UTC().Format(time.RFC3339))
	}
	u := strings.TrimRight(o.cfg.HTTPAddr, "/") + "/api/token-usage/aggregate"
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	body, err := o.httpGetJSON(ctx, "upstream.observability.AggregateTokenUsage", u, tenantID)
	if err != nil {
		return TokenUsageAggregateResponse{}, err
	}
	if len(body) == 0 {
		return TokenUsageAggregateResponse{Groups: []TokenUsageAggregateGroup{}}, nil
	}
	var out TokenUsageAggregateResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return TokenUsageAggregateResponse{}, fmt.Errorf("%w: AggregateTokenUsage: malformed JSON: %v", ErrUpstream, err)
	}
	if out.Groups == nil {
		out.Groups = []TokenUsageAggregateGroup{}
	}
	return out, nil
}

// GetCostCumulative fetches the cumulative cost total from
// `GET /api/cost/cumulative`. Backs the O+ `/bff/oplus/costs`
// `cumulative_cost_usd` field — the BFF divides `total_cost_usd_micros`
// by 1_000_000.
func (o *ObservabilityClient) GetCostCumulative(ctx context.Context, tenantID string, from, to time.Time) (CostCumulativeResponse, error) {
	if o == nil || o.cfg.HTTPAddr == "" {
		return CostCumulativeResponse{}, fmt.Errorf("%w: observability: HTTP addr not wired", ErrUpstream)
	}
	q := url.Values{}
	if tenantID != "" {
		q.Set("tenant_id", tenantID)
	}
	if !from.IsZero() {
		q.Set("from", from.UTC().Format(time.RFC3339))
	}
	if !to.IsZero() {
		q.Set("to", to.UTC().Format(time.RFC3339))
	}
	u := strings.TrimRight(o.cfg.HTTPAddr, "/") + "/api/cost/cumulative"
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	body, err := o.httpGetJSON(ctx, "upstream.observability.GetCostCumulative", u, tenantID)
	if err != nil {
		return CostCumulativeResponse{}, err
	}
	if len(body) == 0 {
		return CostCumulativeResponse{}, nil
	}
	var out CostCumulativeResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return CostCumulativeResponse{}, fmt.Errorf("%w: GetCostCumulative: malformed JSON: %v", ErrUpstream, err)
	}
	return out, nil
}

// httpGetJSON mirrors the governance_client equivalent.
func (o *ObservabilityClient) httpGetJSON(ctx context.Context, spanName, urlStr, tenantID string) ([]byte, error) {
	ctx, span := o.tracer.Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("http.request.method", http.MethodGet),
			attribute.String("server.address", urlHost(urlStr)),
			attribute.String("chora.tenant_id", tenantID),
		),
	)
	defer span.End()

	callCtx, cancel := context.WithTimeout(ctx, o.cfg.PerCallTimeout)
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
	if auth.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+auth.Bearer)
	}
	// Canonical mesh metadata — chora-gcid / chora-tenant-id / x-mesh-user-roles /
	// chora-role-summary. Until 2026-07-14 this client stamped NONE of them, so it
	// could not reach a single role-gated observability endpoint: obs's role gate
	// reads x-mesh-user-roles and FAILS CLOSED, so an absent header denies 100% of
	// calls. That is why the O+ egress kill-switch had to hand-roll its own proxy
	// rather than route through here (CHO-2148 — flagged latent, fixed here).
	//
	// Tenant comes from the caller's argument (it is the query-scoped tenant, which
	// for a platform-global route may differ from auth.TenantID); GCID + Roles come
	// from the VALIDATED session, never a client-supplied header.
	mesh := servicemesh.MarshalToHeaders(servicemesh.MeshClaims{
		GCID:        auth.GCID,
		TenantID:    tenantID,
		Roles:       auth.Roles,
		RoleSummary: auth.RoleSummary,
	})
	for k, vs := range mesh {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	resp, err := o.http.Do(req)
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

// enrichCloudTraceURLs computes the trace deep-link for each row. The
// trace-id AND span-id are taken from explicit fields when present, otherwise
// parsed out of the W3C `traceparent` chora-observability emits. Cloud Trace
// specifics were removed with the cloud decoupling: the link now targets the
// self-hosted tracing UI configured via CHORA_TRACE_UI_URL, and is omitted
// when that is unset.
func enrichCloudTraceURLs(rows []AgentDecisionRow) {
	for i := range rows {
		// chora-observability serializes `traceparent`, not bare trace_id/span_id.
		// Derive both from it when the explicit fields are absent so the deep-link
		// pins the agent's own span + the FE-facing ids populate.
		if rows[i].TraceID == "" {
			rows[i].TraceID = traceIDFromTraceparent(rows[i].Traceparent)
		}
		if rows[i].SpanID == "" {
			rows[i].SpanID = spanIDFromTraceparent(rows[i].Traceparent)
		}
		if rows[i].TraceID == "" || rows[i].CloudTraceURL != "" {
			continue
		}
		rows[i].CloudTraceURL = cloudTraceTraceURL(rows[i].TraceID, rows[i].SpanID)
	}
}

// cloudTraceTraceURL deep-links to a SPECIFIC trace in the self-hosted tracing
// UI. The base URL comes from CHORA_TRACE_UI_URL (e.g. a Grafana/Tempo or
// Jaeger base); an unset base disables the link. Returns "" when base or
// traceID is empty.
func cloudTraceTraceURL(traceID, spanID string) string {
	base := strings.TrimSpace(os.Getenv("CHORA_TRACE_UI_URL"))
	if base == "" || traceID == "" {
		return ""
	}
	base = strings.TrimRight(base, "/")
	if spanID != "" {
		return fmt.Sprintf("%s/trace/%s?span=%s", base, url.PathEscape(traceID), url.PathEscape(spanID))
	}
	return fmt.Sprintf("%s/trace/%s", base, url.PathEscape(traceID))
}

// traceIDFromTraceparent extracts the 32-hex trace-id from a W3C traceparent
// (`<version>-<trace_id>-<span_id>-<flags>`). Returns "" when the header is
// absent, malformed, or carries the all-zero (invalid) trace-id.
func traceIDFromTraceparent(tp string) string {
	tp = strings.TrimSpace(tp)
	if tp == "" {
		return ""
	}
	parts := strings.Split(tp, "-")
	if len(parts) < 4 {
		return ""
	}
	traceID := parts[1]
	if len(traceID) != 32 {
		return ""
	}
	for _, c := range traceID {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	if traceID == "00000000000000000000000000000000" {
		return ""
	}
	return traceID
}

// spanIDFromTraceparent extracts the 16-hex span-id (parts[2]) from a W3C
// traceparent (`<version>-<trace_id>-<span_id>-<flags>`). Returns "" when the
// header is absent, malformed, or carries the all-zero (invalid) span-id. The
// span-id is the orchestrator's per-agent marker span (agent.<agid>) so pinning
// ;spanId= lands the deep-link on THAT agent's own span (§9).
func spanIDFromTraceparent(tp string) string {
	tp = strings.TrimSpace(tp)
	if tp == "" {
		return ""
	}
	parts := strings.Split(tp, "-")
	if len(parts) < 4 {
		return ""
	}
	spanID := parts[2]
	if len(spanID) != 16 {
		return ""
	}
	for _, c := range spanID {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	if spanID == "0000000000000000" {
		return ""
	}
	return spanID
}
