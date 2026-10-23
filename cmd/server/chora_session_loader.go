// chora_session_loader.go — env-driven chora-session HS256 Validator
// construction for chora-gateway.
//
// The /api/* trust boundary validates Chora session JWTs minted by
// POST /api/v1/auth/session/mint. This loader reads the shared HS256 signing
// key + issuer/audience from env and builds the canonical
// chorasession.Validator that chora-gateway plugs into
// WithChoraSessionOnPrefixes.
//
// Per `feedback_no_inline_config`: every input comes from env. The
// constructor fails LOUD when any of these is missing:
//
//	CHORA_SESSION_SIGNER   — HS256 key (shared with chora-identity)
//	CHORA_SESSION_ISSUER   — chora-session iss claim
//	CHORA_SESSION_AUDIENCE — chora-session aud claim
//
// Also fails loud when the signing key is fewer than 32 bytes (HS256 minimum
// per RFC 7518 §3.2).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/apollo-chora/chora-common/auth/chorasession"
)

const (
	// EnvChoraSessionJWTSignerSecret names the key the kiosk edition fetches its
	// session signer by, from the internal secrets file (kiosk_edition.go
	// secretReader). The kiosk compose sets it to CHORA_KIOSK_SESSION_SIGNER.
	EnvChoraSessionJWTSignerSecret = "CHORA_SESSION_JWT_SIGNER_SECRET" // #nosec G101 — env-var name, not a credential.
	EnvChoraSessionIssuerURL       = "CHORA_SESSION_ISSUER"
	EnvChoraSessionAudienceURL     = "CHORA_SESSION_AUDIENCE"
)

// ErrChoraSessionConfigMissing surfaces when a required env var is blank.
var ErrChoraSessionConfigMissing = errors.New("chora-session loader: required env var missing")

// NewChoraSessionValidatorFromEnv constructs the chorasession.Validator from
// env vars. The returned cleanup is a no-op (kept for lifecycle symmetry).
func NewChoraSessionValidatorFromEnv(ctx context.Context) (*chorasession.Validator, func() error, error) {
	noop := func() error { return nil }

	signer := strings.TrimSpace(os.Getenv(EnvChoraSessionSigner))
	if signer == "" {
		return nil, nil, fmt.Errorf("%w: %s", ErrChoraSessionConfigMissing, EnvChoraSessionSigner)
	}
	if len(signer) < 32 {
		return nil, nil, fmt.Errorf("chora-session loader: %s must be ≥32 bytes (got %d)",
			EnvChoraSessionSigner, len(signer))
	}
	issuer := strings.TrimSpace(os.Getenv(EnvChoraSessionIssuerURL))
	if issuer == "" {
		return nil, nil, fmt.Errorf("%w: %s", ErrChoraSessionConfigMissing, EnvChoraSessionIssuerURL)
	}
	audience := strings.TrimSpace(os.Getenv(EnvChoraSessionAudienceURL))
	if audience == "" {
		return nil, nil, fmt.Errorf("%w: %s", ErrChoraSessionConfigMissing, EnvChoraSessionAudienceURL)
	}

	v, err := chorasession.NewValidator([]byte(signer), issuer, audience)
	if err != nil {
		return nil, nil, fmt.Errorf("chora-session loader: build validator: %w", err)
	}
	return v, noop, nil
}

// NewChoraSessionValidatorFromEnvWith is the injectable-secretReader variant the
// kiosk edition needs: the session signer is read from the internal secrets file
// (kiosk_edition.go secretReader) instead of the CHORA_SESSION_SIGNER env var.
// The signer's key name is EnvChoraSessionJWTSignerSecret — for the kiosk the
// compose sets it to CHORA_KIOSK_SESSION_SIGNER, the name the install script
// writes into internal.env. The cloud edition's NewChoraSessionValidatorFromEnv
// is unchanged.
func NewChoraSessionValidatorFromEnvWith(ctx context.Context, sm secretReader) (*chorasession.Validator, func() error, error) {
	signerKey := strings.TrimSpace(os.Getenv(EnvChoraSessionJWTSignerSecret))
	if signerKey == "" {
		return nil, nil, fmt.Errorf("%w: %s", ErrChoraSessionConfigMissing, EnvChoraSessionJWTSignerSecret)
	}
	issuer := strings.TrimSpace(os.Getenv(EnvChoraSessionIssuerURL))
	if issuer == "" {
		return nil, nil, fmt.Errorf("%w: %s", ErrChoraSessionConfigMissing, EnvChoraSessionIssuerURL)
	}
	audience := strings.TrimSpace(os.Getenv(EnvChoraSessionAudienceURL))
	if audience == "" {
		return nil, nil, fmt.Errorf("%w: %s", ErrChoraSessionConfigMissing, EnvChoraSessionAudienceURL)
	}
	if sm == nil {
		return nil, nil, fmt.Errorf("chora-session loader: secret reader required")
	}

	signer, err := sm.GetSecret(ctx, signerKey)
	if err != nil {
		return nil, nil, fmt.Errorf("chora-session loader: fetch %q: %w", signerKey, err)
	}
	if len(signer) < 32 {
		return nil, nil, fmt.Errorf("chora-session loader: signer %q must be ≥32 bytes (got %d)",
			signerKey, len(signer))
	}
	v, err := chorasession.NewValidator([]byte(signer), issuer, audience)
	if err != nil {
		return nil, nil, fmt.Errorf("chora-session loader: build validator: %w", err)
	}
	return v, func() error { return nil }, nil
}
