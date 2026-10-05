// gatewayproxy_test.go — RED-phase TDD specs for the A6 BFF proxy
// aggregator that wires the missing non-auth /api/* gateway routes per
// docs/m13/handoff-fe-to-be-service-2026-05-14.md §A6.
//
// FE probed each route with a real Bearer ChoraSession JWT and found most
// non-auth /api/* routes return GATEWAY_ROUTE_NOT_FOUND — they are not
// routed at the gateway at all. This aggregator fans the FE-facing routes
// out to their REAL downstream paths (verified to exist by reading each
// downstream's HTTP adapter):
//
//	GET  /api/feature-flags                  → chora-tenancy   GET /api/tenants/{tenantID}/entitlements
//	GET  /api/tenants/{id}                   → chora-tenancy   GET /api/tenants/{id}
//	GET  /api/courses/{id}                   → chora-delivery  GET /v1/courses/{id}
//	POST /api/courses/{id}/enrol             → chora-delivery  POST /v1/courses/{id}/enrolments
//	GET  /api/me/knowledge-graph/clusters    → chora-consumption GET /v1/me/knowledge-graph/clusters
//	GET  /api/me/companions                   → chora-consumption GET /v1/me/companions
//	GET  /api/me/companions/{id}/growth       → chora-consumption GET /v1/me/companions/{id}/growth
//	GET  /api/notifications                  → chora-notifications GET /api/notifications
//
// Strict TDD: tests written BEFORE the aggregator implementation.
package gatewayproxy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

// captureBackend is a minimal httptest.Server that records the inbound
// request from the BFF outbound call.
type captureBackend struct {
	srv    *httptest.Server
	hdr    http.Header
	path   string
	rawQ   string
	method string
	body   string
	calls  int
}

func newCaptureBackend(t *testing.T, status int, body string) *captureBackend {
	t.Helper()
	cb := &captureBackend{}
	cb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cb.calls++
		cb.hdr = r.Header.Clone()
		cb.path = r.URL.Path
		cb.rawQ = r.URL.RawQuery
		cb.method = r.Method
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
		}
		cb.body = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(cb.srv.Close)
	return cb
}

func sampleAuth() gatewayproxy.AuthCtx {
	return gatewayproxy.AuthCtx{
		Bearer:      "raw-session-jwt",
		Traceparent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		TenantID:    "01970000-0000-7000-8000-0000000000bb",
		GCID:        "01970000-0000-7000-8000-0000000000aa",
	}
}

// -----------------------------------------------------------------------------
// New / config
// -----------------------------------------------------------------------------

func TestNew_NilWhenAllURLsUnset(t *testing.T) {
	if a := gatewayproxy.New(gatewayproxy.Config{}); a != nil {
		t.Fatal("New with no URLs must return nil so callers can route-skip")
	}
}

func TestNew_NonNilWhenAnyURLSet(t *testing.T) {
	if a := gatewayproxy.New(gatewayproxy.Config{TenancyURL: "http://tenancy"}); a == nil {
		t.Fatal("New with a URL set must return a non-nil aggregator")
	}
}

// -----------------------------------------------------------------------------
// GET /api/feature-flags → chora-tenancy /api/tenants/{tenantID}/entitlements
// -----------------------------------------------------------------------------

func TestGetFeatureFlags_ProxiesToTenantEntitlements(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"items":[],"total":0}`)
	a := gatewayproxy.New(gatewayproxy.Config{TenancyURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.GetFeatureFlags(context.Background(), sampleAuth())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	wantPath := "/api/tenants/01970000-0000-7000-8000-0000000000bb/entitlements"
	if cb.path != wantPath {
		t.Errorf("downstream path = %q; want %q", cb.path, wantPath)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %s; want GET", cb.method)
	}
	// Mesh-trust headers the downstream tenantRequired middleware reads.
	if cb.hdr.Get("X-Tenant-Id") != sampleAuth().TenantID {
		t.Errorf("X-Tenant-Id = %q; want %q", cb.hdr.Get("X-Tenant-Id"), sampleAuth().TenantID)
	}
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("lowercase gcid header = %q; want %q", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
	if cb.hdr.Get("Authorization") != "Bearer raw-session-jwt" {
		t.Errorf("Authorization = %q", cb.hdr.Get("Authorization"))
	}
}

func TestGetFeatureFlags_NoTenant_400(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{TenancyURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.GetFeatureFlags(context.Background(), gatewayproxy.AuthCtx{Bearer: "x"})
	if resp.Status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 when tenant_id missing from auth", resp.Status)
	}
}

func TestGetFeatureFlags_Upstream5xx_502(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusInternalServerError, `{"error":"boom"}`)
	a := gatewayproxy.New(gatewayproxy.Config{TenancyURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.GetFeatureFlags(context.Background(), sampleAuth())
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 on upstream 5xx", resp.Status)
	}
}

// -----------------------------------------------------------------------------
// GET /api/tenants/{id} → chora-tenancy /api/tenants/{id}
// -----------------------------------------------------------------------------

func TestGetTenant_ProxiesToTenancyApiTenants(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"id":"mtm-singapore","display_name":"MTM"}`)
	a := gatewayproxy.New(gatewayproxy.Config{TenancyURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.GetTenant(context.Background(), sampleAuth(), "mtm-singapore")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/api/tenants/mtm-singapore" {
		t.Errorf("downstream path = %q; want /api/tenants/mtm-singapore", cb.path)
	}
}

func TestGetTenant_EmptyID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{TenancyURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.GetTenant(context.Background(), sampleAuth(), "")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty tenant id", resp.Status)
	}
}

func TestGetTenant_DownstreamNotFound_PassThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusNotFound, `{"error":"tenant not found"}`)
	a := gatewayproxy.New(gatewayproxy.Config{TenancyURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.GetTenant(context.Background(), sampleAuth(), "ghost")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 passed through verbatim", resp.Status)
	}
}

// -----------------------------------------------------------------------------
// GET /api/courses/{id} → chora-delivery /v1/courses/{id}
// -----------------------------------------------------------------------------

func TestGetCourse_ProxiesToDeliveryV1Courses(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"id":"course-1","title":"CSM Prep"}`)
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.GetCourse(context.Background(), sampleAuth(), "course-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/v1/courses/course-1" {
		t.Errorf("downstream path = %q; want /v1/courses/course-1", cb.path)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %s; want GET", cb.method)
	}
}

func TestGetCourse_EmptyID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.GetCourse(context.Background(), sampleAuth(), "")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty course id", resp.Status)
	}
}

// -----------------------------------------------------------------------------
// POST /api/courses/{id}/enrol → chora-delivery POST /v1/courses/{id}/enrolments
// -----------------------------------------------------------------------------

func TestEnrolCourse_ProxiesToDeliveryV1Enrolments(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusCreated, `{"enrolment_id":"enr-1","status":"enrolled"}`)
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.EnrolCourse(context.Background(), sampleAuth(), "course-1", []byte(`{"gcid":"x"}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", resp.Status)
	}
	if cb.path != "/v1/courses/course-1/enrolments" {
		t.Errorf("downstream path = %q; want /v1/courses/course-1/enrolments", cb.path)
	}
	if cb.method != http.MethodPost {
		t.Errorf("method = %s; want POST", cb.method)
	}
	if cb.body != `{"gcid":"x"}` {
		t.Errorf("body forwarded = %q; want verbatim", cb.body)
	}
	// chora-delivery enrolment-create reads gcid from body OR header.
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("lowercase gcid header = %q; want %q", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
}

func TestEnrolCourse_EmptyID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.EnrolCourse(context.Background(), sampleAuth(), "", nil)
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty course id", resp.Status)
	}
}

// -----------------------------------------------------------------------------
// GET /api/me/knowledge-graph/clusters → chora-consumption /v1/me/knowledge-graph/clusters
// -----------------------------------------------------------------------------

func TestGetKGClusters_ProxiesToConsumptionV1MeKG(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"data":{"clusters":[],"capRemaining":3,"capMax":3}}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.GetKGClusters(context.Background(), sampleAuth())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/v1/me/knowledge-graph/clusters" {
		t.Errorf("downstream path = %q; want /v1/me/knowledge-graph/clusters", cb.path)
	}
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("lowercase gcid header = %q; want %q", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
}

// -----------------------------------------------------------------------------
// GET /api/me/companions → chora-consumption /v1/me/companions
// -----------------------------------------------------------------------------

func TestListCompanions_ProxiesToConsumptionV1MeCompanions(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"items":[]}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.ListCompanions(context.Background(), sampleAuth())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/v1/me/companions" {
		t.Errorf("downstream path = %q; want /v1/me/companions", cb.path)
	}
}

// -----------------------------------------------------------------------------
// GET /api/me/companions/{id}/growth → chora-consumption /v1/me/companions/{id}/growth
// -----------------------------------------------------------------------------

func TestGetCompanionGrowth_ProxiesToConsumptionGrowth(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"companion_id":"fam-1","stage":3}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.GetCompanionGrowth(context.Background(), sampleAuth(), "fam-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/v1/me/companions/fam-1/growth" {
		t.Errorf("downstream path = %q; want /v1/me/companions/fam-1/growth", cb.path)
	}
}

func TestGetCompanionGrowth_EmptyID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.GetCompanionGrowth(context.Background(), sampleAuth(), "")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty companion id", resp.Status)
	}
}

