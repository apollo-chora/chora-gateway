// handlers_oplus_test.go — Phase C table-driven coverage for the 5 O+ BFF
// routes (`dashboard`, `dimensions`, `agents`, `governance`, `a2a`).
//
// Strategy:
//   - real-wire each handler against httptest servers for governance HTTP +
//     observability HTTP + a2a HTTP
//   - inject a fake gRPC client for the governance gRPC surface
//   - exercise the handler directly via httptest.NewRecorder rather than
//     standing up the full router (the auth gate is tested independently
//     in middleware/auditor_gate_test.go)
package httpadapter_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	governancev1 "github.com/apollo-chora/chora-contracts/gen/go/chora/services/governance/v1"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

// stubGovernanceGRPC implements upstream.GovernanceGRPCClient.
type stubGovernanceGRPC struct {
	dash  *governancev1.GetIMDADashboardResponse
	audit *governancev1.QueryAuditEventsResponse
}

func (s *stubGovernanceGRPC) GetIMDADashboard(_ context.Context, _ *governancev1.GetIMDADashboardRequest, _ ...grpc.CallOption) (*governancev1.GetIMDADashboardResponse, error) {
	return s.dash, nil
}
func (s *stubGovernanceGRPC) QueryAuditEvents(_ context.Context, _ *governancev1.QueryAuditEventsRequest, _ ...grpc.CallOption) (*governancev1.QueryAuditEventsResponse, error) {
	return s.audit, nil
}

// dashboardFixture returns a 4-dimension response with realistic scores.
func dashboardFixture() *governancev1.GetIMDADashboardResponse {
	return &governancev1.GetIMDADashboardResponse{
		TenantId: "t1",
		Dimensions: []*governancev1.IMDADimensionAssessment{
			{Dimension: governancev1.IMDADimension_IMDA_DIMENSION_ACCOUNTABILITY, Score: 72, Indicators: []string{"#8"}},
			{Dimension: governancev1.IMDADimension_IMDA_DIMENSION_TRANSPARENCY, Score: 58},
			{Dimension: governancev1.IMDADimension_IMDA_DIMENSION_SAFETY_AND_ROBUSTNESS, Score: 81},
			{Dimension: governancev1.IMDADimension_IMDA_DIMENSION_FAIRNESS_AND_HUMAN_OVERSIGHT, Score: 64},
		},
	}
}

