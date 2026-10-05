// gatewayproxy_preferences_handler_internal_test.go — SP2.9 handler specs.
//
// Internal (package httpadapter) test: the handler methods are unexported and
// the bridge's 4-list route sync (NewGatewayProxyMux / matchesGatewayProxyPath /
// GatewayProxyPathPrefixes / DefaultJWTGatedPrefixes) is owned by the
// integration step (SP2.9 report), so these tests register the leaves on a LOCAL
// mux and drive them directly — verifying method-gating + auth extraction +
// delegation without depending on the shared dispatcher.
package httpadapter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

// identityCapture records the request the aggregator forwards to chora-identity.
type identityCapture struct {
	srv    *httptest.Server
	path   string
	method string
	gcid   string
	tenant string
	auth   string
	body   string
	calls  int
}

func newIdentityCapture(t *testing.T, status int, respBody string) *identityCapture {
	t.Helper()
	c := &identityCapture{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.calls++
		c.path, c.method = r.URL.Path, r.Method
		c.gcid, c.tenant = r.Header.Get("gcid"), r.Header.Get("X-Tenant-Id")
		c.auth = r.Header.Get("Authorization")
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
		}
		c.body = string(buf)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func prefsHandlerMux(identityURL string) (*http.ServeMux, *GatewayProxyHandler) {
	agg := gatewayproxy.New(gatewayproxy.Config{IdentityURL: identityURL, PerCallTimeout: time.Second})
	h := NewGatewayProxyHandler(agg)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/me/preferences", h.handleMePreferences)
	mux.HandleFunc("/api/me/preferences/dashboard-layout", h.handleDashboardLayout)
	mux.HandleFunc("/api/me/preferences/home-layout", h.handleHomeLayout)
	return mux, h
}

func prefsReq(method, path, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer raw-session-jwt")
	// Stamp the validated mesh claims the gateway JWT middleware would set.
	return r.WithContext(InjectMeshClaimsForTest(r.Context(),
		"01970000-0000-7000-8000-0000000000aa", "01970000-0000-7000-8000-0000000000bb"))
}

func TestHandleMePreferences_GET_ProxiesWithMeshHeaders(t *testing.T) {
	cap := newIdentityCapture(t, http.StatusOK,
		`{"dashboard_layout":{"order":["map","cast","courses"],"updated_at":"2026-07-06T12:00:00Z"}}`)
	mux, _ := prefsHandlerMux(cap.srv.URL)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, prefsReq(http.MethodGet, "/api/me/preferences", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", w.Code, w.Body.String())
	}
	if cap.path != "/api/v1/me/preferences" || cap.method != http.MethodGet {
		t.Errorf("downstream = %s %s; want GET /api/v1/me/preferences", cap.method, cap.path)
	}
	if cap.gcid != "01970000-0000-7000-8000-0000000000aa" {
		t.Errorf("downstream gcid = %q; want the mesh-claim gcid (never body)", cap.gcid)
	}
	if cap.tenant != "01970000-0000-7000-8000-0000000000bb" {
		t.Errorf("downstream X-Tenant-Id = %q; want the mesh tenant", cap.tenant)
	}
	if cap.auth != "Bearer raw-session-jwt" {
		t.Errorf("downstream Authorization = %q; want Bearer forwarded", cap.auth)
	}
	// Body passes through snake_case verbatim.
	if !strings.Contains(w.Body.String(), `"dashboard_layout"`) {
		t.Errorf("body = %q; want snake_case dashboard_layout verbatim", w.Body.String())
	}
}

func TestHandleMePreferences_NonGET_405_NoDownstream(t *testing.T) {
	cap := newIdentityCapture(t, http.StatusOK, `{}`)
	mux, _ := prefsHandlerMux(cap.srv.URL)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, prefsReq(http.MethodPost, "/api/me/preferences", ""))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/me/preferences = %d; want 405", w.Code)
	}
	if cap.calls != 0 {
		t.Errorf("405 must not reach the downstream; calls=%d", cap.calls)
	}
}

