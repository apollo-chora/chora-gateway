// admin_addons_test.go — RED-phase tests for the H+ Add-on Lifecycle
// dashboard BFF alias (CHO-1698, STITCH-H-ADD-1).
//
// The FE hits `GET /api/v1/admin/tenants/me/addons`; this aggregator
// method must rewrite the URL to the parametric backend path
// `/api/v1/admin/tenants/{tenantId}/addons` with tenantId resolved
// from the validated JWT in `AuthCtx`. Hard-coding the canonical
// backend mount path (per feedback_bff_aggregator_path_test) prevents
// the path-stripping bug class CHO-1632 hit in deploy.
package phyllis_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

func TestListAdminMyAddons_HappyPath(t *testing.T) {
	var seenPath, seenMethod, seenAuth, seenGCID, seenTenantID, seenTrace string
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenMethod = r.Method
		seenAuth = r.Header.Get("Authorization")
		seenGCID = r.Header.Get("gcid")
		seenTenantID = r.Header.Get("X-Tenant-Id")
		seenTrace = r.Header.Get("traceparent")
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[{"tenant_id":"01970000-0000-7000-8000-000000000001","addon_plan_id":"019e0000-0000-7000-8000-bbbbbbbbbbbb","addon_code":"companion","display_name":"Companion","status":"ACTIVE","current_tier":"pro","monthly_cost":2400,"seats_used":5,"activated_at":"2026-06-08T00:00:00Z","deactivated_at":null}]}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.ListAdminMyAddons(context.Background(),
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
	// Canonical backend mount path — parametric on tenantId, NOT the literal "me".
	wantPath := "/api/v1/admin/tenants/01970000-0000-7000-8000-000000000001/addons"
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
	// Response body passed through verbatim.
	if !strings.Contains(string(res.Body), `"addon_code":"companion"`) {
		t.Errorf("response body did not forward addon row; got %s", res.Body)
	}
}

// Missing tenant context → 400 BEFORE the upstream call (defense in depth).
func TestListAdminMyAddons_MissingTenantId_400(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("upstream MUST NOT be called when tenant_id is missing")
		w.WriteHeader(http.StatusInternalServerError)
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.ListAdminMyAddons(context.Background(),
		phyllis.AuthCtx{Bearer: "phyllis"},
	)
	if res.Status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 GATEWAY_TENANT_NOT_RESOLVED", res.Status)
	}
}

// Upstream 5xx passes through as 502 via classify().
func TestListAdminMyAddons_Upstream500_NormalisesTo502(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.ListAdminMyAddons(context.Background(),
		phyllis.AuthCtx{
			Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001",
		},
	)
	if res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 (upstream 5xx normalised by classify)", res.Status)
	}
}
