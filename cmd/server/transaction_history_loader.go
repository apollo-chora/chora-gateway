// transaction_history_loader.go — env-driven chora-tenancy
// TransactionHistoryService gRPC client bootstrap for the ADR-205 contextual
// Transaction History BFF (CHO-1940 / Wave B4).
//
// Per feedback_no_inline_config the dial address is sourced from
// CHORA_TENANCY_GRPC_ADDR (chora-tenancy's gRPC port is :9090; e.g.
// `chora-tenancy.tenancy.svc.cluster.local:9090`). Plain HTTP/2 to the mesh
// sidecar (mTLS at L4 via Cloud Service Mesh) — `insecure.NewCredentials()` is
// the canonical chora pattern (mirrors payments_loader.go).
//
// Degradation posture (deliberate divergence from payments_loader, which
// FATALs on a missing addr): Transaction History is an ADDITIVE read surface
// that supersedes the still-working payments-admin purchases path (retired in
// Wave D). Bricking the ENTIRE gateway — which serves every route — because
// this one new surface's addr is unset is the wrong blast radius. So an unset
// addr (or CHORA_TRANSACTION_HISTORY_DISABLED=true) DEGRADES: the routes are
// not registered (404; the FE keeps the old path) with a loud boot log, rather
// than failing the process. A genuine dial/wiring error still returns an error
// the caller log.Fatalfs on (that is a misconfiguration, not absence).
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	tenancyv1 "github.com/apollo-chora/chora-contracts/gen/go/chora/services/tenancy/v1"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
)

// EnvTenancyGRPCAddr supplies the chora-tenancy gRPC dial address for the
// TransactionHistoryService (also home to CompanionEgg + Tenancy gRPC servers).
const EnvTenancyGRPCAddr = "CHORA_TENANCY_GRPC_ADDR"

// EnvTransactionHistoryDisabled — "true" skips registering the routes.
const EnvTransactionHistoryDisabled = "CHORA_TRANSACTION_HISTORY_DISABLED"

// noopCleanup is returned whenever there is no gRPC conn to close.
func noopCleanup() error { return nil }

// NewTransactionHistoryHandlerFromEnv wires the handler + a cleanup fn from env.
//
// Returns (nil, noop, nil) — a graceful skip — when disabled OR when
// CHORA_TENANCY_GRPC_ADDR is unset (additive surface; see file header). Returns
// (nil, noop, err) only on a real dial/construction failure (the caller
// log.Fatalfs).
func NewTransactionHistoryHandlerFromEnv(_ context.Context) (*httpadapter.TransactionHistoryHandler, func() error, error) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(EnvTransactionHistoryDisabled)), "true") {
		return nil, noopCleanup, nil
	}

	addr := strings.TrimSpace(os.Getenv(EnvTenancyGRPCAddr))
	if addr == "" {
		return nil, noopCleanup, nil // degrade (routes 404), do not brick the gateway
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, noopCleanup, fmt.Errorf("transaction-history: dial %s: %w", addr, err)
	}

	tc, err := clients.NewTransactionHistoryClient(clients.TransactionHistoryClientConfig{
		RPC: tenancyv1.NewTransactionHistoryServiceClient(conn),
	})
	if err != nil {
		_ = conn.Close()
		return nil, noopCleanup, fmt.Errorf("transaction-history: NewTransactionHistoryClient: %w", err)
	}

	h, err := httpadapter.NewTransactionHistoryHandler(tc)
	if err != nil {
		_ = conn.Close()
		return nil, noopCleanup, fmt.Errorf("transaction-history: NewTransactionHistoryHandler: %w", err)
	}

	return h, conn.Close, nil
}
