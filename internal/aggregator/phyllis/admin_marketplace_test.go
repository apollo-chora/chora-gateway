// admin_marketplace_test.go — RED-phase tests for the H+ Add-on
// Marketplace catalog browse aliases (CHO-1735).
//
// Two endpoints, both tenant-agnostic catalog reads (no `me` rewrite):
//
//	GET /api/v1/admin/marketplace/addons?<query>      — paginated list
//	GET /api/v1/admin/marketplace/addons/{addonPlanId} — detail
//
// Hard-coding the canonical backend mount paths (per
// feedback_bff_aggregator_path_test) prevents the path-stripping bug
// class CHO-1632 hit in deploy.
package phyllis_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

const testMarketplacePlanID = "019e0000-0000-7000-8000-bbbbbbbbbbbb"

// -----------------------------------------------------------------------------
// ListMarketplaceAddons
// -----------------------------------------------------------------------------

func TestListMarketplaceAddons_HappyPath_QueryForwardedVerbatim(t *testing.T) {
	var seenPath, seenMethod, seenRawQuery, seenAuth, seenGCID, seenTenantID, seenTrace string
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenRawQuery = r.URL.RawQuery
		seenMethod = r.Method
		seenAuth = r.Header.Get("Authorization")
		seenGCID = r.Header.Get("gcid")
		seenTenantID = r.Header.Get("X-Tenant-Id")
		seenTrace = r.Header.Get("traceparent")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[{"addon_plan_id":"019e0000-0000-7000-8000-bbbbbbbbbbbb","code":"companion","display_name":"Companion","category":"AI"}],"next_cursor":null}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	rawQuery := "category=AI&tier=pro&cursor=abc&limit=20"
	res, _ := a.ListMarketplaceAddons(context.Background(), rawQuery,
		phyllis.AuthCtx{
			Bearer:      "phyllis",
			GCID:        "01935f12-0000-7000-8000-0000000000ff",
			TenantID:    "01970000-0000-7000-8000-000000000001",
			Traceparent: "00-trace-id-01",
		},
	)

	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", res.Status)
	}
	if seenMethod != http.MethodGet {
		t.Errorf("upstream method = %q; want GET", seenMethod)
	}
	// Tenant-agnostic catalog — canonical backend path is the SAME as the
	// FE alias (no `me` rewrite, no `{tenantId}` substitution).
	wantPath := "/api/v1/admin/marketplace/addons"
	if seenPath != wantPath {
		t.Errorf("upstream path = %q; want %q (catalog is tenant-agnostic)",
			seenPath, wantPath)
	}
	if seenRawQuery != rawQuery {
		t.Errorf("upstream raw query = %q; want %q (must forward verbatim)",
			seenRawQuery, rawQuery)
	}
	if seenAuth != "Bearer phyllis" {
		t.Errorf("upstream Authorization = %q", seenAuth)
	}
	if seenGCID != "01935f12-0000-7000-8000-0000000000ff" {
		t.Errorf("upstream gcid header = %q", seenGCID)
	}
	if seenTenantID != "01970000-0000-7000-8000-000000000001" {
		t.Errorf("upstream X-Tenant-Id = %q (must be stamped from AuthCtx.TenantID)", seenTenantID)
	}
	if seenTrace != "00-trace-id-01" {
		t.Errorf("upstream traceparent = %q", seenTrace)
	}
	if !strings.Contains(string(res.Body), `"display_name":"Companion"`) {
		t.Errorf("response body did not forward catalog item; got %s", res.Body)
	}
}

// Empty query string is OK — BE returns the default page.
func TestListMarketplaceAddons_HappyPath_EmptyQueryOK(t *testing.T) {
	var seenRawQuery string
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenRawQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[],"next_cursor":null}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.ListMarketplaceAddons(context.Background(), "",
		phyllis.AuthCtx{Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001"},
	)
	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", res.Status)
	}
	if seenRawQuery != "" {
		t.Errorf("upstream raw query = %q; want empty", seenRawQuery)
	}
}

// Upstream 5xx passes through as 502 via classify().
func TestListMarketplaceAddons_Upstream500_NormalisesTo502(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.ListMarketplaceAddons(context.Background(), "",
		phyllis.AuthCtx{Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001"},
	)
	if res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 (upstream 5xx normalised by classify)", res.Status)
	}
}

// -----------------------------------------------------------------------------
// GetMarketplaceAddonDetail
// -----------------------------------------------------------------------------

func TestGetMarketplaceAddonDetail_HappyPath(t *testing.T) {
	var seenPath, seenMethod, seenAuth, seenGCID, seenTenantID string
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenMethod = r.Method
		seenAuth = r.Header.Get("Authorization")
		seenGCID = r.Header.Get("gcid")
		seenTenantID = r.Header.Get("X-Tenant-Id")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"addon_plan_id":"019e0000-0000-7000-8000-bbbbbbbbbbbb","code":"companion","display_name":"Companion","category":"AI","is_installed":false}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.GetMarketplaceAddonDetail(context.Background(), testMarketplacePlanID,
		phyllis.AuthCtx{
			Bearer:   "phyllis",
			GCID:     "01935f12-0000-7000-8000-0000000000ff",
			TenantID: "01970000-0000-7000-8000-000000000001",
		},
	)

	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", res.Status)
	}
	if seenMethod != http.MethodGet {
		t.Errorf("upstream method = %q; want GET", seenMethod)
	}
	wantPath := "/api/v1/admin/marketplace/addons/" + testMarketplacePlanID
	if seenPath != wantPath {
		t.Errorf("upstream path = %q; want %q (catalog is tenant-agnostic)",
			seenPath, wantPath)
	}
	if seenAuth != "Bearer phyllis" {
		t.Errorf("upstream Authorization = %q", seenAuth)
	}
	if seenGCID != "01935f12-0000-7000-8000-0000000000ff" {
		t.Errorf("upstream gcid header = %q", seenGCID)
	}
	if seenTenantID != "01970000-0000-7000-8000-000000000001" {
		t.Errorf("upstream X-Tenant-Id = %q (must stamp from AuthCtx.TenantID for is_installed lookup)", seenTenantID)
	}
	if !strings.Contains(string(res.Body), `"is_installed":false`) {
		t.Errorf("response body did not forward is_installed flag; got %s", res.Body)
	}
}

