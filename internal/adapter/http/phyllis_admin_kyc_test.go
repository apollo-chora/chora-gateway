// phyllis_admin_kyc_test.go — mux dispatch tests for the staff manual-doc KYC
// review subtree (CHO-2103): POST /api/v1/admin/kyc/{gcid}/verify|reject →
// chora-identity AdminKycVerifyHandler. The production JWT gate
// (RequireChoraSessionJWT, main.go) is outside this router; these tests assert
// dispatch + verbatim forwarding + fail-closed path/method handling.
package httpadapter_test

import (
	"net/http"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

const kycSubject = "019eb6a6-4383-7d82-8779-495dfd91221c"

func TestAdminKyc_VerifyDispatchesToIdentity(t *testing.T) {
	identity := newDownstream(t, http.StatusOK, `{"status":"verified"}`)
	srv := newPhyllisServer(t, phyllis.Config{IdentityURL: identity.URL}, nil)

	resp := l1Do(t, srv.URL, http.MethodPost, "/api/v1/admin/kyc/"+kycSubject+"/verify", `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	wantPath := "/api/v1/admin/kyc/" + kycSubject + "/verify"
	if identity.lastMethod != http.MethodPost || identity.lastPath != wantPath {
		t.Errorf("downstream = %s %s; want POST %s", identity.lastMethod, identity.lastPath, wantPath)
	}
}

func TestAdminKyc_RejectDispatchesToIdentity(t *testing.T) {
	identity := newDownstream(t, http.StatusOK, `{"status":"rejected"}`)
	srv := newPhyllisServer(t, phyllis.Config{IdentityURL: identity.URL}, nil)

	body := `{"code":"doc_illegible","retry_allowed":true}`
	resp := l1Do(t, srv.URL, http.MethodPost, "/api/v1/admin/kyc/"+kycSubject+"/reject", body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	wantPath := "/api/v1/admin/kyc/" + kycSubject + "/reject"
	if identity.lastMethod != http.MethodPost || identity.lastPath != wantPath {
		t.Errorf("downstream = %s %s; want POST %s", identity.lastMethod, identity.lastPath, wantPath)
	}
	if identity.lastBody != body {
		t.Errorf("body = %q; want verbatim", identity.lastBody)
	}
}

func TestAdminKyc_BadPathAndMethod_NoUpstream(t *testing.T) {
	identity := newDownstream(t, http.StatusOK, `{}`)
	srv := newPhyllisServer(t, phyllis.Config{IdentityURL: identity.URL}, nil)

	// unknown action → 400
	if resp := l1Do(t, srv.URL, http.MethodPost,
		"/api/v1/admin/kyc/"+kycSubject+"/frobnicate", `{}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad-action status = %d; want 400", resp.StatusCode)
	}
	// missing action segment → 400
	if resp := l1Do(t, srv.URL, http.MethodPost,
		"/api/v1/admin/kyc/"+kycSubject, `{}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing-action status = %d; want 400", resp.StatusCode)
	}
	// GET on verify → 405
	if resp := l1Do(t, srv.URL, http.MethodGet,
		"/api/v1/admin/kyc/"+kycSubject+"/verify", ""); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET verify status = %d; want 405", resp.StatusCode)
	}
	if identity.calls.Load() != 0 {
		t.Errorf("downstream must not be called on dispatch errors (calls=%d)", identity.calls.Load())
	}
}