// -----------------------------------------------------------------------------
// GET /api/notifications → chora-notifications GET /api/notifications
// -----------------------------------------------------------------------------

func TestListNotifications_ProxiesToNotifications(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"items":[],"total":0}`)
	a := gatewayproxy.New(gatewayproxy.Config{NotificationsURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.ListNotifications(context.Background(), sampleAuth(), "limit=20")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/api/notifications" {
		t.Errorf("downstream path = %q; want /api/notifications", cb.path)
	}
	if cb.rawQ != "limit=20" {
		t.Errorf("raw query = %q; want limit=20 forwarded verbatim", cb.rawQ)
	}
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("lowercase gcid header = %q; want %q", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
}

// -----------------------------------------------------------------------------
// A6 follow-up (CHO-1545): atomic session — Phyllis step 7
//
// POST /api/atoms/{atomId}/session         → chora-consumption POST /v1/me/atom-sessions
// POST /api/atoms/{atomId}/session/submit  → chora-consumption POST /v1/me/atom-sessions/{session_id}/answers
//
// chora-consumption has no /api/atoms/{id}/session endpoint — it serves
// the atom-session lifecycle under /v1/me/atom-sessions. The gateway
// remaps: StartAtomSession synthesises the downstream {"atom_id": ...}
// body from the path atomId; SubmitAtomSession lifts session_id out of
// the FE body and forwards the remaining answer fields to the
// /{session_id}/answers grade endpoint.
// -----------------------------------------------------------------------------

func TestStartAtomSession_ProxiesToConsumptionAtomSessions(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusCreated, `{"session_id":"sess-1","atom_id":"atom-cspo-1","status":"in_progress"}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.StartAtomSession(context.Background(), sampleAuth(), "atom-cspo-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", resp.Status)
	}
	if cb.path != "/v1/me/atom-sessions" {
		t.Errorf("downstream path = %q; want /v1/me/atom-sessions", cb.path)
	}
	if cb.method != http.MethodPost {
		t.Errorf("method = %s; want POST", cb.method)
	}
	// The downstream meStartSessionReq shape is {"atom_id": "..."} — the
	// gateway synthesises it from the path atomId (the FE start request
	// has no body).
	if !strings.Contains(cb.body, `"atom_id":"atom-cspo-1"`) {
		t.Errorf("downstream body = %q; want a synthesised {\"atom_id\":\"atom-cspo-1\"}", cb.body)
	}
	// chora-consumption extRequireContext reads X-Tenant-Id + lowercase gcid.
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("lowercase gcid header = %q; want %q", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
	if cb.hdr.Get("X-Tenant-Id") != sampleAuth().TenantID {
		t.Errorf("X-Tenant-Id = %q; want %q", cb.hdr.Get("X-Tenant-Id"), sampleAuth().TenantID)
	}
}

func TestStartAtomSession_EmptyAtomID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.StartAtomSession(context.Background(), sampleAuth(), "")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty atom id", resp.Status)
	}
}

func TestStartAtomSession_Upstream5xx_502(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusInternalServerError, `{"error":"boom"}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.StartAtomSession(context.Background(), sampleAuth(), "atom-1")
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 on upstream 5xx", resp.Status)
	}
}

func TestSubmitAtomSession_ProxiesToConsumptionAnswers(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"session_id":"sess-1","status":"completed","is_correct":true}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	// The FE submit body carries session_id (from the start response) +
	// the answer fields. The gateway lifts session_id into the downstream
	// path and forwards the rest.
	feBody := []byte(`{"session_id":"sess-1","answer_id":"ans-1","answer_index":1,"hint_used":false}`)
	resp, err := a.SubmitAtomSession(context.Background(), sampleAuth(), "atom-cspo-1", feBody)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/v1/me/atom-sessions/sess-1/answers" {
		t.Errorf("downstream path = %q; want /v1/me/atom-sessions/sess-1/answers", cb.path)
	}
	if cb.method != http.MethodPost {
		t.Errorf("method = %s; want POST", cb.method)
	}
	// session_id is stripped from the forwarded body (it lives in the
	// path); the answer fields are forwarded.
	if strings.Contains(cb.body, "session_id") {
		t.Errorf("downstream body = %q; session_id must be stripped (it's in the path)", cb.body)
	}
	if !strings.Contains(cb.body, `"answer_id":"ans-1"`) || !strings.Contains(cb.body, `"answer_index":1`) {
		t.Errorf("downstream body = %q; want answer fields forwarded", cb.body)
	}
}

func TestSubmitAtomSession_EmptyAtomID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.SubmitAtomSession(context.Background(), sampleAuth(), "", []byte(`{"session_id":"s"}`))
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty atom id", resp.Status)
	}
}

func TestSubmitAtomSession_MissingSessionID_400(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: "http://unused", PerCallTimeout: time.Second})
	// No session_id in the body — the gateway cannot build the downstream
	// /{session_id}/answers path, so it short-circuits 400 (NOT a fan-out
	// 502).
	resp, _ := a.SubmitAtomSession(context.Background(), sampleAuth(), "atom-1", []byte(`{"answer_index":1}`))
	if resp.Status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 when session_id missing from body", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_SESSION_ID_REQUIRED") {
		t.Errorf("body = %q; want GATEWAY_SESSION_ID_REQUIRED envelope", resp.Body)
	}
}

func TestSubmitAtomSession_BadJSON_400(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.SubmitAtomSession(context.Background(), sampleAuth(), "atom-1", []byte(`{not json`))
	if resp.Status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 on malformed FE body", resp.Status)
	}
}

func TestSubmitAtomSession_DownstreamDomain409_PassesThrough(t *testing.T) {
	// chora-consumption returns 409 INVALID_TRANSITION on a double-submit.
	// A domain 4xx proves the request reached the downstream handler — it
	// must pass through verbatim, not normalise.
	cb := newCaptureBackend(t, http.StatusConflict, `{"code":"INVALID_TRANSITION"}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.SubmitAtomSession(context.Background(), sampleAuth(), "atom-1", []byte(`{"session_id":"sess-1","answer_index":1}`))
	if resp.Status != http.StatusConflict {
		t.Errorf("status = %d; want 409 passed through verbatim", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "INVALID_TRANSITION") {
		t.Errorf("body = %q; want downstream body passed through", resp.Body)
	}
}

// -----------------------------------------------------------------------------
// Transport-error classification (shared)
// -----------------------------------------------------------------------------

func TestTransportError_502(t *testing.T) {
	// Point at a closed port — connection refused is a transport error.
	a := gatewayproxy.New(gatewayproxy.Config{TenancyURL: "http://127.0.0.1:1", PerCallTimeout: 500 * time.Millisecond})
	resp, _ := a.GetTenant(context.Background(), sampleAuth(), "x")
	if resp.Status != http.StatusBadGateway && resp.Status != http.StatusGatewayTimeout {
		t.Errorf("status = %d; want 502/504 on transport failure", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_UPSTREAM") {
		t.Errorf("body = %q; want a GATEWAY_UPSTREAM_* error envelope", resp.Body)
	}
}

// -----------------------------------------------------------------------------
// A17 (2026-05-15): mana wallet + top-up proxy to chora-identity
//
// GET  /api/v1/me/mana        → chora-identity GET /api/v1/me/mana
// POST /api/v1/me/mana/topup  → chora-identity POST /api/v1/me/mana/topup
//
// Pure passthrough — no body reshape. The downstream 402 InsufficientManaUpsell
// (contract-locked) MUST pass through verbatim. Idempotency-Key header MUST
// be forwarded for the POST per learner-economy.yaml:482.
// -----------------------------------------------------------------------------

func TestMeMana_GET_ProxiesToIdentity(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK,
		`{"gcid":"01970000-0000-7000-8000-0000000000aa","balance_units":1500,"lifetime_earned":2000,"lifetime_spent":500}`)
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.GetMyMana(context.Background(), sampleAuth())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/api/v1/me/mana" {
		t.Errorf("downstream path = %q; want /api/v1/me/mana", cb.path)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %s; want GET", cb.method)
	}
	if cb.hdr.Get("Authorization") != "Bearer raw-session-jwt" {
		t.Errorf("Authorization = %q; want Bearer forwarded", cb.hdr.Get("Authorization"))
	}
	// chora-identity bearerAuth reads the Authorization bearer (a GCID); the
	// X-Tenant-Id + lowercase gcid mesh headers are also forwarded so the
	// downstream tenantFromContext / gcidFromContext middleware can resolve
	// the caller.
	if cb.hdr.Get("X-Tenant-Id") != sampleAuth().TenantID {
		t.Errorf("X-Tenant-Id = %q; want %q", cb.hdr.Get("X-Tenant-Id"), sampleAuth().TenantID)
	}
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("lowercase gcid header = %q; want %q", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
	// Body MUST pass through verbatim — no reshape of the MeMana shape.
	if !strings.Contains(string(resp.Body), `"balance_units":1500`) {
		t.Errorf("body = %q; want downstream MeMana shape passed through", string(resp.Body))
	}
}

func TestMeMana_GET_NoIdentityURL_ReturnsNil(t *testing.T) {
	// All URLs unset → New returns nil; this asserts the contract that the
	// caller can route-skip. (Mirrors TestNew_NilWhenAllURLsUnset for the
	// IdentityURL axis.)
	if a := gatewayproxy.New(gatewayproxy.Config{}); a != nil {
		t.Fatal("New with no URLs must return nil so callers can route-skip")
	}
	// With ONLY IdentityURL set, aggregator must be live.
	if a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: "http://identity"}); a == nil {
		t.Fatal("New with IdentityURL set must return a non-nil aggregator")
	}
}