func TestHandleDashboardLayout_PUT_ProxiesBodyVerbatim(t *testing.T) {
	cap := newIdentityCapture(t, http.StatusOK, `{}`)
	mux, _ := prefsHandlerMux(cap.srv.URL)
	feBody := `{"order":["cast","map","courses"],"updated_at":"2026-07-06T12:00:00Z"}`
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, prefsReq(http.MethodPut, "/api/me/preferences/dashboard-layout", feBody))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", w.Code, w.Body.String())
	}
	if cap.path != "/api/v1/me/preferences/dashboard-layout" || cap.method != http.MethodPut {
		t.Errorf("downstream = %s %s; want PUT /api/v1/me/preferences/dashboard-layout", cap.method, cap.path)
	}
	if cap.body != feBody {
		t.Errorf("downstream body = %q; want verbatim %q", cap.body, feBody)
	}
}

func TestHandleDashboardLayout_UnknownKey_422_NoDownstream(t *testing.T) {
	cap := newIdentityCapture(t, http.StatusOK, `{}`)
	mux, _ := prefsHandlerMux(cap.srv.URL)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, prefsReq(http.MethodPut, "/api/me/preferences/dashboard-layout",
		`{"order":["map","atlas"],"updated_at":"2026-07-06T12:00:00Z"}`))
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown key = %d; want 422 at the edge", w.Code)
	}
	if cap.calls != 0 {
		t.Errorf("edge-rejected PUT must not reach the downstream; calls=%d", cap.calls)
	}
}

func TestHandleDashboardLayout_NonPUT_405(t *testing.T) {
	cap := newIdentityCapture(t, http.StatusOK, `{}`)
	mux, _ := prefsHandlerMux(cap.srv.URL)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, prefsReq(http.MethodGet, "/api/me/preferences/dashboard-layout", ""))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /dashboard-layout = %d; want 405 (PUT only)", w.Code)
	}
}

// ADR-240 Track B: the home-layout leaf forwards the body VERBATIM to
// chora-identity (no edge validation, D11).
func TestHandleHomeLayout_PUT_ProxiesBodyVerbatim(t *testing.T) {
	cap := newIdentityCapture(t, http.StatusOK, `{}`)
	mux, _ := prefsHandlerMux(cap.srv.URL)
	feBody := `{"pins":[{"id":"a-plus-dashboard","x":0},{"id":"wallet","x":1}],"updated_at":"2026-07-18T12:00:00Z"}`
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, prefsReq(http.MethodPut, "/api/me/preferences/home-layout", feBody))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", w.Code, w.Body.String())
	}
	if cap.path != "/api/v1/me/preferences/home-layout" || cap.method != http.MethodPut {
		t.Errorf("downstream = %s %s; want PUT /api/v1/me/preferences/home-layout", cap.method, cap.path)
	}
	if cap.body != feBody {
		t.Errorf("downstream body = %q; want verbatim %q", cap.body, feBody)
	}
	if cap.gcid != "01970000-0000-7000-8000-0000000000aa" {
		t.Errorf("downstream gcid = %q; want the mesh-claim gcid (never body)", cap.gcid)
	}
}

// D11: an unknown pin id is NOT rejected at the edge - it reaches the downstream.
func TestHandleHomeLayout_UnknownPinId_ReachesDownstream_D11(t *testing.T) {
	cap := newIdentityCapture(t, http.StatusOK, `{}`)
	mux, _ := prefsHandlerMux(cap.srv.URL)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, prefsReq(http.MethodPut, "/api/me/preferences/home-layout",
		`{"pins":[{"id":"never-heard-of-this-id"}],"updated_at":"2026-07-18T12:00:00Z"}`))
	if w.Code != http.StatusOK {
		t.Errorf("unknown pin id must NOT be edge-rejected (D11); got %d", w.Code)
	}
	if cap.calls != 1 {
		t.Errorf("home-layout PUT must reach the downstream; calls=%d", cap.calls)
	}
}

func TestHandleHomeLayout_NonPUT_405(t *testing.T) {
	cap := newIdentityCapture(t, http.StatusOK, `{}`)
	mux, _ := prefsHandlerMux(cap.srv.URL)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, prefsReq(http.MethodGet, "/api/me/preferences/home-layout", ""))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /home-layout = %d; want 405 (PUT only)", w.Code)
	}
	if cap.calls != 0 {
		t.Errorf("405 must not reach the downstream; calls=%d", cap.calls)
	}
}
