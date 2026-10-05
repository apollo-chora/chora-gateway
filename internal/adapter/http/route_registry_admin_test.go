// route_registry_admin_test.go — the guard for the gateway's THIRD list.
//
// WHY THIS EXISTS (walk-caught 2026-07-14, CHO-2148).
//
// Wiring a new BFF route needs THREE things in sync, not two:
//
//  1. an aggregator method (e.g. phyllis.GetAdminMyExternalEgress)
//  2. a mux registration  (phyllis_handler.go: mux.HandleFunc(...))
//  3. AN ENTRY IN THE ROUTE REGISTRY  (inmem.NewRouteRepository / defaultRoutes)
//
// Miss (3) and the route is INVISIBLE: authMiddleware (middleware.go) matches
// every inbound path against the registry and answers
//
//	404 {"error":{"code":"GATEWAY_ROUTE_NOT_FOUND","message":"route not found"}}
//
// BEFORE the mux is ever consulted. Everything compiles, every unit test passes,
// the aggregator tests pass (they call the method directly), and the route is
// still dead in production. That is exactly how CHO-2148's H+ egress route
// shipped broken and was only caught on the live walk.
//
// Memory: [[project_gateway_route_three_list_sync]].
//
// This test closes the loop: every /api/v1/admin/... path the phyllis router
// mounts MUST resolve through the registry. A new admin mount with no registry
// entry now fails here, loudly, at build time.
package httpadapter

import (
	"context"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/adapter/inmem"
)

// adminRoutesMountedInPhyllis lists the /api/v1/admin/* leaves phyllis_handler.go
// mounts on the mux. Keep it in step with the mux.HandleFunc calls there.
//
// If you add an admin mount and do NOT add it here, you lose the guard. If you
// add it here and NOT to the registry, this test fails — which is the point.
var adminRoutesMountedInPhyllis = []string{
	"/api/v1/admin/tenants/me/addons",
	"/api/v1/admin/tenants/me/mana-pool",
	"/api/v1/admin/tenants/me/invoices",
	"/api/v1/admin/tenants/me/billing-portal",
	"/api/v1/admin/marketplace/addons",
	// CHO-2148 — the route this test was written for.
	"/api/v1/admin/tenants/me/external-egress",
}

func TestRouteRegistry_ResolvesEveryPhyllisAdminMount(t *testing.T) {
	t.Parallel()
	repo := inmem.NewRouteRepository()
	ctx := context.Background()

	for _, path := range adminRoutesMountedInPhyllis {
		if _, err := repo.Match(ctx, path); err != nil {
			t.Errorf(
				"route %q is mounted on the phyllis mux but is NOT in the route registry.\n"+
					"authMiddleware 404s it (GATEWAY_ROUTE_NOT_FOUND) BEFORE the mux runs, so the\n"+
					"route is dead in production even though it compiles and its unit tests pass.\n"+
					"FIX: add a BFFRoute entry in internal/adapter/inmem/route_repository.go.",
				path,
			)
		}
	}
}

// The external-egress route specifically — the one that shipped broken.
func TestRouteRegistry_ExternalEgressIsRegistered(t *testing.T) {
	t.Parallel()
	repo := inmem.NewRouteRepository()

	matched, err := repo.Match(context.Background(), "/api/v1/admin/tenants/me/external-egress")
	if err != nil {
		t.Fatalf("the H+ external-egress route is not in the registry: %v", err)
	}
	if matched.BackendService != "chora-tenancy" {
		t.Errorf("BackendService = %q, want chora-tenancy (it proxies to the entitlement owner)",
			matched.BackendService)
	}
}
