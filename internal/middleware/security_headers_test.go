// security_headers_test.go: RED-first spec for the gateway security-header
// middleware.
//
// The specification is tests/security/headers/headers_test.go (the live-edge
// suite an authenticated OWASP ZAP scan and a manual probe both agreed with).
// These unit tests restate the same assertions in-process so the guarantee is
// gated at build time rather than only after a deploy.
package middleware_test

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/middleware"
)

// okHandler is the trivial downstream: 200 with a JSON body.
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
}

// serve runs the middleware over handler for a GET of path and returns the
// recorded response.
func serve(t *testing.T, handler http.Handler, path string, reqHeaders map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range reqHeaders {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	middleware.SecurityHeaders(handler).ServeHTTP(w, r)
	return w
}

// --------------------------------------------------------------------------
// The six headers, present on every response
// --------------------------------------------------------------------------

func TestSecurityHeaders_PresentOnSuccess(t *testing.T) {
	w := serve(t, okHandler(), "/healthz", nil)

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
		"X-XSS-Protection":       "0",
	}
	for h, v := range want {
		if got := w.Header().Get(h); got != v {
			t.Errorf("header %s = %q, want %q", h, got, v)
		}
	}
	if csp := w.Header().Get("Content-Security-Policy"); csp == "" {
		t.Error("missing Content-Security-Policy")
	}
}

// A 401 is the response class the DAST scan flagged: the headers must be
// stamped before the auth gate writes its status, not after a happy path.
func TestSecurityHeaders_PresentOn401(t *testing.T) {
	unauthorized := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="chora", error="missing_bearer"`)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"missing_bearer"}`))
	})
	w := serve(t, unauthorized, "/api/me", nil)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	for _, h := range []string{
		"X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy",
		"X-XSS-Protection", "Content-Security-Policy",
	} {
		if w.Header().Get(h) == "" {
			t.Errorf("401 response missing %s", h)
		}
	}
	if got := w.Header().Get("WWW-Authenticate"); got == "" {
		t.Error("middleware must not disturb the downstream WWW-Authenticate header")
	}
}

