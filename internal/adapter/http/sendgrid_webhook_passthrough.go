// sendgrid_webhook_passthrough.go — verbatim POST /webhooks/sendgrid/events →
// chora-notifications.
//
// Per ADR-171 (notification fan-out) + the email channel (CHO-1634):
// SendGrid's Signed Event Webhook POSTs delivered/bounced/dropped events to
// `https://api.chora.site/webhooks/sendgrid/events` → GCLB → Cloud Armor
// (priority-103 allow rule) → chora-gateway BFF → THIS passthrough →
// chora-notifications HTTP listener at
// `chora-notifications.notifications.svc.cluster.local:8080/webhooks/sendgrid/events`.
//
// CRITICAL invariants (mirror the Stripe passthrough):
//  1. NO auth — SendGrid does not carry a Chora JWT. chora-notifications does
//     its OWN ECDSA P-256 signature verification using the
//     X-Twilio-Email-Event-Webhook-Signature + -Timestamp headers against the
//     SendGrid Signed-Event-Webhook public key. The BFF MUST NOT JWT-gate it.
//  2. Body MUST be forwarded BYTE-IDENTICAL — SendGrid signs over
//     (timestamp || rawBody); any whitespace / encoding rewrite breaks
//     signature verification downstream.
//  3. The two X-Twilio-Email-Event-Webhook-* headers MUST be preserved
//     verbatim (carried by the verbatim header copy below).
//  4. No tenant_id / gcid stamping — chora-notifications resolves the tenant
//     from each event's custom_args internally.
//
// The composition pattern mirrors WithStripeWebhookPassthrough: an outer-bridge
// handler that owns its exact path and falls through to the base router for
// everything else.
//
// Per `feedback_no_inline_config` the downstream URL comes from env at boot
// time (CHORA_NOTIFICATIONS_HTTP_ADDR, falling back to the already-wired
// SVC_NOTIFICATIONS_URL — both point at chora-notifications).
package httpadapter

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SendGridWebhookPath is the canonical SendGrid Event Webhook receive path.
// It MUST match chora-notifications' httpadapter.WebhookSendgridPath().
const SendGridWebhookPath = "/webhooks/sendgrid/events"

// DefaultSendGridWebhookTimeout caps the passthrough. SendGrid's Event Webhook
// tolerates a slower response than Stripe (it retries non-2xx with backoff over
// ~24h) and the downstream verifies + durably outbox-enqueues a whole event
// batch, so we allow a more generous budget than the Stripe path.
const DefaultSendGridWebhookTimeout = 10 * time.Second

// SendGridWebhookPassthroughConfig wires the passthrough.
type SendGridWebhookPassthroughConfig struct {
	// NotificationsHTTPAddr is the chora-notifications HTTP base URL (e.g.
	// "http://chora-notifications.notifications.svc.cluster.local:8080").
	// Required.
	NotificationsHTTPAddr string

	// Timeout caps the downstream call. Defaults to
	// DefaultSendGridWebhookTimeout.
	Timeout time.Duration

	// Client lets tests inject a stub. Defaults to a fresh *http.Client with
	// Timeout applied.
	Client *http.Client
}

// sendGridWebhookPassthrough is the handler.
type sendGridWebhookPassthrough struct {
	downstreamURL string
	timeout       time.Duration
	client        *http.Client
}

// newSendGridWebhookPassthrough constructs the passthrough. Returns
// (nil, error) when NotificationsHTTPAddr is empty or invalid.
func newSendGridWebhookPassthrough(cfg SendGridWebhookPassthroughConfig) (*sendGridWebhookPassthrough, error) {
	if strings.TrimSpace(cfg.NotificationsHTTPAddr) == "" {
		return nil, errors.New("sendgrid-webhook-passthrough: NotificationsHTTPAddr required")
	}
	parsed, err := url.Parse(cfg.NotificationsHTTPAddr)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("sendgrid-webhook-passthrough: NotificationsHTTPAddr must include scheme + host")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultSendGridWebhookTimeout
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: timeout + time.Second}
	}
	return &sendGridWebhookPassthrough{
		downstreamURL: strings.TrimRight(cfg.NotificationsHTTPAddr, "/") + SendGridWebhookPath,
		timeout:       timeout,
		client:        client,
	}, nil
}

// ServeHTTP forwards the request verbatim. Body bytes are read into memory
// (SendGrid batches are small; well within budget) so they can be
// signed-by-the-downstream + survive http.NewRequestWithContext's fresh
// io.Reader requirement.
func (h *sendGridWebhookPassthrough) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method_not_allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("gateway: sendgrid webhook passthrough read body: %v", err)
		http.Error(w, `{"error":"bad_request"}`, http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()

	out, err := http.NewRequestWithContext(ctx, http.MethodPost, h.downstreamURL, strings.NewReader(string(body)))
	if err != nil {
		log.Printf("gateway: sendgrid webhook passthrough build request: %v", err)
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}

	// Forward request headers VERBATIM — including the two
	// X-Twilio-Email-Event-Webhook-* headers (signature + timestamp),
	// Content-Type, Content-Length, User-Agent, etc. The ECDSA verification
	// downstream depends on this.
	for k, vs := range r.Header {
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}

	resp, err := h.client.Do(out)
	if err != nil {
		log.Printf("gateway: sendgrid webhook passthrough downstream call: %v", err)
		http.Error(w, `{"error":"bad_gateway"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Forward response headers VERBATIM (Content-Type primarily) + status +
	// body. Standard reverse-proxy shape.
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, copyErr := io.Copy(w, resp.Body); copyErr != nil {
		log.Printf("gateway: sendgrid webhook passthrough response copy: %v", copyErr)
	}
}

// WithSendGridWebhookPassthrough composes the passthrough with a base handler.
// POST /webhooks/sendgrid/events is owned by the passthrough; every other
// request falls through to `base`.
//
// Returns the base handler unchanged when cfg.NotificationsHTTPAddr is empty so
// cmd/server can opt out in unconfigured envs (matching
// WithStripeWebhookPassthrough / WithGatewayProxy).
func WithSendGridWebhookPassthrough(base http.Handler, cfg SendGridWebhookPassthroughConfig) http.Handler {
	if strings.TrimSpace(cfg.NotificationsHTTPAddr) == "" {
		return base
	}
	h, err := newSendGridWebhookPassthrough(cfg)
	if err != nil {
		log.Printf("gateway: sendgrid webhook passthrough disabled: %v", err)
		return base
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == SendGridWebhookPath {
			h.ServeHTTP(w, r)
			return
		}
		base.ServeHTTP(w, r)
	})
}
