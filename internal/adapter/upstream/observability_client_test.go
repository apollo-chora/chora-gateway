// observability_client_test.go — Phase C tests for the chora-observability
// HTTP client.
package upstream_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

func TestObservabilityClient_GetAgents_Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/observability/agents" {
			t.Errorf("path = %s; want /api/v1/observability/agents", r.URL.Path)
		}
		if r.URL.Query().Get("tenant_id") != "t1" {
			t.Errorf("tenant_id = %s; want t1", r.URL.Query().Get("tenant_id"))
		}
		body := upstream.AgentsResponse{
			Crews: []upstream.Crew{
				{
					CrewName: "mcq_ai_assist",
					CrewID:   "crew-mcq",
					Agents: []upstream.Agent{
						{AgentID: "qgen_question", EngineID: "8635637442075951104"},
						{AgentID: "qgen_critic", EngineID: "2658824174880948224"},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second,
	}, nil)
	resp, err := oc.GetAgents(context.Background(), "t1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(resp.Crews) != 1 {
		t.Fatalf("crews = %d; want 1", len(resp.Crews))
	}
	if resp.Crews[0].CrewName != "mcq_ai_assist" {
		t.Errorf("crew name = %s", resp.Crews[0].CrewName)
	}
	if len(resp.Crews[0].Agents) != 2 {
		t.Errorf("agents = %d; want 2", len(resp.Crews[0].Agents))
	}
}

func TestObservabilityClient_GetAgents_NoAddr(t *testing.T) {
	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{}, nil)
	if _, err := oc.GetAgents(context.Background(), "t1"); !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("err = %v; want ErrUpstream wrap", err)
	}
}

