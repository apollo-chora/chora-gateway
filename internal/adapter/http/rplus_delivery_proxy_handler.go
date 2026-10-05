// rplus_delivery_proxy_handler.go — HTTP route bindings for the R+ (Rhythm+)
// surface BFF proxy routes. Composes with the existing WithGatewayProxy
// chain (add-only — never touches gatewayproxy_handler.go) so parallel
// sessions editing the legacy bridge don't collide with R+ work.
//
// Closes the BFF coverage gap for R+ wave-1/2/3 services that have been
// holding hardcoded in-memory fixtures (MTM / DSA-101 / CSPO / Mr. Chen)
// instead of real BFF wiring per feedback_no_stubs_real_wiring + the
// locked R+ build-out plan.
//
// Resource groups owned (each has a real chora-delivery HTTP handler —
// pure verbatim passthrough, no path rewrite):
//
//	/api/bookings[/{...}]            → chora-delivery legacy bookings
//	/api/certifications[/{...}]      → chora-delivery legacy certifications
//	/v1/campus[/{...}]               → chora-delivery legacy campusops
//	/v1/me/applications[/{...}]      → chora-delivery applications (ADR-164
//	                                   Stage C — chora-payments owns the
//	                                   Stripe accept-offer mint via gRPC,
//	                                   REST surface stays in chora-delivery)
//
// Resource groups deliberately NOT registered here (chora-delivery has no
// HTTP handler — Stage C of the R+ plan will build them, then the prefix
// list below grows): rosters / exams / skillsfutures-claims / project-groups /
// wbl-placements / surveys / live-quiz / live-poll / classroom.
//
// Auth: every prefix below is JWT-gated upstream by RequireChoraSessionJWT
// (the prefixes are added to DefaultJWTGatedPrefixes in jwt_auth.go). The
// downstream chora-delivery handlers all use tenantRequired middleware that
// reads X-Tenant-Id from the validated mesh claims the gateway stamps via
// the canonical call() helper.

package httpadapter

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

// RplusDeliveryProxyPathPrefixes lists the BFF path prefixes the R+ delivery
// proxy bridge serves. Exposed for inclusion in DefaultJWTGatedPrefixes.
//
// /v1/me/applications is technically already covered by the existing /v1/me/
// JWT prefix entry — the explicit per-resource entry here keeps the
// audit trail readable + survives any future re-narrowing of /v1/me/.
var RplusDeliveryProxyPathPrefixes = []string{
	"/api/bookings",
	"/api/certifications",
	"/v1/campus",
	"/v1/me/applications",
	// R+ Stage C-lite (2026-05-26) — M6 scheduling + M12 certifications
	// list surfaces. Verbatim passthrough to chora-delivery's new R+
	// handlers (scheduling_handler.go + certifications_list_handler.go).
	"/v1/scheduling",
	"/api/v1/certifications",
	// R+ Stage C-lite wave-2 (2026-05-26) — M7 exams + M13 wbl + admin
	// applications. Verbatim passthrough to chora-delivery handlers
	// (exam_handler.go / wbl_handler.go / applications_admin_handler.go).
	"/api/v1/exams",
	"/api/v1/wbl-placements",
	"/api/v1/applications",
	// R+ four-mode W1 (ADR-190, CHO-1849) — Offering aggregate (delivery_type).
	"/api/v1/offerings",
	// R+ four-mode W2.A (ADR-190, CHO-1850) — universal-finder offering search.
	// Scoped to the offerings entity (NOT the whole /api/v1/search namespace —
	// other entities like /api/v1/search/atoms route to their own domains).
	"/api/v1/search/offerings",
	// R+ four-mode SHORT (ADR-237, CHO-2191) — durable Room catalogue backing
	// the Schedule & Rooms room picker + room_id double-book gate. The delivery
	// handler (POST/GET /api/v1/rooms) + FE picker shipped but this gateway
	// route was omitted, so the browser got GATEWAY_ROUTE_NOT_FOUND and the
	// picker rendered "Couldn't load rooms" — the gate was inert in the UI.
	"/api/v1/rooms",
	// R+ Stage C-lite wave-4 (2026-05-26) — M15b project-groups +
	// M15c SSG skillsfutures-claims + M4 course-centric rosters.
	"/api/v1/project-groups",
	"/api/v1/skillsfutures-claims",
	"/api/v1/rosters",
	// R+ Stage C-lite wave-6 (2026-05-26) — M9 live-quizzes CRUD,
	// M8 classroom-sessions snapshot, λ surveys CRUD, M11 live-polls WS.
	// NOTE: live-quizzes + live-polls subtrees include WebSocket upgrade
	// endpoints — the proxy MUST preserve `Upgrade: websocket` +
	// `Connection: Upgrade` headers and MUST NOT enforce HTTP/2 for the
	// 101 Switching Protocols handshake (μ manifest, ADR-166 §D5).
	"/api/v1/live-quizzes",
	"/api/v1/classroom-sessions",
	"/api/v1/surveys",
	"/api/v1/live-polls",
	// R+ Phase-2 W7 (CHO-2074) — A+ learner-scoped, course-only module-completion
	// read. A+ has NO offeringId, so this course-keyed learner endpoint
	// (GET /api/v1/me/module-progress?course_id=) resolves the learner's own
	// per-module progress by (course, gcid); distinct from the offering-nested
	// instructor cohort read under /api/v1/offerings.
	"/api/v1/me/module-progress",
}

