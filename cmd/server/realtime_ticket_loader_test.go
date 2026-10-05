// realtime_ticket_loader_test.go — strict TDD RED for the SM-aware realtime
// ticket-signer loader (W0 rt, ADR-183). Mirrors chora_session_loader_test
// conventions: stubSecrets injection, env via t.Setenv.
package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/apollo-chora/chora-common/secrets"
)

// stubSecrets is an in-memory secretReader for the loader specs.
func stubSecrets(values map[string]string) secretReader {
	return &stubSecretsImpl{values: values}
}

type stubSecretsImpl struct {
	values map[string]string
}

func (s *stubSecretsImpl) GetSecret(_ context.Context, name string) (string, error) {
	v, ok := s.values[name]
	if !ok {
		return "", fmt.Errorf("stubSecrets: %w: %s", secrets.ErrSecretNotFound, name)
	}
	return v, nil
}

func TestRealtimeTicketLoader_ResolvesSecretManagerID(t *testing.T) {
	t.Setenv("CHORA_REALTIME_TICKET_SIGNER_SECRET_ID", "chora-realtime-ticket-signer")
	t.Setenv("CHORA_REALTIME_TICKET_SIGNER_SECRET", "")
	sm := stubSecrets(map[string]string{
		"chora-realtime-ticket-signer": strings.Repeat("k", 48),
	})
	m, err := NewRealtimeTicketMinterFromEnvSMWith(context.Background(), sm)
	if err != nil {
		t.Fatalf("want minter, got err: %v", err)
	}
	if m == nil {
		t.Fatalf("want non-nil minter when SECRET_ID resolves")
	}
}

func TestRealtimeTicketLoader_SecretMissingIsFatal(t *testing.T) {
	t.Setenv("CHORA_REALTIME_TICKET_SIGNER_SECRET_ID", "chora-realtime-ticket-signer")
	sm := stubSecrets(map[string]string{})
	if _, err := NewRealtimeTicketMinterFromEnvSMWith(context.Background(), sm); err == nil {
		t.Fatalf("want fatal error when the named secret cannot be fetched")
	}
}

func TestRealtimeTicketLoader_ShortSecretIsFatal(t *testing.T) {
	t.Setenv("CHORA_REALTIME_TICKET_SIGNER_SECRET_ID", "chora-realtime-ticket-signer")
	sm := stubSecrets(map[string]string{"chora-realtime-ticket-signer": "short"})
	if _, err := NewRealtimeTicketMinterFromEnvSMWith(context.Background(), sm); err == nil {
		t.Fatalf("want fatal error for <32-byte resolved secret")
	}
}

func TestRealtimeTicketLoader_LiteralFallbackForDev(t *testing.T) {
	t.Setenv("CHORA_REALTIME_TICKET_SIGNER_SECRET_ID", "")
	t.Setenv("CHORA_REALTIME_TICKET_SIGNER_SECRET", strings.Repeat("k", 32))
	m, err := NewRealtimeTicketMinterFromEnvSMWith(context.Background(), nil)
	if err != nil || m == nil {
		t.Fatalf("want literal-env minter, got (%v, %v)", m, err)
	}
}

func TestRealtimeTicketLoader_BothUnsetDisablesCleanly(t *testing.T) {
	t.Setenv("CHORA_REALTIME_TICKET_SIGNER_SECRET_ID", "")
	t.Setenv("CHORA_REALTIME_TICKET_SIGNER_SECRET", "")
	m, err := NewRealtimeTicketMinterFromEnvSMWith(context.Background(), nil)
	if err != nil || m != nil {
		t.Fatalf("want (nil, nil) disabled posture, got (%v, %v)", m, err)
	}
}
