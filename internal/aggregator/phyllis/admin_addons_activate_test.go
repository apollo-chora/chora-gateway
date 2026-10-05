// admin_addons_activate_test.go — RED-phase tests for the H+ Marketplace
// Activate-Free BFF alias (CHO-1742). Sibling of
// admin_addons_deactivate_test.go.
//
// Hard-coding the canonical backend path here per
// `feedback_bff_aggregator_path_test` — the test asserts the parametric
// /tenants/{tenantId}/addons/{planId}:activate URL, not whatever the
// aggregator happens to echo back.
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

func TestActivateAdminMyAddon_HappyPath_200(t *testing.T) {
	var seenPath, seenMethod, seenAuth, seenGCID, seenTenantID, seenTrace string
	var seenBody []byte
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenMethod = r.Method
		seenAuth = r.Header.Get("Authorization")
		seenGCID = r.Header.Get("gcid")
		seenTenantID = r.Header.Get("X-Tenant-Id")
		seenTrace = r.Header.Get("traceparent")
		seenBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"sub-1","tenant_id":"01970000-0000-7000-8000-000000000001","add_on_id":"knowledge_graph","addon_plan_id":"019e0000-0000-7000-8000-bbbbbbbbbbbb","status":"active"}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.ActivateAdminMyAddon(context.Background(), testPlanID, []byte(``),
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
	if seenMethod != http.MethodPost {
		t.Errorf("upstream method = %q; want POST", seenMethod)
	}
	wantPath := "/api/v1/admin/tenants/01970000-0000-7000-8000-000000000001/addons/" + testPlanID + ":activate"
	if seenPath != wantPath {
		t.Errorf("upstream path = %q; want %q (parametric backend; me alias rewritten)",
			seenPath, wantPath)
	}
	if len(seenBody) != 0 {
		t.Errorf("upstream body = %q; want empty (activate is body-less)", seenBody)
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
	if !strings.Contains(string(res.Body), `"status":"active"`) {
		t.Errorf("response body did not forward active status; got %s", res.Body)
	}
}

func TestActivateAdminMyAddon_MissingTenantId_400(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("upstream MUST NOT be called when tenant_id is missing")
		w.WriteHeader(http.StatusInternalServerError)
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.ActivateAdminMyAddon(context.Background(), testPlanID, nil,
		phyllis.AuthCtx{Bearer: "phyllis"},
	)
	if res.Status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 GATEWAY_TENANT_NOT_RESOLVED", res.Status)
	}
}

func TestActivateAdminMyAddon_NotFound_404_PassThrough(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"addon_not_found"}}`))
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.ActivateAdminMyAddon(context.Background(), testPlanID, nil,
		phyllis.AuthCtx{
			Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001",
		},
	)
	if res.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 pass-through", res.Status)
	}
}

func TestActivateAdminMyAddon_Upstream500_NormalisesTo502(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.ActivateAdminMyAddon(context.Background(), testPlanID, nil,
		phyllis.AuthCtx{
			Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001",
		},
	)
	if res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", res.Status)
	}
}
