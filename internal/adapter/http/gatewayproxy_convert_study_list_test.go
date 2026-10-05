// gatewayproxy_convert_study_list_test.go — HTTP route binding tests for the
// WS-4 Collections → study-list conversion route (ADR-233).
//
// Route:
//   - POST /api/v1/collections/{id}/convert-to-study-list
//
// The gateway claims the shape and proxies method + path + body VERBATIM to
// chora-creation, which serves it at the SAME path. Mirrors the W3.B.1
// /api/v1/question-banks/{id}/assemble-test-set binding EXACTLY — same
// single-verb sub-path shape under an already-claimed parametric subtree.
//
// ADR-233 D11 status contract (all passed through verbatim by the bridge):
//   - 201 → {study_list_event_id, atom_count, excluded[]}  partial success
//   - 409 CREATION_COLLECTION_NO_ENTITLED_ATOMS            zero survivors
//   - 403 CREATION_COLLECTION_FORBIDDEN                    actor != owner
//
// Reuses newGwProxyStub / newGwProxyMux / doGwProxyReq from
// gatewayproxy_handler_test.go (same package).
//
// Strict TDD: these tests were written BEFORE the handler + aggregator methods.
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
// POST /api/v1/collections/{id}/convert-to-study-list — happy path (201).
// -----------------------------------------------------------------------------

func TestGwProxy_ConvertToStudyList_201_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		// Mesh-trust headers must be stamped by the canonical call() helper —
		// chora-creation's tenantContext middleware 4xxs without them.
		if r.Header.Get("gcid") != "gcid-001" {
			t.Errorf("downstream lowercase gcid = %q; want gcid-001", r.Header.Get("gcid"))
		}
		if r.Header.Get("X-Tenant-Id") != "tenant-001" {
			t.Errorf("downstream X-Tenant-Id = %q; want tenant-001", r.Header.Get("X-Tenant-Id"))
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"study_list_event_id":"019f-aaa","atom_count":18,"excluded":[]}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost,
		"/api/v1/collections/col-1/convert-to-study-list", `{}`)

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201", w.Code)
	}
	if gotPath != "/api/v1/collections/col-1/convert-to-study-list" {
		t.Errorf("downstream path = %q; want /api/v1/collections/col-1/convert-to-study-list", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("downstream method = %s; want POST", gotMethod)
	}
	if gotBody != `{}` {
		t.Errorf("downstream body = %q; want {} forwarded verbatim", gotBody)
	}
	if !strings.Contains(w.Body.String(), `"study_list_event_id":"019f-aaa"`) {
		t.Errorf("body = %q; want the 201 envelope passed through verbatim", w.Body.String())
	}
}

// The D11 partial-success payload — the excluded[] list with reason codes MUST
// survive the bridge byte-for-byte. The gatewayproxy classify() is a verbatim
// passthrough (unlike CompanionBridge, which camelises); a reshape here would
// silently drop the named exclusions the learner is owed.
func TestGwProxy_ConvertToStudyList_201_ExcludedListPassesThroughVerbatim(t *testing.T) {
	const payload = `{"study_list_event_id":"019f-bbb","atom_count":2,` +
		`"excluded":[{"atom_id":"019f-aaa","reason":"REUSE_VISIBILITY_NARROWED"},` +
		`{"atom_id":"019f-ccc","reason":"ATOM_NOT_PUBLISHED"}]}`
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(payload))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost,
		"/api/v1/collections/col-1/convert-to-study-list", `{}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201", w.Code)
	}
	for _, want := range []string{
		`"excluded"`,
		`"atom_id":"019f-aaa"`,
		`"reason":"REUSE_VISIBILITY_NARROWED"`,
		`"reason":"ATOM_NOT_PUBLISHED"`,
		`"atom_count":2`,
	} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("body missing %s — D11 named exclusions must not be reshaped by the bridge; got %q",
				want, w.Body.String())
		}
	}
}

// -----------------------------------------------------------------------------
// Domain verdicts pass through verbatim (ADR-233 D11 + D9).
// -----------------------------------------------------------------------------

func TestGwProxy_ConvertToStudyList_409_NoEntitledAtoms(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"CREATION_COLLECTION_NO_ENTITLED_ATOMS","message":"no entitled atoms"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost,
		"/api/v1/collections/col-1/convert-to-study-list", `{}`)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d; want 409 passed through", w.Code)
	}
	if !strings.Contains(w.Body.String(), "CREATION_COLLECTION_NO_ENTITLED_ATOMS") {
		t.Errorf("body = %q; want CREATION_COLLECTION_NO_ENTITLED_ATOMS verbatim", w.Body.String())
	}
}

func TestGwProxy_ConvertToStudyList_403_Forbidden(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"CREATION_COLLECTION_FORBIDDEN","message":"not the owner"}`))
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost,
		"/api/v1/collections/col-1/convert-to-study-list", `{}`)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403 passed through", w.Code)
	}
	if !strings.Contains(w.Body.String(), "CREATION_COLLECTION_FORBIDDEN") {
		t.Errorf("body = %q; want CREATION_COLLECTION_FORBIDDEN verbatim", w.Body.String())
	}
}

