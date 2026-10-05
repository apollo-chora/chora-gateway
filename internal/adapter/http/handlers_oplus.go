// handlers_oplus.go — Phase C BFF wiring for the O+ Observability+ surface.
//
// Per the O+ hydration plan (`/Users/daleleung/.claude/plans/atomic-napping-
// spring.md` §"Phase C — BFF wiring"), the O+ surface needs 5 read BFF
// routes + (N13, 2026-05-26) 2 HITL self-claim/release write routes:
//
//	GET  /bff/oplus/dashboard                     — IMDA D1-D4 scores + posture
//	GET  /bff/oplus/dimensions                    — Full per-dimension rubric expand
//	GET  /bff/oplus/agents                        — Crews + Agents hierarchy
//	GET  /bff/oplus/prompts                       - Per-agent prompt evidence (CHO-2364, ADR-197)
//	GET  /bff/oplus/governance                    — Decision Traces + HITL queue + Data Governance (3 tabs)
//	GET  /bff/oplus/a2a                           — A2A view, OR {mode:'pending', mock:{...}}
//	GET  /bff/oplus/costs                         — Aggregated AI cost rollup (by_model + by_agent + cumulative)
//	POST /bff/oplus/governance/hitl/{id}/claim    — HITL self-claim passthrough (N13)
//	POST /bff/oplus/governance/hitl/{id}/release  — HITL release passthrough  (N13)
//	POST /bff/oplus/governance/hitl/{id}/approve  — HITL approve verdict passthrough
//	POST /bff/oplus/governance/hitl/{id}/reject   — HITL reject verdict passthrough
//
// Before Phase C, only `/bff/oplus/governance` existed and it routed
// through FakeUpstream. This file adds the missing 4 routes + rewires the
// existing governance handler to compose REAL governance + observability
// data. The 2 N13 POST handlers live in handlers_oplus_hitl.go.
//
// Architecture anchors:
//   - ADR-141 IMDA dimension labels canonical (accountability /
//     transparency / safety_and_robustness / fairness_and_human_oversight)
//   - ADR-140 gRPC inter-service (governance gRPC client lives in
//     upstream/governance_client.go)
//   - [[imda-governance-4-dimensions]] selective deep-link policy (Cloud
//     Trace UI for trace_id rows; Agent Engine UI for engine_id rows)
//   - [[secrets-and-env]] all endpoints sourced from env
//   - [[feedback-no-stubs-real-wiring]] no in-process stubs; A2A pending
//     mode is an explicit "backend not yet deployed" signal, not a stub
//
// Auth: `/bff/oplus/*` is auditor-role-gated by
// `internal/middleware/auditor_gate.go` (mounted by main.go AFTER the
// chora-session JWT validation chain).
package httpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

// OPlusHandler owns the O+ routes (5 reads + 2 HITL writes since N13 on
// 2026-05-26). Wired into the mux via `RegisterOPlusRoutes`. Each client
// field is nil-safe — for read routes an unwired client surfaces a
// `state:'error'` envelope at HTTP 200; for HITL write routes an unwired
// Governance client surfaces HTTP 503 directly (writes do NOT collapse to
// the read envelope). The A2A client has a special `Pending()` mode that
// maps to the `{mode:'pending'}` envelope on `/bff/oplus/a2a`.
type OPlusHandler struct {
	Governance    *upstream.GovernanceClient
	Observability *upstream.ObservabilityClient
	A2A           *upstream.A2AClient
	// AIKernel serves the CHO-2368 prompt catalogue subtree
	// (/bff/oplus/prompts/{agent_id}/versions[/{version}]). Optional - nil
	// collapses those routes to the state:error envelope (degraded, never
	// fatal), mirroring the weakness-resume loader posture.
	AIKernel        *upstream.AIKernelClient
	DataLineageYAML []byte // optional static data lineage / RACI config
	Now             func() time.Time
	// DisplayCurrency + FxRate drive the O+ cost panel's presentation currency.
	// The rate is a STATIC configured value (not live FX). The ledger stays
	// USD-canonical — these are surfaced as metadata and the FE multiplies for
	// display. Defaults: "USD" @ 1.0 (identity, no conversion).
	DisplayCurrency string
	FxRate          float64
}

// NewOPlusHandler constructs the handler. `now` is overridable for
// deterministic tests; defaults to `time.Now().UTC`.
func NewOPlusHandler(gov *upstream.GovernanceClient, obs *upstream.ObservabilityClient, a2a *upstream.A2AClient, dataLineageYAML []byte) *OPlusHandler {
	return &OPlusHandler{
		Governance:      gov,
		Observability:   obs,
		A2A:             a2a,
		DataLineageYAML: dataLineageYAML,
		Now:             func() time.Time { return time.Now().UTC() },
		DisplayCurrency: "USD",
		FxRate:          1.0,
	}
}

// RegisterOPlusRoutes wires the 5 read routes + 2 HITL write routes (N13)
// onto mux. The existing single-handler `/bff/oplus/governance` registered
// in handler.go's NewRouterWithGraphQL is REPLACED — RegisterOPlusRoutes
// registers it again here so callers that opt into the new Phase-C wiring
// get the real-wire composition. Callers using the legacy NewRouter /
// NewRouterWithGraphQL keep the FakeUpstream-backed governance handler.
func RegisterOPlusRoutes(mux *http.ServeMux, h *OPlusHandler) {
	mux.HandleFunc("/bff/oplus/dashboard", h.Dashboard)
	mux.HandleFunc("/bff/oplus/dimensions", h.Dimensions)
	mux.HandleFunc("/bff/oplus/agents", h.Agents)
	// CHO-2364 (ADR-197 read slice) - per-agent prompt-evidence pass-through.
	// Inherits the /bff/oplus/ prefix auditor gate like every sibling route.
	mux.HandleFunc("/bff/oplus/prompts", h.AgentPrompts)
	// CHO-2368 (ADR-197 M-D read slice) - prompt CONTENT catalogue subtree:
	// /bff/oplus/prompts/{agent_id}/versions[/{version}] via the ai-kernel
	// orchestrator registry. The exact path above still routes the evidence
	// panel; ServeMux prefers the exact match.
	mux.HandleFunc("/bff/oplus/prompts/", h.PromptCatalogue)
	mux.HandleFunc("/bff/oplus/governance", h.Governance3Tab)
	// O+ external-egress audit trail (CHO-2245, ADR-231). Exact path — more
	// specific than the /bff/oplus/governance exact entry and the
	// /bff/oplus/governance/hitl/ subtree, so it routes unambiguously.
	mux.HandleFunc("/bff/oplus/governance/egress-audit", h.EgressAudit)
	mux.HandleFunc("/bff/oplus/a2a", h.A2APanel)
	mux.HandleFunc("/bff/oplus/costs", h.Costs)
	// O+ Agent-Eval evidence drill-down (IMDA D2) — crew-run index (exact path)
	// + per-row drill-down (subtree). Reshaped from the chora-observability
	// agent_eval_evidence view. See handlers_oplus_eval.go.
	mux.HandleFunc("/bff/oplus/eval-runs", h.EvalRuns)
	mux.HandleFunc("/bff/oplus/eval-runs/", h.EvalRunEvidence)
	// N13 — HITL self-claim + release passthrough. Prefix handler dispatches
	// {claim|release} sub-actions via parseHITLActionPath. POST-only; non-POST
	// returns 405. See handlers_oplus_hitl.go for the status mapping.
	mux.HandleFunc("/bff/oplus/governance/hitl/", h.hitlAction)
}

// NewOPlusMux returns a mux that serves the 5 Phase-C /bff/oplus/* routes.
// Combine with WithOPlusRoutes to compose with a base handler that owns the
// other surfaces. Mirrors the NewNotificationsMux / NewKGExploreMux shape.
func NewOPlusMux(h *OPlusHandler) http.Handler {
	mux := http.NewServeMux()
	RegisterOPlusRoutes(mux, h)
	return mux
}

// WithOPlusRoutes composes the 5 Phase-C /bff/oplus/* routes onto a base
// handler — paths matching the prefix are served by the OPlus mux, everything
// else falls through to base. Passes through unchanged when h is nil so
// cmd/server/main.go can opt out in unconfigured envs.
//
// Mount BEFORE wrapping with WithChoraSessionOnPrefixes / AuditorGate so the
// outer JWT validator + role gate see the OPlus paths and authenticate them
// first.
func WithOPlusRoutes(base http.Handler, h *OPlusHandler) http.Handler {
	if h == nil {
		return base
	}
	oplusMux := NewOPlusMux(h)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/bff/oplus/") {
			oplusMux.ServeHTTP(w, r)
			return
		}
		base.ServeHTTP(w, r)
	})
}

// -----------------------------------------------------------------------------
// Response envelopes
// -----------------------------------------------------------------------------

// OPlusEnvelope is the discriminated-union state wrapper the FE consumes
// per anchoring-decision #4 (Polling 30s via Angular signal — each method
// handles loading/error states via discriminated unions
// `{state:'loading'} | {state:'live',data} | {state:'stale',data,since} |
// {state:'error',error}`).
type OPlusEnvelope struct {
	State     string    `json:"state"` // live | stale | error | pending
	FetchedAt time.Time `json:"fetched_at"`
	Error     string    `json:"error,omitempty"`
}

