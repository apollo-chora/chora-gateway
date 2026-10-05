// admin_addons_usage_test.go — RED-phase tests for the H+ Add-on
// Lifecycle usage analytics BFF alias (CHO-1732, STITCH-H-ADD-3).
//
// The FE hits GET /api/v1/admin/tenants/me/addons/{addonPlanId}/usage?
//
//	from=&to=&granularity=
//
// This aggregator method must rewrite `me` to AuthCtx.TenantID and
// forward to the canonical backend GET
//
//	/api/v1/admin/tenants/{tenantId}/addons/{addonPlanId}/usage?<query>
//
// preserving the query string verbatim + stamping AuthCtx headers
// (gcid / X-Tenant-Id / traceparent / Authorization). Hard-coding the
// canonical path (per feedback_bff_aggregator_path_test) prevents the
// path-stripping bug class CHO-1632 hit in deploy.
package phyllis_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

const testUsagePlanID = "019e0000-0000-7000-8000-bbbbbbbbbbbb"

func TestGetAdminMyAddonUsage_HappyPath_QueryForwardedVerbatim(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"addon_plan_id":"019e0000-0000-7000-8000-bbbbbbbbbbbb","tenant_id":"01970000-0000-7000-8000-000000000001","period_from":"2026-05-13T00:00:00Z","period_to":"2026-06-12T00:00:00Z","granularity":"day","seats_total":50,"seats_used":12,"seats_used_peak":18,"time_series":[],"top_consumers":[]}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	rawQuery := "from=2026-05-13T00%3A00%3A00Z&to=2026-06-12T00%3A00%3A00Z&granularity=day"
	res, _ := a.GetAdminMyAddonUsage(context.Background(), testUsagePlanID, rawQuery,
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
	wantPath := "/api/v1/admin/tenants/01970000-0000-7000-8000-000000000001/addons/" + testUsagePlanID + "/usage"
	if seenPath != wantPath {
		t.Errorf("upstream path = %q; want %q (parametric backend; me alias rewritten)",
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
	if !strings.Contains(string(res.Body), `"granularity":"day"`) {
		t.Errorf("response body did not forward granularity; got %s", res.Body)
	}
}

// Empty query string is OK — BE applies its own defaults.
func TestGetAdminMyAddonUsage_HappyPath_EmptyQueryOK(t *testing.T) {
	var seenRawQuery string
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenRawQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"granularity":"day","time_series":[],"top_consumers":[]}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.GetAdminMyAddonUsage(context.Background(), testUsagePlanID, "",
		phyllis.AuthCtx{Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001"},
	)
	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200 (BE-side defaulting)", res.Status)
	}
	if seenRawQuery != "" {
		t.Errorf("upstream raw query = %q; want empty (BE applies defaults)", seenRawQuery)
	}
}

// Missing tenant context → 400 BEFORE any upstream call.
func TestGetAdminMyAddonUsage_MissingTenantId_400(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("upstream MUST NOT be called when tenant_id is missing")
		w.WriteHeader(http.StatusInternalServerError)
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.GetAdminMyAddonUsage(context.Background(), testUsagePlanID, "from=x",
		phyllis.AuthCtx{Bearer: "phyllis"},
	)
	if res.Status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 GATEWAY_TENANT_NOT_RESOLVED", res.Status)
	}
}

// 404 (subscription not found) passes through so the FE can render the
// not-found branch of its discriminated result union.
func TestGetAdminMyAddonUsage_NotFound_404_PassThrough(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"subscription_not_found"}}`))
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.GetAdminMyAddonUsage(context.Background(), testUsagePlanID, "",
		phyllis.AuthCtx{Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001"},
	)
	if res.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 pass-through", res.Status)
	}
}

// Upstream 5xx passes through as 502 via classify().
func TestGetAdminMyAddonUsage_Upstream500_NormalisesTo502(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.GetAdminMyAddonUsage(context.Background(), testUsagePlanID, "",
		phyllis.AuthCtx{Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001"},
	)
	if res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 (upstream 5xx normalised by classify)", res.Status)
	}
}
