// kgexplore_test.go — RED-phase TDD specs for the BFF
// /api/v1/consumption/kg/explore/{atom_id} proxy aggregator (D1.4).
//
// The FE Discovery KG canvas (P1.3) consumes the per-user KG hexagonal
// exploration endpoint. chora-consumption owns the handler
// (services/chora-consumption/internal/adapter/http/fog_handler.go,
// mounted at /api/v1/consumption/kg/explore/) but chora-gateway did not
// proxy it — the route 404'd at the gateway edge.
//
// This aggregator fans GET /api/v1/consumption/kg/explore/{atom_id} out to
// chora-consumption's identical path, stamping the mesh-trust headers the
// downstream's extRequireContext reads (X-Tenant-Id + lowercase gcid, plus
// the canonical chora-gcid / chora-tenant-id / chora-role-summary).
//
// Strict TDD: tests written BEFORE the aggregator implementation.
package kgexplore_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/kgexplore"
)

// captureBackend is a minimal httptest.Server that records the inbound
// request from the BFF outbound call.
type captureBackend struct {
	srv    *httptest.Server
	hdr    http.Header
	path   string
	method string
	calls  int
}

func newCaptureBackend(t *testing.T, status int, body string) *captureBackend {
	t.Helper()
	cb := &captureBackend{}
	cb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cb.calls++
		cb.hdr = r.Header.Clone()
		cb.path = r.URL.Path
		cb.method = r.Method
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(cb.srv.Close)
	return cb
}

func sampleAuth() kgexplore.AuthCtx {
	return kgexplore.AuthCtx{
		Bearer:      "raw-session-jwt",
		Traceparent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		TenantID:    "01970000-0000-7000-8000-0000000000bb",
		GCID:        "01970000-0000-7000-8000-0000000000aa",
	}
}

// -----------------------------------------------------------------------------
// 200 — happy path: proxies GET to chora-consumption's identical path
// -----------------------------------------------------------------------------

