// gatewayproxy_reorder_clone_test.go — HTTP route binding tests for the Wave 2
// BFF proxy routes: QuestionBank question reorder + Atom clone. Both proxy
// method + path + body verbatim to chora-creation, mirroring the W3.B.1
// QuestionBank + P7 atom-question handler bindings.
//
//	POST /api/v1/question-banks/{id}/reorder   (handleQuestionBanksSubpath)
//	POST /api/atoms/{atom_id}/clone            (handleAtomSubpath)
//
// Reuses newGwProxyStub / newGwProxyMux / doGwProxyReq from
// gatewayproxy_handler_test.go (same package).
//
// Strict TDD: tests written BEFORE the handler wiring.
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
// POST /api/v1/question-banks/{id}/reorder
// -----------------------------------------------------------------------------

func TestGwProxy_ReorderQuestionBank_200_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
		}
		gotBody = string(buf)
		if r.Header.Get("gcid") != "gcid-001" {
			t.Errorf("downstream lowercase gcid = %q; want gcid-001", r.Header.Get("gcid"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"qb-1"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/v1/question-banks/qb-1/reorder", `{"question_ids":["q-2","q-1"]}`)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if gotPath != "/api/v1/question-banks/qb-1/reorder" {
		t.Errorf("downstream path = %q; want /api/v1/question-banks/qb-1/reorder", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("downstream method = %s; want POST", gotMethod)
	}
	if gotBody != `{"question_ids":["q-2","q-1"]}` {
		t.Errorf("downstream body = %q; want verbatim", gotBody)
	}
}

func TestGwProxy_ReorderQuestionBank_405_OnGet(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/v1/question-banks/qb-1/reorder", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on GET /api/v1/question-banks/{id}/reorder", w.Code)
	}
}

// -----------------------------------------------------------------------------
// POST /api/atoms/{atom_id}/clone
// -----------------------------------------------------------------------------

func TestGwProxy_CloneAtom_201_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
		}
		gotBody = string(buf)
		if r.Header.Get("gcid") != "gcid-001" {
			t.Errorf("downstream lowercase gcid = %q; want gcid-001", r.Header.Get("gcid"))
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"atom_id":"atom-2"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/atoms/atom-1/clone", `{"title":"Copy"}`)
	if w.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201", w.Code)
	}
	if gotPath != "/api/atoms/atom-1/clone" {
		t.Errorf("downstream path = %q; want /api/atoms/atom-1/clone", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("downstream method = %s; want POST", gotMethod)
	}
	if gotBody != `{"title":"Copy"}` {
		t.Errorf("downstream body = %q; want verbatim", gotBody)
	}
}

func TestGwProxy_CloneAtom_405_OnGet(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet, "/api/atoms/atom-1/clone", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on GET /api/atoms/{atom_id}/clone", w.Code)
	}
}

func TestGwProxy_CloneAtom_404_PassesThrough(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"CREATION_ATOM_NOT_FOUND"}}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/atoms/ghost/clone", `{}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 passed through", w.Code)
	}
	if !strings.Contains(w.Body.String(), "CREATION_ATOM_NOT_FOUND") {
		t.Errorf("body = %q; want CREATION_ATOM_NOT_FOUND passed through verbatim", w.Body.String())
	}
}

// -----------------------------------------------------------------------------
// WithGatewayProxy composition — the bridge MUST claim both new leaves (they
// must NOT fall through to the base router; clone in particular must NOT leak to
// the Phyllis-owned /api/atoms/* surface).
// -----------------------------------------------------------------------------

func TestWithGatewayProxy_ReorderAndClonePaths_HandledByBridge(t *testing.T) {
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
	for _, p := range []string{
		"/api/v1/question-banks/qb-1/reorder",
		"/api/atoms/atom-1/clone",
	} {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, p, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer t")
		r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusTeapot {
			t.Errorf("POST %s: leaked to base — bridge must own the Wave 2 route", p)
		}
	}
}

// Composition discipline: adding /clone to the bridge must NOT regress the
// existing Phyllis-owned /api/atoms/* read/feedback/ai-assist paths.
func TestWithGatewayProxy_AtomNonClonePath_StillFallsThroughToPhyllis(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	agg := gatewayproxy.New(gatewayproxy.Config{
		CreationURL:    stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)
	for _, p := range []string{
		"/api/atoms/atom-1",          // GET atom detail — Phyllis
		"/api/atoms/atom-1/feedback", // POST feedback — Phyllis
		"/api/atoms/ai-assist",       // QGen — Phyllis
	} {
		r := httptest.NewRequest(http.MethodGet, p, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusTeapot {
			t.Errorf("GET %s: bridge captured a Phyllis-owned path (code=%d) — clone must not over-claim", p, w.Code)
		}
	}
}
