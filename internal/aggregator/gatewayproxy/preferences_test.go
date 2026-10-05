// preferences_test.go — SP2.9: RED-phase specs for the A+ dashboard-as-hub
// preference proxy → chora-identity. GET reads the caller's UI prefs; PUT
// upserts the dashboard layout after an edge validation of the order (unknown
// wrapper key → 422 WITHOUT a downstream round-trip). GCID is delegated to the
// downstream via the stamped mesh headers (never the body).
package gatewayproxy_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-common/uiprefs"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

func TestGetPreferences_ProxiesToIdentity(t *testing.T) {
	body := `{"dashboard_layout":{"order":["cast","map","courses"],"updated_at":"2026-07-06T12:00:00Z"}}`
	cb := newCaptureBackend(t, http.StatusOK, body)
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.GetPreferences(context.Background(), sampleAuth())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/api/v1/me/preferences" {
		t.Errorf("downstream path = %q; want /api/v1/me/preferences", cb.path)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %s; want GET", cb.method)
	}
	// Mesh-trust headers the downstream bearerAuth reads.
	if cb.hdr.Get("Authorization") != "Bearer raw-session-jwt" {
		t.Errorf("Authorization = %q; want Bearer forwarded", cb.hdr.Get("Authorization"))
	}
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("lowercase gcid = %q; want %q", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
	if cb.hdr.Get("X-Tenant-Id") != sampleAuth().TenantID {
		t.Errorf("X-Tenant-Id = %q; want %q", cb.hdr.Get("X-Tenant-Id"), sampleAuth().TenantID)
	}
	// Body passes through verbatim (snake_case — NOT camelised; gatewayproxy
	// classify is verbatim, unlike CompanionBridge).
	if !strings.Contains(string(resp.Body), `"dashboard_layout"`) ||
		!strings.Contains(string(resp.Body), `"updated_at"`) {
		t.Errorf("body = %q; want the snake_case dashboard_layout passed through verbatim", string(resp.Body))
	}
}

func TestGetPreferences_EmptyPrefs_PassThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{}`)
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.GetPreferences(context.Background(), sampleAuth())
	if resp.Status != http.StatusOK || string(resp.Body) != `{}` {
		t.Errorf("empty prefs {} must pass through verbatim; got %d %q", resp.Status, string(resp.Body))
	}
}

func TestGetPreferences_Upstream5xx_502(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusInternalServerError, `{"error":"boom"}`)
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.GetPreferences(context.Background(), sampleAuth())
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 on upstream 5xx", resp.Status)
	}
}

func TestSaveDashboardLayout_ProxiesToIdentity(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK,
		`{"dashboard_layout":{"order":["cast","map","courses"],"updated_at":"2026-07-06T12:00:00Z"}}`)
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: cb.srv.URL, PerCallTimeout: time.Second})

	feBody := []byte(`{"order":["cast","map","courses"],"updated_at":"2026-07-06T12:00:00Z"}`)
	resp, err := a.SaveDashboardLayout(context.Background(), sampleAuth(), feBody)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/api/v1/me/preferences/dashboard-layout" {
		t.Errorf("downstream path = %q; want /api/v1/me/preferences/dashboard-layout", cb.path)
	}
	if cb.method != http.MethodPut {
		t.Errorf("method = %s; want PUT", cb.method)
	}
	// Body forwarded verbatim (the downstream persists it).
	if cb.body != string(feBody) {
		t.Errorf("body forwarded = %q; want verbatim", cb.body)
	}
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("lowercase gcid = %q; want %q (never the body)", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
}

func TestSaveDashboardLayout_SubsetOK(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{}`)
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.SaveDashboardLayout(context.Background(), sampleAuth(),
		[]byte(`{"order":["map"],"updated_at":"2026-07-06T12:00:00Z"}`))
	if resp.Status != http.StatusOK {
		t.Errorf("subset order must proxy through; got %d", resp.Status)
	}
	if cb.calls != 1 {
		t.Errorf("valid layout must reach the downstream; calls = %d", cb.calls)
	}
}

