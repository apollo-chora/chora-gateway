// closure_ownership_block_test.go, specs for the closure ownership pre-flight
// (E3, first-launch spec 13.7.2).
//
// The guarantee: no organisation is ever left without a live owner, and the
// closure REQUEST is where that is checked. A GCID holding a live owner row in
// any tenant cannot start a closure saga, on either close route. The escape
// hatch is the ownership handover (S7a) or the operator override (S7c), never
// the admin close route: the organisation is what is protected, not the person
// who asked.
package httpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
)

// fakeOwnershipLookup records the subject it was asked about and plays back a
// canned membership set.
type fakeOwnershipLookup struct {
	gotGCID   string
	gotCaller clients.Caller
	called    bool
	out       []clients.TenantMembershipDTO
	err       error
	// gotIncludeSuspended records what the pre-flight asked for. E3 slice 8:
	// the pre-flight MUST ask for suspended rows, because a suspended owner
	// still holds the tenant. Recorded rather than assumed, so the day someone
	// "tidies" the argument to false the test says why it mattered.
	gotIncludeSuspended bool
}

func (f *fakeOwnershipLookup) ListMemberships(_ context.Context, caller clients.Caller, gcid string, includeSuspended bool) ([]clients.TenantMembershipDTO, error) {
	f.called = true
	f.gotCaller = caller
	f.gotGCID = gcid
	f.gotIncludeSuspended = includeSuspended
	return f.out, f.err
}

// ownsNothing is the default injected by newClosureTestHandler: a real lookup
// answering "this GCID holds no memberships", never a bypass.
func ownsNothing() *fakeOwnershipLookup { return &fakeOwnershipLookup{} }

func ownsNorthwind() *fakeOwnershipLookup {
	return &fakeOwnershipLookup{out: []clients.TenantMembershipDTO{
		{TenantID: testTenantID, TenantSlug: "northwind-academy", Roles: []string{"owner", "admin"}},
	}}
}

func newClosureHandlerWithOwnership(t *testing.T, backend ClosureBackend, own OwnershipLookup) http.Handler {
	t.Helper()
	h, err := NewClosureHandler(ClosureHandlerConfig{Backend: backend, Ownership: own})
	if err != nil {
		t.Fatalf("NewClosureHandler: %v", err)
	}
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	return WithClosureRoutes(base, h)
}

