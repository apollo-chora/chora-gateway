// cors.go — gateway-wide CORS middleware for chora-gateway.
//
// A5 (FE's single biggest blocker — filed in commit f03173cb /
// docs/m13/handoff-fe-to-be-service-2026-05-14.md §A5): CORS was previously
// applied to /api/v1/auth/* ONLY (the mint handler's inline block). Every
// other /api/* route returned 200/401 via curl but with NO
// Access-Control-Allow-Origin header → the browser blocked every response as
// net::ERR_FAILED, blocking ALL browser-side real wiring for the FE.
//
// This middleware applies the SAME allowed-origins logic, methods, headers,
// and OPTIONS preflight short-circuit the mint handler used — gateway-wide,
// so every /api/* route is CORS-correct. The mint endpoint keeps its CORS
// working via this same shared middleware (its inline block was removed —
// no duplicate Access-Control-Allow-Origin headers).
//
// Trust note: CORS is BROWSER-enforced, not server-enforced. This middleware
// only declines to ECHO Access-Control-Allow-Origin for unknown origins —
// the request still reaches the inner handler (a server-to-server caller
// with no Origin header is unaffected). The /api/* trust boundary remains
// the ChoraSession JWT gate (WithChoraSessionOnPrefixes).
package httpadapter

import (
	"net/http"
	"os"
	"strings"
	"sync"
)

// EnvCORSAllowedOrigins is the env var that overrides the default chora-web
// origins allow-list. Format: comma-separated absolute origins (no path,
// no trailing slash). When unset OR empty the defaultCORSAllowedOrigins
// set below is used. Per ADR-164 Stage B + the CJ2-STRIPE-CORS FE ask
// (`docs/m13/e2e-fe-coord-directive-2026-05-16.md §3`).
const EnvCORSAllowedOrigins = "CHORA_CORS_ALLOWED_ORIGINS"

// defaultCORSAllowedOrigins is the canonical set of chora-web origins
// permitted to make cross-origin requests to the gateway: production
// chora.site + the four surface subdomains + localhost dev (ng serve
// 4200/4201 + Storybook 6006). Kept explicit (no wildcard) — CORS
// credentials mode forbids `*` when Access-Control-Allow-Credentials is
// true, and we may flip that on later when session cookies enter the
// picture.
//
// This set is the single source of truth — mint_handler.go's
// isAllowedMintOrigin previously duplicated it; that duplicate was removed
// when the mint handler moved to this shared middleware.
var defaultCORSAllowedOrigins = map[string]struct{}{
	"https://chora.site":       {},
	"https://cplus.chora.site": {},
	"https://hplus.chora.site": {},
	"https://oplus.chora.site": {},
	"https://rplus.chora.site": {},
	"http://localhost:4200":    {},
	"http://localhost:4201":    {},
	"http://localhost:6006":    {},
}

// corsOriginsOnce + corsOrigins memoise the effective set: env-CSV when set,
// otherwise defaultCORSAllowedOrigins. Per-test resets call
// ResetCORSAllowedOriginsForTest below.
var (
	corsOriginsOnce sync.Once
	corsOrigins     map[string]struct{}
)

// loadCORSAllowedOrigins returns the effective allow-list. Honours the
// EnvCORSAllowedOrigins env var (CSV) — when unset OR empty AFTER trimming
// the defaults apply. Each CSV entry is trimmed; empty entries are skipped.
// Memoised on first call; tests use ResetCORSAllowedOriginsForTest to flush.
func loadCORSAllowedOrigins() map[string]struct{} {
	corsOriginsOnce.Do(func() {
		raw := strings.TrimSpace(os.Getenv(EnvCORSAllowedOrigins))
		if raw == "" {
			corsOrigins = defaultCORSAllowedOrigins
			return
		}
		out := map[string]struct{}{}
		for _, entry := range strings.Split(raw, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			out[entry] = struct{}{}
		}
		if len(out) == 0 {
			// All entries blank — fall back to defaults.
			corsOrigins = defaultCORSAllowedOrigins
			return
		}
		corsOrigins = out
	})
	return corsOrigins
}

// ResetCORSAllowedOriginsForTest is a test-only helper that flushes the
// memoised allow-list. The next call to loadCORSAllowedOrigins re-reads the
// env. Exported because tests live in package httpadapter_test (no internal
// access).
func ResetCORSAllowedOriginsForTest() {
	corsOriginsOnce = sync.Once{}
	corsOrigins = nil
}

// CORS response-header constants — shared by the preflight + actual-request
// paths so the two never drift.
const (
	corsAllowMethods = "GET, POST, PUT, PATCH, DELETE, OPTIONS"
	corsAllowHeaders = "Content-Type, Authorization, traceparent, X-Tenant-Id, Idempotency-Key, X-Correlation-ID"
	corsMaxAge       = "3600"
	// corsExposeHeaders lists the response headers a browser may READ. Without
	// this the SPA's fetch cannot see X-Correlation-ID at all: the CORS default
	// exposes only the seven safelisted response headers, so the id the gateway
	// stamps would be invisible to the very client that has to quote it in a
	// support ticket. X-Correlation-ID is also in corsAllowHeaders above so the
	// SPA can SEND one and have it propagated (2026-08-07 header work).
	corsExposeHeaders = "X-Correlation-ID, traceparent"
)

// isAllowedOrigin reports whether origin is a known chora-web origin. An
// empty origin (no Origin header — server-to-server / curl) returns false:
// no CORS headers are stamped, which is the correct no-op for non-browser
// callers. Allow-list is sourced from CHORA_CORS_ALLOWED_ORIGINS (CSV) when
// set, otherwise defaultCORSAllowedOrigins.
func isAllowedOrigin(origin string) bool {
	if origin == "" {
		return false
	}
	_, ok := loadCORSAllowedOrigins()[origin]
	return ok
}

// CORSMiddleware wraps `next` with gateway-wide CORS handling:
//
//   - For an allowed Origin it stamps Access-Control-Allow-Origin (echoing
//     the origin, never `*`), Vary: Origin, -Allow-Methods, -Allow-Headers,
//     and -Max-Age.
//   - An OPTIONS request (CORS preflight) is short-circuited with 204 No
//     Content — the inner handler never runs. This holds even for an
//     unknown origin (204 with no echo; the browser then rejects).
//   - A non-OPTIONS request always proceeds to `next` — CORS is
//     browser-enforced; the server only declines to echo for unknown
//     origins.
//
// Apply as the OUTERMOST wrapper in cmd/server/main.go so every route
// (/api/*, /bff/*, /graphql, health) is CORS-correct and OPTIONS preflights
// are answered before any auth gate or downstream handler runs.
func CORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if isAllowedOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", corsAllowMethods)
			w.Header().Set("Access-Control-Allow-Headers", corsAllowHeaders)
			w.Header().Set("Access-Control-Expose-Headers", corsExposeHeaders)
			w.Header().Set("Access-Control-Max-Age", corsMaxAge)
		}
		if r.Method == http.MethodOptions {
			// CORS preflight — short-circuit with 204 No Content regardless of
			// whether the origin was allowed (an unknown origin gets 204 with
			// no echo, which the browser then rejects — the safe default).
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
