// lane_d_handler_test.go — HTTP route binding tests for Lane D (#60,
// 2026-05-16) proxy claims that close the GATEWAY_ROUTE_NOT_FOUND gap on
// api.chora.site for the Tier 1 routes Lanes A/B/C just shipped:
//
//	/api/v1/test-sets[/{...}]               → chora-delivery (Lane A)
//	/api/v1/assessments[/{...}]             → chora-delivery (Lane B)
//	/api/v1/me/assessments[/{...}]          → chora-delivery (Lane B)
//	/api/atoms/questions/search             → chora-creation (Lane C)
//
// Strict TDD — tests written BEFORE the handler implementation.
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

// -----------------------------------------------------------------------------
// /api/v1/test-sets[/{...}] — chora-delivery (Lane A)
// -----------------------------------------------------------------------------

func TestGwProxy_TestSets_POST_ProxiesToDelivery(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
		}
		gotBody = string(buf)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"test_set_id":"ts-1","state":"DRAFT"}`))
	})
	h := newGwProxyMux(t, stub)

	w := doGwProxyReq(t, h, http.MethodPost, "/api/v1/test-sets", `{"title":"Smoke","description":"lane D"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201; body=%s", w.Code, w.Body.String())
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s; want POST", gotMethod)
	}
	if gotPath != "/api/v1/test-sets" {
		t.Errorf("downstream path = %q; want /api/v1/test-sets", gotPath)
	}
	if !strings.Contains(gotBody, "Smoke") {
		t.Errorf("body lost; want contains 'Smoke'; got %q", gotBody)
	}
	if !strings.Contains(w.Body.String(), `"test_set_id"`) {
		t.Errorf("response body did not pass through verbatim; got %q", w.Body.String())
	}
}

func TestGwProxy_TestSets_GET_ByID_PreservesPath(t *testing.T) {
	var gotPath, gotMethod string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"test_set_id":"ts-1"}`))
	})
	h := newGwProxyMux(t, stub)

	w := doGwProxyReq(t, h, http.MethodGet, "/api/v1/test-sets/ts-1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %s; want GET", gotMethod)
	}
	if gotPath != "/api/v1/test-sets/ts-1" {
		t.Errorf("downstream path = %q; want /api/v1/test-sets/ts-1", gotPath)
	}
}

func TestGwProxy_TestSets_PATCH_PreservesMethod(t *testing.T) {
	var gotMethod string
	var gotBody string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
		}
		gotBody = string(buf)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"test_set_id":"ts-1","title":"new"}`))
	})
	h := newGwProxyMux(t, stub)

	w := doGwProxyReq(t, h, http.MethodPatch, "/api/v1/test-sets/ts-1", `{"title":"new"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("method = %s; want PATCH", gotMethod)
	}
	if !strings.Contains(gotBody, "new") {
		t.Errorf("body lost; got %q", gotBody)
	}
}

func TestGwProxy_TestSets_DELETE_SubResource(t *testing.T) {
	var gotMethod, gotPath string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	h := newGwProxyMux(t, stub)

	w := doGwProxyReq(t, h, http.MethodDelete, "/api/v1/test-sets/ts-1/questions/q-1", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d; want 204", w.Code)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %s; want DELETE", gotMethod)
	}
	if gotPath != "/api/v1/test-sets/ts-1/questions/q-1" {
		t.Errorf("path = %q; want /api/v1/test-sets/ts-1/questions/q-1", gotPath)
	}
}

func TestGwProxy_TestSets_GET_ForwardsRawQuery(t *testing.T) {
	var gotRawQ string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotRawQ = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	h := newGwProxyMux(t, stub)

	w := doGwProxyReq(t, h, http.MethodGet, "/api/v1/test-sets?state=PUBLISHED&page_size=20", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if gotRawQ != "state=PUBLISHED&page_size=20" {
		t.Errorf("downstream rawQuery = %q; want state=PUBLISHED&page_size=20", gotRawQ)
	}
}

func TestGwProxy_TestSets_Upstream5xx_502(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/v1/test-sets", "")
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", w.Code)
	}
}

// -----------------------------------------------------------------------------
// /api/v1/assessments[/{...}] — chora-delivery (Lane B instructor side)
// -----------------------------------------------------------------------------

func TestGwProxy_Assessments_POST_ProxiesToDelivery(t *testing.T) {
	var gotPath, gotMethod string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"a-1","state":"DRAFT"}`))
	})
	h := newGwProxyMux(t, stub)

	w := doGwProxyReq(t, h, http.MethodPost, "/api/v1/assessments", `{"test_set_id":"ts-1"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201; body=%s", w.Code, w.Body.String())
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s; want POST", gotMethod)
	}
	if gotPath != "/api/v1/assessments" {
		t.Errorf("downstream path = %q; want /api/v1/assessments", gotPath)
	}
}

func TestGwProxy_Assessments_GET_Monitor_PreservesPath(t *testing.T) {
	var gotPath string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"invited_count":3}`))
	})
	h := newGwProxyMux(t, stub)

	w := doGwProxyReq(t, h, http.MethodGet, "/api/v1/assessments/a-1/monitor", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if gotPath != "/api/v1/assessments/a-1/monitor" {
		t.Errorf("path = %q; want /api/v1/assessments/a-1/monitor", gotPath)
	}
}