func TestMeManaTopup_POST_ForwardsIdempotencyKey(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusCreated,
		`{"topup_id":"tu-1","gcid":"01970000-0000-7000-8000-0000000000aa","units_credited":100}`)
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: cb.srv.URL, PerCallTimeout: time.Second})

	auth := sampleAuth()
	auth.IdempotencyKey = "uuid-test-1234"
	feBody := []byte(`{"amount_cents":100,"currency":"USD","payment_method_id":"pm_123"}`)
	resp, err := a.TopupMana(context.Background(), auth, feBody)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", resp.Status)
	}
	if cb.path != "/api/v1/me/mana/topup" {
		t.Errorf("downstream path = %q; want /api/v1/me/mana/topup", cb.path)
	}
	if cb.method != http.MethodPost {
		t.Errorf("method = %s; want POST", cb.method)
	}
	// Idempotency-Key MUST be forwarded per learner-economy.yaml:482.
	if cb.hdr.Get("Idempotency-Key") != "uuid-test-1234" {
		t.Errorf("Idempotency-Key = %q; want uuid-test-1234 forwarded", cb.hdr.Get("Idempotency-Key"))
	}
	// Body MUST be forwarded verbatim.
	if cb.body != `{"amount_cents":100,"currency":"USD","payment_method_id":"pm_123"}` {
		t.Errorf("body = %q; want verbatim", cb.body)
	}
	if cb.hdr.Get("Authorization") != "Bearer raw-session-jwt" {
		t.Errorf("Authorization = %q; want Bearer forwarded", cb.hdr.Get("Authorization"))
	}
}

func TestMeManaTopup_POST_NoIdempotencyKey_Optional(t *testing.T) {
	// The contract marks Idempotency-Key as optional. When absent on the FE
	// call, the gateway must NOT stamp an empty-string Idempotency-Key header
	// (which would defeat the downstream's idempotency-fallback synthesis).
	cb := newCaptureBackend(t, http.StatusCreated, `{"topup_id":"tu-1"}`)
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: cb.srv.URL, PerCallTimeout: time.Second})

	feBody := []byte(`{"amount_cents":100}`)
	resp, _ := a.TopupMana(context.Background(), sampleAuth(), feBody)
	if resp.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", resp.Status)
	}
	if got := cb.hdr.Get("Idempotency-Key"); got != "" {
		t.Errorf("Idempotency-Key = %q; want empty (header should not be set when absent on FE)", got)
	}
}

func TestMeMana_PassesThrough402_InsufficientManaUpsell(t *testing.T) {
	// chora-identity returns 402 with InsufficientManaUpsell body on a
	// payment-required error. 402 is contract-locked — the gateway MUST
	// pass it through verbatim (NOT normalise to a 5xx envelope) so the FE
	// can render the upsell UI.
	upsellBody := `{"error":{"code":"ECONOMY_INSUFFICIENT_MANA","message":"top-up required","details":{"upsell":{"required_units":500,"recommended_plan_code":"companion_standard","topup_suggestion":{"amount_cents":1500,"currency":"USD"}}}}}`
	cb := newCaptureBackend(t, http.StatusPaymentRequired, upsellBody)
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.TopupMana(context.Background(), sampleAuth(), []byte(`{"amount_cents":100}`))
	if resp.Status != http.StatusPaymentRequired {
		t.Errorf("status = %d; want 402 passed through verbatim", resp.Status)
	}
	if string(resp.Body) != upsellBody {
		t.Errorf("body = %q; want InsufficientManaUpsell body passed through verbatim", string(resp.Body))
	}
}

func TestMeMana_PassesThrough402_OnGET(t *testing.T) {
	// Symmetry: a downstream 402 on the GET path (e.g., a saga-state lock)
	// also passes through verbatim.
	body := `{"error":{"code":"ECONOMY_LOCKED","message":"account suspended"}}`
	cb := newCaptureBackend(t, http.StatusPaymentRequired, body)
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.GetMyMana(context.Background(), sampleAuth())
	if resp.Status != http.StatusPaymentRequired {
		t.Errorf("status = %d; want 402 passed through verbatim", resp.Status)
	}
	if string(resp.Body) != body {
		t.Errorf("body = %q; want downstream body passed through verbatim", string(resp.Body))
	}
}

func TestMeManaTopup_Upstream5xx_502(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusInternalServerError, `{"error":"boom"}`)
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.TopupMana(context.Background(), sampleAuth(), []byte(`{"amount_cents":100}`))
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 on upstream 5xx", resp.Status)
	}
}

// -----------------------------------------------------------------------------
// CHO-1883 (2026-06-26) — GET /api/v1/me/mana/ledger → chora-identity
// GET /api/v1/me/mana/ledger (pure passthrough; rawQuery forwarded verbatim
// like ListMyEnrolments). chora-identity owns listManaLedger
// (me_economy_handlers.go) and RLS-scopes the rows to the JWT gcid. The gateway
// never proxied it (404 at the edge per the 2026-06-25 mana-parity handoff
// §2.4), so the A+ Wallet could only show a balance, not a transaction list.
// -----------------------------------------------------------------------------

func TestMeManaLedger_GET_ProxiesToIdentity(t *testing.T) {
	body := `{"items":[{"entry_id":"019eff8b-0000-7000-8000-000000000001","direction":"debit","units":10,"reason":"companion_action","source_action_id":"question_authoring_generate","balance_after_units":26387,"recorded_at":"2026-06-26T16:20:00Z"}]}`
	cb := newCaptureBackend(t, http.StatusOK, body)
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.ListMyManaLedger(context.Background(), sampleAuth(), "page_size=20&direction=debit")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/api/v1/me/mana/ledger" {
		t.Errorf("downstream path = %q; want /api/v1/me/mana/ledger", cb.path)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %s; want GET", cb.method)
	}
	if cb.rawQ != "page_size=20&direction=debit" {
		t.Errorf("rawQuery = %q; want forwarded verbatim", cb.rawQ)
	}
	if cb.hdr.Get("Authorization") != "Bearer raw-session-jwt" {
		t.Errorf("Authorization = %q; want Bearer forwarded", cb.hdr.Get("Authorization"))
	}
	if cb.hdr.Get("X-Tenant-Id") != sampleAuth().TenantID {
		t.Errorf("X-Tenant-Id = %q; want %q", cb.hdr.Get("X-Tenant-Id"), sampleAuth().TenantID)
	}
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("lowercase gcid header = %q; want %q", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
	// Body MUST pass through verbatim — no reshape of the ledger items.
	if !strings.Contains(string(resp.Body), `"source_action_id":"question_authoring_generate"`) {
		t.Errorf("body = %q; want downstream ledger items passed through", string(resp.Body))
	}
}

func TestMeManaLedger_GET_NoQuery_NoTrailingQuestionMark(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"items":[]}`)
	a := gatewayproxy.New(gatewayproxy.Config{IdentityURL: cb.srv.URL, PerCallTimeout: time.Second})
	_, _ = a.ListMyManaLedger(context.Background(), sampleAuth(), "")
	if cb.path != "/api/v1/me/mana/ledger" {
		t.Errorf("downstream path = %q; want /api/v1/me/mana/ledger", cb.path)
	}
	if cb.rawQ != "" {
		t.Errorf("rawQuery = %q; want empty (no trailing ?)", cb.rawQ)
	}
}

// -----------------------------------------------------------------------------
// P7 (2026-05-15): manual question authoring — pure passthrough to chora-creation
//
// POST   /api/atoms/{atom_id}/questions                       → chora-creation (verbatim path)
// PATCH  /api/atoms/{atom_id}/questions/{question_id}         → chora-creation (verbatim path)
// GET    /api/atoms/{atom_id}/questions/{question_id}         → chora-creation (verbatim path)
// DELETE /api/atoms/{atom_id}/questions/{question_id}         → chora-creation (verbatim path)
//
// All routes pass the path through verbatim — chora-creation already serves
// /api/atoms/{atom_id}/questions[...] (commit 83a769e7 / chora-creation
// :p3-b82d1e32). Status passthrough must cover the FE-bound envelope:
// 201 / 200 / 204 / 400 / 401 / 403 / 404 / 409 (CREATION_QUESTION_DUPLICATE,
// D2 — 1 atom = 1 question) / 422 (validation). The gateway MUST NOT reshape
// any of these.
//
// IdentityURL is OFF in these tests — only CreationURL is wired so we can
// assert ONLY the questions methods route to chora-creation.
// -----------------------------------------------------------------------------

func TestNew_NonNilWhenCreationURLSet(t *testing.T) {
	if a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://creation"}); a == nil {
		t.Fatal("New with CreationURL set must return a non-nil aggregator")
	}
}

func TestQuestions_POST_ProxiesToCreation(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusCreated,
		`{"question":{"question_id":"01970000-0000-7000-8000-0000000000q1","type":"mcq","prompt":"P7"},"revision_id":"01970000-0000-7000-8000-0000000000r1"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	atomID := "00000000-0000-7000-8000-00000000a0a2"
	body := []byte(`{"type":"mcq","prompt":"P7","mcq":{"options":[{"option_id":"o1","label":"A","is_correct":true,"explainer":"y"},{"option_id":"o2","label":"B","is_correct":false,"explainer":"n"}]}}`)
	resp, err := a.CreateQuestion(context.Background(), sampleAuth(), atomID, body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", resp.Status)
	}
	// Path MUST be the verbatim chora-creation path.
	wantPath := "/api/atoms/" + atomID + "/questions"
	if cb.path != wantPath {
		t.Errorf("downstream path = %q; want %q", cb.path, wantPath)
	}
	if cb.method != http.MethodPost {
		t.Errorf("method = %s; want POST", cb.method)
	}
	// Body forwarded verbatim.
	if cb.body != string(body) {
		t.Errorf("body forwarded = %q; want verbatim", cb.body)
	}
	// Bearer + mesh headers stamped.
	if cb.hdr.Get("Authorization") != "Bearer raw-session-jwt" {
		t.Errorf("Authorization = %q; want Bearer forwarded", cb.hdr.Get("Authorization"))
	}
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("lowercase gcid = %q; want %q", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
	if cb.hdr.Get("X-Tenant-Id") != sampleAuth().TenantID {
		t.Errorf("X-Tenant-Id = %q; want %q", cb.hdr.Get("X-Tenant-Id"), sampleAuth().TenantID)
	}
}

func TestQuestions_POST_EmptyAtomID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.CreateQuestion(context.Background(), sampleAuth(), "", []byte(`{}`))
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty atom id", resp.Status)
	}
}

