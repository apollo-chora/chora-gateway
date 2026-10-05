// mint_loader_test.go — specs for the username/password mint env wiring.
package main

import (
	"context"
	"testing"
	"time"
)

func TestResolveIdentityURL(t *testing.T) {
	t.Setenv(EnvIdentityURL, "http://identity.example:9000")
	t.Setenv(EnvSVCIdentityURL, "http://legacy.example:8080")
	if got := resolveIdentityURL(); got != "http://identity.example:9000" {
		t.Errorf("CHORA_IDENTITY_URL should win, got %q", got)
	}

	t.Setenv(EnvIdentityURL, "")
	t.Setenv(EnvSVCIdentityURL, "http://legacy.example:8080")
	if got := resolveIdentityURL(); got != "http://legacy.example:8080" {
		t.Errorf("SVC_IDENTITY_URL fallback, got %q", got)
	}

	t.Setenv(EnvIdentityURL, "")
	t.Setenv(EnvSVCIdentityURL, "")
	if got := resolveIdentityURL(); got != DefaultIdentityURL {
		t.Errorf("default = %q want %q", got, DefaultIdentityURL)
	}
}

func TestParseTTLSecondsOrDefault(t *testing.T) {
	t.Setenv(EnvChoraSessionTTLSeconds, "3600")
	if got := parseTTLSecondsOrDefault(EnvChoraSessionTTLSeconds, time.Hour); got != time.Hour {
		t.Errorf("env 3600 → %v want 1h", got)
	}
	t.Setenv(EnvChoraSessionTTLSeconds, "")
	if got := parseTTLSecondsOrDefault(EnvChoraSessionTTLSeconds, time.Hour); got != time.Hour {
		t.Errorf("blank → %v want default 1h", got)
	}
	t.Setenv(EnvChoraSessionTTLSeconds, "not-a-number")
	if got := parseTTLSecondsOrDefault(EnvChoraSessionTTLSeconds, time.Hour); got != time.Hour {
		t.Errorf("invalid → %v want default 1h", got)
	}
}

func TestNewMintHandlerFromEnv_DisabledReturnsNilHandler(t *testing.T) {
	t.Setenv(EnvMintDisabled, "true")
	h, cleanup, err := NewMintHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if h != nil {
		t.Fatal("MINT_DISABLED=true must return a nil handler")
	}
	if cleanup == nil {
		t.Fatal("cleanup must be non-nil")
	}
}

func TestNewMintHandlerFromEnv_FailsLoudOnEachRequiredEnv(t *testing.T) {
	for _, missing := range []string{EnvChoraSessionSigner, EnvChoraSessionIssuer, EnvChoraSessionAudience} {
		missing := missing
		t.Run(missing, func(t *testing.T) {
			t.Setenv(EnvMintDisabled, "")
			t.Setenv(EnvChoraSessionSigner, "test-mint-session-signer-0123456789abcdef")
			t.Setenv(EnvChoraSessionIssuer, "https://api.chora.site")
			t.Setenv(EnvChoraSessionAudience, "chora-local")
			t.Setenv(EnvIdentityURL, "http://127.0.0.1:1")
			t.Setenv(missing, "")
			if _, _, err := NewMintHandlerFromEnv(context.Background()); err == nil {
				t.Fatalf("want error when %s is blank", missing)
			}
		})
	}
}

func TestNewMintHandlerFromEnv_Wires(t *testing.T) {
	t.Setenv(EnvMintDisabled, "")
	t.Setenv(EnvChoraSessionSigner, "test-mint-session-signer-0123456789abcdef")
	t.Setenv(EnvChoraSessionIssuer, "https://api.chora.site")
	t.Setenv(EnvChoraSessionAudience, "chora-local")
	t.Setenv(EnvIdentityURL, "http://127.0.0.1:1")
	h, cleanup, err := NewMintHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if h == nil {
		t.Fatal("handler must be wired")
	}
	if cleanup == nil {
		t.Fatal("cleanup must be non-nil")
	}
	if err := cleanup(); err != nil {
		t.Errorf("cleanup: %v", err)
	}
}

func TestNewMintHandlerFromEnv_RejectsShortSigner(t *testing.T) {
	t.Setenv(EnvMintDisabled, "")
	t.Setenv(EnvChoraSessionSigner, "too-short")
	t.Setenv(EnvChoraSessionIssuer, "https://api.chora.site")
	t.Setenv(EnvChoraSessionAudience, "chora-local")
	if _, _, err := NewMintHandlerFromEnv(context.Background()); err == nil {
		t.Fatal("want error on <32-byte signer")
	}
}
