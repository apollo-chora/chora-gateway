// cors_test.go — RED-phase TDD specs for the gateway-wide CORS middleware
// (A5 — FE's single biggest blocker, filed in commit f03173cb /
// docs/m13/handoff-fe-to-be-service-2026-05-14.md §A5).
//
// Confirmed cause: CORS was applied to /api/v1/auth/* ONLY (the mint
// handler's inline block). Every other /api/* route returned 200/401 via
// curl but with NO Access-Control-Allow-Origin header → the browser blocked
// every response as net::ERR_FAILED. This blocks ALL browser-side real
// wiring for the FE.
//
// The mint endpoint's CORS logic is extracted into a shared
// CORSMiddleware applied to all /api/* routes — same allowed-origins set,
// methods, headers, OPTIONS preflight short-circuit.
//
// Strict TDD: tests written BEFORE the CORS middleware implementation.
package httpadapter_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
)

// echoOK is a trivial downstream handler that 200s — lets the CORS tests
// assert the middleware stamps headers regardless of what the inner mux does.
func echoOK() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
}

// -----------------------------------------------------------------------------
// A non-preflight GET on any /api/* route from an allowed origin gets the
// Access-Control-Allow-Origin echo.
// -----------------------------------------------------------------------------

func TestCORSMiddleware_StampsACAOOnAllAPIRoutes(t *testing.T) {
	t.Parallel()
	wrapped := httpadapter.CORSMiddleware(echoOK())

	// Routes that were 404'ing CORS prior — every /api/* route must now echo.
	paths := []string{
		"/api/catalog?public=true",
		"/api/feature-flags",
		"/api/v1/me/knowledge-graph/clusters",
		"/api/v1/notifications",
		"/api/companion/daily-dose",
		"/api/v1/consumption/kg/explore/atom-1",
	}
	for _, p := range paths {
		p := p
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, p, nil)
			r.Header.Set("Origin", "https://chora.site")
			w := httptest.NewRecorder()
			wrapped.ServeHTTP(w, r)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (CORS must not change the response code)", w.Code)
			}
			if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://chora.site" {
				t.Errorf("Access-Control-Allow-Origin = %q, want https://chora.site", got)
			}
			if got := w.Header().Get("Vary"); !strings.Contains(got, "Origin") {
				t.Errorf("Vary = %q, want it to contain Origin", got)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// OPTIONS preflight on any /api/* route returns 204 + the CORS headers,
// short-circuiting before the inner handler runs.
// -----------------------------------------------------------------------------

func TestCORSMiddleware_OptionsPreflight_204(t *testing.T) {
	t.Parallel()
	innerCalled := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		innerCalled = true
		w.WriteHeader(http.StatusOK)
	})
	wrapped := httpadapter.CORSMiddleware(inner)

	r := httptest.NewRequest(http.MethodOptions, "/api/catalog", nil)
	r.Header.Set("Origin", "https://chora.site")
	r.Header.Set("Access-Control-Request-Method", "GET")
	r.Header.Set("Access-Control-Request-Headers", "content-type,authorization")
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", w.Code)
	}
	if innerCalled {
		t.Error("inner handler MUST NOT run for an OPTIONS preflight")
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://chora.site" {
		t.Errorf("Access-Control-Allow-Origin = %q, want https://chora.site", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("Access-Control-Allow-Methods missing on preflight")
	}
	acah := w.Header().Get("Access-Control-Allow-Headers")
	if !strings.Contains(acah, "Content-Type") {
		t.Errorf("Access-Control-Allow-Headers = %q, want it to contain Content-Type", acah)
	}
	if !strings.Contains(acah, "Authorization") {
		t.Errorf("Access-Control-Allow-Headers = %q, want it to contain Authorization", acah)
	}
	// EPIC-1a: the batch/question-jobs + mana-topup FE calls send a custom
	// Idempotency-Key header; without it in the preflight allow-list the
	// browser blocks the POST (net::ERR_FAILED). Regression guard.
	if !strings.Contains(acah, "Idempotency-Key") {
		t.Errorf("Access-Control-Allow-Headers = %q, want it to contain Idempotency-Key", acah)
	}
	if got := w.Header().Get("Access-Control-Max-Age"); got == "" {
		t.Error("Access-Control-Max-Age missing on preflight")
	}
}

// -----------------------------------------------------------------------------
// A disallowed origin gets NO Access-Control-Allow-Origin echo (safe default —
// the browser then rejects the response). The request itself still proceeds.
// -----------------------------------------------------------------------------

func TestCORSMiddleware_DisallowedOrigin_NoEcho(t *testing.T) {
	t.Parallel()
	wrapped := httpadapter.CORSMiddleware(echoOK())

	r := httptest.NewRequest(http.MethodGet, "/api/catalog", nil)
	r.Header.Set("Origin", "https://attacker.example.com")
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, r)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty for an unknown origin", got)
	}
	// The request still reaches the inner handler (CORS is browser-enforced,
	// not server-enforced — the server just declines to echo).
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — CORS must not block server-side", w.Code)
	}
}