// -----------------------------------------------------------------------------
// Method gate — the dispatcher 405s eagerly so a misbehaving FE call does not
// waste a downstream round-trip (mirrors the assemble-test-set branch).
// -----------------------------------------------------------------------------

func TestGwProxy_ConvertToStudyList_405_OnGet(t *testing.T) {
	called := false
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodGet,
		"/api/v1/collections/col-1/convert-to-study-list", "")

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405", w.Code)
	}
	if called {
		t.Error("downstream was called on a 405 path — the gateway must 405 eagerly")
	}
}

// A deeper sub-path under the verb must still 404 — the convert route is a leaf,
// not a subtree. Guards against the branch being written as a HasPrefix match.
func TestGwProxy_ConvertToStudyList_404_OnDeeperSubpath(t *testing.T) {
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := newGwProxyMux(t, stub)
	w := doGwProxyReq(t, h, http.MethodPost,
		"/api/v1/collections/col-1/convert-to-study-list/extra", `{}`)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 — convert-to-study-list is a leaf, not a subtree", w.Code)
	}
}

// -----------------------------------------------------------------------------
// WithGatewayProxy composition — the bridge MUST claim the convert path (it must
// NOT fall through to the base router).
// -----------------------------------------------------------------------------

func TestWithGatewayProxy_ConvertToStudyListPath_HandledByBridge(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"study_list_event_id":"019f-aaa","atom_count":1,"excluded":[]}`))
	})
	agg := gatewayproxy.New(gatewayproxy.Config{
		CreationURL:    stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)

	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/v1/collections/col-1/convert-to-study-list", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer t")
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code == http.StatusTeapot {
		t.Fatal("POST /api/v1/collections/{id}/convert-to-study-list leaked to base — " +
			"the bridge must own the whole /api/v1/collections/ subtree")
	}
	if w.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201 from the bridge", w.Code)
	}
}

// -----------------------------------------------------------------------------
// JWT-gate prefix coverage — DefaultJWTGatedPrefixes MUST cover the convert route
// so RequireChoraSessionJWT validates + stamps mesh claims before the handler
// runs (else chora-creation's tenantContext 4xxs with missing-context).
//
// The route sits under the EXISTING "/api/v1/collections" prefix entry
// (jwt_auth.go), and choraSessionOnPathPrefix matches with strings.HasPrefix —
// so no new list entry is needed. This test PROVES that rather than assuming it,
// and fails loudly if a future edit narrows the prefix to an exact match.
// -----------------------------------------------------------------------------

func TestDefaultJWTGatedPrefixes_CoversConvertToStudyList(t *testing.T) {
	wantCovered := []string{
		"/api/v1/collections/col-1/convert-to-study-list",
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
			t.Errorf("path %q is NOT covered by DefaultJWTGatedPrefixes — JWT gate would skip, "+
				"mesh claims would be empty, chora-creation would 4xx", p)
		}
	}
}
