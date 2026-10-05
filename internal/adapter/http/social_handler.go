// social_handler.go — C+ social BFF handlers per CHO-1457.
//
// Routes (per `chora-contracts/openapi/bff-gateway.yaml` Social tag):
//
//	GET    /v1/feed                                 — paginated feed
//	GET    /v1/me/social                            — caller SocialProfile
//	POST   /v1/atoms/{atom_id}/share                — share atom to feed
//	POST   /v1/posts/{post_id}/reactions            — react to a post
//	DELETE /v1/posts/{post_id}/reactions/{rid}      — remove a reaction
//	GET    /v1/posts/{post_id}/comments             — fetch threaded comments
//	POST   /v1/posts/{post_id}/comments             — add comment / reply
//	GET    /v1/discovery/courses/public             — public catalogue (Phyllis Step 5)
//
// All routes are STUBS in this MVP — read paths return 200 with empty
// `data` arrays; writes return 501 Not Implemented (idempotent DELETE
// returns 204). Full implementation lands when chora-sharing learner-
// facing API ships (M12+).
package httpadapter

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/social"
	"github.com/apollo-chora/chora-gateway/internal/domain/route"
	"github.com/apollo-chora/chora-gateway/internal/domain/session"
)

// SocialHandler binds the social aggregator to the BFF mux.
type SocialHandler struct {
	agg *social.Aggregator
}

// NewSocialHandler constructs a SocialHandler.
func NewSocialHandler(agg *social.Aggregator) *SocialHandler {
	return &SocialHandler{agg: agg}
}

// NewRouterWithSocial wires the BFF mux including the C+ social MVP
// routes. graphqlHandler may be nil. Composes with the existing
// Phyllis + surface routes — none are removed.
func NewRouterWithSocial(
	routes route.Repository,
	sessions session.Repository,
	up upstream.Client,
	pAgg *phyllis.Aggregator,
	sAgg *social.Aggregator,
	graphqlHandler http.Handler,
) http.Handler {
	base := NewRouterWithPhyllisAndGraphQL(routes, sessions, up, pAgg, graphqlHandler)
	if sAgg == nil {
		return base
	}
	sh := NewSocialHandler(sAgg)

	// We wrap the existing handler with a new mux that shadows the /v1/*
	// social paths. Anything not matched falls through to `base`. Doing
	// it this way keeps the existing NewRouterWithPhyllisAndGraphQL
	// constructor closed for modification (open for extension).
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/feed", sh.handleFeed)
	mux.HandleFunc("/v1/feed/shared-atoms", sh.handleSharedAtomsFeed)
	mux.HandleFunc("/v1/me/social", sh.handleMySocial)
	mux.HandleFunc("/v1/me/bookmarks", sh.handleBookmarks)
	// Companion-milestone drafts + share preference (CHO-2258). Gateway list 1 of
	// 3 — /v1/me/ is already in DefaultJWTGatedPrefixes (list 2) and the
	// ns/sharing Istio policy already allowlists these paths (list 3).
	mux.HandleFunc("/v1/me/post-drafts", sh.handleMilestoneDrafts)                              // GET the owner's queue
	mux.HandleFunc("/v1/me/post-drafts/", sh.handleMilestoneDraftScoped)                        // POST {id}/publish | {id}/discard
	mux.HandleFunc("/v1/me/preferences/companion-milestone-share", sh.handleMilestoneSharePref) // GET | POST auto|draft|suppress
	// ADR-254 D9 alias, drop after the SPA cut (WP-X go): same handler; the
	// sharing upstream path stays pre-rename until sharing's own window.
	mux.HandleFunc("/v1/me/preferences/familiar-milestone-share", sh.handleMilestoneSharePref)
	mux.HandleFunc("/v1/connections", sh.handleConnections)
	mux.HandleFunc("/v1/connections/", sh.handleConnectionScoped) // B-lite.2 relationship subtree
	mux.HandleFunc("/v1/atoms/", sh.handleAtomScoped)             // /v1/atoms/{id}/share
	mux.HandleFunc("/v1/posts/", sh.handlePostScoped)             // /v1/posts/{id}/{reactions|comments}/...
	mux.HandleFunc("/v1/duels", sh.handleDuels)
	mux.HandleFunc("/v1/duels/", sh.handleDuelScoped)
	mux.HandleFunc("/v1/me/profile", sh.handleProfile)
	mux.HandleFunc("/v1/me/profile/", sh.handleProfileScoped)
	mux.HandleFunc("/v1/leaderboard", sh.handleLeaderboard) // GET (US4)
	mux.HandleFunc("/v1/discovery/courses/public", sh.handlePublicCourses)
	// Anything else: delegate to the base router.
	mux.Handle("/", base)
	return mux
}