// -----------------------------------------------------------------------------
// An OPTIONS preflight from a disallowed origin still returns 204 but with NO
// CORS echo (the browser then rejects). The inner handler must not run.
// -----------------------------------------------------------------------------

func TestCORSMiddleware_DisallowedOriginPreflight_204NoEcho(t *testing.T) {
	t.Parallel()
	innerCalled := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		innerCalled = true
	})
	wrapped := httpadapter.CORSMiddleware(inner)

	r := httptest.NewRequest(http.MethodOptions, "/api/catalog", nil)
	r.Header.Set("Origin", "https://attacker.example.com")
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, r)

	if w.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", w.Code)
	}
	if innerCalled {
		t.Error("inner handler MUST NOT run for an OPTIONS preflight, even from a bad origin")
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty for an unknown origin", got)
	}
}

// -----------------------------------------------------------------------------
// A request with no Origin header (server-to-server, curl without -H Origin)
// passes through untouched — no CORS headers, normal response.
// -----------------------------------------------------------------------------

func TestCORSMiddleware_NoOriginHeader_Passthrough(t *testing.T) {
	t.Parallel()
	wrapped := httpadapter.CORSMiddleware(echoOK())

	r := httptest.NewRequest(http.MethodGet, "/api/catalog", nil)
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty when no Origin header", got)
	}
}

// -----------------------------------------------------------------------------
// CHORA_CORS_ALLOWED_ORIGINS (Stage B of ADR-164) overrides the default
// allow-list with a CSV. Tests run sequentially (no t.Parallel) so the
// per-process memoised allow-list is replaced once per test.
// -----------------------------------------------------------------------------

func TestCORSMiddleware_EnvOverride_AllowsConfiguredOrigin(t *testing.T) {
	t.Setenv("CHORA_CORS_ALLOWED_ORIGINS",
		"https://stage.chora.site,https://preview.chora.site")
	httpadapter.ResetCORSAllowedOriginsForTest()
	t.Cleanup(httpadapter.ResetCORSAllowedOriginsForTest)

	wrapped := httpadapter.CORSMiddleware(echoOK())

	r := httptest.NewRequest(http.MethodGet, "/api/v1/checkout/course", nil)
	r.Header.Set("Origin", "https://stage.chora.site")
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, r)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://stage.chora.site" {
		t.Errorf("ACAO = %q, want https://stage.chora.site", got)
	}
}

func TestCORSMiddleware_EnvOverride_RejectsDefaultOriginOutsideCSV(t *testing.T) {
	t.Setenv("CHORA_CORS_ALLOWED_ORIGINS", "https://only-this.example")
	httpadapter.ResetCORSAllowedOriginsForTest()
	t.Cleanup(httpadapter.ResetCORSAllowedOriginsForTest)

	wrapped := httpadapter.CORSMiddleware(echoOK())

	r := httptest.NewRequest(http.MethodGet, "/api/v1/checkout/course", nil)
	r.Header.Set("Origin", "https://chora.site")
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, r)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO = %q, want empty when env CSV excludes the default origin", got)
	}
}

func TestCORSMiddleware_EnvOverride_EmptyFallsBackToDefaults(t *testing.T) {
	t.Setenv("CHORA_CORS_ALLOWED_ORIGINS", "  ,  ,  ")
	httpadapter.ResetCORSAllowedOriginsForTest()
	t.Cleanup(httpadapter.ResetCORSAllowedOriginsForTest)

	wrapped := httpadapter.CORSMiddleware(echoOK())

	r := httptest.NewRequest(http.MethodGet, "/api/v1/checkout/course", nil)
	r.Header.Set("Origin", "https://chora.site")
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, r)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://chora.site" {
		t.Errorf("ACAO = %q, want default origin when env CSV is blank", got)
	}
}

// -----------------------------------------------------------------------------
// The localhost dev origins + surface subdomains are all allowed (parity with
// the original isAllowedMintOrigin set the mint handler used).
// -----------------------------------------------------------------------------

func TestCORSMiddleware_AllowedOriginSet(t *testing.T) {
	t.Parallel()
	wrapped := httpadapter.CORSMiddleware(echoOK())

	allowed := []string{
		"https://chora.site",
		"https://cplus.chora.site",
		"https://hplus.chora.site",
		"https://oplus.chora.site",
		"https://rplus.chora.site",
		"http://localhost:4200",
		"http://localhost:4201",
		"http://localhost:6006",
	}
	for _, origin := range allowed {
		origin := origin
		t.Run(origin, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/api/catalog", nil)
			r.Header.Set("Origin", origin)
			w := httptest.NewRecorder()
			wrapped.ServeHTTP(w, r)
			if got := w.Header().Get("Access-Control-Allow-Origin"); got != origin {
				t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, origin)
			}
		})
	}
}
