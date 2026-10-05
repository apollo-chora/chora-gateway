// rplus_delivery_test.go — TDD coverage for ProxyDeliveryVerbatim. The
// aggregator method is the generic R+ delivery passthrough; each test
// asserts that one resource group's typical operation forwards correctly
// (method + path + body + query) and that mesh-trust headers are stamped.
//
// Strict TDD per feedback_strict_tdd. Tests written BEFORE the handler
// layer registrations; the handler layer is exercised separately in
// rplus_delivery_proxy_handler_test.go.

package gatewayproxy_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

func newRplusAggregator(t *testing.T, stubURL string) *gatewayproxy.Aggregator {
	t.Helper()
	agg := gatewayproxy.New(gatewayproxy.Config{
		DeliveryURL:    stubURL,
		PerCallTimeout: time.Second,
	})
	if agg == nil {
		t.Fatal("aggregator nil — DeliveryURL should have wired it")
	}
	return agg
}

func newRplusStub(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func authCtx() gatewayproxy.AuthCtx {
	return gatewayproxy.AuthCtx{
		Bearer:   "test-bearer",
		TenantID: "tenant-001",
		GCID:     "gcid-001",
	}
}

// -----------------------------------------------------------------------------
// /api/bookings — verbatim
// -----------------------------------------------------------------------------

func TestProxyDeliveryVerbatim_Bookings_POST_ProxiesToDelivery(t *testing.T) {
	var gotPath, gotMethod, gotTenant, gotGCID string
	var gotBody string
	stub := newRplusStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotTenant = r.Header.Get("X-Tenant-Id")
		gotGCID = r.Header.Get("gcid")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"booking_id":"b-1","state":"PENDING"}`))
	})
	agg := newRplusAggregator(t, stub.URL)

	resp, err := agg.ProxyDeliveryVerbatim(
		context.Background(), authCtx(),
		http.MethodPost, "/api/bookings", "",
		[]byte(`{"course_id":"c-1","gcid":"gcid-001"}`),
		"application/json",
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", resp.Status)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s; want POST", gotMethod)
	}
	if gotPath != "/api/bookings" {
		t.Errorf("downstream path = %q; want /api/bookings", gotPath)
	}
	if gotTenant != "tenant-001" {
		t.Errorf("X-Tenant-Id = %q; want tenant-001", gotTenant)
	}
	if gotGCID != "gcid-001" {
		t.Errorf("gcid = %q; want gcid-001", gotGCID)
	}
	if !strings.Contains(gotBody, `"course_id":"c-1"`) {
		t.Errorf("body = %q; want includes course_id", gotBody)
	}
	if !bytes.Contains(resp.Body, []byte(`"booking_id"`)) {
		t.Errorf("response body lost; got %q", string(resp.Body))
	}
}

func TestProxyDeliveryVerbatim_Bookings_GET_ByID_PreservesPath(t *testing.T) {
	var gotPath string
	stub := newRplusStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"booking_id":"b-1"}`))
	})
	agg := newRplusAggregator(t, stub.URL)

	resp, _ := agg.ProxyDeliveryVerbatim(
		context.Background(), authCtx(),
		http.MethodGet, "/api/bookings/b-1", "", nil, "",
	)
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.Status)
	}
	if gotPath != "/api/bookings/b-1" {
		t.Errorf("downstream path = %q; want /api/bookings/b-1", gotPath)
	}
}

// -----------------------------------------------------------------------------
// /api/certifications — verbatim
// -----------------------------------------------------------------------------

func TestProxyDeliveryVerbatim_Certifications_GET_List(t *testing.T) {
	var gotPath, gotQuery string
	stub := newRplusStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	})
	agg := newRplusAggregator(t, stub.URL)

	resp, _ := agg.ProxyDeliveryVerbatim(
		context.Background(), authCtx(),
		http.MethodGet, "/api/certifications", "state=ACTIVE&page=2", nil, "",
	)
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.Status)
	}
	if gotPath != "/api/certifications" {
		t.Errorf("downstream path = %q; want /api/certifications", gotPath)
	}
	if gotQuery != "state=ACTIVE&page=2" {
		t.Errorf("downstream rawQuery = %q; want state=ACTIVE&page=2", gotQuery)
	}
}

