// gatewayproxy_reuse_visibility_test.go — HTTP route binding for the ADR-229
// WS-1 (CHO-2127) author reuse-consent leaf:
//
//	PATCH /api/atoms/{atom_id}/reuse-visibility   (handleAtomSubpath)
//
// Proxies method + path + body verbatim to chora-creation, mirroring the
// Phase-A.2 clone lane. Reuses newGwProxyStub / newGwProxyMux / doGwProxyReq
// from gatewayproxy_handler_test.go (same package).
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

func TestGwProxy_ReuseVisibility_200_ProxiesToCreation(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"atom_id":"atom-1","reuse_visibility":"tenant"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPatch, "/api/atoms/atom-1/reuse-visibility", `{"reuse_visibility":"tenant"}`)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 (body=%s)", w.Code, w.Body.String())
	}
	if gotPath != "/api/atoms/atom-1/reuse-visibility" {
		t.Errorf("downstream path = %q; want /api/atoms/atom-1/reuse-visibility", gotPath)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("downstream method = %s; want PATCH", gotMethod)
	}
	if gotBody != `{"reuse_visibility":"tenant"}` {
		t.Errorf("downstream body = %q; want verbatim", gotBody)
	}
}

// A downstream 403 (CREATION_NOT_AUTHOR) must pass through verbatim — it is
// the semantic signal the FE renders.
func TestGwProxy_ReuseVisibility_403_PassesThrough(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"CREATION_NOT_AUTHOR","message":"only the atom author may change this"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPatch, "/api/atoms/atom-1/reuse-visibility", `{"reuse_visibility":"tenant"}`)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403 passthrough", w.Code)
	}
	if !strings.Contains(w.Body.String(), "CREATION_NOT_AUTHOR") {
		t.Errorf("body = %q; want the downstream CREATION_NOT_AUTHOR envelope verbatim", w.Body.String())
	}
}

func TestGwProxy_ReuseVisibility_405_OnPost(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost, "/api/atoms/atom-1/reuse-visibility", `{}`)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on POST /api/atoms/{id}/reuse-visibility", w.Code)
	}
}

// TestWithGatewayProxy_ReuseVisibilityPath_HandledByBridge — the dispatcher
// must CLAIM the leaf (matchesGatewayProxyPath) so it never leaks to the
// Phyllis base router, which has no reuse-visibility route.
func TestWithGatewayProxy_ReuseVisibilityPath_HandledByBridge(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	agg := gatewayproxy.New(gatewayproxy.Config{
		CreationURL:    stub.URL,
		PerCallTimeout: time.Second,
	})
	baseHit := false
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		baseHit = true
		w.WriteHeader(http.StatusNotFound)
	})
	h := httpadapter.WithGatewayProxy(base, agg)

	r := httptest.NewRequestWithContext(context.Background(), http.MethodPatch,
		"/api/atoms/atom-1/reuse-visibility", strings.NewReader(`{"reuse_visibility":"private"}`))
	r.Header.Set("Authorization", "Bearer testtoken")
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if baseHit {
		t.Fatalf("PATCH /api/atoms/{id}/reuse-visibility leaked to the base (Phyllis) router; the bridge must claim it")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 from the bridge", w.Code)
	}
}