// TestObservabilityClient_GetAgents_PreservesHasRecentActivity proves the
// crew-level has_recent_activity flag obs emits survives the BFF unmarshal onto
// the typed upstream.Crew struct. Because the BFF re-marshals through this
// struct, the field is DROPPED unless it exists here — which was the /o/agents
// "no recent activity" banner bug.
func TestObservabilityClient_GetAgents_PreservesHasRecentActivity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"crews":[
			{"crew_name":"qgen","crew_id":"qgen","has_recent_activity":true,
			 "agents":[{"agent_id":"qgen-mcq","stats":{"invocations_24h":3}}]},
			{"crew_name":"oe_grading","crew_id":"oe_grading","has_recent_activity":false,
			 "agents":[{"agent_id":"oe_evaluator","stats":{"invocations_24h":0}}]}
		]}`))
	}))
	defer srv.Close()

	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second,
	}, nil)
	resp, err := oc.GetAgents(context.Background(), "t1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	got := map[string]bool{}
	for _, c := range resp.Crews {
		got[c.CrewName] = c.HasRecentActivity
	}
	if !got["qgen"] {
		t.Errorf("qgen HasRecentActivity = false; want true (obs emitted true)")
	}
	if got["oe_grading"] {
		t.Errorf("oe_grading HasRecentActivity = true; want false (obs emitted false)")
	}
}

// TestObservabilityClient_GetAgentPrompts_Happy proves the CHO-2364 client
// method hits /api/v1/observability/agent-prompts with the tenant forwarded
// and passes the agents body through RAW: an upstream field this client has
// never heard of must survive (the has_recent_activity lesson, by design).
func TestObservabilityClient_GetAgentPrompts_Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/observability/agent-prompts" {
			t.Errorf("path = %s; want /api/v1/observability/agent-prompts", r.URL.Path)
		}
		if r.URL.Query().Get("tenant_id") != "t1" {
			t.Errorf("tenant_id = %s; want t1", r.URL.Query().Get("tenant_id"))
		}
		_, _ = w.Write([]byte(`{"agents":[
			{"agent_id":"qgen_question","crew_name":"qgen","evidence_kind":"decisions",
			 "decisions_total":4,"latest_prompt_version":"v4","future_field":"kept",
			 "versions":[{"prompt_version":"v4","prompt_source":"grimoire","decisions":1,"last_seen":"2026-07-20T13:00:00Z"}],
			 "use_cases":[{"key":"ai_assist_single","decisions":2}]},
			{"agent_id":"companion","crew_name":"companion","evidence_kind":"ritual_stamps",
			 "runs_total":3,"versions":[{"prompt_version":"v3","runs":2,"last_seen":"2026-07-22T09:00:00Z"}]}
		]}`))
	}))
	defer srv.Close()

	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second,
	}, nil)
	resp, err := oc.GetAgentPrompts(context.Background(), "t1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(resp.Agents, &rows); err != nil {
		t.Fatalf("agents not raw JSON array: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("agents = %d; want 2", len(rows))
	}
	if rows[0]["agent_id"] != "qgen_question" || rows[0]["decisions_total"].(float64) != 4 {
		t.Errorf("rows[0] = %#v", rows[0])
	}
	// The unknown field must survive the pass-through untouched.
	if rows[0]["future_field"] != "kept" {
		t.Errorf("future_field dropped by the pass-through: %#v", rows[0])
	}
	if rows[1]["evidence_kind"] != "ritual_stamps" {
		t.Errorf("rows[1] = %#v", rows[1])
	}
}

func TestObservabilityClient_GetAgentPrompts_NoAddr(t *testing.T) {
	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{}, nil)
	if _, err := oc.GetAgentPrompts(context.Background(), "t1"); !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("err = %v; want ErrUpstream wrap", err)
	}
}

// TestObservabilityClient_GetAgentPrompts_EmptyBody proves the 404/empty
// upstream body normalizes to an empty agents array (the FE always receives
// a well-formed array, never null).
func TestObservabilityClient_GetAgentPrompts_EmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second,
	}, nil)
	resp, err := oc.GetAgentPrompts(context.Background(), "t1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(resp.Agents) != "[]" {
		t.Errorf("agents = %q; want [] on empty upstream body", string(resp.Agents))
	}
}

func TestObservabilityClient_GetAgentDecisions_EnvelopeShape(t *testing.T) {
	t.Setenv("CHORA_TRACE_UI_URL", "https://trace.chora.local")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/agent-decisions") {
			t.Errorf("path = %s; want /api/agent-decisions", r.URL.Path)
		}
		body := upstream.AgentDecisionsResponse{
			Items: []upstream.AgentDecisionRow{
				{LogID: "log-1", AgentID: "qgen_question", TraceID: "0af7651916cd43dd8448eb211c80319c"},
				{LogID: "log-2", AgentID: "qgen_critic", TraceID: "1bc7651916cd43dd8448eb211c80319c"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second,
	}, nil)
	resp, err := oc.GetAgentDecisions(context.Background(), "t1", "", "", 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("items = %d; want 2", len(resp.Items))
	}
	for i, row := range resp.Items {
		// Self-hosted tracing UI deep-link (cloud Cloud Trace deep-links removed).
		// These rows carry a bare TraceID (no traceparent) so there is no span.
		if !strings.Contains(row.CloudTraceURL, "trace.chora.local/trace/") {
			t.Errorf("[%d] CloudTraceURL = %s; want self-hosted trace deep-link", i, row.CloudTraceURL)
		}
		if strings.Contains(row.CloudTraceURL, "console.the cloud console") {
			t.Errorf("[%d] CloudTraceURL = %s; must NOT reference cloud", i, row.CloudTraceURL)
		}
	}
}

func TestObservabilityClient_GetAgentDecisions_BareArrayShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Bare-array fallback — some legacy responses use this shape.
		_, _ = w.Write([]byte(`[{"log_id":"log-1","agent_id":"qgen_question","trace_id":"abc123"}]`))
	}))
	defer srv.Close()
	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second,
	}, nil)
	resp, err := oc.GetAgentDecisions(context.Background(), "t1", "", "", 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items = %d; want 1", len(resp.Items))
	}
}

// TestObservabilityClient_GetAgentDecisions_PromptConditions proves the durable
// prompt-explainability map<string,string> (ADR-197 M-A.5) deserializes from the
// raw chora-observability decision JSON onto AgentDecisionRow.PromptConditions
// via the pinned `prompt_conditions` tag.
func TestObservabilityClient_GetAgentDecisions_PromptConditions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"log_id":"log-1","agent_id":"qgen_question","prompt_conditions":{"difficulty":"hard","subject":"algebra"}}]}`))
	}))
	defer srv.Close()
	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second,
	}, nil)
	resp, err := oc.GetAgentDecisions(context.Background(), "t1", "", "", 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items = %d; want 1", len(resp.Items))
	}
	pc := resp.Items[0].PromptConditions
	if pc["difficulty"] != "hard" || pc["subject"] != "algebra" {
		t.Errorf("PromptConditions = %v; want {difficulty:hard, subject:algebra}", pc)
	}
}