func TestSecurityHeaders_PresentOn404(t *testing.T) {
	notFound := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"code":"GATEWAY_ROUTE_NOT_FOUND"}}`, http.StatusNotFound)
	})
	w := serve(t, notFound, "/api/v1/nonexistent", nil)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if w.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("404 response missing X-Frame-Options: DENY")
	}
}

// A downstream handler must not be able to weaken the guarantee. This is the
// difference between middleware that sets a default and middleware that
// enforces an invariant.
func TestSecurityHeaders_DownstreamCannotWeaken(t *testing.T) {
	hostile := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Frame-Options", "ALLOWALL")
		w.Header().Set("Content-Security-Policy", "default-src *")
		w.Header().Set("X-Content-Type-Options", "")
		w.WriteHeader(http.StatusOK)
	})
	w := serve(t, hostile, "/api/v1/atoms", nil)

	if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY (downstream override must not win)", got)
	}
	if got := w.Header().Get("Content-Security-Policy"); strings.Contains(got, "*") {
		t.Errorf("CSP = %q, want the gateway policy (downstream override must not win)", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

// --------------------------------------------------------------------------
// Content-Security-Policy: an API gateway serves JSON, never a document
// --------------------------------------------------------------------------

func TestSecurityHeaders_CSPIsRestrictiveForJSON(t *testing.T) {
	w := serve(t, okHandler(), "/healthz", nil)
	csp := w.Header().Get("Content-Security-Policy")

	for _, directive := range []string{
		"default-src 'none'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"form-action 'none'",
	} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP missing %q: %s", directive, csp)
		}
	}

	// A policy copied from a document-serving default would carry these. On a
	// JSON API every one of them is a widening with no caller to serve.
	for _, forbidden := range []string{
		"'unsafe-inline'", "'unsafe-eval'", "script-src", "style-src", "img-src", "*",
	} {
		if strings.Contains(csp, forbidden) {
			t.Errorf("CSP contains document-oriented token %q, which widens a JSON-only surface: %s", forbidden, csp)
		}
	}

	// sandbox is deliberately absent: the gateway proxies file downloads
	// (Content-Disposition, CHO-1883) and a sandbox without allow-downloads
	// would break them in the browser.
	if strings.Contains(csp, "sandbox") {
		t.Errorf("CSP must not carry sandbox, it breaks the Content-Disposition download path: %s", csp)
	}
}

// --------------------------------------------------------------------------
// HSTS
// --------------------------------------------------------------------------

func TestSecurityHeaders_HSTSOnForwardedHTTPS(t *testing.T) {
	w := serve(t, okHandler(), "/healthz", map[string]string{"X-Forwarded-Proto": "https"})

	hsts := w.Header().Get("Strict-Transport-Security")
	if hsts == "" {
		t.Fatal("missing Strict-Transport-Security behind X-Forwarded-Proto: https")
	}
	for _, want := range []string{"max-age=31536000", "includeSubDomains"} {
		if !strings.Contains(hsts, want) {
			t.Errorf("HSTS = %q, want containing %q", hsts, want)
		}
	}
}

// The pod never terminates TLS, so an absent X-Forwarded-Proto is the norm for
// any hop that is not the GCLB. Suppressing on absence would mean a change in
// the proxy chain silently drops the guarantee at the edge, which is the exact
// failure this middleware exists to prevent.
func TestSecurityHeaders_HSTSWhenForwardedProtoAbsent(t *testing.T) {
	w := serve(t, okHandler(), "/healthz", nil)
	if w.Header().Get("Strict-Transport-Security") == "" {
		t.Error("HSTS must be emitted when X-Forwarded-Proto is absent (deployed gateway is TLS-only at the edge)")
	}
}

// RFC 6797 section 7.2: an HSTS host must not send the header over a
// non-secure transport. An explicit http hop is the operator declaring one.
func TestSecurityHeaders_HSTSSuppressedOnDeclaredPlaintext(t *testing.T) {
	w := serve(t, okHandler(), "/healthz", map[string]string{"X-Forwarded-Proto": "http"})
	if got := w.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS = %q, want empty on a declared plaintext hop", got)
	}
}

// A developer running the gateway locally must not have their browser pin
// localhost to https.
func TestSecurityHeaders_HSTSSuppressedOnLoopback(t *testing.T) {
	for _, host := range []string{"localhost:8000", "127.0.0.1:8080", "[::1]:8080"} {
		t.Run(host, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			r.Host = host
			w := httptest.NewRecorder()
			middleware.SecurityHeaders(okHandler()).ServeHTTP(w, r)

			if got := w.Header().Get("Strict-Transport-Security"); got != "" {
				t.Errorf("HSTS = %q, want empty for loopback host %s", got, host)
			}
			// The rest of the guarantee still applies locally.
			if w.Header().Get("X-Frame-Options") != "DENY" {
				t.Error("loopback response must still carry X-Frame-Options")
			}
		})
	}
}

// --------------------------------------------------------------------------
// Cache-Control: a default, not an override
// --------------------------------------------------------------------------

func TestSecurityHeaders_CacheControlDefaultsToNoStore(t *testing.T) {
	silent := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	w := serve(t, silent, "/api/v1/atoms", nil)

	if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("Cache-Control = %q, want containing no-store", got)
	}
}

// A handler that has a considered cache policy keeps it. stripe_config_handler
// sets private, max-age=60 on purpose.
func TestSecurityHeaders_CacheControlDownstreamWins(t *testing.T) {
	cached := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "private, max-age=60")
		w.WriteHeader(http.StatusOK)
	})
	w := serve(t, cached, "/api/v1/stripe-config", nil)

	if got := w.Header().Get("Cache-Control"); got != "private, max-age=60" {
		t.Errorf("Cache-Control = %q, want the downstream policy private, max-age=60", got)
	}
}

// --------------------------------------------------------------------------
// No technology leak
// --------------------------------------------------------------------------

func TestSecurityHeaders_StripsTechnologyLeak(t *testing.T) {
	leaky := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Server", "Go-http-client/2.0")
		w.Header().Set("X-Powered-By", "Express")
		w.WriteHeader(http.StatusOK)
	})
	w := serve(t, leaky, "/api/v1/atoms", nil)

	if got := w.Header().Get("Server"); got != "" {
		t.Errorf("Server = %q, want stripped", got)
	}
	if got := w.Header().Get("X-Powered-By"); got != "" {
		t.Errorf("X-Powered-By = %q, want stripped", got)
	}
}

// --------------------------------------------------------------------------
// Streaming and upgrade paths must survive the ResponseWriter wrapper
// --------------------------------------------------------------------------

// payments_stream.go and companionbridge.go both do `flusher, _ := w.(http.Flusher)`
// and silently stop flushing when the assertion fails. A wrapper that drops
// Flusher would brick SSE with no error anywhere.
func TestSecurityHeaders_PreservesFlusher(t *testing.T) {
	var sawFlusher bool
	sse := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, sawFlusher = w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
	})
	_ = serve(t, sse, "/api/v1/checkout/stream", nil)

	if !sawFlusher {
		t.Error("wrapped ResponseWriter dropped http.Flusher, so SSE would silently stop flushing")
	}
}

// Satisfying the Flusher interface is not the same as forwarding the call. A
// wrapper whose Flush is a no-op passes the assertion test above and still
// stalls every SSE stream, so assert the flush actually reached the writer.
func TestSecurityHeaders_FlushReachesTheUnderlyingWriter(t *testing.T) {
	sse := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: one\n\n"))
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("no Flusher")
			return
		}
		f.Flush()
	})
	w := serve(t, sse, "/api/v1/checkout/stream", nil)

	if !w.Flushed {
		t.Error("Flush did not reach the underlying ResponseWriter, so SSE would buffer forever")
	}
}

// Unwrap must return the real writer, not the wrapper, or http.ResponseController
// loops or resolves to the wrong target.
func TestSecurityHeaders_UnwrapReturnsTheUnderlyingWriter(t *testing.T) {
	var inner http.ResponseWriter
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			t.Error("no Unwrap")
			return
		}
		inner = u.Unwrap()
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	middleware.SecurityHeaders(h).ServeHTTP(rec, r)

	if inner == nil {
		t.Fatal("Unwrap returned nil")
	}
	if inner != http.ResponseWriter(rec) {
		t.Error("Unwrap must return the original ResponseWriter passed to the middleware")
	}
}

// rplus_delivery_proxy_handler.go and social_handler.go do
// `hj, ok := w.(http.Hijacker); if !ok { ... }` for the WebSocket upgrade.
func TestSecurityHeaders_PreservesHijacker(t *testing.T) {
	var sawHijacker bool
	ws := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, sawHijacker = w.(http.Hijacker)
		w.WriteHeader(http.StatusOK)
	})
	_ = serve(t, ws, "/v1/campus/ws", nil)

	if !sawHijacker {
		t.Error("wrapped ResponseWriter dropped http.Hijacker, so the WebSocket upgrade would 500")
	}
}

// The wrapper must delegate Hijack to the real writer rather than answer it
// itself. httptest.ResponseRecorder is not a Hijacker, so a delegating wrapper
// reports the underlying error instead of pretending to succeed.
func TestSecurityHeaders_HijackDelegates(t *testing.T) {
	var hijackErr error
	var conn net.Conn
	var rw *bufio.ReadWriter
	ws := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("no Hijacker")
			return
		}
		conn, rw, hijackErr = hj.Hijack()
	})
	_ = serve(t, ws, "/v1/campus/ws", nil)

	if hijackErr == nil {
		t.Error("Hijack on a non-hijackable writer must return an error, not a fabricated success")
	}
	if conn != nil || rw != nil {
		t.Error("Hijack must not fabricate a connection")
	}
}

// http.ResponseController resolves Flusher/Hijacker through wrappers only when
// the wrapper exposes Unwrap. Go 1.20+ code in this repo may rely on it.
func TestSecurityHeaders_WrapperUnwraps(t *testing.T) {
	var ok bool
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, ok = w.(interface{ Unwrap() http.ResponseWriter })
		w.WriteHeader(http.StatusOK)
	})
	_ = serve(t, h, "/healthz", nil)

	if !ok {
		t.Error("wrapper must expose Unwrap() http.ResponseWriter for http.ResponseController")
	}
}

// A handler that never calls WriteHeader still gets the guarantee, because the
// implicit 200 on first Write must run the same path.
func TestSecurityHeaders_ImplicitWriteHeader(t *testing.T) {
	implicit := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	w := serve(t, implicit, "/healthz", nil)

	if w.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("implicit 200 response missing X-Frame-Options")
	}
}
