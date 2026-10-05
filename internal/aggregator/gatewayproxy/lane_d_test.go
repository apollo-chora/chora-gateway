// lane_d_test.go — gateway proxy tests for the Tier 1 routes shipped by
// Lanes A (test-sets), B (assessments + me/assessments), and C
// (atoms/questions/search), 2026-05-16.
//
// Closes #60 — chora-gateway BFF proxy claims for the routes that returned
// GATEWAY_ROUTE_NOT_FOUND at api.chora.site because gateway didn't route them.
//
// Each method is a verbatim passthrough:
//   - request body + query string + path params forwarded unchanged
//   - method preserved (GET / POST / PATCH / DELETE)
//   - mesh-trust headers stamped via the canonical call() helper
//   - 5xx → 502 GATEWAY_UPSTREAM_5XX (only status normalisation)
//   - 2xx + 4xx pass through verbatim
//
// Mirrors the existing gatewayproxy_test.go conventions:
//   - captureBackend records the inbound request from the BFF proxy
//   - sampleAuth() supplies canonical mesh-claim values
package gatewayproxy_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

// -----------------------------------------------------------------------------
// ProxyTestSets — /api/v1/test-sets[/{...}] verbatim passthrough to chora-delivery
// -----------------------------------------------------------------------------

// Happy path — POST /api/v1/test-sets creates a test set.
func TestProxyTestSets_POST_Create_ProxiesToDelivery(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusCreated, `{"test_set_id":"ts-1","state":"DRAFT"}`)
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: cb.srv.URL, PerCallTimeout: time.Second})

	body := []byte(`{"title":"Smoke TS","description":"lane D"}`)
	resp, err := a.ProxyTestSets(context.Background(), sampleAuth(), http.MethodPost, "/api/v1/test-sets", "", body, "application/json")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", resp.Status)
	}
	// Verbatim path on chora-delivery.
	if cb.path != "/api/v1/test-sets" {
		t.Errorf("downstream path = %q; want /api/v1/test-sets", cb.path)
	}
	if cb.method != http.MethodPost {
		t.Errorf("method = %s; want POST", cb.method)
	}
	if cb.body != string(body) {
		t.Errorf("body = %q; want %q", cb.body, string(body))
	}
	// Mesh-trust headers.
	if cb.hdr.Get("X-Tenant-Id") != sampleAuth().TenantID {
		t.Errorf("X-Tenant-Id = %q; want %q", cb.hdr.Get("X-Tenant-Id"), sampleAuth().TenantID)
	}
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("gcid = %q; want %q", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
}

// GET /api/v1/test-sets/{id} preserves path params + GET.
func TestProxyTestSets_GET_ByID_PreservesPath(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"test_set_id":"ts-1"}`)
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.ProxyTestSets(context.Background(), sampleAuth(), http.MethodGet, "/api/v1/test-sets/ts-1", "", nil, "")
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/api/v1/test-sets/ts-1" {
		t.Errorf("downstream path = %q; want /api/v1/test-sets/ts-1", cb.path)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %s; want GET", cb.method)
	}
}

// Query string forwarded verbatim — listTestSets with state filter.
func TestProxyTestSets_GET_ForwardsRawQuery(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"items":[],"page":1}`)
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: cb.srv.URL, PerCallTimeout: time.Second})

	_, _ = a.ProxyTestSets(context.Background(), sampleAuth(), http.MethodGet, "/api/v1/test-sets", "state=PUBLISHED&page_size=20", nil, "")
	if cb.rawQ != "state=PUBLISHED&page_size=20" {
		t.Errorf("downstream rawQuery = %q; want state=PUBLISHED&page_size=20", cb.rawQ)
	}
}

