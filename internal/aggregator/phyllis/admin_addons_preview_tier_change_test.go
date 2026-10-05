// admin_addons_preview_tier_change_test.go — RED-phase tests for the
// H+ Add-on Lifecycle preview-tier-change BFF alias (CHO-1768, gap
// surfaced post-CHO-1759 epic merge).
//
// The FE (CHO-1766) hits POST /api/v1/admin/tenants/me/addons/{addonPlanId}/
// preview-tier-change with a PreviewAddonTierRequest body. This
// aggregator must rewrite `me` to AuthCtx.TenantID and forward to the
// canonical chora-tenancy mount
//
//	POST /api/v1/admin/tenants/{tenantId}/addons/{addonPlanId}/preview-tier-change
//
// preserving the request body verbatim + stamping AuthCtx headers
// (gcid / X-Tenant-Id / traceparent / Authorization). Hard-coding the
// canonical path per feedback_bff_aggregator_path_test prevents the
// CHO-1632 path-stripping bug class.
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

const testPreviewTierPlanID = "019e0000-0000-7000-8000-bbbbbbbbbbbb"

func TestPreviewAdminMyAddonTier_HappyPath(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"from_tier":"starter","to_tier":"pro","current_monthly_cents":4900,"target_monthly_cents":9900,"billing_delta_cents":2500,"next_invoice_total_cents":12400,"proration_mode":"create_prorations","effective_at":"2026-06-16T00:00:00Z","deferred_to_cycle_end":false,"currency":"usd"}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	body := []byte(`{"target_tier_code":"pro","proration_mode":"create_prorations"}`)
	res, _ := a.PreviewAdminMyAddonTier(context.Background(), testPreviewTierPlanID, body,
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
	wantPath := "/api/v1/admin/tenants/01970000-0000-7000-8000-000000000001/addons/" + testPreviewTierPlanID + "/preview-tier-change"
	if seenPath != wantPath {
		t.Errorf("upstream path = %q; want %q (canonical chora-tenancy mount; me alias rewritten)",
			seenPath, wantPath)
	}
	if string(seenBody) != string(body) {
		t.Errorf("upstream body = %q; want %q (preview payload must forward verbatim)",
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
	if !strings.Contains(string(res.Body), `"billing_delta_cents":2500`) {
		t.Errorf("response body did not forward billing_delta_cents; got %s", res.Body)
	}
}

func TestPreviewAdminMyAddonTier_MissingTenantId_400(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("upstream MUST NOT be called when tenant_id is missing")
		w.WriteHeader(http.StatusInternalServerError)
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.PreviewAdminMyAddonTier(context.Background(), testPreviewTierPlanID,
		[]byte(`{"target_tier_code":"pro","proration_mode":"create_prorations"}`),
		phyllis.AuthCtx{Bearer: "phyllis"},
	)
	if res.Status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 GATEWAY_TENANT_NOT_RESOLVED", res.Status)
	}
}

// 422 (no_stripe_subscription / stripe_price_missing / validation) passes
// through unchanged so the FE can render the right banner.
func TestPreviewAdminMyAddonTier_Validation_422_PassThrough(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"code":"no_stripe_subscription","message":"legacy purchase predates subscription billing"}}`))
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.PreviewAdminMyAddonTier(context.Background(), testPreviewTierPlanID,
		[]byte(`{"target_tier_code":"pro","proration_mode":"create_prorations"}`),
		phyllis.AuthCtx{Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001"},
	)
	if res.Status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d; want 422 pass-through", res.Status)
	}
	if !strings.Contains(string(res.Body), `"no_stripe_subscription"`) {
		t.Errorf("response body did not preserve error code; got %s", res.Body)
	}
}

func TestPreviewAdminMyAddonTier_NotFound_404_PassThrough(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.PreviewAdminMyAddonTier(context.Background(), testPreviewTierPlanID,
		[]byte(`{"target_tier_code":"pro","proration_mode":"create_prorations"}`),
		phyllis.AuthCtx{Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001"},
	)
	if res.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 pass-through", res.Status)
	}
}

func TestPreviewAdminMyAddonTier_Upstream500_NormalisesTo502(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.PreviewAdminMyAddonTier(context.Background(), testPreviewTierPlanID,
		[]byte(`{"target_tier_code":"pro","proration_mode":"create_prorations"}`),
		phyllis.AuthCtx{Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001"},
	)
	if res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 (upstream 5xx normalised by classify)", res.Status)
	}
}
