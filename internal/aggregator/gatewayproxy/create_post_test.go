// create_post_test.go — TDD for the async daily-dose AI enrichment BFF route.
package gatewayproxy_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

func TestAggregator_GetDailyDoseAI_ProxiesToConsumption(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"greeting":"hi","ai_picks":["a"],"degraded":false}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, err := a.GetDailyDoseAI(context.Background(), sampleAuth())
	if err != nil {
		t.Fatalf("GetDailyDoseAI err = %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.calls != 1 || cb.method != http.MethodGet || cb.path != "/companion/daily-dose/ai" {
		t.Errorf("downstream = %d %s %s; want 1 GET /companion/daily-dose/ai", cb.calls, cb.method, cb.path)
	}
	if cb.hdr.Get("X-Tenant-Id") == "" || cb.hdr.Get("gcid") == "" {
		t.Errorf("X-Tenant-Id + gcid must be stamped; tenant=%q gcid=%q",
			cb.hdr.Get("X-Tenant-Id"), cb.hdr.Get("gcid"))
	}
}
