// gatewayproxy_publish_atom_test.go — A22 Phase K publish-atom gateway route.
//
// Per FE A22 ack: the gateway claims POST /api/atoms/{id}/publish (a new
// /api/atoms/{id}/* sub-resource the previous P7 routing predicate didn't
// cover) and proxies it verbatim to chora-creation. Response is bare
// LearningAtom (no {atom} wrap) per A22.Q2.
//
// Tests cover:
//   - Happy path (200 + body forwarded verbatim, downstream path = /publish)
//   - 404 NOT_FOUND passes through
//   - 409 NO_PUBLISHED_REVISION passes through with discriminated code
//   - 405 on non-POST methods
//   - WithGatewayProxy claims the path (does NOT leak to base)
//   - GET /api/atoms/{id} still falls through (regression guard for Phyllis)
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

func TestGwProxy_PublishAtom_200_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		if r.Header.Get("gcid") != "gcid-001" {
			t.Errorf("downstream lowercase gcid = %q; want gcid-001", r.Header.Get("gcid"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"atom_id":"00000000-0000-7000-8000-00000000a0a2","status":"published","current_revision_id":"01970000-0000-7000-8000-0000000000r1","current_revision_number":1}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost,
		"/api/atoms/00000000-0000-7000-8000-00000000a0a2/publish", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/api/atoms/00000000-0000-7000-8000-00000000a0a2/publish" {
		t.Errorf("downstream path = %q; want verbatim chora-creation /publish path", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("downstream method = %s; want POST", gotMethod)
	}
	// CRITICAL — bare LearningAtom, no {atom} wrap (A22.Q2).
	if strings.HasPrefix(strings.TrimSpace(w.Body.String()), `{"atom":`) {
		t.Errorf("body wrapped with {atom: ...}; want bare LearningAtom; got %q", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"status":"published"`) {
		t.Errorf("body missing status=published; got %q", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"current_revision_number":1`) {
		t.Errorf("body missing current_revision_number; got %q", w.Body.String())
	}
}

func TestGwProxy_PublishAtom_404_PassesThrough(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"CREATION_ATOM_NOT_FOUND","message":"atom not found"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost,
		"/api/atoms/01970000-0000-7000-aaaa-bbbbbbbbbbbb/publish", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 passthrough; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "CREATION_ATOM_NOT_FOUND") {
		t.Errorf("body = %q; want CREATION_ATOM_NOT_FOUND passed through verbatim", w.Body.String())
	}
}

func TestGwProxy_PublishAtom_409_NoPublishedRevision_PassesThrough(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"CREATION_ATOM_NO_PUBLISHED_REVISION","message":"cannot publish atom without at least one published revision"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost,
		"/api/atoms/00000000-0000-7000-8000-00000000a0a2/publish", "")
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d; want 409 passthrough", w.Code)
	}
	if !strings.Contains(w.Body.String(), "CREATION_ATOM_NO_PUBLISHED_REVISION") {
		t.Errorf("body = %q; want discriminated code passed through verbatim", w.Body.String())
	}
}

func TestGwProxy_PublishAtom_405_OnNonPost(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	for _, m := range []string{http.MethodGet, http.MethodPatch, http.MethodDelete} {
		t.Run(m, func(t *testing.T) {
			w := doGwProxyReq(t, h, m,
				"/api/atoms/00000000-0000-7000-8000-00000000a0a2/publish", "")
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("method %s: status = %d; want 405", m, w.Code)
			}
		})
	}
}

// WithGatewayProxy must claim /api/atoms/{id}/publish — it must NOT fall
// through to the base (Phyllis) router. Mirrors the P7 questions test.
func TestWithGatewayProxy_AtomPublishPath_HandledByBridge(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"atom_id":"x","status":"published"}`))
	})
	agg := gatewayproxy.New(gatewayproxy.Config{
		CreationURL:    stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/atoms/atom-1/publish", nil)
	r.Header.Set("Authorization", "Bearer t")
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusTeapot {
		t.Error("publish path leaked to base — bridge must own /api/atoms/{id}/publish")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 from the bridge", w.Code)
	}
}

// Regression guard: GET /api/atoms/{id} (Phyllis-owned atom read) MUST
// continue to fall through to the base router. Adding /publish to the
// bridge must not regress the existing FE A16 read-side.
func TestWithGatewayProxy_AtomGetPath_StillFallsThroughAfterPublishAdded(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	agg := gatewayproxy.New(gatewayproxy.Config{
		CreationURL:    stub.URL,
		ConsumptionURL: stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)
	// Sibling /api/atoms/{id}/* paths that MUST still go to Phyllis.
	for _, p := range []string{
		"/api/atoms/00000000-0000-7000-8000-00000000a0a2",
		"/api/atoms/00000000-0000-7000-8000-00000000a0a2/feedback",
	} {
		r := httptest.NewRequest(http.MethodGet, p, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusTeapot {
			t.Errorf("path %q: status = %d; want 418 — non-publish atom path must fall through to Phyllis", p, w.Code)
		}
	}
}
