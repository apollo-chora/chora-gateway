// gatewayproxy_transcript_test.go — HTTP route-binding tests for the W6
// StudentTranscript read API BFF proxy:
//
//	GET /api/v1/me/transcript              (self, gcid-scoped)
//	GET /api/v1/transcript/by-assessments  (tenant-wide, role-gated downstream)
//
// Mirrors the CHO-2040 R8-6 Proofing Tests GET-only-leaf tests in
// gatewayproxy_handler_test.go (route present, query forwarded, 405 on wrong
// method); DefaultJWTGatedPrefixes coverage is asserted separately in
// jwt_auth_test.go.
package httpadapter_test

import (
	"net/http"
	"testing"
)

// -----------------------------------------------------------------------------
// GET /api/v1/me/transcript
// -----------------------------------------------------------------------------

func TestGwProxy_MeTranscript_200_ForwardsQuery(t *testing.T) {
	var gotPath, gotQuery string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		if r.Header.Get("gcid") != "gcid-001" {
			t.Errorf("downstream lowercase gcid = %q; want gcid-001", r.Header.Get("gcid"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/v1/me/transcript?limit=10", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/v1/me/transcript" {
		t.Errorf("downstream path = %q; want /v1/me/transcript", gotPath)
	}
	if gotQuery != "limit=10" {
		t.Errorf("downstream query = %q; want limit=10", gotQuery)
	}
}

func TestGwProxy_MeTranscript_405_OnPost(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/v1/me/transcript", `{}`)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on POST", w.Code)
	}
}

// -----------------------------------------------------------------------------
// GET /api/v1/transcript/by-assessments
// -----------------------------------------------------------------------------

func TestGwProxy_TranscriptByAssessments_200_ForwardsQuery(t *testing.T) {
	var gotPath, gotQuery string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		if r.Header.Get("X-Tenant-Id") != "tenant-001" {
			t.Errorf("downstream X-Tenant-Id = %q; want tenant-001", r.Header.Get("X-Tenant-Id"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/v1/transcript/by-assessments?assessment_ids=a-1,a-2", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/v1/transcript/by-assessments" {
		t.Errorf("downstream path = %q; want /v1/transcript/by-assessments", gotPath)
	}
	if gotQuery != "assessment_ids=a-1,a-2" {
		t.Errorf("downstream query = %q; want assessment_ids=a-1,a-2", gotQuery)
	}
}

func TestGwProxy_TranscriptByAssessments_405_OnPost(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/v1/transcript/by-assessments", `{}`)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on POST", w.Code)
	}
}