// 404 (unknown plan) passes through so the FE can render the not-found state.
func TestGetMarketplaceAddonDetail_NotFound_404_PassThrough(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.GetMarketplaceAddonDetail(context.Background(), testMarketplacePlanID,
		phyllis.AuthCtx{Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001"},
	)
	if res.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 pass-through", res.Status)
	}
}

// Upstream 5xx passes through as 502 via classify().
func TestGetMarketplaceAddonDetail_Upstream500_NormalisesTo502(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.GetMarketplaceAddonDetail(context.Background(), testMarketplacePlanID,
		phyllis.AuthCtx{Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001"},
	)
	if res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", res.Status)
	}
}

// -----------------------------------------------------------------------------
// CHO-1748 (CHO-1745 Sub 3) — system + installed pass-through regression guards
//
// The aggregator is a verbatim proxy for marketplace responses, so these
// assertions enforce that future refactors don't accidentally start
// transforming the body (e.g. by introducing a typed DTO that drops unknown
// fields) and silently strip system / installed before they reach the FE.
//
// Path assertions reinforce feedback_bff_aggregator_path_test: the canonical
// backend mount is `/api/v1/admin/marketplace/addons[/{addonPlanId}]` — the
// catalog is tenant-agnostic so there's no `me` → `{tenant_id}` rewrite, but
// the test still asserts the path explicitly so any future per-tenant
// rewrite proposal triggers an obvious test churn rather than slipping
// through silently.
// -----------------------------------------------------------------------------

func TestListMarketplaceAddons_SystemAndInstalledPassThrough(t *testing.T) {
	var seenPath string
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[` +
			`{"addon_plan_id":"019e0000-0000-7000-8000-aaaaaaaaaaaa","code":"base","display_name":"Chora Base","category":"Core","system":true,"installed":true},` +
			`{"addon_plan_id":"019e0000-0000-7000-8000-bbbbbbbbbbbb","code":"companion","display_name":"Companion","category":"AI","system":false,"installed":false}` +
			`],"next_cursor":null}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.ListMarketplaceAddons(context.Background(), "",
		phyllis.AuthCtx{Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001"},
	)

	if res.Status != http.StatusOK {
		t.Fatalf("status = %d; want 200", res.Status)
	}
	if seenPath != "/api/v1/admin/marketplace/addons" {
		t.Errorf("upstream path = %q; want canonical /api/v1/admin/marketplace/addons (no `me` rewrite)", seenPath)
	}
	body := string(res.Body)
	if !strings.Contains(body, `"system":true`) {
		t.Errorf("response body missing system=true for base; got %s", body)
	}
	if !strings.Contains(body, `"installed":true`) {
		t.Errorf("response body missing installed=true for base; got %s", body)
	}
	if !strings.Contains(body, `"system":false`) {
		t.Errorf("response body missing system=false for companion; got %s", body)
	}
	if !strings.Contains(body, `"installed":false`) {
		t.Errorf("response body missing installed=false for companion; got %s", body)
	}
}

func TestGetMarketplaceAddonDetail_SystemAndInstalledPassThrough(t *testing.T) {
	var seenPath string
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"addon_plan_id":"019e0000-0000-7000-8000-aaaaaaaaaaaa","code":"base","display_name":"Chora Base","category":"Core","system":true,"installed":true,"is_installed":true,"cta":"Manage"}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.GetMarketplaceAddonDetail(context.Background(), testMarketplacePlanID,
		phyllis.AuthCtx{Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001"},
	)

	if res.Status != http.StatusOK {
		t.Fatalf("status = %d; want 200", res.Status)
	}
	wantPath := "/api/v1/admin/marketplace/addons/" + testMarketplacePlanID
	if seenPath != wantPath {
		t.Errorf("upstream path = %q; want %q (no `me` rewrite)", seenPath, wantPath)
	}
	body := string(res.Body)
	if !strings.Contains(body, `"system":true`) {
		t.Errorf("response body missing system flag; got %s", body)
	}
	if !strings.Contains(body, `"installed":true`) {
		t.Errorf("response body missing canonical installed flag; got %s", body)
	}
	// `is_installed` is kept as a legacy alias by chora-tenancy (CHO-1747)
	// for the transitional window until CHO-1749 cuts the FE consumer over;
	// the aggregator must forward it unchanged too.
	if !strings.Contains(body, `"is_installed":true`) {
		t.Errorf("response body missing legacy is_installed alias; got %s", body)
	}
}
