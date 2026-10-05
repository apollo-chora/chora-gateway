// v1_atoms_test.go — RED-phase TDD specs for CHO-2261.
//
// The A+ Daily Dose resolves each AI-picked atom's detail via
// GET /api/v1/atoms/{id} (AtomService.loadAtom, ADR-196 enrichment). That
// prefix was unregistered at the gateway → GATEWAY_ROUTE_NOT_FOUND (404) →
// the pick was fail-SOFT dropped (catchError(() => of(null))) → the learner
// silently got a thinner dose.
//
// The atom's authored detail is served by chora-creation at /api/atoms/{id}
// (NO /v1/ — this is the path the live mesh AuthorizationPolicy
// creation/allow-from-gateway allowlists, and the path the working PLAY page
// uses). So the fix is a verbatim proxy that TRANSLATES the gateway-facing
// /api/v1/atoms/{id} onto the downstream /api/atoms/{id}: the mesh contract is
// reused unchanged (no /api/v1/atoms/* allowlist entry needed).
//
// Strict TDD: these tests are written BEFORE Aggregator.GetAtomByID exists.
package gatewayproxy_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

// GetAtomByID proxies GET /api/v1/atoms/{id} → chora-creation /api/atoms/{id}.
// The KEY assertion is the path TRANSLATION (v1 stripped) so the request lands
// on the already-allowlisted downstream path.
func TestGetAtomByID_ProxiesToCreation_TranslatesV1Prefix(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK,
		`{"id":"019f6baf-0a31-708e-8923-954f39602102","title":"Road cycling for beginners","atom_type":"mcq"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.GetAtomByID(context.Background(), sampleAuth(),
		"019f6baf-0a31-708e-8923-954f39602102")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	// The translation is the whole point: gateway-facing /api/v1/atoms/{id}
	// MUST hit downstream /api/atoms/{id} (the allowlisted, learner-safe path).
	if cb.path != "/api/atoms/019f6baf-0a31-708e-8923-954f39602102" {
		t.Errorf("downstream path = %q; want /api/atoms/019f6baf-0a31-708e-8923-954f39602102 (v1 stripped)", cb.path)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %s; want GET", cb.method)
	}
	// Response body forwarded verbatim (UNWRAPPED atom — atom.service.ts consumes
	// the LearningAtom directly, no {atom:...} envelope).
	if !strings.Contains(string(resp.Body), `"Road cycling for beginners"`) {
		t.Errorf("body = %q; want verbatim unwrapped atom", string(resp.Body))
	}
	// Mesh-trust headers — chora-creation getAtom reads tenantFromContext (RLS).
	if cb.hdr.Get("X-Tenant-Id") != sampleAuth().TenantID {
		t.Errorf("X-Tenant-Id = %q; want %q", cb.hdr.Get("X-Tenant-Id"), sampleAuth().TenantID)
	}
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("lowercase gcid header = %q; want %q", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
}

func TestGetAtomByID_EmptyID_404_NoDownstream(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.GetAtomByID(context.Background(), sampleAuth(), "")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty atom id", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_ATOM_ID_REQUIRED") {
		t.Errorf("body = %q; want GATEWAY_ATOM_ID_REQUIRED code", string(resp.Body))
	}
}

func TestGetAtomByID_Upstream404_PassThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusNotFound, `{"error":{"code":"CREATION_ATOM_NOT_FOUND"}}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.GetAtomByID(context.Background(), sampleAuth(), "ghost")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 verbatim from upstream", resp.Status)
	}
}

func TestGetAtomByID_Upstream5xx_502(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusInternalServerError, `boom`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.GetAtomByID(context.Background(), sampleAuth(), "atom-1")
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 on upstream 5xx", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_UPSTREAM_5XX") {
		t.Errorf("body = %q; want GATEWAY_UPSTREAM_5XX code in 502 envelope", string(resp.Body))
	}
}