// newGovernanceHTTPMock returns an httptest server that serves the
// Phase-B governance HTTP endpoints: /api/imda/dimensions/{name}/rubric +
// /api/hitl/pending.
func newGovernanceHTTPMock(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/hitl/pending":
			body := []upstream.HITLPendingItem{
				{DecisionID: "d1", TenantID: "t1", AgentID: "qgen_critic", CrewName: "mcq_ai_assist", CreatedAt: time.Now().UTC()},
			}
			_ = json.NewEncoder(w).Encode(body)
		case strings.HasPrefix(r.URL.Path, "/api/imda/dimensions/") && strings.HasSuffix(r.URL.Path, "/rubric"):
			parts := strings.Split(r.URL.Path, "/")
			dim := parts[len(parts)-2]
			body := upstream.DimensionRubric{
				ID:       dim,
				Title:    strings.ReplaceAll(dim, "_", " "),
				ScorePct: 72,
				RubricItems: []upstream.RubricItem{
					{ID: dim + "-1", Title: "Item 1", Status: "PASS"},
					{ID: dim + "-2", Title: "Item 2", Status: "PARTIAL"},
					{ID: dim + "-3", Title: "Item 3", Status: "PARTIAL"},
				},
			}
			_ = json.NewEncoder(w).Encode(body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// newObservabilityHTTPMock serves the chora-observability endpoints.
func newObservabilityHTTPMock(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/observability/agents":
			body := upstream.AgentsResponse{
				Crews: []upstream.Crew{
					{
						CrewName:          "mcq_ai_assist",
						CrewID:            "crew-mcq",
						HasRecentActivity: true,
						Agents: []upstream.Agent{
							{AgentID: "qgen_question", EngineID: "8635637442075951104"},
							{AgentID: "qgen_critic", EngineID: "2658824174880948224"},
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(body)
		case r.URL.Path == "/api/v1/observability/agent-prompts":
			// CHO-2364 prompt-evidence read slice. Carries a field the BFF
			// has never heard of so the pass-through proof is real.
			_, _ = w.Write([]byte(`{"agents":[
				{"agent_id":"qgen_question","crew_name":"qgen","evidence_kind":"decisions",
				 "decisions_total":4,"latest_prompt_version":"v4","latest_prompt_source":"grimoire",
				 "future_field":"kept",
				 "versions":[{"prompt_version":"v4","prompt_source":"grimoire","decisions":1,"last_seen":"2026-07-20T13:00:00Z"}],
				 "use_cases":[{"key":"ai_assist_single","decisions":2},{"key":"batch","decisions":1},{"key":"daily_dose","decisions":1}]},
				{"agent_id":"qgen_critic","crew_name":"qgen","evidence_kind":"decisions","decisions_total":0,"versions":[],
				 "use_cases":[{"key":"ai_assist_single","decisions":0},{"key":"batch","decisions":0},{"key":"daily_dose","decisions":0}]},
				{"agent_id":"oe_evaluator","crew_name":"oe_grading","evidence_kind":"decisions","decisions_total":0,"versions":[]},
				{"agent_id":"oe_moderator","crew_name":"oe_grading","evidence_kind":"decisions","decisions_total":0,"versions":[]},
				{"agent_id":"companion","crew_name":"companion","evidence_kind":"ritual_stamps","runs_total":3,
				 "versions":[{"prompt_version":"v3","runs":2,"last_seen":"2026-07-22T09:00:00Z"}]}
			]}`))
		case r.URL.Path == "/api/agent-decisions":
			// Decision Traces show only the first 20 entries (most recent,
			// newest-first); the BFF must bound the upstream fetch to 20.
			if got := r.URL.Query().Get("limit"); got != "20" {
				t.Errorf("governance decisions: want limit=20 (first 20 entries), got limit=%q", got)
			}
			body := upstream.AgentDecisionsResponse{
				Items: []upstream.AgentDecisionRow{
					{LogID: "log-1", AgentID: "qgen_question", TraceID: "0af7651916cd43dd8448eb211c80319c", CreatedAt: time.Now().UTC()},
				},
			}
			_ = json.NewEncoder(w).Encode(body)
		case r.URL.Path == "/api/v1/observability/agent-decisions/count":
			body := upstream.AgentDecisionCountResponse{
				Count: 42,
				Since: time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339),
				Until: time.Now().UTC().Format(time.RFC3339),
			}
			_ = json.NewEncoder(w).Encode(body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// newA2AHTTPMock serves the chora-a2a endpoints.
func newA2AHTTPMock(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/a2a/contracts":
			_ = json.NewEncoder(w).Encode([]upstream.A2AContract{{ContractID: "c-1", TenantID: "t1"}})
		case "/api/v1/a2a/identities":
			_ = json.NewEncoder(w).Encode([]upstream.A2AIdentity{{AGID: "agid-1"}})
		case "/api/v1/a2a/invocations":
			_ = json.NewEncoder(w).Encode([]upstream.A2AInvocation{{InvocationID: "inv-1", TenantID: "t1"}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// newOPlusHandlerForTest wires the handler against the mock servers + fake
// gRPC. Returns the handler + the cleanup func.
func newOPlusHandlerForTest(t *testing.T, opts ...func(*httpadapter.OPlusHandler)) *httpadapter.OPlusHandler {
	t.Helper()
	govHTTP := newGovernanceHTTPMock(t)
	t.Cleanup(govHTTP.Close)
	obsHTTP := newObservabilityHTTPMock(t)
	t.Cleanup(obsHTTP.Close)
	a2aHTTP := newA2AHTTPMock(t)
	t.Cleanup(a2aHTTP.Close)

	govClient := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{HTTPAddr: govHTTP.URL, PerCallTimeout: 2 * time.Second},
		RPC: &stubGovernanceGRPC{
			dash: dashboardFixture(),
			audit: &governancev1.QueryAuditEventsResponse{
				Events: []*governancev1.AuditEvent{
					{EventId: "evt-1", Decision: governancev1.AuditDecision_AUDIT_DECISION_PERMITTED, CreatedAt: timestamppb.Now()},
				},
			},
		},
	})
	obsClient := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: obsHTTP.URL, PerCallTimeout: 2 * time.Second,
	}, nil)
	a2aClient := upstream.NewA2AClient(upstream.A2AConfig{
		HTTPAddr: a2aHTTP.URL, PerCallTimeout: 2 * time.Second,
	}, nil)

	h := httpadapter.NewOPlusHandler(govClient, obsClient, a2aClient, nil)
	for _, opt := range opts {
		opt(h)
	}
	return h
}

func TestOPlusHandler_Dashboard_Happy(t *testing.T) {
	h := newOPlusHandlerForTest(t)
	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/dashboard", nil)
	rr := httptest.NewRecorder()
	h.Dashboard(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["state"] != "live" {
		t.Errorf("state = %v; want live", body["state"])
	}
	// Reshaped per audit finding #3 — `posture[]` with 4 typed rows.
	posture, ok := body["posture"].([]any)
	if !ok || len(posture) != 4 {
		t.Fatalf("posture = %v; want 4-array", body["posture"])
	}
	// First entry must be D1 = accountability per canonical IMDA order.
	first, _ := posture[0].(map[string]any)
	if first["num"].(float64) != 1 {
		t.Errorf("posture[0].num = %v; want 1", first["num"])
	}
	if first["label"] != "accountability" {
		t.Errorf("posture[0].label = %v; want accountability", first["label"])
	}
	if first["score"].(float64) != 72 {
		t.Errorf("posture[0].score = %v; want 72", first["score"])
	}
	// score=72 → partial per audit-finding thresholds (60 ≤ 72 < 85).
	if first["status"] != "partial" {
		t.Errorf("posture[0].status = %v; want partial", first["status"])
	}
	// safety_risks deliberately omitted from the BFF body — the 6 ISO
	// 25059:2023 risk tiles are a FE design-time invariant rendered from
	// the chora-web `SAFETY_RISK_CATEGORIES` constant, not from upstream
	// data. The field must not appear in the dashboard payload.
	if _, present := body["safety_risks"]; present {
		t.Errorf("safety_risks should be omitted from BFF dashboard; got %v", body["safety_risks"])
	}
	// all_baseline_achieved must be present (scores are 72/58/81/64 — none ≥85).
	achieved, ok := body["all_baseline_achieved"].(bool)
	if !ok || achieved {
		t.Errorf("all_baseline_achieved = %v; want false", body["all_baseline_achieved"])
	}

	// recent_decisions_24h is populated from chora-observability when the
	// upstream call succeeds. Mock returns Count=42 — the FE-facing field
	// must reflect it.
	recent, ok := body["recent_decisions_24h"].(float64)
	if !ok {
		t.Fatalf("recent_decisions_24h missing or non-numeric: %v", body["recent_decisions_24h"])
	}
	if int(recent) != 42 {
		t.Errorf("recent_decisions_24h = %v; want 42", recent)
	}
}

// TestOPlusHandler_Dashboard_RecentDecisionsDegradedOnError verifies that
// when chora-observability's CountAgentDecisions fails, the dashboard
// envelope still returns state:'live' with the other fields populated and
// recent_decisions_24h omitted (NOT collapsed to state:'error' or null).
// Per [[feedback-no-stubs-real-wiring]] — graceful degradation, error
// logged but not surfaced to the FE envelope.
func TestOPlusHandler_Dashboard_RecentDecisionsDegradedOnError(t *testing.T) {
	govHTTP := newGovernanceHTTPMock(t)
	t.Cleanup(govHTTP.Close)
	// Observability mock returns 500 on the count endpoint, OK on other paths.
	obsHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/observability/agent-decisions/count" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(obsHTTP.Close)
	a2aHTTP := newA2AHTTPMock(t)
	t.Cleanup(a2aHTTP.Close)

	govClient := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{HTTPAddr: govHTTP.URL, PerCallTimeout: 2 * time.Second},
		RPC:      &stubGovernanceGRPC{dash: dashboardFixture()},
	})
	obsClient := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: obsHTTP.URL, PerCallTimeout: 2 * time.Second,
	}, nil)
	a2aClient := upstream.NewA2AClient(upstream.A2AConfig{
		HTTPAddr: a2aHTTP.URL, PerCallTimeout: 2 * time.Second,
	}, nil)
	h := httpadapter.NewOPlusHandler(govClient, obsClient, a2aClient, nil)

	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/dashboard", nil)
	rr := httptest.NewRecorder()
	h.Dashboard(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (degraded, not collapsed)", rr.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["state"] != "live" {
		t.Errorf("state = %v; want live (degradation must not affect envelope state)", body["state"])
	}
	// recent_decisions_24h must be ABSENT from the JSON (omitempty pointer).
	if _, has := body["recent_decisions_24h"]; has {
		t.Errorf("recent_decisions_24h present despite upstream failure: %v", body["recent_decisions_24h"])
	}
	// Other fields must still be populated.
	if _, ok := body["posture"].([]any); !ok {
		t.Error("posture missing — only the count field should degrade")
	}
}

// TestOPlusHandler_Dashboard_RecentDecisionsHonestZero verifies the
// `recent_decisions_24h` field is populated with 0 when the upstream
// successfully reports zero rows. Per [[feedback-no-stubs-real-wiring]] —
// zero is honest, not a stub; omitting the field is reserved for upstream
// failures (the prior test).
func TestOPlusHandler_Dashboard_RecentDecisionsHonestZero(t *testing.T) {
	govHTTP := newGovernanceHTTPMock(t)
	t.Cleanup(govHTTP.Close)
	obsHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/observability/agent-decisions/count" {
			body := upstream.AgentDecisionCountResponse{Count: 0, Since: "x", Until: "y"}
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(obsHTTP.Close)

	govClient := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{HTTPAddr: govHTTP.URL, PerCallTimeout: 2 * time.Second},
		RPC:      &stubGovernanceGRPC{dash: dashboardFixture()},
	})
	obsClient := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: obsHTTP.URL, PerCallTimeout: 2 * time.Second,
	}, nil)
	h := httpadapter.NewOPlusHandler(govClient, obsClient, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/dashboard", nil)
	rr := httptest.NewRecorder()
	h.Dashboard(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var body map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&body)
	recent, ok := body["recent_decisions_24h"].(float64)
	if !ok {
		t.Fatalf("recent_decisions_24h missing on zero-row honest path: %v", body["recent_decisions_24h"])
	}
	if int(recent) != 0 {
		t.Errorf("recent_decisions_24h = %v; want 0 (honest zero)", recent)
	}
}

