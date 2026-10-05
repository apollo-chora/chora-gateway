// setup_test.go — RED-phase tests for the Setup Wizard Phase C BFF
// aggregator (CHO-1682). FIRST multi-downstream aggregator in the
// codebase: fans out POST /api/v1/tenants/setup to
//
//  1. chora-identity POST /api/v1/tenants/me/idp-providers (identity slice)
//  2. chora-tenancy   PATCH /api/v1/tenants/me/finish-setup  (no body)
//
// Identity runs FIRST; only on identity success does the aggregator call
// tenancy. A failed identity write MUST NOT leave a tenant marked complete.
//
// Path assertions hard-code the canonical downstream mount paths
// (per feedback_bff_aggregator_path_test): echoing seenPath would mask
// path-stripping bugs that only surface in prod.
package phyllis_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

const (
	canonicalIdentityPath = "/api/v1/tenants/me/idp-providers"
	canonicalTenancyPath  = "/api/v1/tenants/me/finish-setup"
)

// ---------------------------------------------------------------------------
// Happy path — identity 200 → tenancy 200 → 200 envelope
// ---------------------------------------------------------------------------

func TestSetupTenant_HappyPath_FansOutAndCombinesResponses(t *testing.T) {
	var seenIdentityPath, seenIdentityMethod, seenIdentityBody, seenIdentityTenant string
	var seenTenancyPath, seenTenancyMethod, seenTenancyTenant string

	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenIdentityPath = r.URL.Path
		seenIdentityMethod = r.Method
		seenIdentityTenant = r.Header.Get("X-Tenant-Id")
		b, _ := io.ReadAll(r.Body)
		seenIdentityBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"019e0000-0000-7000-8000-iiiiiiiiiiii","tenant_id":"01970000-0000-7000-8000-aaaaaaaaaaaa","provider_type":"oidc","client_id":"acme","client_secret_name":"projects/chora-489812/secrets/sm-name","discovery_url":"https://issuer.example/openid","singpass_enabled":false,"created_at":"2026-06-07T00:00:00Z","updated_at":"2026-06-07T00:00:00Z"}`))
	})
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenTenancyPath = r.URL.Path
		seenTenancyMethod = r.Method
		seenTenancyTenant = r.Header.Get("X-Tenant-Id")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"tenant_id":"01970000-0000-7000-8000-aaaaaaaaaaaa","wizard_completed_at":"2026-06-07T00:00:01Z","newly_completed":true}`))
	})

	a := phyllis.New(phyllis.Config{
		IdentityURL:    identity.URL,
		TenancyURL:     tenancy.URL,
		PerCallTimeout: 2 * time.Second,
	}, nil)

	res, _ := a.SetupTenant(context.Background(),
		phyllis.AuthCtx{
			Bearer:      "phyllis",
			GCID:        "01935f12-0000-7000-8000-0000000000ff",
			TenantID:    "01970000-0000-7000-8000-aaaaaaaaaaaa",
			Traceparent: "00-trace-id-01",
		},
		[]byte(`{"identity":{"provider_type":"oidc","client_id":"acme","client_secret":"shhh","discovery_url":"https://issuer.example/openid"}}`),
	)

	if res.Status != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", res.Status, string(res.Body))
	}

	// Canonical paths — hard-coded, NOT echoed.
	if seenIdentityPath != canonicalIdentityPath {
		t.Errorf("identity path = %q; want %q", seenIdentityPath, canonicalIdentityPath)
	}
	if seenIdentityMethod != http.MethodPost {
		t.Errorf("identity method = %q; want POST", seenIdentityMethod)
	}
	if seenTenancyPath != canonicalTenancyPath {
		t.Errorf("tenancy path = %q; want %q", seenTenancyPath, canonicalTenancyPath)
	}
	if seenTenancyMethod != http.MethodPatch {
		t.Errorf("tenancy method = %q; want PATCH", seenTenancyMethod)
	}

	// AuthCtx propagated — same X-Tenant-Id stamped on BOTH calls.
	if seenIdentityTenant != "01970000-0000-7000-8000-aaaaaaaaaaaa" {
		t.Errorf("identity X-Tenant-Id = %q", seenIdentityTenant)
	}
	if seenTenancyTenant != "01970000-0000-7000-8000-aaaaaaaaaaaa" {
		t.Errorf("tenancy X-Tenant-Id = %q", seenTenancyTenant)
	}

	// Body sent to identity = the `identity` slice unwrapped (NOT the outer
	// wizard envelope). The aggregator's contract is "give chora-identity
	// the shape it expects on /me/idp-providers".
	if !strings.Contains(seenIdentityBody, `"provider_type":"oidc"`) {
		t.Errorf("identity body missing provider_type; got %s", seenIdentityBody)
	}
	if !strings.Contains(seenIdentityBody, `"client_id":"acme"`) {
		t.Errorf("identity body missing client_id; got %s", seenIdentityBody)
	}
	if strings.Contains(seenIdentityBody, `"identity":`) {
		t.Errorf("identity body should be the unwrapped slice, not the wizard envelope; got %s", seenIdentityBody)
	}

	// Response envelope combines BOTH downstream payloads.
	var resp struct {
		IdpProvider map[string]any `json:"idp_provider"`
		FinishSetup map[string]any `json:"finish_setup"`
	}
	if err := json.Unmarshal(res.Body, &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, string(res.Body))
	}
	if resp.IdpProvider["provider_type"] != "oidc" {
		t.Errorf("idp_provider.provider_type = %v", resp.IdpProvider["provider_type"])
	}
	if resp.FinishSetup["newly_completed"] != true {
		t.Errorf("finish_setup.newly_completed = %v; want true", resp.FinishSetup["newly_completed"])
	}
}