// PostureItemView is the per-dimension posture row the FE renders on the
// dashboard. Per audit finding #3: `posture: [{num, label, score, status}]`.
// `num` is the canonical D1-D4 index (1..4), `label` is the ADR-141 ID,
// `status` is derived from `score` via deriveDimensionStatus thresholds.
type PostureItemView struct {
	Num    int    `json:"num"`    // 1..4 per canonical IMDA order
	Label  string `json:"label"`  // accountability | transparency | safety_and_robustness | fairness_and_human_oversight
	Score  int32  `json:"score"`  // 0..100 — rename of upstream ScorePct
	Status string `json:"status"` // achieved | partial | attention | pending
}

// DashboardResponse is the /bff/oplus/dashboard body shape. The 6
// ISO 25059:2023 safety-risk tiles are a FE-canonical design-time
// invariant (`SAFETY_RISK_CATEGORIES` in chora-web/.../imda-dimensions.ts);
// the BFF no longer ships them because the FE renders from its local
// constant. Removing the dead `safety_risks` field clears the
// schema-reconciliation TODO(M12) marker without adding a downstream
// projection no consumer would read.
type DashboardResponse struct {
	OPlusEnvelope
	Posture             []PostureItemView `json:"posture"`
	AllBaselineAchieved bool              `json:"all_baseline_achieved"`
	RecentDecisions24h  *int              `json:"recent_decisions_24h,omitempty"`
}

// RubricItemView is the per-rubric-item row the FE renders inside a
// dimension panel. Per audit finding #5: `ref`, `requirement`, `tools[]`,
// `tool_coverage`, `evidence_source_url`.
type RubricItemView struct {
	Ref               string   `json:"ref"`
	Requirement       string   `json:"requirement"`
	ToolCoverage      string   `json:"tool_coverage"`
	Tools             []string `json:"tools"`
	Status            string   `json:"status"` // lowercase: pass | partial | fail
	EvidenceSourceURL *string  `json:"evidence_source_url"`
	Priority          string   `json:"priority"` // P1 | P2 | P3 (empty = unscored)
}

// DimensionRubricView is the per-dimension panel the FE renders on the
// /dimensions surface. Per audit finding #5 + FE DimensionsData type: each
// dimension carries `num`, `label`, `score`, `status`, `rubric_items[]`.
type DimensionRubricView struct {
	Num         int              `json:"num"`
	Label       string           `json:"label"`
	Score       int32            `json:"score"`
	Status      string           `json:"status"`
	Description string           `json:"description,omitempty"`
	RubricItems []RubricItemView `json:"rubric_items"`
}

// DimensionsResponse is the /bff/oplus/dimensions body shape.
type DimensionsResponse struct {
	OPlusEnvelope
	Dimensions []DimensionRubricView `json:"dimensions"`
}

// AgentsBodyResponse is the /bff/oplus/agents body shape.
type AgentsBodyResponse struct {
	OPlusEnvelope
	Crews []upstream.Crew `json:"crews"`
}

// AgentPromptsBodyResponse is the /bff/oplus/prompts body shape (CHO-2364,
// ADR-197 read slice). Agents stays raw JSON so the five-agent
// prompt-evidence array from chora-observability passes through byte-exact;
// a typed re-marshal here would silently drop upstream fields the BFF does
// not know about yet (the Crew.has_recent_activity lesson).
type AgentPromptsBodyResponse struct {
	OPlusEnvelope
	Agents json.RawMessage `json:"agents"`
}

// GovernanceDecisionView is the per-decision row the FE renders in the
// Governance > Decision Traces tab. Per audit finding #1.
type GovernanceDecisionView struct {
	ID         string `json:"id"`
	WorkflowID string `json:"workflow_id"`
	Agent      string `json:"agent"`
	AgentSlug  string `json:"agent_slug"`
	// DecisionType carries the agent's quality-gate VERDICT (accepted |
	// rejected | refused | completed_with_warning) — the human-meaningful
	// "what did the agent decide" the FE "Decision" column renders.
	DecisionType  string  `json:"decision_type"`
	Model         string  `json:"model"`
	Cost          string  `json:"cost"` // pre-formatted USD string (e.g. "$0.0021")
	Timestamp     string  `json:"timestamp"`
	TraceID       *string `json:"trace_id"`
	CloudTraceURL *string `json:"cloud_trace_url,omitempty"`
	// ReasoningSummary is the agent's OWN rationale for this decision (= qgen
	// critic_notes) — the IMDA D2 "why" the FE Decision-Traces row-click
	// reasoning panel renders. Empty when the decision row carried no reasoning.
	ReasoningSummary string `json:"reasoning_summary,omitempty"`
	// InputHash / OutputHash are the PII-safe citation of the reviewed content
	// (sha256 hex) — the panel shows these, never raw input/output.
	InputHash  string `json:"input_hash,omitempty"`
	OutputHash string `json:"output_hash,omitempty"`
	// PromptConditions is the durable prompt-explainability map<string,string>
	// (ADR-197 M-A.5) — the conditions/variables bound when the agent's prompt
	// was composed. Passed straight through from the chora-observability
	// decision row; the O+ reasoning panel renders it as the "why this prompt"
	// record. Empty/omitted when the upstream row carried no conditions.
	PromptConditions map[string]string `json:"prompt_conditions,omitempty"`
}

// GovernanceHitlView is the per-HITL-row the FE renders in the
// Governance > Oversight tab. Per audit finding #2.
//
// Assignee is `*string` (nullable JSON) — `null` = unassigned (default at
// creation, until a reviewer claims the gate via the deferred-to-wave-N+1
// POST /api/hitl/decisions/{id}/claim endpoint). Per
// [[feedback-no-stubs-real-wiring]] the BFF passes the chora-governance
// `assignee_gcid` through without synthetic placeholders; the FE renders
// `null` as the localised "unassigned" string.
type GovernanceHitlView struct {
	ID            string  `json:"id"`
	WorkflowID    string  `json:"workflow_id"`
	Gate          string  `json:"gate"` // question_review | report_review | material_decision
	Agent         string  `json:"agent"`
	Summary       string  `json:"summary"`
	AutonomyLevel string  `json:"autonomy_level"`
	WaitingSince  string  `json:"waiting_since"`
	Assignee      *string `json:"assignee"`
}

// GovernanceTabsResponse is the /bff/oplus/governance body shape — 3 tabs.
// Reshaped per audit findings #1 + #2 to the FE-canonical contract.
type GovernanceTabsResponse struct {
	OPlusEnvelope
	Decisions   []GovernanceDecisionView `json:"decisions"`
	HITLPending []GovernanceHitlView     `json:"hitl_pending"`
	DataLineage any                      `json:"data_lineage"`
	// WindowDays is the data-currency window (in days) the Decision Traces are
	// drawn from — surfaced so O+ can show auditors "last N days · as of <ts>".
	WindowDays int `json:"window_days"`
}

// Decision-Traces data currency. Auditors review the recent decision history,
// not just the last day; the window is the canonical currency level surfaced
// in O+ (the FE renders "last N days · as of <fetched_at>"). The limit bounds
// the table to the first 20 entries — the 20 most recent decisions
// (listDecisions returns newest-first) of that window.
const (
	governanceDecisionWindowDays = 90
	governanceDecisionLimit      = 20
)

// A2AContractView is the per-contract row the FE renders on the A2A console.
// Per audit finding #4: rename `contract_id → id`, populate `partner` display
// name, compute `scope[]`, `last_invocation`, `invocations_30d`.
type A2AContractView struct {
	ID             string   `json:"id"`
	Partner        string   `json:"partner"`
	AGID           string   `json:"agid"`
	Scope          []string `json:"scope"`
	Status         string   `json:"status"`
	LastInvocation *string  `json:"last_invocation"`
	Invocations30d int      `json:"invocations_30d"`
	CreatedAt      string   `json:"created_at"`
}

// A2AExternalAgentView is the per-external-agent row the FE renders on the
// A2A console. Per audit finding #4: rename `identities → external_agents`,
// add `trust_level`, `key_fingerprint`, `rotated_at`.
type A2AExternalAgentView struct {
	AGID           string `json:"agid"`
	Partner        string `json:"partner"`
	TrustLevel     string `json:"trust_level"`
	KeyFingerprint string `json:"key_fingerprint"`
	RotatedAt      string `json:"rotated_at"`
}

// A2AInvocationView is the per-invocation row the FE renders on the A2A
// console. Per audit finding #4: rename `invocation_id → id`,
// `outcome → status`, `started_at → timestamp`; compute `endpoint`,
// `latency_ms`.
type A2AInvocationView struct {
	ID         string `json:"id"`
	ContractID string `json:"contract_id"`
	AGID       string `json:"agid"`
	Partner    string `json:"partner"`
	Endpoint   string `json:"endpoint"`
	Status     string `json:"status"` // success | denied | error
	LatencyMS  int    `json:"latency_ms"`
	Timestamp  string `json:"timestamp"`
}

