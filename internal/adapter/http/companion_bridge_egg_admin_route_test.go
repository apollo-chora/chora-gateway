// companion_bridge_egg_admin_route_test.go — D6 route binding for the H+
// pod-catalogue editor's read/save pair.
package httpadapter_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/companionbridge"
)

// The admin pair lives UNDER /api/v1/companion-eggs/, whose subtree handler
// reads the next segment as a SKU and only accepts a /odds suffix. If the two
// admin patterns did not out-rank it, every editor request would 404 while a
// perfectly good handler sat unreachable. This is the guard for that.
func TestEggAdminRoutes_AreNotSwallowedByTheSKUSubtree(t *testing.T) {
	var seen []string
	ten := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sku":"egg.standard.v1","display_name":"Standard","price_cents":999}`))
	}))
	t.Cleanup(ten.Close)

	h := httpadapter.NewCompanionBridgeMux(companionbridge.New(companionbridge.Config{
		TenancyURL: ten.URL, PerCallTimeout: time.Second,
	}))

	get := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/api/v1/companion-eggs/admin/catalog/egg.standard.v1", nil)
	getW := httptest.NewRecorder()
	h.ServeHTTP(getW, get)
	if getW.Code != http.StatusOK {
		t.Errorf("admin read: got %d, want 200 (the SKU subtree swallowed it?) body=%s", getW.Code, getW.Body.String())
	}

	post := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/v1/companion-eggs/admin/catalog", strings.NewReader(`{"sku":"egg.standard.v1"}`))
	post.Header.Set("Content-Type", "application/json")
	postW := httptest.NewRecorder()
	h.ServeHTTP(postW, post)
	if postW.Code != http.StatusOK {
		t.Errorf("admin save: got %d, want 200 body=%s", postW.Code, postW.Body.String())
	}

	want := []string{
		"GET /api/admin/familiar-eggs/catalog/egg.standard.v1",
		"POST /api/admin/familiar-eggs/catalog",
	}
	if len(seen) != 2 || seen[0] != want[0] || seen[1] != want[1] {
		t.Errorf("upstream calls = %v; want %v", seen, want)
	}
}

// The ADR-254 D9 alias prefix must reach the same handlers, or an SPA still on
// the pre-rename path silently loses the editor.
func TestEggAdminRoutes_ReachableViaTheFamiliarEggsAlias(t *testing.T) {
	var seen string
	ten := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sku":"egg.standard.v1"}`))
	}))
	t.Cleanup(ten.Close)

	h := httpadapter.NewCompanionBridgeMux(companionbridge.New(companionbridge.Config{
		TenancyURL: ten.URL, PerCallTimeout: time.Second,
	}))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/api/v1/familiar-eggs/admin/catalog/egg.standard.v1", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("alias read: got %d, want 200 body=%s", w.Code, w.Body.String())
	}
	if seen != "/api/admin/familiar-eggs/catalog/egg.standard.v1" {
		t.Errorf("alias upstream = %q", seen)
	}
}

// A write verb the pair does not implement must be refused, not proxied.
func TestEggAdminRoutes_RejectUnsupportedVerbs(t *testing.T) {
	ten := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("upstream must not be called for an unsupported verb")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ten.Close)
	h := httpadapter.NewCompanionBridgeMux(companionbridge.New(companionbridge.Config{
		TenancyURL: ten.URL, PerCallTimeout: time.Second,
	}))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete,
		"/api/v1/companion-eggs/admin/catalog", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("got %d, want 405", w.Code)
	}
}
