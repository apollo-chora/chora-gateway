// closure_loader.go — env-driven account-closure saga trigger bootstrap for
// chora-gateway (CHO-1719, ADR-181 D5).
//
// Wires the 4 closure trigger routes (closure_handler.go) to the
// chora-closure-orchestrator over the typed clients.ClosureClient.
//
// Env contract (per `feedback_no_inline_config` — no inline URLs):
//
//	CLOSURE_ORCHESTRATOR_URL              closure-orchestrator base URL.
//	                                      ABSENT → loader returns a nil handler
//	                                      and the closure routes answer 503
//	                                      CLOSURE_UNAVAILABLE (degraded, like
//	                                      the WebAuthn login/finish
//	                                      MINT_DISABLED 503 posture — never a
//	                                      silent 404, never a fatal boot).
//	CLOSURE_GRACE_PERIOD_DAYS             OPTIONAL grace window the gateway
//	                                      requests on every saga start
//	                                      (orchestrator REQUIRES 1-365).
//	                                      Default 30. Out-of-range / non-int
//	                                      values fail LOUD at boot.
//	CHORA_TENANCY_GRPC_ADDR               REQUIRED whenever CLOSURE_ORCHESTRATOR_URL
//	                                      is set. Backs the ownership pre-flight
//	                                      the close routes run before starting a
//	                                      saga (E3, first-launch spec 13.7.2).
//	                                      Shared with the tx-history and
//	                                      tenancy-admin loaders, own conn.
//	                                      Absent → boot error, never a close
//	                                      route that skips the check.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	tenancyv1 "github.com/apollo-chora/chora-contracts/gen/go/chora/services/tenancy/v1"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
)

const (
	// EnvClosureOrchestratorURL supplies the chora-closure-orchestrator base
	// URL (a mesh DNS name, or a local address).
	EnvClosureOrchestratorURL = "CLOSURE_ORCHESTRATOR_URL"
	// EnvClosureGraceDays overrides the default 30-day grace window.
	EnvClosureGraceDays = "CLOSURE_GRACE_PERIOD_DAYS"
)

// NewClosureHandlerFromEnv constructs the closure trigger handler entirely
// from env vars. Returns (nil, noopCleanup, nil) when CLOSURE_ORCHESTRATOR_URL
// is unset, the caller still mounts WithClosureRoutes so the routes 503
// CLOSURE_UNAVAILABLE instead of 404ing.
//
// The returned cleanup closes the chora-tenancy gRPC conn that backs the
// ownership pre-flight. It owns its own conn, like tenancy_admin_loader, so a
// closure-side failure cannot take the sub-tenant create route with it.
//
// CHORA_TENANCY_GRPC_ADDR is REQUIRED once closure is configured. It is not an
// additive surface that may degrade: without the membership read the close
// routes cannot tell an owner from anybody else, and a closure that skips the
// check can leave an organisation permanently ownerless. So an unset address
// here is a boot error, not a 404 and not a route that quietly stops checking.
func NewClosureHandlerFromEnv(ctx context.Context) (*httpadapter.ClosureHandler, func() error, error) {
	baseURL := strings.TrimSpace(os.Getenv(EnvClosureOrchestratorURL))
	if baseURL == "" {
		return nil, noopCleanup, nil
	}

	graceDays := 30
	if v := strings.TrimSpace(os.Getenv(EnvClosureGraceDays)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 365 {
			return nil, noopCleanup, fmt.Errorf("closure: %s must be an integer in [1,365], got %q",
				EnvClosureGraceDays, v)
		}
		graceDays = n
	}

	cc, err := clients.NewClosureClient(clients.ClosureClientConfig{
		BaseURL: baseURL,
	})
	if err != nil {
		return nil, noopCleanup, fmt.Errorf("closure: client: %w", err)
	}

	tenancyAddr := strings.TrimSpace(os.Getenv(EnvTenancyGRPCAddr))
	if tenancyAddr == "" {
		return nil, noopCleanup, fmt.Errorf(
			"closure: %s is required when %s is set (the close routes must check tenant ownership before starting a saga)",
			EnvTenancyGRPCAddr, EnvClosureOrchestratorURL)
	}
	conn, err := grpc.NewClient(tenancyAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, noopCleanup, fmt.Errorf("closure: dial tenancy %s: %w", tenancyAddr, err)
	}
	own, err := clients.NewTenancyAdminClient(clients.TenancyAdminClientConfig{
		RPC: tenancyv1.NewTenancyClient(conn),
	})
	if err != nil {
		_ = conn.Close()
		return nil, noopCleanup, fmt.Errorf("closure: ownership lookup: %w", err)
	}

	h, err := httpadapter.NewClosureHandler(httpadapter.ClosureHandlerConfig{
		Backend:         cc,
		Ownership:       own,
		GracePeriodDays: graceDays,
	})
	if err != nil {
		_ = conn.Close()
		return nil, noopCleanup, fmt.Errorf("closure: handler: %w", err)
	}
	return h, conn.Close, nil
}
