// security_headers.go: gateway-wide security response headers.
//
// Three independent probes on 2026-08-07 found the same gap: an authenticated
// OWASP ZAP scan of api.chora.site, a manual curl of /healthz and a 401 path,
// and tests/security/headers/headers_test.go, which has asserted these headers
// since it was written against a gateway that never sent one of them. A
// repo-wide grep for X-Frame-Options returned exactly one file: that test.
//
// chora-contracts/openapi/gateway.yaml has always documented a middleware
// chain that sets them. The contract and the code disagreed and the code was
// wrong. This file is the code catching up.
//
// Why middleware and not load balancer config: the five SPA backend buckets
// already carry X-Frame-Options, X-Content-Type-Options and HSTS as
// customResponseHeaders at the CDN edge, so the browser surfaces are covered.
// The api backend service carries none. Putting the guarantee here means it
// travels with the service through any deploy lane, cluster or ingress change,
// rather than depending on a load balancer field nobody re-checks.
//
// The middleware is an ENFORCEMENT, not a default. It re-asserts the six
// headers when the status is written, so a proxied upstream response or a
// downstream handler cannot weaken them. Cache-Control is the deliberate
// exception: it is seeded as a no-store default before the handler runs and a
// handler with a considered cache policy (stripe_config_handler's
// private, max-age=60) keeps it.
package middleware

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strings"
)

// Security header names, one place so the pre-flight seed and the
// write-time re-assertion cannot drift.
const (
	HeaderCSP                = "Content-Security-Policy"
	HeaderHSTS               = "Strict-Transport-Security"
	HeaderFrameOptions       = "X-Frame-Options"
	HeaderContentTypeOptions = "X-Content-Type-Options"
	HeaderReferrerPolicy     = "Referrer-Policy"
	HeaderXSSProtection      = "X-XSS-Protection"
	HeaderCacheControl       = "Cache-Control"
	HeaderForwardedProto     = "X-Forwarded-Proto"
	headerServer             = "Server"
	headerPoweredBy          = "X-Powered-By"
)

// ContentSecurityPolicy is the policy for a surface that serves JSON and never
// a document.
//
// A CSP copied from a document-serving default would be wrong here in both
// directions: it would carry script-src / style-src / img-src allowances for
// resources this service never returns, and it would omit the lock-down that
// costs nothing on an API. Every directive below denies rather than permits.
//
//   - default-src 'none'    nothing may be loaded from a gateway response
//   - frame-ancestors 'none' the CSP-era equivalent of X-Frame-Options: DENY,
//     and the one modern browsers actually consult
//   - base-uri 'none'       no <base> can retarget a relative URL, should a
//     response ever be rendered as markup
//   - form-action 'none'    no form in such a response could post anywhere
//
// A CSP on an API response governs only that response treated as a document.
// It does not constrain the SPA's fetch calls: chora-web's own document CSP
// governs those. So this policy is free to be maximally restrictive.
//
// `sandbox` is deliberately NOT included. It would be the natural next step for
// a never-a-document surface, but the gateway proxies file downloads with
// Content-Disposition preserved (CHO-1883, gatewayproxy.go), and a sandbox
// without allow-downloads blocks them in the browser. The four directives above
// carry the protection without that cost.
const ContentSecurityPolicy = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// StrictTransportSecurity pins TLS for a year across every chora.site
// subdomain. This matches what the five SPA buckets already send at the CDN
// edge, so a browser sees one consistent policy for the whole platform.
//
// `preload` is deliberately omitted: submission to the browser preload list is
// effectively irreversible for the apex domain and is an owner decision, not a
// middleware default.
const StrictTransportSecurity = "max-age=31536000; includeSubDomains"

// Fixed values for the remaining headers.
const (
	// FrameOptionsDeny: no framing of an API response, ever.
	FrameOptionsDeny = "DENY"
	// ContentTypeOptionsNoSniff: a JSON body must never be sniffed into
	// something executable.
	ContentTypeOptionsNoSniff = "nosniff"
	// ReferrerPolicyStrictOrigin: send the origin cross-site, never the path.
	// Gateway paths carry atom, tenant and GCID identifiers.
	ReferrerPolicyStrictOrigin = "strict-origin-when-cross-origin"
	// XSSProtectionDisabled is 0, not 1. The legacy XSS auditor is removed from
	// current browsers and its filter was itself exploitable; 0 explicitly
	// disables it rather than asking for a broken mitigation.
	XSSProtectionDisabled = "0"
	// CacheControlNoStore is the default for a surface whose responses are
	// tenant-scoped and authenticated.
	CacheControlNoStore = "no-store"
)