// PATCH preserves method on /test-sets/{id} updateTestSet endpoint.
func TestProxyTestSets_PATCH_PreservesMethod(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"test_set_id":"ts-1","title":"new"}`)
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: cb.srv.URL, PerCallTimeout: time.Second})

	body := []byte(`{"title":"new"}`)
	resp, _ := a.ProxyTestSets(context.Background(), sampleAuth(), http.MethodPatch, "/api/v1/test-sets/ts-1", "", body, "application/json")
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.method != http.MethodPatch {
		t.Errorf("method = %s; want PATCH", cb.method)
	}
	if cb.body != string(body) {
		t.Errorf("body = %q; want %q", cb.body, string(body))
	}
}

// DELETE preserves method on /test-sets/{id}/questions/{qId} removeTestSetQuestion.
func TestProxyTestSets_DELETE_PreservesMethod_SubResource(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusNoContent, ``)
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.ProxyTestSets(context.Background(), sampleAuth(), http.MethodDelete, "/api/v1/test-sets/ts-1/questions/q-1", "", nil, "")
	if resp.Status != http.StatusNoContent {
		t.Errorf("status = %d; want 204", resp.Status)
	}
	if cb.method != http.MethodDelete {
		t.Errorf("method = %s; want DELETE", cb.method)
	}
	if cb.path != "/api/v1/test-sets/ts-1/questions/q-1" {
		t.Errorf("downstream path = %q; want /api/v1/test-sets/ts-1/questions/q-1", cb.path)
	}
}

// 5xx upstream → 502 normalisation.
func TestProxyTestSets_Upstream5xx_502(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusInternalServerError, `{"error":"boom"}`)
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.ProxyTestSets(context.Background(), sampleAuth(), http.MethodGet, "/api/v1/test-sets", "", nil, "")
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 on upstream 5xx", resp.Status)
	}
}

// 4xx error pass-through — downstream 404 surfaces verbatim.
func TestProxyTestSets_Upstream404_PassThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusNotFound, `{"error":"test set not found"}`)
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.ProxyTestSets(context.Background(), sampleAuth(), http.MethodGet, "/api/v1/test-sets/missing", "", nil, "")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 passthrough", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "test set not found") {
		t.Errorf("body must passthrough 404 envelope; got %q", string(resp.Body))
	}
}

// -----------------------------------------------------------------------------
// ProxyAssessments — /api/v1/assessments[/{...}] verbatim passthrough
// -----------------------------------------------------------------------------

func TestProxyAssessments_POST_Create_ProxiesToDelivery(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusCreated, `{"id":"a-1","state":"DRAFT"}`)
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: cb.srv.URL, PerCallTimeout: time.Second})

	body := []byte(`{"test_set_id":"ts-1","title":"Smoke","invited_gcids":["00000000-0000-7000-8000-000000001999"]}`)
	resp, _ := a.ProxyAssessments(context.Background(), sampleAuth(), http.MethodPost, "/api/v1/assessments", "", body, "application/json")
	if resp.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", resp.Status)
	}
	if cb.path != "/api/v1/assessments" {
		t.Errorf("downstream path = %q; want /api/v1/assessments", cb.path)
	}
	if cb.method != http.MethodPost {
		t.Errorf("method = %s; want POST", cb.method)
	}
	if cb.body != string(body) {
		t.Errorf("body = %q; want %q", cb.body, string(body))
	}
}

func TestProxyAssessments_GET_Monitor_PreservesPathParam(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"invited_count":3}`)
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.ProxyAssessments(context.Background(), sampleAuth(), http.MethodGet, "/api/v1/assessments/a-1/monitor", "", nil, "")
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/api/v1/assessments/a-1/monitor" {
		t.Errorf("downstream path = %q; want /api/v1/assessments/a-1/monitor", cb.path)
	}
}

func TestProxyAssessments_GET_ForwardsRawQuery(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"items":[]}`)
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: cb.srv.URL, PerCallTimeout: time.Second})
	_, _ = a.ProxyAssessments(context.Background(), sampleAuth(), http.MethodGet, "/api/v1/assessments/a-1/submissions", "state=GRADED", nil, "")
	if cb.rawQ != "state=GRADED" {
		t.Errorf("downstream rawQuery = %q; want state=GRADED", cb.rawQ)
	}
}

// POST /api/v1/assessments/{id}/release-results — the critical demo CTA.
func TestProxyAssessments_POST_ReleaseResults(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"released":true}`)
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.ProxyAssessments(context.Background(), sampleAuth(), http.MethodPost, "/api/v1/assessments/a-1/release-results", "", []byte(`{}`), "application/json")
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/api/v1/assessments/a-1/release-results" {
		t.Errorf("downstream path = %q; want /api/v1/assessments/a-1/release-results", cb.path)
	}
}

func TestProxyAssessments_Upstream5xx_502(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusInternalServerError, `{"error":"boom"}`)
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.ProxyAssessments(context.Background(), sampleAuth(), http.MethodGet, "/api/v1/assessments/x", "", nil, "")
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", resp.Status)
	}
}