func TestQuestions_PATCH_ProxiesToCreation(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK,
		`{"question":{"question_id":"01970000-0000-7000-8000-0000000000q1","prompt":"P7 v2"}}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	atomID := "00000000-0000-7000-8000-00000000a0a2"
	qID := "01970000-0000-7000-8000-0000000000q1"
	body := []byte(`{"prompt":"P7 v2"}`)
	resp, err := a.EditQuestion(context.Background(), sampleAuth(), atomID, qID, body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	wantPath := "/api/atoms/" + atomID + "/questions/" + qID
	if cb.path != wantPath {
		t.Errorf("downstream path = %q; want %q", cb.path, wantPath)
	}
	if cb.method != http.MethodPatch {
		t.Errorf("method = %s; want PATCH", cb.method)
	}
	if cb.body != string(body) {
		t.Errorf("body forwarded = %q; want verbatim", cb.body)
	}
}

func TestQuestions_PATCH_EmptyQuestionID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.EditQuestion(context.Background(), sampleAuth(), "atom-1", "", []byte(`{}`))
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty question id", resp.Status)
	}
}

func TestQuestions_GET_ProxiesToCreation(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK,
		`{"question":{"question_id":"01970000-0000-7000-8000-0000000000q1","type":"mcq","prompt":"P7","mcq_payload":{"options":[]}},"current_revision":{"revision_id":"r1"}}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	atomID := "00000000-0000-7000-8000-00000000a0a2"
	qID := "01970000-0000-7000-8000-0000000000q1"
	resp, err := a.GetQuestion(context.Background(), sampleAuth(), atomID, qID)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	wantPath := "/api/atoms/" + atomID + "/questions/" + qID
	if cb.path != wantPath {
		t.Errorf("downstream path = %q; want %q", cb.path, wantPath)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %s; want GET", cb.method)
	}
	// Author projection body passed through verbatim — no reshape.
	if !strings.Contains(string(resp.Body), `"mcq_payload"`) {
		t.Errorf("body = %q; want author projection passed through verbatim", string(resp.Body))
	}
}

func TestQuestions_GET_EmptyQuestionID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.GetQuestion(context.Background(), sampleAuth(), "atom-1", "")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty question id", resp.Status)
	}
}

func TestQuestions_DELETE_ProxiesToCreation(t *testing.T) {
	// chora-creation's deleteQuestion returns 204 with no body (idempotent
	// soft-delete per ddd-enforcement #5). The gateway must passthrough the
	// 204 + the empty body cleanly — no envelope synthesis.
	cb := newCaptureBackend(t, http.StatusNoContent, ``)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	atomID := "00000000-0000-7000-8000-00000000a0a2"
	qID := "01970000-0000-7000-8000-0000000000q1"
	resp, err := a.DeleteQuestion(context.Background(), sampleAuth(), atomID, qID)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusNoContent {
		t.Errorf("status = %d; want 204", resp.Status)
	}
	if len(resp.Body) != 0 {
		t.Errorf("body = %q; want empty 204 body passed through", string(resp.Body))
	}
	wantPath := "/api/atoms/" + atomID + "/questions/" + qID
	if cb.path != wantPath {
		t.Errorf("downstream path = %q; want %q", cb.path, wantPath)
	}
	if cb.method != http.MethodDelete {
		t.Errorf("method = %s; want DELETE", cb.method)
	}
	// Bearer + mesh headers stamped on DELETE too.
	if cb.hdr.Get("Authorization") != "Bearer raw-session-jwt" {
		t.Errorf("Authorization = %q; want Bearer forwarded", cb.hdr.Get("Authorization"))
	}
}

func TestQuestions_DELETE_EmptyQuestionID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.DeleteQuestion(context.Background(), sampleAuth(), "atom-1", "")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty question id", resp.Status)
	}
}

// D2 conflict — POST a second question to an atom that already has one. The
// gateway MUST pass the 409 + the CREATION_QUESTION_DUPLICATE body through
// verbatim (NOT reshape to a 5xx envelope).
func TestQuestions_409_PassesThrough_DuplicateBody(t *testing.T) {
	dupBody := `{"error":{"code":"CREATION_QUESTION_DUPLICATE","message":"atom already has a question (D2: 1 atom = 1 question)"}}`
	cb := newCaptureBackend(t, http.StatusConflict, dupBody)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.CreateQuestion(context.Background(), sampleAuth(),
		"00000000-0000-7000-8000-00000000a0a2", []byte(`{"type":"mcq"}`))
	if resp.Status != http.StatusConflict {
		t.Errorf("status = %d; want 409 passed through verbatim", resp.Status)
	}
	if string(resp.Body) != dupBody {
		t.Errorf("body = %q; want CREATION_QUESTION_DUPLICATE body verbatim", string(resp.Body))
	}
}

// 422 validation passthrough (e.g., MCQ with no correct option).
func TestQuestions_POST_422_PassesThrough_ValidationEnvelope(t *testing.T) {
	v := `{"error":{"code":"VALIDATION_FAILED","message":"mcq must have ≥1 correct option"}}`
	cb := newCaptureBackend(t, http.StatusUnprocessableEntity, v)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.CreateQuestion(context.Background(), sampleAuth(),
		"00000000-0000-7000-8000-00000000a0a2", []byte(`{"type":"mcq"}`))
	if resp.Status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d; want 422 passed through verbatim", resp.Status)
	}
	if string(resp.Body) != v {
		t.Errorf("body = %q; want VALIDATION_FAILED body verbatim", string(resp.Body))
	}
}

// 404 on the GET passthrough (question not found in the atom).
func TestQuestions_GET_404_PassesThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusNotFound,
		`{"error":{"code":"CREATION_QUESTION_NOT_FOUND","message":"question not found"}}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.GetQuestion(context.Background(), sampleAuth(),
		"00000000-0000-7000-8000-00000000a0a2", "01970000-0000-7000-8000-0000000000q1")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 passed through verbatim", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "CREATION_QUESTION_NOT_FOUND") {
		t.Errorf("body = %q; want CREATION_QUESTION_NOT_FOUND envelope", string(resp.Body))
	}
}

// 204 idempotent re-delete — the second DELETE may legitimately return 204
// (no body) per the contract idempotency guarantee.
func TestQuestions_204_NoBody_OnDelete(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusNoContent, ``)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.DeleteQuestion(context.Background(), sampleAuth(),
		"00000000-0000-7000-8000-00000000a0a2", "01970000-0000-7000-8000-0000000000q1")
	if resp.Status != http.StatusNoContent {
		t.Errorf("status = %d; want 204", resp.Status)
	}
	if len(resp.Body) != 0 {
		t.Errorf("body length = %d; want empty body on 204 passthrough", len(resp.Body))
	}
	// Crucially — the 204 MUST NOT be reshaped into an envelope with body.
}

func TestQuestions_Upstream5xx_502(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusInternalServerError, `{"error":"boom"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.CreateQuestion(context.Background(), sampleAuth(),
		"00000000-0000-7000-8000-00000000a0a2", []byte(`{}`))
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 on upstream 5xx", resp.Status)
	}
}

// -----------------------------------------------------------------------------
// P7.5 (2026-05-15) — AI single-question async + model-answer adhoc fill
// -----------------------------------------------------------------------------
// New /question-jobs + /ai-model-answer-jobs leaves under /api/atoms/. The
// gateway proxies all four leaves verbatim to chora-creation, preserving the
// 202 / 200 / 402 / 404 / 422 / 503 status semantics:
//
//	POST   /api/atoms/{atom_id}/question-jobs                            → CreateQuestionJob
//	GET    /api/atoms/{atom_id}/question-jobs/{job_id}                   → GetQuestionJob
//	POST   /api/atoms/{atom_id}/question-jobs/{job_id}/accept            → AcceptQuestionJob
//	POST   /api/atoms/{atom_id}/questions/{q_id}/ai-model-answer-jobs    → CreateModelAnswerJob
//
// The POST /question-jobs route may carry multipart/form-data (batch source
// material upload path; P5+ — chora-creation handles parse). The gateway
// MUST stream the request body untouched + preserve Content-Type + Content-
// Length headers.
// -----------------------------------------------------------------------------