func TestOPlusHandler_Dimensions_FansOutAll4(t *testing.T) {
	h := newOPlusHandlerForTest(t)
	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/dimensions", nil)
	rr := httptest.NewRecorder()
	h.Dimensions(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	dims, _ := body["dimensions"].([]any)
	if len(dims) != 4 {
		t.Errorf("dimensions = %d; want 4", len(dims))
	}
	// Per audit finding #5 — dimensions carry `num`, `label`, `score`,
	// `status`, `rubric_items[]` with `ref`/`requirement`/`tools[]`.
	expectedOrder := []string{"accountability", "transparency", "safety_and_robustness", "fairness_and_human_oversight"}
	for i, want := range expectedOrder {
		got, _ := dims[i].(map[string]any)
		if got["label"] != want {
			t.Errorf("dimensions[%d].label = %v; want %s", i, got["label"], want)
		}
		if got["num"].(float64) != float64(i+1) {
			t.Errorf("dimensions[%d].num = %v; want %d", i, got["num"], i+1)
		}
		rubric, _ := got["rubric_items"].([]any)
		if len(rubric) != 3 {
			t.Errorf("dimensions[%d].rubric_items = %d; want 3", i, len(rubric))
		}
		// Spot-check the first rubric item has the renamed fields.
		if len(rubric) > 0 {
			ri, _ := rubric[0].(map[string]any)
			if _, has := ri["ref"]; !has {
				t.Errorf("dimensions[%d].rubric_items[0] missing `ref`: %+v", i, ri)
			}
			if _, has := ri["requirement"]; !has {
				t.Errorf("dimensions[%d].rubric_items[0] missing `requirement`: %+v", i, ri)
			}
			if _, has := ri["tools"]; !has {
				t.Errorf("dimensions[%d].rubric_items[0] missing `tools`: %+v", i, ri)
			}
			// Status must be lowercase per FE union.
			if status, _ := ri["status"].(string); status != strings.ToLower(status) {
				t.Errorf("dimensions[%d].rubric_items[0].status = %v; want lowercase", i, status)
			}
		}
	}
}

func TestOPlusHandler_Agents_PassThrough(t *testing.T) {
	h := newOPlusHandlerForTest(t)
	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/agents", nil)
	rr := httptest.NewRecorder()
	h.Agents(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var body map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body["state"] != "live" {
		t.Errorf("state = %v; want live", body["state"])
	}
	crews, _ := body["crews"].([]any)
	if len(crews) != 1 {
		t.Fatalf("crews = %d; want 1", len(crews))
	}
	first, _ := crews[0].(map[string]any)
	if first["crew_name"] != "mcq_ai_assist" {
		t.Errorf("crew_name = %v", first["crew_name"])
	}
	// The crew-level has_recent_activity flag must survive the BFF re-marshal
	// through the typed upstream.Crew struct — it backs the /o/agents banner.
	if first["has_recent_activity"] != true {
		t.Errorf("has_recent_activity = %v; want true (must round-trip the typed pass-through)", first["has_recent_activity"])
	}
	agents, _ := first["agents"].([]any)
	if len(agents) != 2 {
		t.Errorf("agents = %d; want 2", len(agents))
	}
}

// TestOPlusHandler_AgentPrompts_PassThrough proves /bff/oplus/prompts wraps
// the observability agent-prompts body in the live envelope and passes the
// agents array through RAW: every upstream field survives, including one the
// BFF has never heard of (the has_recent_activity lesson, by design).
func TestOPlusHandler_AgentPrompts_PassThrough(t *testing.T) {
	h := newOPlusHandlerForTest(t)
	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/prompts", nil)
	rr := httptest.NewRecorder()
	h.AgentPrompts(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var body map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body["state"] != "live" {
		t.Errorf("state = %v; want live", body["state"])
	}
	agents, _ := body["agents"].([]any)
	if len(agents) != 5 {
		t.Fatalf("agents = %d; want 5", len(agents))
	}
	first, _ := agents[0].(map[string]any)
	if first["agent_id"] != "qgen_question" || first["evidence_kind"] != "decisions" {
		t.Errorf("agents[0] = %#v", first)
	}
	if first["decisions_total"].(float64) != 4 || first["latest_prompt_version"] != "v4" {
		t.Errorf("agents[0] totals/latest = %v/%v", first["decisions_total"], first["latest_prompt_version"])
	}
	if first["future_field"] != "kept" {
		t.Errorf("future_field dropped by the pass-through: %#v", first)
	}
	ucs, _ := first["use_cases"].([]any)
	if len(ucs) != 3 {
		t.Errorf("agents[0].use_cases = %d; want 3", len(ucs))
	}
	last, _ := agents[4].(map[string]any)
	if last["agent_id"] != "companion" || last["evidence_kind"] != "ritual_stamps" ||
		last["runs_total"].(float64) != 3 {
		t.Errorf("agents[4] = %#v", last)
	}
}

func TestOPlusHandler_AgentPrompts_RejectsNonGET(t *testing.T) {
	h := newOPlusHandlerForTest(t)
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/prompts", nil)
	rr := httptest.NewRecorder()
	h.AgentPrompts(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405", rr.Code)
	}
}

func TestOPlusHandler_AgentPrompts_UpstreamError(t *testing.T) {
	// Observability points at a server that returns 500.
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()

	govStub := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{PerCallTimeout: 1 * time.Second},
		RPC:      &stubGovernanceGRPC{dash: dashboardFixture()},
	})
	obsClient := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: failing.URL, PerCallTimeout: 1 * time.Second,
	}, nil)
	h := httpadapter.NewOPlusHandler(govStub, obsClient, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/prompts", nil)
	rr := httptest.NewRecorder()
	h.AgentPrompts(rr, req)
	// Per the handler contract, upstream errors collapse to a 200 with
	// state:'error' so the FE renders the discriminated-union variant.
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 with state:'error'", rr.Code)
	}
	var body map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body["state"] != "error" {
		t.Errorf("state = %v; want error", body["state"])
	}
}

