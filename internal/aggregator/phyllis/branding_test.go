// branding_test.go — RED-phase tests for the Setup Wizard Phase A BFF
// aggregator (CHO-1655). Verifies the aggregator forwards
// PATCH /api/v1/tenants/me/branding to the canonical chora-tenancy
// mount path `/api/v1/tenants/me/branding` with the body + AuthCtx
// stamping intact.
//
// Path assertion follows the lesson from CHO-1632 deploy regression
// (feedback_bff_aggregator_path_test memory): MUST hard-code the
// canonical backend mount path, NOT echo back the URL the aggregator
// dialled. Echoing masks path-stripping bugs that only surface in prod.
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

func TestUpdateMeBranding_HappyPath(t *testing.T) {
	var seenPath, seenMethod, seenBody, seenAuth, seenGCID, seenTenantID, seenTrace string
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenMethod = r.Method
		seenAuth = r.Header.Get("Authorization")
		seenGCID = r.Header.Get("gcid")
		seenTenantID = r.Header.Get("X-Tenant-Id")
		seenTrace = r.Header.Get("traceparent")
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"primary_color_hex":"#FF5500","logo_url":"https://cdn.example.com/logo.png","custom_domain":""}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.UpdateMeBranding(context.Background(),
		phyllis.AuthCtx{
			Bearer:      "phyllis",
			GCID:        "01935f12-0000-7000-8000-0000000000ff",
			TenantID:    "01970000-0000-7000-8000-000000000001",
			Traceparent: "00-trace-id-01",
		},
		[]byte(`{"primary_color_hex":"#FF5500","logo_url":"https://cdn.example.com/logo.png"}`),
	)

	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", res.Status)
	}
	if seenMethod != http.MethodPatch {
		t.Errorf("upstream method = %q; want PATCH", seenMethod)
	}
	// CRITICAL: assert the EXACT canonical mount path on chora-tenancy
	// (per feedback_bff_aggregator_path_test). NOT echoing seenPath
	// from the aggregator's URL config.
	if seenPath != "/api/v1/tenants/me/branding" {
		t.Errorf("upstream path = %q; want /api/v1/tenants/me/branding (canonical chora-tenancy mount)", seenPath)
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
	if !strings.Contains(seenBody, `"primary_color_hex":"#FF5500"`) {
		t.Errorf("upstream body did not forward primary_color_hex; got %s", seenBody)
	}
	if !strings.Contains(seenBody, `"logo_url":"https://cdn.example.com/logo.png"`) {
		t.Errorf("upstream body did not forward logo_url; got %s", seenBody)
	}
}

func TestUpdateMeBranding_UpstreamBadRequest_PassesThrough(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_argument","message":"primary_color_hex must be #RRGGBB"}}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.UpdateMeBranding(context.Background(),
		phyllis.AuthCtx{Bearer: "phyllis", GCID: "g", TenantID: "t"},
		[]byte(`{"primary_color_hex":"red"}`),
	)

	if res.Status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (passed through)", res.Status)
	}
	if !strings.Contains(string(res.Body), "invalid_argument") {
		t.Errorf("body should echo upstream error; got %s", string(res.Body))
	}
}

func TestUpdateMeBranding_UpstreamUnauthorized_PassesThrough(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"gateway_unauthenticated"}}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	// Note: AuthCtx without TenantID stamped here — exercises the
	// upstream defence-in-depth path, not the gateway's own gating
	// (that's covered by jwt_auth_test.go).
	res, _ := a.UpdateMeBranding(context.Background(),
		phyllis.AuthCtx{Bearer: "phyllis"},
		[]byte(`{"primary_color_hex":"#FF0000"}`),
	)

	if res.Status != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401 (passed through)", res.Status)
	}
}

func TestUpdateMeBranding_UpstreamNotFound_PassesThrough(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"tenant_not_found"}}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.UpdateMeBranding(context.Background(),
		phyllis.AuthCtx{Bearer: "phyllis", GCID: "g", TenantID: "missing-tid"},
		[]byte(`{"primary_color_hex":"#FF0000"}`),
	)

	if res.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 (passed through)", res.Status)
	}
}

func TestUpdateMeBranding_Upstream500_NormalisesTo502(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error"}}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.UpdateMeBranding(context.Background(),
		phyllis.AuthCtx{Bearer: "phyllis", GCID: "g", TenantID: "t"},
		[]byte(`{"primary_color_hex":"#FF0000"}`),
	)

	if res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 (5xx normalised via classify)", res.Status)
	}
	if !strings.Contains(string(res.Body), "GATEWAY_UPSTREAM_5XX") {
		t.Errorf("body should carry GATEWAY_UPSTREAM_5XX; got %s", string(res.Body))
	}
}