// 1. CreateQuestionJob — single-Q ai_draft happy path.
func TestQuestionJobs_POST_AIDraft_ProxiesToCreation(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusAccepted,
		`{"job_id":"01970000-0000-7000-8000-0000000000j1","status":"queued","poll_url":"/api/atoms/atom-1/question-jobs/01970000-0000-7000-8000-0000000000j1"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	atomID := "00000000-0000-7000-8000-00000000a0a2"
	body := []byte(`{"type":"ai_draft","question_type":"mcq","prompt":"Generate MCQ on Scrum","count":1}`)
	resp, err := a.CreateQuestionJob(context.Background(), sampleAuth(), atomID, "application/json", body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusAccepted {
		t.Errorf("status = %d; want 202", resp.Status)
	}
	wantPath := "/api/atoms/" + atomID + "/question-jobs"
	if cb.path != wantPath {
		t.Errorf("downstream path = %q; want %q", cb.path, wantPath)
	}
	if cb.method != http.MethodPost {
		t.Errorf("method = %s; want POST", cb.method)
	}
	if cb.body != string(body) {
		t.Errorf("body forwarded = %q; want verbatim", cb.body)
	}
	if cb.hdr.Get("Authorization") != "Bearer raw-session-jwt" {
		t.Errorf("Authorization = %q; want Bearer forwarded", cb.hdr.Get("Authorization"))
	}
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("lowercase gcid = %q; want %q", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
}

// 2. CreateQuestionJob — multipart batch source upload pass-through.
func TestQuestionJobs_POST_Multipart_StreamsBodyAndContentType(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusAccepted,
		`{"job_id":"01970000-0000-7000-8000-0000000000j2","status":"queued"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: 60 * time.Second})

	atomID := "00000000-0000-7000-8000-00000000a0a2"
	multipartBody := "--abc\r\nContent-Disposition: form-data; name=\"file\"; filename=\"src.pdf\"\r\nContent-Type: application/pdf\r\n\r\n<pdf bytes>\r\n--abc--\r\n"
	resp, err := a.CreateQuestionJob(context.Background(), sampleAuth(), atomID,
		"multipart/form-data; boundary=abc", []byte(multipartBody))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusAccepted {
		t.Errorf("status = %d; want 202", resp.Status)
	}
	if !strings.HasPrefix(cb.hdr.Get("Content-Type"), "multipart/form-data") {
		t.Errorf("Content-Type = %q; want multipart/form-data prefix", cb.hdr.Get("Content-Type"))
	}
	if cb.body != multipartBody {
		t.Errorf("multipart body forwarded = %q; want verbatim", cb.body)
	}
}

// 3. CreateQuestionJob — 402 insufficient mana passes through verbatim.
func TestQuestionJobs_POST_402_PassesThroughInsufficientMana(t *testing.T) {
	manaErr := `{"error":{"code":"INSUFFICIENT_MANA","message":"need 10 mana","upsell":{"required_units":10,"current_balance_units":0,"recommended_plan_code":"basic","recommended_topup_units":0,"stripe_checkout_url":null}}}`
	cb := newCaptureBackend(t, http.StatusPaymentRequired, manaErr)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.CreateQuestionJob(context.Background(), sampleAuth(),
		"00000000-0000-7000-8000-00000000a0a2", "application/json",
		[]byte(`{"type":"ai_draft","question_type":"mcq","prompt":"x"}`))
	if resp.Status != http.StatusPaymentRequired {
		t.Errorf("status = %d; want 402 passed through", resp.Status)
	}
	if string(resp.Body) != manaErr {
		t.Errorf("body = %q; want INSUFFICIENT_MANA envelope verbatim", string(resp.Body))
	}
}

// 4. CreateQuestionJob — 503 when chora-identity gRPC NOT wired upstream.
func TestQuestionJobs_POST_503_NotWired_PassesThrough(t *testing.T) {
	notWired := `{"error":{"code":"CREATION_JOBS_NOT_WIRED","message":"mana client unavailable"}}`
	cb := newCaptureBackend(t, http.StatusServiceUnavailable, notWired)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.CreateQuestionJob(context.Background(), sampleAuth(),
		"00000000-0000-7000-8000-00000000a0a2", "application/json",
		[]byte(`{"type":"ai_draft","question_type":"mcq","prompt":"x"}`))
	if resp.Status != http.StatusServiceUnavailable {
		t.Errorf("status = %d; want 503 passed through (chora-identity not wired)", resp.Status)
	}
	if string(resp.Body) != notWired {
		t.Errorf("body = %q; want CREATION_JOBS_NOT_WIRED envelope", string(resp.Body))
	}
}

// 5. CreateQuestionJob — empty atom_id 404.
func TestQuestionJobs_POST_EmptyAtomID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.CreateQuestionJob(context.Background(), sampleAuth(), "", "application/json", []byte(`{}`))
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty atom id", resp.Status)
	}
}

// 6. GetQuestionJob — happy-path proxy.
func TestQuestionJobs_GET_ProxiesToCreation(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK,
		`{"job_id":"01970000-0000-7000-8000-0000000000j1","atom_id":"a","job_type":"ai_draft","status":"completed","mana_charged":10,"candidate_questions":[]}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	atomID := "00000000-0000-7000-8000-00000000a0a2"
	jobID := "01970000-0000-7000-8000-0000000000j1"
	resp, err := a.GetQuestionJob(context.Background(), sampleAuth(), atomID, jobID)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	wantPath := "/api/atoms/" + atomID + "/question-jobs/" + jobID
	if cb.path != wantPath {
		t.Errorf("downstream path = %q; want %q", cb.path, wantPath)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %s; want GET", cb.method)
	}
}

// 7. GetQuestionJob — empty job id 404.
func TestQuestionJobs_GET_EmptyJobID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.GetQuestionJob(context.Background(), sampleAuth(), "atom-1", "")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty job id", resp.Status)
	}
}

// 8. AcceptQuestionJob — happy-path proxy.
func TestQuestionJobs_AcceptPOST_ProxiesToCreation(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK,
		`{"persisted":[{"question_id":"01970000-0000-7000-8000-0000000000q1"}],"mana_debited":10}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	atomID := "00000000-0000-7000-8000-00000000a0a2"
	jobID := "01970000-0000-7000-8000-0000000000j1"
	body := []byte(`{"accepted_candidates":[{"draft_id":"d1"}]}`)
	resp, err := a.AcceptQuestionJob(context.Background(), sampleAuth(), atomID, jobID, body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	wantPath := "/api/atoms/" + atomID + "/question-jobs/" + jobID + "/accept"
	if cb.path != wantPath {
		t.Errorf("downstream path = %q; want %q", cb.path, wantPath)
	}
	if cb.method != http.MethodPost {
		t.Errorf("method = %s; want POST", cb.method)
	}
	if cb.body != string(body) {
		t.Errorf("body forwarded = %q; want verbatim", cb.body)
	}
}

// 9. AcceptQuestionJob — empty atom_id 404.
func TestQuestionJobs_AcceptPOST_EmptyAtomID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.AcceptQuestionJob(context.Background(), sampleAuth(), "", "job-1", []byte(`{}`))
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty atom id", resp.Status)
	}
}

