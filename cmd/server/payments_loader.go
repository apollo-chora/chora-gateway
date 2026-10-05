// payments_loader.go — env-driven chora-payments gRPC client bootstrap for
// the /api/v1/checkout/* REST proxy (Stage B of ADR-164).
//
// Per `feedback_no_inline_config`: the chora-payments gRPC dial address is
// sourced from CHORA_PAYMENTS_GRPC_ADDR. Per `feedback_no_stubs_real_wiring`:
// the loader returns an error when the env is unset — chora-gateway refuses
// to start in production without it. The boot env gate (boot_env_gate.go)
// surfaces the missing var name at process start.
//
// Trust model: chora-gateway → chora-payments speaks plain HTTP/2 to its
// sidecar; Cloud Service Mesh handles mTLS at L4. `insecure.NewCredentials()`
// is the canonical chora pattern (mirrors chora-delivery main.go's gRPC dial
// to SVC_CREATION_GRPC_URL + chora-identity's mana-service client).
//
// Caller pattern in main():
//
//	paymentsHandler, paymentsCleanup, err := NewPaymentsCheckoutHandlerFromEnv(ctx)
//	if err != nil {
//	    log.Fatalf("payments handler init failed: %v", err)
//	}
//	defer paymentsCleanup()
//	mux = httpadapter.WithPaymentsCheckout(mux, paymentsHandler)
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
)

// EnvPaymentsGRPCAddr is the env var that supplies the chora-payments gRPC
// dial address (e.g. `chora-payments.payments.svc.cluster.local:9090` in
// production, `localhost:9090` in dev). REQUIRED — boot fails loud when
// unset per feedback_no_stubs_real_wiring.
const EnvPaymentsGRPCAddr = "CHORA_PAYMENTS_GRPC_ADDR"

// EnvPaymentsHTTPAddr is the env var that supplies the chora-payments REST
// admin surface base URL (e.g. `http://chora-payments.payments.svc.cluster.local`
// in production). Consumed by gatewayproxy.LoadConfigFromEnv to populate
// Config.PaymentsURL for the H+ tx-history admin routes per
// chora-contracts/openapi/payments-admin.yaml (Phase 2 Agent A2, 2026-05-26).
//
// Aligns with the existing EnvPaymentsGRPCAddr twin (`CHORA_PAYMENTS_GRPC_ADDR`)
// and the live `CHORA_PAYMENTS_HTTP_ADDR` already plumbed in
// chora-infra/k8s/services/chora-gateway/deployment.yaml for the Stripe webhook
// passthrough — single env var name now serves both that and tx-history.
//
// Defaults to the in-cluster K8s DNS name when unset so dev pods boot without
// explicit wiring; production deployment manifests SHOULD set it explicitly
// for clarity. Listed here for discoverability — the actual env read happens
// in services/chora-gateway/internal/aggregator/gatewayproxy/gatewayproxy.go
// LoadConfigFromEnv() per the canonical no-inline-config convention.
const EnvPaymentsHTTPAddr = "CHORA_PAYMENTS_HTTP_ADDR"

// EnvPaymentsDisabled — "true" returns (nil, nil-cleanup, nil) so the
// /api/v1/checkout/* routes are not registered (dev-only opt-out).
const EnvPaymentsDisabled = "CHORA_PAYMENTS_DISABLED"

// EnvManaPacks supplies the per-user mana-pack price catalogue as a JSON array
// (WS-2.4). Sourced from Secret Manager / Terraform per feedback_no_inline_config.
// When unset, /api/v1/checkout/user-mana fails loud (503) — the price is never
// taken from the client body (umbrella-currency abuse vector). Example:
//
//	[{"sku":"mana_pack_1000","mana_units":1000,"amount_cents":199,"currency":"USD"}]
const EnvManaPacks = "CHORA_MANA_PACKS"

// ErrPaymentsAddrMissing is returned when CHORA_PAYMENTS_GRPC_ADDR is
// empty AND the loader has not been disabled.
var ErrPaymentsAddrMissing = errors.New("payments: CHORA_PAYMENTS_GRPC_ADDR required")

// NewPaymentsCheckoutHandlerFromEnv constructs a wired *PaymentsCheckoutHandler
// + a cleanup fn (closes the underlying gRPC conn) entirely from env vars.
//
// Returns (nil, nil-cleanup, nil) when CHORA_PAYMENTS_DISABLED=true so
// callers can boot without the route during local dev. Returns
// (nil, nil-cleanup, ErrPaymentsAddrMissing) when CHORA_PAYMENTS_GRPC_ADDR
// is empty AND the loader has not been disabled — the caller log.Fatalfs.
func NewPaymentsCheckoutHandlerFromEnv(_ context.Context) (*httpadapter.PaymentsCheckoutHandler, func() error, error) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(EnvPaymentsDisabled)), "true") {
		return nil, func() error { return nil }, nil
	}

	addr := strings.TrimSpace(os.Getenv(EnvPaymentsGRPCAddr))
	if addr == "" {
		return nil, func() error { return nil }, ErrPaymentsAddrMissing
	}

	conn, err := grpc.NewClient(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, func() error { return nil }, fmt.Errorf("payments: dial %s: %w", addr, err)
	}

	// ADR-254 D9 W4-to-W5 overlap: this binary is built against the renamed
	// contracts, but chora-payments does not roll until W5 and its mesh
	// allowlist admits only the Familiar-spelled egg method. The shim sends
	// that ONE RPC to the old path (field numbers are unchanged, so the bytes
	// are identical) and leaves every other RPC on its generated path. Logged,
	// not silent, so the overlap cannot be forgotten in the binary. Replaced by
	// paymentsv1.NewPaymentServiceClient(conn) in the W5 payments commit, which
	// must also add a server-side legacy alias for chora-tenancy.
	log.Printf("gateway: payments egg checkout dials the W4-to-W5 legacy method %s (drop with the W5 payments roll)",
		clients.LegacyCompanionEggMethod)
	pc, err := clients.NewPaymentsClient(clients.PaymentsClientConfig{
		RPC: clients.NewPaymentServiceClientWithLegacyEggMethod(conn),
	})
	if err != nil {
		_ = conn.Close()
		return nil, func() error { return nil }, fmt.Errorf("payments: NewPaymentsClient: %w", err)
	}

	h, err := httpadapter.NewPaymentsCheckoutHandler(pc)
	if err != nil {
		_ = conn.Close()
		return nil, func() error { return nil }, fmt.Errorf("payments: NewPaymentsCheckoutHandler: %w", err)
	}

	// WS-2.4 — attach the server-side mana-pack price catalogue. A malformed
	// CHORA_MANA_PACKS fails loud at boot; an unset one leaves the user-mana
	// route 503 (never client-priced).
	packs, perr := httpadapter.ParseManaPackCatalogue(os.Getenv(EnvManaPacks))
	if perr != nil {
		_ = conn.Close()
		return nil, func() error { return nil }, fmt.Errorf("payments: mana packs: %w", perr)
	}
	h = h.WithManaPacks(packs)

	cleanup := func() error {
		return conn.Close()
	}
	return h, cleanup, nil
}
