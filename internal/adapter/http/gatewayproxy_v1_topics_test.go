// gatewayproxy_v1_topics_test.go — CHO-2276 route-binding + guard specs
// (topic-tree Sub-phase B). Mirrors gatewayproxy_v1_atoms_test.go (CHO-2261).
//
// The /api/v1/topics prefix must be wired at the gateway so the A+/R+ topic-tree
// UI can reach chora-creation's /api/topics surface (Sub-phase A, CHO-2275).
// Wiring a bridge route needs FIVE things in sync:
//
//  1. inmem route registry     — authMiddleware 404s GATEWAY_ROUTE_NOT_FOUND
//     against this BEFORE the mux ever runs.
//  2. DefaultJWTGatedPrefixes   — RequireChoraSessionJWT stamps X-Tenant-Id +
//     gcid + the typed Roles (x-mesh-user-roles) the
//     downstream admin gate reads. Fail-closed.
//  3. GatewayProxyPathPrefixes  — the bridge's parallel doc list.
//  4. NewGatewayProxyMux         — serves the collection + item leaves.
//  5. matchesGatewayProxyPath    — WithGatewayProxy routes them to the bridge.
//
// Miss any one and the route is dead in production while every unit test that
// calls the handler directly still passes. TestV1TopicsRoute_RegisteredInEveryList
// is the guard that fails RED if a future refactor drops any of the five.
//
// Strict TDD: written BEFORE the handlers + list entries exist.
package httpadapter_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/adapter/inmem"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

const sampleTopicID = "019f6baf-0a31-708e-8923-954f39602102"

// -----------------------------------------------------------------------------
// Mux binding — reads translate /api/v1/topics[/{id}] → /api/topics[/{id}]
// -----------------------------------------------------------------------------

func TestGwProxy_V1TopicsCollection_GET_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod, gotQuery string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotQuery = r.URL.Path, r.Method, r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	h := newGwProxyMux(t, stub)

	w := doGwProxyReq(t, h, http.MethodGet, "/api/v1/topics?parent_id="+sampleTopicID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", w.Code, w.Body.String())
	}
	if gotPath != "/api/topics" {
		t.Errorf("downstream path = %q; want /api/topics (collection, v1 translated)", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("downstream method = %s; want GET", gotMethod)
	}
	if gotQuery != "parent_id="+sampleTopicID {
		t.Errorf("downstream query = %q; want parent_id forwarded verbatim", gotQuery)
	}
}