// 10. CreateModelAnswerJob — happy-path proxy.
func TestQuestionJobs_ModelAnswerPOST_ProxiesToCreation(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusAccepted,
		`{"job_id":"01970000-0000-7000-8000-0000000000j3","status":"queued","poll_url":"/api/atoms/a/question-jobs/01970000-0000-7000-8000-0000000000j3"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	atomID := "00000000-0000-7000-8000-00000000a0a2"
	qID := "01970000-0000-7000-8000-0000000000q1"
	body := []byte(`{"tone_hint":"formal"}`)
	resp, err := a.CreateModelAnswerJob(context.Background(), sampleAuth(), atomID, qID, body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusAccepted {
		t.Errorf("status = %d; want 202", resp.Status)
	}
	wantPath := "/api/atoms/" + atomID + "/questions/" + qID + "/ai-model-answer-jobs"
	if cb.path != wantPath {
		t.Errorf("downstream path = %q; want %q", cb.path, wantPath)
	}
	if cb.method != http.MethodPost {
		t.Errorf("method = %s; want POST", cb.method)
	}
}

// 11. CreateModelAnswerJob — empty question id 404 BEFORE any outbound call.
func TestQuestionJobs_ModelAnswerPOST_EmptyQuestionID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.CreateModelAnswerJob(context.Background(), sampleAuth(), "atom-1", "", []byte(`{}`))
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty question id", resp.Status)
	}
}

// 12. CreateModelAnswerJob — 402 InsufficientMana passes through.
func TestQuestionJobs_ModelAnswerPOST_402_PassesThrough(t *testing.T) {
	manaErr := `{"error":{"code":"INSUFFICIENT_MANA","message":"need 5 mana"}}`
	cb := newCaptureBackend(t, http.StatusPaymentRequired, manaErr)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.CreateModelAnswerJob(context.Background(), sampleAuth(),
		"00000000-0000-7000-8000-00000000a0a2", "01970000-0000-7000-8000-0000000000q1", []byte(`{}`))
	if resp.Status != http.StatusPaymentRequired {
		t.Errorf("status = %d; want 402 passed through", resp.Status)
	}
}

// 13. 5xx → 502 normalisation on all four endpoints.
func TestQuestionJobs_Upstream5xx_502(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusInternalServerError, `{"error":"boom"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	for _, name := range []string{"create", "get", "accept", "model-answer"} {
		name := name
		t.Run(name, func(t *testing.T) {
			var resp gatewayproxy.Response
			switch name {
			case "create":
				resp, _ = a.CreateQuestionJob(context.Background(), sampleAuth(),
					"a", "application/json", []byte(`{}`))
			case "get":
				resp, _ = a.GetQuestionJob(context.Background(), sampleAuth(), "a", "j")
			case "accept":
				resp, _ = a.AcceptQuestionJob(context.Background(), sampleAuth(),
					"a", "j", []byte(`{}`))
			case "model-answer":
				resp, _ = a.CreateModelAnswerJob(context.Background(), sampleAuth(),
					"a", "q", []byte(`{}`))
			}
			if resp.Status != http.StatusBadGateway {
				t.Errorf("status = %d; want 502 on upstream 5xx", resp.Status)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// QuestionTypes registry — pure passthrough to chora-creation
//
// Bug fixed: the legacy /api/atoms/{id} Phyllis route was claiming the static
// /api/atoms/question-types path and wrapping the chora-creation body in
// {"atom": ...}. Canonical contract per chora-contracts/openapi/
// creation-questions.yaml::listQuestionTypes returns {"items": [...]} at the
// TOP level — NOT wrapped. The gatewayproxy bridge now owns this exact static
// path and forwards the body BYTE-IDENTICAL.
// -----------------------------------------------------------------------------

// TestQuestionTypes_GET_ProxiesToCreation_BodyVerbatim is the canonical RED
// spec: gateway response body must equal the chora-creation body byte-for-byte.
// No {"atom": ...} wrap, no reshape — just a passthrough.
func TestQuestionTypes_GET_ProxiesToCreation_BodyVerbatim(t *testing.T) {
	downstream := `{"items":[{"code":"mcq","label_en":"Multiple Choice","label_zh":null,"enabled":true,"scope":"phyllis"}]}`
	cb := newCaptureBackend(t, http.StatusOK, downstream)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.GetQuestionTypes(context.Background(), sampleAuth())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	// Downstream path: chora-creation serves at the SAME path the FE hits.
	if cb.path != "/api/atoms/question-types" {
		t.Errorf("downstream path = %q; want /api/atoms/question-types", cb.path)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %s; want GET", cb.method)
	}
	// CRITICAL — body BYTE-IDENTICAL to the downstream. No {"atom": ...} wrap.
	if string(resp.Body) != downstream {
		t.Errorf("body wrapped — got %q; want byte-identical %q", string(resp.Body), downstream)
	}
	// Defensive: assert the wrap key is NOT present.
	if strings.Contains(string(resp.Body), `"atom"`) {
		t.Errorf("body contains atom wrap key: %q", string(resp.Body))
	}
	// Defensive: assert top-level "items" is present.
	if !strings.Contains(string(resp.Body), `"items"`) {
		t.Errorf("body missing top-level items key: %q", string(resp.Body))
	}
	// Bearer + mesh headers stamped on the outbound call (downstream may
	// require X-Tenant-Id even though the registry is tenant-agnostic).
	if cb.hdr.Get("Authorization") != "Bearer raw-session-jwt" {
		t.Errorf("Authorization = %q; want Bearer forwarded", cb.hdr.Get("Authorization"))
	}
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("lowercase gcid header = %q; want %q", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
}

// 5xx → 502 normalisation (same envelope semantics as the rest of this
// aggregator).
func TestQuestionTypes_GET_Upstream5xx_502(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusInternalServerError, `{"error":"boom"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.GetQuestionTypes(context.Background(), sampleAuth())
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 on upstream 5xx", resp.Status)
	}
}

// 4xx downstream (e.g. malformed Authorization) passes through verbatim — the
// registry is auth-aware (chora-creation gates with the standard auth gate
// even though it serves the same registry to every caller).
func TestQuestionTypes_GET_4xx_PassesThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusUnauthorized, `{"error":{"code":"UNAUTH","message":"no auth"}}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.GetQuestionTypes(context.Background(), sampleAuth())
	if resp.Status != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401 passthrough", resp.Status)
	}
	if !strings.Contains(string(resp.Body), `"UNAUTH"`) {
		t.Errorf("body = %q; want downstream 401 envelope passed through", string(resp.Body))
	}
}

// -----------------------------------------------------------------------------
// ListInstructorCourses — GET /api/v1/instructors/{instructor_gcid}/courses
// proxy to chora-delivery (closes debt #4 / A6).
// -----------------------------------------------------------------------------

func TestListInstructorCourses_ProxiesToDeliveryByInstructor(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"items":[],"total":0,"page":1,"per":20}`)
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: cb.srv.URL, PerCallTimeout: time.Second})

	const instructor = "00000000-0000-7000-8000-000000001999"
	resp, err := a.ListInstructorCourses(context.Background(), sampleAuth(), instructor, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	// The downstream path mirrors the FE-facing path verbatim — chora-delivery
	// serves the canonical /api/v1/instructors/{id}/courses route.
	wantPath := "/api/v1/instructors/" + instructor + "/courses"
	if cb.path != wantPath {
		t.Errorf("downstream path = %q; want %q", cb.path, wantPath)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %s; want GET", cb.method)
	}
	// Mesh-trust headers must flow so the downstream tenantRequired middleware
	// + role-gate (x-mesh-user-roles) work.
	if cb.hdr.Get("X-Tenant-Id") != sampleAuth().TenantID {
		t.Errorf("X-Tenant-Id = %q; want %q", cb.hdr.Get("X-Tenant-Id"), sampleAuth().TenantID)
	}
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("lowercase gcid header = %q; want %q", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
}

func TestListInstructorCourses_EmptyInstructorID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.ListInstructorCourses(context.Background(), sampleAuth(), "", "")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty instructor id", resp.Status)
	}
}

func TestListInstructorCourses_ForwardsQueryString(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"items":[],"total":0,"page":3,"per":50}`)
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: cb.srv.URL, PerCallTimeout: time.Second})

	const instructor = "00000000-0000-7000-8000-000000001999"
	_, err := a.ListInstructorCourses(context.Background(), sampleAuth(), instructor, "page=3&per=50")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if cb.rawQ != "page=3&per=50" {
		t.Errorf("downstream rawQuery = %q; want page=3&per=50", cb.rawQ)
	}
}

func TestListInstructorCourses_DownstreamForbidden_PassThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusForbidden,
		`{"error":"Forbidden","message":"caller is not the instructor and lacks the instructor/admin role"}`)
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: cb.srv.URL, PerCallTimeout: time.Second})

	const instructor = "00000000-0000-7000-8000-000000001999"
	resp, _ := a.ListInstructorCourses(context.Background(), sampleAuth(), instructor, "")
	if resp.Status != http.StatusForbidden {
		t.Errorf("status = %d; want 403 passthrough", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "instructor/admin role") {
		t.Errorf("body must passthrough 403 envelope; got %q", string(resp.Body))
	}
}

func TestListInstructorCourses_Upstream5xx_502(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusInternalServerError, `{"error":"boom"}`)
	a := gatewayproxy.New(gatewayproxy.Config{DeliveryURL: cb.srv.URL, PerCallTimeout: time.Second})

	const instructor = "00000000-0000-7000-8000-000000001999"
	resp, _ := a.ListInstructorCourses(context.Background(), sampleAuth(), instructor, "")
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 on upstream 5xx", resp.Status)
	}
}

// -----------------------------------------------------------------------------
// H+ tx-history (Phase 2 Agent A2, 2026-05-26) — chora-payments admin REST.
//
// 4 routes:
//   GET  /api/v1/admin/payments/purchases               → ListPurchaseHistory
//   POST /api/v1/admin/payments/{purchase_id}/refund    → IssuePaymentRefund
//   GET  /api/v1/admin/payments/export                  → ExportPurchases (streaming)
//   GET  /api/v1/admin/payments/stream                  → StreamPaymentEvents (SSE, payments_stream_test.go)
//
// Per chora-contracts/openapi/payments-admin.yaml. PLATFORM_OPERATOR is the
// new role; auth.Roles propagates as a comma-joined X-Chora-Role header.
// -----------------------------------------------------------------------------

// platformOperatorAuth returns the canonical sampleAuth() with the new
// PLATFORM_OPERATOR role injected so the X-Chora-Role assertion exercises the
// real cross-tenant operator flow.
func platformOperatorAuth() gatewayproxy.AuthCtx {
	a := sampleAuth()
	a.Roles = []string{"PLATFORM_OPERATOR"}
	return a
}

// tenantAdminAuth returns the canonical sampleAuth() with the TENANT_ADMIN
// role injected (single-tenant default — no cross-tenant scope).
func tenantAdminAuth() gatewayproxy.AuthCtx {
	a := sampleAuth()
	a.Roles = []string{"TENANT_ADMIN"}
	return a
}