func TestObservabilityClient_GetCorrelation_Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/correlations/") {
			t.Errorf("path = %s; want /api/correlations/...", r.URL.Path)
		}
		body := upstream.CorrelationResponse{
			CorrelationID: "corr-1",
			TraceID:       "abc",
			Spans:         []any{map[string]any{"name": "span-a"}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()
	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second,
	}, nil)
	resp, err := oc.GetCorrelation(context.Background(), "t1", "corr-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp == nil || resp.CorrelationID != "corr-1" {
		t.Errorf("resp = %+v", resp)
	}
}

func TestObservabilityClient_GetCorrelation_EmptyID(t *testing.T) {
	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{HTTPAddr: "http://x"}, nil)
	if _, err := oc.GetCorrelation(context.Background(), "t1", "  "); !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("err = %v; want ErrUpstream wrap", err)
	}
}

func TestObservabilityClient_CountAgentDecisions_Happy(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		body := upstream.AgentDecisionCountResponse{
			Count: 17,
			Since: "2026-05-25T12:00:00Z",
			Until: "2026-05-26T12:00:00Z",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second,
	}, nil)
	since := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	resp, err := oc.CountAgentDecisions(context.Background(), "t1", since, until)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotPath != "/api/v1/observability/agent-decisions/count" {
		t.Errorf("path = %s; want /api/v1/observability/agent-decisions/count", gotPath)
	}
	// Query must carry tenant_id + since + until (URL-encoded).
	if !strings.Contains(gotQuery, "tenant_id=t1") {
		t.Errorf("query missing tenant_id=t1: %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "since=") {
		t.Errorf("query missing since=: %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "until=") {
		t.Errorf("query missing until=: %s", gotQuery)
	}
	if resp.Count != 17 {
		t.Errorf("count = %d; want 17", resp.Count)
	}
}

func TestObservabilityClient_CountAgentDecisions_NoAddr(t *testing.T) {
	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{}, nil)
	_, err := oc.CountAgentDecisions(context.Background(), "t1", time.Time{}, time.Time{})
	if !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("err = %v; want ErrUpstream wrap", err)
	}
}

func TestObservabilityClient_CountAgentDecisions_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second,
	}, nil)
	_, err := oc.CountAgentDecisions(context.Background(), "t1", time.Time{}, time.Time{})
	if !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("err = %v; want ErrUpstream wrap (500 surfaces loud)", err)
	}
}

func TestObservabilityClient_CountAgentDecisions_ZeroTimes_NoQueryParams(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		body := upstream.AgentDecisionCountResponse{Count: 0}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()
	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second,
	}, nil)
	_, err := oc.CountAgentDecisions(context.Background(), "t1", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Zero times → no since/until in query; only tenant_id is forwarded.
	if strings.Contains(gotQuery, "since=") {
		t.Errorf("zero since should NOT serialize: %s", gotQuery)
	}
	if strings.Contains(gotQuery, "until=") {
		t.Errorf("zero until should NOT serialize: %s", gotQuery)
	}
}

func TestObservabilityClient_AggregateTokenUsage_Happy(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		body := upstream.TokenUsageAggregateResponse{
			Groups: []upstream.TokenUsageAggregateGroup{
				{Group: "gemini-2.5-flash-lite", TotalCostUsdMicros: 1_234_500, PromptTokens: 100, CompletionTokens: 50, EntryCount: 2},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second,
	}, nil)
	resp, err := oc.AggregateTokenUsage(context.Background(), "t1", "model", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotPath != "/api/token-usage/aggregate" {
		t.Errorf("path = %s; want /api/token-usage/aggregate", gotPath)
	}
	if !strings.Contains(gotQuery, "group_by=model") {
		t.Errorf("query missing group_by=model: %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "tenant_id=t1") {
		t.Errorf("query missing tenant_id=t1: %s", gotQuery)
	}
	if len(resp.Groups) != 1 {
		t.Fatalf("groups = %d; want 1", len(resp.Groups))
	}
	if resp.Groups[0].Group != "gemini-2.5-flash-lite" {
		t.Errorf("group = %s", resp.Groups[0].Group)
	}
	if resp.Groups[0].TotalCostUsdMicros != 1_234_500 {
		t.Errorf("micros = %d; want 1234500", resp.Groups[0].TotalCostUsdMicros)
	}
}

func TestObservabilityClient_AggregateTokenUsage_EmptyBodyNonNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 204-like empty body — client must return a non-nil empty slice.
		_, _ = w.Write([]byte(``))
	}))
	defer srv.Close()
	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second,
	}, nil)
	resp, err := oc.AggregateTokenUsage(context.Background(), "t1", "agent", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Groups == nil {
		t.Error("groups should be a non-nil empty slice")
	}
}