func TestGwProxy_V1TopicsItem_GET_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"` + sampleTopicID + `","name":"Cycling"}`))
	})
	h := newGwProxyMux(t, stub)

	w := doGwProxyReq(t, h, http.MethodGet, "/api/v1/topics/"+sampleTopicID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", w.Code, w.Body.String())
	}
	if gotPath != "/api/topics/"+sampleTopicID {
		t.Errorf("downstream path = %q; want /api/topics/{id} (v1 translated)", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("downstream method = %s; want GET", gotMethod)
	}
}

// -----------------------------------------------------------------------------
// Mux binding — writes forward method + body verbatim (translated).
// -----------------------------------------------------------------------------

func TestGwProxy_V1TopicsCollection_POST_ProxiesBodyVerbatim(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
		}
		gotBody = string(buf)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"new"}`))
	})
	h := newGwProxyMux(t, stub)

	w := doGwProxyReq(t, h, http.MethodPost, "/api/v1/topics", `{"name":"New topic"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201 (body=%s)", w.Code, w.Body.String())
	}
	if gotPath != "/api/topics" {
		t.Errorf("downstream path = %q; want /api/topics", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("downstream method = %s; want POST", gotMethod)
	}
	if gotBody != `{"name":"New topic"}` {
		t.Errorf("downstream body = %q; want the create body verbatim", gotBody)
	}
}

func TestGwProxy_V1TopicsItem_Move_POST_ProxiesToCreation(t *testing.T) {
	var gotPath, gotMethod string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"` + sampleTopicID + `"}`))
	})
	h := newGwProxyMux(t, stub)

	w := doGwProxyReq(t, h, http.MethodPost, "/api/v1/topics/"+sampleTopicID+"/move", `{"parent_id":null}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", w.Code, w.Body.String())
	}
	if gotPath != "/api/topics/"+sampleTopicID+"/move" {
		t.Errorf("downstream path = %q; want /api/topics/{id}/move", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("downstream method = %s; want POST", gotMethod)
	}
}

func TestGwProxy_V1TopicsItem_Atoms_POST_ProxiesToCreation(t *testing.T) {
	var gotPath string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"attached"}`))
	})
	h := newGwProxyMux(t, stub)

	w := doGwProxyReq(t, h, http.MethodPost, "/api/v1/topics/"+sampleTopicID+"/atoms", `{"atom_id":"a1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", w.Code, w.Body.String())
	}
	if gotPath != "/api/topics/"+sampleTopicID+"/atoms" {
		t.Errorf("downstream path = %q; want /api/topics/{id}/atoms", gotPath)
	}
}

func TestGwProxy_V1TopicsItem_PUT_and_DELETE_ProxiesToCreation(t *testing.T) {
	for _, m := range []string{http.MethodPut, http.MethodDelete} {
		var gotPath, gotMethod string
		stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
			gotPath, gotMethod = r.URL.Path, r.Method
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		})
		h := newGwProxyMux(t, stub)

		w := doGwProxyReq(t, h, m, "/api/v1/topics/"+sampleTopicID, `{"name":"x"}`)
		if gotPath != "/api/topics/"+sampleTopicID {
			t.Errorf("%s downstream path = %q; want /api/topics/{id}", m, gotPath)
		}
		if gotMethod != m {
			t.Errorf("downstream method = %s; want %s", gotMethod, m)
		}
		if w.Code != http.StatusOK {
			t.Errorf("%s status = %d; want 200", m, w.Code)
		}
	}
}

// -----------------------------------------------------------------------------
// Fail loud, not silent (AC): unsupported methods 405 at the gateway and must
// NOT reach the downstream; an unknown sub-resource / empty id is 404.
// -----------------------------------------------------------------------------

func TestGwProxy_V1Topics_BadMethods_405_NoDownstream(t *testing.T) {
	cases := []struct {
		method, path string
	}{
		{http.MethodDelete, "/api/v1/topics"},                          // collection: GET/POST only
		{http.MethodPut, "/api/v1/topics"},                             // collection: GET/POST only
		{http.MethodPost, "/api/v1/topics/" + sampleTopicID},           // item: GET/PUT/DELETE only
		{http.MethodGet, "/api/v1/topics/" + sampleTopicID + "/move"},  // move: POST only
		{http.MethodGet, "/api/v1/topics/" + sampleTopicID + "/atoms"}, // atoms: POST only
	}
	for _, c := range cases {
		called := false
		stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		})
		h := newGwProxyMux(t, stub)

		w := doGwProxyReq(t, h, c.method, c.path, "")
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d; want 405", c.method, c.path, w.Code)
		}
		if called {
			t.Errorf("%s %s reached the downstream — a 405 must be gateway-local", c.method, c.path)
		}
	}
}

func TestGwProxy_V1TopicsItem_EmptyID_and_UnknownSub_404(t *testing.T) {
	cases := []string{
		"/api/v1/topics/", // empty id
		"/api/v1/topics/" + sampleTopicID + "/bogus",       // unknown sub-resource
		"/api/v1/topics/" + sampleTopicID + "/atoms/extra", // too deep
	}
	for _, p := range cases {
		called := false
		stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		})
		h := newGwProxyMux(t, stub)

		w := doGwProxyReq(t, h, http.MethodGet, p, "")
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d; want 404", p, w.Code)
		}
		if called {
			t.Errorf("GET %s reached the downstream — a bad shape must 404 gateway-local", p)
		}
	}
}

// The operator-only backfill must NEVER be exposed at the public gateway (it
// bypasses the tenant-header gate; tenant is in the body). The bridge owns only
// /api/v1/topics — /api/internal/topics/* is not registered anywhere here.
func TestGwProxy_InternalTopicsBackfill_NotOwnedByBridge(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot) // sentinel: fell through to base (not bridge-owned)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	agg := gatewayproxy.New(gatewayproxy.Config{CreationURL: stub.URL, PerCallTimeout: time.Second})
	h := httpadapter.WithGatewayProxy(base, agg)

	r := httptest.NewRequest(http.MethodPost, "/api/internal/topics/backfill", strings.NewReader(`{"tenant_id":"x"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTeapot {
		t.Errorf("/api/internal/topics/backfill = %d; want 418 — the bridge must NOT own it", w.Code)
	}
}

// -----------------------------------------------------------------------------
// Ownership — WithGatewayProxy must claim /api/v1/topics[/{id}] for the bridge,
// NOT leak to the base (Phyllis) router.
// -----------------------------------------------------------------------------

func TestWithGatewayProxy_V1TopicsPaths_HandledByBridge(t *testing.T) {
	for _, p := range []string{"/api/v1/topics", "/api/v1/topics/" + sampleTopicID, "/api/v1/topics/" + sampleTopicID + "/move"} {
		base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		})
		stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
		})
		agg := gatewayproxy.New(gatewayproxy.Config{CreationURL: stub.URL, PerCallTimeout: time.Second})
		h := httpadapter.WithGatewayProxy(base, agg)

		method := http.MethodGet
		if strings.HasSuffix(p, "/move") {
			method = http.MethodPost
		}
		r := httptest.NewRequestWithContext(context.Background(), method, p, nil)
		r.Header.Set("Authorization", "Bearer t")
		r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)

		if w.Code == http.StatusTeapot {
			t.Errorf("%s leaked to base — the bridge must own /api/v1/topics", p)
		}
	}
}