// -----------------------------------------------------------------------------
// /v1/campus — verbatim
// -----------------------------------------------------------------------------

func TestProxyDeliveryVerbatim_Campus_POST_PreservesContentType(t *testing.T) {
	var gotPath, gotContentType string
	stub := newRplusStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"campus_id":"camp-1"}`))
	})
	agg := newRplusAggregator(t, stub.URL)

	resp, _ := agg.ProxyDeliveryVerbatim(
		context.Background(), authCtx(),
		http.MethodPost, "/v1/campus", "",
		[]byte(`{"name":"Tampines","capacity":50}`),
		"application/json",
	)
	if resp.Status != http.StatusCreated {
		t.Fatalf("status = %d; want 201", resp.Status)
	}
	if gotPath != "/v1/campus" {
		t.Errorf("downstream path = %q; want /v1/campus", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q; want application/json", gotContentType)
	}
}

// -----------------------------------------------------------------------------
// /v1/me/applications — verbatim (ADR-164 Stage C surface)
// -----------------------------------------------------------------------------

func TestProxyDeliveryVerbatim_MeApplications_GET_List(t *testing.T) {
	var gotPath string
	stub := newRplusStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	})
	agg := newRplusAggregator(t, stub.URL)

	resp, _ := agg.ProxyDeliveryVerbatim(
		context.Background(), authCtx(),
		http.MethodGet, "/v1/me/applications", "", nil, "",
	)
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.Status)
	}
	if gotPath != "/v1/me/applications" {
		t.Errorf("downstream path = %q; want /v1/me/applications", gotPath)
	}
}

func TestProxyDeliveryVerbatim_MeApplications_POST_SubPath(t *testing.T) {
	var gotPath string
	stub := newRplusStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"checkout_url":"https://stripe.example/cs_test_xxx"}`))
	})
	agg := newRplusAggregator(t, stub.URL)

	resp, _ := agg.ProxyDeliveryVerbatim(
		context.Background(), authCtx(),
		http.MethodPost, "/v1/me/applications/app-1/accept-offer", "",
		[]byte(`{}`), "application/json",
	)
	if resp.Status != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", resp.Status)
	}
	if gotPath != "/v1/me/applications/app-1/accept-offer" {
		t.Errorf("downstream path = %q; want /v1/me/applications/app-1/accept-offer", gotPath)
	}
}

// -----------------------------------------------------------------------------
// Status normalisation — 5xx → 502
// -----------------------------------------------------------------------------

func TestProxyDeliveryVerbatim_Upstream5xx_NormalisedTo502(t *testing.T) {
	stub := newRplusStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"DB connection lost"}`))
	})
	agg := newRplusAggregator(t, stub.URL)

	resp, _ := agg.ProxyDeliveryVerbatim(
		context.Background(), authCtx(),
		http.MethodGet, "/api/bookings", "", nil, "",
	)
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 (5xx normalisation)", resp.Status)
	}
}

func TestProxyDeliveryVerbatim_Downstream4xx_PassThrough(t *testing.T) {
	stub := newRplusStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"DELIVERY_INSUFFICIENT_ROLE"}`))
	})
	agg := newRplusAggregator(t, stub.URL)

	resp, _ := agg.ProxyDeliveryVerbatim(
		context.Background(), authCtx(),
		http.MethodGet, "/v1/me/applications", "", nil, "",
	)
	if resp.Status != http.StatusForbidden {
		t.Errorf("status = %d; want 403 verbatim", resp.Status)
	}
	if !bytes.Contains(resp.Body, []byte("INSUFFICIENT_ROLE")) {
		t.Errorf("body = %q; want passthrough error envelope", string(resp.Body))
	}
}
