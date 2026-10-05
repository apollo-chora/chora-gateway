// gatewayproxy_questionbank_test.go — HTTP route binding tests for the
// QuestionBank BFF proxy routes (W3.B.1). These mirror the CHO-1618 Personal
// Collections handler bindings EXACTLY: the gateway claims the route shapes and
// proxies method + path + body verbatim to chora-creation, which serves the
// QuestionBank route table at the SAME path the FE hits.
//
// Routes:
//   - POST   /api/v1/question-banks
//   - GET    /api/v1/me/question-banks
//   - GET    /api/v1/question-banks/{id}
//   - PATCH  /api/v1/question-banks/{id}
//   - DELETE /api/v1/question-banks/{id}
//   - POST   /api/v1/question-banks/{id}/questions
//   - GET    /api/v1/question-banks/{id}/questions
//   - DELETE /api/v1/question-banks/{id}/questions/{qid}
//   - POST   /api/v1/question-banks/{id}/assemble-test-set
//
// Plus: mesh-trust header propagation, 405 on wrong method, missing-id 404,
// WithGatewayProxy composition (bridge ownership), and JWT-gate coverage.
//
// Reuses newGwProxyStub / newGwProxyMux / doGwProxyReq from
// gatewayproxy_handler_test.go (same package).
//
// Strict TDD: tests written BEFORE the handler implementation.
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
// POST /api/v1/question-banks (create)
// -----------------------------------------------------------------------------

func TestGwProxy_QuestionBankCreate_201_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		if r.Header.Get("gcid") != "gcid-001" {
			t.Errorf("downstream lowercase gcid = %q; want gcid-001", r.Header.Get("gcid"))
		}
		if r.Header.Get("X-Tenant-Id") != "tenant-001" {
			t.Errorf("downstream X-Tenant-Id = %q; want tenant-001", r.Header.Get("X-Tenant-Id"))
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"qb-1"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/v1/question-banks", `{"name":"My Bank"}`)
	if w.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201", w.Code)
	}
	if gotPath != "/api/v1/question-banks" {
		t.Errorf("downstream path = %q; want /api/v1/question-banks", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("downstream method = %s; want POST", gotMethod)
	}
}

func TestGwProxy_QuestionBanksCollection_405_OnGet(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	// GET on the collection root is not a valid route (the canonical list is
	// GET /api/v1/me/question-banks). Expect 405.
	w := doGwProxyReq(t, h, http.MethodGet, "/api/v1/question-banks", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on GET /api/v1/question-banks", w.Code)
	}
}

// -----------------------------------------------------------------------------
// GET /api/v1/me/question-banks (caller-scoped list)
// -----------------------------------------------------------------------------

func TestGwProxy_ListMyQuestionBanks_200_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/v1/me/question-banks", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/api/v1/me/question-banks" {
		t.Errorf("downstream path = %q; want /api/v1/me/question-banks", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("downstream method = %s; want GET", gotMethod)
	}
}

func TestGwProxy_ListMyQuestionBanks_405_OnPost(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/v1/me/question-banks", `{}`)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on POST /api/v1/me/question-banks", w.Code)
	}
}

// -----------------------------------------------------------------------------
// GET / PATCH / DELETE /api/v1/question-banks/{id}
// -----------------------------------------------------------------------------

func TestGwProxy_GetQuestionBank_200_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"qb-1"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/v1/question-banks/qb-1", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/api/v1/question-banks/qb-1" {
		t.Errorf("downstream path = %q; want /api/v1/question-banks/qb-1", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("downstream method = %s; want GET", gotMethod)
	}
}

func TestGwProxy_PatchQuestionBank_200_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
		}
		gotBody = string(buf)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"qb-1","name":"Renamed"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPatch, "/api/v1/question-banks/qb-1", `{"name":"Renamed"}`)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/api/v1/question-banks/qb-1" {
		t.Errorf("downstream path = %q; want /api/v1/question-banks/qb-1", gotPath)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("downstream method = %s; want PATCH", gotMethod)
	}
	if gotBody != `{"name":"Renamed"}` {
		t.Errorf("downstream body = %q; want verbatim", gotBody)
	}
}