func TestProxyAssessments_Upstream403_PassThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusForbidden, `{"error":"caller lacks instructor role"}`)
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.ProxyAssessments(context.Background(), sampleAuth(), http.MethodPost, "/api/v1/assessments", "", []byte(`{}`), "application/json")
	if resp.Status != http.StatusForbidden {
		t.Errorf("status = %d; want 403 passthrough", resp.Status)
	}
}

// -----------------------------------------------------------------------------
// ProxyMeAssessments — /api/v1/me/assessments[/{...}] verbatim passthrough
// (learner side — chora-delivery handler enforces self-gcid cohort gate)
// -----------------------------------------------------------------------------

func TestProxyMeAssessments_GET_List_ProxiesToDelivery(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"items":[{"id":"a-1"}]}`)
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.ProxyMeAssessments(context.Background(), sampleAuth(), http.MethodGet, "/api/v1/me/assessments", "", nil, "")
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/api/v1/me/assessments" {
		t.Errorf("downstream path = %q; want /api/v1/me/assessments", cb.path)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %s; want GET", cb.method)
	}
	// Self-cohort enforcement — gateway forwards mesh claims so the downstream
	// handler can scope to the caller GCID.
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("gcid header = %q; want %q (self-cohort enforcement)", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
}

// POST /api/v1/me/assessments/{id}/submissions startMySubmission.
func TestProxyMeAssessments_POST_StartSubmission(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusCreated, `{"submission_id":"sub-1","state":"IN_PROGRESS"}`)
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.ProxyMeAssessments(context.Background(), sampleAuth(), http.MethodPost, "/api/v1/me/assessments/a-1/submissions", "", []byte(`{}`), "application/json")
	if resp.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", resp.Status)
	}
	if cb.path != "/api/v1/me/assessments/a-1/submissions" {
		t.Errorf("downstream path = %q; want /api/v1/me/assessments/a-1/submissions", cb.path)
	}
}

// PATCH /api/v1/me/assessments/{id}/submissions/{subId}/autosave — method preservation.
func TestProxyMeAssessments_PATCH_Autosave_PreservesMethod(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"saved_at":"2026-05-16T00:00:00Z"}`)
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: cb.srv.URL, PerCallTimeout: time.Second})

	body := []byte(`{"answers":[{"question_id":"q-1","answer_text":"x"}]}`)
	resp, _ := a.ProxyMeAssessments(context.Background(), sampleAuth(), http.MethodPatch, "/api/v1/me/assessments/a-1/submissions/sub-1/autosave", "", body, "application/json")
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.method != http.MethodPatch {
		t.Errorf("method = %s; want PATCH", cb.method)
	}
	if cb.body != string(body) {
		t.Errorf("body lost in passthrough; want %q got %q", string(body), cb.body)
	}
}

func TestProxyMeAssessments_Upstream5xx_502(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusInternalServerError, `{"error":"boom"}`)
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.ProxyMeAssessments(context.Background(), sampleAuth(), http.MethodGet, "/api/v1/me/assessments", "", nil, "")
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", resp.Status)
	}
}

// -----------------------------------------------------------------------------
// ProxyQuestionSearch — GET /api/atoms/questions/search verbatim passthrough
// to chora-creation. The downstream serves at the SAME path.
// -----------------------------------------------------------------------------

func TestProxyQuestionSearch_GET_ProxiesToCreation(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"items":[],"page":1,"per":20,"total":0}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.ProxyQuestionSearch(context.Background(), sampleAuth(), "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/api/atoms/questions/search" {
		t.Errorf("downstream path = %q; want /api/atoms/questions/search", cb.path)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %s; want GET", cb.method)
	}
}

func TestProxyQuestionSearch_ForwardsQueryString(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"items":[]}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	_, _ = a.ProxyQuestionSearch(context.Background(), sampleAuth(), "q=math&types=mcq,oe&per=10")
	if cb.rawQ != "q=math&types=mcq,oe&per=10" {
		t.Errorf("downstream rawQuery = %q; want q=math&types=mcq,oe&per=10", cb.rawQ)
	}
}

func TestProxyQuestionSearch_Upstream5xx_502(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusInternalServerError, `{"error":"boom"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.ProxyQuestionSearch(context.Background(), sampleAuth(), "")
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", resp.Status)
	}
}

func TestProxyQuestionSearch_Upstream401_PassThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusUnauthorized, `{"error":"unauthorized"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.ProxyQuestionSearch(context.Background(), sampleAuth(), "")
	if resp.Status != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401 passthrough", resp.Status)
	}
}