func TestObservabilityClient_AggregateTokenUsage_NoAddr(t *testing.T) {
	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{}, nil)
	if _, err := oc.AggregateTokenUsage(context.Background(), "t1", "model", time.Time{}, time.Time{}); !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("err = %v; want ErrUpstream wrap", err)
	}
}

func TestObservabilityClient_AggregateTokenUsage_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second,
	}, nil)
	if _, err := oc.AggregateTokenUsage(context.Background(), "t1", "model", time.Time{}, time.Time{}); !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("err = %v; want ErrUpstream wrap (500 surfaces loud)", err)
	}
}

func TestObservabilityClient_GetCostCumulative_Happy(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body := upstream.CostCumulativeResponse{
			TotalCostUsdMicros: 11_104_500,
			TotalCostUsd:       "$11.10",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()
	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second,
	}, nil)
	resp, err := oc.GetCostCumulative(context.Background(), "t1", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotPath != "/api/cost/cumulative" {
		t.Errorf("path = %s; want /api/cost/cumulative", gotPath)
	}
	if resp.TotalCostUsdMicros != 11_104_500 {
		t.Errorf("micros = %d; want 11104500", resp.TotalCostUsdMicros)
	}
}

// TestObservabilityClient_AggregateTokenUsage_ForwardsWindow proves a non-zero
// from/to is serialized as RFC3339 `from`/`to` query params, and a zero time is
// omitted (mirrors CountAgentDecisions' since/until convention).
func TestObservabilityClient_AggregateTokenUsage_ForwardsWindow(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(upstream.TokenUsageAggregateResponse{Groups: []upstream.TokenUsageAggregateGroup{}})
	}))
	defer srv.Close()
	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second,
	}, nil)

	from := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	if _, err := oc.AggregateTokenUsage(context.Background(), "t1", "model", from, to); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(gotQuery, "from=2026-05-30T12%3A00%3A00Z") {
		t.Errorf("query missing from=2026-05-30T12:00:00Z: %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "to=2026-06-29T12%3A00%3A00Z") {
		t.Errorf("query missing to=2026-06-29T12:00:00Z: %s", gotQuery)
	}

	// Zero times must NOT serialize.
	if _, err := oc.AggregateTokenUsage(context.Background(), "t1", "model", time.Time{}, time.Time{}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Contains(gotQuery, "from=") || strings.Contains(gotQuery, "to=") {
		t.Errorf("zero window should NOT serialize from/to: %s", gotQuery)
	}
}

// TestObservabilityClient_GetCostCumulative_ForwardsWindow — same for the
// cumulative endpoint.
func TestObservabilityClient_GetCostCumulative_ForwardsWindow(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(upstream.CostCumulativeResponse{TotalCostUsdMicros: 0})
	}))
	defer srv.Close()
	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second,
	}, nil)

	from := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC)
	if _, err := oc.GetCostCumulative(context.Background(), "t1", from, to); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(gotQuery, "from=2026-06-22T00%3A00%3A00Z") {
		t.Errorf("query missing from: %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "to=2026-06-29T00%3A00%3A00Z") {
		t.Errorf("query missing to: %s", gotQuery)
	}
}

func TestObservabilityClient_GetCostCumulative_NoAddr(t *testing.T) {
	oc := upstream.NewObservabilityClient(upstream.ObservabilityConfig{}, nil)
	if _, err := oc.GetCostCumulative(context.Background(), "t1", time.Time{}, time.Time{}); !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("err = %v; want ErrUpstream wrap", err)
	}
}

func TestLoadObservabilityConfigFromEnv_FallbackToLegacy(t *testing.T) {
	t.Setenv("CHORA_OBSERVABILITY_HTTP_ADDR", "")
	t.Setenv("SVC_OBSERVABILITY_URL", "http://legacy:8080")
	c := upstream.LoadObservabilityConfigFromEnv()
	if c.HTTPAddr != "http://legacy:8080" {
		t.Errorf("addr = %s; want legacy fallback", c.HTTPAddr)
	}
}

func TestLoadObservabilityConfigFromEnv_DedicatedTakesPrecedence(t *testing.T) {
	t.Setenv("CHORA_OBSERVABILITY_HTTP_ADDR", "http://dedicated:8080")
	t.Setenv("SVC_OBSERVABILITY_URL", "http://legacy:8080")
	c := upstream.LoadObservabilityConfigFromEnv()
	if c.HTTPAddr != "http://dedicated:8080" {
		t.Errorf("addr = %s; want dedicated", c.HTTPAddr)
	}
}