func TestIssuePaymentRefund_ProxiesToPaymentsRefund(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"refund_id":"re_abc","refunded_at":"2026-05-26T00:00:00Z","amount_cents":7900,"state":"refunded"}`)
	a := gatewayproxy.New(gatewayproxy.Config{PaymentsURL: cb.srv.URL, PerCallTimeout: time.Second})

	const purchaseID = "01970000-0000-7000-8000-0000000000dd"
	body := []byte(`{"aggregate_type":"course_purchase","reason":"learner request"}`)
	resp, err := a.IssuePaymentRefund(context.Background(), tenantAdminAuth(), purchaseID, "", body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	wantPath := "/api/v1/admin/payments/" + purchaseID + "/refund"
	if cb.path != wantPath {
		t.Errorf("downstream path = %q; want %q", cb.path, wantPath)
	}
	if cb.method != http.MethodPost {
		t.Errorf("method = %s; want POST", cb.method)
	}
	if cb.body != string(body) {
		t.Errorf("body = %q; want forwarded verbatim", cb.body)
	}
	if cb.hdr.Get("X-Chora-Role") != "TENANT_ADMIN" {
		t.Errorf("X-Chora-Role = %q; want TENANT_ADMIN", cb.hdr.Get("X-Chora-Role"))
	}
}

func TestIssuePaymentRefund_EmptyPurchaseID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{PaymentsURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.IssuePaymentRefund(context.Background(), tenantAdminAuth(), "", "", []byte(`{}`))
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty purchase_id (defensive)", resp.Status)
	}
}

func TestIssuePaymentRefund_DownstreamConflict_PassThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusConflict, `{"code":"already_refunded","message":"purchase already refunded"}`)
	a := gatewayproxy.New(gatewayproxy.Config{PaymentsURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.IssuePaymentRefund(context.Background(), tenantAdminAuth(),
		"01970000-0000-7000-8000-0000000000dd", "", []byte(`{"aggregate_type":"course_purchase"}`))
	if resp.Status != http.StatusConflict {
		t.Errorf("status = %d; want 409 passed through verbatim", resp.Status)
	}
}

func TestIssuePaymentRefund_AuditorForbidden_PassThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusForbidden, `{"code":"forbidden","message":"AUDITOR cannot issue refund"}`)
	a := gatewayproxy.New(gatewayproxy.Config{PaymentsURL: cb.srv.URL, PerCallTimeout: time.Second})

	auditor := sampleAuth()
	auditor.Roles = []string{"AUDITOR"}
	resp, _ := a.IssuePaymentRefund(context.Background(), auditor,
		"01970000-0000-7000-8000-0000000000dd", "", []byte(`{"aggregate_type":"course_purchase"}`))
	if resp.Status != http.StatusForbidden {
		t.Errorf("status = %d; want 403 passed through (AUDITOR cannot refund)", resp.Status)
	}
}

func TestLoadConfigFromEnv_PaymentsURLDefault(t *testing.T) {
	t.Setenv("CHORA_PAYMENTS_HTTP_ADDR", "")
	cfg := gatewayproxy.LoadConfigFromEnv()
	const wantDefault = "http://chora-payments.payments.svc.cluster.local"
	if cfg.PaymentsURL != wantDefault {
		t.Errorf("PaymentsURL = %q; want default %q when CHORA_PAYMENTS_HTTP_ADDR unset", cfg.PaymentsURL, wantDefault)
	}
}

func TestLoadConfigFromEnv_PaymentsURLOverride(t *testing.T) {
	t.Setenv("CHORA_PAYMENTS_HTTP_ADDR", "http://payments.dev.example:8080")
	cfg := gatewayproxy.LoadConfigFromEnv()
	if cfg.PaymentsURL != "http://payments.dev.example:8080" {
		t.Errorf("PaymentsURL = %q; want env override honoured", cfg.PaymentsURL)
	}
}

// -----------------------------------------------------------------------------
// WS-7b — GET /api/atoms/{atomId}/revisions → chora-creation (same path)
// -----------------------------------------------------------------------------

func TestListAtomRevisions_HappyPath_200(t *testing.T) {
	// RED: ListAtomRevisions must not exist yet; this test verifies the happy
	// path: 200 with an AtomRevisionPage containing 3 items. Downstream path
	// must be /api/atoms/{atomId}/revisions with mesh-trust headers stamped.
	revBody := `{"items":[{"id":"r1"},{"id":"r2"},{"id":"r3"}],"next_page_token":""}`
	cb := newCaptureBackend(t, http.StatusOK, revBody)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.ListAtomRevisions(context.Background(), sampleAuth(), "atom-abc123", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/api/atoms/atom-abc123/revisions" {
		t.Errorf("downstream path = %q; want /api/atoms/atom-abc123/revisions", cb.path)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %s; want GET", cb.method)
	}
	// Mesh-trust headers — chora-creation enforces RLS using X-Tenant-Id.
	if cb.hdr.Get("X-Tenant-Id") != sampleAuth().TenantID {
		t.Errorf("X-Tenant-Id = %q; want %q", cb.hdr.Get("X-Tenant-Id"), sampleAuth().TenantID)
	}
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("lowercase gcid = %q; want %q", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
	if cb.hdr.Get("traceparent") != sampleAuth().Traceparent {
		t.Errorf("traceparent = %q; want %q", cb.hdr.Get("traceparent"), sampleAuth().Traceparent)
	}
	if string(resp.Body) != revBody {
		t.Errorf("body = %q; want upstream body verbatim", string(resp.Body))
	}
}

func TestListAtomRevisions_WithPaginationParams(t *testing.T) {
	// page_size + page_token forwarded verbatim via rawQuery.
	cb := newCaptureBackend(t, http.StatusOK, `{"items":[],"next_page_token":"tok-xyz"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	_, err := a.ListAtomRevisions(context.Background(), sampleAuth(), "atom-abc123", "page_size=5&page_token=tok-abc")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if cb.rawQ != "page_size=5&page_token=tok-abc" {
		t.Errorf("rawQuery forwarded = %q; want page_size=5&page_token=tok-abc", cb.rawQ)
	}
}

func TestListAtomRevisions_EmptyAtomID_404(t *testing.T) {
	// Empty atomID must short-circuit 404 BEFORE any outbound call.
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.ListAtomRevisions(context.Background(), sampleAuth(), "", "")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty atomID", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_ATOM_ID_REQUIRED") {
		t.Errorf("body = %q; want GATEWAY_ATOM_ID_REQUIRED code", string(resp.Body))
	}
}

func TestListAtomRevisions_Upstream401_PassThrough(t *testing.T) {
	// Auth failure from chora-creation must pass through verbatim (not 502).
	cb := newCaptureBackend(t, http.StatusUnauthorized, `{"error":{"code":"UNAUTHORIZED"}}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.ListAtomRevisions(context.Background(), sampleAuth(), "atom-abc", "")
	if resp.Status != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401 verbatim from upstream", resp.Status)
	}
}

func TestListAtomRevisions_Upstream404_PassThrough(t *testing.T) {
	// Atom not found (chora-creation 404) must pass through verbatim.
	cb := newCaptureBackend(t, http.StatusNotFound, `{"error":{"code":"CREATION_ATOM_NOT_FOUND"}}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.ListAtomRevisions(context.Background(), sampleAuth(), "ghost-atom", "")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 verbatim from upstream", resp.Status)
	}
}