func TestGwProxy_Assessments_POST_ReleaseResults(t *testing.T) {
	var gotPath, gotMethod string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"released":true}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/v1/assessments/a-1/release-results", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s; want POST", gotMethod)
	}
	if gotPath != "/api/v1/assessments/a-1/release-results" {
		t.Errorf("path = %q; want /api/v1/assessments/a-1/release-results", gotPath)
	}
}

func TestGwProxy_Assessments_Upstream403_PassThrough(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"caller lacks instructor role"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/v1/assessments", `{}`)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403 passthrough", w.Code)
	}
}

// -----------------------------------------------------------------------------
// /api/v1/me/assessments[/{...}] — chora-delivery (Lane B learner side)
// -----------------------------------------------------------------------------

func TestGwProxy_MeAssessments_GET_List(t *testing.T) {
	var gotPath, gotGcid string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotGcid = r.Header.Get("gcid")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[{"id":"a-1"}]}`))
	})
	h := newGwProxyMux(t, stub)

	w := doGwProxyReq(t, h, http.MethodGet, "/api/v1/me/assessments", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if gotPath != "/api/v1/me/assessments" {
		t.Errorf("path = %q; want /api/v1/me/assessments", gotPath)
	}
	// Self-cohort enforcement — gateway stamps the caller GCID; downstream
	// scopes results to that GCID.
	if gotGcid != "gcid-001" {
		t.Errorf("gcid header = %q; want gcid-001 (self-cohort)", gotGcid)
	}
}

func TestGwProxy_MeAssessments_POST_StartSubmission(t *testing.T) {
	var gotPath, gotMethod string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"submission_id":"sub-1","state":"IN_PROGRESS"}`))
	})
	h := newGwProxyMux(t, stub)

	w := doGwProxyReq(t, h, http.MethodPost, "/api/v1/me/assessments/a-1/submissions", `{}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201", w.Code)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s; want POST", gotMethod)
	}
	if gotPath != "/api/v1/me/assessments/a-1/submissions" {
		t.Errorf("path = %q; want /api/v1/me/assessments/a-1/submissions", gotPath)
	}
}

func TestGwProxy_MeAssessments_PATCH_Autosave_PreservesMethod(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
		}
		gotBody = string(buf)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"saved_at":"2026-05-16T00:00:00Z"}`))
	})
	h := newGwProxyMux(t, stub)

	w := doGwProxyReq(t, h, http.MethodPatch,
		"/api/v1/me/assessments/a-1/submissions/sub-1/autosave",
		`{"answers":[{"question_id":"q-1","answer_text":"x"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("method = %s; want PATCH", gotMethod)
	}
	if gotPath != "/api/v1/me/assessments/a-1/submissions/sub-1/autosave" {
		t.Errorf("path = %q; want /api/v1/me/assessments/a-1/submissions/sub-1/autosave", gotPath)
	}
	if !strings.Contains(gotBody, "answer_text") {
		t.Errorf("body lost; want contains 'answer_text'; got %q", gotBody)
	}
}

func TestGwProxy_MeAssessments_POST_SubmitSubmission(t *testing.T) {
	var gotPath, gotMethod string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"state":"SUBMITTED"}`))
	})
	h := newGwProxyMux(t, stub)

	w := doGwProxyReq(t, h, http.MethodPost,
		"/api/v1/me/assessments/a-1/submissions/sub-1/submit", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s; want POST", gotMethod)
	}
	if gotPath != "/api/v1/me/assessments/a-1/submissions/sub-1/submit" {
		t.Errorf("path = %q", gotPath)
	}
}