// -----------------------------------------------------------------------------
// /v1/feed/shared-atoms  (Atom Sharing Redesign — US1)
// -----------------------------------------------------------------------------

func (sh *SocialHandler) handleSharedAtomsFeed(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	resp, _ := sh.agg.GetSharedAtomsFeed(r.Context(), social.GetSharedAtomsParams{
		Auth:               authCtxFromRequest(r),
		TenantID:           r.URL.Query().Get("tenant_id"),
		ViewerGCID:         r.URL.Query().Get("viewer_gcid"),
		Cursor:             r.URL.Query().Get("cursor"),
		Limit:              limit,
		TopicFilter:        r.URL.Query().Get("topic_filter"),
		QuestionTypeFilter: r.URL.Query().Get("question_type_filter"),
		Scope:              r.URL.Query().Get("scope"),
	})
	writeSocialResp(w, resp)
}

// -----------------------------------------------------------------------------
// /v1/duels  +  /v1/duels/{duel_id}
// -----------------------------------------------------------------------------

func (sh *SocialHandler) handleDuels(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	resp, _ := sh.agg.GetDuel(r.Context(), social.GetDuelParams{
		Auth:     authCtxFromRequest(r),
		TenantID: r.URL.Query().Get("tenant_id"),
	})
	writeSocialResp(w, resp)
}