// -----------------------------------------------------------------------------
// GUARD — the /api/v1/topics route must be present in EVERY required list.
// A future refactor that drops any one goes RED here.
// -----------------------------------------------------------------------------

func TestV1TopicsRoute_RegisteredInEveryList(t *testing.T) {
	const collection = "/api/v1/topics"
	item := "/api/v1/topics/" + sampleTopicID
	move := item + "/move"
	atoms := item + "/atoms"

	// (1) inmem route registry — authMiddleware Match() runs BEFORE the mux;
	// a miss here is GATEWAY_ROUTE_NOT_FOUND (the CHO-2261 signature).
	repo := inmem.NewRouteRepository()
	for _, p := range []string{collection, item, move, atoms} {
		matched, err := repo.Match(context.Background(), p)
		if err != nil {
			t.Errorf("route %q not in the inmem registry — authMiddleware 404s it "+
				"(GATEWAY_ROUTE_NOT_FOUND) before the mux. Add a BFFRoute in "+
				"internal/adapter/inmem/route_repository.go.", p)
			continue
		}
		if matched.BackendService != "chora-creation" {
			t.Errorf("registry BackendService for %q = %q; want chora-creation", p, matched.BackendService)
		}
	}

	// (2) DefaultJWTGatedPrefixes — must HasPrefix-cover all so RequireChoraSessionJWT
	// stamps X-Tenant-Id + gcid + the typed Roles (x-mesh-user-roles) the
	// downstream admin gate reads; fail-closed on an empty header.
	assertPrefixCovered(t, "DefaultJWTGatedPrefixes", httpadapter.DefaultJWTGatedPrefixes, collection, item, move, atoms)

	// (3) GatewayProxyPathPrefixes — the bridge's parallel doc list.
	assertPrefixCovered(t, "GatewayProxyPathPrefixes", httpadapter.GatewayProxyPathPrefixes, collection, item, move, atoms)

	// (4)+(5) mux + matchesGatewayProxyPath — exercised via WithGatewayProxy:
	// all paths must be bridge-owned (proven non-teapot in
	// TestWithGatewayProxy_V1TopicsPaths_HandledByBridge above).
}