// RplusDeliveryProxyHandler binds ProxyDeliveryVerbatim to the BFF mux for
// the R+ surface resource groups.
type RplusDeliveryProxyHandler struct {
	agg *gatewayproxy.Aggregator
}

// NewRplusDeliveryProxyHandler constructs the handler.
func NewRplusDeliveryProxyHandler(agg *gatewayproxy.Aggregator) *RplusDeliveryProxyHandler {
	return &RplusDeliveryProxyHandler{agg: agg}
}

// NewRplusDeliveryProxyMux returns a mux that serves the R+ delivery proxy
// routes. Combine with WithRplusDeliveryProxy to compose with a base handler
// that owns non-bridge paths.
//
// Each prefix is registered twice — exact + trailing-slash subtree — so
// net/http's most-specific-match wins for both bare collection-level GETs
// and parametric sub-resource paths.
func NewRplusDeliveryProxyMux(agg *gatewayproxy.Aggregator) http.Handler {
	h := NewRplusDeliveryProxyHandler(agg)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/bookings", h.handle)
	mux.HandleFunc("/api/bookings/", h.handle)
	mux.HandleFunc("/api/certifications", h.handle)
	mux.HandleFunc("/api/certifications/", h.handle)
	mux.HandleFunc("/v1/campus", h.handle)
	mux.HandleFunc("/v1/campus/", h.handle)
	mux.HandleFunc("/v1/me/applications", h.handle)
	mux.HandleFunc("/v1/me/applications/", h.handle)
	// R+ Stage C-lite (2026-05-26): M6 + M12 list surfaces — exact + subtree.
	mux.HandleFunc("/v1/scheduling", h.handle)
	mux.HandleFunc("/v1/scheduling/", h.handle)
	mux.HandleFunc("/api/v1/certifications", h.handle)
	mux.HandleFunc("/api/v1/certifications/", h.handle)
	// Wave-2: M7 exams + M13 wbl + admin applications — exact + subtree.
	mux.HandleFunc("/api/v1/exams", h.handle)
	mux.HandleFunc("/api/v1/exams/", h.handle)
	// R+ four-mode W1 (ADR-190, CHO-1849) — offerings exact + subtree.
	mux.HandleFunc("/api/v1/offerings", h.handle)
	mux.HandleFunc("/api/v1/offerings/", h.handle)
	// R+ four-mode W2.A (ADR-190, CHO-1850) — offering finder search (exact;
	// no subtree — /api/v1/search/atoms etc. belong to other domains).
	mux.HandleFunc("/api/v1/search/offerings", h.handle)
	// R+ four-mode SHORT (ADR-237, CHO-2191) — rooms exact + subtree.
	mux.HandleFunc("/api/v1/rooms", h.handle)
	mux.HandleFunc("/api/v1/rooms/", h.handle)
	mux.HandleFunc("/api/v1/wbl-placements", h.handle)
	mux.HandleFunc("/api/v1/wbl-placements/", h.handle)
	mux.HandleFunc("/api/v1/applications", h.handle)
	mux.HandleFunc("/api/v1/applications/", h.handle)
	// Wave-4: M15b project-groups + M15c skillsfutures-claims + M4 rosters.
	mux.HandleFunc("/api/v1/project-groups", h.handle)
	mux.HandleFunc("/api/v1/project-groups/", h.handle)
	mux.HandleFunc("/api/v1/skillsfutures-claims", h.handle)
	mux.HandleFunc("/api/v1/skillsfutures-claims/", h.handle)
	mux.HandleFunc("/api/v1/rosters", h.handle)
	mux.HandleFunc("/api/v1/rosters/", h.handle)
	// Wave-6: M9 live-quizzes + M8 classroom-sessions + λ surveys +
	// M11 live-polls (WS endpoints in live-quizzes + live-polls subtrees).
	mux.HandleFunc("/api/v1/live-quizzes", h.handle)
	mux.HandleFunc("/api/v1/live-quizzes/", h.handle)
	mux.HandleFunc("/api/v1/classroom-sessions", h.handle)
	mux.HandleFunc("/api/v1/classroom-sessions/", h.handle)
	mux.HandleFunc("/api/v1/surveys", h.handle)
	mux.HandleFunc("/api/v1/surveys/", h.handle)
	mux.HandleFunc("/api/v1/live-polls/", h.handle)
	// R+ Phase-2 W7 (CHO-2074) — A+ learner course-only module-progress read.
	mux.HandleFunc("/api/v1/me/module-progress", h.handle)
	mux.HandleFunc("/api/v1/me/module-progress/", h.handle)
	return mux
}