// ---------------------------------------------------------------------------
// Identity 400 → 400 + tenancy NOT called
// ---------------------------------------------------------------------------

func TestSetupTenant_Identity400_TenancyNeverCalled(t *testing.T) {
	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_input","message":"oidc requires client_id"}}`))
	})
	var tenancyCalls int32
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tenancyCalls, 1)
		w.WriteHeader(http.StatusOK)
	})

	a := phyllis.New(phyllis.Config{
		IdentityURL: identity.URL, TenancyURL: tenancy.URL, PerCallTimeout: 2 * time.Second,
	}, nil)
	res, _ := a.SetupTenant(context.Background(),
		phyllis.AuthCtx{Bearer: "x", GCID: "g", TenantID: "t"},
		[]byte(`{"identity":{"provider_type":"oidc"}}`),
	)

	if res.Status != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400; body=%s", res.Status, string(res.Body))
	}
	if !strings.Contains(string(res.Body), "invalid_input") {
		t.Errorf("body should echo identity 400; got %s", string(res.Body))
	}
	if got := atomic.LoadInt32(&tenancyCalls); got != 0 {
		t.Errorf("tenancy calls on identity failure = %d; want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Identity 5xx → 502 + tenancy NOT called
// ---------------------------------------------------------------------------

func TestSetupTenant_Identity5xx_NormalisesTo502_TenancyNeverCalled(t *testing.T) {
	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	var tenancyCalls int32
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tenancyCalls, 1)
		w.WriteHeader(http.StatusOK)
	})

	a := phyllis.New(phyllis.Config{
		IdentityURL: identity.URL, TenancyURL: tenancy.URL, PerCallTimeout: 2 * time.Second,
	}, nil)
	res, _ := a.SetupTenant(context.Background(),
		phyllis.AuthCtx{Bearer: "x", GCID: "g", TenantID: "t"},
		[]byte(`{"identity":{"provider_type":"oidc","client_id":"c","client_secret":"s","discovery_url":"https://x.example/openid"}}`),
	)

	if res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", res.Status)
	}
	if got := atomic.LoadInt32(&tenancyCalls); got != 0 {
		t.Errorf("tenancy calls on identity 5xx = %d; want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Tenancy 5xx after identity success — 502 with `upstream: tenancy` hint
// ---------------------------------------------------------------------------

func TestSetupTenant_TenancyFailureAfterIdentitySuccess_502(t *testing.T) {
	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"i","tenant_id":"t","provider_type":"oidc","client_id":"c","client_secret_name":"sn","discovery_url":"https://x.example","singpass_enabled":false,"created_at":"2026-06-07T00:00:00Z","updated_at":"2026-06-07T00:00:00Z"}`))
	})
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	a := phyllis.New(phyllis.Config{
		IdentityURL: identity.URL, TenancyURL: tenancy.URL, PerCallTimeout: 2 * time.Second,
	}, nil)
	res, _ := a.SetupTenant(context.Background(),
		phyllis.AuthCtx{Bearer: "x", GCID: "g", TenantID: "t"},
		[]byte(`{"identity":{"provider_type":"oidc","client_id":"c","client_secret":"s","discovery_url":"https://x.example/openid"}}`),
	)

	if res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", res.Status)
	}
	// Body should hint that the identity row IS created (so the caller
	// knows a retry is safe — both downstreams are idempotent).
	if !strings.Contains(string(res.Body), "tenancy") {
		t.Errorf("502 body should name tenancy as the failing leg; got %s", string(res.Body))
	}
}

// ---------------------------------------------------------------------------
// Missing identity slice — 400 with no downstream calls
// ---------------------------------------------------------------------------

func TestSetupTenant_MissingIdentitySlice_400_NoCalls(t *testing.T) {
	var identityCalls, tenancyCalls int32
	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&identityCalls, 1)
		w.WriteHeader(http.StatusOK)
	})
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tenancyCalls, 1)
		w.WriteHeader(http.StatusOK)
	})

	a := phyllis.New(phyllis.Config{
		IdentityURL: identity.URL, TenancyURL: tenancy.URL, PerCallTimeout: 2 * time.Second,
	}, nil)
	res, _ := a.SetupTenant(context.Background(),
		phyllis.AuthCtx{Bearer: "x", GCID: "g", TenantID: "t"},
		[]byte(`{}`), // no `identity` slice
	)

	if res.Status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 for empty identity slice", res.Status)
	}
	if atomic.LoadInt32(&identityCalls) != 0 || atomic.LoadInt32(&tenancyCalls) != 0 {
		t.Errorf("no downstream calls expected; identity=%d tenancy=%d",
			identityCalls, tenancyCalls)
	}
}
