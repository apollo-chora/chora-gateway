// home_preferences_test.go - ADR-240 Track B: RED-phase specs for the shell
// /home launcher layout proxy → chora-identity. Unlike the dashboard-layout
// proxy (which edge-validates a CLOSED wrapper enum), the home-layout proxy
// FORWARDS the body verbatim and validates NOTHING (D11): the FE pin registry is
// the single id authority, and chora-identity is the sole structural authority.
// GCID is delegated downstream via the stamped mesh headers (never the body).
package gatewayproxy_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

func TestSaveHomeLayout_ProxiesToIdentity(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK,
		`{"home_layout":{"pins":[{"id":"wallet"}],"updated_at":"2026-07-18T12:00:00Z"}}`)
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: cb.srv.URL, PerCallTimeout: time.Second})

	feBody := []byte(`{"pins":[{"id":"a-plus-dashboard","x":0},{"id":"wallet","x":1}],"updated_at":"2026-07-18T12:00:00Z"}`)
	resp, err := a.SaveHomeLayout(context.Background(), sampleAuth(), feBody)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/api/v1/me/preferences/home-layout" {
		t.Errorf("downstream path = %q; want /api/v1/me/preferences/home-layout", cb.path)
	}
	if cb.method != http.MethodPut {
		t.Errorf("method = %s; want PUT", cb.method)
	}
	// Body forwarded verbatim (the downstream persists it; position fields kept).
	if cb.body != string(feBody) {
		t.Errorf("body forwarded = %q; want verbatim", cb.body)
	}
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("lowercase gcid = %q; want %q (never the body)", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
	if cb.hdr.Get("X-Tenant-Id") != sampleAuth().TenantID {
		t.Errorf("X-Tenant-Id = %q; want %q", cb.hdr.Get("X-Tenant-Id"), sampleAuth().TenantID)
	}
}

// ADR-240 D11: the gateway does NOT edge-validate pin ids. An unknown id (which
// the dashboard-layout proxy would 422 against its closed enum) MUST proxy
// through - the id is inert in a GCID-scoped blob and grants nothing.
func TestSaveHomeLayout_UnknownPinId_ProxiesThrough_D11(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{}`)
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.SaveHomeLayout(context.Background(), sampleAuth(),
		[]byte(`{"pins":[{"id":"an-id-the-gateway-never-heard-of"}],"updated_at":"2026-07-18T12:00:00Z"}`))
	if resp.Status != http.StatusOK {
		t.Errorf("gateway must NOT edge-validate pin ids (D11); got %d", resp.Status)
	}
	if cb.calls != 1 {
		t.Errorf("home-layout must reach the downstream (no edge id-validation); calls = %d", cb.calls)
	}
}

// The gateway forwards home-layout UNCONDITIONALLY - even a structurally-odd body
// is proxied (chora-identity is the sole structural authority, D11). A malformed
// body must NOT short-circuit at the edge.
func TestSaveHomeLayout_ForwardsWithoutStructuralValidation(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusUnprocessableEntity, `{"code":"IDENTITY_INVALID_HOME_LAYOUT"}`)
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.SaveHomeLayout(context.Background(), sampleAuth(), []byte(`{"pins":"nope"}`))
	if cb.calls != 1 {
		t.Errorf("gateway must forward home-layout unconditionally; calls = %d", cb.calls)
	}
	if resp.Status != http.StatusUnprocessableEntity {
		t.Errorf("downstream 422 must pass through; got %d", resp.Status)
	}
}

func TestSaveHomeLayout_Upstream5xx_502(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusInternalServerError, `{"error":"boom"}`)
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.SaveHomeLayout(context.Background(), sampleAuth(),
		[]byte(`{"pins":[],"updated_at":"2026-07-18T12:00:00Z"}`))
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 on upstream 5xx", resp.Status)
	}
}

func TestSaveHomeLayout_Downstream422_PassesThrough(t *testing.T) {
	v := `{"code":"IDENTITY_INVALID_HOME_UPDATED_AT","message":"updated_at must be an RFC3339 timestamp"}`
	cb := newCaptureBackend(t, http.StatusUnprocessableEntity, v)
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.SaveHomeLayout(context.Background(), sampleAuth(),
		[]byte(`{"pins":[{"id":"wallet"}],"updated_at":"garbage"}`))
	if resp.Status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d; want 422 passed through verbatim", resp.Status)
	}
	if string(resp.Body) != v {
		t.Errorf("body = %q; want downstream 422 body verbatim", string(resp.Body))
	}
}

func TestSaveHomeLayout_EmptyPins_ProxiesThrough_D7(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{}`)
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.SaveHomeLayout(context.Background(), sampleAuth(),
		[]byte(`{"pins":[],"updated_at":"2026-07-18T12:00:00Z"}`))
	if resp.Status != http.StatusOK || cb.calls != 1 {
		t.Errorf("empty home (D7) must proxy through; status=%d calls=%d", resp.Status, cb.calls)
	}
	if !strings.Contains(cb.body, `"pins":[]`) {
		t.Errorf("empty pins must forward verbatim; body=%q", cb.body)
	}
}