func TestSaveDashboardLayout_UnknownKey_422_NoDownstream(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{}`)
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.SaveDashboardLayout(context.Background(), sampleAuth(),
		[]byte(`{"order":["map","atlas"],"updated_at":"2026-07-06T12:00:00Z"}`))
	if resp.Status != http.StatusUnprocessableEntity {
		t.Errorf("unknown wrapper key must be 422 at the edge; got %d", resp.Status)
	}
	if cb.calls != 0 {
		t.Errorf("edge-rejected layout must NOT reach the downstream; calls = %d", cb.calls)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_INVALID_DASHBOARD_LAYOUT") {
		t.Errorf("body = %q; want a GATEWAY_INVALID_DASHBOARD_LAYOUT envelope", string(resp.Body))
	}
}

func TestSaveDashboardLayout_EmptyOrder_422(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.SaveDashboardLayout(context.Background(), sampleAuth(),
		[]byte(`{"order":[],"updated_at":"2026-07-06T12:00:00Z"}`))
	if resp.Status != http.StatusUnprocessableEntity {
		t.Errorf("empty order must be 422; got %d", resp.Status)
	}
}

func TestSaveDashboardLayout_DuplicateKey_422(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.SaveDashboardLayout(context.Background(), sampleAuth(),
		[]byte(`{"order":["map","map"],"updated_at":"2026-07-06T12:00:00Z"}`))
	if resp.Status != http.StatusUnprocessableEntity {
		t.Errorf("duplicate key must be 422; got %d", resp.Status)
	}
}

func TestSaveDashboardLayout_BadJSON_400(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.SaveDashboardLayout(context.Background(), sampleAuth(), []byte(`{not json`))
	if resp.Status != http.StatusBadRequest {
		t.Errorf("malformed JSON must be 400; got %d", resp.Status)
	}
}

// A downstream 422 (authoritative identity validation, e.g. bad updated_at that
// the gateway does not check) passes through verbatim.
func TestSaveDashboardLayout_Downstream422_PassesThrough(t *testing.T) {
	v := `{"code":"IDENTITY_INVALID_DASHBOARD_UPDATED_AT","message":"updated_at must be an RFC3339 timestamp"}`
	cb := newCaptureBackend(t, http.StatusUnprocessableEntity, v)
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: cb.srv.URL, PerCallTimeout: time.Second})
	// order is valid so the gateway forwards; the downstream rejects the timestamp.
	resp, _ := a.SaveDashboardLayout(context.Background(), sampleAuth(),
		[]byte(`{"order":["map"],"updated_at":"garbage"}`))
	if resp.Status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d; want 422 passed through verbatim", resp.Status)
	}
	if string(resp.Body) != v {
		t.Errorf("body = %q; want downstream 422 body verbatim", string(resp.Body))
	}
}

func TestNew_NonNilWhenIdentityURLSet_Preferences(t *testing.T) {
	if a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: "http://identity"}); a == nil {
		t.Fatal("New with IdentityURL set must return a non-nil aggregator")
	}
}

// TestSaveDashboardLayout_CanonicalFullOrder_OK is the CHO-2274 regression: the
// full canonical wrapper order (which the deployed FE sends, including "study"
// and "transcript") must clear the edge guard and proxy through. Before the fix
// the edge map only knew {map,cast,courses} so this 422'd at the edge.
func TestSaveDashboardLayout_CanonicalFullOrder_OK(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{}`)
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: cb.srv.URL, PerCallTimeout: time.Second})

	order := `"` + strings.Join(uiprefs.DashboardWrapperKeys(), `","`) + `"`
	body := []byte(`{"order":[` + order + `],"updated_at":"2026-07-17T12:00:00Z"}`)
	resp, err := a.SaveDashboardLayout(context.Background(), sampleAuth(), body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("full canonical order must proxy through; got %d body %q", resp.Status, string(resp.Body))
	}
	if cb.calls != 1 {
		t.Errorf("valid full-canonical layout must reach the downstream; calls = %d", cb.calls)
	}
}

// TestSaveDashboardLayout_EdgeAcceptsEveryCanonicalKey is the drift guard: the
// edge validator must accept every key in the single canonical source, so the
// gateway map can never fall behind uiprefs by hand (CHO-2274). A key present in
// uiprefs but rejected at the edge fails here.
func TestSaveDashboardLayout_EdgeAcceptsEveryCanonicalKey(t *testing.T) {
	for _, k := range uiprefs.DashboardWrapperKeys() {
		cb := newCaptureBackend(t, http.StatusOK, `{}`)
		a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: cb.srv.URL, PerCallTimeout: time.Second})
		body := []byte(`{"order":["` + k + `"],"updated_at":"2026-07-17T12:00:00Z"}`)
		resp, _ := a.SaveDashboardLayout(context.Background(), sampleAuth(), body)
		if resp.Status != http.StatusOK {
			t.Errorf("canonical key %q rejected at the edge (status %d); the edge set drifted from uiprefs", k, resp.Status)
		}
	}
}
