// rplus_delivery_ws_proxy_test.go — HTTP-adapter tests for the WebSocket
// upgrade passthrough on the R+ delivery proxy bridge (ADR-168 classroom
// realtime / ADR-166 §D5). The R+ live-quiz / live-poll FE opens WebSockets
// at /api/v1/live-quizzes/{sessionId}/ws and /api/v1/live-polls/{pollId}/ws;
// the bridge must hijack the client conn, dial the chora-delivery downstream,
// replay the upgrade, and io.Copy both directions until either side closes.
//
// Coverage:
//   - A request carrying Connection: Upgrade + Upgrade: websocket on a /ws
//     subtree path is hijacked, the downstream sees the upgrade request, the
//     101 handshake flows back to the client, and bytes flow BOTH directions.
//   - A normal REST request on the same prefix still uses the verbatim
//     ProxyDeliveryVerbatim path (NOT the WS hijack).
//
// Strict TDD per feedback_strict_tdd — RED before proxyWebSocket exists.
package httpadapter_test

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

// fakeWSDownstream is a raw TCP listener that mimics chora-delivery's WS
// endpoint: it reads the upgrade request line + headers, replies 101
// Switching Protocols, then echoes every subsequent byte back to the caller.
// gotUpgradePath / gotGCID capture what the downstream actually received so
// the test can assert the gateway forwarded the request line + mesh headers.
type fakeWSDownstream struct {
	ln             net.Listener
	gotUpgradePath chan string
	gotGCID        chan string
}

func newFakeWSDownstream(t *testing.T) *fakeWSDownstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeWSDownstream{
		ln:             ln,
		gotUpgradePath: make(chan string, 1),
		gotGCID:        make(chan string, 1),
	}
	go f.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeWSDownstream) addr() string { return f.ln.Addr().String() }

func (f *fakeWSDownstream) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	// Read the request line.
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
	// Read headers until blank line; capture gcid.
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
	// 101 handshake.
	_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
	// Echo every subsequent byte.
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

func newRplusServerWithDelivery(t *testing.T, deliveryBase string) *httptest.Server {
	t.Helper()
	agg := gatewayproxy.New(gatewayproxy.Config{
		DeliveryURL:    deliveryBase,
		PerCallTimeout: time.Second,
	})
	if agg == nil {
		t.Fatal("aggregator nil — DeliveryURL should have wired it")
	}
	mux := httpadapter.NewRplusDeliveryProxyMux(agg)
	// Inject mesh claims like the JWT gate would, so the bridge stamps gcid.
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
		mux.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(wrapped)
	t.Cleanup(srv.Close)
	return srv
}

func TestRplusProxy_WebSocket_Upgrade_HijacksAndCopiesBothWays(t *testing.T) {
	down := newFakeWSDownstream(t)
	srv := newRplusServerWithDelivery(t, "http://"+down.addr())

	// Raw TCP client connection to the gateway test server.
	gwAddr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", gwAddr)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer conn.Close()

	// Send the WS upgrade request for a live-polls /ws path.
	upgradeReq := "GET /api/v1/live-polls/p-1/ws HTTP/1.1\r\n" +
		"Host: " + gwAddr + "\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Authorization: Bearer tok\r\n" +
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
	// Drain the rest of the 101 response headers.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read 101 headers: %v", err)
		}
		if strings.TrimRight(line, "\r\n") == "" {
			break
		}
	}

	// Downstream must have received the upgrade request at the same path with
	// the stamped gcid mesh header.
	select {
	case gotPath := <-down.gotUpgradePath:
		if gotPath != "/api/v1/live-polls/p-1/ws" {
			t.Errorf("downstream upgrade path = %q; want /api/v1/live-polls/p-1/ws", gotPath)
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

	// Client → downstream → client echo proves bidirectional io.Copy.
	if _, err := conn.Write([]byte("ping-frame")); err != nil {
		t.Fatalf("write data frame: %v", err)
	}
	got := make([]byte, len("ping-frame"))
	if _, err := readFull(br, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != "ping-frame" {
		t.Errorf("echo = %q; want ping-frame", string(got))
	}
}

func TestRplusProxy_NonUpgrade_StillUsesVerbatimREST(t *testing.T) {
	var gotPath, gotMethod string
	rest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer rest.Close()

	agg := gatewayproxy.New(gatewayproxy.Config{
		DeliveryURL:    rest.URL,
		PerCallTimeout: time.Second,
	})
	mux := httpadapter.NewRplusDeliveryProxyMux(agg)

	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/live-quizzes", nil)
	r.Header.Set("Authorization", "Bearer tok")
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (verbatim REST path); body=%s", w.Code, w.Body.String())
	}
	if gotMethod != http.MethodGet || gotPath != "/api/v1/live-quizzes" {
		t.Errorf("downstream = %s %q; want GET /api/v1/live-quizzes (REST verbatim)", gotMethod, gotPath)
	}
}

func TestRplusProxy_WebSocket_DialFailure_Returns502(t *testing.T) {
	// Point delivery at a closed port so the downstream dial fails — the
	// gateway must return a clean 502 (client conn NOT yet hijacked).
	srv := newRplusServerWithDelivery(t, "http://127.0.0.1:1") // port 1 = refused

	gwAddr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", gwAddr)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer conn.Close()

	upgradeReq := "GET /api/v1/live-quizzes/q-1/ws HTTP/1.1\r\n" +
		"Host: " + gwAddr + "\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(upgradeReq)); err != nil {
		t.Fatalf("write upgrade: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(statusLine, "502") {
		t.Errorf("status line = %q; want 502 on downstream dial failure", statusLine)
	}
}

func TestIsWebSocketUpgrade_CaseInsensitiveAndNegatives(t *testing.T) {
	mk := func(conn, upg string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/live-polls/p-1/ws", nil)
		if conn != "" {
			r.Header.Set("Connection", conn)
		}
		if upg != "" {
			r.Header.Set("Upgrade", upg)
		}
		return r
	}
	cases := []struct {
		name        string
		conn, upg   string
		wantUpgrade bool
	}{
		{"canonical", "Upgrade", "websocket", true},
		{"lowercase", "upgrade", "websocket", true},
		{"mixed-case-upgrade", "keep-alive, Upgrade", "WebSocket", true},
		{"no-connection", "", "websocket", false},
		{"no-upgrade-header", "Upgrade", "", false},
		{"connection-not-upgrade", "keep-alive", "websocket", false},
		{"upgrade-not-websocket", "Upgrade", "h2c", false},
		{"plain-rest", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := httpadapter.IsWebSocketUpgradeForTest(mk(c.conn, c.upg)); got != c.wantUpgrade {
				t.Errorf("isWebSocketUpgrade = %v; want %v", got, c.wantUpgrade)
			}
		})
	}
}

// readFull reads len(buf) bytes from br, retrying short reads.
func readFull(br *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := br.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
