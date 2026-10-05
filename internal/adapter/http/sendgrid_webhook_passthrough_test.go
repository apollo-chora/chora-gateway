package httpadapter

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSendGridWebhookPassthrough_ForwardsBodyAndSignatureHeadersVerbatim(t *testing.T) {
	t.Parallel()
	var (
		gotBody   string
		gotSig    string
		gotTS     string
		gotCT     string
		gotMethod string
		gotPath   string
	)
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotSig = r.Header.Get("X-Twilio-Email-Event-Webhook-Signature")
		gotTS = r.Header.Get("X-Twilio-Email-Event-Webhook-Timestamp")
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"received":true}`))
	}))
	defer downstream.Close()

	h, err := newSendGridWebhookPassthrough(SendGridWebhookPassthroughConfig{NotificationsHTTPAddr: downstream.URL})
	if err != nil {
		t.Fatalf("newSendGridWebhookPassthrough: %v", err)
	}

	// A realistic SendGrid Event Webhook batch — the ECDSA signature downstream
	// is computed over (timestamp || rawBody), so ANY byte rewrite breaks it.
	rawBody := `[{"email":"learner@example.com","event":"delivered","sg_message_id":"abc.def","timestamp":1780000000}]`
	req := httptest.NewRequest(http.MethodPost, "/webhooks/sendgrid/events", strings.NewReader(rawBody))
	req.Header.Set("X-Twilio-Email-Event-Webhook-Signature", "MEUCIQ...verbatim==")
	req.Header.Set("X-Twilio-Email-Event-Webhook-Timestamp", "1780000001")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	if gotMethod != http.MethodPost {
		t.Errorf("downstream method=%s, want POST", gotMethod)
	}
	if gotPath != "/webhooks/sendgrid/events" {
		t.Errorf("downstream path=%s, want /webhooks/sendgrid/events", gotPath)
	}
	if gotSig != "MEUCIQ...verbatim==" {
		t.Errorf("downstream signature header=%q, want verbatim", gotSig)
	}
	if gotTS != "1780000001" {
		t.Errorf("downstream timestamp header=%q, want verbatim", gotTS)
	}
	if gotCT != "application/json" {
		t.Errorf("downstream Content-Type=%q, want application/json", gotCT)
	}
	if gotBody != rawBody {
		t.Errorf("downstream body mismatch (signature would break):\n got: %q\nwant: %q", gotBody, rawBody)
	}
	if !strings.Contains(w.Body.String(), "received") {
		t.Errorf("response body forwarding broken: %s", w.Body.String())
	}
}

func TestSendGridWebhookPassthrough_ForwardsDownstream401(t *testing.T) {
	t.Parallel()
	// A forged-signature SendGrid event yields a 401 downstream; the gateway
	// must forward that status verbatim (it does NOT re-interpret the verdict).
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"signature verification failed"}`))
	}))
	defer downstream.Close()
	h, err := newSendGridWebhookPassthrough(SendGridWebhookPassthroughConfig{NotificationsHTTPAddr: downstream.URL})
	if err != nil {
		t.Fatalf("newSendGridWebhookPassthrough: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/sendgrid/events", strings.NewReader(`[]`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401 (verbatim from downstream)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "signature verification failed") {
		t.Errorf("body not forwarded verbatim: %s", w.Body.String())
	}
}

func TestSendGridWebhookPassthrough_RejectsNonPOST(t *testing.T) {
	t.Parallel()
	h, _ := newSendGridWebhookPassthrough(SendGridWebhookPassthroughConfig{NotificationsHTTPAddr: "http://example.invalid"})
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/webhooks/sendgrid/events", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("method=%s status=%d, want 405", method, w.Code)
		}
	}
}

func TestSendGridWebhookPassthrough_DownstreamTimeout(t *testing.T) {
	t.Parallel()
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer downstream.Close()
	h, err := newSendGridWebhookPassthrough(SendGridWebhookPassthroughConfig{
		NotificationsHTTPAddr: downstream.URL,
		Timeout:               50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newSendGridWebhookPassthrough: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/sendgrid/events", strings.NewReader(`[]`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Errorf("status=%d, want 502 on downstream timeout", w.Code)
	}
}

func TestNewSendGridWebhookPassthrough_RequiresAddr(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, addr string }{
		{"empty", ""},
		{"whitespace", "  "},
		{"no-scheme", "chora-notifications.notifications.svc.cluster.local:8080"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newSendGridWebhookPassthrough(SendGridWebhookPassthroughConfig{NotificationsHTTPAddr: tc.addr})
			if err == nil {
				t.Fatalf("expected error for addr=%q", tc.addr)
			}
		})
	}
}

func TestWithSendGridWebhookPassthrough_PassthroughOwnsWebhookPath(t *testing.T) {
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

	composed := WithSendGridWebhookPassthrough(base, SendGridWebhookPassthroughConfig{NotificationsHTTPAddr: downstream.URL})

	// Webhook path goes to passthrough (NOT the base/JWT-gated handler).
	webhookReq := httptest.NewRequest(http.MethodPost, "/webhooks/sendgrid/events", strings.NewReader(`[]`))
	webhookW := httptest.NewRecorder()
	composed.ServeHTTP(webhookW, webhookReq)
	if webhookW.Code != http.StatusOK {
		t.Errorf("webhook path: status=%d, want 200 (downstream)", webhookW.Code)
	}
	if baseHits != 0 {
		t.Errorf("webhook path leaked through to base handler (would JWT-gate the unauthenticated SendGrid callback)")
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

func TestWithSendGridWebhookPassthrough_DisabledWhenAddrEmpty(t *testing.T) {
	t.Parallel()
	baseHits := 0
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		baseHits++
		w.WriteHeader(http.StatusTeapot)
	})
	composed := WithSendGridWebhookPassthrough(base, SendGridWebhookPassthroughConfig{NotificationsHTTPAddr: ""})
	req := httptest.NewRequest(http.MethodPost, "/webhooks/sendgrid/events", strings.NewReader(`[]`))
	w := httptest.NewRecorder()
	composed.ServeHTTP(w, req)
	if w.Code != http.StatusTeapot {
		t.Errorf("disabled passthrough should fall through; status=%d, want 418", w.Code)
	}
	if baseHits != 1 {
		t.Errorf("base hits=%d, want 1", baseHits)
	}
}
