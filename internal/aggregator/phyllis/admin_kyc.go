// admin_kyc.go — staff manual-doc KYC review BFF proxies (CHO-2103, W4 Exam BC).
//
// POST /api/v1/admin/kyc/{gcid}/verify|reject → chora-identity's
// AdminKycVerifyHandler, forwarded VERBATIM (same path, body untouched). The
// identity handler completes the manual_doc lifecycle (submitted→verified|
// rejected), unblocking the Exam BC admit gate for manual-doc candidates.
//
// Authz is enforced DOWNSTREAM: identity's adminGate reads the Bucket 4
// x-mesh-user-roles mesh header (TRAINING_ADMIN/TENANT_ADMIN), which call()
// stamps from AuthCtx.Roles — the gateway does not enforce the role itself.
// requireTenant guards here so a tenant-less caller 400s before the upstream
// call (the identity adminGate needs tenant context anyway). The {gcid} is the
// SUBJECT of the verification (path param), never the caller.
package phyllis

import (
	"context"
	"net/http"
	"net/url"
)

// VerifyAdminKyc — POST /api/v1/admin/kyc/{gcid}/verify → chora-identity
// adminKycVerify (submitted→verified). Body forwarded verbatim (optional).
// Mutating → WriteCallTimeout.
func (a *Aggregator) VerifyAdminKyc(ctx context.Context, auth AuthCtx, gcid string, body []byte) (Response, error) {
	if resp, ok := requireTenant(auth); !ok {
		return resp, nil
	}
	return a.withWriteBudget(ctx, func(c context.Context) Response {
		u := a.cfg.IdentityURL + "/api/v1/admin/kyc/" + url.PathEscape(gcid) + "/verify"
		return classify(a.callWrite(c, http.MethodPost, u, body, auth))
	}), nil
}

// RejectAdminKyc — POST /api/v1/admin/kyc/{gcid}/reject → chora-identity
// adminKycReject (submitted→rejected). Body {code, notes, retry_allowed}
// forwarded verbatim. Mutating → WriteCallTimeout.
func (a *Aggregator) RejectAdminKyc(ctx context.Context, auth AuthCtx, gcid string, body []byte) (Response, error) {
	if resp, ok := requireTenant(auth); !ok {
		return resp, nil
	}
	return a.withWriteBudget(ctx, func(c context.Context) Response {
		u := a.cfg.IdentityURL + "/api/v1/admin/kyc/" + url.PathEscape(gcid) + "/reject"
		return classify(a.callWrite(c, http.MethodPost, u, body, auth))
	}), nil
}
