// admin_addons_change_tier_test.go — RED-phase tests for the H+ Add-on
// Lifecycle change-tier BFF alias (CHO-1733, STITCH-H-ADD-4).
//
// The FE hits PATCH /api/v1/admin/tenants/me/addons/{addonPlanId} with a
// `ChangeAddonTierRequest` body. This aggregator method must rewrite
// `me` to AuthCtx.TenantID and forward to the canonical backend
//
//	PATCH /api/v1/admin/tenants/{tenantId}/addons/{addonPlanId}
//
// preserving the request body verbatim + stamping AuthCtx headers
// (gcid / X-Tenant-Id / traceparent / Authorization). Hard-coding the
// canonical path (per feedback_bff_aggregator_path_test) prevents the
// path-stripping bug class CHO-1632 hit in deploy.
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

const testChangeTierPlanID = "019e0000-0000-7000-8000-bbbbbbbbbbbb"

func TestChangeAdminMyAddonTier_HappyPath(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"subscription_id":"01970000-0000-7000-8000-000000000010","billing_delta_cents":24000,"effective_at":"2026-06-13T00:00:00Z","schedule_id":null,"from_tier":"starter","to_tier":"pro"}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	body := []byte(`{"target_plan_id":"019e0000-0000-7000-8000-cccccccccccc","proration_mode":"create_prorations","effective_at":null}`)
	res, _ := a.ChangeAdminMyAddonTier(context.Background(), testChangeTierPlanID, body,
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
	if seenMethod != http.MethodPatch {
		t.Errorf("upstream method = %q; want PATCH", seenMethod)
	}
	// Canonical backend path — parametric on tenantId, NOT the literal "me".
	wantPath := "/api/v1/admin/tenants/01970000-0000-7000-8000-000000000001/addons/" + testChangeTierPlanID
	if seenPath != wantPath {
		t.Errorf("upstream path = %q; want %q (parametric backend; me alias rewritten)",
			seenPath, wantPath)
	}
	if string(seenBody) != string(body) {
		t.Errorf("upstream body = %q; want %q (change-tier payload must forward verbatim)",
			seenBody, body)
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
	if !strings.Contains(string(res.Body), `"billing_delta_cents":24000`) {
		t.Errorf("response body did not forward billing_delta_cents; got %s", res.Body)
	}
}

// Missing tenant context → 400 BEFORE any upstream call.
func TestChangeAdminMyAddonTier_MissingTenantId_400(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("upstream MUST NOT be called when tenant_id is missing")
		w.WriteHeader(http.StatusInternalServerError)
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.ChangeAdminMyAddonTier(context.Background(), testChangeTierPlanID,
		[]byte(`{"target_plan_id":"x","proration_mode":"create_prorations"}`),
		phyllis.AuthCtx{Bearer: "phyllis"},
	)
	if res.Status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 GATEWAY_TENANT_NOT_RESOLVED", res.Status)
	}
}

// 409 (not upgrade-eligible) passes through unchanged so the FE can
// render the `conflict` branch of its discriminated result union.
func TestChangeAdminMyAddonTier_Conflict_409_PassThrough(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"not_upgrade_eligible","message":"pending cancellation"}}`))
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.ChangeAdminMyAddonTier(context.Background(), testChangeTierPlanID,
		[]byte(`{"target_plan_id":"x","proration_mode":"create_prorations"}`),
		phyllis.AuthCtx{Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001"},
	)
	if res.Status != http.StatusConflict {
		t.Errorf("status = %d; want 409 pass-through", res.Status)
	}
	if !strings.Contains(string(res.Body), `"not_upgrade_eligible"`) {
		t.Errorf("response body did not preserve not_upgrade_eligible code; got %s", res.Body)
	}
}

// 422 (validation — invalid target plan / plan-family mismatch) passes
// through so the FE can render the `validation-error` branch.
func TestChangeAdminMyAddonTier_Validation_422_PassThrough(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_target_plan"}}`))
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.ChangeAdminMyAddonTier(context.Background(), testChangeTierPlanID,
		[]byte(`{"target_plan_id":"x","proration_mode":"create_prorations"}`),
		phyllis.AuthCtx{Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001"},
	)
	if res.Status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d; want 422 pass-through", res.Status)
	}
}

// 404 (subscription not found) passes through.
func TestChangeAdminMyAddonTier_NotFound_404_PassThrough(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.ChangeAdminMyAddonTier(context.Background(), testChangeTierPlanID,
		[]byte(`{"target_plan_id":"x","proration_mode":"create_prorations"}`),
		phyllis.AuthCtx{Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001"},
	)
	if res.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 pass-through", res.Status)
	}
}

// Upstream 5xx passes through as 502 via classify().
func TestChangeAdminMyAddonTier_Upstream500_NormalisesTo502(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.ChangeAdminMyAddonTier(context.Background(), testChangeTierPlanID,
		[]byte(`{"target_plan_id":"x","proration_mode":"create_prorations"}`),
		phyllis.AuthCtx{Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001"},
	)
	if res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 (upstream 5xx normalised by classify)", res.Status)
	}
}
