// readiness_tenant_scope_test.go: RED-first guard for the cross-tenant read on
// GET /api/v1/admin/readiness.
//
// THE DEFECT
// ----------
// The handler gated on hasPlatformOperatorRole || hasTenantAdminRole and then
// took the tenant VERBATIM from the client-supplied X-Tenant-Id header. The
// role gate asks "are you an admin of something"; the header answers "of
// which tenant". Nothing joined them, so any tenant admin of any tenant could
// read any other tenant's readiness posture by changing one request header.
//
// That is a fifth cross-tenant path. The hard invariant puts cross-tenant
// access on exactly four ADR-scoped surfaces, all at the database layer with
// audit-before-SQL, and makes PLATFORM_OPERATOR the sole tenant-less
// cross-tenant role. This one was unscoped, unaudited, at the HTTP layer, and
// reachable by an ordinary tenant owner.
//
// WHY IT SURVIVED REVIEW, WHICH IS THE PART WORTH REMEMBERING
// -----------------------------------------------------------
// Two pieces of careful reasoning sit right next to it. The handler's own doc
// says the audience gate runs first "so a learner cannot use the shape of the
// error to learn whether a tenant header was accepted": someone thought hard
// about information leakage through error shapes and never checked membership
// at all. And this file's own readinessReq helper says seeding roles from a
// header "would test nothing: a caller-supplied header is exactly what the JWT
// middleware exists to replace, and a gate that trusted one would be the bug".
// The principle was stated exactly right for ROLES and then not applied to the
// TENANT in the same handler. A gate can be thoughtfully designed against the
// wrong threat, and the care is what makes it read as considered.
//
// THE FIX SHAPE IS THE ESTATE'S OWN
// ---------------------------------
// phyllis_handler.go carries a post-mortem of this same defect class and the
// ratified answer: a validated session's tenant wins, and a client header is
// honoured only for a caller who has no session at all. requireMeshIdentity
// (payments_handler.go) is the same rule stated positively, and
// decodeClosureBody names it "the identity-from-session rule".
package httpadapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/readiness"
)

const (
	// ownTenant is the tenant the session actually holds.
	ownTenant = "11111111-1111-7111-8111-111111111111"
	// otherTenant is a tenant the session holds no membership in. Reading it
	// is the escalation.
	otherTenant = "99999999-9999-7999-8999-999999999999"
	scopeGCID   = "22222222-2222-7222-8222-222222222222"
)

// readinessReqScoped builds a request whose SESSION tenant and HEADER tenant
// can differ, which readinessReq deliberately cannot: it pins both to the same
// value, so every existing test in this package sits on the safe side of the
// defect and none of them could ever have seen it.
func readinessReqScoped(roles, sessionTenant, headerTenant string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/readiness", nil)
	if headerTenant != "" {
		r.Header.Set("X-Tenant-Id", headerTenant)
	}
	r.Header.Set("gcid", scopeGCID)
	claims := &servicemesh.MeshClaims{GCID: scopeGCID, TenantID: sessionTenant}
	if roles != "" {
		claims.Roles = []string{roles}
	}
	return r.WithContext(withMeshClaims(r.Context(), claims))
}

// -----------------------------------------------------------------------------
// The escalation.
// -----------------------------------------------------------------------------

// A tenant owner asking for a tenant they do not hold must be refused. This is
// the measured case: a session carrying membership in exactly one tenant got
// HTTP 200 and genuinely per-tenant data for three others.
func TestReadiness_TenantAdminCannotReadAnotherTenant(t *testing.T) {
	h := &ReadinessHandler{Agg: stubReadinessAgg{status: http.StatusOK, body: `{"rows":[]}`}}
	for _, role := range []string{"tenant_admin", "admin", "owner", "super_admin", "OWNER"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, readinessReqScoped(role, ownTenant, otherTenant))
		if w.Code != http.StatusForbidden {
			t.Errorf("role %q: status = %d, want 403; a %s of %s must not read %s",
				role, w.Code, role, ownTenant, otherTenant)
		}
	}
}