// A2AResponse is the /bff/oplus/a2a body shape, supporting two modes per
// Phase B6. Reshaped per audit finding #4 — `identities → external_agents`,
// per-field renames + computed view types.
type A2AResponse struct {
	OPlusEnvelope
	Mode           string                 `json:"mode"` // live | pending
	Contracts      []A2AContractView      `json:"contracts,omitempty"`
	ExternalAgents []A2AExternalAgentView `json:"external_agents,omitempty"`
	Invocations    []A2AInvocationView    `json:"invocations,omitempty"`
	Mock           map[string]any         `json:"mock,omitempty"`
}

// CostKeyRow is one row of the by-model / by-agent cost breakdown the FE
// renders on the O+ cost panel. `key` is the group key (model id or agent
// id); `cost_usd` is the float USD total derived from the upstream int64
// micros (`total_cost_usd_micros / 1_000_000`).
type CostKeyRow struct {
	Key              string  `json:"key"`
	CostUSD          float64 `json:"cost_usd"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
}

// CostsResponse is the /bff/oplus/costs body shape. Unlike the other O+
// reads it is NOT wrapped in the OPlusEnvelope discriminated union — the FE
// GovernanceService consumes the raw object directly and derives
// live/stale/error from the HTTP status. On success the BFF returns 200 with
// this object; on observability-upstream failure it returns 503 (mirroring
// the HITL write-path upstream-unavailable handling).
//
// `cumulative_cost_usd` is the float USD total from
// `GET /api/cost/cumulative` (`total_cost_usd_micros / 1_000_000`).
// `by_model` and `by_agent` are the per-group rollups from
// `GET /api/token-usage/aggregate?group_by={model,agent}`. `fetched_at` is
// stamped server-side.
type CostsResponse struct {
	FetchedAt         time.Time    `json:"fetched_at"`
	CumulativeCostUSD float64      `json:"cumulative_cost_usd"`
	ByModel           []CostKeyRow `json:"by_model"`
	ByAgent           []CostKeyRow `json:"by_agent"`
	// Currency + FxRate are presentation metadata. The `*_usd` amounts above
	// stay USD-canonical; the FE multiplies by FxRate and renders Currency.
	// Defaults "USD" @ 1.0 mean "show USD as-is".
	Currency string  `json:"currency"`
	FxRate   float64 `json:"fx_rate"`
}

// EgressAuditResponse is the /bff/oplus/governance/egress-audit body — the
// external-web egress audit slice for O+ (IMDA D2 transparency + D1
// accountability, CHO-2245 / ADR-231). Like the Costs read it is NOT wrapped in
// the discriminated-union envelope: it maps upstream status honestly (a
// governance DENY surfaces as 4xx, an unavailable upstream as 503), so the FE
// derives live/error from the HTTP status. Items is always a non-nil array.
type EgressAuditResponse struct {
	FetchedAt time.Time                   `json:"fetched_at"`
	Items     []upstream.EgressAuditEvent `json:"items"`
	Count     int                         `json:"count"`
}

// egressAuditLimit bounds the O+ egress-audit slice fetched from governance.
const egressAuditLimit = 100

// -----------------------------------------------------------------------------
// Handlers
// -----------------------------------------------------------------------------

// Dashboard returns the IMDA D1-D4 + posture summary. Composes
// governance.GetIMDADashboard + governance.GetDimensionRubric (parallel
// for each of D1-D4) so rubric pass/partial/fail counts can be derived
// per-dimension. Reshaped per audit finding #3 to the FE-canonical
// contract (`posture[]`, `all_baseline_achieved`, `recent_decisions_24h`).
// The 6 ISO 25059:2023 safety-risk tiles are FE-canonical design-time
// invariants and are no longer shipped by the BFF.
//
// `recent_decisions_24h` is sourced via observability.CountAgentDecisions
// against chora_observability.agent_decision_log. Per
// [[feedback-no-stubs-real-wiring]] — a real SQL COUNT(*); zero is honest;
// query failure degrades the field (omitted) without collapsing the rest of
// the dashboard envelope (state stays `live` because the governance + posture
// sources succeeded).
func (h *OPlusHandler) Dashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	ctx := withUpstreamAuthCtx(r)
	tenantID, _ := upstreamCtx(ctx)

	dimensions, err := h.Governance.GetIMDADashboard(ctx, tenantID)
	if err != nil {
		writeOPlusError(w, "GetIMDADashboard", err, h.now())
		return
	}

	// Reshape per audit finding #3 — canonical IMDA D1..D4 order with
	// derived `num`, `score`, `status` fields.
	posture := buildPostureView(dimensions)
	allAchieved := allBaselineAchieved(posture)

	resp := DashboardResponse{
		OPlusEnvelope:       OPlusEnvelope{State: "live", FetchedAt: h.now()},
		Posture:             posture,
		AllBaselineAchieved: allAchieved,
	}

	// recent_decisions_24h — real-wire pull from chora-observability. When
	// the observability client is unwired or the upstream call fails the
	// field is omitted (FE renders the field as absent) while the rest of
	// the dashboard stays live. Logs the upstream error so the degradation
	// is auditable without leaking into the FE envelope.
	if h.Observability != nil {
		until := h.now()
		since := until.Add(-24 * time.Hour)
		countResp, countErr := h.Observability.CountAgentDecisions(ctx, tenantID, since, until)
		if countErr != nil {
			log.Printf("oplus dashboard: CountAgentDecisions degraded: %v", countErr)
		} else {
			c := int(countResp.Count)
			resp.RecentDecisions24h = &c
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// Dimensions returns the full per-dimension rubric expand (D1-D4).
// Fans out 4 governance HTTP calls in parallel; errors on any one collapse
// to the error envelope (the FE renders `state:'error'`). Reshaped per
// audit finding #5 — rubric items renamed (`id → ref`, `title → requirement`)
// + derived `tools[]` + `tool_coverage` percentage.
func (h *OPlusHandler) Dimensions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	ctx := withUpstreamAuthCtx(r)
	tenantID, _ := upstreamCtx(ctx)

	rubrics, err := h.fanOutRubrics(ctx, tenantID)
	if err != nil {
		writeOPlusError(w, "GetDimensionRubric", err, h.now())
		return
	}

	// Reshape per audit finding #5 — map producer rubric items to the
	// FE-canonical `ref`, `requirement`, `tools[]`, `tool_coverage` shape.
	views := make([]DimensionRubricView, 0, len(rubrics))
	for i, r := range rubrics {
		views = append(views, mapDimensionRubric(i, r))
	}

	resp := DimensionsResponse{
		OPlusEnvelope: OPlusEnvelope{State: "live", FetchedAt: h.now()},
		Dimensions:    views,
	}
	writeJSON(w, http.StatusOK, resp)
}

// Agents pass-through to chora-observability /api/v1/observability/agents.
func (h *OPlusHandler) Agents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	ctx := withUpstreamAuthCtx(r)
	tenantID, _ := upstreamCtx(ctx)

	agents, err := h.Observability.GetAgents(ctx, tenantID)
	if err != nil {
		writeOPlusError(w, "GetAgents", err, h.now())
		return
	}
	resp := AgentsBodyResponse{
		OPlusEnvelope: OPlusEnvelope{State: "live", FetchedAt: h.now()},
		Crews:         agents.Crews,
	}
	writeJSON(w, http.StatusOK, resp)
}

// AgentPrompts is the /bff/oplus/prompts pass-through to chora-observability
// /api/v1/observability/agent-prompts (CHO-2364, ADR-197 read slice).
// Mirrors Agents: GET only, tenant from the validated upstream auth context,
// upstream failures collapse to the 200 {state:'error'} envelope the FE
// discriminates on. The agents body passes through raw (see
// AgentPromptsBodyResponse).
func (h *OPlusHandler) AgentPrompts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	ctx := withUpstreamAuthCtx(r)
	tenantID, _ := upstreamCtx(ctx)

	prompts, err := h.Observability.GetAgentPrompts(ctx, tenantID)
	if err != nil {
		writeOPlusError(w, "GetAgentPrompts", err, h.now())
		return
	}
	resp := AgentPromptsBodyResponse{
		OPlusEnvelope: OPlusEnvelope{State: "live", FetchedAt: h.now()},
		Agents:        prompts.Agents,
	}
	writeJSON(w, http.StatusOK, resp)
}

// Governance3Tab returns the 3-tab governance view: Decision Traces (from
// observability) + HITL pending queue (from governance HTTP) + Data
// Governance lineage (from a small static config). Reshaped per audit
// findings #1 + #2 — decisions and HITL items mapped to the FE-canonical
// view shape with derived `workflow_id`, `agent_slug`, `decision_type`,
// `hitl_status`, `model`, `confidence`, `cost`, `gate`, `summary`,
// `autonomy_level`, `waiting_since`, `assignee`.
//
// This handler REPLACES the original FakeUpstream-backed
// `/bff/oplus/governance` handler when Phase-C wiring is used.
func (h *OPlusHandler) Governance3Tab(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	ctx := withUpstreamAuthCtx(r)
	tenantID, _ := upstreamCtx(ctx)

	since := h.now().Add(-governanceDecisionWindowDays * 24 * time.Hour).Format(time.RFC3339)
	decisions, decErr := h.Observability.GetAgentDecisions(ctx, tenantID, since, "", governanceDecisionLimit)
	if decErr != nil {
		writeOPlusError(w, "GetAgentDecisions", decErr, h.now())
		return
	}

	// HITL (Oversight tab) is a SECONDARY source. A failure there — e.g. the
	// mesh-authz gap on chora-governance /api/hitl/pending (403) — must NOT
	// blank the Decision Traces tab, whose data (from chora-observability)
	// already succeeded above. Mirror the Dashboard's graceful-degradation
	// invariant: log + empty section, keep the 3-tab view live. The HITL queue
	// renders empty until the governance authz allowlist is extended for the
	// gateway principal.
	hitl, hitlErr := h.Governance.GetHITLPending(ctx, tenantID, 50)
	if hitlErr != nil {
		log.Printf("oplus governance: GetHITLPending degraded (HITL tab empty): %v", hitlErr)
		hitl = nil
	}

	// Reshape producer rows → FE-canonical views per audit findings #1 + #2.
	decisionViews := make([]GovernanceDecisionView, 0, len(decisions.Items))
	for _, d := range decisions.Items {
		decisionViews = append(decisionViews, mapAgentDecision(d))
	}
	hitlViews := make([]GovernanceHitlView, 0, len(hitl))
	for _, item := range hitl {
		hitlViews = append(hitlViews, mapHITLItem(item))
	}

	resp := GovernanceTabsResponse{
		OPlusEnvelope: OPlusEnvelope{State: "live", FetchedAt: h.now()},
		Decisions:     decisionViews,
		HITLPending:   hitlViews,
		DataLineage:   h.dataLineagePayload(),
		WindowDays:    governanceDecisionWindowDays,
	}
	writeJSON(w, http.StatusOK, resp)
}

// A2APanel returns either the live A2A console view or a `mode:'pending'`
// envelope when chora-a2a has no configured backend. Reshaped per audit
// finding #4 — `contract_id → id`, `invocation_id → id`, `outcome → status`,
// `identities → external_agents`, partner display name lookup, computed
// `trust_level`, `key_fingerprint`, `scope[]`, `endpoint`, `latency_ms`.
func (h *OPlusHandler) A2APanel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	ctx := withUpstreamAuthCtx(r)
	tenantID, _ := upstreamCtx(ctx)

	if h.A2A == nil || h.A2A.Pending() {
		writeJSON(w, http.StatusOK, A2AResponse{
			OPlusEnvelope: OPlusEnvelope{State: "pending", FetchedAt: h.now()},
			Mode:          "pending",
			Mock:          a2aMockPayload(),
		})
		return
	}

	since := h.now().Add(-24 * time.Hour).Format(time.RFC3339)

	contracts, contractsErr := h.A2A.GetContracts(ctx, tenantID)
	identities, identitiesErr := h.A2A.GetIdentities(ctx, tenantID)
	invocations, invocationsErr := h.A2A.GetInvocations(ctx, tenantID, since, 50)

	// If any single call landed in pending mode (the explicit not-deployed
	// sentinel), collapse to pending. Otherwise the first hard error wins.
	if errors.Is(contractsErr, upstream.ErrA2APending) ||
		errors.Is(identitiesErr, upstream.ErrA2APending) ||
		errors.Is(invocationsErr, upstream.ErrA2APending) {
		writeJSON(w, http.StatusOK, A2AResponse{
			OPlusEnvelope: OPlusEnvelope{State: "pending", FetchedAt: h.now()},
			Mode:          "pending",
			Mock:          a2aMockPayload(),
		})
		return
	}
	if err := firstErr(contractsErr, identitiesErr, invocationsErr); err != nil {
		writeOPlusError(w, "A2APanel", err, h.now())
		return
	}

	// Build a partner-name lookup from the identities slice so contract +
	// invocation rows can resolve `partner` display strings without an
	// extra backend hop. Falls back to partner_id when no match.
	partnerLookup := buildPartnerLookup(identities)

	contractViews := make([]A2AContractView, 0, len(contracts))
	for _, c := range contracts {
		contractViews = append(contractViews, mapA2AContract(c, partnerLookup))
	}
	agentViews := make([]A2AExternalAgentView, 0, len(identities))
	for _, id := range identities {
		agentViews = append(agentViews, mapA2AIdentity(id))
	}
	invocationViews := make([]A2AInvocationView, 0, len(invocations))
	for _, inv := range invocations {
		invocationViews = append(invocationViews, mapA2AInvocation(inv, partnerLookup))
	}

	writeJSON(w, http.StatusOK, A2AResponse{
		OPlusEnvelope:  OPlusEnvelope{State: "live", FetchedAt: h.now()},
		Mode:           "live",
		Contracts:      contractViews,
		ExternalAgents: agentViews,
		Invocations:    invocationViews,
	})
}

// Costs aggregates the chora-observability cost endpoints into ONE response
// the FE consumes directly (no discriminated-union wrapper — the FE derives
// live/stale/error from the HTTP status). Composes three observability HTTP
// calls:
//
//	GET /api/token-usage/aggregate?group_by=model → by_model
//	GET /api/token-usage/aggregate?group_by=agent → by_agent
//	GET /api/cost/cumulative                       → cumulative_cost_usd
//
// Upstream cost is int64 MICROS (1e-6 USD); the BFF divides by 1_000_000 for
// the float `cost_usd` / `cumulative_cost_usd` fields. `fetched_at` is stamped
// server-side. On ANY observability-upstream failure (incl. client unwired)
// the handler returns 503 — mirroring the HITL write-path upstream-unavailable
// handling, since this read has no envelope to collapse into.
func (h *OPlusHandler) Costs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	ctx := withUpstreamAuthCtx(r)
	tenantID, _ := upstreamCtx(ctx)

	// Time-window filter — `?range=day|week|month|all` (empty == all-time).
	// The window is computed server-side off `h.now()` and forwarded as
	// `from`/`to` to every observability cost call so all three rollups share
	// one consistent window. An unrecognised token is a loud 400.
	from, to, ok := costRangeWindow(r.URL.Query().Get("range"), h.now())
	if !ok {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_RANGE",
			"range must be one of day, week, month, all")
		return
	}

	if h.Observability == nil {
		writeError(w, http.StatusServiceUnavailable, "GATEWAY_OBSERVABILITY_UNAVAILABLE",
			"observability client not wired")
		return
	}

	byModel, err := h.Observability.AggregateTokenUsage(ctx, tenantID, "model", from, to)
	if err != nil {
		writeCostsError(w, "AggregateTokenUsage(model)", err)
		return
	}
	byAgent, err := h.Observability.AggregateTokenUsage(ctx, tenantID, "agent", from, to)
	if err != nil {
		writeCostsError(w, "AggregateTokenUsage(agent)", err)
		return
	}
	cumulative, err := h.Observability.GetCostCumulative(ctx, tenantID, from, to)
	if err != nil {
		writeCostsError(w, "GetCostCumulative", err)
		return
	}

	resp := CostsResponse{
		FetchedAt:         h.now(),
		CumulativeCostUSD: microsToUSD(cumulative.TotalCostUsdMicros),
		ByModel:           mapCostGroups(byModel.Groups),
		ByAgent:           mapCostGroups(byAgent.Groups),
		Currency:          h.displayCurrency(),
		FxRate:            h.fxRate(),
	}
	writeJSON(w, http.StatusOK, resp)
}

// EgressAudit serves GET /bff/oplus/governance/egress-audit — the O+
// external-egress audit trail (CHO-2245, ADR-231 Amendment 2026-07-17).
// Auditor/admin/owner gated by the AuditorGate on /bff/oplus/* (mounted in
// main.go). It filters the governance audit_log to action=external_egress via
// the HTTP /api/audit upstream.
//
// Status mapping is fail-loud ("a policy DENY must be 4xx"):
//   - 200 with the slice on success (Items always a non-nil array)
//   - governance 4xx (deny / not-found) → the SAME 4xx, never a masked 5xx
//   - governance 5xx / unwired / transport → 503
func (h *OPlusHandler) EgressAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if h.Governance == nil {
		writeError(w, http.StatusServiceUnavailable, "GATEWAY_GOVERNANCE_UNAVAILABLE",
			"governance client not wired")
		return
	}
	ctx := withUpstreamAuthCtx(r)
	tenantID, _ := upstreamCtx(ctx)

	events, err := h.Governance.QueryEgressAudit(ctx, tenantID, egressAuditLimit)
	if err != nil {
		// A governance DENY (4xx) surfaces verbatim as a 4xx — masking it as a
		// 5xx would erase the reason. Everything else (5xx / transport / unwired)
		// is an upstream-unavailable 503.
		var re *upstream.GovernanceReadError
		if errors.As(err, &re) {
			writeError(w, re.StatusCode, "GATEWAY_GOVERNANCE_DENIED", re.Error())
			return
		}
		writeError(w, http.StatusServiceUnavailable, "GATEWAY_GOVERNANCE_UNAVAILABLE",
			"chora-governance is unavailable")
		return
	}
	if events == nil {
		events = []upstream.EgressAuditEvent{}
	}
	writeJSON(w, http.StatusOK, EgressAuditResponse{
		FetchedAt: h.now(),
		Items:     events,
		Count:     len(events),
	})
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func (h *OPlusHandler) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}

// displayCurrency / fxRate are zero-value-safe accessors so a directly
// constructed OPlusHandler{} (not via NewOPlusHandler) never reports a blank
// currency or a 0 rate that would zero out every displayed amount.
func (h *OPlusHandler) displayCurrency() string {
	if h.DisplayCurrency == "" {
		return "USD"
	}
	return h.DisplayCurrency
}

func (h *OPlusHandler) fxRate() float64 {
	if h.FxRate <= 0 {
		return 1.0
	}
	return h.FxRate
}

// fanOutRubrics calls GetDimensionRubric for each of the 4 canonical
// dimensions in parallel, returns the slice in canonical order. Any
// error short-circuits with the first non-nil result.
func (h *OPlusHandler) fanOutRubrics(ctx context.Context, tenantID string) ([]upstream.DimensionRubric, error) {
	if h.Governance == nil {
		return nil, errors.New("governance client not wired")
	}
	dims := upstream.CanonicalIMDADimensions
	out := make([]upstream.DimensionRubric, len(dims))
	errs := make([]error, len(dims))

	var wg sync.WaitGroup
	for i, name := range dims {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			rub, err := h.Governance.GetDimensionRubric(ctx, tenantID, name)
			if err != nil {
				errs[i] = err
				return
			}
			if rub != nil {
				out[i] = *rub
			} else {
				out[i] = upstream.DimensionRubric{ID: name}
			}
		}(i, name)
	}
	wg.Wait()
	if err := firstErr(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

// dataLineagePayload returns the data lineage + RACI for the
// Data Governance tab. Per Phase C3 plan + the [[imda-governance-4-
// dimensions]] three-audience explainability model, RACI is intentionally
// hand-curated. The YAML loader is wired via the constructor; falls back
// to a minimal default when no YAML is present.
func (h *OPlusHandler) dataLineagePayload() any {
	if len(h.DataLineageYAML) > 0 {
		// We only emit the raw bytes when no parser is wired — chora-web
		// re-parses on the client. Keeps the BFF dependency-light.
		return string(h.DataLineageYAML)
	}
	return defaultDataLineage()
}

// firstErr returns the first non-nil error in args, or nil.
func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// writeOPlusError emits the error envelope with the discriminated-union
// `state:'error'` marker. HTTP status stays 200 so the FE can render the
// in-page error variant rather than a generic 5xx page.
func writeOPlusError(w http.ResponseWriter, label string, err error, fetchedAt time.Time) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	body := map[string]any{
		"state":      "error",
		"fetched_at": fetchedAt.Format(time.RFC3339),
		"error":      label + ": " + err.Error(),
	}
	_ = json.NewEncoder(w).Encode(body)
}

// writeCostsError surfaces an observability-upstream failure on the
// /bff/oplus/costs route as a real 503. Unlike the enveloped O+ reads, the
// cost panel has no discriminated-union wrapper — the FE derives the error
// state from the HTTP status, so the failure must NOT collapse to a 200.
func writeCostsError(w http.ResponseWriter, label string, err error) {
	log.Printf("oplus costs: %s upstream error: %v", label, err)
	writeError(w, http.StatusServiceUnavailable, "GATEWAY_OBSERVABILITY_UNAVAILABLE",
		"chora-observability is unavailable")
}

// costRangeWindow maps the O+ cost panel's `range` filter token to an absolute
// [from, to) window anchored at `now`. Windows are ROLLING (not calendar):
// day = last 24h, week = last 7 days, month = last 30 days. The empty token and
// "all" mean all-time (zero bounds → observability applies no time filter).
// Returns ok=false for an unrecognised token so the handler can fail loud (400)
// rather than silently widening to all-time.
func costRangeWindow(rangeParam string, now time.Time) (from, to time.Time, ok bool) {
	switch rangeParam {
	case "", "all":
		return time.Time{}, time.Time{}, true
	case "day":
		return now.Add(-24 * time.Hour), now, true
	case "week":
		return now.Add(-7 * 24 * time.Hour), now, true
	case "month":
		return now.Add(-30 * 24 * time.Hour), now, true
	default:
		return time.Time{}, time.Time{}, false
	}
}

// microsToUSD converts an int64 micros (1e-6 USD) value to a float USD value.
// Upstream chora-observability stores all cost as int64 micros to avoid float
// drift; the BFF surfaces the float for the FE cost panel.
func microsToUSD(micros int64) float64 {
	return float64(micros) / 1_000_000.0
}

// mapCostGroups reshapes upstream chora-observability ledger aggregate groups
// (group key + int64 micros + token counts) into the FE-canonical CostKeyRow
// (`key` + float `cost_usd` + token counts). Always returns a non-nil slice so
// the FE receives a JSON array, never null.
func mapCostGroups(groups []upstream.TokenUsageAggregateGroup) []CostKeyRow {
	out := make([]CostKeyRow, 0, len(groups))
	for _, g := range groups {
		out = append(out, CostKeyRow{
			Key:              g.Group,
			CostUSD:          microsToUSD(g.TotalCostUsdMicros),
			PromptTokens:     g.PromptTokens,
			CompletionTokens: g.CompletionTokens,
		})
	}
	return out
}

// -----------------------------------------------------------------------------
// Transformers — producer aggregates → FE-canonical views
// -----------------------------------------------------------------------------

// canonicalDimensionOrder is the canonical IMDA D1-D4 sequencing per ADR-141.
// Used as a lookup for the `num` field on PostureItemView and DimensionRubricView.
var canonicalDimensionOrder = map[string]int{
	"accountability":               1,
	"transparency":                 2,
	"safety_and_robustness":        3,
	"fairness_and_human_oversight": 4,
}

// deriveDimensionStatus maps a 0..100 score to the FE-canonical status string
// per the audit-finding #3 thresholds:
//
//	score ≥ 85         → achieved
//	60 ≤ score < 85    → partial
//	30 ≤ score < 60    → attention
//	score < 30         → pending
func deriveDimensionStatus(score int32) string {
	switch {
	case score >= 85:
		return "achieved"
	case score >= 60:
		return "partial"
	case score >= 30:
		return "attention"
	default:
		return "pending"
	}
}

// deriveDimensionStatusWithPriority drives the O+ dimensions traffic-light from
// the per-item P1/P2/P3 FAIL counts rather than the raw score (2026-06-02
// directive). Only FAILs count — PARTIAL/PASS do not; P3 never downgrades:
//
//	any P1 FAIL      → attention (RED)
//	≥ 2 P2 FAILs     → partial   (AMBER)
//	otherwise        → achieved  (GREEN)
//	no rubric items  → pending   (awaiting data — score basis unavailable)
//
// The score is retained for the "no items" pending fallback only; once a
// dimension has resolved rubric items the priority rule is authoritative.
func deriveDimensionStatusWithPriority(score int32, items []upstream.RubricItem) string {
	if len(items) == 0 {
		return deriveDimensionStatus(score)
	}
	p1Fail, p2Fail := 0, 0
	for _, it := range items {
		if !strings.EqualFold(strings.TrimSpace(it.Status), "FAIL") {
			continue
		}
		switch strings.ToUpper(strings.TrimSpace(it.Priority)) {
		case "P1":
			p1Fail++
		case "P2":
			p2Fail++
		}
	}
	switch {
	case p1Fail >= 1:
		return "attention"
	case p2Fail >= 2:
		return "partial"
	default:
		return "achieved"
	}
}

// buildPostureView maps a producer dimensions slice (whatever order the
// projector emits in) to the FE-canonical posture slice sorted by D1-D4.
// Missing dimensions are filled with `pending` placeholders so the FE
// always receives a 4-element array.
func buildPostureView(dims []upstream.IMDADimensionSummary) []PostureItemView {
	byID := make(map[string]upstream.IMDADimensionSummary, len(dims))
	for _, d := range dims {
		byID[d.ID] = d
	}
	canonical := upstream.CanonicalIMDADimensions
	out := make([]PostureItemView, 0, len(canonical))
	for i, id := range canonical {
		d, ok := byID[id]
		score := int32(0)
		if ok {
			score = d.ScorePct
		}
		status := deriveDimensionStatus(score)
		if !ok {
			status = "pending"
		}
		out = append(out, PostureItemView{
			Num:    i + 1, // 1..4 per canonical IMDA order
			Label:  id,
			Score:  score,
			Status: status,
		})
	}
	return out
}

// allBaselineAchieved reports whether every posture row carries the
// `achieved` status (used by the dashboard `all_baseline_achieved` flag).
func allBaselineAchieved(posture []PostureItemView) bool {
	if len(posture) == 0 {
		return false
	}
	for _, p := range posture {
		if p.Status != "achieved" {
			return false
		}
	}
	return true
}

// mapDimensionRubric reshapes a producer DimensionRubric (with raw
// id/title/evidence_source rubric items) to the FE-canonical
// DimensionRubricView (with derived `num`, `label`, `score`, `status`,
// `ref`, `requirement`, `tools[]`, `tool_coverage`).
func mapDimensionRubric(idx int, r upstream.DimensionRubric) DimensionRubricView {
	num := idx + 1
	if n, ok := canonicalDimensionOrder[r.ID]; ok {
		num = n
	}
	status := deriveDimensionStatusWithPriority(r.ScorePct, r.RubricItems)
	items := make([]RubricItemView, 0, len(r.RubricItems))
	for _, ri := range r.RubricItems {
		items = append(items, mapRubricItem(ri))
	}
	return DimensionRubricView{
		Num:         num,
		Label:       r.ID,
		Score:       r.ScorePct,
		Status:      status,
		Description: r.Description,
		RubricItems: items,
	}
}

// mapRubricItem reshapes a producer RubricItem to the FE-canonical
// RubricItemView per audit finding #5: rename `id → ref`,
// `title → requirement`; derive `tools[]` from `evidence_source` split on
// `+` or `,`; compute `tool_coverage` percentage as `count(tools-with-link)
// / count(tools)` if any tools have evidence URLs, else falls back to the
// raw evidence_source string for human display.
func mapRubricItem(ri upstream.RubricItem) RubricItemView {
	tools := splitToolsFromEvidenceSource(ri.EvidenceSource)
	coverage := computeToolCoverage(ri.EvidenceSource, ri.EvidenceSourceURL)
	var evidenceURL *string
	if strings.TrimSpace(ri.EvidenceSourceURL) != "" {
		u := ri.EvidenceSourceURL
		evidenceURL = &u
	}
	// Prefer the short rubric ref ("1.1") for display; the id slug
	// ("audit_trail_coverage") overflows the narrow ref column and wraps into
	// cramped tiny text. Fall back to the id only when ref is absent.
	ref := strings.TrimSpace(ri.Ref)
	if ref == "" {
		ref = ri.ID
	}
	return RubricItemView{
		Ref:               ref,
		Requirement:       ri.Title,
		ToolCoverage:      coverage,
		Tools:             tools,
		Status:            strings.ToLower(strings.TrimSpace(ri.Status)),
		EvidenceSourceURL: evidenceURL,
		Priority:          strings.ToUpper(strings.TrimSpace(ri.Priority)),
	}
}

// splitToolsFromEvidenceSource normalises an evidence_source string like
// "Cloud Trace + AgentDecisionLog" into a slice of lowercase + underscore
// tool slugs `["cloud_trace", "agent_decision_log"]`. Per audit finding #5.
func splitToolsFromEvidenceSource(src string) []string {
	s := strings.TrimSpace(src)
	if s == "" {
		return []string{}
	}
	// Split on either `+` or `,`; trim each chunk; normalise to
	// lowercase + underscore-instead-of-space.
	normalized := strings.NewReplacer("+", ",").Replace(s)
	parts := strings.Split(normalized, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		slug := strings.ToLower(p)
		slug = strings.ReplaceAll(slug, " ", "_")
		out = append(out, slug)
	}
	return out
}

// computeToolCoverage returns a human-readable coverage string for the
// rubric item. Per audit finding #5 — falls back to the raw
// evidence_source string when no URL is available so the FE keeps a
// useful display label.
func computeToolCoverage(src, url string) string {
	src = strings.TrimSpace(src)
	if src == "" {
		return ""
	}
	return src
}

// mapAgentDecision reshapes a producer AgentDecisionRow to the FE-canonical
// GovernanceDecisionView per audit finding #1. Derived fields:
//   - WorkflowID ← CorrelationID (falls back to LogID)
//   - AgentSlug ← lowercase-with-hyphens of AGID/AgentID
//   - DecisionType ← Verdict (falls back to DecisionKind / DecisionType / Decision)
//   - Model ← ModelID
//   - Cost ← formatUSD(CostUSD)
//   - Timestamp ← CreatedAt RFC3339
func mapAgentDecision(d upstream.AgentDecisionRow) GovernanceDecisionView {
	wfID := d.CorrelationID
	if wfID == "" {
		wfID = d.LogID
	}
	slugSource := d.AGID
	if slugSource == "" {
		slugSource = d.AgentID
	}
	// Agent display: chora-observability emits `agid`, not `agent_id` — fall
	// back to AGID so the column isn't blank.
	agent := d.AgentID
	if agent == "" {
		agent = d.AGID
	}
	// Decision type: prefer the agent's quality-gate VERDICT (accepted |
	// rejected | completed_with_warning | refused — the human-meaningful "what
	// did the agent decide", CHO-1700), then the enriched `decision_kind`
	// (CHO-1560), then the current-shape `decision_type` obs serializes, then
	// the legacy `decision`. The verdict is distinct from the 4-value
	// decision_type ENUM (which was the base `respond` for every qgen row).
	decType := d.Verdict
	if decType == "" {
		decType = d.DecisionKind
	}
	if decType == "" {
		decType = d.DecisionType
	}
	if decType == "" {
		decType = d.Decision
	}
	var traceID *string
	if d.TraceID != "" {
		t := d.TraceID
		traceID = &t
	}
	var ctURL *string
	if d.CloudTraceURL != "" {
		u := d.CloudTraceURL
		ctURL = &u
	}
	// Reasoning panel (IMDA D2): prefer the agent's bounded reasoning_summary
	// (= qgen critic_notes), fall back to the top-level `reason`. Hashes are the
	// PII-safe citation. The row-click panel renders these.
	reasoningSummary, inputHash, outputHash := "", "", ""
	if d.Reasoning != nil {
		reasoningSummary = d.Reasoning.Summary
		inputHash = d.Reasoning.InputHash
		outputHash = d.Reasoning.OutputHash
	}
	if reasoningSummary == "" {
		reasoningSummary = d.Reason
	}
	return GovernanceDecisionView{
		ID:               d.LogID,
		WorkflowID:       wfID,
		Agent:            agent,
		AgentSlug:        slugifyAgent(slugSource),
		DecisionType:     decType,
		Model:            d.ModelID,
		Cost:             formatUSD(d.CostUSD),
		Timestamp:        d.CreatedAt.UTC().Format(time.RFC3339),
		TraceID:          traceID,
		CloudTraceURL:    ctURL,
		ReasoningSummary: reasoningSummary,
		InputHash:        inputHash,
		OutputHash:       outputHash,
		PromptConditions: d.PromptConditions,
	}
}

// slugifyAgent computes the FE `agent_slug` from an agent_id or AGID.
// Replaces underscores with hyphens, lowercases.
func slugifyAgent(src string) string {
	s := strings.ToLower(strings.TrimSpace(src))
	s = strings.ReplaceAll(s, "_", "-")
	return s
}

// formatUSD returns the FE-canonical cost string (e.g. "$0.0021") for a
// nullable USD float pointer. nil → empty string.
func formatUSD(amount *float64) string {
	if amount == nil {
		return ""
	}
	return fmt.Sprintf("$%.4f", *amount)
}

// mapHITLItem reshapes a producer HITLPendingItem to the FE-canonical
// GovernanceHitlView per audit finding #2.
//
// AssigneeGCID is pass-through: nil → null in JSON. The FE renders an
// "unassigned" localised string when null. No synthetic placeholder is
// substituted at the BFF layer per [[feedback-no-stubs-real-wiring]].
func mapHITLItem(h upstream.HITLPendingItem) GovernanceHitlView {
	wfID := h.CorrelationID
	if wfID == "" {
		wfID = h.DecisionID
	}
	summary := h.Summary
	if summary == "" {
		summary = h.Reason
	}
	return GovernanceHitlView{
		ID:            h.DecisionID,
		WorkflowID:    wfID,
		Gate:          normaliseGate(h.DecisionKind),
		Agent:         h.AgentID,
		Summary:       summary,
		AutonomyLevel: strings.TrimSpace(h.AutonomyLevel),
		WaitingSince:  h.CreatedAt.UTC().Format(time.RFC3339),
		Assignee:      h.AssigneeGCID,
	}
}

// normaliseGate maps a producer decision_kind to one of the FE-canonical
// gate strings: `question_review`, `report_review`, `material_decision`.
// Unknown kinds fall through to `material_decision` (the catch-all in the
// FE GovernanceHitlItem union).
func normaliseGate(kind string) string {
	k := strings.ToLower(strings.TrimSpace(kind))
	switch k {
	case "question_review", "report_review", "material_decision":
		return k
	}
	// Heuristics for legacy strings.
	switch {
	case strings.Contains(k, "question"):
		return "question_review"
	case strings.Contains(k, "report"):
		return "report_review"
	default:
		return "material_decision"
	}
}

// buildPartnerLookup constructs an AGID → partner-display-name lookup from
// the A2A identities slice, used by mapA2AContract + mapA2AInvocation to
// resolve the FE `partner` field without an extra backend hop.
func buildPartnerLookup(ids []upstream.A2AIdentity) map[string]string {
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		name := id.PartnerName
		if name == "" {
			name = id.DisplayName
		}
		if id.AGID != "" {
			out[id.AGID] = name
		}
	}
	return out
}