func (sh *SocialHandler) handleDuelScoped(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/"), "/v1/duels/")
	parts := strings.Split(rest, "/")
	if len(parts) < 1 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	switch {
	// /v1/duels/queue — POST (enter), DELETE (cancel)
	case len(parts) == 1 && parts[0] == "queue":
		switch r.Method {
		case http.MethodPost:
			var body []byte
			if r.Body != nil {
				body, _ = io.ReadAll(r.Body)
				_ = r.Body.Close()
			}
			resp, _ := sh.agg.QueueDuel(r.Context(), social.QueueDuelParams{
				Auth:     authCtxFromRequest(r),
				TenantID: r.URL.Query().Get("tenant_id"),
				Body:     body,
			})
			writeSocialResp(w, resp)
		case http.MethodDelete:
			resp, _ := sh.agg.CancelQueueDuel(r.Context(), social.QueueDuelParams{
				Auth:     authCtxFromRequest(r),
				TenantID: r.URL.Query().Get("tenant_id"),
			})
			writeSocialResp(w, resp)
		default:
			methodNotAllowed(w)
		}

	// /v1/duels/queue/heartbeat — POST
	case len(parts) == 2 && parts[0] == "queue" && parts[1] == "heartbeat":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		resp, _ := sh.agg.HeartbeatQueue(r.Context(), social.QueueDuelParams{
			Auth:     authCtxFromRequest(r),
			TenantID: r.URL.Query().Get("tenant_id"),
		})
		writeSocialResp(w, resp)

	// /v1/duels/queue/status — GET
	case len(parts) == 2 && parts[0] == "queue" && parts[1] == "status":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		resp, _ := sh.agg.QueueStatus(r.Context(), social.QueueDuelParams{
			Auth:     authCtxFromRequest(r),
			TenantID: r.URL.Query().Get("tenant_id"),
		})
		writeSocialResp(w, resp)

	case len(parts) == 1 && parts[0] == "my-rating":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		resp, _ := sh.agg.GetMyRating(r.Context(), social.GetMyRatingParams{
			Auth:     authCtxFromRequest(r),
			TenantID: r.URL.Query().Get("tenant_id"),
		})
		writeSocialResp(w, resp)

	case len(parts) == 1 && parts[0] == "leaderboard":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		resp, _ := sh.agg.GetDuelLeaderboard(r.Context(), social.GetDuelLeaderboardParams{
			Auth:     authCtxFromRequest(r),
			TenantID: r.URL.Query().Get("tenant_id"),
		})
		writeSocialResp(w, resp)

	case len(parts) == 1:
		duelID := parts[0]
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		resp, _ := sh.agg.GetDuel(r.Context(), social.GetDuelParams{
			Auth:     authCtxFromRequest(r),
			TenantID: r.URL.Query().Get("tenant_id"),
			DuelID:   duelID,
		})
		writeSocialResp(w, resp)

	case len(parts) == 2 && parts[1] == "answer":
		duelID := parts[0]
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
			_ = r.Body.Close()
		}
		resp, _ := sh.agg.SubmitDuelAnswer(r.Context(), social.SubmitDuelAnswerParams{
			Auth:   authCtxFromRequest(r),
			DuelID: duelID,
			Body:   body,
		})
		writeSocialResp(w, resp)

	case len(parts) == 2 && parts[1] == "ws":
		duelID := parts[0]
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		// WebSocket upgrade requests cannot be served by the normal
		// aggregator fan-out (a plain HTTP GET can't carry the 101
		// Switching Protocols handshake). Detect the upgrade + proxy
		// directly to chora-sharing, stamping X-Tenant-Id + gcid headers
		// the sharing WS handler requires. Mirrors the R+ delivery WS
		// passthrough (ADR-168 / ADR-166 §D5).
		if isWebSocketUpgrade(r) {
			sh.proxyDuelWebSocket(w, r, duelID)
			return
		}
		resp, _ := sh.agg.DuelWS(r.Context(), social.DuelWSParams{
			Auth:   authCtxFromRequest(r),
			DuelID: duelID,
		})
		writeSocialResp(w, resp)

	default:
		http.NotFound(w, r)
	}
}

