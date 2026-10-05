// concept_graph_test.go — RED-phase TDD specs for the learner-sovereign
// Discovery Knowledge Graph BFF proxy (A+ Discovery). The concept-graph
// aggregate is built + deployed on chora-consumption at
// /v1/me/concept-graph[/...] but is DORMANT at the BFF — no gateway route.
// These tests mirror goals_test.go / growth_edges_test.go EXACTLY:
// ProxyConceptGraph is a VERBATIM passthrough that strips the /api prefix
// (→ /v1/me/concept-graph) and forwards method + path + rawQuery + body +
// Content-Type verbatim, delegating learner scoping to chora-consumption
// (which reads X-Tenant-Id + lowercase gcid off the stamped mesh claims via
// extRequireContext). Because the forward is verbatim, the SAME aggregator
// method serves every route shape — the downstream router does the real
// sub-route dispatch (list / reroot / concepts / concepts/{id} / edges /
// edges/{id}).
//
//	GET    /api/v1/me/concept-graph              → GET    /v1/me/concept-graph
//	POST   /api/v1/me/concept-graph/reroot       → POST   /v1/me/concept-graph/reroot
//	POST   /api/v1/me/concept-graph/concepts     → POST   /v1/me/concept-graph/concepts
//	PATCH  /api/v1/me/concept-graph/concepts/{id}→ PATCH  /v1/me/concept-graph/concepts/{id}
//	DELETE /api/v1/me/concept-graph/concepts/{id}→ DELETE /v1/me/concept-graph/concepts/{id}
//	POST   /api/v1/me/concept-graph/edges        → POST   /v1/me/concept-graph/edges
//	DELETE /api/v1/me/concept-graph/edges/{id}   → DELETE /v1/me/concept-graph/edges/{id}
//
// Strict TDD: tests written BEFORE the aggregator implementation.
package gatewayproxy_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

// GET /api/v1/me/concept-graph list — path translated (/api stripped), filter
// query forwarded verbatim, mesh-trust headers (X-Tenant-Id + lowercase gcid)
// stamped so the downstream can RLS-scope the read + build the graph.
func TestProxyConceptGraph_ListPathTranslatedQueryPreserved(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"concepts":[],"edges":[],"rootConceptId":null}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.ProxyConceptGraph(context.Background(), sampleAuth(),
		http.MethodGet, "/api/v1/me/concept-graph", "include=edges", nil, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/v1/me/concept-graph" {
		t.Errorf("downstream path = %q; want /v1/me/concept-graph", cb.path)
	}
	if cb.rawQ != "include=edges" {
		t.Errorf("downstream query = %q; want the verbatim filter query", cb.rawQ)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %q; want GET", cb.method)
	}
	if cb.hdr.Get("X-Tenant-Id") != sampleAuth().TenantID {
		t.Errorf("X-Tenant-Id = %q; want %q", cb.hdr.Get("X-Tenant-Id"), sampleAuth().TenantID)
	}
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("lowercase gcid = %q; want %q", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
}

// POST /api/v1/me/concept-graph/reroot — re-root the learner-sovereign
// hierarchy. Subtree path translated; JSON body forwarded verbatim.
func TestProxyConceptGraph_RerootForwardsBody(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"rootConceptId":"c-9"}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	body := []byte(`{"conceptId":"c-9"}`)
	resp, err := a.ProxyConceptGraph(context.Background(), sampleAuth(),
		http.MethodPost, "/api/v1/me/concept-graph/reroot", "", body, "application/json")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/v1/me/concept-graph/reroot" {
		t.Errorf("downstream path = %q; want /v1/me/concept-graph/reroot", cb.path)
	}
	if cb.method != http.MethodPost {
		t.Errorf("method = %q; want POST", cb.method)
	}
	if cb.body != string(body) {
		t.Errorf("downstream body = %q; want the reroot JSON forwarded verbatim", cb.body)
	}
}