// mapA2AContract reshapes a producer A2AContract to the FE-canonical
// A2AContractView per audit finding #4.
func mapA2AContract(c upstream.A2AContract, partnerLookup map[string]string) A2AContractView {
	partner := c.PartnerName
	if partner == "" {
		// Resolve via identities lookup if AGID is known.
		if c.AGID != "" {
			if p, ok := partnerLookup[c.AGID]; ok && p != "" {
				partner = p
			}
		}
		if partner == "" {
			partner = c.PartnerID
		}
	}
	scope := c.Scope
	if scope == nil {
		scope = []string{}
	}
	var lastInv *string
	if c.LastInvocationAt != "" {
		s := c.LastInvocationAt
		lastInv = &s
	}
	created := ""
	if !c.CreatedAt.IsZero() {
		created = c.CreatedAt.UTC().Format(time.RFC3339)
	}
	return A2AContractView{
		ID:             c.ContractID,
		Partner:        partner,
		AGID:           c.AGID,
		Scope:          scope,
		Status:         c.Status,
		LastInvocation: lastInv,
		Invocations30d: c.Invocations30d,
		CreatedAt:      created,
	}
}

// mapA2AIdentity reshapes a producer A2AIdentity to the FE-canonical
// A2AExternalAgentView per audit finding #4.
//
// Both `trust_level` and `key_fingerprint` are PASS-THROUGH from the
// chora-a2a-gateway producer (oplus_router.registrationToOPlusMap),
// which derives them from real backend state:
//
//   - trust_level: chora-a2a-gateway runs partner.Registration.
//     DeriveTrustLevelView(now, invocations) → verified|pilot|experimental
//     based on Registration state + DNS-verification + approval age +
//     recent-invocation error rate. See
//     services/chora-a2a-gateway/internal/domain/partner/trust_level.go
//     for the canonical rule. The BFF MUST NOT fabricate a default.
//
//   - key_fingerprint: chora-a2a-gateway emits the SHA-256 of the
//     registered Ed25519 PEM (ExternalAgentIdentity.KeyFingerprint) when
//     a key is on file, OR an empty string when no key has been
//     uploaded yet. The BFF MUST NOT fabricate sha256(agid) — that was
//     the M12 placeholder removed per [[feedback-no-stubs-real-wiring]]
//     (null/empty is honest; sha256(agid) is fake).
//
// `rotated_at` continues to fall back to CreatedAt because both
// timestamps reference identity-lifecycle anchors; the producer surfaces
// the latest of registration / Rotate via ExternalAgentIdentity.UpdatedAt
// when an identity is on file, and the BFF keeps the CreatedAt fallback
// for compatibility with older payloads that never carried the field.
func mapA2AIdentity(id upstream.A2AIdentity) A2AExternalAgentView {
	partner := id.PartnerName
	if partner == "" {
		partner = id.DisplayName
	}
	// trust_level — pure pass-through. Producer is the source of truth;
	// when the producer emits an empty trust_level the BFF preserves it
	// rather than fabricating a default. The FE renders the empty pill
	// only if the upstream contract is broken — which is the correct
	// loud-fail behaviour per [[feedback-no-stubs-real-wiring]].
	trust := strings.ToLower(strings.TrimSpace(id.TrustLevel))

	// key_fingerprint — pure pass-through. NEVER compute sha256(agid).
	// When the producer has no identity on file the field is empty; the
	// FE renders a blank cell which is the correct deployment-pending
	// signal.
	fingerprint := id.KeyFingerprint

	rotated := id.RotatedAt
	if rotated == "" && !id.CreatedAt.IsZero() {
		rotated = id.CreatedAt.UTC().Format(time.RFC3339)
	}
	return A2AExternalAgentView{
		AGID:           id.AGID,
		Partner:        partner,
		TrustLevel:     trust,
		KeyFingerprint: fingerprint,
		RotatedAt:      rotated,
	}
}