func TestGwProxy_DeleteQuestionBank_204_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusNoContent)
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodDelete, "/api/v1/question-banks/qb-1", "")
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d; want 204", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("body = %q; want empty 204 body passed through", w.Body.String())
	}
	if gotPath != "/api/v1/question-banks/qb-1" {
		t.Errorf("downstream path = %q; want /api/v1/question-banks/qb-1", gotPath)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("downstream method = %s; want DELETE", gotMethod)
	}
}

func TestGwProxy_QuestionBankItem_405_OnUnsupportedMethod(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	// POST on /{id} is not a valid leaf (GET/PATCH/DELETE only). Expect 405.
	w := doGwProxyReq(t, h, http.MethodPost, "/api/v1/question-banks/qb-1", `{}`)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on POST /api/v1/question-banks/{id}", w.Code)
	}
}

func TestGwProxy_QuestionBankItem_404_MissingID(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	// Trailing slash with no id — the subtree dispatcher must 404 BEFORE any
	// outbound call (never build a downstream URL with an empty segment).
	w := doGwProxyReq(t, h, http.MethodGet, "/api/v1/question-banks/", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on missing question bank id", w.Code)
	}
	if !strings.Contains(w.Body.String(), "GATEWAY_QUESTION_BANK_ID_REQUIRED") {
		t.Errorf("body = %q; want GATEWAY_QUESTION_BANK_ID_REQUIRED", w.Body.String())
	}
}

// -----------------------------------------------------------------------------
// POST / GET /api/v1/question-banks/{id}/questions
// -----------------------------------------------------------------------------

func TestGwProxy_AddQuestionBankQuestion_201_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
		}
		gotBody = string(buf)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"qb-1"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/v1/question-banks/qb-1/questions", `{"question_id":"q-9"}`)
	if w.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201", w.Code)
	}
	if gotPath != "/api/v1/question-banks/qb-1/questions" {
		t.Errorf("downstream path = %q; want /api/v1/question-banks/qb-1/questions", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("downstream method = %s; want POST", gotMethod)
	}
	if gotBody != `{"question_id":"q-9"}` {
		t.Errorf("downstream body = %q; want verbatim", gotBody)
	}
}

func TestGwProxy_ListQuestionBankQuestions_200_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/v1/question-banks/qb-1/questions", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/api/v1/question-banks/qb-1/questions" {
		t.Errorf("downstream path = %q; want /api/v1/question-banks/qb-1/questions", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("downstream method = %s; want GET", gotMethod)
	}
}

func TestGwProxy_QuestionBankQuestions_405_OnPatch(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	// PATCH on the questions collection is not a valid route (POST/GET only).
	w := doGwProxyReq(t, h, http.MethodPatch, "/api/v1/question-banks/qb-1/questions", `{}`)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on PATCH /api/v1/question-banks/{id}/questions", w.Code)
	}
}

// -----------------------------------------------------------------------------
// DELETE /api/v1/question-banks/{id}/questions/{qid}
// -----------------------------------------------------------------------------

func TestGwProxy_RemoveQuestionBankQuestion_204_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusNoContent)
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodDelete, "/api/v1/question-banks/qb-1/questions/q-9", "")
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d; want 204", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("body = %q; want empty 204 body passed through", w.Body.String())
	}
	if gotPath != "/api/v1/question-banks/qb-1/questions/q-9" {
		t.Errorf("downstream path = %q; want /api/v1/question-banks/qb-1/questions/q-9", gotPath)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("downstream method = %s; want DELETE", gotMethod)
	}
}

// -----------------------------------------------------------------------------
// POST /api/v1/question-banks/{id}/assemble-test-set
// -----------------------------------------------------------------------------