// proxyDuelWebSocket performs a transparent WebSocket-upgrade passthrough to
// chora-sharing for /v1/duels/{duel_id}/ws. It hijacks the client connection,
// dials chora-sharing directly, replays the upgrade request line + headers
// (with X-Tenant-Id + gcid mesh-trust headers stamped from the validated
// ChoraSession JWT), then io.Copy's both directions until either side closes.
// Mirrors RplusDeliveryProxyHandler.proxyWebSocket (ADR-168 / ADR-166 §D5).
//
// The access_token query param (which carried the session JWT for the
// browser WS handshake — browsers cannot set the Authorization header on a
// native WebSocket) is stripped before forwarding; the validated identity is
// stamped onto the mesh-trust headers via StampDownstreamHeaders.
func (sh *SocialHandler) proxyDuelWebSocket(w http.ResponseWriter, r *http.Request, duelID string) {
	base := sh.agg.SharingBaseURL()
	if base == "" {
		http.Error(w, `{"error":{"code":"GATEWAY_UPSTREAM_NOT_CONFIGURED","message":"sharing service URL empty"}}`,
			http.StatusBadGateway)
		return
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		http.Error(w, `{"error":{"code":"GATEWAY_UPSTREAM_BAD_URL","message":"sharing base URL unparseable"}}`,
			http.StatusBadGateway)
		return
	}
	downstreamHost := hostWithDefaultPort(u)

	// Dial chora-sharing FIRST — if it fails we can still write a clean HTTP
	// error to the (not-yet-hijacked) client.
	upConn, err := net.DialTimeout("tcp", downstreamHost, 10*time.Second)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"code":"GATEWAY_UPSTREAM_ERROR","message":%q}}`, err.Error()),
			http.StatusBadGateway)
		return
	}
	defer upConn.Close()

	// Build the upgrade request to chora-sharing. Strip the access_token
	// query param (carried the session JWT for the browser handshake); the
	// validated identity is stamped onto mesh-trust headers below.
	requestURI := "/v1/duels/" + duelID + "/ws"
	// #nosec G704 — target host is the env-configured chora-sharing base URL
	// (not user input); duelID is validated by isUUIDShape upstream.
	out, err := http.NewRequest(r.Method, "http://"+u.Host+requestURI, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"code":"GATEWAY_UPSTREAM_ERROR","message":%q}}`, err.Error()),
			http.StatusBadGateway)
		return
	}
	// Forward the client's WS handshake headers verbatim (preserves Upgrade /
	// Connection / Sec-WebSocket-* so the handshake stays valid).
	for k, vs := range r.Header {
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}
	out.Host = u.Host
	sh.agg.StampDownstreamHeaders(out, authCtxFromRequest(r))

	bw := bufio.NewWriter(upConn)
	if _, err := fmt.Fprintf(bw, "%s %s HTTP/1.1\r\n", r.Method, requestURI); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"code":"GATEWAY_UPSTREAM_ERROR","message":%q}}`, err.Error()),
			http.StatusBadGateway)
		return
	}
	if _, err := fmt.Fprintf(bw, "Host: %s\r\n", u.Host); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"code":"GATEWAY_UPSTREAM_ERROR","message":%q}}`, err.Error()),
			http.StatusBadGateway)
		return
	}
	if err := out.Header.Write(bw); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"code":"GATEWAY_UPSTREAM_ERROR","message":%q}}`, err.Error()),
			http.StatusBadGateway)
		return
	}
	if _, err := bw.WriteString("\r\n"); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"code":"GATEWAY_UPSTREAM_ERROR","message":%q}}`, err.Error()),
			http.StatusBadGateway)
		return
	}
	if err := bw.Flush(); err != nil {
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

// -----------------------------------------------------------------------------
// /v1/me/profile  +  /v1/me/profile/{generate|tags}
// -----------------------------------------------------------------------------

func (sh *SocialHandler) handleProfile(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	resp, _ := sh.agg.GetProfile(r.Context(), social.GetProfileParams{
		Auth: authCtxFromRequest(r),
	})
	writeSocialResp(w, resp)
}

func (sh *SocialHandler) handleProfileScoped(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/me/profile/"), "/")
	switch rest {
	case "ws":
		// /v1/me/profile/ws — WebSocket upgrade for async profile-generation
		// notifications. The browser cannot set the Authorization header on a
		// native WebSocket handshake (RFC 6455), so the session JWT travels as
		// ?access_token=... + is validated by the gateway's extractSessionToken
		// (jwt_auth.go). The validated identity is stamped onto mesh-trust
		// headers below. Mirrors the duel WS proxy (proxyDuelWebSocket) +
		// the R+ delivery WS passthrough (ADR-168 / ADR-166 §D5).
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if isWebSocketUpgrade(r) {
			sh.proxyProfileWebSocket(w, r)
			return
		}
		// A non-upgrade GET on /v1/me/profile/ws is a client error — there
		// is no REST body to return. 404 keeps it consistent with the
		// default branch.
		http.NotFound(w, r)
	case "generate":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
			_ = r.Body.Close()
		}
		resp, _ := sh.agg.GenerateProfile(r.Context(), social.GenerateProfileParams{
			Auth: authCtxFromRequest(r),
			Body: body,
		})
		writeSocialResp(w, resp)
	case "tags":
		if !requireMethod(w, r, http.MethodPut) {
			return
		}
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
			_ = r.Body.Close()
		}
		resp, _ := sh.agg.UpdateProfileTags(r.Context(), social.UpdateProfileTagsParams{
			Auth: authCtxFromRequest(r),
			Body: body,
		})
		writeSocialResp(w, resp)
	default:
		http.NotFound(w, r)
	}
}

