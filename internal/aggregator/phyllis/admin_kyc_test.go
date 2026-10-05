// admin_kyc_test.go — TDD RED for the staff manual-doc KYC review BFF proxies
// (CHO-2103). POST /api/v1/admin/kyc/{gcid}/verify|reject → chora-identity
// AdminKycVerifyHandler, forwarded VERBATIM. The identity adminGate gates on the
// Bucket 4 x-mesh-user-roles mesh header, which call() stamps from AuthCtx.Roles;
// the {gcid} is the SUBJECT of the verification (path), never the caller.
package phyllis_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

const kycSubjectGcid = "019eb6a6-4383-7d82-8779-495dfd91221c"

func TestVerifyAdminKyc_postsToIdentityVerifyPath(t *testing.T) {
	var seenPath, seenMethod, seenRoles, seenTenant, seenBody string
	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath, seenMethod = r.URL.Path, r.Method
		seenRoles = r.Header.Get("x-mesh-user-roles")
		seenTenant = r.Header.Get("X-Tenant-Id")
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		seenBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"verified"}`))
	})

	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: time.Second}, nil)
	res, _ := a.VerifyAdminKyc(context.Background(), l1Auth(), kycSubjectGcid, []byte(`{}`))

	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200 (body=%s)", res.Status, res.Body)
	}
	wantPath := "/api/v1/admin/kyc/" + kycSubjectGcid + "/verify"
	if seenMethod != http.MethodPost || seenPath != wantPath {
		t.Errorf("upstream = %s %s; want POST %s", seenMethod, seenPath, wantPath)
	}
	if seenRoles == "" {
		t.Error("x-mesh-user-roles must be stamped (identity adminGate reads it)")
	}
	if seenTenant != l1Tenant {
		t.Errorf("X-Tenant-Id = %q; want %s", seenTenant, l1Tenant)
	}
	if seenBody != "{}" {
		t.Errorf("body = %q; want forwarded verbatim", seenBody)
	}
}

func TestRejectAdminKyc_postsToIdentityRejectPath(t *testing.T) {
	var seenPath, seenMethod, seenBody string
	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath, seenMethod = r.URL.Path, r.Method
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		seenBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"rejected"}`))
	})

	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: time.Second}, nil)
	body := `{"code":"doc_illegible","retry_allowed":true}`
	res, _ := a.RejectAdminKyc(context.Background(), l1Auth(), kycSubjectGcid, []byte(body))

	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200 (body=%s)", res.Status, res.Body)
	}
	wantPath := "/api/v1/admin/kyc/" + kycSubjectGcid + "/reject"
	if seenMethod != http.MethodPost || seenPath != wantPath {
		t.Errorf("upstream = %s %s; want POST %s", seenMethod, seenPath, wantPath)
	}
	if seenBody != body {
		t.Errorf("body = %q; want forwarded verbatim", seenBody)
	}
}

func TestVerifyAdminKyc_requiresTenant(t *testing.T) {
	called := false
	identity := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: time.Second}, nil)

	auth := l1Auth()
	auth.TenantID = "" // no tenant context
	res, _ := a.VerifyAdminKyc(context.Background(), auth, kycSubjectGcid, []byte(`{}`))

	if res.Status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (tenant guard)", res.Status)
	}
	if called {
		t.Error("upstream must NOT be called when tenant context is missing")
	}
}
