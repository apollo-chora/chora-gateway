// oplus_mint_wrappers_test.go — coverage for the composition wrappers'
// nil-handler passthrough branches: WithOPlusRoutes / WithMintRoute return
// `base` untouched when the handler is nil (the unconfigured-env posture),
// and route correctly when wired.
package httpadapter_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
)

func teapotBase(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
}

func TestWithOPlusRoutes_NilHandlerPassesThrough(t *testing.T) {
	base := teapotBase(t)
	h := httpadapter.WithOPlusRoutes(base, nil)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/governance", nil)
	h.ServeHTTP(w, req)
	if w.Code != http.StatusTeapot {
		t.Errorf("nil handler → %d; want 418 (base passthrough)", w.Code)
	}
}

func TestWithMintRoute_NilHandlerPassesThrough(t *testing.T) {
	base := teapotBase(t)
	h := httpadapter.WithMintRoute(base, nil)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/session/mint", nil)
	h.ServeHTTP(w, req)
	if w.Code != http.StatusTeapot {
		t.Errorf("nil mint → %d; want 418 (base passthrough)", w.Code)
	}

	// A non-mint path also passes through.
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusTeapot {
		t.Errorf("healthz → %d; want 418", w2.Code)
	}
}

// TestWithOPlusRoutes_WiredDoesNotShadowBase — with a handler wired, the
// /bff/oplus/ prefix is owned while everything else falls through.
func TestWithOPlusRoutes_WiredDoesNotShadowBase(t *testing.T) {
	base := teapotBase(t)
	h := httpadapter.WithOPlusRoutes(base, newOPlusHandlerForTest(t))

	// Non-oplus path → base.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/not-oplus", nil)
	h.ServeHTTP(w, req)
	if w.Code != http.StatusTeapot {
		t.Errorf("non-oplus → %d; want 418", w.Code)
	}

	// Oplus prefix is owned by the mux (its own 404 handling on unknown leaf).
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/bff/oplus/governance", nil)
	h.ServeHTTP(w2, req2)
	if w2.Code == http.StatusTeapot {
		t.Errorf("oplus-owned path must not fall through to base; got 418")
	}
	_ = io.Discard
}
