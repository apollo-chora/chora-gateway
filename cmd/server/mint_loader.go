// mint_loader.go — env-driven username/password → Chora session JWT mint
// handler construction for chora-gateway.
//
// Per `feedback_no_inline_config`, every value comes from env vars.
//
// Required env vars (fail-loud at boot; no silent defaults):
//
//	CHORA_SESSION_SIGNER   HS256 signing key (≥32 bytes), shared with
//	                       chora-identity. No key exchange.
//	CHORA_SESSION_ISSUER   iss claim baked into the minted session.
//	CHORA_SESSION_AUDIENCE aud claim baked into the minted session.
//
// Optional:
//
//	CHORA_IDENTITY_URL             chora-identity base URL. Defaults to
//	                               http://identity:8080. Falls back to
//	                               SVC_IDENTITY_URL when set.
//	CHORA_SESSION_TTL_SECONDS      defaults to 3600 (= 1h).
//	MINT_DISABLED                  "true" returns (nil, nil) so the route is
//	                               not registered (dev path).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/domain/session"
)

const (
	EnvChoraSessionSigner     = "CHORA_SESSION_SIGNER" // #nosec G101 — env-var name, not a credential.
	EnvChoraSessionIssuer     = "CHORA_SESSION_ISSUER"
	EnvChoraSessionAudience   = "CHORA_SESSION_AUDIENCE"
	EnvChoraSessionTTLSeconds = "CHORA_SESSION_TTL_SECONDS"
	EnvIdentityURL            = "CHORA_IDENTITY_URL"
	EnvSVCIdentityURL         = "SVC_IDENTITY_URL"
	EnvMintDisabled           = "MINT_DISABLED"
)

// DefaultIdentityURL is the local-stack chora-identity address.
const DefaultIdentityURL = "http://identity:8080"

// resolveIdentityURL returns the chora-identity base URL. CHORA_IDENTITY_URL
// wins; SVC_IDENTITY_URL is accepted as a compatibility fallback; otherwise
// the local default applies.
func resolveIdentityURL() string {
	if v := strings.TrimSpace(os.Getenv(EnvIdentityURL)); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv(EnvSVCIdentityURL)); v != "" {
		return v
	}
	return DefaultIdentityURL
}

// ErrMintConfigMissing is returned when a REQUIRED env var is missing.
var ErrMintConfigMissing = errors.New("mint config missing required env var")

// NewMintHandlerFromEnv constructs the mint handler entirely from env vars.
// Returns (nil, nil) when MINT_DISABLED=true so callers can boot without the
// route during local dev.
func NewMintHandlerFromEnv(ctx context.Context, sessions session.Repository) (*httpadapter.MintHandler, func() error, error) {
	noop := func() error { return nil }
	if strings.EqualFold(strings.TrimSpace(os.Getenv(EnvMintDisabled)), "true") {
		return nil, noop, nil
	}

	signer := strings.TrimSpace(os.Getenv(EnvChoraSessionSigner))
	if signer == "" {
		return nil, nil, fmt.Errorf("%w: %s", ErrMintConfigMissing, EnvChoraSessionSigner)
	}
	if len(signer) < 32 {
		return nil, nil, fmt.Errorf("mint: %s must be ≥32 bytes (got %d)", EnvChoraSessionSigner, len(signer))
	}
	sessionIssuer := strings.TrimSpace(os.Getenv(EnvChoraSessionIssuer))
	if sessionIssuer == "" {
		return nil, nil, fmt.Errorf("%w: %s", ErrMintConfigMissing, EnvChoraSessionIssuer)
	}
	sessionAud := strings.TrimSpace(os.Getenv(EnvChoraSessionAudience))
	if sessionAud == "" {
		return nil, nil, fmt.Errorf("%w: %s", ErrMintConfigMissing, EnvChoraSessionAudience)
	}

	identityURL := resolveIdentityURL()

	credsClient, err := clients.NewIdentityCredentialsClient(clients.IdentityCredentialsClientConfig{
		BaseURL: identityURL,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("mint: credentials client: %w", err)
	}
	identityClient, err := clients.NewIdentityResolveClient(clients.IdentityResolveClientConfig{
		BaseURL: identityURL,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("mint: identity client: %w", err)
	}

	cfg := httpadapter.MintHandlerConfig{
		Credentials:   credsClient,
		Identity:      identityClient,
		Sessions:      sessions,
		SessionSigner: []byte(signer),
		SessionIssuer: sessionIssuer,
		SessionAud:    sessionAud,
		SessionTTL:    parseTTLSecondsOrDefault(EnvChoraSessionTTLSeconds, time.Hour),
	}
	h, err := httpadapter.NewMintHandler(cfg)
	if err != nil {
		return nil, nil, err
	}
	log.Printf("mint: wired (identity=%s, ttl=%s)", identityURL, cfg.SessionTTL)
	return h, noop, nil
}

func parseTTLSecondsOrDefault(envKey string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return def
}
