package httpadapter

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStripeWebhookPassthrough_ForwardsBodyVerbatim(t *testing.T) {
	t.Parallel()
	var (
		gotBody   string
		gotSig    string
		gotMethod string
		gotPath   string
	)
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotSig = r.Header.Get("Stripe-Signature")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"received":true}`))
	}))
	defer downstream.Close()

	h, err := newStripeWebhookPassthrough(StripeWebhookPassthroughConfig{PaymentsHTTPAddr: downstream.URL})
	if err != nil {
		t.Fatalf("newStripeWebhookPassthrough: %v", err)
	}

	rawBody := `{"id":"evt_test_xxx","type":"checkout.session.completed","data":{"object":{}}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/stripe", strings.NewReader(rawBody))
	req.Header.Set("Stripe-Signature", "t=1234567890,v1=abcdef")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	if gotMethod != http.MethodPost {
		t.Errorf("downstream method=%s, want POST", gotMethod)
	}
	if gotPath != "/v1/webhooks/stripe" {
		t.Errorf("downstream path=%s, want /v1/webhooks/stripe", gotPath)
	}
	if gotSig != "t=1234567890,v1=abcdef" {
		t.Errorf("downstream Stripe-Signature=%q, want verbatim", gotSig)
	}
	if gotBody != rawBody {
		t.Errorf("downstream body mismatch:\n got: %q\nwant: %q", gotBody, rawBody)
	}
	if !strings.Contains(w.Body.String(), "received") {
		t.Errorf("body forwarding broken: %s", w.Body.String())
	}
}

func TestStripeWebhookPassthrough_ForwardsDownstream400(t *testing.T) {
	t.Parallel()
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"signature verification failed"}`))
	}))
	defer downstream.Close()
	h, err := newStripeWebhookPassthrough(StripeWebhookPassthroughConfig{PaymentsHTTPAddr: downstream.URL})
	if err != nil {
		t.Fatalf("newStripeWebhookPassthrough: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/stripe", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 (verbatim from downstream)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "signature verification failed") {
		t.Errorf("body not forwarded verbatim: %s", w.Body.String())
	}
}

func TestStripeWebhookPassthrough_RejectsNonPOST(t *testing.T) {
	t.Parallel()
	h, _ := newStripeWebhookPassthrough(StripeWebhookPassthroughConfig{PaymentsHTTPAddr: "http://example.invalid"})
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/v1/webhooks/stripe", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("method=%s status=%d, want 405", method, w.Code)
		}
	}
}

func TestStripeWebhookPassthrough_DownstreamTimeout(t *testing.T) {
	t.Parallel()
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer downstream.Close()
	h, err := newStripeWebhookPassthrough(StripeWebhookPassthroughConfig{
		PaymentsHTTPAddr: downstream.URL,
		Timeout:          50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newStripeWebhookPassthrough: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/stripe", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Errorf("status=%d, want 502 on downstream timeout", w.Code)
	}
}

func TestNewStripeWebhookPassthrough_RequiresAddr(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, addr string }{
		{"empty", ""},
		{"whitespace", "  "},
		{"no-scheme", "chora-payments.payments.svc.cluster.local:8080"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newStripeWebhookPassthrough(StripeWebhookPassthroughConfig{PaymentsHTTPAddr: tc.addr})
			if err == nil {
				t.Fatalf("expected error for addr=%q", tc.addr)
			}
		})
	}
}

func TestWithStripeWebhookPassthrough_PassthroughOwnsWebhookPath(t *testing.T) {
	t.Parallel()
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"received":true}`))
	}))
	defer downstream.Close()

	baseHits := 0
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		baseHits++
		w.WriteHeader(http.StatusTeapot)
	})

	composed := WithStripeWebhookPassthrough(base, StripeWebhookPassthroughConfig{PaymentsHTTPAddr: downstream.URL})

	// Webhook path goes to passthrough.
	webhookReq := httptest.NewRequest(http.MethodPost, "/v1/webhooks/stripe", strings.NewReader(`{}`))
	webhookW := httptest.NewRecorder()
	composed.ServeHTTP(webhookW, webhookReq)
	if webhookW.Code != http.StatusOK {
		t.Errorf("webhook path: status=%d, want 200 (downstream)", webhookW.Code)
	}
	if baseHits != 0 {
		t.Errorf("webhook path leaked through to base handler")
	}

	// Other paths fall through to base.
	otherReq := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	otherW := httptest.NewRecorder()
	composed.ServeHTTP(otherW, otherReq)
	if otherW.Code != http.StatusTeapot {
		t.Errorf("non-webhook path: status=%d, want 418 (base)", otherW.Code)
	}
	if baseHits != 1 {
		t.Errorf("base hits=%d, want 1", baseHits)
	}
}

func TestWithStripeWebhookPassthrough_DisabledWhenAddrEmpty(t *testing.T) {
	t.Parallel()
	baseHits := 0
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		baseHits++
		w.WriteHeader(http.StatusTeapot)
	})
	composed := WithStripeWebhookPassthrough(base, StripeWebhookPassthroughConfig{PaymentsHTTPAddr: ""})
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/stripe", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	composed.ServeHTTP(w, req)
	if w.Code != http.StatusTeapot {
		t.Errorf("disabled passthrough should fall through; status=%d, want 418", w.Code)
	}
	if baseHits != 1 {
		t.Errorf("base hits=%d, want 1", baseHits)
	}
}
