// traceparent_enrich_test.go — verifies the gateway derives the Cloud Trace
// deep-link (trace_id) from the W3C `traceparent` that chora-observability
// emits on agent_decision rows (it serializes `traceparent`, NOT a bare
// `trace_id`). Regression guard for the O+ Decision Traces "View in Cloud
// Trace ↗" deep-links once qgen/critic agents emit good trace telemetry
// (2026-06-02). Tested through the public GetAgentDecisions API.
package upstream_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

func TestObservabilityClient_GetAgentDecisions_DerivesTraceIDFromTraceparent(t *testing.T) {
	t.Setenv("CHORA_TRACE_UI_URL", "https://trace.chora.local")
	const traceID = "c7d2b4a939168862fa71880b5fb2e08b"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := upstream.AgentDecisionsResponse{
			Items: []upstream.AgentDecisionRow{
				// Canonical: real traceparent, no bare trace_id → derive both.
				{LogID: "log-1", AgentID: "qgen_crew",
					Traceparent: "00-" + traceID + "-f171040a776ba3d9-01"},
				// All-zero trace-id is the invalid sentinel → no trace_id / URL.
				{LogID: "log-2", AgentID: "qgen_critic",
					Traceparent: "00-00000000000000000000000000000000-0000000000000000-00"},
				// Malformed (too few segments) → no trace_id / URL.
				{LogID: "log-3", AgentID: "qgen_question", Traceparent: "garbage"},
				// Explicit trace_id still wins (legacy producers).
				{LogID: "log-4", AgentID: "qgen_crew", TraceID: "1bc7651916cd43dd8448eb211c80319c"},
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
	if len(resp.Items) != 4 {
		t.Fatalf("items = %d; want 4", len(resp.Items))
	}

	// Row 0 — trace-id AND span-id parsed from traceparent; deep-link uses the
	// new Cloud Trace Explorer matrix params (;traceId=;spanId=) so it lands on
	// the agent's OWN per-agent marker span, NOT the dead legacy /traces/list?tid=
	// (silently dropped by the new console). Span id is the per-agent marker span
	// the orchestrator stamps (agent.<agid>) — see §9.
	const spanID = "f171040a776ba3d9"
	if resp.Items[0].TraceID != traceID {
		t.Errorf("row0 TraceID = %q; want %q (parsed from traceparent)", resp.Items[0].TraceID, traceID)
	}
	if resp.Items[0].SpanID != spanID {
		t.Errorf("row0 SpanID = %q; want %q (parsed from traceparent)", resp.Items[0].SpanID, spanID)
	}
	u0 := resp.Items[0].CloudTraceURL
	if !strings.Contains(u0, "trace.chora.local/trace/"+traceID) {
		t.Errorf("row0 CloudTraceURL = %q; want self-hosted trace deep-link for %s", u0, traceID)
	}
	if !strings.Contains(u0, "span="+spanID) {
		t.Errorf("row0 CloudTraceURL = %q; want span=%s (per-agent span)", u0, spanID)
	}

	// Rows 1 + 2 — invalid/malformed traceparent → no trace_id, no URL.
	for _, i := range []int{1, 2} {
		if resp.Items[i].TraceID != "" || resp.Items[i].CloudTraceURL != "" {
			t.Errorf("row%d got TraceID=%q URL=%q; want empty (invalid traceparent)",
				i, resp.Items[i].TraceID, resp.Items[i].CloudTraceURL)
		}
	}

	// Row 3 — explicit trace_id (no traceparent) → ;traceId= matrix param, but
	// NO ;spanId= (none to derive). Still the new explorer format, never legacy.
	if resp.Items[3].TraceID != "1bc7651916cd43dd8448eb211c80319c" {
		t.Errorf("row3 TraceID = %q; want explicit trace_id preserved", resp.Items[3].TraceID)
	}
	u3 := resp.Items[3].CloudTraceURL
	if !strings.Contains(u3, "trace.chora.local/trace/1bc7651916cd43dd8448eb211c80319c") {
		t.Errorf("row3 CloudTraceURL = %q; want self-hosted trace deep-link", u3)
	}
	if strings.Contains(u3, "span=") {
		t.Errorf("row3 CloudTraceURL = %q; want NO span= (no traceparent)", u3)
	}
}
