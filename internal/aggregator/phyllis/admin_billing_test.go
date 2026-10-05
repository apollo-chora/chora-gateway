// admin_billing_test.go — RED-phase tests for the H+ Billing BFF
// aliases:
//
//	GET  /api/v1/admin/tenants/me/invoices       → ListAdminMyInvoices
//	POST /api/v1/admin/tenants/me/billing-portal → CreateAdminMyBillingPortalSession
//
// The FE hits the `me` alias; this aggregator rewrites it to
// AuthCtx.TenantID and forwards to chora-payments at the canonical
//
//	GET  /api/v1/admin/tenants/{tenantId}/invoices?{rawQuery}
//	POST /api/v1/admin/tenants/{tenantId}/billing-portal
//
// preserving query string + request body verbatim + stamping AuthCtx
// headers (gcid / X-Tenant-Id / traceparent / Authorization).
// Canonical-path assertion per [[bff-aggregator-path-test]] prevents the
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

const billingTenantID = "01970000-0000-7000-8000-000000000001"

func TestListAdminMyInvoices_HappyPath(t *testing.T) {
	var seenPath, seenMethod, seenAuth, seenGCID, seenTenantID, seenTrace, seenRawQuery string
	payments := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenRawQuery = r.URL.RawQuery
		seenMethod = r.Method
		seenAuth = r.Header.Get("Authorization")
		seenGCID = r.Header.Get("gcid")
		seenTenantID = r.Header.Get("X-Tenant-Id")
		seenTrace = r.Header.Get("traceparent")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[{"stripe_invoice_id":"in_test_001","number":"INV-001","total_cents":4900,"description":"tms:starter"}],"next_cursor":null}`))
	})
	a := phyllis.New(phyllis.Config{PaymentsURL: payments.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.ListAdminMyInvoices(context.Background(), "limit=25&starting_after=in_test_prev",
		phyllis.AuthCtx{
			Bearer:      "phyllis",
			GCID:        "01935f12-0000-7000-8000-0000000000ff",
			TenantID:    billingTenantID,
			Traceparent: "00-trace-id-01",
		},
	)

	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200, body=%s", res.Status, res.Body)
	}
	if seenMethod != http.MethodGet {
		t.Errorf("upstream method = %q; want GET", seenMethod)
	}
	wantPath := "/api/v1/admin/tenants/" + billingTenantID + "/invoices"
	if seenPath != wantPath {
		t.Errorf("upstream path = %q; want %q (parametric backend; me alias rewritten)", seenPath, wantPath)
	}
	if seenRawQuery != "limit=25&starting_after=in_test_prev" {
		t.Errorf("upstream rawQuery = %q; want it forwarded verbatim", seenRawQuery)
	}
	if seenAuth != "Bearer phyllis" {
		t.Errorf("upstream Authorization = %q", seenAuth)
	}
	if seenGCID != "01935f12-0000-7000-8000-0000000000ff" {
		t.Errorf("upstream gcid = %q", seenGCID)
	}
	if seenTenantID != billingTenantID {
		t.Errorf("upstream X-Tenant-Id = %q", seenTenantID)
	}
	if seenTrace != "00-trace-id-01" {
		t.Errorf("upstream traceparent = %q", seenTrace)
	}
	if !strings.Contains(string(res.Body), `"stripe_invoice_id":"in_test_001"`) {
		t.Errorf("response body lost the upstream payload; got %s", res.Body)
	}
}

func TestListAdminMyInvoices_MissingTenantId_400(t *testing.T) {
	payments := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("upstream MUST NOT be called when tenant_id missing")
		w.WriteHeader(http.StatusInternalServerError)
	})
	a := phyllis.New(phyllis.Config{PaymentsURL: payments.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.ListAdminMyInvoices(context.Background(), "", phyllis.AuthCtx{Bearer: "phyllis"})
	if res.Status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 GATEWAY_TENANT_NOT_RESOLVED", res.Status)
	}
}

func TestListAdminMyInvoices_Upstream500_NormalisesTo502(t *testing.T) {
	payments := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	})
	a := phyllis.New(phyllis.Config{PaymentsURL: payments.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.ListAdminMyInvoices(context.Background(), "",
		phyllis.AuthCtx{Bearer: "phyllis", TenantID: billingTenantID},
	)
	if res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", res.Status)
	}
}

func TestCreateAdminMyBillingPortalSession_HappyPath(t *testing.T) {
	var seenPath, seenMethod string
	var seenBody []byte
	payments := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenMethod = r.Method
		seenBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"url":"https://billing.stripe.com/p/session/test_xyz"}`))
	})
	a := phyllis.New(phyllis.Config{PaymentsURL: payments.URL, PerCallTimeout: 1 * time.Second}, nil)
	body := []byte(`{"return_url":"http://localhost:4200/h/billing"}`)
	res, _ := a.CreateAdminMyBillingPortalSession(context.Background(), body,
		phyllis.AuthCtx{Bearer: "phyllis", TenantID: billingTenantID},
	)
	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200, body=%s", res.Status, res.Body)
	}
	if seenMethod != http.MethodPost {
		t.Errorf("upstream method = %q; want POST", seenMethod)
	}
	wantPath := "/api/v1/admin/tenants/" + billingTenantID + "/billing-portal"
	if seenPath != wantPath {
		t.Errorf("upstream path = %q; want %q", seenPath, wantPath)
	}
	if string(seenBody) != string(body) {
		t.Errorf("upstream body = %q; want %q (forwarded verbatim)", seenBody, body)
	}
	if !strings.Contains(string(res.Body), `billing.stripe.com`) {
		t.Errorf("response body lost the portal URL; got %s", res.Body)
	}
}

func TestCreateAdminMyBillingPortalSession_MissingTenantId_400(t *testing.T) {
	payments := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("upstream MUST NOT be called when tenant_id missing")
		w.WriteHeader(http.StatusInternalServerError)
	})
	a := phyllis.New(phyllis.Config{PaymentsURL: payments.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.CreateAdminMyBillingPortalSession(context.Background(), []byte(`{}`),
		phyllis.AuthCtx{Bearer: "phyllis"},
	)
	if res.Status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", res.Status)
	}
}

func TestCreateAdminMyBillingPortalSession_422_PassThrough(t *testing.T) {
	payments := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"no_stripe_customer"}`))
	})
	a := phyllis.New(phyllis.Config{PaymentsURL: payments.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.CreateAdminMyBillingPortalSession(context.Background(), []byte(`{"return_url":"x"}`),
		phyllis.AuthCtx{Bearer: "phyllis", TenantID: billingTenantID},
	)
	if res.Status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d; want 422 pass-through", res.Status)
	}
}