func TestExplore_HappyPath_ProxiesToConsumption(t *testing.T) {
	t.Parallel()
	consumption := newCaptureBackend(t, http.StatusOK,
		`{"focal_node":"atom-123","neighbors":[{"atom_id":"atom-9","relation":"prerequisite","confidence":0.9,"fog_label":"hidden","is_junction":false}]}`)

	a := kgexplore.New(kgexplore.Config{
		ConsumptionURL: consumption.srv.URL,
		PerCallTimeout: 1 * time.Second,
	})
	if a == nil {
		t.Fatal("aggregator nil — ConsumptionURL was set, expected non-nil")
	}

	resp, err := a.Explore(context.Background(), sampleAuth(), "atom-123")
	if err != nil {
		t.Fatalf("Explore err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.Status, resp.Body)
	}
	// Downstream path must mirror the chora-consumption fog_handler mount.
	if consumption.path != "/api/v1/consumption/kg/explore/atom-123" {
		t.Errorf("downstream path = %q; want /api/v1/consumption/kg/explore/atom-123", consumption.path)
	}
	if consumption.method != http.MethodGet {
		t.Errorf("downstream method = %q; want GET", consumption.method)
	}
	// Body passed through verbatim (chora-consumption already emits the
	// ADR-143 §7 {focal_node, neighbors[]} shape the FE consumes).
	if !strings.Contains(string(resp.Body), `"focal_node"`) {
		t.Errorf("body = %s; missing focal_node", resp.Body)
	}
}

// -----------------------------------------------------------------------------
// Mesh-trust headers — chora-consumption's extRequireContext reads
// X-Tenant-Id + lowercase gcid; the canonical chora-* headers ride along.
// -----------------------------------------------------------------------------

func TestExplore_StampsMeshTrustHeaders(t *testing.T) {
	t.Parallel()
	consumption := newCaptureBackend(t, http.StatusOK, `{"focal_node":"a","neighbors":[]}`)
	a := kgexplore.New(kgexplore.Config{ConsumptionURL: consumption.srv.URL, PerCallTimeout: time.Second})

	auth := sampleAuth()
	_, err := a.Explore(context.Background(), auth, "atom-x")
	if err != nil {
		t.Fatalf("Explore err: %v", err)
	}

	// chora-consumption fog_handler.extRequireContext reads X-Tenant-Id +
	// lowercase gcid. Both MUST be present or the handler 400s MISSING_CONTEXT.
	if got := consumption.hdr.Get("X-Tenant-Id"); got != auth.TenantID {
		t.Errorf("X-Tenant-Id = %q, want %q", got, auth.TenantID)
	}
	if got := consumption.hdr.Get("gcid"); got != auth.GCID {
		t.Errorf("gcid (lowercase) = %q, want %q", got, auth.GCID)
	}
	// Canonical mesh-trust headers ride along for services on the mesh-claims path.
	if got := consumption.hdr.Get("chora-gcid"); got != auth.GCID {
		t.Errorf("chora-gcid = %q, want %q", got, auth.GCID)
	}
	if got := consumption.hdr.Get("chora-tenant-id"); got != auth.TenantID {
		t.Errorf("chora-tenant-id = %q, want %q", got, auth.TenantID)
	}
	// Bearer + traceparent propagated.
	if got := consumption.hdr.Get("Authorization"); got != "Bearer "+auth.Bearer {
		t.Errorf("Authorization = %q, want Bearer pass-through", got)
	}
	if got := consumption.hdr.Get("traceparent"); got != auth.Traceparent {
		t.Errorf("traceparent = %q, want propagated", got)
	}
}

// -----------------------------------------------------------------------------
// atom_id is path-escaped so a malformed id can't break the upstream URL
// -----------------------------------------------------------------------------

func TestExplore_PathEscapesAtomID(t *testing.T) {
	t.Parallel()
	consumption := newCaptureBackend(t, http.StatusOK, `{"focal_node":"a","neighbors":[]}`)
	a := kgexplore.New(kgexplore.Config{ConsumptionURL: consumption.srv.URL, PerCallTimeout: time.Second})

	_, err := a.Explore(context.Background(), sampleAuth(), "atom with space")
	if err != nil {
		t.Fatalf("Explore err: %v", err)
	}
	// httptest decodes the path — the escaped id round-trips back to the raw value.
	if consumption.path != "/api/v1/consumption/kg/explore/atom with space" {
		t.Errorf("downstream path = %q; want the decoded atom id", consumption.path)
	}
}

// -----------------------------------------------------------------------------
// 4xx pass-through — a downstream handler error (e.g. the not-yet-fixed fog
// engine's 400, or a 404) is forwarded verbatim, NOT normalised.
// -----------------------------------------------------------------------------

func TestExplore_Downstream4xx_PassThrough(t *testing.T) {
	t.Parallel()
	consumption := newCaptureBackend(t, http.StatusBadRequest,
		`{"code":"FOG_VALIDATE_FAILED","message":"validate-node FAILED"}`)
	a := kgexplore.New(kgexplore.Config{ConsumptionURL: consumption.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.Explore(context.Background(), sampleAuth(), "atom-1")
	if err != nil {
		t.Fatalf("Explore err: %v", err)
	}
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 pass-through", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "FOG_VALIDATE_FAILED") {
		t.Errorf("body = %s; downstream error envelope should pass through", resp.Body)
	}
}

// -----------------------------------------------------------------------------
// 502 — upstream 5xx is normalised to 502 Bad Gateway (cascade-safe)
// -----------------------------------------------------------------------------

func TestExplore_Upstream5xx_Becomes502(t *testing.T) {
	t.Parallel()
	consumption := newCaptureBackend(t, http.StatusInternalServerError, `boom`)
	a := kgexplore.New(kgexplore.Config{ConsumptionURL: consumption.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.Explore(context.Background(), sampleAuth(), "atom-1")
	if err != nil {
		t.Fatalf("Explore err: %v", err)
	}
	if resp.Status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_UPSTREAM_5XX") {
		t.Errorf("body = %s; missing GATEWAY_UPSTREAM_5XX", resp.Body)
	}
}

// -----------------------------------------------------------------------------
// 504 — upstream timeout surfaces as 504 Gateway Timeout
// -----------------------------------------------------------------------------

func TestExplore_UpstreamTimeout_Becomes504(t *testing.T) {
	t.Parallel()
	consumption := newCaptureBackend(t, http.StatusOK, `{}`)
	// Slow handler — replace with a sleeping handler.
	consumption.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	a := kgexplore.New(kgexplore.Config{ConsumptionURL: consumption.srv.URL, PerCallTimeout: 50 * time.Millisecond})

	resp, err := a.Explore(context.Background(), sampleAuth(), "atom-1")
	if err != nil {
		t.Fatalf("Explore err: %v", err)
	}
	if resp.Status != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504; body=%s", resp.Status, resp.Body)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_UPSTREAM_TIMEOUT") {
		t.Errorf("body = %s; missing GATEWAY_UPSTREAM_TIMEOUT", resp.Body)
	}
}

// -----------------------------------------------------------------------------
// 502 — transport error (unreachable upstream) is normalised to 502.
// -----------------------------------------------------------------------------

func TestExplore_TransportError_Becomes502(t *testing.T) {
	t.Parallel()
	// Point at a host that will refuse the connection — closed-port loopback.
	a := kgexplore.New(kgexplore.Config{
		ConsumptionURL: "http://127.0.0.1:1", // port 1 — guaranteed connection refused
		PerCallTimeout: 1 * time.Second,
	})
	resp, err := a.Explore(context.Background(), sampleAuth(), "atom-1")
	if err != nil {
		t.Fatalf("Explore err: %v", err)
	}
	if resp.Status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for a transport error", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_UPSTREAM_ERROR") {
		t.Errorf("body = %s; missing GATEWAY_UPSTREAM_ERROR", resp.Body)
	}
}

// -----------------------------------------------------------------------------
// 504 — a caller-canceled context surfaces as 504 (GATEWAY_REQUEST_CANCELED).
// -----------------------------------------------------------------------------

func TestExplore_CanceledContext_Becomes504(t *testing.T) {
	t.Parallel()
	consumption := newCaptureBackend(t, http.StatusOK, `{}`)
	consumption.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	a := kgexplore.New(kgexplore.Config{ConsumptionURL: consumption.srv.URL, PerCallTimeout: 2 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel almost immediately so the in-flight call observes context.Canceled.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	resp, err := a.Explore(ctx, sampleAuth(), "atom-1")
	if err != nil {
		t.Fatalf("Explore err: %v", err)
	}
	if resp.Status != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 for a canceled context", resp.Status)
	}
}

// -----------------------------------------------------------------------------
// New returns nil when ConsumptionURL is unset — caller route-skips so the
// route stays 404 in unconfigured dev envs (clearer than a 502 every call).
// -----------------------------------------------------------------------------

func TestNew_NilWhenConsumptionURLUnset(t *testing.T) {
	t.Parallel()
	if a := kgexplore.New(kgexplore.Config{}); a != nil {
		t.Errorf("New with empty ConsumptionURL should return nil; got %#v", a)
	}
}

// -----------------------------------------------------------------------------
// LoadConfigFromEnv reads SVC_CONSUMPTION_URL per feedback_no_inline_config.
// -----------------------------------------------------------------------------

func TestLoadConfigFromEnv_ReadsSvcConsumptionURL(t *testing.T) {
	t.Setenv("SVC_CONSUMPTION_URL", "http://chora-consumption.consumption.svc.cluster.local:8080")
	cfg := kgexplore.LoadConfigFromEnv()
	if cfg.ConsumptionURL != "http://chora-consumption.consumption.svc.cluster.local:8080" {
		t.Errorf("ConsumptionURL = %q; want the SVC_CONSUMPTION_URL value", cfg.ConsumptionURL)
	}
	if cfg.PerCallTimeout == 0 {
		t.Errorf("PerCallTimeout = 0; ApplyDefaults should have set a non-zero default")
	}
}

// -----------------------------------------------------------------------------
// Empty atom_id is rejected before the outbound call (defensive — the handler
// should already guard, but the aggregator must not build a bad URL).
// -----------------------------------------------------------------------------

func TestExplore_EmptyAtomID_NoOutboundCall(t *testing.T) {
	t.Parallel()
	consumption := newCaptureBackend(t, http.StatusOK, `{}`)
	a := kgexplore.New(kgexplore.Config{ConsumptionURL: consumption.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.Explore(context.Background(), sampleAuth(), "")
	if err != nil {
		t.Fatalf("Explore err: %v", err)
	}
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for empty atom_id", resp.Status)
	}
	if consumption.calls != 0 {
		t.Errorf("downstream called %d times; want 0 — empty atom_id must short-circuit", consumption.calls)
	}
}