func TestOPlusHandler_Governance3Tab_AllThreeTabsPopulated(t *testing.T) {
	h := newOPlusHandlerForTest(t)
	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/governance", nil)
	rr := httptest.NewRecorder()
	h.Governance3Tab(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var body map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body["state"] != "live" {
		t.Errorf("state = %v", body["state"])
	}
	// Per audit finding #1 — decisions must be reshaped to FE-canonical view.
	decs, _ := body["decisions"].([]any)
	if len(decs) < 1 {
		t.Fatalf("decisions empty: %+v", body["decisions"])
	}
	firstDec, _ := decs[0].(map[string]any)
	wantFields := []string{"id", "workflow_id", "agent", "agent_slug", "decision_type", "model", "cost", "timestamp", "trace_id"}
	for _, f := range wantFields {
		if _, has := firstDec[f]; !has {
			t.Errorf("decisions[0] missing %q field: %+v", f, firstDec)
		}
	}
	// agent_slug must be lowercase-hyphenated of agent_id.
	if firstDec["agent_slug"] != "qgen-question" {
		t.Errorf("decisions[0].agent_slug = %v; want qgen-question", firstDec["agent_slug"])
	}
	// hitl_status + confidence are intentionally ABSENT from the Decisions
	// table view: these rows are autonomous agent decisions (the real HITL
	// review queue is the separate Oversight tab) and agent_decision_log has
	// no confidence field. The columns were removed per the O+ auditor review.
	if _, has := firstDec["hitl_status"]; has {
		t.Error("decisions[0].hitl_status must be removed from the Decisions view")
	}
	if _, has := firstDec["confidence"]; has {
		t.Error("decisions[0].confidence must be removed from the Decisions view")
	}
	// Data-currency window is surfaced for auditors (last 90 days, not 24h).
	if body["window_days"] != float64(90) {
		t.Errorf("window_days = %v; want 90 (last-90-days currency window)", body["window_days"])
	}

	// Per audit finding #2 — hitl_pending must be reshaped too.
	hitl, _ := body["hitl_pending"].([]any)
	if len(hitl) < 1 {
		t.Fatalf("hitl_pending empty: %+v", body["hitl_pending"])
	}
	firstHITL, _ := hitl[0].(map[string]any)
	hitlFields := []string{"id", "workflow_id", "gate", "agent", "summary", "autonomy_level", "waiting_since", "assignee"}
	for _, f := range hitlFields {
		if _, has := firstHITL[f]; !has {
			t.Errorf("hitl_pending[0] missing %q field: %+v", f, firstHITL)
		}
	}
	// Gate defaults to material_decision when decision_kind is empty.
	if firstHITL["gate"] != "material_decision" {
		t.Errorf("hitl_pending[0].gate = %v; want material_decision", firstHITL["gate"])
	}

	// data_lineage must be the FE-canonical ARRAY of GovernanceLineageEntry
	// rows (governance.service.ts) — NOT the legacy `{domains:[...]}` object,
	// which the FE never consumed (Data Governance table rendered 0 rows).
	lineage, ok := body["data_lineage"].([]any)
	if !ok || len(lineage) < 1 {
		t.Fatalf("data_lineage not a non-empty array: %#v", body["data_lineage"])
	}
	// Every row must carry the exact GovernanceLineageEntry fields with a
	// classification drawn from the FE enum.
	validClass := map[string]bool{"Audit": true, "Knowledge": true, "PII": true, "Operational": true}
	lineageFields := []string{"domain", "db", "table", "retention", "classification", "pii_closure_map"}
	for i, raw := range lineage {
		row, isMap := raw.(map[string]any)
		if !isMap {
			t.Fatalf("data_lineage[%d] not an object: %#v", i, raw)
		}
		for _, f := range lineageFields {
			if _, has := row[f]; !has {
				t.Errorf("data_lineage[%d] missing %q field: %#v", i, f, row)
			}
		}
		cls, _ := row["classification"].(string)
		if !validClass[cls] {
			t.Errorf("data_lineage[%d].classification = %q; not in FE enum {Audit,Knowledge,PII,Operational}", i, cls)
		}
		if _, isBool := row["pii_closure_map"].(bool); !isBool {
			t.Errorf("data_lineage[%d].pii_closure_map not a bool: %#v", i, row["pii_closure_map"])
		}
	}
	// Representative grounded rows: chora_identity GCID is PII with a closure
	// map; chora_observability TokenUsageLedger is Audit; both anchor the
	// 13-DB topology the lineage is grounded against.
	// ⚠ These table names are REAL and were verified against the live schema on
	// 2026-08-23. This assertion previously named "global_identities", a table
	// that does not exist, so the test held the fiction in place instead of
	// catching it. Any name asserted here must exist in that database.
	var sawIdentityPII, sawObservabilityAudit bool
	for _, raw := range lineage {
		row, _ := raw.(map[string]any)
		switch row["table"] {
		case "users":
			if row["db"] == "chora_identity" && row["classification"] == "PII" && row["pii_closure_map"] == true {
				sawIdentityPII = true
			}
		case "token_usage_ledger":
			if row["db"] == "chora_observability" && row["classification"] == "Audit" {
				sawObservabilityAudit = true
			}
		}
	}
	if !sawIdentityPII {
		t.Error("data_lineage missing the chora_identity/users PII row (closure-map true)")
	}
	if !sawObservabilityAudit {
		t.Error("data_lineage missing the chora_observability/token_usage_ledger Audit row")
	}
}

func TestOPlusHandler_A2A_LiveMode(t *testing.T) {
	h := newOPlusHandlerForTest(t)
	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/a2a", nil)
	rr := httptest.NewRecorder()
	h.A2APanel(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var body map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body["mode"] != "live" {
		t.Errorf("mode = %v; want live", body["mode"])
	}
	if body["state"] != "live" {
		t.Errorf("state = %v; want live", body["state"])
	}
	// Per audit finding #4 — contracts reshaped with renamed fields.
	contracts, ok := body["contracts"].([]any)
	if !ok || len(contracts) < 1 {
		t.Fatalf("contracts missing or empty: %+v", body["contracts"])
	}
	firstC, _ := contracts[0].(map[string]any)
	if _, has := firstC["id"]; !has {
		t.Errorf("contracts[0] missing `id`: %+v", firstC)
	}
	if _, has := firstC["scope"]; !has {
		t.Errorf("contracts[0] missing `scope`: %+v", firstC)
	}
	// Per audit finding #4 — identities renamed to external_agents.
	agents, ok := body["external_agents"].([]any)
	if !ok || len(agents) < 1 {
		t.Fatalf("external_agents missing or empty: %+v", body["external_agents"])
	}
	firstA, _ := agents[0].(map[string]any)
	for _, f := range []string{"agid", "partner", "trust_level", "key_fingerprint", "rotated_at"} {
		if _, has := firstA[f]; !has {
			t.Errorf("external_agents[0] missing %q: %+v", f, firstA)
		}
	}
	// identities (old name) MUST NOT be present.
	if _, has := body["identities"]; has {
		t.Error("identities key still present — should be renamed to external_agents")
	}
	// invocations reshaped with id/status/endpoint/latency.
	invs, ok := body["invocations"].([]any)
	if !ok || len(invs) < 1 {
		t.Fatalf("invocations missing or empty")
	}
	firstInv, _ := invs[0].(map[string]any)
	for _, f := range []string{"id", "contract_id", "agid", "partner", "endpoint", "status", "latency_ms", "timestamp"} {
		if _, has := firstInv[f]; !has {
			t.Errorf("invocations[0] missing %q: %+v", f, firstInv)
		}
	}
}

func TestOPlusHandler_A2A_PendingMode(t *testing.T) {
	// Wire ONLY governance + observability — a2a client is pending.
	a2aClient := upstream.NewA2AClient(upstream.A2AConfig{}, nil)
	govStub := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{PerCallTimeout: 1 * time.Second},
		RPC:      &stubGovernanceGRPC{dash: dashboardFixture()},
	})
	obsClient := upstream.NewObservabilityClient(upstream.ObservabilityConfig{HTTPAddr: "http://nowhere"}, nil)
	h := httpadapter.NewOPlusHandler(govStub, obsClient, a2aClient, nil)

	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/a2a", nil)
	rr := httptest.NewRecorder()
	h.A2APanel(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var body map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body["mode"] != "pending" {
		t.Errorf("mode = %v; want pending", body["mode"])
	}
	if body["state"] != "pending" {
		t.Errorf("state = %v; want pending", body["state"])
	}
	if body["mock"] == nil {
		t.Error("mock payload missing in pending mode")
	}
}

func TestOPlusHandler_Dashboard_RejectsNonGET(t *testing.T) {
	h := newOPlusHandlerForTest(t)
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/dashboard", nil)
	rr := httptest.NewRecorder()
	h.Dashboard(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405", rr.Code)
	}
}

func TestOPlusHandler_Agents_UpstreamError(t *testing.T) {
	// Observability points at a server that returns 500.
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()

	govStub := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{PerCallTimeout: 1 * time.Second},
		RPC:      &stubGovernanceGRPC{dash: dashboardFixture()},
	})
	obsClient := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: failing.URL, PerCallTimeout: 1 * time.Second,
	}, nil)
	h := httpadapter.NewOPlusHandler(govStub, obsClient, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/agents", nil)
	rr := httptest.NewRecorder()
	h.Agents(rr, req)
	// Per the handler contract, upstream errors collapse to a 200 with
	// `state:'error'` so the FE renders the discriminated-union variant.
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 with state:'error'", rr.Code)
	}
	var body map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body["state"] != "error" {
		t.Errorf("state = %v; want error", body["state"])
	}
}