// WithRplusDeliveryProxy composes the R+ delivery proxy mux with a base
// handler: routes the R+ bridge owns are served by the bridge; everything
// else falls through to `base`. Passes through when `agg` is nil so
// cmd/server/main.go can opt out in unconfigured envs.
func WithRplusDeliveryProxy(base http.Handler, agg *gatewayproxy.Aggregator) http.Handler {
	if agg == nil {
		return base
	}
	bridgeMux := NewRplusDeliveryProxyMux(agg)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if matchesRplusDeliveryProxyPath(r.URL.Path) {
			bridgeMux.ServeHTTP(w, r)
			return
		}
		base.ServeHTTP(w, r)
	})
}

// matchesRplusDeliveryProxyPath reports whether the R+ delivery proxy bridge
// owns the path. Exact leaf matches OR subtree membership (prefix + "/").
func matchesRplusDeliveryProxyPath(p string) bool {
	for _, prefix := range RplusDeliveryProxyPathPrefixes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// handle is the single dispatch function for every R+ delivery proxy route.
// All registered prefixes use verbatim passthrough — method + path + body +
// Content-Type forwarded byte-for-byte to chora-delivery, which serves at
// the same path the FE hits. Mesh-trust headers stamped by the call helper.
func (h *RplusDeliveryProxyHandler) handle(w http.ResponseWriter, r *http.Request) {
	// ADR-168 classroom realtime / ADR-166 §D5 — the live-quizzes + live-polls
	// /ws subtrees carry WebSocket upgrade requests. A verbatim HTTP proxy
	// cannot carry the 101 Switching Protocols handshake, so hijack the client
	// conn + dial chora-delivery directly. REST (non-upgrade) requests keep the
	// existing verbatim path unchanged.
	if isWebSocketUpgrade(r) {
		h.proxyWebSocket(w, r)
		return
	}
	body := readBody(r)
	resp, _ := h.agg.ProxyDeliveryVerbatim(
		r.Context(), gatewayProxyAuthFromRequest(r),
		r.Method, r.URL.Path, r.URL.RawQuery, body, r.Header.Get("Content-Type"),
	)
	writeGatewayProxyResp(w, resp)
}

// IsWebSocketUpgradeForTest exposes isWebSocketUpgrade to the external test
// package (mirrors the InjectMeshClaimsForTest convention) so the upgrade-
// detection truth table can be exercised without a live conn.
func IsWebSocketUpgradeForTest(r *http.Request) bool { return isWebSocketUpgrade(r) }

// isWebSocketUpgrade reports whether the request is a WebSocket upgrade
// handshake. Per RFC 6455 §4.1 the `Connection` header is a token list that
// MUST include `Upgrade` and the `Upgrade` header MUST include `websocket` —
// both matched case-insensitively (HTTP header tokens are case-insensitive,
// and intermediaries may re-case them).
func isWebSocketUpgrade(r *http.Request) bool {
	if !headerTokenContains(r.Header.Get("Connection"), "upgrade") {
		return false
	}
	return headerTokenContains(r.Header.Get("Upgrade"), "websocket")
}

// headerTokenContains reports whether the comma-separated header value carries
// the wanted token (case-insensitive, whitespace-trimmed).
func headerTokenContains(headerValue, want string) bool {
	for _, tok := range strings.Split(headerValue, ",") {
		if strings.EqualFold(strings.TrimSpace(tok), want) {
			return true
		}
	}
	return false
}

// wsDialTimeout caps the TCP dial to chora-delivery for the upgrade handshake.
const wsDialTimeout = 10 * time.Second

// proxyWebSocket performs a transparent WebSocket-upgrade passthrough to
// chora-delivery. It hijacks the client connection, dials the chora-delivery
// downstream resolved from the SAME env-sourced config the verbatim REST proxy
// uses (DeliveryBaseURL — no inline config), replays the upgrade request line +
// headers (with mesh-trust headers stamped), then io.Copy's both directions
// until either side closes. HTTP/2 is NOT enforced for the 101 handshake
// (ADR-166 §D5) — net.Dial yields a plain HTTP/1.1 cleartext conn.
func (h *RplusDeliveryProxyHandler) proxyWebSocket(w http.ResponseWriter, r *http.Request) {
	base := h.agg.DeliveryBaseURL()
	if base == "" {
		http.Error(w, `{"error":{"code":"GATEWAY_UPSTREAM_NOT_CONFIGURED","message":"delivery service URL empty"}}`,
			http.StatusBadGateway)
		return
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		http.Error(w, `{"error":{"code":"GATEWAY_UPSTREAM_BAD_URL","message":"delivery base URL unparseable"}}`,
			http.StatusBadGateway)
		return
	}
	downstreamHost := hostWithDefaultPort(u)

	// Dial chora-delivery FIRST — if it fails we can still write a clean HTTP
	// error to the (not-yet-hijacked) client.
	upConn, err := net.DialTimeout("tcp", downstreamHost, wsDialTimeout)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"code":"GATEWAY_UPSTREAM_ERROR","message":%q}}`, err.Error()),
			http.StatusBadGateway)
		return
	}
	defer upConn.Close()

	// Build + write the upgrade request line + headers to chora-delivery. The
	// request URI is the same path + query the FE hit (verbatim — chora-delivery
	// serves the /ws endpoint at the same path). Mesh-trust headers are stamped
	// via the aggregator so the WS handshake carries the same trust context a
	// REST call would (tenantRequired + gcid context read them on the upgrade).
	if err := writeUpgradeRequest(upConn, r, h.agg, u.Host); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"code":"GATEWAY_UPSTREAM_ERROR","message":%q}}`, err.Error()),
			http.StatusBadGateway)
		return
	}

	// Hijack the client conn AFTER the downstream dial + request write succeed.
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, `{"error":{"code":"GATEWAY_HIJACK_UNSUPPORTED","message":"response writer is not a Hijacker"}}`,
			http.StatusInternalServerError)
		return
	}
	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"code":"GATEWAY_HIJACK_FAILED","message":%q}}`, err.Error()),
			http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// Bidirectional copy. Each direction closes on the first error/EOF; the
	// deferred Close() on the opposite conn unblocks the peer copy goroutine.
	errc := make(chan error, 2)
	// downstream → client: drain anything chora-delivery already buffered in
	// upBuf is not applicable (we read directly from upConn) — but the client
	// side may have buffered bytes in clientBuf.Reader from the pipelined
	// upgrade request, so flush those to the downstream first via the reader.
	go func() {
		_, e := io.Copy(upConn, clientBuf) // client → downstream
		errc <- e
	}()
	go func() {
		_, e := io.Copy(clientConn, upConn) // downstream → client
		errc <- e
	}()
	<-errc // first side to close ends the proxy; defers close both conns.
}

// hostWithDefaultPort returns host:port, defaulting the port from the URL
// scheme when the base URL omitted it (e.g. http://svc → svc:80).
func hostWithDefaultPort(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	if strings.EqualFold(u.Scheme, "https") || strings.EqualFold(u.Scheme, "wss") {
		return net.JoinHostPort(u.Hostname(), "443")
	}
	return net.JoinHostPort(u.Hostname(), "80")
}

// writeUpgradeRequest serialises the WebSocket upgrade HTTP/1.1 request line +
// headers to the downstream conn. It forwards the client's headers verbatim
// (preserving Upgrade / Connection / Sec-WebSocket-* so the handshake stays
// valid), overrides Host with the downstream authority, then stamps the
// canonical mesh-trust headers via the aggregator.
func writeUpgradeRequest(upConn net.Conn, r *http.Request, agg *gatewayproxy.Aggregator, downstreamAuthority string) error {
	// Strip the access_token query param before forwarding. It carried the
	// session JWT for the browser WS handshake (browsers cannot set the
	// Authorization header on a native WebSocket upgrade, so the gateway JWT
	// gate reads the token from access_token — see extractSessionToken in
	// jwt_auth.go). The validated identity is already stamped onto the
	// mesh-trust headers below; forwarding the raw token would leak it into
	// chora-delivery's access logs. chora-delivery's WS handler reads only the
	// path (sessionId) — re-encoding the remaining query is safe.
	requestURI := r.URL.Path
	if q := r.URL.Query(); len(q) > 0 {
		q.Del("access_token")
		if enc := q.Encode(); enc != "" {
			requestURI += "?" + enc
		}
	}

	// Clone the client request onto a fresh outbound request so the aggregator
	// can stamp mesh headers onto a Header map without mutating the live conn's
	// request. Start from the client's headers (preserves WS handshake headers).
	// #nosec G704 — downstreamAuthority is the env-configured chora-delivery
	// base URL host (not user input); requestURI is the downgrade handshake path.
	out, err := http.NewRequest(r.Method, "http://"+downstreamAuthority+requestURI, nil)
	if err != nil {
		return fmt.Errorf("build upgrade request: %w", err)
	}
	for k, vs := range r.Header {
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}
	out.Host = downstreamAuthority
	agg.StampDownstreamHeaders(out, gatewayProxyAuthFromRequest(r))

	bw := bufio.NewWriter(upConn)
	if _, err := fmt.Fprintf(bw, "%s %s HTTP/1.1\r\n", r.Method, requestURI); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(bw, "Host: %s\r\n", downstreamAuthority); err != nil {
		return err
	}
	if err := out.Header.Write(bw); err != nil {
		return err
	}
	if _, err := bw.WriteString("\r\n"); err != nil {
		return err
	}
	return bw.Flush()
}