// SecurityHeaders wraps next so every response carries the platform security
// headers, whatever status the downstream writes and whether or not it wrote
// one explicitly.
//
// Apply it in cmd/server/main.go outside the CORS middleware, so a preflight
// short-circuit and an auth-gate 401 are both covered.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hsts := hstsApplies(r)

		// Seed before the handler runs. A handler that writes its status and
		// returns still gets the headers, and a handler that inspects
		// w.Header() sees the real policy.
		applySecurityHeaders(w.Header(), hsts)

		// Cache-Control is a seeded default, not an enforced invariant: a
		// handler with a considered policy overrides it below.
		if w.Header().Get(HeaderCacheControl) == "" {
			w.Header().Set(HeaderCacheControl, CacheControlNoStore)
		}

		next.ServeHTTP(&securedWriter{ResponseWriter: w, hsts: hsts}, r)
	})
}

// applySecurityHeaders stamps the invariant set onto h. HSTS is conditional;
// the other five are unconditional.
func applySecurityHeaders(h http.Header, hsts bool) {
	h.Set(HeaderCSP, ContentSecurityPolicy)
	h.Set(HeaderFrameOptions, FrameOptionsDeny)
	h.Set(HeaderContentTypeOptions, ContentTypeOptionsNoSniff)
	h.Set(HeaderReferrerPolicy, ReferrerPolicyStrictOrigin)
	h.Set(HeaderXSSProtection, XSSProtectionDisabled)
	if hsts {
		h.Set(HeaderHSTS, StrictTransportSecurity)
	}
	// Never advertise the stack. Go's net/http sets no Server header of its
	// own, so these only ever fire on a value copied from a proxied upstream.
	h.Del(headerServer)
	h.Del(headerPoweredBy)
}

// hstsApplies reports whether this request arrived over TLS at the edge.
//
// The pod never terminates TLS, so r.TLS is nil for every in-cluster request
// and the X-Forwarded-Proto the GCLB stamps is the only signal available.
//
//   - explicit https, or a real TLS conn  -> emit
//   - explicit non-https                  -> suppress. RFC 6797 section 7.2
//     forbids sending HSTS over a non-secure transport, and an explicit value
//     is the proxy chain declaring one.
//   - loopback host                       -> suppress, so a developer running
//     the gateway locally does not have their browser pin localhost to https.
//   - absent                              -> emit. A deployed gateway is only
//     ever reachable by a browser through the TLS-terminating load balancer.
//     Suppressing on absence would mean a change in the proxy chain silently
//     drops the guarantee at the edge, which is the exact failure this
//     middleware exists to prevent.
func hstsApplies(r *http.Request) bool {
	if isLoopbackHost(r.Host) {
		return false
	}
	if proto := strings.TrimSpace(r.Header.Get(HeaderForwardedProto)); proto != "" {
		// A comma-separated chain lists the client-facing hop first.
		if i := strings.IndexByte(proto, ','); i >= 0 {
			proto = strings.TrimSpace(proto[:i])
		}
		return strings.EqualFold(proto, "https")
	}
	return true
}

// isLoopbackHost reports whether host addresses the local machine.
func isLoopbackHost(host string) bool {
	h := strings.TrimSpace(host)
	if h == "" {
		return false
	}
	if hostOnly, _, err := net.SplitHostPort(h); err == nil {
		h = hostOnly
	}
	h = strings.Trim(h, "[]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// securedWriter re-asserts the security headers at the moment the status is
// written, which is the last point at which a downstream handler or a copied
// upstream header could have weakened them.
//
// It forwards Flusher and Hijacker explicitly. The gateway's SSE paths do
// `flusher, _ := w.(http.Flusher)` (payments_stream.go, companionbridge.go) and
// silently stop flushing on a failed assertion, and the WebSocket upgrades do
// `hj, ok := w.(http.Hijacker)` (rplus_delivery_proxy_handler.go,
// social_handler.go). A wrapper that dropped either would break streaming with
// no error in any log.
type securedWriter struct {
	http.ResponseWriter
	hsts        bool
	wroteHeader bool
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *securedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *securedWriter) WriteHeader(status int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		applySecurityHeaders(w.Header(), w.hsts)
	}
	w.ResponseWriter.WriteHeader(status)
}

// Write covers the implicit 200 a handler triggers by writing a body without
// calling WriteHeader.
func (w *securedWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer so SSE keeps streaming.
func (w *securedWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying writer so the WebSocket upgrade works. It
// returns the real error when the underlying writer cannot hijack rather than
// fabricating a success.
func (w *securedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("chora-gateway: underlying ResponseWriter does not support Hijack")
	}
	return hj.Hijack()
}
