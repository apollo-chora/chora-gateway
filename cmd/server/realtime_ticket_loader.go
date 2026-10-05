// realtime_ticket_loader.go — SM-aware construction of the realtime
// stream-ticket minter (W0 rt, ADR-183; routes_realtime_ticket.go).
//
// Mirrors the chora_session_loader convention: the deployment carries
// CHORA_REALTIME_TICKET_SIGNER_SECRET_ID naming a Secret Manager secret
// (chora-realtime-ticket-signer — the SAME secret chora-realtime's validator
// resolves), and the loader fetches it at boot. The literal
// CHORA_REALTIME_TICKET_SIGNER_SECRET env remains as the local-dev fallback.
// Both unset → (nil, nil): the route 404s cleanly (nil-bridge convention).
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/apollo-chora/chora-common/secrets"
	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
)

// EnvRealtimeTicketSignerSecretID names the Secret Manager secret holding the
// shared HS256 ticket signer. Matches chora-realtime's config.go env so both
// sides of the contract deploy with identical manifest vocabulary.
// #nosec G101 — env-var name, not a credential.
const EnvRealtimeTicketSignerSecretID = "CHORA_REALTIME_TICKET_SIGNER_SECRET_ID"

// secretReader narrows the env-backed secrets contract to the single method
// the loaders need. Production wires *secrets.Client; tests pass a stub.
type secretReader interface {
	GetSecret(ctx context.Context, name string) (string, error)
}

// NewRealtimeTicketMinterFromEnvSM resolves the ticket signer (Secret Manager
// ID preferred, literal env fallback) and constructs the minter. The returned
// cleanup releases the Secret Manager connection; caller MUST defer it.
// Returns (nil, noop, nil) when no signer is configured (disabled posture).
func NewRealtimeTicketMinterFromEnvSM(ctx context.Context) (*httpadapter.RealtimeTicketMinter, func() error, error) {
	noop := func() error { return nil }
	if strings.TrimSpace(os.Getenv(EnvRealtimeTicketSignerSecretID)) == "" {
		// Literal-env dev path (or fully disabled).
		m, err := httpadapter.NewRealtimeTicketMinterFromEnv()
		return m, noop, err
	}
	project := resolveSourceProject()
	sm, err := secrets.NewClient(ctx, project)
	if err != nil {
		return nil, noop, fmt.Errorf("realtime-ticket loader: secret manager client: %w", err)
	}
	m, err := NewRealtimeTicketMinterFromEnvSMWith(ctx, sm)
	if err != nil {
		_ = sm.Close()
		return nil, noop, err
	}
	return m, sm.Close, nil
}

// NewRealtimeTicketMinterFromEnvSMWith is the test-friendly variant taking an
// injectable secretReader (nil is allowed only on the literal/disabled paths).
func NewRealtimeTicketMinterFromEnvSMWith(ctx context.Context, sm secretReader) (*httpadapter.RealtimeTicketMinter, error) {
	secretID := strings.TrimSpace(os.Getenv(EnvRealtimeTicketSignerSecretID))
	if secretID == "" {
		return httpadapter.NewRealtimeTicketMinterFromEnv()
	}
	if sm == nil {
		return nil, fmt.Errorf("realtime-ticket loader: secret reader required")
	}
	// Same bounded-fetch posture as the session loader: Secret Manager can
	// take seconds under cold-start contention.
	bootstrapSecs, _ := strconv.Atoi(os.Getenv("CHORA_BOOTSTRAP_TIMEOUT_SECONDS"))
	if bootstrapSecs <= 0 {
		bootstrapSecs = 30
	}
	fetchCtx, cancel := context.WithTimeout(ctx, time.Duration(bootstrapSecs)*time.Second)
	defer cancel()

	signer, err := sm.GetSecret(fetchCtx, secretID)
	if err != nil {
		return nil, fmt.Errorf("realtime-ticket loader: fetch %q: %w", secretID, err)
	}
	m, err := httpadapter.NewRealtimeTicketMinter([]byte(signer))
	if err != nil {
		return nil, fmt.Errorf("realtime-ticket loader: %q: %w", secretID, err)
	}
	return m, nil
}
