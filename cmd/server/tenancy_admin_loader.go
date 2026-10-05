// tenancy_admin_loader.go — env-driven chora-tenancy Tenancy service (gRPC)
// client bootstrap for the ADR-217 Phase 2.2 operator sub-tenant BFF route
// (CHO-2008).
//
// Reuses EnvTenancyGRPCAddr (chora-tenancy's gRPC port :9090; the same addr that
// serves TransactionHistoryService) per feedback_no_inline_config. Plain HTTP/2
// to the mesh sidecar (mTLS at L4) via insecure.NewCredentials() — the canonical
// chora pattern (mirrors transaction_history_loader.go). A separate conn from the
// tx-history loader keeps blast-radius isolated (each loader owns its conn).
//
// Degradation posture (same as transaction_history_loader): the sub-tenant create
// route is an ADDITIVE operator surface. An unset EnvTenancyGRPCAddr (or
// CHORA_TENANCY_ADMIN_DISABLED=true) DEGRADES — the route is not registered (404)
// with a loud boot log — rather than bricking the whole gateway. A genuine
// dial/wiring error still returns an error the caller log.Fatalfs on.
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

// EnvTenancyAdminDisabled — "true" skips registering the sub-tenant route.
const EnvTenancyAdminDisabled = "CHORA_TENANCY_ADMIN_DISABLED"

// NewTenancyAdminHandlerFromEnv wires the handler + a cleanup fn from env.
//
// Returns (nil, noop, nil) — a graceful skip — when disabled OR when
// CHORA_TENANCY_GRPC_ADDR is unset (additive surface). Returns (nil, noop, err)
// only on a real dial/construction failure (the caller log.Fatalfs).
func NewTenancyAdminHandlerFromEnv(_ context.Context) (*httpadapter.TenancyAdminHandler, func() error, error) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(EnvTenancyAdminDisabled)), "true") {
		return nil, noopCleanup, nil
	}

	addr := strings.TrimSpace(os.Getenv(EnvTenancyGRPCAddr))
	if addr == "" {
		return nil, noopCleanup, nil // degrade (route 404), do not brick the gateway
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, noopCleanup, fmt.Errorf("tenancy-admin: dial %s: %w", addr, err)
	}

	tc, err := clients.NewTenancyAdminClient(clients.TenancyAdminClientConfig{
		RPC: tenancyv1.NewTenancyClient(conn),
	})
	if err != nil {
		_ = conn.Close()
		return nil, noopCleanup, fmt.Errorf("tenancy-admin: NewTenancyAdminClient: %w", err)
	}

	h, err := httpadapter.NewTenancyAdminHandler(tc)
	if err != nil {
		_ = conn.Close()
		return nil, noopCleanup, fmt.Errorf("tenancy-admin: NewTenancyAdminHandler: %w", err)
	}

	return h, conn.Close, nil
}