// -----------------------------------------------------------------------------
// GET /api/atoms/questions/search — chora-creation (Lane C)
// -----------------------------------------------------------------------------

func TestGwProxy_QuestionSearch_GET_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod, gotRawQ string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotRawQ = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[],"page":1,"per":20,"total":0}`))
	})
	h := newGwProxyMux(t, stub)

	w := doGwProxyReq(t, h, http.MethodGet, "/api/atoms/questions/search?q=math&per=20", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", w.Code, w.Body.String())
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %s; want GET", gotMethod)
	}
	if gotPath != "/api/atoms/questions/search" {
		t.Errorf("downstream path = %q; want /api/atoms/questions/search", gotPath)
	}
	if gotRawQ != "q=math&per=20" {
		t.Errorf("downstream rawQuery = %q; want q=math&per=20", gotRawQ)
	}
	if !strings.Contains(w.Body.String(), `"items"`) {
		t.Errorf("body did not pass through verbatim; got %q", w.Body.String())
	}
}

func TestGwProxy_QuestionSearch_405_OnPost(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("downstream MUST NOT be invoked on 405")
		w.WriteHeader(http.StatusOK)
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/atoms/questions/search", `{}`)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on POST", w.Code)
	}
}

func TestGwProxy_QuestionSearch_Upstream5xx_502(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/atoms/questions/search", "")
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", w.Code)
	}
}

// -----------------------------------------------------------------------------
// JWT-gate prefix coverage — DefaultJWTGatedPrefixes MUST cover Lane D routes
// so RequireChoraSessionJWT validates before the handler runs.
// -----------------------------------------------------------------------------

func TestDefaultJWTGatedPrefixes_CoversLaneD(t *testing.T) {
	// Each FE-facing path the FE will hit MUST be covered by at least one prefix
	// in DefaultJWTGatedPrefixes. /api/v1/test-sets and /api/v1/assessments and
	// /api/v1/me/assessments and /api/atoms/questions/search are the 4 leaves
	// Lane D claims.
	wantCovered := []string{
		"/api/v1/test-sets",
		"/api/v1/test-sets/ts-1",
		"/api/v1/test-sets/ts-1/questions",
		"/api/v1/assessments",
		"/api/v1/assessments/a-1",
		"/api/v1/assessments/a-1/release-results",
		"/api/v1/me/assessments",
		"/api/v1/me/assessments/a-1",
		"/api/v1/me/assessments/a-1/submissions/sub-1/submit",
		"/api/atoms/questions/search",
	}
	for _, p := range wantCovered {
		covered := false
		for _, prefix := range httpadapter.DefaultJWTGatedPrefixes {
			if strings.HasPrefix(p, prefix) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("path %q is NOT covered by DefaultJWTGatedPrefixes — JWT gate would skip", p)
		}
	}
}

// -----------------------------------------------------------------------------
// WithGatewayProxy composition — bridge must own Lane D paths (not the base).
// -----------------------------------------------------------------------------

func TestWithGatewayProxy_LaneD_BridgeOwnsPaths(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot) // base sentinel
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	agg := gatewayproxy.New(gatewayproxy.Config{
		DeliveryURL:    stub.URL,
		CreationURL:    stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)

	bridgeOwned := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/test-sets"},
		{http.MethodGet, "/api/v1/test-sets/ts-1"},
		{http.MethodPost, "/api/v1/assessments"},
		{http.MethodGet, "/api/v1/assessments/a-1/monitor"},
		{http.MethodGet, "/api/v1/me/assessments"},
		{http.MethodGet, "/api/v1/me/assessments/a-1"},
		{http.MethodGet, "/api/atoms/questions/search"},
	}
	for _, c := range bridgeOwned {
		r := httptest.NewRequestWithContext(context.Background(), c.method, c.path, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer t")
		r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusTeapot {
			t.Errorf("path %s %s leaked to base — bridge must own it", c.method, c.path)
		}
	}
}
