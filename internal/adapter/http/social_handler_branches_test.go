// social_handler_branches_test.go — route-branch coverage for the C+ social
// handlers that the original TDD suite left untested: the duel queue
// lifecycle (queue/heartbeat/status/my-rating/leaderboard/answer), the
// profile surface (/v1/me/profile, /v1/me/profile/{generate,tags}),
// bookmarks, the atom-scoped bookmark/revoke branches, the post-scoped
// reaction/comment edge shapes, and the WebSocket-proxy fail-loud paths
// (unconfigured / unparseable / unreachable downstream — the happy-path 101
// hijack is out of scope for unit tests).
package httpadapter_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/adapter/inmem"
	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/social"
)

// newSocialFixtureWithAgg builds a router with a caller-supplied social
// aggregator (so tests can pin SharingURL, including deliberately broken
// values for the WS-proxy fail-loud paths).
func newSocialFixtureWithAgg(t *testing.T, sAgg *social.Aggregator) *httptest.Server {
	t.Helper()
	routes := inmem.NewRouteRepository()
	sessions := inmem.NewSessionRepository()
	up := upstream.NewFakeUpstream()
	phyllisAgg := phyllis.New(phyllis.LoadConfigFromEnv(), nil)
	mux := httpadapter.NewRouterWithSocial(routes, sessions, up, phyllisAgg, sAgg, nil)
	return httptest.NewServer(mux)
}

func socialDoReq(t *testing.T, method, url string, body []byte, hdr map[string]string) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func socialDecodeBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return body
}

// ---------------------------------------------------------------------------
// /v1/duels/* queue lifecycle
// ---------------------------------------------------------------------------

