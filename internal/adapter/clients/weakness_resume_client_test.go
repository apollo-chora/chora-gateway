// weakness_resume_client_test.go — wire specs for the chora-ai-kernel-
// orchestrator Growth-Edge HITL resume client (ADR-205 WS-2/WS-8, CHO-1973).
//
// Verifies the load-bearing contract with the orchestrator resume endpoint: POST
// to /v1/orchestrator/weakness/{upload_id}/resume, tenant scope on the
// X-Tenant-Id HEADER (never the body), the bounded FE PANEL-SHAPE review payload
// forwarded verbatim (action + per-edge decisions + added_struggles +
// selected_outputs; NO run_id), the orchestrator's job/panel response forwarded
// through verbatim (the gateway is a pure proxy), and {error, detail} envelopes
// mapped to a structured upstream error vs a transport 503.
package clients

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewWeaknessResumeClient_FailsLoudOnEmptyBaseURL(t *testing.T) {
	if _, err := NewWeaknessResumeClient(WeaknessResumeClientConfig{BaseURL: "  "}); err == nil {
		t.Fatal("expected error on empty BaseURL (no-inline-config), got nil")
	}
}

func TestResumeWeakness_ForwardsHeadersAndPanelBody(t *testing.T) {
	var gotMethod, gotPath, gotTenant, gotGCID, gotTrace string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotTenant = r.Header.Get("X-Tenant-Id")
		gotGCID = r.Header.Get("gcid")
		gotTrace = r.Header.Get("traceparent")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// confirm → completed job (the orchestrator's job/panel shape).
		_, _ = w.Write([]byte(`{"upload_id":"upl-1","status":"COMPLETED",` +
			`"upserted_growth_edge_ids":["ge-1","ge-2"]}`))
	}))
	defer srv.Close()

	c, err := NewWeaknessResumeClient(WeaknessResumeClientConfig{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewWeaknessResumeClient: %v", err)
	}
	raw, err := c.ResumeWeakness(context.Background(), "upl-1", WeaknessResumeRequest{
		Action: "confirm",
		Edges: []WeaknessEdgeDecision{
			{ProposedEdgeID: "edge-1", Decision: "accept", Difficulty: "harder"},
			{ProposedEdgeID: "edge-2", Decision: "merge", MergeIntoID: "edge-9"},
		},
		AddedStruggles: []string{"adding-fractions"},
		// familiar_coaching is consumption's kept pre-rename output_selection wire key, forwarded opaquely.
		SelectedOutputs: []string{"focused_dose", "familiar_coaching"},
	}, WeaknessResumeIdentity{
		TenantID:    "tenant-1",
		GCID:        "gcid-1",
		Traceparent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
	})
	if err != nil {
		t.Fatalf("ResumeWeakness: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/orchestrator/weakness/upl-1/resume" {
		t.Errorf("path = %q", gotPath)
	}
	if gotTenant != "tenant-1" {
		t.Errorf("X-Tenant-Id = %q, want tenant-1 (identity must ride the header)", gotTenant)
	}
	if gotGCID != "gcid-1" {
		t.Errorf("gcid header = %q, want gcid-1", gotGCID)
	}
	if !strings.HasPrefix(gotTrace, "00-") {
		t.Errorf("traceparent not forwarded: %q", gotTrace)
	}

	// --- panel-shape body assertions (the canonical Command(resume=…) value) ---
	// run_id is GONE; identity must NOT be smuggled into the body.
	if _, ok := gotBody["run_id"]; ok {
		t.Error("run_id must NOT be in the resume body (dropped — thread keyed by {tenant, upload})")
	}
	if _, ok := gotBody["tenant_id"]; ok {
		t.Error("tenant_id must not be in the resume body")
	}
	if gotBody["action"] != "confirm" {
		t.Errorf("body action wrong: %+v", gotBody)
	}
	if _, ok := gotBody["added_struggles"].([]any); !ok {
		t.Errorf("added_struggles not forwarded as a list: %+v", gotBody["added_struggles"])
	}
	sel, _ := gotBody["selected_outputs"].([]any)
	if len(sel) != 2 || sel[1] != "familiar_coaching" {
		t.Errorf("selected_outputs not forwarded: %+v", gotBody["selected_outputs"])
	}
	edges, _ := gotBody["edges"].([]any)
	if len(edges) != 2 {
		t.Fatalf("edges not forwarded: %+v", gotBody["edges"])
	}
	e0, _ := edges[0].(map[string]any)
	if e0["proposed_edge_id"] != "edge-1" || e0["decision"] != "accept" || e0["difficulty"] != "harder" {
		t.Errorf("edge[0] wrong: %+v", e0)
	}
	// merge_into_id is omitempty — present on the merge edge, absent on the accept.
	if _, ok := e0["merge_into_id"]; ok {
		t.Errorf("edge[0] (accept) must omit merge_into_id: %+v", e0)
	}
	e1, _ := edges[1].(map[string]any)
	if e1["decision"] != "merge" || e1["merge_into_id"] != "edge-9" {
		t.Errorf("edge[1] (merge) wrong: %+v", e1)
	}

	// --- response pass-through: the orchestrator job/panel is forwarded verbatim ---
	var job map[string]any
	if err := json.Unmarshal(raw, &job); err != nil {
		t.Fatalf("decode pass-through job body %q: %v", string(raw), err)
	}
	if job["upload_id"] != "upl-1" || job["status"] != "COMPLETED" {
		t.Errorf("job not forwarded verbatim: %+v", job)
	}
	ids, _ := job["upserted_growth_edge_ids"].([]any)
	if len(ids) != 2 || ids[0] != "ge-1" {
		t.Errorf("upserted_growth_edge_ids not forwarded: %+v", job["upserted_growth_edge_ids"])
	}
}

func TestResumeWeakness_ReiterateRefreshedPanelPassThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var gotBody map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		if gotBody["action"] != "reiterate" {
			t.Errorf("server saw action = %v, want reiterate", gotBody["action"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// reiterate → AWAITING_REVIEW + a refreshed review panel.
		_, _ = w.Write([]byte(`{"upload_id":"upl-1","status":"AWAITING_REVIEW",` +
			`"review":{"proposed_edges":[{"proposed_edge_id":"edge-7"}],` +
			`"candidate_struggles":[],"available_outputs":[]}}`))
	}))
	defer srv.Close()

	c, _ := NewWeaknessResumeClient(WeaknessResumeClientConfig{BaseURL: srv.URL})
	raw, err := c.ResumeWeakness(context.Background(), "upl-1", WeaknessResumeRequest{
		Action: "reiterate",
		Edges:  []WeaknessEdgeDecision{{ProposedEdgeID: "edge-1", Decision: "reject"}},
	}, WeaknessResumeIdentity{TenantID: "tenant-1"})
	if err != nil {
		t.Fatalf("ResumeWeakness: %v", err)
	}
	var job map[string]any
	if err := json.Unmarshal(raw, &job); err != nil {
		t.Fatalf("decode reiterate body %q: %v", string(raw), err)
	}
	if job["status"] != "AWAITING_REVIEW" {
		t.Errorf("reiterate status = %v, want AWAITING_REVIEW", job["status"])
	}
	review, ok := job["review"].(map[string]any)
	if !ok {
		t.Fatalf("refreshed review panel not forwarded: %+v", job)
	}
	pe, _ := review["proposed_edges"].([]any)
	if len(pe) != 1 {
		t.Errorf("refreshed proposed_edges not forwarded: %+v", review["proposed_edges"])
	}
}

func TestResumeWeakness_StructuredUpstreamErrorOn4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_action","detail":"action must be one of [confirm, reiterate]"}`))
	}))
	defer srv.Close()

	c, _ := NewWeaknessResumeClient(WeaknessResumeClientConfig{BaseURL: srv.URL})
	_, err := c.ResumeWeakness(context.Background(), "upl-1",
		WeaknessResumeRequest{Action: "bogus"}, WeaknessResumeIdentity{TenantID: "t"})
	var ue *WeaknessResumeUpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("want *WeaknessResumeUpstreamError, got %v", err)
	}
	if ue.StatusCode != http.StatusBadRequest || ue.Code != "invalid_action" {
		t.Errorf("upstream error wrong: %+v", ue)
	}
	if !strings.Contains(ue.Message, "action must be one of") {
		t.Errorf("detail not folded into message: %q", ue.Message)
	}
}