// POST /api/v1/me/concept-graph/concepts — create a concept (empty-ok concepts
// organise atoms). Subtree path translated; JSON body forwarded verbatim.
func TestProxyConceptGraph_CreateConceptForwardsBody(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusCreated, `{"conceptId":"c-1","label":"Photosynthesis"}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	body := []byte(`{"label":"Photosynthesis","parentConceptId":null}`)
	resp, err := a.ProxyConceptGraph(context.Background(), sampleAuth(),
		http.MethodPost, "/api/v1/me/concept-graph/concepts", "", body, "application/json")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", resp.Status)
	}
	if cb.path != "/v1/me/concept-graph/concepts" {
		t.Errorf("downstream path = %q; want /v1/me/concept-graph/concepts", cb.path)
	}
	if cb.method != http.MethodPost {
		t.Errorf("method = %q; want POST", cb.method)
	}
	if cb.body != string(body) {
		t.Errorf("downstream body = %q; want the concept JSON forwarded verbatim", cb.body)
	}
}

// PATCH /api/v1/me/concept-graph/concepts/{id} — rename / re-parent a concept.
// The parametric subpath must reach chora-consumption verbatim with the body.
func TestProxyConceptGraph_UpdateConceptSubpathForwardsBody(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"conceptId":"c-1","label":"Cell biology"}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	body := []byte(`{"label":"Cell biology"}`)
	resp, err := a.ProxyConceptGraph(context.Background(), sampleAuth(),
		http.MethodPatch, "/api/v1/me/concept-graph/concepts/c-1", "", body, "application/json")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/v1/me/concept-graph/concepts/c-1" {
		t.Errorf("downstream path = %q; want /v1/me/concept-graph/concepts/c-1", cb.path)
	}
	if cb.method != http.MethodPatch {
		t.Errorf("method = %q; want PATCH", cb.method)
	}
	if cb.body != string(body) {
		t.Errorf("downstream body = %q; want the concept patch forwarded verbatim", cb.body)
	}
}

// DELETE /api/v1/me/concept-graph/concepts/{id} — remove a concept (bodyless).
// The DELETE method + parametric path must forward verbatim.
func TestProxyConceptGraph_DeleteConceptSubpath(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusNoContent, ``)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.ProxyConceptGraph(context.Background(), sampleAuth(),
		http.MethodDelete, "/api/v1/me/concept-graph/concepts/c-1", "", nil, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusNoContent {
		t.Errorf("status = %d; want 204", resp.Status)
	}
	if cb.path != "/v1/me/concept-graph/concepts/c-1" {
		t.Errorf("downstream path = %q; want /v1/me/concept-graph/concepts/c-1", cb.path)
	}
	if cb.method != http.MethodDelete {
		t.Errorf("method = %q; want DELETE", cb.method)
	}
}

// POST /api/v1/me/concept-graph/edges — add a re-rootable hierarchy edge.
func TestProxyConceptGraph_CreateEdgeForwardsBody(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusCreated, `{"edgeId":"e-1"}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	body := []byte(`{"fromConceptId":"c-1","toConceptId":"c-2","kind":"prereq"}`)
	resp, err := a.ProxyConceptGraph(context.Background(), sampleAuth(),
		http.MethodPost, "/api/v1/me/concept-graph/edges", "", body, "application/json")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", resp.Status)
	}
	if cb.path != "/v1/me/concept-graph/edges" {
		t.Errorf("downstream path = %q; want /v1/me/concept-graph/edges", cb.path)
	}
	if cb.method != http.MethodPost {
		t.Errorf("method = %q; want POST", cb.method)
	}
	if cb.body != string(body) {
		t.Errorf("downstream body = %q; want the edge JSON forwarded verbatim", cb.body)
	}
}

// DELETE /api/v1/me/concept-graph/edges/{id} — remove an edge (bodyless).
func TestProxyConceptGraph_DeleteEdgeSubpath(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusNoContent, ``)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.ProxyConceptGraph(context.Background(), sampleAuth(),
		http.MethodDelete, "/api/v1/me/concept-graph/edges/e-1", "", nil, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusNoContent {
		t.Errorf("status = %d; want 204", resp.Status)
	}
	if cb.path != "/v1/me/concept-graph/edges/e-1" {
		t.Errorf("downstream path = %q; want /v1/me/concept-graph/edges/e-1", cb.path)
	}
	if cb.method != http.MethodDelete {
		t.Errorf("method = %q; want DELETE", cb.method)
	}
}

// A downstream 4xx (e.g. 409 concept-cycle) proves the request reached
// chora-consumption — it MUST pass through verbatim so the FE renders the real
// semantic error rather than a generic gateway 5xx.
func TestProxyConceptGraph_Downstream4xxPassesThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusConflict, `{"error":"CONCEPT_CYCLE"}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.ProxyConceptGraph(context.Background(), sampleAuth(),
		http.MethodPost, "/api/v1/me/concept-graph/edges", "", []byte(`{}`), "application/json")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusConflict {
		t.Errorf("status = %d; want 409 passed through verbatim", resp.Status)
	}
}

// A downstream 5xx normalises to 502 GATEWAY_UPSTREAM_5XX via classify (genuine
// upstream failure — never leak a raw 500 to the FE).
func TestProxyConceptGraph_Downstream5xxNormalisesTo502(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusInternalServerError, `boom`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.ProxyConceptGraph(context.Background(), sampleAuth(),
		http.MethodGet, "/api/v1/me/concept-graph", "", nil, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 (downstream 5xx normalised)", resp.Status)
	}
}
