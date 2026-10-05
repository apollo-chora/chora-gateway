// chora_session_loader_test.go — specs for env-driven chora-session HS256
// Validator construction in chora-gateway.
package main

import (
	"context"
	"strings"
	"testing"
)

const sessionTestSigner = "test-mint-session-signer-0123456789abcdef"

func TestNewChoraSessionValidatorFromEnv_HappyPath(t *testing.T) {
	t.Setenv(EnvChoraSessionSigner, sessionTestSigner)
	t.Setenv(EnvChoraSessionIssuerURL, "https://api.chora.site")
	t.Setenv(EnvChoraSessionAudienceURL, "chora-local")

	v, cleanup, err := NewChoraSessionValidatorFromEnv(context.Background())
	if err != nil {
		t.Fatalf("NewChoraSessionValidatorFromEnv: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if v == nil {
		t.Fatal("expected non-nil validator")
	}
}

func TestNewChoraSessionValidatorFromEnv_FailsLoudOnMissingSigner(t *testing.T) {
	t.Setenv(EnvChoraSessionSigner, "")
	t.Setenv(EnvChoraSessionIssuerURL, "https://api.chora.site")
	t.Setenv(EnvChoraSessionAudienceURL, "chora-local")

	_, _, err := NewChoraSessionValidatorFromEnv(context.Background())
	if err == nil {
		t.Fatal("expected error when CHORA_SESSION_SIGNER missing")
	}
	if !strings.Contains(err.Error(), EnvChoraSessionSigner) {
		t.Errorf("err should reference %s; got %v", EnvChoraSessionSigner, err)
	}
}

func TestNewChoraSessionValidatorFromEnv_FailsLoudOnMissingIssuer(t *testing.T) {
	t.Setenv(EnvChoraSessionSigner, sessionTestSigner)
	t.Setenv(EnvChoraSessionIssuerURL, "")
	t.Setenv(EnvChoraSessionAudienceURL, "chora-local")

	_, _, err := NewChoraSessionValidatorFromEnv(context.Background())
	if err == nil {
		t.Fatal("expected error when CHORA_SESSION_ISSUER missing")
	}
	if !strings.Contains(err.Error(), EnvChoraSessionIssuerURL) {
		t.Errorf("err should reference %s; got %v", EnvChoraSessionIssuerURL, err)
	}
}

func TestNewChoraSessionValidatorFromEnv_FailsLoudOnMissingAudience(t *testing.T) {
	t.Setenv(EnvChoraSessionSigner, sessionTestSigner)
	t.Setenv(EnvChoraSessionIssuerURL, "https://api.chora.site")
	t.Setenv(EnvChoraSessionAudienceURL, "")

	_, _, err := NewChoraSessionValidatorFromEnv(context.Background())
	if err == nil {
		t.Fatal("expected error when CHORA_SESSION_AUDIENCE missing")
	}
	if !strings.Contains(err.Error(), EnvChoraSessionAudienceURL) {
		t.Errorf("err should reference %s; got %v", EnvChoraSessionAudienceURL, err)
	}
}

func TestNewChoraSessionValidatorFromEnv_FailsLoudOnShortSigner(t *testing.T) {
	t.Setenv(EnvChoraSessionSigner, "too-short")
	t.Setenv(EnvChoraSessionIssuerURL, "https://api.chora.site")
	t.Setenv(EnvChoraSessionAudienceURL, "chora-local")

	_, _, err := NewChoraSessionValidatorFromEnv(context.Background())
	if err == nil {
		t.Fatal("expected error when signer < 32 bytes")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "32") {
		t.Errorf("err should mention 32-byte minimum; got %v", err)
	}
}
