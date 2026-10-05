// admin_addons_cancel_deactivation_test.go — CHO-1785 follow-up.
//
// Mirrors admin_addons_deactivate_test.go but for the new
// :cancel-deactivation action shipped in PR #99. The FE hits
//
//	POST /api/v1/admin/tenants/me/addons/{addonPlanId}:cancel-deactivation
//
// and the aggregator forwards to the canonical parametric path
//
//	/api/v1/admin/tenants/{tenantId}/addons/{addonPlanId}:cancel-deactivation
//
// Hard-coding the canonical path (per feedback_bff_aggregator_path_test —
// CHO-1632 lesson) prevents path-stripping regressions in deploy.
package phyllis_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

func TestCancelDeactivationAdminMyAddon_HappyPath(t *testing.T) {
	var seenPath, seenMethod, seenAuth, seenGCID, seenTenantID string
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenMethod = r.Method
		seenAuth = r.Header.Get("Authorization")
		seenGCID = r.Header.Get("gcid")
		seenTenantID = r.Header.Get("X-Tenant-Id")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ACTIVE","current_tier":"starter"}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.CancelDeactivationAdminMyAddon(context.Background(), testPlanID,
		phyllis.AuthCtx{
			Bearer:   "phyllis",
			GCID:     "01935f12-0000-7000-8000-0000000000ff",
			TenantID: "01970000-0000-7000-8000-000000000001",
		},
	)

	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", res.Status)
	}
	if seenMethod != http.MethodPost {
		t.Errorf("upstream method = %q; want POST", seenMethod)
	}
	wantPath := "/api/v1/admin/tenants/01970000-0000-7000-8000-000000000001/addons/" + testPlanID + ":cancel-deactivation"
	if seenPath != wantPath {
		t.Errorf("upstream path = %q; want %q (parametric backend; me alias rewritten)", seenPath, wantPath)
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
	if !strings.Contains(string(res.Body), `"status":"ACTIVE"`) {
		t.Errorf("response body did not forward ACTIVE; got %s", res.Body)
	}
}

func TestCancelDeactivationAdminMyAddon_MissingTenantID_400(t *testing.T) {
	// AuthCtx.TenantID empty short-circuits to 400 before the upstream
	// call — same gate as DeactivateAdminMyAddon.
	a := phyllis.New(phyllis.Config{TenancyURL: "http://unused", PerCallTimeout: time.Second}, nil)
	res, _ := a.CancelDeactivationAdminMyAddon(context.Background(), testPlanID,
		phyllis.AuthCtx{Bearer: "x", GCID: "g"},
	)
	if res.Status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", res.Status)
	}
	if !strings.Contains(string(res.Body), "GATEWAY_TENANT_NOT_RESOLVED") {
		t.Errorf("body = %s; want GATEWAY_TENANT_NOT_RESOLVED", res.Body)
	}
}