// The aggregator must never be reached for a tenant the caller does not hold.
// Asserting the status alone would pass if the refusal happened AFTER the
// fan-out, which would still have read the other tenant's data.
func TestReadiness_ForeignTenantNeverReachesTheAggregator(t *testing.T) {
	spy := &spyReadinessAgg{}
	h := &ReadinessHandler{Agg: spy}
	h.ServeHTTP(httptest.NewRecorder(), readinessReqScoped("owner", ownTenant, otherTenant))
	if spy.calls != 0 {
		t.Fatalf("aggregator was called %d time(s) for a foreign tenant; the refusal must precede the read", spy.calls)
	}
}

// -----------------------------------------------------------------------------
// The controls. Each one must be able to fail, or the refusal above is just a
// handler that says no to everything.
// -----------------------------------------------------------------------------

// The legitimate case: a tenant admin reading its OWN tenant. Planted so that
// an over-tight fix (refusing every tenant-admin) turns this red rather than
// looking like a clean pass.
func TestReadiness_TenantAdminReadsItsOwnTenant(t *testing.T) {
	h := &ReadinessHandler{Agg: stubReadinessAgg{status: http.StatusOK, body: `{"rows":[]}`}}
	for _, role := range []string{"tenant_admin", "admin", "owner", "super_admin"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, readinessReqScoped(role, ownTenant, ownTenant))
		if w.Code != http.StatusOK {
			t.Errorf("role %q: status = %d, want 200 for the caller's OWN tenant; body=%s",
				role, w.Code, w.Body.String())
		}
	}
}

// PLATFORM_OPERATOR is the documented exception: ADR-165 makes it the sole
// tenant-less cross-tenant role, so it keeps its reach across tenants. Pinning
// it here means a later tightening cannot quietly remove the one cross-tenant
// principal the architecture sanctions.
func TestReadiness_PlatformOperatorKeepsCrossTenantReach(t *testing.T) {
	h := &ReadinessHandler{Agg: stubReadinessAgg{status: http.StatusOK, body: `{"rows":[]}`}}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, readinessReqScoped("platform_operator", ownTenant, otherTenant))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; platform_operator is the sanctioned cross-tenant role (ADR-165); body=%s",
			w.Code, w.Body.String())
	}
}

// A tenant-less operator session (no tenant claim at all) must still work.
// PLATFORM_OPERATOR holds no tenant_memberships row by design, so a fix that
// required the session tenant to match would lock out the one role that is
// supposed to span tenants.
func TestReadiness_TenantlessOperatorSessionStillReads(t *testing.T) {
	h := &ReadinessHandler{Agg: stubReadinessAgg{status: http.StatusOK, body: `{"rows":[]}`}}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, readinessReqScoped("platform_operator", "", otherTenant))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a tenant-less operator session; body=%s", w.Code, w.Body.String())
	}
}

// A tenant admin whose session carries NO tenant must be refused rather than
// falling through to the header. This is the residual hole the phyllis
// post-mortem describes: a validated session with an empty tenant used to let
// the client header decide. Refusing is the honest answer, and it is what
// keeps the fix from depending on a mint invariant holding forever.
func TestReadiness_TenantlessAdminSessionCannotBorrowTheHeader(t *testing.T) {
	h := &ReadinessHandler{Agg: stubReadinessAgg{status: http.StatusOK, body: `{"rows":[]}`}}
	for _, role := range []string{"tenant_admin", "admin", "owner"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, readinessReqScoped(role, "", otherTenant))
		if w.Code != http.StatusForbidden {
			t.Errorf("role %q: status = %d, want 403; a session with no tenant must not take one from a header",
				role, w.Code)
		}
	}
}

// The refusal must not become an existence oracle. The comparison is against
// the caller's OWN session tenant and never touches the target, so a tenant
// that exists and one that does not must be indistinguishable.
func TestReadiness_RefusalIsNotAnExistenceOracle(t *testing.T) {
	h := &ReadinessHandler{Agg: stubReadinessAgg{status: http.StatusOK, body: `{"rows":[]}`}}
	real := httptest.NewRecorder()
	h.ServeHTTP(real, readinessReqScoped("owner", ownTenant, otherTenant))
	fake := httptest.NewRecorder()
	h.ServeHTTP(fake, readinessReqScoped("owner", ownTenant, "00000000-0000-7000-8000-000000000000"))

	if real.Code != fake.Code {
		t.Fatalf("status differs: existing tenant %d vs made-up tenant %d", real.Code, fake.Code)
	}
	if real.Body.String() != fake.Body.String() {
		t.Fatalf("body differs:\n existing: %s\n made-up:  %s", real.Body.String(), fake.Body.String())
	}
}

