// gatewayproxy_dose_preferences_test.go — CHO-2045 / ADR-224: HTTP route-binding
// tests for the A+ Dose Preferences leaf. The bridge must OWN /api/v1/me/dose-
// preferences (GET + PUT) and DefaultJWTGatedPrefixes must cover it so mesh claims
// are stamped before the downstream chora-consumption handler runs.
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

func TestWithGatewayProxy_DosePreferencesPath_HandledByBridge(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	agg := gatewayproxy.New(gatewayproxy.Config{
		ConsumptionURL: stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/me/dose-preferences"},
		{http.MethodPut, "/api/v1/me/dose-preferences"},
	} {
		r := httptest.NewRequestWithContext(context.Background(), tc.method, tc.path, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer t")
		r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusTeapot {
			t.Errorf("%s %s: leaked to base — bridge must own the dose-preferences route", tc.method, tc.path)
		}
	}
}

func TestDefaultJWTGatedPrefixes_CoversDosePreferences(t *testing.T) {
	p := "/api/v1/me/dose-preferences"
	covered := false
	for _, prefix := range httpadapter.DefaultJWTGatedPrefixes {
		if strings.HasPrefix(p, prefix) {
			covered = true
			break
		}
	}
	if !covered {
		t.Errorf("path %q is NOT covered by DefaultJWTGatedPrefixes — JWT gate would skip, mesh claims empty, chora-consumption would 4xx", p)
	}
}
