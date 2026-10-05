// companionbridge_chat_test.go — ADR-154 conversational chat SSE pass-through
// proxy tests.
//
// The chat proxy differs from the other 9 PROD-D routes because SSE
// requires direct ResponseWriter access (the proxy forwards each engine
// frame to the FE the moment it arrives — no body buffering). Validation
// goals:
//
//  1. Content-Type=text/event-stream + Cache-Control=no-cache +
//     X-Accel-Buffering=no headers are forwarded verbatim.
//  2. SSE frames are forwarded in order, line-by-line, with the inter-frame
//     `\n\n` separator preserved.
//  3. Mesh-trust headers (Authorization, traceparent, X-Tenant-Id,
//     X-Chora-GCID, chora-gcid mesh-claims) are stamped outbound.
//  4. Inbound POST body (chat request JSON) is forwarded verbatim.
//  5. 5xx upstream → 502 JSON error (since we haven't yet committed headers).
//  6. 4xx upstream (e.g., 402 insufficient_mana) passes through verbatim
//     with the JSON body unchanged (FE renders the upsell envelope).
package companionbridge_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/companionbridge"
)

// -----------------------------------------------------------------------------
// 1. Happy path — SSE frames flow through with headers preserved
// -----------------------------------------------------------------------------

func TestChatStream_ForwardsSSEFramesAndHeaders(t *testing.T) {
	// Upstream emits 3 SSE frames separated by the canonical blank line.
	upstream := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: session_open\ndata: {\"engine_session_id\":\"s1\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "event: token\ndata: {\"text\":\"hi\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "event: turn_complete\ndata: {\"output_tokens\":2,\"model\":\"gemini-2.5-flash-lite\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	})

	cfg := companionbridge.Config{ConsumptionURL: upstream.URL}
	b := companionbridge.New(cfg)

	w := httptest.NewRecorder()
	body := strings.NewReader(`{"message":"hi"}`)
	err := b.ChatStream(t.Context(), basicAuth(),
		"01957c8c-1111-7000-aaaa-1111aaaa1111", body, w)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type = %q; want text/event-stream", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q; want no-cache", got)
	}
	if got := w.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q; want no", got)
	}

	bodyOut := w.Body.String()
	for _, want := range []string{
		"event: session_open",
		"event: token",
		"event: turn_complete",
		`"engine_session_id":"s1"`,
		`"text":"hi"`,
	} {
		if !strings.Contains(bodyOut, want) {
			t.Errorf("body missing %q; got=%q", want, bodyOut)
		}
	}

	// Upstream MUST have received the Authorization + tenant + GCID headers.
	if upstream.lastAuth != "Bearer fb-tok" {
		t.Errorf("upstream.Authorization = %q; want Bearer fb-tok", upstream.lastAuth)
	}
	if upstream.lastTenant != "tenant-001" {
		t.Errorf("upstream.X-Tenant-Id = %q; want tenant-001", upstream.lastTenant)
	}
	if upstream.lastGCID != "gcid-001" {
		t.Errorf("upstream.GCID = %q; want gcid-001", upstream.lastGCID)
	}
	if upstream.lastTP != "00-aaa-bbb-01" {
		t.Errorf("upstream.traceparent = %q; want 00-aaa-bbb-01", upstream.lastTP)
	}
	if !bytes.Contains(upstream.lastBody, []byte(`"message":"hi"`)) {
		t.Errorf("upstream.body = %q; want forwarded chat request", string(upstream.lastBody))
	}
}

// -----------------------------------------------------------------------------
// 2. Path mapping — /api/v1/me/companions/{id}/chat → /v1/me/companions/{id}/chat
// -----------------------------------------------------------------------------

func TestChatStream_PathMappingStripsApiV1Prefix(t *testing.T) {
	upstream := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	})

	cfg := companionbridge.Config{ConsumptionURL: upstream.URL}
	b := companionbridge.New(cfg)
	w := httptest.NewRecorder()
	_ = b.ChatStream(t.Context(), basicAuth(),
		"01957c8c-1111-7000-aaaa-1111aaaa1111", strings.NewReader(`{"message":"x"}`), w)

	wantPath := "/v1/me/companions/01957c8c-1111-7000-aaaa-1111aaaa1111/chat"
	if upstream.lastPath != wantPath {
		t.Errorf("upstream.path = %q; want %q", upstream.lastPath, wantPath)
	}
}

// -----------------------------------------------------------------------------
// 3. 4xx pass-through — 402 insufficient_mana JSON body preserved verbatim
// -----------------------------------------------------------------------------

func TestChatStream_4xxPassesThroughVerbatim(t *testing.T) {
	upstreamBody := `{"error":{"code":"insufficient_mana","upsell":{"required_units":5,"current_balance_units":0}}}`
	upstream := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, upstreamBody)
	})

	cfg := companionbridge.Config{ConsumptionURL: upstream.URL}
	b := companionbridge.New(cfg)
	w := httptest.NewRecorder()
	err := b.ChatStream(t.Context(), basicAuth(),
		"01957c8c-1111-7000-aaaa-1111aaaa1111", strings.NewReader(`{"message":"x"}`), w)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	if w.Code != http.StatusPaymentRequired {
		t.Errorf("status = %d; want 402", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"insufficient_mana"`) {
		t.Errorf("body missing upsell envelope; got=%q", w.Body.String())
	}
}

// -----------------------------------------------------------------------------
// 4. 5xx upstream → 502 JSON gateway error
// -----------------------------------------------------------------------------

func TestChatStream_5xxUpstream_Returns502(t *testing.T) {
	upstream := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `<html>upstream boom</html>`)
	})

	cfg := companionbridge.Config{ConsumptionURL: upstream.URL}
	b := companionbridge.New(cfg)
	w := httptest.NewRecorder()
	err := b.ChatStream(t.Context(), basicAuth(),
		"fam", strings.NewReader(`{"message":"x"}`), w)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", w.Code)
	}
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q; want application/json on 5xx → 502 normalisation", got)
	}
	if !strings.Contains(w.Body.String(), "GATEWAY_UPSTREAM_5XX") {
		t.Errorf("body missing GATEWAY_UPSTREAM_5XX code; got=%q", w.Body.String())
	}
}

// -----------------------------------------------------------------------------
// 5. Timeout → 504
// -----------------------------------------------------------------------------

func TestChatStream_Timeout_Returns504(t *testing.T) {
	upstream := newStub(t, func(_ http.ResponseWriter, _ *http.Request) {
		// Block longer than the test's per-call timeout.
		time.Sleep(500 * time.Millisecond)
	})

	cfg := companionbridge.Config{
		ConsumptionURL: upstream.URL,
		// Tight ChatTimeout so the test fires the 504 path quickly.
		ChatTimeout: 50 * time.Millisecond,
	}
	b := companionbridge.New(cfg)
	w := httptest.NewRecorder()
	err := b.ChatStream(t.Context(), basicAuth(),
		"fam", strings.NewReader(`{"message":"x"}`), w)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if w.Code != http.StatusGatewayTimeout {
		t.Errorf("status = %d; want 504 on upstream timeout", w.Code)
	}
}

// -----------------------------------------------------------------------------
// 6. Empty consumption URL → typed 502 with informative code
// -----------------------------------------------------------------------------

func TestChatStream_EmptyConsumptionURL_Returns502(t *testing.T) {
	cfg := companionbridge.Config{ConsumptionURL: ""}
	b := companionbridge.New(cfg)
	w := httptest.NewRecorder()
	err := b.ChatStream(t.Context(), basicAuth(),
		"fam", strings.NewReader(`{"message":"x"}`), w)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", w.Code)
	}
}
