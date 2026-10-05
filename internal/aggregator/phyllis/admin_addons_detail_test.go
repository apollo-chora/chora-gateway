// admin_addons_detail_test.go — RED-phase tests for the H+ Add-on
// Lifecycle marketplace tile detail BFF alias (CHO-1734, STITCH-H-ADD-5
// — final sub-story closing epic CHO-1697).
//
// The FE hits GET /api/v1/admin/tenants/me/addons/{addonPlanId}. This
// aggregator method must rewrite `me` to AuthCtx.TenantID and forward
// to the canonical backend
//
//	GET /api/v1/admin/tenants/{tenantId}/addons/{addonPlanId}
//
// returning the full `AddOnDetail` envelope (entitlements, integrations,
// compliance, pricing_tiers, current_subscription) + stamping AuthCtx
// headers. Hard-coding the canonical path (per
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

const testDetailPlanID = "019e0000-0000-7000-8000-bbbbbbbbbbbb"

func TestGetAdminMyAddonDetail_HappyPath(t *testing.T) {
	var seenPath, seenMethod, seenAuth, seenGCID, seenTenantID, seenTrace string
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenMethod = r.Method
		seenAuth = r.Header.Get("Authorization")
		seenGCID = r.Header.Get("gcid")
		seenTenantID = r.Header.Get("X-Tenant-Id")
		seenTrace = r.Header.Get("traceparent")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"addon_plan_id":"019e0000-0000-7000-8000-bbbbbbbbbbbb","code":"companion","display_name":"Companion","category":"AI","entitlements":[],"integrations":[],"compliance":{"gdpr":true,"pdpa":true,"ccpa":false,"imda":true,"data_residency_regions":["SG"]},"pricing_tiers":[{"tier_code":"pro","label":"Pro","monthly_price_cents":2400,"currency":"USD"}],"current_subscription":{"activated_at":"2026-06-08T00:00:00Z","current_tier":"pro","status":"ACTIVE","billing_cycle":"MONTHLY"}}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.GetAdminMyAddonDetail(context.Background(), testDetailPlanID,
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
	// Canonical backend path — parametric on tenantId, NOT the literal "me".
	wantPath := "/api/v1/admin/tenants/01970000-0000-7000-8000-000000000001/addons/" + testDetailPlanID
	if seenPath != wantPath {
		t.Errorf("upstream path = %q; want %q (parametric backend; me alias rewritten)",
			seenPath, wantPath)
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
		t.Errorf("response body did not forward display_name; got %s", res.Body)
	}
	if !strings.Contains(string(res.Body), `"current_tier":"pro"`) {
		t.Errorf("response body did not forward current_subscription; got %s", res.Body)
	}
}

// Missing tenant context → 400 BEFORE any upstream call.
func TestGetAdminMyAddonDetail_MissingTenantId_400(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("upstream MUST NOT be called when tenant_id is missing")
		w.WriteHeader(http.StatusInternalServerError)
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.GetAdminMyAddonDetail(context.Background(), testDetailPlanID,
		phyllis.AuthCtx{Bearer: "phyllis"},
	)
	if res.Status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 GATEWAY_TENANT_NOT_RESOLVED", res.Status)
	}
}

// 404 (tenant not subscribed) passes through so the FE can render the
// "Not currently subscribed" empty state.
func TestGetAdminMyAddonDetail_NotFound_404_PassThrough(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.GetAdminMyAddonDetail(context.Background(), testDetailPlanID,
		phyllis.AuthCtx{Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001"},
	)
	if res.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 pass-through", res.Status)
	}
}

// Upstream 5xx passes through as 502 via classify().
func TestGetAdminMyAddonDetail_Upstream500_NormalisesTo502(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.GetAdminMyAddonDetail(context.Background(), testDetailPlanID,
		phyllis.AuthCtx{Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001"},
	)
	if res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 (upstream 5xx normalised by classify)", res.Status)
	}
}