// mapA2AInvocation reshapes a producer A2AInvocation to the FE-canonical
// A2AInvocationView per audit finding #4. `outcome → status`, `started_at
// → timestamp`, computed `latency_ms` from started_at/ended_at when both
// timestamps are present.
func mapA2AInvocation(inv upstream.A2AInvocation, partnerLookup map[string]string) A2AInvocationView {
	partner := inv.PartnerName
	if partner == "" {
		if p, ok := partnerLookup[inv.AGID]; ok {
			partner = p
		}
	}
	status := normaliseInvocationStatus(inv.Outcome)
	latency := inv.LatencyMS
	if latency == 0 && !inv.StartedAt.IsZero() && !inv.EndedAt.IsZero() {
		latency = int(inv.EndedAt.Sub(inv.StartedAt).Milliseconds())
		if latency < 0 {
			latency = 0
		}
	}
	// Endpoint pass-through — chora-a2a now records the dispatch URL on
	// every invocation (see invocation.NewParams.Endpoint). Empty string
	// is the honest-null contract for legacy / pre-resolution rows; the FE
	// renders an em dash. NO placeholder substitution.
	endpoint := inv.Endpoint
	timestamp := ""
	if !inv.StartedAt.IsZero() {
		timestamp = inv.StartedAt.UTC().Format(time.RFC3339)
	}
	return A2AInvocationView{
		ID:         inv.InvocationID,
		ContractID: inv.ContractID,
		AGID:       inv.AGID,
		Partner:    partner,
		Endpoint:   endpoint,
		Status:     status,
		LatencyMS:  latency,
		Timestamp:  timestamp,
	}
}

