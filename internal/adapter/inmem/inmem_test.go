package inmem_test

import (
	"context"
	"errors"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/adapter/inmem"
	"github.com/apollo-chora/chora-gateway/internal/domain/route"
	"github.com/apollo-chora/chora-gateway/internal/domain/session"
)

func TestSessionRepository_SaveGetDelete(t *testing.T) {
	repo := inmem.NewSessionRepository()
	s, _ := session.New(session.NewParams{Gcid: "g", TenantID: "t", Roles: []string{"learner"}})

	ctx := context.Background()
	if err := repo.Save(ctx, s); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := repo.Get(ctx, s.Token)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Gcid != s.Gcid {
		t.Errorf("gcid mismatch: %q vs %q", got.Gcid, s.Gcid)
	}
	if err := repo.Delete(ctx, s.Token); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := repo.Get(ctx, s.Token); !errors.Is(err, session.ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound after delete, got %v", err)
	}
}

func TestSessionRepository_DeleteIdempotent(t *testing.T) {
	repo := inmem.NewSessionRepository()
	if err := repo.Delete(context.Background(), "no-such-token"); err != nil {
		t.Errorf("delete on missing token should be idempotent: %v", err)
	}
}

func TestSessionRepository_GetMissing(t *testing.T) {
	repo := inmem.NewSessionRepository()
	if _, err := repo.Get(context.Background(), "x"); !errors.Is(err, session.ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestRouteRepository_DefaultsMatch(t *testing.T) {
	repo := inmem.NewRouteRepository()
	ctx := context.Background()

	cases := map[string]string{
		"/bff/aplus/home":       "chora-consumption",
		"/bff/cplus/feed":       "chora-sharing",
		"/bff/hplus/tenant":     "chora-tenancy",
		"/bff/oplus/governance": "chora-governance",
		"/bff/rplus/courses":    "chora-delivery",
		"/api/auth/session":     "chora-identity",
	}
	for path, wantBackend := range cases {
		r, err := repo.Match(ctx, path)
		if err != nil {
			t.Errorf("Match(%q) err=%v", path, err)
			continue
		}
		if r.BackendService != wantBackend {
			t.Errorf("Match(%q).BackendService=%q want %q", path, r.BackendService, wantBackend)
		}
	}
}

func TestRouteRepository_ProxyWildcard(t *testing.T) {
	repo := inmem.NewRouteRepository()
	r, err := repo.Match(context.Background(), "/api/proxy/chora-creation/atoms/123")
	if err != nil {
		t.Fatalf("expected wildcard match, err=%v", err)
	}
	if r.PathPattern != "/api/proxy/*" {
		t.Errorf("matched pattern = %q; want /api/proxy/*", r.PathPattern)
	}
}

func TestRouteRepository_NotFound(t *testing.T) {
	repo := inmem.NewRouteRepository()
	if _, err := repo.Match(context.Background(), "/nope"); !errors.Is(err, route.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// CHO-1655 Phase A — the Setup Wizard step 1 Branding route MUST be in
// the in-memory route table. authMiddleware (middleware.go) calls
// routes.Match BEFORE the inner mux dispatch — if the path isn't seeded
// here, every authenticated PATCH /api/v1/tenants/me/branding gets a
// 404 GATEWAY_ROUTE_NOT_FOUND, even though the handler is registered
// on the mux (phyllis_handler.go) and the JWT gate prefix entry exists
// (jwt_auth.go DefaultJWTGatedPrefixes). The mux registration alone is
// not enough — the route MUST also be advertised by NewRouteRepository
// so the auth layer admits it to the dispatch path.
func TestRouteRepository_TenantsMeBrandingRouted(t *testing.T) {
	repo := inmem.NewRouteRepository()
	r, err := repo.Match(context.Background(), "/api/v1/tenants/me/branding")
	if err != nil {
		t.Fatalf("Match(/api/v1/tenants/me/branding) err=%v — Setup Wizard branding will 404 at the gateway", err)
	}
	if r.BackendService != "chora-tenancy" {
		t.Errorf("BackendService=%q want chora-tenancy", r.BackendService)
	}
}

// CHO-1664 Phase B — same 3-layer-wiring lesson as Phase A. Adding the
// handler to the mux + the entry to DefaultJWTGatedPrefixes is not
// enough on its own; without an entry here, authMiddleware short-
// circuits with 404 GATEWAY_ROUTE_NOT_FOUND for every authenticated
// POST /api/v1/tenants/me/addons. See feedback_gateway_three_layer_route_wiring.
func TestRouteRepository_TenantsMeAddonsRouted(t *testing.T) {
	repo := inmem.NewRouteRepository()
	r, err := repo.Match(context.Background(), "/api/v1/tenants/me/addons")
	if err != nil {
		t.Fatalf("Match(/api/v1/tenants/me/addons) err=%v — Setup Wizard step 2 will 404 at the gateway", err)
	}
	if r.BackendService != "chora-tenancy" {
		t.Errorf("BackendService=%q want chora-tenancy", r.BackendService)
	}
}

// CHO-1682 — Setup Wizard step 4 Apply aggregator. The route must be in
// the inmem table so the gateway recognises it (matches phyllis_handler.go
// mux entry + jwt_auth.go DefaultJWTGatedPrefixes entry — 3-layer wiring
// discipline). BackendService=chora-gateway because the BFF answers the
// route locally and fans out to identity + tenancy.
func TestRouteRepository_TenantsSetupRouted(t *testing.T) {
	repo := inmem.NewRouteRepository()
	r, err := repo.Match(context.Background(), "/api/v1/tenants/setup")
	if err != nil {
		t.Fatalf("Match(/api/v1/tenants/setup) err=%v — Setup Wizard step 4 will 404 at the gateway", err)
	}
	if r.BackendService != "chora-gateway" {
		t.Errorf("BackendService=%q want chora-gateway (BFF answers locally + fans out)", r.BackendService)
	}
}

// CHO-1698 STITCH-H-ADD-1 — H+ Add-on Lifecycle dashboard alias.
// /api/v1/admin/tenants/me/addons proxies to chora-tenancy's parametric
// admin endpoint; the route_repository entry pins the 3-layer wiring
// (mux + JWT prefix + this entry — per feedback_gateway_three_layer_route_wiring).
func TestRouteRepository_AdminTenantsMeAddonsRouted(t *testing.T) {
	repo := inmem.NewRouteRepository()
	r, err := repo.Match(context.Background(), "/api/v1/admin/tenants/me/addons")
	if err != nil {
		t.Fatalf("Match(/api/v1/admin/tenants/me/addons) err=%v — H+ Add-on dashboard will 404 at the gateway", err)
	}
	if r.BackendService != "chora-tenancy" {
		t.Errorf("BackendService=%q want chora-tenancy", r.BackendService)
	}
}

// CHO-1731 STITCH-H-ADD-2 — H+ Add-on deactivation modal alias.
// /api/v1/admin/tenants/me/addons/{addonPlanId}:deactivate proxies to
// chora-tenancy's parametric admin endpoint; the route_repository entry
// pins the 3-layer wiring (mux + JWT prefix + this entry — per
// feedback_gateway_three_layer_route_wiring). Without this entry the
// deactivation POST would 404 at the gateway with GATEWAY_ROUTE_NOT_FOUND
// (the CHO-1655 bug class).
func TestRouteRepository_AdminTenantsMeAddonsDeactivateRouted(t *testing.T) {
	repo := inmem.NewRouteRepository()
	// Sub-path must match — the FE URL is parametric on {addonPlanId} and
	// carries the `:deactivate` action suffix.
	r, err := repo.Match(context.Background(),
		"/api/v1/admin/tenants/me/addons/019e0000-0000-7000-8000-bbbbbbbbbbbb:deactivate")
	if err != nil {
		t.Fatalf("Match(deactivate path) err=%v — H+ Add-on deactivation will 404 at the gateway", err)
	}
	if r.BackendService != "chora-tenancy" {
		t.Errorf("BackendService=%q want chora-tenancy", r.BackendService)
	}
}

// CHO-1708 WP-4 — `:auto-renew` me-style mana-pool alias. Matches() is
// exact-or-`/*`-prefix, and a colon suffix shares no `/` boundary with the
// bare mana-pool entry, so the route table needs an EXPLICIT entry (same
// lesson as `:topup`). Without it, authMiddleware 404s the route before the
// mux handler ever runs (feedback_gateway_three_layer_route_wiring).
func TestRouteRepository_AdminTenantsMeManaPoolAutoRenewRouted(t *testing.T) {
	repo := inmem.NewRouteRepository()
	r, err := repo.Match(context.Background(), "/api/v1/admin/tenants/me/mana-pool:auto-renew")
	if err != nil {
		t.Fatalf("Match(/api/v1/admin/tenants/me/mana-pool:auto-renew) err=%v — H+ auto-renew will 404 at the gateway", err)
	}
	if r.BackendService != "chora-tenancy" {
		t.Errorf("BackendService=%q want chora-tenancy", r.BackendService)
	}
}

// CHO-1735 — H+ Marketplace catalog browse routes. authMiddleware calls
// Match() BEFORE the inner mux dispatch; missing route_repository entries
// silently 404 with GATEWAY_ROUTE_NOT_FOUND. Per
// `feedback_gateway_three_layer_route_wiring` we pin both endpoints.
func TestRouteRepository_AdminMarketplaceAddonsRouted(t *testing.T) {
	repo := inmem.NewRouteRepository()
	// List endpoint — exact match.
	r, err := repo.Match(context.Background(), "/api/v1/admin/marketplace/addons")
	if err != nil {
		t.Fatalf("Match(list) err=%v — H+ Marketplace will 404 at the gateway", err)
	}
	if r.BackendService != "chora-tenancy" {
		t.Errorf("BackendService=%q want chora-tenancy", r.BackendService)
	}
	// Detail endpoint — wildcard captures {addonPlanId}.
	r2, err2 := repo.Match(context.Background(),
		"/api/v1/admin/marketplace/addons/019e0000-0000-7000-8000-bbbbbbbbbbbb")
	if err2 != nil {
		t.Fatalf("Match(detail) err=%v — H+ Marketplace detail will 404 at the gateway", err2)
	}
	if r2.BackendService != "chora-tenancy" {
		t.Errorf("detail BackendService=%q want chora-tenancy", r2.BackendService)
	}
}

func TestRouteRepository_All(t *testing.T) {
	repo := inmem.NewRouteRepository()
	all, err := repo.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) == 0 {
		t.Error("expected non-empty route table")
	}
}
