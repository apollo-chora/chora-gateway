// stripe_webhook_passthrough.go — verbatim POST /v1/webhooks/stripe → chora-payments.
//
// Per ADR-164 + handoff-from-infra-adr-164-stage-a3-close-2026-05-24.md §2:
// Stripe webhooks land at `https://api.chora.site/v1/webhooks/stripe` →
// GCLB → Cloud Armor (priority-102 allow rule) → chora-gateway BFF → THIS
// passthrough → chora-payments HTTP listener at
// `chora-payments.payments.svc.cluster.local:8080/v1/webhooks/stripe`.
//
// CRITICAL invariants:
//  1. NO auth — Stripe does not carry a JWT. chora-payments does its OWN
//     HMAC verification using the Stripe-Signature header + the shared
//     STRIPE_WEBHOOK_SECRET. The BFF MUST NOT block on missing JWT.
//  2. Body MUST be forwarded BYTE-IDENTICAL — Stripe's HMAC signs the raw
//     JSON payload; any whitespace / encoding rewrite breaks signature
//     verification on the downstream.
//  3. Stripe-Signature header MUST be preserved verbatim.
//  4. No tenant_id / gcid stamping — Stripe events carry their own
//     metadata.tenant_id which chora-payments parses internally.
//
// The composition pattern mirrors WithGatewayProxy / WithCompanionBridge:
// outer-bridge handler that owns its exact path and falls through to the
// base router for everything else.
//
// Per `feedback_no_inline_config` the downstream URL comes from env at
// boot time (CHORA_PAYMENTS_HTTP_ADDR, e.g.
// `http://chora-payments.payments.svc.cluster.local:8080`).
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

// StripeWebhookPath is the canonical webhook receive path.
const StripeWebhookPath = "/v1/webhooks/stripe"

// DefaultStripeWebhookTimeout caps the passthrough. Stripe's webhook
// delivery has a hard 5s response budget; we give the downstream ~4s
// to verify+dispatch, leaving ~1s for the BFF round-trip.
const DefaultStripeWebhookTimeout = 4500 * time.Millisecond

// StripeWebhookPassthroughConfig wires the passthrough.
type StripeWebhookPassthroughConfig struct {
	// PaymentsHTTPAddr is the chora-payments HTTP base URL (e.g.
	// "http://chora-payments.payments.svc.cluster.local:8080"). Required.
	PaymentsHTTPAddr string

	// Timeout caps the downstream call. Defaults to
	// DefaultStripeWebhookTimeout.
	Timeout time.Duration

	// Client lets tests inject a stub. Defaults to a fresh *http.Client
	// with Timeout applied.
	Client *http.Client
}

// stripeWebhookPassthrough is the handler.
type stripeWebhookPassthrough struct {
	downstreamURL string
	timeout       time.Duration
	client        *http.Client
}

// newStripeWebhookPassthrough constructs the passthrough. Returns
// (nil, error) when PaymentsHTTPAddr is empty or invalid.
func newStripeWebhookPassthrough(cfg StripeWebhookPassthroughConfig) (*stripeWebhookPassthrough, error) {
	if strings.TrimSpace(cfg.PaymentsHTTPAddr) == "" {
		return nil, errors.New("stripe-webhook-passthrough: PaymentsHTTPAddr required")
	}
	parsed, err := url.Parse(cfg.PaymentsHTTPAddr)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("stripe-webhook-passthrough: PaymentsHTTPAddr must include scheme + host")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultStripeWebhookTimeout
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: timeout + time.Second}
	}
	return &stripeWebhookPassthrough{
		downstreamURL: strings.TrimRight(cfg.PaymentsHTTPAddr, "/") + StripeWebhookPath,
		timeout:       timeout,
		client:        client,
	}, nil
}

// ServeHTTP forwards the request verbatim. Body bytes are read into
// memory (Stripe webhooks cap at 256 KB; well within budget) so they
// can be signed-by-the-downstream + survive an inevitable
// http.NewRequestWithContext requirement for a fresh io.Reader.
func (h *stripeWebhookPassthrough) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method_not_allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("gateway: stripe webhook passthrough read body: %v", err)
		http.Error(w, `{"error":"bad_request"}`, http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()

	out, err := http.NewRequestWithContext(ctx, http.MethodPost, h.downstreamURL, strings.NewReader(string(body)))
	if err != nil {
		log.Printf("gateway: stripe webhook passthrough build request: %v", err)
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}

	// Forward request headers VERBATIM — including Stripe-Signature,
	// Content-Type, Content-Length, User-Agent, etc. The HMAC verification
	// downstream depends on this.
	for k, vs := range r.Header {
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}

	resp, err := h.client.Do(out)
	if err != nil {
		log.Printf("gateway: stripe webhook passthrough downstream call: %v", err)
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
		log.Printf("gateway: stripe webhook passthrough response copy: %v", copyErr)
	}
}

// WithStripeWebhookPassthrough composes the passthrough with a base
// handler. POST /v1/webhooks/stripe is owned by the passthrough; every
// other request falls through to `base`.
//
// Returns the base handler unchanged when cfg.PaymentsHTTPAddr is empty
// so cmd/server can opt out in unconfigured envs (matching the existing
// WithGatewayProxy / WithCompanionBridge pattern).
func WithStripeWebhookPassthrough(base http.Handler, cfg StripeWebhookPassthroughConfig) http.Handler {
	if strings.TrimSpace(cfg.PaymentsHTTPAddr) == "" {
		return base
	}
	h, err := newStripeWebhookPassthrough(cfg)
	if err != nil {
		log.Printf("gateway: stripe webhook passthrough disabled: %v", err)
		return base
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == StripeWebhookPath {
			h.ServeHTTP(w, r)
			return
		}
		base.ServeHTTP(w, r)
	})
}