// normaliseInvocationStatus maps a producer outcome string (`permitted`,
// `denied`, `error`, ...) to the FE-canonical invocation status union
// (`success` | `denied` | `error`).
func normaliseInvocationStatus(outcome string) string {
	switch strings.ToLower(strings.TrimSpace(outcome)) {
	case "permitted", "success", "ok":
		return "success"
	case "denied":
		return "denied"
	default:
		return "error"
	}
}

// lineageRow is the FE-canonical per-table lineage entry consumed by the
// O+ Data Governance tab. It mirrors chora-web's `GovernanceLineageEntry`
// (governance.service.ts): the FE iterates `data_lineage` as a flat ARRAY
// of these rows (oplus-governance.component.ts → `lineage()` → template
// `@for (l of lineage(); track l.table)`). The RACI matrix the tab renders
// is a design-time FE constant (`RACI_MATRIX` in imda-dimensions.ts) and is
// NOT sourced from this payload — so the BFF emits lineage rows only.
//
// `classification` is constrained to the FE enum
// (`'Audit' | 'Knowledge' | 'PII' | 'Operational'`); `pii_closure_map`
// reports whether the owning domain declares a `PII_Closure_Map.yaml` entry
// covering the table (per the federated account-closure saga in
// .claude/rules/ddd-enforcement.md).
type lineageRow struct {
	Domain         string `json:"domain"`
	DB             string `json:"db"`
	Table          string `json:"table"`
	Retention      string `json:"retention"`
	Classification string `json:"classification"`
	PIIClosureMap  bool   `json:"pii_closure_map"`
}

