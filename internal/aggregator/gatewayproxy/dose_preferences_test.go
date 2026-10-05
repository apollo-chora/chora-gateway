// dose_preferences_test.go — CHO-2045 / ADR-224: ProxyDosePreferences is a
// verbatim /api-stripped passthrough for the A+ Dose Preferences leaf (GET read
// + PUT replace) → chora-consumption /v1/me/dose-preferences. Mirrors ProxyGoals.
package gatewayproxy_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

func TestProxyDosePreferences_PathTranslatedMethodPreserved(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		cb := newCaptureBackend(t, http.StatusOK, `{"excludedMapIds":[]}`)
		a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

		resp, err := a.ProxyDosePreferences(context.Background(), sampleAuth(),
			method, "/api/v1/me/dose-preferences", "", nil, "application/json")
		if err != nil {
			t.Fatalf("%s err: %v", method, err)
		}
		if resp.Status != http.StatusOK {
			t.Errorf("%s status = %d; want 200", method, resp.Status)
		}
		if cb.path != "/v1/me/dose-preferences" {
			t.Errorf("%s downstream path = %q; want /v1/me/dose-preferences", method, cb.path)
		}
		if cb.method != method {
			t.Errorf("downstream method = %q; want %q", cb.method, method)
		}
		// Mesh-trust headers the downstream requireContext reads.
		if cb.hdr.Get("X-Tenant-Id") != sampleAuth().TenantID {
			t.Errorf("X-Tenant-Id = %q; want %q", cb.hdr.Get("X-Tenant-Id"), sampleAuth().TenantID)
		}
		if cb.hdr.Get("gcid") != sampleAuth().GCID {
			t.Errorf("lowercase gcid = %q; want %q", cb.hdr.Get("gcid"), sampleAuth().GCID)
		}
	}
}
