// stripe_config_handler.go — GET /api/v1/stripe-config endpoint.
//
// Surfaces the Stripe **publishable** key + checkout URL prefixes to the
// chora-web FE so the SPA can hand off to Stripe Checkout without baking
// the publishable key into the bundle at compile time. The key is
// resolved at startup from Secret Manager (`chora-stripe-publishable-key`
// by default) and cached in-process with a 5-min TTL.
//
// Auth: this route lives behind the canonical Chora session JWT gate
// (DefaultJWTGatedPrefixes). Anonymous callers are 401'd by
// WithChoraSessionOnPrefixes before they reach this handler.
//
// Per `feedback_no_inline_config`: secret name + URL prefixes come from
// env (StripeConfigHandlerConfig.PublishableKeyName / SuccessUrlPrefix /
// CancelUrl). NO literal key strings or URLs in this file.
package httpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SecretReader is the minimal Secret Manager surface needed by the
// stripe-config handler. Production wires *cgcsecrets.Client; tests pass
// a *cgcsecrets.StubClient.
type SecretReader interface {
	GetSecret(ctx context.Context, name string) (string, error)
}

// StripeConfigHandler serves GET /api/v1/stripe-config.
type StripeConfigHandler struct {
	secrets            SecretReader
	publishableKeyName string
	checkoutSuccessURL string
	checkoutCancelURL  string
	cacheTTL           time.Duration
	now                func() time.Time

	mu       sync.RWMutex
	cached   string
	cachedAt time.Time
}

// StripeConfigHandlerConfig is the constructor input. All required
// fields must be set; NewStripeConfigHandler errors otherwise so the
// binary fails fast at startup.
type StripeConfigHandlerConfig struct {
	// Secrets is the GSM client (production) or stub (tests). REQUIRED.
	Secrets SecretReader

	// PublishableKeyName is the Secret Manager secret name to fetch
	// (e.g., "chora-stripe-publishable-key"). REQUIRED.
	PublishableKeyName string

	// CheckoutSuccessURLPrefix is the BFF-facing URL prefix appended with
	// the hatching session id on successful checkout (e.g.,
	// "https://chora.site/a/companion/hatching/"). Optional — when empty
	// the response field is omitted.
	CheckoutSuccessURLPrefix string

	// CheckoutCancelURL is the BFF-facing URL the FE redirects to when
	// the user cancels Stripe Checkout. Optional — when empty the
	// response field is omitted.
	CheckoutCancelURL string

	// CacheTTL is how long a successfully-fetched publishable key is
	// cached in-process. Defaults to 5 minutes. Below 1s clamps to the
	// default — the cache is the only thing keeping Secret Manager call
	// volume reasonable under FE-driven load.
	CacheTTL time.Duration

	// Now is a clock fn for tests. Defaults to time.Now.
	Now func() time.Time
}

// NewStripeConfigHandler constructs a handler. Returns error when
// required deps are missing.
func NewStripeConfigHandler(cfg StripeConfigHandlerConfig) (*StripeConfigHandler, error) {
	if cfg.Secrets == nil {
		return nil, errors.New("stripe-config: secrets reader required")
	}
	if strings.TrimSpace(cfg.PublishableKeyName) == "" {
		return nil, errors.New("stripe-config: publishable key secret name required")
	}
	cacheTTL := cfg.CacheTTL
	if cacheTTL < time.Second {
		cacheTTL = 5 * time.Minute
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &StripeConfigHandler{
		secrets:            cfg.Secrets,
		publishableKeyName: strings.TrimSpace(cfg.PublishableKeyName),
		checkoutSuccessURL: strings.TrimSpace(cfg.CheckoutSuccessURLPrefix),
		checkoutCancelURL:  strings.TrimSpace(cfg.CheckoutCancelURL),
		cacheTTL:           cacheTTL,
		now:                now,
	}, nil
}

// StripeConfigResponse is the JSON shape returned to the FE.
//
// publishableKey is REQUIRED in the response; the URL fields are omitted
// (via omitempty) when the loader didn't supply them — the FE then
// falls back to its compile-time defaults.
type StripeConfigResponse struct {
	PublishableKey           string `json:"publishableKey"`
	CheckoutSuccessURLPrefix string `json:"checkoutSuccessUrlPrefix,omitempty"`
	CheckoutCancelURL        string `json:"checkoutCancelUrl,omitempty"`
}

// ServeHTTP is the GET handler.
//
// Returns:
//   - 200 with StripeConfigResponse on success (cached or freshly fetched)
//   - 405 on non-GET
//   - 503 when no cached value exists AND the upstream Secret Manager
//     call failed (transient backend unavailability — FE retries)
func (h *StripeConfigHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, `{"error":{"code":"METHOD_NOT_ALLOWED","message":"GET required"}}`, http.StatusMethodNotAllowed)
		return
	}

	key, err := h.resolveKey(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"code":"STRIPE_CONFIG_UNAVAILABLE","message":%q}}`, err.Error()), http.StatusServiceUnavailable)
		return
	}

	resp := StripeConfigResponse{
		PublishableKey:           key,
		CheckoutSuccessURLPrefix: h.checkoutSuccessURL,
		CheckoutCancelURL:        h.checkoutCancelURL,
	}
	w.Header().Set("Content-Type", "application/json")
	// Short browser-cache; the FE auth-service may re-call between
	// page navigations so a few seconds of staleness is acceptable.
	w.Header().Set("Cache-Control", "private, max-age=60")
	_ = json.NewEncoder(w).Encode(resp)
}

// resolveKey returns a cached value when fresh; otherwise fetches from
// Secret Manager. Cache hits are read-locked; cache fills are
// write-locked.
func (h *StripeConfigHandler) resolveKey(ctx context.Context) (string, error) {
	now := h.now()

	h.mu.RLock()
	cached := h.cached
	cachedAt := h.cachedAt
	h.mu.RUnlock()
	if cached != "" && now.Sub(cachedAt) < h.cacheTTL {
		return cached, nil
	}

	// Cache miss / stale — fetch from Secret Manager.
	val, err := h.secrets.GetSecret(ctx, h.publishableKeyName)
	if err != nil {
		// If we have a cached value (even if stale), prefer it over a
		// hard failure (operator may have momentary GSM outage).
		if cached != "" {
			return cached, nil
		}
		return "", fmt.Errorf("fetch %s: %w", h.publishableKeyName, err)
	}
	val = strings.TrimSpace(val)
	if val == "" {
		if cached != "" {
			return cached, nil
		}
		return "", fmt.Errorf("%s: empty value", h.publishableKeyName)
	}

	h.mu.Lock()
	h.cached = val
	h.cachedAt = now
	h.mu.Unlock()

	return val, nil
}

// WithStripeConfigRoute shadows GET /api/v1/stripe-config on the supplied
// handler. Anything else falls through. When h is nil this is a
// passthrough — keeps cmd/server/main.go boot-resilient when the loader
// is intentionally disabled in dev.
func WithStripeConfigRoute(base http.Handler, h *StripeConfigHandler) http.Handler {
	if h == nil {
		return base
	}
	mux := http.NewServeMux()
	mux.Handle("/api/v1/stripe-config", h)
	mux.Handle("/", base)
	return mux
}