// proxyProfileWebSocket performs a transparent WebSocket-upgrade passthrough
// to chora-sharing for /v1/me/profile/ws. It hijacks the client connection,
// dials chora-sharing directly, replays the upgrade request line + headers
// (with X-Tenant-Id + gcid mesh-trust headers stamped from the validated
// ChoraSession JWT), then io.Copy's both directions until either side closes.
//
// Identical mechanics to proxyDuelWebSocket above — the only difference is the
// downstream request URI (no {duel_id} path param). See proxyDuelWebSocket
// for the full rationale (hijack-after-dial, access_token strip, mesh-trust
// header stamping).
func (sh *SocialHandler) proxyProfileWebSocket(w http.ResponseWriter, r *http.Request) {
	base := sh.agg.SharingBaseURL()
	if base == "" {
		http.Error(w, `{"error":{"code":"GATEWAY_UPSTREAM_NOT_CONFIGURED","message":"sharing service URL empty"}}`,
			http.StatusBadGateway)
		return
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		http.Error(w, `{"error":{"code":"GATEWAY_UPSTREAM_BAD_URL","message":"sharing base URL unparseable"}}`,
			http.StatusBadGateway)
		return
	}
	downstreamHost := hostWithDefaultPort(u)

	// Dial chora-sharing FIRST — if it fails we can still write a clean HTTP
	// error to the (not-yet-hijacked) client.
	upConn, err := net.DialTimeout("tcp", downstreamHost, 10*time.Second)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"code":"GATEWAY_UPSTREAM_ERROR","message":%q}}`, err.Error()),
			http.StatusBadGateway)
		return
	}
	defer upConn.Close()

	// Build the upgrade request to chora-sharing. Strip the access_token
	// query param (carried the session JWT for the browser handshake); the
	// validated identity is stamped onto mesh-trust headers below.
	requestURI := "/v1/me/profile/ws"
	out, err := http.NewRequest(r.Method, "http://"+u.Host+requestURI, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"code":"GATEWAY_UPSTREAM_ERROR","message":%q}}`, err.Error()),
			http.StatusBadGateway)
		return
	}
	// Forward the client's WS handshake headers verbatim (preserves Upgrade /
	// Connection / Sec-WebSocket-* so the handshake stays valid).
	for k, vs := range r.Header {
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}
	out.Host = u.Host
	sh.agg.StampDownstreamHeaders(out, authCtxFromRequest(r))

	bw := bufio.NewWriter(upConn)
	if _, err := fmt.Fprintf(bw, "%s %s HTTP/1.1\r\n", r.Method, requestURI); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"code":"GATEWAY_UPSTREAM_ERROR","message":%q}}`, err.Error()),
			http.StatusBadGateway)
		return
	}
	if _, err := fmt.Fprintf(bw, "Host: %s\r\n", u.Host); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"code":"GATEWAY_UPSTREAM_ERROR","message":%q}}`, err.Error()),
			http.StatusBadGateway)
		return
	}
	if err := out.Header.Write(bw); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"code":"GATEWAY_UPSTREAM_ERROR","message":%q}}`, err.Error()),
			http.StatusBadGateway)
		return
	}
	if _, err := bw.WriteString("\r\n"); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"code":"GATEWAY_UPSTREAM_ERROR","message":%q}}`, err.Error()),
			http.StatusBadGateway)
		return
	}
	if err := bw.Flush(); err != nil {
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

// -----------------------------------------------------------------------------
// /v1/leaderboard  (Atom Sharing Redesign — US4)
// -----------------------------------------------------------------------------

func (sh *SocialHandler) handleLeaderboard(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	resp, _ := sh.agg.GetLeaderboard(r.Context(), social.GetLeaderboardParams{
		Auth:          authCtxFromRequest(r),
		TenantID:      r.URL.Query().Get("tenant_id"),
		Scope:         r.URL.Query().Get("scope"),
		ScopeTargetID: r.URL.Query().Get("scope_target_id"),
		Metric:        r.URL.Query().Get("metric"),
		Period:        r.URL.Query().Get("period"),
		Season:        r.URL.Query().Get("season"),
		Limit:         limit,
		Cursor:        r.URL.Query().Get("cursor"),
	})
	writeSocialResp(w, resp)
}

// -----------------------------------------------------------------------------
// /v1/feed
// -----------------------------------------------------------------------------

func (sh *SocialHandler) handleFeed(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	resp, _ := sh.agg.GetFeed(r.Context(), social.GetFeedParams{
		Auth:     authCtxFromRequest(r),
		TenantID: r.URL.Query().Get("tenant_id"),
		Limit:    limit,
		Cursor:   r.URL.Query().Get("cursor"),
	})
	writeSocialResp(w, resp)
}

// -----------------------------------------------------------------------------
// /v1/me/social
// -----------------------------------------------------------------------------

func (sh *SocialHandler) handleMySocial(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	resp, _ := sh.agg.GetMySocial(r.Context(), authCtxFromRequest(r))
	writeSocialResp(w, resp)
}

// -----------------------------------------------------------------------------
// /v1/atoms/{atom_id}/share
// -----------------------------------------------------------------------------

func (sh *SocialHandler) handleAtomScoped(w http.ResponseWriter, r *http.Request) {
	// Path shape: /v1/atoms/{atom_id}/{share|bookmark}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/atoms/")
	parts := strings.Split(strings.TrimSuffix(rest, "/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	atomID := parts[0]
	action := parts[1]
	switch action {
	case "share":
		switch r.Method {
		case http.MethodPost:
			// ADR-196 D4: opaque body passthrough. The shareAtomRequest DTO
			// {atom_revision_id, caption, license_terms, royalty_rate} is forwarded
			// to chora-sharing verbatim — the BFF does not reshape it. atom_id comes
			// from the path; author_gcid + tenant_id from the identity headers;
			// Idempotency-Key rides on AuthCtx (populated by authCtxFromRequest).
			var body []byte
			if r.Body != nil {
				body, _ = io.ReadAll(r.Body)
				_ = r.Body.Close()
			}
			resp, _ := sh.agg.ShareAtomToFeed(r.Context(), social.ShareAtomParams{
				Auth:   authCtxFromRequest(r),
				AtomID: atomID,
				Body:   body,
			})
			writeSocialResp(w, resp)
		case http.MethodDelete:
			resp, _ := sh.agg.RevokeShareFromFeed(r.Context(), social.ShareAtomParams{
				Auth:   authCtxFromRequest(r),
				AtomID: atomID,
			})
			writeSocialResp(w, resp)
		default:
			methodNotAllowed(w)
		}

	case "bookmark":
		auth := authCtxFromRequest(r)
		switch r.Method {
		case http.MethodPost:
			resp, _ := sh.agg.BookmarkAtom(r.Context(), social.BookmarkParams{
				Auth:   auth,
				AtomID: atomID,
			})
			writeSocialResp(w, resp)
		case http.MethodDelete:
			resp, _ := sh.agg.UnbookmarkAtom(r.Context(), social.BookmarkParams{
				Auth:   auth,
				AtomID: atomID,
			})
			writeSocialResp(w, resp)
		default:
			methodNotAllowed(w)
		}

	default:
		http.NotFound(w, r)
	}
}

// -----------------------------------------------------------------------------
// /v1/posts/{post_id}/{reactions|comments}/...
// -----------------------------------------------------------------------------

func (sh *SocialHandler) handlePostScoped(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/posts/")
	parts := strings.Split(strings.TrimSuffix(rest, "/"), "/")
	// Valid shapes:
	//   parts == [postID, "reactions"]                            (POST)
	//   parts == [postID, "reactions", reactionID]                (DELETE)
	//   parts == [postID, "comments"]                             (GET, POST)
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	postID := parts[0]

	switch parts[1] {
	case "reactions":
		if len(parts) == 2 {
			sh.handleCreateReaction(w, r, postID)
			return
		}
		if len(parts) == 3 {
			sh.handleDeleteReaction(w, r, postID, parts[2])
			return
		}
		http.NotFound(w, r)
	case "comments":
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			sh.handleGetComments(w, r, postID)
		case http.MethodPost:
			sh.handleAddComment(w, r, postID)
		default:
			methodNotAllowed(w)
		}
	default:
		http.NotFound(w, r)
	}
}

func (sh *SocialHandler) handleCreateReaction(w http.ResponseWriter, r *http.Request, postID string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req struct {
		Kind string `json:"kind"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	_ = r.Body.Close()
	resp, _ := sh.agg.CreateReaction(r.Context(), social.CreateReactionParams{
		Auth:   authCtxFromRequest(r),
		PostID: postID,
		Type:   req.Kind,
	})
	writeSocialResp(w, resp)
}

func (sh *SocialHandler) handleDeleteReaction(w http.ResponseWriter, r *http.Request, postID, reactionID string) {
	if !requireMethod(w, r, http.MethodDelete) {
		return
	}
	resp, _ := sh.agg.DeleteReaction(r.Context(), social.DeleteReactionParams{
		Auth:       authCtxFromRequest(r),
		PostID:     postID,
		ReactionID: reactionID,
	})
	writeSocialResp(w, resp)
}

func (sh *SocialHandler) handleGetComments(w http.ResponseWriter, r *http.Request, postID string) {
	resp, _ := sh.agg.GetComments(r.Context(), social.GetCommentsParams{
		Auth:   authCtxFromRequest(r),
		PostID: postID,
		Cursor: r.URL.Query().Get("cursor"),
	})
	writeSocialResp(w, resp)
}

func (sh *SocialHandler) handleAddComment(w http.ResponseWriter, r *http.Request, postID string) {
	var req struct {
		Body            string `json:"body"`
		ParentCommentID string `json:"parent_comment_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	_ = r.Body.Close()
	resp, _ := sh.agg.AddComment(r.Context(), social.AddCommentParams{
		Auth:            authCtxFromRequest(r),
		PostID:          postID,
		Body:            req.Body,
		ParentCommentID: req.ParentCommentID,
	})
	writeSocialResp(w, resp)
}

// -----------------------------------------------------------------------------
// /v1/discovery/courses/public
// -----------------------------------------------------------------------------

func (sh *SocialHandler) handlePublicCourses(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	resp, _ := sh.agg.GetPublicCourses(r.Context(), social.GetPublicCoursesParams{
		Auth:     authCtxFromRequest(r),
		TenantID: r.URL.Query().Get("tenant_id"),
	})
	writeSocialResp(w, resp)
}

// -----------------------------------------------------------------------------
// /v1/connections
// -----------------------------------------------------------------------------

func (sh *SocialHandler) handleConnections(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	resp, _ := sh.agg.ListConnections(r.Context(), social.ListConnectionsParams{
		Auth:   authCtxFromRequest(r),
		Type:   r.URL.Query().Get("type"),
		Cursor: r.URL.Query().Get("cursor"),
		Limit:  limit,
	})
	writeSocialResp(w, resp)
}

// -----------------------------------------------------------------------------
// /v1/connections/* — relationship subtree (ADR-230 B-lite.2)
// -----------------------------------------------------------------------------

// handleConnectionScoped routes the relationship writes + pending-list read:
//
//	POST   /v1/connections/follows                          — follow {gcid}
//	DELETE /v1/connections/follows/{gcid}                   — unfollow
//	POST   /v1/connections/blocks                           — block {gcid}
//	DELETE /v1/connections/blocks/{gcid}                    — unblock
//	POST   /v1/connections/friend-requests                  — request {gcid}
//	GET    /v1/connections/friend-requests                  — pending lists
//	POST   /v1/connections/friend-requests/{gcid}/accept    — accept
//	DELETE /v1/connections/friend-requests/incoming/{gcid}  — decline
//	DELETE /v1/connections/friend-requests/outgoing/{gcid}  — cancel
//	DELETE /v1/connections/friends/{gcid}                   — unfriend
//
// Every match forwards verbatim through the aggregator's RelationshipOp
// (opaque passthrough, ADR-196 D4) — chora-sharing owns semantics + status
// codes. Unknown shapes 404; known shapes with the wrong method 405.
func (sh *SocialHandler) handleConnectionScoped(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/connections/"), "/")
	parts := strings.Split(rest, "/")

	forward := func() {
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
			_ = r.Body.Close()
		}
		subpath := rest
		if q := r.URL.RawQuery; q != "" {
			subpath += "?" + q // reads carry params (?limit=) — forward verbatim
		}
		resp, _ := sh.agg.RelationshipOp(r.Context(), social.RelationshipOpParams{
			Auth:    authCtxFromRequest(r),
			Method:  r.Method,
			Subpath: subpath,
			Body:    body,
		})
		writeSocialResp(w, resp)
	}
	requireThenForward := func(methods ...string) {
		for _, m := range methods {
			if r.Method == m {
				forward()
				return
			}
		}
		methodNotAllowed(w)
	}

	switch {
	case len(parts) == 1 && parts[0] == "follows":
		requireThenForward(http.MethodPost)
	case len(parts) == 2 && parts[0] == "follows" && parts[1] != "":
		requireThenForward(http.MethodDelete)
	case len(parts) == 1 && parts[0] == "blocks":
		requireThenForward(http.MethodPost)
	case len(parts) == 2 && parts[0] == "blocks" && parts[1] != "":
		requireThenForward(http.MethodDelete)
	case len(parts) == 1 && parts[0] == "friend-requests":
		requireThenForward(http.MethodPost, http.MethodGet)
	case len(parts) == 1 && parts[0] == "suggestions": // B-lite.3 bounded FoF
		requireThenForward(http.MethodGet)
	case len(parts) == 3 && parts[0] == "friend-requests" && parts[2] == "accept" && parts[1] != "":
		requireThenForward(http.MethodPost)
	case len(parts) == 3 && parts[0] == "friend-requests" &&
		(parts[1] == "incoming" || parts[1] == "outgoing") && parts[2] != "":
		requireThenForward(http.MethodDelete)
	case len(parts) == 2 && parts[0] == "friends" && parts[1] != "":
		requireThenForward(http.MethodDelete)
	default:
		http.NotFound(w, r)
	}
}

// -----------------------------------------------------------------------------
// /v1/me/bookmarks
// -----------------------------------------------------------------------------

func (sh *SocialHandler) handleBookmarks(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	resp, _ := sh.agg.ListBookmarks(r.Context(), social.ListBookmarksParams{
		Auth:   authCtxFromRequest(r),
		Cursor: r.URL.Query().Get("cursor"),
		Limit:  limit,
	})
	writeSocialResp(w, resp)
}

// -----------------------------------------------------------------------------
// shared helpers
// -----------------------------------------------------------------------------

func writeSocialResp(w http.ResponseWriter, resp social.Response) {
	if resp.Status == 0 {
		resp.Status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.Status)
	if resp.Body != nil {
		_, _ = w.Write(resp.Body)
	}
}

func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
		"method not allowed on this path")
}

// _ keeps io import alive if future signed-body validation is added.
var _ = io.Discard