// defaultDataLineage returns the hand-curated per-table data-lineage rows
// for the O+ Data Governance tab when no YAML override is configured (the
// CHORA_GATEWAY_DATA_LINEAGE_PATH env path takes precedence per
// [[secrets-and-env]]). This is design-time governance metadata grounded in
// Chora's 13-database topology (5 core + 8 supporting per
// .claude/rules/ddd-enforcement.md) — representative key aggregate tables
// per domain with honest classification / retention / closure-map values.
//
// Shape is the FE-canonical ARRAY of GovernanceLineageEntry rows (NOT the
// legacy `{domains:[...]}` RACI object, which the FE never consumed → the
// table rendered zero rows). Classification ∈ {Audit, Knowledge, PII,
// Operational}.
// ⚠ Table names were swept against each database's own live schema on
// 2026-08-23 and 13 of them did not exist. They are corrected here to the real
// tables, verified by COLUMNS and not by name similarity. Four further rows
// could not be corrected without a governance ruling and are listed in the
// audit report rather than guessed at: chora_notifications.channel_preferences
// is ambiguous between two real candidates; chora_creation.media_assets is a
// JSONB COLUMN on learning_atoms rather than a table; and
// chora_creation.atom_semantic_edges was RELOCATED to chora_consumption, so
// correcting it also moves the Domain attribution. This is a compliance
// surface, so a wrong name is worse than a name known to be wrong.
func defaultDataLineage() any {
	return []lineageRow{
		// ── 5 core Content {verb} domains ────────────────────────────────
		// chora_creation — LearningAtom (authoring), AtomRevision, Media.
		{Domain: "Content Creation", DB: "chora_creation", Table: "learning_atoms", Retention: "Indefinite (soft-delete)", Classification: "Knowledge", PIIClosureMap: false},
		{Domain: "Content Creation", DB: "chora_creation", Table: "atom_revisions", Retention: "Append-only / permanent", Classification: "Knowledge", PIIClosureMap: false},
		{Domain: "Content Creation", DB: "chora_creation", Table: "media_assets", Retention: "Indefinite (soft-delete)", Classification: "Knowledge", PIIClosureMap: false},
		{Domain: "Content Creation", DB: "chora_creation", Table: "atom_semantic_edges", Retention: "Indefinite (soft-delete)", Classification: "Knowledge", PIIClosureMap: false},
		// chora_consumption - LearningPath, AtomAttempt, Companion, KnowledgeGraph (per-user).
		{Domain: "Content Consumption", DB: "chora_consumption", Table: "atomic_sessions", Retention: "5 years then pseudonymise", Classification: "Operational", PIIClosureMap: true},
		{Domain: "Content Consumption", DB: "chora_consumption", Table: "learning_paths", Retention: "5 years then pseudonymise", Classification: "Operational", PIIClosureMap: true},
		{Domain: "Content Consumption", DB: "chora_consumption", Table: "companions", Retention: "Until account closure (crypto-shred)", Classification: "PII", PIIClosureMap: true},
		{Domain: "Content Consumption", DB: "chora_consumption", Table: "kg_user_map_clusters", Retention: "Until account closure (crypto-shred)", Classification: "PII", PIIClosureMap: true},
		// chora_sharing — social graph, Posts, Reactions, Duels, Leaderboards.
		{Domain: "Content Sharing", DB: "chora_sharing", Table: "posts", Retention: "Until account closure (pseudonymise)", Classification: "PII", PIIClosureMap: true},
		{Domain: "Content Sharing", DB: "chora_sharing", Table: "reactions", Retention: "Until account closure (pseudonymise)", Classification: "Operational", PIIClosureMap: true},
		{Domain: "Content Sharing", DB: "chora_sharing", Table: "leaderboard_snapshots", Retention: "Rolling 90 days", Classification: "Operational", PIIClosureMap: false},
		// chora_delivery — Course/Class, Booking, Certification, Rostering, SkillsFutures.
		{Domain: "Content Delivery", DB: "chora_delivery", Table: "course_enrollments", Retention: "7 years (regulatory)", Classification: "PII", PIIClosureMap: true},
		{Domain: "Content Delivery", DB: "chora_delivery", Table: "certifications", Retention: "7 years (regulatory)", Classification: "PII", PIIClosureMap: true},
		{Domain: "Content Delivery", DB: "chora_delivery", Table: "class_rostering", Retention: "5 years then pseudonymise", Classification: "Operational", PIIClosureMap: true},
		// chora_a2a — A2AContract, ExternalAgentIdentity (AGID), A2AInvocation.
		{Domain: "Agent-to-Agent", DB: "chora_a2a", Table: "a2a_contracts", Retention: "7 years (contractual)", Classification: "Operational", PIIClosureMap: false},
		{Domain: "Agent-to-Agent", DB: "chora_a2a", Table: "a2a_invocations", Retention: "Rolling 90 days", Classification: "Audit", PIIClosureMap: false},
		// ── 8 supporting / platform domains ──────────────────────────────
		// chora_identity — GCID, TenantMembership, closure saga state (highest-sensitivity PII).
		{Domain: "Identity", DB: "chora_identity", Table: "users", Retention: "Until account closure (crypto-shred)", Classification: "PII", PIIClosureMap: true},
		{Domain: "Identity", DB: "chora_identity", Table: "tenant_memberships", Retention: "Until account closure (crypto-shred)", Classification: "PII", PIIClosureMap: true},
		{Domain: "Identity", DB: "chora_identity", Table: "closure_sagas", Retention: "10 years (audit)", Classification: "Audit", PIIClosureMap: true},
		// chora_tenancy — Tenant, TenantEntitlement, billing business-logic (Stripe correlation extracted to Payments).
		{Domain: "Tenancy", DB: "chora_tenancy", Table: "tenants", Retention: "Indefinite (soft-delete)", Classification: "Operational", PIIClosureMap: false},
		{Domain: "Tenancy", DB: "chora_tenancy", Table: "add_on_subscriptions", Retention: "Indefinite (soft-delete)", Classification: "Operational", PIIClosureMap: false},
		{Domain: "Tenancy", DB: "chora_tenancy", Table: "invoices", Retention: "7 years (tax)", Classification: "PII", PIIClosureMap: true},
		// chora_governance — Gatekeeper decisions, audit log, IMDA evidence, HITL gates.
		{Domain: "Governance", DB: "chora_governance", Table: "audit_log", Retention: "10 years (audit, immutable)", Classification: "Audit", PIIClosureMap: false},
		{Domain: "Governance", DB: "chora_governance", Table: "hitl_decision_log", Retention: "10 years (audit)", Classification: "Audit", PIIClosureMap: false},
		{Domain: "Governance", DB: "chora_governance", Table: "policy_violation_log", Retention: "10 years (audit)", Classification: "Audit", PIIClosureMap: false},
		// chora_observability — TokenUsageLedger, AgentDecisionLog (billing-grade audit).
		{Domain: "Observability", DB: "chora_observability", Table: "token_usage_ledger", Retention: "7 years (billing audit)", Classification: "Audit", PIIClosureMap: false},
		{Domain: "Observability", DB: "chora_observability", Table: "agent_decision_log", Retention: "10 years (audit)", Classification: "Audit", PIIClosureMap: false},
		// chora_notifications — delivery records, channel preferences.
		{Domain: "Notifications", DB: "chora_notifications", Table: "delivery_logs", Retention: "Rolling 180 days", Classification: "PII", PIIClosureMap: true},
		{Domain: "Notifications", DB: "chora_notifications", Table: "channel_preferences", Retention: "Until account closure (pseudonymise)", Classification: "PII", PIIClosureMap: true},
		// chora_ai_kernel — orchestrator checkpoints, agent registry, prompt config.
		{Domain: "AI Kernel", DB: "chora_ai_kernel", Table: "checkpoints", Retention: "Rolling 30 days", Classification: "Operational", PIIClosureMap: false},
		{Domain: "AI Kernel", DB: "chora_ai_kernel", Table: "chora_agent_registry", Retention: "Indefinite (soft-delete)", Classification: "Operational", PIIClosureMap: false},
		// chora_support — tickets, knowledge base, SLA policies, CSAT.
		{Domain: "Support", DB: "chora_support", Table: "support_tickets", Retention: "5 years then pseudonymise", Classification: "PII", PIIClosureMap: true},
		{Domain: "Support", DB: "chora_support", Table: "ticket_feedback", Retention: "5 years then pseudonymise", Classification: "PII", PIIClosureMap: true},
		// chora_payments — 5 Purchase aggregates (Stripe-correlation extract, ADR-164).
		{Domain: "Payments", DB: "chora_payments", Table: "course_purchases", Retention: "7 years (financial)", Classification: "PII", PIIClosureMap: true},
		{Domain: "Payments", DB: "chora_payments", Table: "user_subscriptions", Retention: "7 years (financial)", Classification: "PII", PIIClosureMap: true},
		{Domain: "Payments", DB: "chora_payments", Table: "tenant_mana_topups", Retention: "7 years (financial)", Classification: "Operational", PIIClosureMap: false},
	}
}