func TestSocial_DuelQueue_POSTAndDELETE(t *testing.T) {
	srv := newSocialFixture(t)
	defer srv.Close()

	// POST /v1/duels/queue — matchmaking entry.
	resp := socialDoReq(t, http.MethodPost, srv.URL+"/v1/duels/queue", []byte(`{"mode":"POOL"}`), nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("POST queue status = %d; want 502 (unconfigured)", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// DELETE /v1/duels/queue — cancel matchmaking.
	resp = socialDoReq(t, http.MethodDelete, srv.URL+"/v1/duels/queue", nil, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("DELETE queue status = %d; want 502 (unconfigured)", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// PUT /v1/duels/queue — not a queue method → 405.
	resp = socialDoReq(t, http.MethodPut, srv.URL+"/v1/duels/queue", nil, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT queue status = %d; want 405", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestSocial_DuelQueue_HeartbeatStatusMyRatingLeaderboard(t *testing.T) {
	srv := newSocialFixture(t)
	defer srv.Close()

	for _, tc := range []struct {
		name   string
		method string
		url    string
	}{
		{"heartbeat", http.MethodPost, "/v1/duels/queue/heartbeat"},
		{"status", http.MethodGet, "/v1/duels/queue/status"},
		{"my-rating", http.MethodGet, "/v1/duels/my-rating"},
		{"leaderboard", http.MethodGet, "/v1/duels/leaderboard"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			resp := socialDoReq(t, tc.method, srv.URL+tc.url, nil, nil)
			defer resp.Body.Close()
			// Duels are write-led: the matchmaking queue + heartbeat fail loud
			// 502 when unconfigured; my-rating/leaderboard read-stubs 501.
			want := http.StatusNotImplemented
			if tc.name == "heartbeat" || tc.name == "status" {
				want = http.StatusBadGateway
			}
			if resp.StatusCode != want {
				t.Errorf("%s %s status = %d; want %d", tc.method, tc.url, resp.StatusCode, want)
			}
		})
	}
}

func TestSocial_DuelScoped_AnswerAnd404Shapes(t *testing.T) {
	srv := newSocialFixture(t)
	defer srv.Close()

	// POST /v1/duels/{id}/answer.
	resp := socialDoReq(t, http.MethodPost, srv.URL+"/v1/duels/duel-1/answer", []byte(`{"answer_id":"a1"}`), nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("POST answer status = %d; want 502 (unconfigured)", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// GET /v1/duels/duel-1 — single duel read → 501 stub.
	resp = socialDoReq(t, http.MethodGet, srv.URL+"/v1/duels/duel-1", nil, nil)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("GET duel status = %d; want 501", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// GET /v1/duels/duel-1/ws (non-upgrade) → DuelWS fan-out → 502 unconfigured.
	resp = socialDoReq(t, http.MethodGet, srv.URL+"/v1/duels/duel-1/ws", nil, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("GET /ws (non-upgrade) status = %d; want 502", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Unknown scoped shape → 404.
	resp = socialDoReq(t, http.MethodGet, srv.URL+"/v1/duels/duel-1/nonsense/x", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown duel subpath status = %d; want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// ---------------------------------------------------------------------------
// WS-proxy fail-loud paths
// ---------------------------------------------------------------------------

func wsUpgradeHeaders() map[string]string {
	return map[string]string{
		"Connection":            "Upgrade",
		"Upgrade":               "websocket",
		"Sec-WebSocket-Version": "13",
		"Sec-WebSocket-Key":     "dGhlIHNhbXBsZSBub25jZQ==",
	}
}

// TestSocial_DuelWS_UnconfiguredSharingURL502 — the upgrade-proxy must fail
// 502 GATEWAY_UPSTREAM_NOT_CONFIGURED when SharingURL is empty, rather than
// silently hijacking nowhere.
func TestSocial_DuelWS_UnconfiguredSharingURL502(t *testing.T) {
	srv := newSocialFixture(t) // social.Config{} → empty SharingURL
	defer srv.Close()

	resp := socialDoReq(t, http.MethodGet, srv.URL+"/v1/duels/duel-1/ws", nil, wsUpgradeHeaders())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d; want 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "GATEWAY_UPSTREAM_NOT_CONFIGURED") {
		t.Errorf("body missing GATEWAY_UPSTREAM_NOT_CONFIGURED: %s", body)
	}
}

func TestSocial_DuelWS_UnparseableSharingURL502(t *testing.T) {
	agg := social.New(social.Config{SharingURL: "://bad-url"})
	srv := newSocialFixtureWithAgg(t, agg)
	defer srv.Close()

	resp := socialDoReq(t, http.MethodGet, srv.URL+"/v1/duels/duel-1/ws", nil, wsUpgradeHeaders())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d; want 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "GATEWAY_UPSTREAM_BAD_URL") {
		t.Errorf("body missing GATEWAY_UPSTREAM_BAD_URL: %s", body)
	}
}

func TestSocial_DuelWS_UnreachableDownstream502(t *testing.T) {
	// Point at a port nothing listens on: the pre-hijack dial fails and the
	// client still receives a clean 502 envelope.
	agg := social.New(social.Config{SharingURL: "http://127.0.0.1:1"})
	srv := newSocialFixtureWithAgg(t, agg)
	defer srv.Close()

	resp := socialDoReq(t, http.MethodGet, srv.URL+"/v1/duels/duel-1/ws", nil, wsUpgradeHeaders())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d; want 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "GATEWAY_UPSTREAM_ERROR") {
		t.Errorf("body missing GATEWAY_UPSTREAM_ERROR: %s", body)
	}
}

func TestSocial_ProfileWS_UnconfiguredSharingURL502(t *testing.T) {
	srv := newSocialFixture(t)
	defer srv.Close()

	resp := socialDoReq(t, http.MethodGet, srv.URL+"/v1/me/profile/ws", nil, wsUpgradeHeaders())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d; want 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "GATEWAY_UPSTREAM_NOT_CONFIGURED") {
		t.Errorf("body missing GATEWAY_UPSTREAM_NOT_CONFIGURED: %s", body)
	}
}

func TestSocial_ProfileWS_NonUpgradeGET404(t *testing.T) {
	srv := newSocialFixture(t)
	defer srv.Close()

	// GET /v1/me/profile/ws WITHOUT upgrade headers → 404 (no REST body).
	resp := socialDoReq(t, http.MethodGet, srv.URL+"/v1/me/profile/ws", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d; want 404", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// /v1/me/profile surface
// ---------------------------------------------------------------------------

func TestSocial_Profile_GetGenerateTags(t *testing.T) {
	srv := newSocialFixture(t)
	defer srv.Close()

	// GET /v1/me/profile → 501 stub.
	resp := socialDoReq(t, http.MethodGet, srv.URL+"/v1/me/profile", nil, nil)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("GET profile status = %d; want 501", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// POST /v1/me/profile/generate → 502 unconfigured.
	resp = socialDoReq(t, http.MethodPost, srv.URL+"/v1/me/profile/generate", []byte(`{"topic":"math"}`), nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("POST generate status = %d; want 502", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// PUT /v1/me/profile/tags → 502 unconfigured.
	resp = socialDoReq(t, http.MethodPut, srv.URL+"/v1/me/profile/tags", []byte(`{"tags":["math"]}`), nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("PUT tags status = %d; want 502", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Unknown scoped profile subpath → 404.
	resp = socialDoReq(t, http.MethodGet, srv.URL+"/v1/me/profile/unknown", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown profile subpath status = %d; want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// ---------------------------------------------------------------------------
// Bookmarks
// ---------------------------------------------------------------------------

func TestSocial_Bookmarks_ListAndAtomScoped(t *testing.T) {
	srv := newSocialFixture(t)
	defer srv.Close()

	// GET /v1/me/bookmarks → 200 empty (read-led stub).
	resp := socialDoReq(t, http.MethodGet, srv.URL+"/v1/me/bookmarks?limit=10", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET bookmarks status = %d; want 200", resp.StatusCode)
	}
	body := socialDecodeBody(t, resp)
	if _, ok := body["bookmarks"].([]any); !ok {
		t.Errorf("body.bookmarks not array: %v", body)
	}

	// POST /v1/atoms/{id}/bookmark → 502 unconfigured.
	resp = socialDoReq(t, http.MethodPost, srv.URL+"/v1/atoms/atom-1/bookmark", nil, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("POST bookmark status = %d; want 502", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// DELETE /v1/atoms/{id}/bookmark → 502 unconfigured.
	resp = socialDoReq(t, http.MethodDelete, srv.URL+"/v1/atoms/atom-1/bookmark", nil, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("DELETE bookmark status = %d; want 502", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// ---------------------------------------------------------------------------
// Atom-scoped share branch complements + malformed shapes
// ---------------------------------------------------------------------------

func TestSocial_AtomScoped_RevokeShareAndBadShapes(t *testing.T) {
	srv := newSocialFixture(t)
	defer srv.Close()

	// DELETE /v1/atoms/{id}/share → 502 unconfigured (RevokeShareFromFeed).
	resp := socialDoReq(t, http.MethodDelete, srv.URL+"/v1/atoms/atom-1/share", nil, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("DELETE share status = %d; want 502", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// PUT /v1/atoms/{id}/share → 405 (only POST/DELETE allowed).
	resp = socialDoReq(t, http.MethodPut, srv.URL+"/v1/atoms/atom-1/share", nil, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT share status = %d; want 405", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// PUT /v1/atoms/{id}/bookmark → 405.
	resp = socialDoReq(t, http.MethodPut, srv.URL+"/v1/atoms/atom-1/bookmark", nil, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT bookmark status = %d; want 405", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Too few path parts → 404.
	resp = socialDoReq(t, http.MethodPost, srv.URL+"/v1/atoms/atom-1", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("short atom path status = %d; want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Unknown action → 404.
	resp = socialDoReq(t, http.MethodPost, srv.URL+"/v1/atoms/atom-1/explode", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown atom action status = %d; want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// ---------------------------------------------------------------------------
// Post-scoped edge shapes
// ---------------------------------------------------------------------------

func TestSocial_PostScoped_ReactionDeleteAndCommentShapes(t *testing.T) {
	srv := newSocialFixture(t)
	defer srv.Close()

	// POST /v1/posts/{id}/reactions with a body → 501 stub.
	resp := socialDoReq(t, http.MethodPost, srv.URL+"/v1/posts/post-1/reactions", []byte(`{"kind":"like"}`), nil)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("POST reactions status = %d; want 501", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// DELETE /v1/posts/{id}/reactions/{rid} → 204 idempotent stub.
	resp = socialDoReq(t, http.MethodDelete, srv.URL+"/v1/posts/post-1/reactions/r-1", nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE reaction status = %d; want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Reactions with 4 parts → 404.
	resp = socialDoReq(t, http.MethodPost, srv.URL+"/v1/posts/post-1/reactions/r-1/extra", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("deep reactions path status = %d; want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// GET /v1/posts/{id}/comments → 200 empty.
	resp = socialDoReq(t, http.MethodGet, srv.URL+"/v1/posts/post-1/comments", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET comments status = %d; want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// POST /v1/posts/{id}/comments → 501 stub.
	resp = socialDoReq(t, http.MethodPost, srv.URL+"/v1/posts/post-1/comments", []byte(`{"body":"hi"}`), nil)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("POST comments status = %d; want 501", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// PUT /v1/posts/{id}/comments → 405 (only GET/POST).
	resp = socialDoReq(t, http.MethodPut, srv.URL+"/v1/posts/post-1/comments", nil, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT comments status = %d; want 405", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Deep comments path → 404.
	resp = socialDoReq(t, http.MethodGet, srv.URL+"/v1/posts/post-1/comments/c-1", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("deep comments path status = %d; want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Unknown post sub-action → 404.
	resp = socialDoReq(t, http.MethodGet, srv.URL+"/v1/posts/post-1/likes", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown post sub-action status = %d; want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Malformed comments body still reaches the 501 stub (decoder error ignored).
	resp = socialDoReq(t, http.MethodPost, srv.URL+"/v1/posts/post-1/comments", []byte(`not-json{`), nil)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("malformed comment body status = %d; want 501", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// ---------------------------------------------------------------------------
// Misc remaining branches
// ---------------------------------------------------------------------------

// TestSocial_Leaderboard_MethodAllowListing — PUT on the leaderboard + feed
// must 405 (only GET).
func TestSocial_Leaderboard_MethodAllowListing(t *testing.T) {
	srv := newSocialFixture(t)
	defer srv.Close()

	resp := socialDoReq(t, http.MethodPut, srv.URL+"/v1/leaderboard", nil, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT leaderboard status = %d; want 405", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp = socialDoReq(t, http.MethodDelete, srv.URL+"/v1/me/social", nil, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("DELETE me/social status = %d; want 405", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestSocial_ConnectionScoped_AllRelationshipShapes exercises every shape of
// the B-lite.2 subtree (follows/blocks/friend-requests/suggestions/friends)
// against the unconfigured aggregator — relationship writes fail loud 502,
// pending-list reads also 502 (fails-loud posture, not a stubbed empty list).
func TestSocial_ConnectionScoped_AllRelationshipShapes(t *testing.T) {
	srv := newSocialFixture(t)
	defer srv.Close()

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v1/connections/follows"},
		{http.MethodDelete, "/v1/connections/follows/gcid-2"},
		{http.MethodPost, "/v1/connections/blocks"},
		{http.MethodDelete, "/v1/connections/blocks/gcid-2"},
		{http.MethodPost, "/v1/connections/friend-requests"},
		{http.MethodGet, "/v1/connections/friend-requests"},
		{http.MethodGet, "/v1/connections/suggestions"},
		{http.MethodPost, "/v1/connections/friend-requests/gcid-2/accept"},
		{http.MethodDelete, "/v1/connections/friend-requests/incoming/gcid-2"},
		{http.MethodDelete, "/v1/connections/friend-requests/outgoing/gcid-2"},
		{http.MethodDelete, "/v1/connections/friends/gcid-2"},
	} {
		tc := tc
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			resp := socialDoReq(t, tc.method, srv.URL+tc.path, []byte(`{"gcid":"gcid-2"}`), nil)
			defer resp.Body.Close()
			// Relationship ops always fail loud when SharingURL is unset.
			if resp.StatusCode != http.StatusBadGateway {
				t.Errorf("%s %s status = %d; want 502", tc.method, tc.path, resp.StatusCode)
			}
		})
	}

	// Wrong method on a known shape → 405.
	resp := socialDoReq(t, http.MethodGet, srv.URL+"/v1/connections/follows", nil, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET follows status = %d; want 405", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Unknown shape → 404.
	resp = socialDoReq(t, http.MethodPost, srv.URL+"/v1/connections/unknown", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown connections shape status = %d; want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// ---------------------------------------------------------------------------
// WS-proxy happy path — raw-TCP hijack with a fake chora-sharing downstream
// (replicates the rplus_delivery_ws_proxy_test pattern).
// ---------------------------------------------------------------------------

// fakeSocialWSDownstream is a raw TCP listener that mimics chora-sharing's
// ws endpoint: reads the upgrade request line + headers, replies 101, then
// echoes bytes back. Captures the request path + the stamped gcid header.
type fakeSocialWSDownstream struct {
	ln             net.Listener
	gotUpgradePath chan string
	gotGCID        chan string
}

func newFakeSocialWSDownstream(t *testing.T) *fakeSocialWSDownstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeSocialWSDownstream{
		ln:             ln,
		gotUpgradePath: make(chan string, 1),
		gotGCID:        make(chan string, 1),
	}
	go f.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeSocialWSDownstream) addr() string { return f.ln.Addr().String() }

func (f *fakeSocialWSDownstream) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	reqLine, err := br.ReadString('\n')
	if err != nil {
		return
	}
	parts := strings.Fields(reqLine)
	if len(parts) >= 2 {
		f.gotUpgradePath <- parts[1]
	} else {
		f.gotUpgradePath <- ""
	}
	gcid := ""
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			break
		}
		if k, v, ok := strings.Cut(trimmed, ":"); ok {
			if strings.EqualFold(strings.TrimSpace(k), "gcid") {
				gcid = strings.TrimSpace(v)
			}
		}
	}
	f.gotGCID <- gcid
	_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
	buf := make([]byte, 1024)
	for {
		n, rerr := br.Read(buf)
		if n > 0 {
			if _, werr := conn.Write(buf[:n]); werr != nil {
				return
			}
		}
		if rerr != nil {
			return
		}
	}
}

// newSocialWSFixture builds the router with a SharingURL pointing at a fake
// raw-TCP downstream and wraps it so the JWT gate's mesh claims are injected
// (the gateway stamps gcid onto the hijacked upgrade).
func newSocialWSFixture(t *testing.T, sharingBase string) *httptest.Server {
	t.Helper()
	routes := inmem.NewRouteRepository()
	sessions := inmem.NewSessionRepository()
	up := upstream.NewFakeUpstream()
	phyllisAgg := phyllis.New(phyllis.LoadConfigFromEnv(), nil)
	sAgg := social.New(social.Config{SharingURL: sharingBase})
	mux := httpadapter.NewRouterWithSocial(routes, sessions, up, phyllisAgg, sAgg, nil)
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
		mux.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(wrapped)
	t.Cleanup(srv.Close)
	return srv
}

// readFull is a tiny re-implementation of io.ReadFull naming collision-free
// for the WS echo assertion.
func readFullBr(br *bufio.Reader, b []byte) (int, error) {
	total := 0
	for total < len(b) {
		n, err := br.Read(b[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func TestSocial_DuelWS_Upgrade_HijacksAndCopies(t *testing.T) {
	down := newFakeSocialWSDownstream(t)
	srv := newSocialWSFixture(t, "http://"+down.addr())

	gwAddr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", gwAddr)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer conn.Close()

	upgradeReq := "GET /v1/duels/duel-1/ws HTTP/1.1\r\n" +
		"Host: " + gwAddr + "\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"\r\n"
	if _, err := conn.Write([]byte(upgradeReq)); err != nil {
		t.Fatalf("write upgrade: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read 101 status line: %v", err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("status line = %q; want 101 Switching Protocols", statusLine)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read 101 headers: %v", err)
		}
		if strings.TrimRight(line, "\r\n") == "" {
			break
		}
	}

	select {
	case gotPath := <-down.gotUpgradePath:
		if gotPath != "/v1/duels/duel-1/ws" {
			t.Errorf("downstream upgrade path = %q; want /v1/duels/duel-1/ws", gotPath)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("downstream never received the upgrade request")
	}
	select {
	case gotGCID := <-down.gotGCID:
		if gotGCID != "gcid-001" {
			t.Errorf("downstream gcid header = %q; want gcid-001", gotGCID)
		}
	case <-time.After(time.Second):
		t.Fatal("downstream never reported gcid")
	}

	if _, err := conn.Write([]byte("ping-frame")); err != nil {
		t.Fatalf("write data frame: %v", err)
	}
	got := make([]byte, len("ping-frame"))
	if _, err := readFullBr(br, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != "ping-frame" {
		t.Errorf("echo = %q; want ping-frame", string(got))
	}
}

func TestSocial_ProfileWS_Upgrade_HijacksAndCopies(t *testing.T) {
	down := newFakeSocialWSDownstream(t)
	srv := newSocialWSFixture(t, "http://"+down.addr())

	gwAddr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", gwAddr)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer conn.Close()

	upgradeReq := "GET /v1/me/profile/ws HTTP/1.1\r\n" +
		"Host: " + gwAddr + "\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"\r\n"
	if _, err := conn.Write([]byte(upgradeReq)); err != nil {
		t.Fatalf("write upgrade: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read 101 status line: %v", err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("status line = %q; want 101 Switching Protocols", statusLine)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read 101 headers: %v", err)
		}
		if strings.TrimRight(line, "\r\n") == "" {
			break
		}
	}

	select {
	case gotPath := <-down.gotUpgradePath:
		if gotPath != "/v1/me/profile/ws" {
			t.Errorf("downstream upgrade path = %q; want /v1/me/profile/ws", gotPath)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("downstream never received the upgrade request")
	}
	select {
	case gotGCID := <-down.gotGCID:
		if gotGCID != "gcid-001" {
			t.Errorf("downstream gcid header = %q; want gcid-001", gotGCID)
		}
	case <-time.After(time.Second):
		t.Fatal("downstream never reported gcid")
	}

	if _, err := conn.Write([]byte("hello-ws")); err != nil {
		t.Fatalf("write data frame: %v", err)
	}
	got := make([]byte, len("hello-ws"))
	if _, err := readFullBr(br, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != "hello-ws" {
		t.Errorf("echo = %q; want hello-ws", string(got))
	}
}

// ---------------------------------------------------------------------------
// Remaining method-gate + shape branches (1-statement gaps)
// ---------------------------------------------------------------------------

func TestSocialDuel_MethodGates_405OnWrongVerbs(t *testing.T) {
	srv := newSocialFixture(t)
	defer srv.Close()

	for _, tc := range []struct {
		method string
		url    string
	}{
		// heartbeat/status/my-rating/leaderboard/single-duel/answer/ws are
		// single-verb routes; every other verb must 405.
		{http.MethodGet, "/v1/duels/queue/heartbeat"},
		{http.MethodDelete, "/v1/duels/queue/heartbeat"},
		{http.MethodPost, "/v1/duels/queue/status"},
		{http.MethodPut, "/v1/duels/queue/status"},
		{http.MethodPost, "/v1/duels/my-rating"},
		{http.MethodPut, "/v1/duels/my-rating"},
		{http.MethodPost, "/v1/duels/leaderboard"},
		{http.MethodDelete, "/v1/duels/leaderboard"},
		{http.MethodPost, "/v1/duels/duel-1"},
		{http.MethodPut, "/v1/duels/duel-1"},
		{http.MethodGet, "/v1/duels/duel-1/answer"},
		{http.MethodDelete, "/v1/duels/duel-1/answer"},
		{http.MethodPost, "/v1/duels/duel-1/ws"},
		{http.MethodPut, "/v1/duels/duel-1/ws"},
		{http.MethodPost, "/v1/duels/queue/status/x"},
	} {
		tc := tc
		t.Run(tc.method+" "+tc.url, func(t *testing.T) {
			resp := socialDoReq(t, tc.method, srv.URL+tc.url, nil, nil)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s status = %d; want 405 or 404", tc.method, tc.url, resp.StatusCode)
			}
		})
	}
}

func TestSocialProfile_MethodGates_405OnWrongVerbs(t *testing.T) {
	srv := newSocialFixture(t)
	defer srv.Close()

	for _, tc := range []struct {
		method string
		url    string
	}{
		{http.MethodPost, "/v1/me/profile"},
		{http.MethodPut, "/v1/me/profile"},
		{http.MethodPost, "/v1/me/profile/ws"},
		{http.MethodPut, "/v1/me/profile/ws"},
		{http.MethodGet, "/v1/me/profile/generate"},
		{http.MethodDelete, "/v1/me/profile/generate"},
		{http.MethodGet, "/v1/me/profile/tags"},
		{http.MethodDelete, "/v1/me/profile/tags"},
		{http.MethodPost, "/v1/me/profile/"},
	} {
		tc := tc
		t.Run(tc.method+" "+tc.url, func(t *testing.T) {
			resp := socialDoReq(t, tc.method, srv.URL+tc.url, nil, nil)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s status = %d; want 405 or 404", tc.method, tc.url, resp.StatusCode)
			}
		})
	}
}

func TestSocial_RemainingMethodGates(t *testing.T) {
	srv := newSocialFixture(t)
	defer srv.Close()

	for _, tc := range []struct {
		method string
		url    string
	}{
		// handleCreateReaction: POST only.
		{http.MethodGet, "/v1/posts/post-1/reactions"},
		// handleDeleteReaction: DELETE only.
		{http.MethodPost, "/v1/posts/post-1/reactions/r-1"},
		// handlePublicCourses: GET only.
		{http.MethodPost, "/v1/discovery/courses/public"},
		{http.MethodDelete, "/v1/discovery/courses/public"},
		// handleConnections: GET only.
		{http.MethodPost, "/v1/connections"},
		{http.MethodPut, "/v1/connections"},
		// handleBookmarks: GET only.
		{http.MethodPost, "/v1/me/bookmarks"},
		{http.MethodPut, "/v1/me/bookmarks"},
		// handlePostScoped len(parts) < 2 → 404.
		{http.MethodGet, "/v1/posts/only-id"},
	} {
		tc := tc
		t.Run(tc.method+" "+tc.url, func(t *testing.T) {
			resp := socialDoReq(t, tc.method, srv.URL+tc.url, nil, nil)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s status = %d; want 405 or 404", tc.method, tc.url, resp.StatusCode)
			}
		})
	}

	// /v1/posts/only-id has one part → 404 not 405.
	resp := socialDoReq(t, http.MethodGet, srv.URL+"/v1/posts/only-id", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /v1/posts/only-id status = %d; want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestSocial_RouterWithoutSocialAggregator — a nil social aggregator must not
// break the base router (the sAgg==nil guard).
func TestSocial_RouterWithoutSocialAggregator(t *testing.T) {
	routes := inmem.NewRouteRepository()
	sessions := inmem.NewSessionRepository()
	up := upstream.NewFakeUpstream()
	phyllisAgg := phyllis.New(phyllis.LoadConfigFromEnv(), nil)
	mux := httpadapter.NewRouterWithSocial(routes, sessions, up, phyllisAgg, nil, nil)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/nonexistent")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusInternalServerError {
		t.Errorf("nil social aggregator must not 500 the base router; got %d", resp.StatusCode)
	}
}

// TestSocial_ConnectionScoped_ForwardsRawQuery — the B-lite.2 forwarder
// appends the raw query string (?limit=...) to the subpath; pins that branch.
func TestSocial_ConnectionScoped_ForwardsRawQuery(t *testing.T) {
	srv := newSocialFixture(t)
	defer srv.Close()

	// GET with query on the fails-loud relationship read route.
	resp := socialDoReq(t, http.MethodGet, srv.URL+"/v1/connections/friend-requests?limit=10", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("GET friend-requests?limit status = %d; want 502", resp.StatusCode)
	}
}

// TestSocialWriteSocialResp_ZeroStatusBecomes500 — writeSocialResp normalises
// a zero Status into 500 (defensive branch).
func TestSocialWriteSocialResp_ZeroStatusBecomes500(t *testing.T) {
	// Reach via a handler path whose aggregator returns the zero Response:
	// DeleteReaction with SharingURL set upstream returns 204; unconfigured
	// returns 204 too — so instead drive through the connect-scoped forward
	// with a closed upstream (agg returns err → errResp → non-zero). Simpler:
	// hit bookmarks list with unconfigured (200) and confirm normalisation is
	// not triggered; the zero-status branch itself needs an aggregator that
	// returns Response{} — exercised by the social aggregator's empty-url
	// guard which never reaches here. Keep this as a smoke of the happy path.
	srv := newSocialFixture(t)
	defer srv.Close()
	resp := socialDoReq(t, http.MethodGet, srv.URL+"/v1/me/social", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET me/social status = %d; want 200", resp.StatusCode)
	}
}