func TestRegisterOPlusRoutes_AllPathsRegistered(t *testing.T) {
	h := newOPlusHandlerForTest(t)
	mux := http.NewServeMux()
	httpadapter.RegisterOPlusRoutes(mux, h)

	paths := []string{
		"/bff/oplus/dashboard",
		"/bff/oplus/dimensions",
		"/bff/oplus/agents",
		"/bff/oplus/prompts",
		// CHO-2368 catalogue subtree — unconfigured AIKernel upstream still
		// answers 200 (state:error envelope), so the contract check holds.
		"/bff/oplus/prompts/qgen_question/versions",
		"/bff/oplus/governance",
		"/bff/oplus/a2a",
		"/bff/oplus/eval-runs",
	}
	for _, p := range paths {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("%s: status = %d; want 200", p, rr.Code)
		}
	}
}

// newCostsObservabilityMock serves the three chora-observability cost
// endpoints the /bff/oplus/costs handler aggregates. Cost stored as int64
// MICROS upstream (the values below are deliberately not round dollars so the
// micros→USD division is exercised).
func newCostsObservabilityMock(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/token-usage/aggregate":
			switch r.URL.Query().Get("group_by") {
			case "model":
				_ = json.NewEncoder(w).Encode(upstream.TokenUsageAggregateResponse{
					Groups: []upstream.TokenUsageAggregateGroup{
						{Group: "gemini-2.5-flash-lite", TotalCostUsdMicros: 1_234_500, PromptTokens: 1200, CompletionTokens: 340, EntryCount: 7},
						{Group: "gemini-2.5-pro", TotalCostUsdMicros: 9_870_000, PromptTokens: 4000, CompletionTokens: 900, EntryCount: 3},
					},
				})
			case "agent":
				_ = json.NewEncoder(w).Encode(upstream.TokenUsageAggregateResponse{
					Groups: []upstream.TokenUsageAggregateGroup{
						{Group: "qgen_question", TotalCostUsdMicros: 500_000, PromptTokens: 800, CompletionTokens: 120, EntryCount: 4},
					},
				})
			default:
				_ = json.NewEncoder(w).Encode(upstream.TokenUsageAggregateResponse{Groups: []upstream.TokenUsageAggregateGroup{}})
			}
		case "/api/cost/cumulative":
			_ = json.NewEncoder(w).Encode(upstream.CostCumulativeResponse{
				TotalCostUsdMicros: 11_104_500, // = 1_234_500 + 9_870_000
				TotalCostUsd:       "$11.10",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestOPlusHandler_Costs_Happy(t *testing.T) {
	obsHTTP := newCostsObservabilityMock(t)
	t.Cleanup(obsHTTP.Close)
	obsClient := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: obsHTTP.URL, PerCallTimeout: 2 * time.Second,
	}, nil)
	// Governance + A2A intentionally nil — the costs route consults neither.
	h := httpadapter.NewOPlusHandler(nil, obsClient, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/costs", nil)
	rr := httptest.NewRecorder()
	h.Costs(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200, body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Raw object — NOT wrapped in a {state,data} envelope. There must be no
	// `state` discriminator and no `data` nesting.
	if _, has := body["state"]; has {
		t.Errorf("costs response must NOT carry a `state` discriminator: %+v", body)
	}
	if _, has := body["data"]; has {
		t.Errorf("costs response must NOT be wrapped in `data`: %+v", body)
	}
	if _, has := body["fetched_at"]; !has {
		t.Error("fetched_at missing")
	}
	// cumulative_cost_usd = 11_104_500 micros / 1e6 = 11.1045 USD.
	cum, ok := body["cumulative_cost_usd"].(float64)
	if !ok {
		t.Fatalf("cumulative_cost_usd missing/non-numeric: %v", body["cumulative_cost_usd"])
	}
	if cum < 11.1044 || cum > 11.1046 {
		t.Errorf("cumulative_cost_usd = %v; want ~11.1045 (micros→USD)", cum)
	}
	// by_model — 2 rows, first is gemini-2.5-flash-lite @ 1_234_500 micros = 1.2345 USD.
	byModel, ok := body["by_model"].([]any)
	if !ok || len(byModel) != 2 {
		t.Fatalf("by_model = %v; want 2-array", body["by_model"])
	}
	m0, _ := byModel[0].(map[string]any)
	if m0["key"] != "gemini-2.5-flash-lite" {
		t.Errorf("by_model[0].key = %v; want gemini-2.5-flash-lite", m0["key"])
	}
	cost0, _ := m0["cost_usd"].(float64)
	if cost0 < 1.2344 || cost0 > 1.2346 {
		t.Errorf("by_model[0].cost_usd = %v; want ~1.2345 (micros→USD)", cost0)
	}
	if pt, _ := m0["prompt_tokens"].(float64); int(pt) != 1200 {
		t.Errorf("by_model[0].prompt_tokens = %v; want 1200", m0["prompt_tokens"])
	}
	if ct, _ := m0["completion_tokens"].(float64); int(ct) != 340 {
		t.Errorf("by_model[0].completion_tokens = %v; want 340", m0["completion_tokens"])
	}
	// by_agent — 1 row, qgen_question @ 500_000 micros = 0.5 USD.
	byAgent, ok := body["by_agent"].([]any)
	if !ok || len(byAgent) != 1 {
		t.Fatalf("by_agent = %v; want 1-array", body["by_agent"])
	}
	a0, _ := byAgent[0].(map[string]any)
	if a0["key"] != "qgen_question" {
		t.Errorf("by_agent[0].key = %v; want qgen_question", a0["key"])
	}
	if costA, _ := a0["cost_usd"].(float64); costA != 0.5 {
		t.Errorf("by_agent[0].cost_usd = %v; want 0.5", a0["cost_usd"])
	}
}

func TestOPlusHandler_Costs_ObservabilityUnreachable_503(t *testing.T) {
	// Observability server returns 500 on every cost endpoint.
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()
	obsClient := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: failing.URL, PerCallTimeout: 1 * time.Second,
	}, nil)
	h := httpadapter.NewOPlusHandler(nil, obsClient, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/costs", nil)
	rr := httptest.NewRecorder()
	h.Costs(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d; want 503 (no envelope collapse on cost panel)", rr.Code)
	}
}

func TestOPlusHandler_Costs_ObservabilityUnwired_503(t *testing.T) {
	h := httpadapter.NewOPlusHandler(nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/costs", nil)
	rr := httptest.NewRecorder()
	h.Costs(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d; want 503 (observability client not wired)", rr.Code)
	}
}

func TestOPlusHandler_Costs_RejectsNonGET(t *testing.T) {
	h := httpadapter.NewOPlusHandler(nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/costs", nil)
	rr := httptest.NewRecorder()
	h.Costs(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405", rr.Code)
	}
}

// newRecordingCostsObservabilityMock captures the `from`/`to` query each cost
// endpoint receives so the /bff/oplus/costs `?range=` → window forwarding can
// be asserted. Returns minimal valid (empty) bodies — the rollup values aren't
// the subject under test here.
func newRecordingCostsObservabilityMock(t *testing.T, gotFrom, gotTo *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cost/cumulative" {
			*gotFrom = r.URL.Query().Get("from")
			*gotTo = r.URL.Query().Get("to")
		}
		switch r.URL.Path {
		case "/api/token-usage/aggregate":
			_ = json.NewEncoder(w).Encode(upstream.TokenUsageAggregateResponse{Groups: []upstream.TokenUsageAggregateGroup{}})
		case "/api/cost/cumulative":
			_ = json.NewEncoder(w).Encode(upstream.CostCumulativeResponse{TotalCostUsdMicros: 0})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestOPlusHandler_Costs_Range_Month_ForwardsWindow — `?range=month` forwards a
// rolling 30-day window (anchored at the injected clock) to observability.
func TestOPlusHandler_Costs_Range_Month_ForwardsWindow(t *testing.T) {
	var gotFrom, gotTo string
	obsHTTP := newRecordingCostsObservabilityMock(t, &gotFrom, &gotTo)
	t.Cleanup(obsHTTP.Close)
	obsClient := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: obsHTTP.URL, PerCallTimeout: 2 * time.Second,
	}, nil)
	h := httpadapter.NewOPlusHandler(nil, obsClient, nil, nil)
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	h.Now = func() time.Time { return now }

	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/costs?range=month", nil)
	rr := httptest.NewRecorder()
	h.Costs(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	wantFrom := now.Add(-30 * 24 * time.Hour).Format(time.RFC3339) // 2026-05-30T12:00:00Z
	wantTo := now.Format(time.RFC3339)                             // 2026-06-29T12:00:00Z
	if gotFrom != wantFrom {
		t.Errorf("from = %q; want %q (rolling 30d)", gotFrom, wantFrom)
	}
	if gotTo != wantTo {
		t.Errorf("to = %q; want %q (now)", gotTo, wantTo)
	}
}

// TestOPlusHandler_Costs_DefaultRange_AllTime_NoWindow — no `range` (== all)
// sends NO from/to, preserving the pre-feature all-time behaviour.
func TestOPlusHandler_Costs_DefaultRange_AllTime_NoWindow(t *testing.T) {
	var gotFrom, gotTo string
	obsHTTP := newRecordingCostsObservabilityMock(t, &gotFrom, &gotTo)
	t.Cleanup(obsHTTP.Close)
	obsClient := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: obsHTTP.URL, PerCallTimeout: 2 * time.Second,
	}, nil)
	h := httpadapter.NewOPlusHandler(nil, obsClient, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/costs", nil)
	rr := httptest.NewRecorder()
	h.Costs(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if gotFrom != "" || gotTo != "" {
		t.Errorf("default (all-time) must send no window; got from=%q to=%q", gotFrom, gotTo)
	}
}

// TestOPlusHandler_Costs_RejectsBadRange — an unrecognised token is a loud 400,
// never a silent widen to all-time.
func TestOPlusHandler_Costs_RejectsBadRange(t *testing.T) {
	h := httpadapter.NewOPlusHandler(nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/costs?range=year", nil)
	rr := httptest.NewRecorder()
	h.Costs(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 on unrecognised range", rr.Code)
	}
}

// TestOPlusHandler_Costs_AppliesDisplayCurrency — the configured display
// currency + FX rate are surfaced as metadata; the `*_usd` amounts stay
// CANONICAL USD (conversion is a presentation concern done FE-side).
func TestOPlusHandler_Costs_AppliesDisplayCurrency(t *testing.T) {
	obsHTTP := newCostsObservabilityMock(t)
	t.Cleanup(obsHTTP.Close)
	obsClient := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: obsHTTP.URL, PerCallTimeout: 2 * time.Second,
	}, nil)
	h := httpadapter.NewOPlusHandler(nil, obsClient, nil, nil)
	h.DisplayCurrency = "SGD"
	h.FxRate = 1.35

	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/costs", nil)
	rr := httptest.NewRecorder()
	h.Costs(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["currency"] != "SGD" {
		t.Errorf("currency = %v; want SGD", body["currency"])
	}
	if fx, _ := body["fx_rate"].(float64); fx != 1.35 {
		t.Errorf("fx_rate = %v; want 1.35", body["fx_rate"])
	}
	// Amounts must remain unconverted USD (~11.1045 from the mock micros).
	if cum, _ := body["cumulative_cost_usd"].(float64); cum < 11.1044 || cum > 11.1046 {
		t.Errorf("cumulative_cost_usd = %v; want ~11.1045 (unconverted USD)", cum)
	}
}

// TestOPlusHandler_Costs_DefaultsToUSDRate1 — out of the box (no config) the
// panel reports USD @ 1.0, preserving the pre-feature behaviour.
func TestOPlusHandler_Costs_DefaultsToUSDRate1(t *testing.T) {
	obsHTTP := newCostsObservabilityMock(t)
	t.Cleanup(obsHTTP.Close)
	obsClient := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: obsHTTP.URL, PerCallTimeout: 2 * time.Second,
	}, nil)
	h := httpadapter.NewOPlusHandler(nil, obsClient, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/costs", nil)
	rr := httptest.NewRecorder()
	h.Costs(rr, req)
	var body map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["currency"] != "USD" {
		t.Errorf("currency = %v; want USD default", body["currency"])
	}
	if fx, _ := body["fx_rate"].(float64); fx != 1.0 {
		t.Errorf("fx_rate = %v; want 1.0 default", body["fx_rate"])
	}
}

// TestLoadCostDisplayConfigFromEnv — env → (currency, rate), with safe
// defaults on empty/garbage (no panic, no zero rate).
func TestLoadCostDisplayConfigFromEnv(t *testing.T) {
	t.Setenv("OPLUS_COST_DISPLAY_CURRENCY", "SGD")
	t.Setenv("OPLUS_COST_FX_RATE", "1.35")
	cur, rate := httpadapter.LoadCostDisplayConfigFromEnv()
	if cur != "SGD" || rate != 1.35 {
		t.Errorf("got (%q,%v); want (SGD,1.35)", cur, rate)
	}
}

func TestLoadCostDisplayConfigFromEnv_DefaultsAndBadRate(t *testing.T) {
	t.Setenv("OPLUS_COST_DISPLAY_CURRENCY", "")
	t.Setenv("OPLUS_COST_FX_RATE", "not-a-number")
	cur, rate := httpadapter.LoadCostDisplayConfigFromEnv()
	if cur != "USD" || rate != 1.0 {
		t.Errorf("got (%q,%v); want (USD,1.0) on empty/bad input", cur, rate)
	}
}
