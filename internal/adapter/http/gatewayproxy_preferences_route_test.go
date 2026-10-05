// gatewayproxy_preferences_route_test.go — SP2.9 (CHO-2048): HTTP route-binding
// guard for the A+ dashboard-as-hub UI preferences leaves. The bridge must OWN
// /api/me/preferences (GET) + /api/me/preferences/dashboard-layout (PUT), and
// DefaultJWTGatedPrefixes must cover them so mesh claims are stamped before the
// downstream chora-identity handler runs. Mirrors the dose-preferences guard so
// a future 4-list refactor can't silently drop the route (project_gateway_route
// _three_list_sync).
package httpadapter_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

func TestWithGatewayProxy_PreferencesPath_HandledByBridge(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	agg := gatewayproxy.New(gatewayproxy.Config{
		IdentityURL:    stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/me/preferences"},
		{http.MethodPut, "/api/me/preferences/dashboard-layout"},
		{http.MethodPut, "/api/me/preferences/home-layout"},
	} {
		r := httptest.NewRequestWithContext(
			context.Background(),
			tc.method,
			tc.path,
			strings.NewReader(`{"order":["map","cast","courses"],"updated_at":"2026-07-07T00:00:00Z"}`),
		)
		r.Header.Set("Authorization", "Bearer t")
		r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusTeapot {
			t.Errorf("%s %s: leaked to base — bridge must own the preferences route", tc.method, tc.path)
		}
	}
}

func TestDefaultJWTGatedPrefixes_CoversPreferences(t *testing.T) {
	for _, p := range []string{
		"/api/me/preferences",
		"/api/me/preferences/dashboard-layout",
		"/api/me/preferences/home-layout",
	} {
		covered := false
		for _, prefix := range httpadapter.DefaultJWTGatedPrefixes {
			if strings.HasPrefix(p, prefix) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("path %q is NOT covered by DefaultJWTGatedPrefixes — JWT gate would skip, mesh claims empty, chora-identity would 4xx", p)
		}
	}
}
