// stripe_config_loader.go — env-driven GET /api/v1/stripe-config wiring.
//
// Per `feedback_no_inline_config`: the publishable key + checkout URLs come
// from env / Secret Manager. NO hard-coded values in this file.
//
// Required env vars:
//
//	STRIPE_PUBLISHABLE_KEY_SECRET_ID   Secret Manager secret name resolving
//	                                   to the Stripe publishable key
//	                                   (e.g., "chora-stripe-publishable-key").
//	CHORA_SOURCE_PROJECT               logical project for the secrets client
//	                                   (defaults to chora-local).
//
// Optional:
//
//	STRIPE_CHECKOUT_SUCCESS_URL_PREFIX  appended with hatching session id on
//	                                    successful Stripe checkout. Omitted
//	                                    from response when empty (FE falls
//	                                    back to compile-time default).
//	STRIPE_CHECKOUT_CANCEL_URL          where the FE redirects on cancel.
//	                                    Omitted from response when empty.
//	STRIPE_CONFIG_CACHE_TTL_SECONDS     in-process cache TTL. Defaults to 300.
//	STRIPE_CONFIG_DISABLED              "true" returns (nil, nil) so the
//	                                    route is not registered (dev path).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/apollo-chora/chora-common/secrets"
	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
)

const (
	EnvStripePublishableKeySecret = "STRIPE_PUBLISHABLE_KEY_SECRET_ID"
	EnvStripeCheckoutSuccessURL   = "STRIPE_CHECKOUT_SUCCESS_URL_PREFIX"
	EnvStripeCheckoutCancelURL    = "STRIPE_CHECKOUT_CANCEL_URL"
	EnvStripeConfigCacheTTL       = "STRIPE_CONFIG_CACHE_TTL_SECONDS"
	EnvStripeConfigDisabled       = "STRIPE_CONFIG_DISABLED"
)

// ErrStripeConfigMissing is returned when STRIPE_PUBLISHABLE_KEY_SECRET_ID
// is empty AND the loader has not been disabled. The caller treats this
// as a fatal error at startup.
var ErrStripeConfigMissing = errors.New("stripe-config: STRIPE_PUBLISHABLE_KEY_SECRET_ID required")

// NewStripeConfigHandlerFromEnv constructs the handler entirely from env vars.
//
// Returns (nil, nil, nil) when STRIPE_CONFIG_DISABLED=true so callers can
// boot without the route during local dev. Caller MUST defer the returned
// cleanup when non-nil to release the Secret Manager gRPC connection on
// shutdown.
//
// Shares the Secret Manager client construction pattern with
// NewMintHandlerFromEnv — both use libs/chora-go-common/secrets and the
// canonical source-project env var.
func NewStripeConfigHandlerFromEnv(ctx context.Context) (*httpadapter.StripeConfigHandler, func() error, error) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(EnvStripeConfigDisabled)), "true") {
		return nil, func() error { return nil }, nil
	}

	publishableSecretName := strings.TrimSpace(os.Getenv(EnvStripePublishableKeySecret))
	if publishableSecretName == "" {
		return nil, nil, ErrStripeConfigMissing
	}

	project := resolveSourceProject()
	sm, err := secrets.NewClient(ctx, project)
	if err != nil {
		return nil, nil, fmt.Errorf("stripe-config: secret manager: %w", err)
	}

	cfg := httpadapter.StripeConfigHandlerConfig{
		Secrets:                  sm,
		PublishableKeyName:       publishableSecretName,
		CheckoutSuccessURLPrefix: strings.TrimSpace(os.Getenv(EnvStripeCheckoutSuccessURL)),
		CheckoutCancelURL:        strings.TrimSpace(os.Getenv(EnvStripeCheckoutCancelURL)),
		CacheTTL:                 parseSecondsOrDefault(EnvStripeConfigCacheTTL, 5*time.Minute),
	}
	h, err := httpadapter.NewStripeConfigHandler(cfg)
	if err != nil {
		_ = sm.Close()
		return nil, nil, err
	}
	return h, sm.Close, nil
}

func parseSecondsOrDefault(envKey string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return def
}