func TestResumeWeakness_NonJSON200IsUnavailable(t *testing.T) {
	// A 200 with a non-JSON body is a broken orchestrator contract — fail loud
	// (never forward garbage to the SPA) as a transport-unavailable, not a 200.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<html>not json</html>`))
	}))
	defer srv.Close()

	c, _ := NewWeaknessResumeClient(WeaknessResumeClientConfig{BaseURL: srv.URL})
	raw, err := c.ResumeWeakness(context.Background(), "upl-1",
		WeaknessResumeRequest{Action: "confirm"}, WeaknessResumeIdentity{TenantID: "t"})
	if !errors.Is(err, ErrWeaknessResumeUpstreamUnavailable) {
		t.Fatalf("want ErrWeaknessResumeUpstreamUnavailable for non-JSON 200, got %v", err)
	}
	if raw != nil {
		t.Errorf("want nil body on non-JSON 200, got %q", string(raw))
	}
}

func TestResumeWeakness_UnavailableOn5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"resume_failed"}`))
	}))
	defer srv.Close()

	c, _ := NewWeaknessResumeClient(WeaknessResumeClientConfig{BaseURL: srv.URL})
	_, err := c.ResumeWeakness(context.Background(), "upl-1",
		WeaknessResumeRequest{Action: "confirm"}, WeaknessResumeIdentity{TenantID: "t"})
	if !errors.Is(err, ErrWeaknessResumeUpstreamUnavailable) {
		t.Fatalf("want ErrWeaknessResumeUpstreamUnavailable, got %v", err)
	}
}