// ownedTenantsFromBody reads the refusal's owned_tenants list so the screen can
// name the organisation and link to its handover.
func ownedTenantsFromBody(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var env struct {
		Error struct {
			OwnedTenants []map[string]any `json:"owned_tenants"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode refusal %q: %v", rec.Body.String(), err)
	}
	return env.Error.OwnedTenants
}

// --- the block itself -------------------------------------------------------

func TestMeClose_409WhenCallerOwnsATenant_NoSagaStarted(t *testing.T) {
	backend := &fakeClosureBackend{}
	h := newClosureHandlerWithOwnership(t, backend, ownsNorthwind())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, PathMeAccountClose, `{"reason":"leaving"}`, sessionClaims()))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != "CLOSURE_OWNS_TENANT" {
		t.Errorf("code = %q, want CLOSURE_OWNS_TENANT", got)
	}
	// The load-bearing assertion: the saga never started.
	if backend.requestCalled {
		t.Error("RequestClosure was called despite the ownership block")
	}
	owned := ownedTenantsFromBody(t, rec)
	if len(owned) != 1 {
		t.Fatalf("owned_tenants = %v, want one entry", owned)
	}
	if owned[0]["tenant_id"] != testTenantID || owned[0]["tenant_slug"] != "northwind-academy" {
		t.Errorf("owned_tenants[0] = %v", owned[0])
	}
}

func TestMeClose_202WhenCallerOwnsNothing(t *testing.T) {
	backend := &fakeClosureBackend{
		requestOut: &clients.ClosureCloseResponse{SagaID: testSagaID, State: "CLOSING"},
	}
	own := &fakeOwnershipLookup{out: []clients.TenantMembershipDTO{
		{TenantID: testTenantID, TenantSlug: "northwind-academy", Roles: []string{"admin", "learner"}},
	}}
	h := newClosureHandlerWithOwnership(t, backend, own)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, PathMeAccountClose, `{"reason":"leaving"}`, sessionClaims()))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	if !backend.requestCalled {
		t.Error("RequestClosure was not called")
	}
	if own.gotGCID != testSessionGCID {
		t.Errorf("ownership subject = %q, want the session gcid %q", own.gotGCID, testSessionGCID)
	}
}

// The tenancy enum stores 'owner' lowercase, but role-capabilities.ts maps both
// 'owner' and 'OWNER', so the comparison is liberal-in like the operator gate.
func TestMeClose_409OwnerRoleMatchIsCaseInsensitive(t *testing.T) {
	backend := &fakeClosureBackend{}
	own := &fakeOwnershipLookup{out: []clients.TenantMembershipDTO{
		{TenantID: testTenantID, TenantSlug: "northwind-academy", Roles: []string{" OWNER "}},
	}}
	h := newClosureHandlerWithOwnership(t, backend, own)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, PathMeAccountClose, "", sessionClaims()))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	if backend.requestCalled {
		t.Error("RequestClosure was called despite the ownership block")
	}
}

// A role that merely CONTAINS "owner" is not ownership. Without an exact match
// a future 'co_owner' or 'owner_delegate' role would block every closure.
func TestMeClose_202WhenRoleMerelyContainsOwner(t *testing.T) {
	backend := &fakeClosureBackend{
		requestOut: &clients.ClosureCloseResponse{SagaID: testSagaID, State: "CLOSING"},
	}
	own := &fakeOwnershipLookup{out: []clients.TenantMembershipDTO{
		{TenantID: testTenantID, TenantSlug: "northwind-academy", Roles: []string{"owner_delegate"}},
	}}
	h := newClosureHandlerWithOwnership(t, backend, own)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, PathMeAccountClose, "", sessionClaims()))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
}

// --- the admin route is blocked too -----------------------------------------

func TestAdminClose_409WhenTargetOwnsATenant_NoSagaStarted(t *testing.T) {
	backend := &fakeClosureBackend{}
	h := newClosureHandlerWithOwnership(t, backend, ownsNorthwind())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, adminClosePath(testTargetGCID), `{"fast_close":true}`, operatorClaims()))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != "CLOSURE_OWNS_TENANT" {
		t.Errorf("code = %q, want CLOSURE_OWNS_TENANT", got)
	}
	if backend.requestCalled {
		t.Error("RequestClosure was called despite the ownership block")
	}
}

// The subject of the check is the TARGET, not the operator. An operator who
// owns an organisation must still be able to close somebody else's account.
func TestAdminClose_ChecksTheTargetGCIDNotTheOperator(t *testing.T) {
	backend := &fakeClosureBackend{
		requestOut: &clients.ClosureCloseResponse{SagaID: testSagaID, State: "CLOSING"},
	}
	own := ownsNothing()
	h := newClosureHandlerWithOwnership(t, backend, own)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, adminClosePath(testTargetGCID), "{}", operatorClaims()))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	if own.gotGCID != testTargetGCID {
		t.Errorf("ownership subject = %q, want the TARGET gcid %q (the operator is %q)",
			own.gotGCID, testTargetGCID, testSessionGCID)
	}
	// The mesh identity on the outbound read is the operator's, so the read is
	// attributable to the caller who triggered it.
	if own.gotCaller.GCID != testSessionGCID {
		t.Errorf("ownership caller = %q, want the operator %q", own.gotCaller.GCID, testSessionGCID)
	}
}

// The operator gate still runs first: a non-operator gets 403, and the
// ownership lookup is never dialled on their behalf.
func TestAdminClose_403BeforeOwnershipLookup(t *testing.T) {
	backend := &fakeClosureBackend{}
	own := ownsNorthwind()
	h := newClosureHandlerWithOwnership(t, backend, own)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, adminClosePath(testTargetGCID), "{}", sessionClaims()))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if own.called {
		t.Error("the ownership lookup ran for a caller who is not an operator")
	}
}

// --- the check must never fail open -----------------------------------------

func TestMeClose_503WhenOwnershipLookupFails_NoSagaStarted(t *testing.T) {
	backend := &fakeClosureBackend{}
	own := &fakeOwnershipLookup{err: errors.New("tenancy unreachable")}
	h := newClosureHandlerWithOwnership(t, backend, own)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, PathMeAccountClose, "", sessionClaims()))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != "CLOSURE_OWNERSHIP_CHECK_UNAVAILABLE" {
		t.Errorf("code = %q, want CLOSURE_OWNERSHIP_CHECK_UNAVAILABLE", got)
	}
	if backend.requestCalled {
		t.Error("RequestClosure was called although ownership could not be checked")
	}
}

func TestAdminClose_503WhenOwnershipLookupFails_NoSagaStarted(t *testing.T) {
	backend := &fakeClosureBackend{}
	own := &fakeOwnershipLookup{err: errors.New("tenancy unreachable")}
	h := newClosureHandlerWithOwnership(t, backend, own)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, adminClosePath(testTargetGCID), "{}", operatorClaims()))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
	if backend.requestCalled {
		t.Error("RequestClosure was called although ownership could not be checked")
	}
}

// The dependency is mandatory at construction, the same posture the backend
// already has. Degraded mode is a nil *ClosureHandler answering 503 on all four
// routes, never a handler that silently skips the guarantee.
func TestNewClosureHandler_RefusesWithoutOwnershipLookup(t *testing.T) {
	_, err := NewClosureHandler(ClosureHandlerConfig{Backend: &fakeClosureBackend{}})
	if err == nil {
		t.Fatal("want NewClosureHandler to refuse a nil ownership lookup")
	}
}

// --- cancel and status are NOT blocked --------------------------------------

// An owner who has already started a closure must still be able to cancel it,
// and to read its state. Blocking those would trap the account in CLOSING.
func TestMeCancel_NotBlockedByOwnership(t *testing.T) {
	backend := &fakeClosureBackend{
		statusOut: &clients.ClosureStatusResponse{SagaID: testSagaID, GCID: testSessionGCID, State: "CLOSING"},
		cancelOut: &clients.ClosureCancelResponse{SagaID: testSagaID, State: "ACTIVE"},
	}
	own := ownsNorthwind()
	h := newClosureHandlerWithOwnership(t, backend, own)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, PathMeAccountCloseCancel,
		`{"closure_id":"`+testSagaID+`"}`, sessionClaims()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !backend.cancelCalled {
		t.Error("CancelClosure was not called")
	}
	if own.called {
		t.Error("the ownership lookup ran on the cancel route")
	}
}

func TestMeStatus_NotBlockedByOwnership(t *testing.T) {
	backend := &fakeClosureBackend{
		statusOut: &clients.ClosureStatusResponse{SagaID: testSagaID, GCID: testSessionGCID, State: "CLOSING"},
	}
	own := ownsNorthwind()
	h := newClosureHandlerWithOwnership(t, backend, own)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodGet,
		PathMeAccountClosure+"?closure_id="+testSagaID, "", sessionClaims()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if own.called {
		t.Error("the ownership lookup ran on the status route")
	}
}

// --- identity precedence ----------------------------------------------------

// The subject comes from the validated session, never from the body. A body
// carrying somebody else's gcid must not redirect the ownership check.
func TestMeClose_OwnershipSubjectIsTheSessionNeverTheBody(t *testing.T) {
	backend := &fakeClosureBackend{}
	own := ownsNorthwind()
	h := newClosureHandlerWithOwnership(t, backend, own)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, PathMeAccountClose,
		`{"gcid":"`+testTargetGCID+`","reason":"leaving"}`,
		&servicemesh.MeshClaims{GCID: testSessionGCID, TenantID: testTenantID, Roles: []string{"learner"}}))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	if own.gotGCID != testSessionGCID {
		t.Errorf("ownership subject = %q, want the session gcid %q", own.gotGCID, testSessionGCID)
	}
}

// -----------------------------------------------------------------------------
// include_suspended (E3 slice 8)
// -----------------------------------------------------------------------------

// TestClose_PreflightAsksForSuspendedMemberships pins the one argument that
// makes this pre-flight correct against a suspended owner.
//
// `list_memberships_by_gcid` excludes rows with suspended_at set, while the
// one-live-owner index (tenancy 0036) and the membership write guards ignore
// suspension. So a suspended owner STILL HOLDS the tenant while being invisible
// to the default read. A pre-flight that asked with false would clear that GCID,
// start the saga, and orphan the organisation this whole check exists to
// protect.
//
// Asserted on BOTH close routes, because the guarantee protects the
// organisation, not the person who asked.
func TestClose_PreflightAsksForSuspendedMemberships(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		claims *servicemesh.MeshClaims
		body   string
	}{
		{"self-close", PathMeAccountClose, sessionClaims(), `{"reason":"leaving"}`},
		{"operator-close", adminClosePath(testTargetGCID), operatorClaims(), `{"fast_close":true}`},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			lookup := ownsNothing()
			h := newClosureHandlerWithOwnership(t, &fakeClosureBackend{
				requestOut: &clients.ClosureCloseResponse{SagaID: testSagaID, State: "CLOSING"},
			}, lookup)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, closureReq(t, http.MethodPost, tc.path, tc.body, tc.claims))

			if !lookup.called {
				t.Fatal("the ownership pre-flight never ran")
			}
			if !lookup.gotIncludeSuspended {
				t.Fatal("the pre-flight asked with includeSuspended=false; a suspended owner still holds the tenant, so this would clear them and orphan the organisation")
			}
		})
	}
}
