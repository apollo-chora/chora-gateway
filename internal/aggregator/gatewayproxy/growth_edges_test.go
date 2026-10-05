package gatewayproxy_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

// Epic-1b W8 — ProxyGrowthEdges proxies /api/v1/me/growth-edges to
// chora-consumption with the /api prefix stripped (→ /v1/me/growth-edges) and
// the filter/sort/page query string forwarded verbatim.
func TestProxyGrowthEdges_ListPathTranslatedQueryPreserved(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"growth_edges":[],"next_cursor":null}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.ProxyGrowthEdges(context.Background(), sampleAuth(),
		http.MethodGet, "/api/v1/me/growth-edges",
		"min_strength=0.3&sort=strength_desc&page_size=20", nil, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/v1/me/growth-edges" {
		t.Errorf("downstream path = %q; want /v1/me/growth-edges", cb.path)
	}
	if cb.rawQ != "min_strength=0.3&sort=strength_desc&page_size=20" {
		t.Errorf("downstream query = %q; want the verbatim filter/sort/page query", cb.rawQ)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %q; want GET", cb.method)
	}
}

// The multipart upload POST must reach /v1/me/growth-edges/uploads with the
// caller Content-Type (incl. the multipart boundary) and body forwarded
// verbatim, so chora-consumption can parse the marked-test file.
func TestProxyGrowthEdges_UploadPreservesMultipartContentType(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusAccepted, `{"upload_id":"u1","status":"QUEUED"}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	const ct = "multipart/form-data; boundary=----choraGrowthEdge"
	body := []byte("------choraGrowthEdge\r\nContent-Disposition: form-data; name=\"upload_kind\"\r\n\r\nmarked_test\r\n------choraGrowthEdge--\r\n")

	resp, err := a.ProxyGrowthEdges(context.Background(), sampleAuth(),
		http.MethodPost, "/api/v1/me/growth-edges/uploads", "", body, ct)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusAccepted {
		t.Errorf("status = %d; want 202", resp.Status)
	}
	if cb.path != "/v1/me/growth-edges/uploads" {
		t.Errorf("downstream path = %q; want /v1/me/growth-edges/uploads", cb.path)
	}
	if cb.method != http.MethodPost {
		t.Errorf("method = %q; want POST", cb.method)
	}
	if got := cb.hdr.Get("Content-Type"); got != ct {
		t.Errorf("downstream Content-Type = %q; want the multipart boundary preserved (%q)", got, ct)
	}
	if cb.body != string(body) {
		t.Errorf("downstream body not forwarded verbatim")
	}
}
