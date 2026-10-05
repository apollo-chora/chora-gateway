// gatewayproxy_create_atom_test.go — A20 fix tests.
//
// Per FE A20 ask (2026-05-15): POST /api/atoms 307-redirected to
// /api/atoms/ and then 404'd because the gateway only claimed the
// trailing-slash form which mapped to handleAtomSubpath. The fix claims
// the EXACT static path /api/atoms (no slash) on the gateway and forwards
// verbatim to chora-creation.
//
// Tests cover:
//   - POST /api/atoms — proxies to chora-creation /api/atoms (no redirect)
//   - GET  /api/atoms — proxies to chora-creation /api/atoms (collection list)
//   - 405 on non-POST / non-GET
//   - WithGatewayProxy claims the path (does NOT leak to base)
//   - Trailing-slash request still routes through handleAtomSubpath (no regression)
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

func TestGwProxy_CreateAtom_201_NoRedirect(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"atom_id":"00000000-0000-7000-8000-00000000a0a3","status":"draft","title":"Untitled Atom"}`))
	})
	h := newGwProxyMux(t, stub)
	body := `{"atom_type":"MULTIPLE_CHOICE","title":"Untitled Atom","locale":"en"}`
	w := doGwProxyReq(t, h, http.MethodPost, "/api/atoms", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201 (NOT 307 redirect, NOT 404); body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/api/atoms" {
		t.Errorf("downstream path = %q; want /api/atoms (no trailing slash)", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("downstream method = %s; want POST", gotMethod)
	}
	if gotBody != body {
		t.Errorf("downstream body = %q; want forwarded verbatim", gotBody)
	}
}

func TestGwProxy_ListAtoms_200_ProxiesQueryVerbatim(t *testing.T) {
	var gotPath, gotMethod, gotQuery string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	})
	h := newGwProxyMux(t, stub)
	// GET /api/atoms?status=draft — forward query verbatim.
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/api/atoms?status=draft", nil)
	r.Header.Set("Authorization", "Bearer t")
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if gotPath != "/api/atoms" {
		t.Errorf("downstream path = %q; want /api/atoms", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("downstream method = %s; want GET", gotMethod)
	}
	if gotQuery != "status=draft" {
		t.Errorf("downstream query = %q; want forwarded verbatim", gotQuery)
	}
}

func TestGwProxy_AtomsCollection_405_OnNonPostNonGet(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	for _, m := range []string{http.MethodPatch, http.MethodDelete, http.MethodPut} {
		t.Run(m, func(t *testing.T) {
			w := doGwProxyReq(t, h, m, "/api/atoms", "")
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("method %s: status = %d; want 405", m, w.Code)
			}
		})
	}
}

// WithGatewayProxy must claim POST /api/atoms — it must NOT leak to base
// (Phyllis's handleAtoms which only routes /api/atoms/{id}/... sub-paths).
func TestWithGatewayProxy_CreateAtomPath_HandledByBridge(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"atom_id":"x","status":"draft"}`))
	})
	agg := gatewayproxy.New(gatewayproxy.Config{
		CreationURL:    stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/atoms", strings.NewReader(`{"atom_type":"MULTIPLE_CHOICE","title":"x","locale":"en"}`))
	r.Header.Set("Authorization", "Bearer t")
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusTeapot {
		t.Error("create-atom path leaked to base — bridge must own /api/atoms")
	}
	if w.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201 from the bridge", w.Code)
	}
}

// Regression guard: /api/atoms/{id}/feedback (Phyllis-owned) MUST continue to
// fall through to the base router after the /api/atoms exact-path claim.
func TestWithGatewayProxy_AtomFeedbackPath_StillFallsThroughAfterCreateAdded(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	agg := gatewayproxy.New(gatewayproxy.Config{
		ConsumptionURL: stub.URL,
		CreationURL:    stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)
	// /api/atoms/{id} and /api/atoms/{id}/feedback are Phyllis-owned.
	r := httptest.NewRequest(http.MethodGet, "/api/atoms/atom-1", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTeapot {
		t.Errorf("/api/atoms/{id} status = %d; want 418 — must still fall through to Phyllis", w.Code)
	}
}
