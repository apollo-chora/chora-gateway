// admin_addons_deactivate_test.go — RED-phase tests for the H+ Add-on
// Lifecycle deactivation modal BFF alias (CHO-1731, STITCH-H-ADD-2).
//
// The FE hits POST /api/v1/admin/tenants/me/addons/{addonPlanId}:deactivate;
// this aggregator method rewrites `me` to AuthCtx.TenantID and forwards
// to the canonical backend POST
//
//	/api/v1/admin/tenants/{tenantId}/addons/{addonPlanId}:deactivate
//
// preserving the request body (reason / reason_text / effective_at) +
// stamping AuthCtx headers (gcid / X-Tenant-Id / traceparent /
// Authorization). Hard-coding the canonical path (per
// feedback_bff_aggregator_path_test) prevents the path-stripping bug
// class CHO-1632 hit in deploy.
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

const testPlanID = "019e0000-0000-7000-8000-bbbbbbbbbbbb"

func TestDeactivateAdminMyAddon_HappyPath_Immediate(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"tenant_id":"01970000-0000-7000-8000-000000000001","addon_plan_id":"019e0000-0000-7000-8000-bbbbbbbbbbbb","status":"DEACTIVATED","requested_at":"2026-06-12T00:00:00Z","reason":"cost"}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	body := []byte(`{"reason":"cost","reason_text":"too expensive"}`)
	res, _ := a.DeactivateAdminMyAddon(context.Background(), testPlanID, body,
		phyllis.AuthCtx{
			Bearer:      "phyllis",
			GCID:        "01935f12-0000-7000-8000-0000000000ff",
			TenantID:    "01970000-0000-7000-8000-000000000001",
			Traceparent: "00-trace-id-01",
		},
	)

	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200 (immediate deactivation)", res.Status)
	}
	if seenMethod != http.MethodPost {
		t.Errorf("upstream method = %q; want POST", seenMethod)
	}
	// Canonical backend path — parametric on tenantId, NOT the literal "me".
	wantPath := "/api/v1/admin/tenants/01970000-0000-7000-8000-000000000001/addons/" + testPlanID + ":deactivate"
	if seenPath != wantPath {
		t.Errorf("upstream path = %q; want %q (parametric backend; me alias rewritten)",
			seenPath, wantPath)
	}
	if string(seenBody) != string(body) {
		t.Errorf("upstream body = %q; want %q (deactivation payload must forward verbatim)",
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
	// Response body passed through verbatim.
	if !strings.Contains(string(res.Body), `"status":"DEACTIVATED"`) {
		t.Errorf("response body did not forward DEACTIVATED status; got %s", res.Body)
	}
}

func TestDeactivateAdminMyAddon_HappyPath_Scheduled202(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"DEACTIVATION_SCHEDULED","effective_at":"2026-07-01T00:00:00Z"}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.DeactivateAdminMyAddon(context.Background(), testPlanID,
		[]byte(`{"reason":"cost","effective_at":"2026-07-01T00:00:00Z"}`),
		phyllis.AuthCtx{
			Bearer:   "phyllis",
			TenantID: "01970000-0000-7000-8000-000000000001",
		},
	)
	if res.Status != http.StatusAccepted {
		t.Errorf("status = %d; want 202 (scheduled deactivation pass-through)", res.Status)
	}
	if !strings.Contains(string(res.Body), `"DEACTIVATION_SCHEDULED"`) {
		t.Errorf("response body did not forward DEACTIVATION_SCHEDULED; got %s", res.Body)
	}
}

// Missing tenant context → 400 BEFORE the upstream call (defense in depth).
func TestDeactivateAdminMyAddon_MissingTenantId_400(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("upstream MUST NOT be called when tenant_id is missing")
		w.WriteHeader(http.StatusInternalServerError)
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.DeactivateAdminMyAddon(context.Background(), testPlanID,
		[]byte(`{"reason":"cost"}`),
		phyllis.AuthCtx{Bearer: "phyllis"},
	)
	if res.Status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 GATEWAY_TENANT_NOT_RESOLVED", res.Status)
	}
}

// 423 Locked compliance lock must pass through unmodified (FE renders the
// lock state from the body). classify() treats 4xx as pass-through.
func TestDeactivateAdminMyAddon_ComplianceLock_423_PassThrough(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusLocked)
		_, _ = w.Write([]byte(`{"error":{"code":"compliance_locked","message":"required by active SkillsFuture cert"}}`))
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.DeactivateAdminMyAddon(context.Background(), testPlanID,
		[]byte(`{"reason":"cost"}`),
		phyllis.AuthCtx{
			Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001",
		},
	)
	if res.Status != http.StatusLocked {
		t.Errorf("status = %d; want 423 (compliance-lock pass-through)", res.Status)
	}
	if !strings.Contains(string(res.Body), `"compliance_locked"`) {
		t.Errorf("response body did not preserve compliance_locked code; got %s", res.Body)
	}
}

// Upstream 5xx passes through as 502 via classify().
func TestDeactivateAdminMyAddon_Upstream500_NormalisesTo502(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.DeactivateAdminMyAddon(context.Background(), testPlanID,
		[]byte(`{"reason":"cost"}`),
		phyllis.AuthCtx{
			Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001",
		},
	)
	if res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 (upstream 5xx normalised by classify)", res.Status)
	}
}

// 404 (plan not subscribed) passes through unchanged so the FE can
// surface the "not-found" branch of the DeactivateResult union.
func TestDeactivateAdminMyAddon_NotFound_404_PassThrough(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"subscription_not_found"}}`))
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.DeactivateAdminMyAddon(context.Background(), testPlanID,
		[]byte(`{"reason":"cost"}`),
		phyllis.AuthCtx{
			Bearer: "phyllis", TenantID: "01970000-0000-7000-8000-000000000001",
		},
	)
	if res.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 pass-through", res.Status)
	}
}