// Ordering guard. The audience gate runs first by design, so a learner asking
// for a foreign tenant must get the audience refusal, not a tenant-scope one:
// otherwise the error shape tells an unprivileged caller which tenant headers
// are accepted, which is the leak the handler's own doc was written to avoid.
func TestReadiness_LearnerStillFailsOnAudienceNotOnScope(t *testing.T) {
	h := &ReadinessHandler{Agg: stubReadinessAgg{status: http.StatusOK, body: `{"rows":[]}`}}
	foreign := httptest.NewRecorder()
	h.ServeHTTP(foreign, readinessReqScoped("learner", ownTenant, otherTenant))
	own := httptest.NewRecorder()
	h.ServeHTTP(own, readinessReqScoped("learner", ownTenant, ownTenant))

	if foreign.Code != http.StatusForbidden || own.Code != http.StatusForbidden {
		t.Fatalf("learner must be 403 either way: foreign %d, own %d", foreign.Code, own.Code)
	}
	if foreign.Body.String() != own.Body.String() {
		t.Fatalf("a learner can tell the two apart from the error body:\n foreign: %s\n own:     %s",
			foreign.Body.String(), own.Body.String())
	}
}

// spyReadinessAgg counts calls so a test can assert the aggregator was never
// reached, which a status assertion alone cannot show.
type spyReadinessAgg struct{ calls int }

func (s *spyReadinessAgg) GetReadiness(_ context.Context, _ readiness.AuthCtx) (readiness.Response, error) {
	s.calls++
	return readiness.Response{Status: http.StatusOK, Body: []byte(`{"rows":[]}`)}, nil
}

// -----------------------------------------------------------------------------
// The defence-in-depth fallback.
//
// sessionTenantID prefers mesh claims and falls back to the raw Chora-session
// claims, the same precedence sessionRoles and requireMeshIdentity use. Every
// test above seeds mesh claims, so without these two the fallback branch is
// declared and never invoked, which is not coverage: it is an untested path
// that only runs when the mesh claims are the ones missing, i.e. exactly when
// it matters.
// -----------------------------------------------------------------------------

// readinessReqRawClaims seeds ONLY the raw Chora-session claims, as a
// route-ordering slip would leave them.
func readinessReqRawClaims(roles, sessionTenant, headerTenant string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/readiness", nil)
	if headerTenant != "" {
		r.Header.Set("X-Tenant-Id", headerTenant)
	}
	r.Header.Set("gcid", scopeGCID)
	ctx := InjectChoraSessionClaimsForTest(r.Context(), scopeGCID, sessionTenant, "a@b.test")
	// Roles still arrive as mesh claims with NO tenant, so the audience gate
	// passes and the tenant must come from the raw claims alone.
	ctx = withMeshClaims(ctx, &servicemesh.MeshClaims{GCID: scopeGCID, Roles: []string{roles}})
	return r.WithContext(ctx)
}

func TestReadiness_FallsBackToRawSessionClaimsForItsOwnTenant(t *testing.T) {
	h := &ReadinessHandler{Agg: stubReadinessAgg{status: http.StatusOK, body: `{"rows":[]}`}}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, readinessReqRawClaims("owner", ownTenant, ownTenant))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; the raw-claims fallback must resolve the caller's own tenant; body=%s",
			w.Code, w.Body.String())
	}
}

func TestReadiness_FallsBackToRawSessionClaimsAndStillRefusesAForeignTenant(t *testing.T) {
	h := &ReadinessHandler{Agg: stubReadinessAgg{status: http.StatusOK, body: `{"rows":[]}`}}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, readinessReqRawClaims("owner", ownTenant, otherTenant))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; the fallback must scope, not merely resolve", w.Code)
	}
}
