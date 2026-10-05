// bootstrap_test.go — RED tests for the H+ Setup-Tenant BFF route
// (CHO-1632 Phase 3). Verifies the aggregator forwards
// POST /api/v1/tenants/bootstrap to chora-tenancy `/v1/tenants/bootstrap`
// with the body + auth context unchanged.
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

func TestBootstrapTenant_HappyPath(t *testing.T) {
	var seenPath, seenMethod, seenBody, seenAuth, seenGCID, seenTrace string
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenMethod = r.Method
		seenAuth = r.Header.Get("Authorization")
		seenGCID = r.Header.Get("gcid")
		seenTrace = r.Header.Get("traceparent")
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"tenant_id":"01935f12-0000-7000-8000-000000000001","owner_member_id":"01935f12-0000-7000-8000-000000000002","entitlement_id":"01935f12-0000-7000-8000-000000000003","created_at":"2026-06-01T12:00:00Z"}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.BootstrapTenant(context.Background(),
		phyllis.AuthCtx{
			Bearer:      "phyllis",
			GCID:        "01935f12-0000-7000-8000-0000000000ff",
			Traceparent: "00-trace-id-01",
		},
		[]byte(`{"name":"Htet Aung Dev Tenant"}`),
	)

	if res.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", res.Status)
	}
	if seenMethod != http.MethodPost {
		t.Errorf("upstream method = %q; want POST", seenMethod)
	}
	if seenPath != "/api/v1/tenants/bootstrap" {
		t.Errorf("upstream path = %q; want /v1/tenants/bootstrap", seenPath)
	}
	if seenAuth != "Bearer phyllis" {
		t.Errorf("upstream Authorization = %q", seenAuth)
	}
	if seenGCID != "01935f12-0000-7000-8000-0000000000ff" {
		t.Errorf("upstream gcid = %q", seenGCID)
	}
	if seenTrace != "00-trace-id-01" {
		t.Errorf("upstream traceparent = %q", seenTrace)
	}
	if !strings.Contains(seenBody, `"name":"Htet Aung Dev Tenant"`) {
		t.Errorf("upstream body did not forward request: %s", seenBody)
	}
	if !strings.Contains(string(res.Body), `"tenant_id"`) {
		t.Errorf("response body did not pass-through upstream payload: %s", string(res.Body))
	}
}

func TestBootstrapTenant_409Conflict_passThrough(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"already_member","message":"caller already holds membership"}}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.BootstrapTenant(context.Background(),
		phyllis.AuthCtx{Bearer: "x", GCID: "01935f12-0000-7000-8000-0000000000ff"},
		[]byte(`{"name":"Dup"}`),
	)

	if res.Status != http.StatusConflict {
		t.Errorf("status = %d; want 409 (pass-through)", res.Status)
	}
}

func TestBootstrapTenant_400BadRequest_passThrough(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_argument"}}`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.BootstrapTenant(context.Background(),
		phyllis.AuthCtx{Bearer: "x", GCID: "01935f12-0000-7000-8000-0000000000ff"},
		[]byte(`{"name":""}`),
	)
	if res.Status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", res.Status)
	}
}

func TestBootstrapTenant_5xxNormalisedTo502(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`oops`))
	})

	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.BootstrapTenant(context.Background(),
		phyllis.AuthCtx{Bearer: "x", GCID: "01935f12-0000-7000-8000-0000000000ff"},
		[]byte(`{"name":"x"}`),
	)
	if res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 (upstream 5xx normalised)", res.Status)
	}
}

func TestBootstrapTenant_emptyTenancyURL_502(t *testing.T) {
	a := phyllis.New(phyllis.Config{TenancyURL: "", PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.BootstrapTenant(context.Background(),
		phyllis.AuthCtx{Bearer: "x", GCID: "01935f12-0000-7000-8000-0000000000ff"},
		[]byte(`{"name":"x"}`),
	)
	if res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 when TenancyURL is unset", res.Status)
	}
}
