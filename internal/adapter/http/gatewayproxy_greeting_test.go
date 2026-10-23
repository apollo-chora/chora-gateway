// gatewayproxy_greeting_test.go: K49 (kiosk D8, ADR-259 D11). The edge
// forwards POST /api/companion/greeting verbatim to chora-consumption's
// /companion/greeting (the greeting-only call for one named Companion), with
// the long timeout the greeting turn needs; consumption's answer, a failure
// included, passes through unchanged.
package httpadapter_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

func k49GatewayMux(t *testing.T, status int, body string) (http.Handler, *struct{ method, path, body string }) {
	t.Helper()
	seen := &struct{ method, path, body string }{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen.method, seen.path, seen.body = r.Method, r.URL.Path, string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(up.Close)
	agg := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: up.URL, PerCallTimeout: time.Second, PostCreateTimeout: 5 * time.Second})
	return httpadapter.NewGatewayProxyMux(agg), seen
}

func TestGwRoutes_K49_GreetingForwardedVerbatim(t *testing.T) {
	mux, seen := k49GatewayMux(t, http.StatusOK, `{"greeting":"Hoo!","model":"glm-5.3-flash","replayed":false}`)
	reqBody := `{"companion_id":"01a1411f-0000-7000-8000-00000000a0a2","turn_id":"01a1411f-0000-7000-8000-0000000000f1"}`
	w := doGwProxyReq(t, mux, http.MethodPost, "/api/companion/greeting", reqBody)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"greeting":"Hoo!"`) {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	if seen.method != http.MethodPost || seen.path != "/companion/greeting" || seen.body != reqBody {
		t.Fatalf("downstream saw %s %s %q", seen.method, seen.path, seen.body)
	}
}

func TestGwRoutes_K49_FailurePassesThrough(t *testing.T) {
	mux, _ := k49GatewayMux(t, http.StatusServiceUnavailable, `{"error":"GREETING_FAILED","turn_id":"t"}`)
	w := doGwProxyReq(t, mux, http.MethodPost, "/api/companion/greeting", `{"companion_id":"c"}`)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "GREETING_FAILED") || !strings.Contains(w.Body.String(), `"turn_id":"t"`) {
		t.Fatalf("a failed greeting must reach the client as itself, got %d %s", w.Code, w.Body.String())
	}
}

func TestGwRoutes_K49_PostOnly(t *testing.T) {
	mux, seen := k49GatewayMux(t, http.StatusOK, `{}`)
	if w := doGwProxyReq(t, mux, http.MethodGet, "/api/companion/greeting", ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: want 405, got %d", w.Code)
	}
	if seen.path != "" {
		t.Fatalf("a refused method must not reach consumption")
	}
}

func TestWithGatewayProxy_K49_GreetingOwnedByTheBridge(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(up.Close)
	h := httpadapter.WithGatewayProxy(base, gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: up.URL, PerCallTimeout: time.Second}))
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/companion/greeting", strings.NewReader(`{"companion_id":"c"}`))
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusTeapot {
		t.Fatalf("/api/companion/greeting leaked to the base router")
	}
}
