// addons_test.go — RED-phase tests for the Setup Wizard Phase B BFF
// aggregator (CHO-1664). Verifies the aggregator forwards
// POST /api/v1/tenants/me/addons to the canonical chora-tenancy mount
// path `/api/v1/tenants/me/addons` with body + AuthCtx stamping intact.
//
// Path assertion hard-codes the canonical backend mount path (per
// feedback_bff_aggregator_path_test): echoing seenPath from the
// aggregator's URL config would mask path-stripping bugs that only
// surface in prod (CHO-1632 deploy regression).
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

func TestSubscribeMeAddOns_HappyPath(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"subscriptions":[{"subscription_id":"0197...","add_on_code":"kg_hexagonal","status":"pending_activation","created_at":"2026-06-05T00:00:00Z","newly_subscribed":true}]}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.SubscribeMeAddOns(context.Background(),
		phyllis.AuthCtx{
			Bearer:      "phyllis",
			GCID:        "01935f12-0000-7000-8000-0000000000ff",
			TenantID:    "01970000-0000-7000-8000-000000000001",
			Traceparent: "00-trace-id-01",
		},
		[]byte(`{"add_on_codes":["kg_hexagonal","marketplace"]}`),
	)

	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", res.Status)
	}
	if seenMethod != http.MethodPost {
		t.Errorf("upstream method = %q; want POST", seenMethod)
	}
	// CRITICAL: assert the EXACT canonical mount path on chora-tenancy
	// (per feedback_bff_aggregator_path_test). NOT echoing seenPath
	// from the aggregator's URL config.
	if seenPath != "/api/v1/tenants/me/addons" {
		t.Errorf("upstream path = %q; want /api/v1/tenants/me/addons (canonical chora-tenancy mount)", seenPath)
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
	if !strings.Contains(seenBody, `"add_on_codes"`) {
		t.Errorf("upstream body did not forward add_on_codes; got %s", seenBody)
	}
	if !strings.Contains(seenBody, `kg_hexagonal`) {
		t.Errorf("upstream body missing kg_hexagonal; got %s", seenBody)
	}
}

func TestSubscribeMeAddOns_UpstreamBadRequest_PassesThrough(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"unknown_addon_code","message":"unknown add-on code(s): bogus"}}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.SubscribeMeAddOns(context.Background(),
		phyllis.AuthCtx{Bearer: "phyllis", GCID: "g", TenantID: "t"},
		[]byte(`{"add_on_codes":["bogus"]}`),
	)

	if res.Status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (passed through)", res.Status)
	}
	if !strings.Contains(string(res.Body), "unknown_addon_code") {
		t.Errorf("body should echo upstream error; got %s", string(res.Body))
	}
}

func TestSubscribeMeAddOns_UpstreamUnauthorized_PassesThrough(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"gateway_unauthenticated"}}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.SubscribeMeAddOns(context.Background(),
		phyllis.AuthCtx{Bearer: "phyllis"},
		[]byte(`{"add_on_codes":["tms"]}`),
	)

	if res.Status != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401 (passed through)", res.Status)
	}
}

func TestSubscribeMeAddOns_Upstream500_NormalisesTo502(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error"}}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.SubscribeMeAddOns(context.Background(),
		phyllis.AuthCtx{Bearer: "phyllis", GCID: "g", TenantID: "t"},
		[]byte(`{"add_on_codes":["tms"]}`),
	)

	if res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 (5xx normalised via classify)", res.Status)
	}
	if !strings.Contains(string(res.Body), "GATEWAY_UPSTREAM_5XX") {
		t.Errorf("body should carry GATEWAY_UPSTREAM_5XX; got %s", string(res.Body))
	}
}