// a2aMockPayload is the explicit deployment-pending mock returned when
// chora-a2a has no configured backend. The FE renders the "A2A backend
// deployment pending" banner over this payload.
func a2aMockPayload() map[string]any {
	return map[string]any{
		"contracts": []map[string]any{
			{"contract_id": "mock-contract-001", "partner_id": "mock-partner-a", "status": "active"},
		},
		"identities": []map[string]any{
			{"agid": "mock-agid-001", "display_name": "Mock external agent A", "status": "active"},
		},
		"invocations": []map[string]any{
			{"invocation_id": "mock-inv-001", "contract_id": "mock-contract-001", "outcome": "permitted"},
		},
		"_note": "chora-a2a backend deployment pending — CHORA_A2A_HTTP_ADDR is unset",
	}
}

// LoadDataLineageYAMLFromEnv returns the on-disk path's contents for the
// data-lineage YAML configured via CHORA_GATEWAY_DATA_LINEAGE_PATH.
// Returns nil when unset or unreadable — caller falls back to the default
// payload. Per [[secrets-and-env]] — file path comes from env, not inline.
func LoadDataLineageYAMLFromEnv() []byte {
	path := strings.TrimSpace(os.Getenv("CHORA_GATEWAY_DATA_LINEAGE_PATH"))
	if path == "" {
		return nil
	}
	// #nosec G703 — path is env-controlled per secrets-and-env (deployment
	// operator sets CHORA_GATEWAY_DATA_LINEAGE_PATH, not end-user input).
	b, err := os.ReadFile(path) //nolint:gosec // path is env-controlled per secrets-and-env
	if err != nil {
		return nil
	}
	return b
}

// LoadCostDisplayConfigFromEnv reads the O+ cost-panel presentation currency
// from env per [[secrets-and-env]] (no inline config):
//
//	OPLUS_COST_DISPLAY_CURRENCY — display currency code (default "USD")
//	OPLUS_COST_FX_RATE          — STATIC display-units-per-USD rate (default 1.0)
//
// The rate is a static configured value, NOT live FX — update it on redeploy.
// Empty/garbage input falls back to the USD@1.0 identity (no conversion).
func LoadCostDisplayConfigFromEnv() (currency string, fxRate float64) {
	currency = strings.TrimSpace(os.Getenv("OPLUS_COST_DISPLAY_CURRENCY"))
	if currency == "" {
		currency = "USD"
	}
	fxRate = 1.0
	if v := strings.TrimSpace(os.Getenv("OPLUS_COST_FX_RATE")); v != "" {
		if r, err := strconv.ParseFloat(v, 64); err == nil && r > 0 {
			fxRate = r
		}
	}
	return currency, fxRate
}
