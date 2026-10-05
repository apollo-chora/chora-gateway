// goals_test.go — RED-phase TDD specs for the ADR-204 learner-owned Goal BFF
// proxy (A+ Discovery). The Goal aggregate is built + deployed on
// chora-consumption at /v1/me/goals[/{id}] but was DORMANT — no BFF route.
// These tests mirror growth_edges_test.go EXACTLY: ProxyGoals is a verbatim
// passthrough that strips the /api prefix (→ /v1/me/goals) and forwards
// method + rawQuery + body + Content-Type verbatim, delegating learner scoping
// to chora-consumption (which reads X-Tenant-Id + lowercase gcid off the
// stamped mesh claims).
//
//	GET   /api/v1/me/goals        → chora-consumption GET   /v1/me/goals
//	POST  /api/v1/me/goals        → chora-consumption POST  /v1/me/goals
//	PATCH /api/v1/me/goals/{id}   → chora-consumption PATCH /v1/me/goals/{id}
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

// ADR-204 — ProxyGoals proxies GET /api/v1/me/goals to chora-consumption with
// the /api prefix stripped (→ /v1/me/goals), the filter query forwarded
// verbatim, and the mesh-trust headers (X-Tenant-Id + lowercase gcid) stamped
// so the downstream can RLS-scope the read + derive primaryLens.
func TestProxyGoals_ListPathTranslatedQueryPreserved(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"items":[],"primaryLens":"curiosity"}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.ProxyGoals(context.Background(), sampleAuth(),
		http.MethodGet, "/api/v1/me/goals", "status=active", nil, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/v1/me/goals" {
		t.Errorf("downstream path = %q; want /v1/me/goals", cb.path)
	}
	if cb.rawQ != "status=active" {
		t.Errorf("downstream query = %q; want the verbatim filter query", cb.rawQ)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %q; want GET", cb.method)
	}
	// Mesh-trust headers the downstream extRequireContext middleware reads.
	if cb.hdr.Get("X-Tenant-Id") != sampleAuth().TenantID {
		t.Errorf("X-Tenant-Id = %q; want %q", cb.hdr.Get("X-Tenant-Id"), sampleAuth().TenantID)
	}
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("lowercase gcid = %q; want %q", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
}

// POST /api/v1/me/goals create — the JSON GoalDTO body must reach
// chora-consumption /v1/me/goals verbatim (path translated, method preserved).
func TestProxyGoals_CreateForwardsJSONBody(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusCreated, `{"goalId":"g-1","kind":"curiosity","status":"active"}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	body := []byte(`{"kind":"curiosity","conceptSet":["photosynthesis"],"northStarNote":"learn plants"}`)
	resp, err := a.ProxyGoals(context.Background(), sampleAuth(),
		http.MethodPost, "/api/v1/me/goals", "", body, "application/json")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", resp.Status)
	}
	if cb.path != "/v1/me/goals" {
		t.Errorf("downstream path = %q; want /v1/me/goals", cb.path)
	}
	if cb.method != http.MethodPost {
		t.Errorf("method = %q; want POST", cb.method)
	}
	if cb.body != string(body) {
		t.Errorf("downstream body = %q; want the JSON GoalDTO forwarded verbatim", cb.body)
	}
}

// PATCH /api/v1/me/goals/{id} update — the parametric subpath must reach
// chora-consumption /v1/me/goals/{id} with the JSON patch body forwarded
// verbatim (status change / companion attach-detach).
func TestProxyGoals_UpdateSubpathForwardsBody(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"goalId":"g-1","status":"achieved"}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	body := []byte(`{"status":"achieved"}`)
	resp, err := a.ProxyGoals(context.Background(), sampleAuth(),
		http.MethodPatch, "/api/v1/me/goals/g-1", "", body, "application/json")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/v1/me/goals/g-1" {
		t.Errorf("downstream path = %q; want /v1/me/goals/g-1", cb.path)
	}
	if cb.method != http.MethodPatch {
		t.Errorf("method = %q; want PATCH", cb.method)
	}
	if cb.body != string(body) {
		t.Errorf("downstream body = %q; want the JSON patch forwarded verbatim", cb.body)
	}
}

// A downstream 4xx (e.g. 422 validation) proves the request reached
// chora-consumption — it MUST pass through verbatim so the FE renders the real
// semantic error rather than a generic gateway 5xx.
func TestProxyGoals_Downstream4xxPassesThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusUnprocessableEntity, `{"error":"INVALID_STATUS_TRANSITION"}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.ProxyGoals(context.Background(), sampleAuth(),
		http.MethodPatch, "/api/v1/me/goals/g-1", "", []byte(`{"status":"paused"}`), "application/json")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d; want 422 passed through verbatim", resp.Status)
	}
}

// A downstream 5xx normalises to 502 GATEWAY_UPSTREAM_5XX via classify (genuine
// upstream failure — never leak a raw 500 to the FE).
func TestProxyGoals_Downstream5xxNormalisesTo502(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusInternalServerError, `boom`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.ProxyGoals(context.Background(), sampleAuth(),
		http.MethodGet, "/api/v1/me/goals", "", nil, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 (downstream 5xx normalised)", resp.Status)
	}
}
