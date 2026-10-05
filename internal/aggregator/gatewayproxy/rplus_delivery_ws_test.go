// rplus_delivery_ws_test.go — TDD coverage for the WebSocket-upgrade
// passthrough support surface on the Aggregator (ADR-168 classroom realtime
// / ADR-166 §D5). The R+ live-quiz / live-poll FE opens WebSockets at
// /api/v1/live-quizzes/{sessionId}/ws and /api/v1/live-polls/{pollId}/ws;
// the gateway must dial the SAME chora-delivery downstream the verbatim HTTP
// proxy uses. These tests pin the two support methods the adapter-layer WS
// proxy relies on:
//
//   - DeliveryBaseURL() returns the env-sourced SVC_DELIVERY_URL base so the
//     adapter dials the right host:port (no inline config).
//   - StampDownstreamHeaders(req, auth) stamps the canonical mesh-trust
//     headers on the raw upgrade request the adapter writes downstream.
//
// Strict TDD per feedback_strict_tdd — RED before the support methods exist.
package gatewayproxy_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

func TestDeliveryBaseURL_ReturnsConfiguredBase(t *testing.T) {
	agg := gatewayproxy.New(gatewayproxy.Config{
		DeliveryURL:    "http://chora-delivery.delivery.svc.cluster.local:8080",
		PerCallTimeout: time.Second,
	})
	if agg == nil {
		t.Fatal("aggregator nil — DeliveryURL should have wired it")
	}
	if got := agg.DeliveryBaseURL(); got != "http://chora-delivery.delivery.svc.cluster.local:8080" {
		t.Errorf("DeliveryBaseURL() = %q; want the configured SVC_DELIVERY_URL base", got)
	}
}

func TestStampDownstreamHeaders_StampsMeshTrust(t *testing.T) {
	agg := gatewayproxy.New(gatewayproxy.Config{
		DeliveryURL:    "http://stub",
		PerCallTimeout: time.Second,
	})
	if agg == nil {
		t.Fatal("aggregator nil")
	}
	req, _ := http.NewRequest(http.MethodGet, "http://stub/api/v1/live-polls/p-1/ws", nil)
	agg.StampDownstreamHeaders(req, gatewayproxy.AuthCtx{
		Bearer:   "tok",
		TenantID: "tenant-001",
		GCID:     "gcid-001",
	})
	if req.Header.Get("Authorization") != "Bearer tok" {
		t.Errorf("Authorization = %q; want Bearer tok", req.Header.Get("Authorization"))
	}
	if req.Header.Get("X-Tenant-Id") != "tenant-001" {
		t.Errorf("X-Tenant-Id = %q; want tenant-001", req.Header.Get("X-Tenant-Id"))
	}
	if req.Header.Get("gcid") != "gcid-001" {
		t.Errorf("gcid = %q; want gcid-001", req.Header.Get("gcid"))
	}
}