func TestGwProxy_AssembleQuestionBankTestSet_202_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
		}
		gotBody = string(buf)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":"job-1","test_set_status":"assembling"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/v1/question-banks/qb-1/assemble-test-set", `{"title":"Midterm"}`)
	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d; want 202", w.Code)
	}
	if gotPath != "/api/v1/question-banks/qb-1/assemble-test-set" {
		t.Errorf("downstream path = %q; want /api/v1/question-banks/qb-1/assemble-test-set", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("downstream method = %s; want POST", gotMethod)
	}
	if gotBody != `{"title":"Midterm"}` {
		t.Errorf("downstream body = %q; want verbatim", gotBody)
	}
}

func TestGwProxy_AssembleQuestionBankTestSet_405_OnGet(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/v1/question-banks/qb-1/assemble-test-set", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on GET /api/v1/question-banks/{id}/assemble-test-set", w.Code)
	}
}

// -----------------------------------------------------------------------------
// 4xx passthrough — a downstream domain 4xx proves the request reached the
// chora-creation handler and must surface unchanged.
// -----------------------------------------------------------------------------

func TestGwProxy_QuestionBank_409_PassesThrough_DuplicateQuestion(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"CREATION_QUESTION_BANK_DUPLICATE_QUESTION"}}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/v1/question-banks/qb-1/questions", `{"question_id":"dup"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d; want 409 passed through", w.Code)
	}
	if !strings.Contains(w.Body.String(), "CREATION_QUESTION_BANK_DUPLICATE_QUESTION") {
		t.Errorf("body = %q; want CREATION_QUESTION_BANK_DUPLICATE_QUESTION passed through verbatim", w.Body.String())
	}
}

// -----------------------------------------------------------------------------
// WithGatewayProxy composition — the bridge MUST claim the question-bank paths
// (they must NOT fall through to the base router).
// -----------------------------------------------------------------------------

func TestWithGatewayProxy_QuestionBanksPath_HandledByBridge(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	agg := gatewayproxy.New(gatewayproxy.Config{
		CreationURL:    stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPost, "/api/v1/question-banks"},
		{http.MethodGet, "/api/v1/me/question-banks"},
		{http.MethodGet, "/api/v1/question-banks/qb-1"},
		{http.MethodPatch, "/api/v1/question-banks/qb-1"},
		{http.MethodDelete, "/api/v1/question-banks/qb-1"},
		{http.MethodPost, "/api/v1/question-banks/qb-1/questions"},
		{http.MethodGet, "/api/v1/question-banks/qb-1/questions"},
		{http.MethodDelete, "/api/v1/question-banks/qb-1/questions/q-9"},
		{http.MethodPost, "/api/v1/question-banks/qb-1/assemble-test-set"},
	} {
		r := httptest.NewRequestWithContext(context.Background(), tc.method, tc.path, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer t")
		r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusTeapot {
			t.Errorf("%s %s: leaked to base — bridge must own the question-bank routes", tc.method, tc.path)
		}
	}
}

// -----------------------------------------------------------------------------
// JWT-gate prefix coverage — DefaultJWTGatedPrefixes MUST cover the question-bank
// routes so RequireChoraSessionJWT validates + stamps mesh claims before the
// handler runs (else the downstream chora-creation tenantContext 4xxs).
// -----------------------------------------------------------------------------

func TestDefaultJWTGatedPrefixes_CoversQuestionBanks(t *testing.T) {
	wantCovered := []string{
		"/api/v1/question-banks",
		"/api/v1/question-banks/qb-1",
		"/api/v1/question-banks/qb-1/questions",
		"/api/v1/question-banks/qb-1/questions/q-9",
		"/api/v1/question-banks/qb-1/assemble-test-set",
		"/api/v1/me/question-banks",
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
			t.Errorf("path %q is NOT covered by DefaultJWTGatedPrefixes — JWT gate would skip, mesh claims would be empty, chora-creation would 4xx", p)
		}
	}
}
