// admin_external_egress_test.go — specs for the H+ tenant external web-egress
// BFF route (CHO-2148).
//
// The `me` segment must resolve from the VALIDATED AuthCtx, never from the path
// — this route turns on external web egress for a whole tenant, so a caller
// naming someone else's tenant would be a cross-tenant entitlement escalation.
// Hard-coding the canonical backend path here (per feedback_bff_aggregator_path_test)
// prevents the path-stripping bug class CHO-1632 hit in deploy.
package phyllis_test

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

const (
	eeTenantID = "11111111-1111-7111-8111-111111111111"
	eeGCID     = "00000000-0000-7000-8000-000000001999"
)

func eeAuth() phyllis.AuthCtx {
	return phyllis.AuthCtx{
		Bearer:      "phyllis",
		GCID:        eeGCID,
		TenantID:    eeTenantID,
		Traceparent: "00-trace-id-01",
		Roles:       []string{"TENANT_ADMIN"},
	}
}

func TestGetAdminMyExternalEgress_RewritesMeToTheCallersTenant(t *testing.T) {
	var seenPath, seenMethod, seenTenant, seenRoles string
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenMethod = r.Method
		seenTenant = r.Header.Get("X-Tenant-Id")
		seenRoles = r.Header.Get("x-mesh-user-roles")
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"tenant_id":"` + eeTenantID + `","egress_enabled":false,"opted_in":false,"daily_call_ceiling":50}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: time.Second}, nil)
	res, _ := a.GetAdminMyExternalEgress(context.Background(), eeAuth())

	if res.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Status)
	}
	want := "/api/v1/admin/tenants/" + eeTenantID + "/external-egress"
	if seenPath != want {
		t.Errorf("upstream path = %q, want %q — `me` must resolve from the validated "+
			"AuthCtx, never from the caller's path", seenPath, want)
	}
	if seenMethod != http.MethodGet {
		t.Errorf("method = %s, want GET", seenMethod)
	}
	if seenTenant != eeTenantID {
		t.Errorf("X-Tenant-Id = %q, want %q", seenTenant, eeTenantID)
	}
	// The upstream gate (callerHoldsHPlusAdminRole) reads THIS header fail-closed.
	// If phyllis stops stamping it, every call 403s — the CHO-1708 bug class.
	if seenRoles == "" {
		t.Error("x-mesh-user-roles was NOT stamped — chora-tenancy's admin gate reads " +
			"it fail-closed, so an unstamped call would 403 on every request")
	}
}

func TestSetAdminMyExternalEgress_PatchesTheCallersTenant(t *testing.T) {
	var seenPath, seenMethod string
	var seenBody []byte
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenMethod = r.Method
		seenBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"tenant_id":"` + eeTenantID + `","egress_enabled":true,"opted_in":true,"daily_call_ceiling":25}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: time.Second}, nil)
	body := []byte(`{"egress_enabled":true,"daily_call_ceiling":25}`)
	res, _ := a.SetAdminMyExternalEgress(context.Background(), eeAuth(), body)

	if res.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", res.Status, string(res.Body))
	}
	want := "/api/v1/admin/tenants/" + eeTenantID + "/external-egress"
	if seenPath != want {
		t.Errorf("upstream path = %q, want %q", seenPath, want)
	}
	if seenMethod != http.MethodPatch {
		t.Errorf("method = %s, want PATCH — BffClientService has no put() and "+
			"chora-tenancy has no PUT handlers", seenMethod)
	}
	if string(seenBody) != string(body) {
		t.Errorf("upstream body = %s, want %s (forwarded verbatim)", seenBody, body)
	}
}

// A missing tenant must short-circuit BEFORE any upstream call — never let an
// unresolved tenant reach an entitlement write.
func TestExternalEgress_MissingTenant_400_BeforeUpstream(t *testing.T) {
	called := false
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: time.Second}, nil)

	for _, tc := range []struct {
		name string
		call func() (phyllis.Response, error)
	}{
		{"GET", func() (phyllis.Response, error) {
			return a.GetAdminMyExternalEgress(context.Background(), phyllis.AuthCtx{GCID: eeGCID})
		}},
		{"PATCH", func() (phyllis.Response, error) {
			return a.SetAdminMyExternalEgress(context.Background(), phyllis.AuthCtx{GCID: eeGCID}, []byte(`{"egress_enabled":true}`))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, _ := tc.call()
			if res.Status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 on an unresolved tenant", res.Status)
			}
			if called {
				t.Fatal("an unresolved tenant must NOT reach the upstream entitlement write")
			}
		})
	}
}