func TestListAtomRevisions_Upstream5xx_502(t *testing.T) {
	// Upstream 5xx must normalise to 502 GATEWAY_UPSTREAM_5XX with explicit
	// message (fail-loud per feedback_no_stubs_real_wiring).
	cb := newCaptureBackend(t, http.StatusInternalServerError, `{"error":"boom"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.ListAtomRevisions(context.Background(), sampleAuth(), "atom-abc", "")
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 on upstream 5xx", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_UPSTREAM_5XX") {
		t.Errorf("body = %q; want GATEWAY_UPSTREAM_5XX code in 502 envelope", string(resp.Body))
	}
}

// -----------------------------------------------------------------------------
// CHO-1618 — Personal Collections → chora-creation (same path, verbatim proxy)
// -----------------------------------------------------------------------------

func TestCreateCollection_POST_ProxiesToCreation(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusCreated, `{"collection_id":"col-1","title":"My List"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	body := []byte(`{"title":"My List","visibility":"PRIVATE"}`)
	resp, err := a.CreateCollection(context.Background(), sampleAuth(), body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", resp.Status)
	}
	if cb.path != "/api/v1/collections" {
		t.Errorf("downstream path = %q; want /api/v1/collections", cb.path)
	}
	if cb.method != http.MethodPost {
		t.Errorf("method = %s; want POST", cb.method)
	}
	if cb.body != string(body) {
		t.Errorf("body forwarded = %q; want verbatim", cb.body)
	}
	// Mesh-trust headers — chora-creation tenantContext middleware reads these.
	if cb.hdr.Get("X-Tenant-Id") != sampleAuth().TenantID {
		t.Errorf("X-Tenant-Id = %q; want %q", cb.hdr.Get("X-Tenant-Id"), sampleAuth().TenantID)
	}
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("lowercase gcid header = %q; want %q", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
}

func TestCreateCollection_400_PassesThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusBadRequest, `{"error":{"code":"CREATION_INVALID_BODY"}}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.CreateCollection(context.Background(), sampleAuth(), []byte(`{}`))
	if resp.Status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 verbatim from upstream", resp.Status)
	}
}

func TestListMyCollections_GET_ProxiesToCreation(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"items":[],"total":0}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.ListMyCollections(context.Background(), sampleAuth())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/api/v1/me/collections" {
		t.Errorf("downstream path = %q; want /api/v1/me/collections", cb.path)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %s; want GET", cb.method)
	}
}

func TestGetCollection_ProxiesToCreation(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"collection_id":"col-1"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.GetCollection(context.Background(), sampleAuth(), "col-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/api/v1/collections/col-1" {
		t.Errorf("downstream path = %q; want /api/v1/collections/col-1", cb.path)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %s; want GET", cb.method)
	}
}

func TestGetCollection_EmptyID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.GetCollection(context.Background(), sampleAuth(), "")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty collection id", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_COLLECTION_ID_REQUIRED") {
		t.Errorf("body = %q; want GATEWAY_COLLECTION_ID_REQUIRED code", string(resp.Body))
	}
}

func TestGetCollection_Upstream404_PassThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusNotFound, `{"error":{"code":"CREATION_COLLECTION_NOT_FOUND"}}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.GetCollection(context.Background(), sampleAuth(), "ghost")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 verbatim from upstream", resp.Status)
	}
}

func TestPatchCollection_ProxiesToCreation(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"collection_id":"col-1","title":"Renamed"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	body := []byte(`{"title":"Renamed"}`)
	resp, err := a.PatchCollection(context.Background(), sampleAuth(), "col-1", body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/api/v1/collections/col-1" {
		t.Errorf("downstream path = %q; want /api/v1/collections/col-1", cb.path)
	}
	if cb.method != http.MethodPatch {
		t.Errorf("method = %s; want PATCH", cb.method)
	}
	if cb.body != string(body) {
		t.Errorf("body forwarded = %q; want verbatim", cb.body)
	}
}

func TestPatchCollection_EmptyID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.PatchCollection(context.Background(), sampleAuth(), "", []byte(`{}`))
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty collection id", resp.Status)
	}
}

func TestPatchCollection_Forbidden_PassThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusForbidden, `{"error":{"code":"CREATION_COLLECTION_FORBIDDEN"}}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.PatchCollection(context.Background(), sampleAuth(), "col-1", []byte(`{"title":"x"}`))
	if resp.Status != http.StatusForbidden {
		t.Errorf("status = %d; want 403 verbatim from upstream", resp.Status)
	}
}

func TestDeleteCollection_204_NoBody(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusNoContent, ``)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.DeleteCollection(context.Background(), sampleAuth(), "col-1")
	if resp.Status != http.StatusNoContent {
		t.Errorf("status = %d; want 204", resp.Status)
	}
	if len(resp.Body) != 0 {
		t.Errorf("body length = %d; want empty body on 204 passthrough", len(resp.Body))
	}
	if cb.path != "/api/v1/collections/col-1" {
		t.Errorf("downstream path = %q; want /api/v1/collections/col-1", cb.path)
	}
	if cb.method != http.MethodDelete {
		t.Errorf("method = %s; want DELETE", cb.method)
	}
}

func TestDeleteCollection_EmptyID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.DeleteCollection(context.Background(), sampleAuth(), "")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty collection id", resp.Status)
	}
}

func TestAddCollectionAtom_ProxiesToCreation(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusCreated, `{"collection_id":"col-1","atoms":[{"atom_id":"atom-9"}]}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	body := []byte(`{"atom_id":"atom-9"}`)
	resp, err := a.AddCollectionAtom(context.Background(), sampleAuth(), "col-1", body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", resp.Status)
	}
	if cb.path != "/api/v1/collections/col-1/atoms" {
		t.Errorf("downstream path = %q; want /api/v1/collections/col-1/atoms", cb.path)
	}
	if cb.method != http.MethodPost {
		t.Errorf("method = %s; want POST", cb.method)
	}
	if cb.body != string(body) {
		t.Errorf("body forwarded = %q; want verbatim", cb.body)
	}
}

func TestAddCollectionAtom_EmptyID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.AddCollectionAtom(context.Background(), sampleAuth(), "", []byte(`{"atom_id":"x"}`))
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty collection id", resp.Status)
	}
}

func TestAddCollectionAtom_409_PassesThrough_DuplicateAtom(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusConflict, `{"error":{"code":"CREATION_COLLECTION_DUPLICATE_ATOM"}}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.AddCollectionAtom(context.Background(), sampleAuth(), "col-1", []byte(`{"atom_id":"dup"}`))
	if resp.Status != http.StatusConflict {
		t.Errorf("status = %d; want 409 verbatim from upstream", resp.Status)
	}
}

func TestRemoveCollectionAtom_204_NoBody(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusNoContent, ``)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.RemoveCollectionAtom(context.Background(), sampleAuth(), "col-1", "atom-9")
	if resp.Status != http.StatusNoContent {
		t.Errorf("status = %d; want 204", resp.Status)
	}
	if len(resp.Body) != 0 {
		t.Errorf("body length = %d; want empty body on 204 passthrough", len(resp.Body))
	}
	if cb.path != "/api/v1/collections/col-1/atoms/atom-9" {
		t.Errorf("downstream path = %q; want /api/v1/collections/col-1/atoms/atom-9", cb.path)
	}
	if cb.method != http.MethodDelete {
		t.Errorf("method = %s; want DELETE", cb.method)
	}
}

func TestRemoveCollectionAtom_EmptyCollectionID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.RemoveCollectionAtom(context.Background(), sampleAuth(), "", "atom-9")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty collection id", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_COLLECTION_ID_REQUIRED") {
		t.Errorf("body = %q; want GATEWAY_COLLECTION_ID_REQUIRED code", string(resp.Body))
	}
}

func TestRemoveCollectionAtom_EmptyAtomID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.RemoveCollectionAtom(context.Background(), sampleAuth(), "col-1", "")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty atom id", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_ATOM_ID_REQUIRED") {
		t.Errorf("body = %q; want GATEWAY_ATOM_ID_REQUIRED code", string(resp.Body))
	}
}

func TestCollections_Upstream5xx_502(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusInternalServerError, `{"error":"boom"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.ListMyCollections(context.Background(), sampleAuth())
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 on upstream 5xx", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_UPSTREAM_5XX") {
		t.Errorf("body = %q; want GATEWAY_UPSTREAM_5XX code in 502 envelope", string(resp.Body))
	}
}

// -----------------------------------------------------------------------------
// SearchMyCollections (WS-8 — A+ search-hub collection tab)
// -----------------------------------------------------------------------------

func TestSearchMyCollections_FiltersByTitleAndCountsAtoms(t *testing.T) {
	listBody := `{"items":[
		{"collection_id":"c-1","title":"Git Basics","atoms":[{"atom_id":"a1"},{"atom_id":"a2"}]},
		{"collection_id":"c-2","title":"Algebra","atoms":[{"atom_id":"a3"}]},
		{"collection_id":"c-3","title":"Advanced Git","atoms":[]}
	],"total":3}`
	cb := newCaptureBackend(t, http.StatusOK, listBody)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.SearchMyCollections(context.Background(), sampleAuth(), "git", 10)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	// Reuses the existing caller-scoped list read — no new downstream contract.
	if cb.path != "/api/v1/me/collections" {
		t.Errorf("downstream path = %q, want /api/v1/me/collections", cb.path)
	}

	var got struct {
		Items []struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			AtomCount int    `json:"atom_count"`
		} `json:"items"`
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatalf("decode body: %v (%s)", err, resp.Body)
	}
	if len(got.Items) != 2 {
		t.Fatalf("got %d items, want 2 (Git Basics + Advanced Git)", len(got.Items))
	}
	if got.Items[0].ID != "c-1" || got.Items[0].Title != "Git Basics" || got.Items[0].AtomCount != 2 {
		t.Errorf("item0 = %+v, want id=c-1 title='Git Basics' atom_count=2", got.Items[0])
	}
	if got.Items[1].ID != "c-3" || got.Items[1].AtomCount != 0 {
		t.Errorf("item1 = %+v, want id=c-3 atom_count=0", got.Items[1])
	}
}

func TestSearchMyCollections_EmptyQueryReturnsAllUpToLimit(t *testing.T) {
	listBody := `{"items":[
		{"collection_id":"c-1","title":"One","atoms":[]},
		{"collection_id":"c-2","title":"Two","atoms":[]},
		{"collection_id":"c-3","title":"Three","atoms":[]}
	],"total":3}`
	cb := newCaptureBackend(t, http.StatusOK, listBody)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.SearchMyCollections(context.Background(), sampleAuth(), "", 2)
	var got struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("got %d items, want 2 (limit cap with empty query)", len(got.Items))
	}
}

func TestSearchMyCollections_PropagatesUpstreamAuthError(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusUnauthorized, `{"error":{"code":"UNAUTHENTICATED"}}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.SearchMyCollections(context.Background(), sampleAuth(), "git", 10)
	if resp.Status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 propagated verbatim", resp.Status)
	}
}

func TestSearchMyCollections_Upstream5xxNormalisesTo502(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusInternalServerError, `boom`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.SearchMyCollections(context.Background(), sampleAuth(), "git", 10)
	if resp.Status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (upstream 5xx normalised)", resp.Status)
	}
}
