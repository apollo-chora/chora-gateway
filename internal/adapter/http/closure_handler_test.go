// closure_handler_test.go — behaviour specs for the account-closure saga
// trigger BFF routes (CHO-1719, ADR-181 D5; contract:
// chora-contracts/openapi/auth-gateway.yaml Closure tag).
//
// Internal-package test (like maxbody_test.go) so specs can stamp
// MeshClaims (incl. Roles for the PLATFORM_OPERATOR gate) directly via
// withMeshClaims — the JWT middleware is exercised separately in
// jwt_auth_test.go.
package httpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
)

const (
	testSessionGCID = "00000000-0000-7000-8000-00000000aaaa"
	testTenantID    = "00000000-0000-7000-8000-00000000bbbb"
	testTargetGCID  = "00000000-0000-7000-8000-00000000cccc"
	testSagaID      = "00000000-0000-7000-8000-00000000dddd"
)

// fakeClosureBackend records calls + plays back canned responses.
type fakeClosureBackend struct {
	requestIn     *clients.ClosureRequest
	requestRole   string
	requestOut    *clients.ClosureCloseResponse
	requestErr    error
	cancelID      string
	cancelIn      *clients.ClosureCancelRequest
	cancelOut     *clients.ClosureCancelResponse
	cancelErr     error
	cancelCalled  bool
	statusID      string
	statusOut     *clients.ClosureStatusResponse
	statusErr     error
	statusCalled  bool
	requestCalled bool
}

func (f *fakeClosureBackend) RequestClosure(_ context.Context, req clients.ClosureRequest, roleHeader string) (*clients.ClosureCloseResponse, error) {
	f.requestCalled = true
	f.requestIn = &req
	f.requestRole = roleHeader
	return f.requestOut, f.requestErr
}

func (f *fakeClosureBackend) CancelClosure(_ context.Context, closureID string, req clients.ClosureCancelRequest) (*clients.ClosureCancelResponse, error) {
	f.cancelCalled = true
	f.cancelID = closureID
	f.cancelIn = &req
	return f.cancelOut, f.cancelErr
}

func (f *fakeClosureBackend) GetClosureStatus(_ context.Context, closureID string) (*clients.ClosureStatusResponse, error) {
	f.statusCalled = true
	f.statusID = closureID
	return f.statusOut, f.statusErr
}

func newClosureTestHandler(t *testing.T, backend ClosureBackend, graceDays int) http.Handler {
	t.Helper()
	// ownsNothing is a REAL lookup answering "this GCID holds no memberships",
	// so every spec below keeps meaning "the subject owns no organisation".
	// It is not a bypass: the ownership block itself is specified in
	// closure_ownership_block_test.go.
	h, err := NewClosureHandler(ClosureHandlerConfig{
		Backend: backend, Ownership: ownsNothing(), GracePeriodDays: graceDays,
	})
	if err != nil {
		t.Fatalf("NewClosureHandler: %v", err)
	}
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot) // sentinel: fell through to base
	})
	return WithClosureRoutes(base, h)
}

func closureReq(t *testing.T, method, target string, body string, claims *servicemesh.MeshClaims) *http.Request {
	t.Helper()
	var rd *bytes.Reader
	if body == "" {
		rd = bytes.NewReader(nil)
	} else {
		rd = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, target, rd)
	if claims != nil {
		req = req.WithContext(withMeshClaims(req.Context(), claims))
	}
	return req
}

func sessionClaims() *servicemesh.MeshClaims {
	return &servicemesh.MeshClaims{GCID: testSessionGCID, TenantID: testTenantID, Roles: []string{"learner"}}
}

func operatorClaims() *servicemesh.MeshClaims {
	// Mixed case on purpose — the gate must compare case-insensitively.
	return &servicemesh.MeshClaims{GCID: testSessionGCID, TenantID: testTenantID, Roles: []string{"learner", "Platform_Operator"}}
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode response body %q: %v", rec.Body.String(), err)
	}
	return m
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope %q: %v", rec.Body.String(), err)
	}
	return env.Error.Code
}

// --- POST /api/v1/me/account/close ------------------------------------------

func TestMeClose_MethodNotAllowed(t *testing.T) {
	h := newClosureTestHandler(t, &fakeClosureBackend{}, 0)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodGet, PathMeAccountClose, "", sessionClaims()))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestMeClose_401WithoutClaims(t *testing.T) {
	h := newClosureTestHandler(t, &fakeClosureBackend{}, 0)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, PathMeAccountClose, `{"reason":"bye"}`, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestMeClose_401WithoutTenantScope(t *testing.T) {
	h := newClosureTestHandler(t, &fakeClosureBackend{}, 0)
	rec := httptest.NewRecorder()
	claims := &servicemesh.MeshClaims{GCID: testSessionGCID, TenantID: ""}
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, PathMeAccountClose, `{"reason":"bye"}`, claims))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestMeClose_202HappyPath_IdentityFromSessionNeverBody(t *testing.T) {
	backend := &fakeClosureBackend{
		requestOut: &clients.ClosureCloseResponse{
			SagaID: testSagaID, State: "closing",
			GraceEndsAt: "2026-07-12T00:00:00Z", RequestedAt: "2026-06-12T00:00:00Z",
		},
	}
	h := newClosureTestHandler(t, backend, 0)
	rec := httptest.NewRecorder()
	// Body smuggles gcid/tenant_id/fast_close — ALL must be ignored: identity
	// comes from the session JWT claims, fast_close is NEVER settable here.
	body := `{"reason":"bye","gcid":"evil","tenant_id":"evil","fast_close":true}`
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, PathMeAccountClose, body, sessionClaims()))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	in := backend.requestIn
	if in == nil {
		t.Fatal("backend not called")
	}
	if in.GCID != testSessionGCID || in.TenantID != testTenantID || in.RequestedByGCID != testSessionGCID {
		t.Fatalf("identity not session-derived: %#v", in)
	}
	if in.FastClose {
		t.Fatal("fast_close MUST NOT be settable on the self-close route")
	}
	if in.Reason != "bye" {
		t.Fatalf("reason = %q, want bye", in.Reason)
	}
	if in.GracePeriodDays != 30 {
		t.Fatalf("grace_period_days = %d, want default 30", in.GracePeriodDays)
	}
	if backend.requestRole != "" {
		t.Fatalf("self-close must not forward X-Chora-Role, got %q", backend.requestRole)
	}
	out := decodeBody(t, rec)
	if out["saga_id"] != testSagaID || out["gcid"] != testSessionGCID || out["state"] != "closing" {
		t.Fatalf("unexpected body: %v", out)
	}
	if out["grace_ends_at"] != "2026-07-12T00:00:00Z" {
		t.Fatalf("grace_ends_at missing: %v", out)
	}
}

func TestMeClose_EmptyBodyAccepted(t *testing.T) {
	backend := &fakeClosureBackend{
		requestOut: &clients.ClosureCloseResponse{SagaID: testSagaID, State: "closing"},
	}
	h := newClosureTestHandler(t, backend, 14)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, PathMeAccountClose, "", sessionClaims()))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if backend.requestIn.GracePeriodDays != 14 {
		t.Fatalf("grace_period_days = %d, want configured 14", backend.requestIn.GracePeriodDays)
	}
}

func TestMeClose_ReasonTooLong400(t *testing.T) {
	backend := &fakeClosureBackend{}
	h := newClosureTestHandler(t, backend, 0)
	rec := httptest.NewRecorder()
	body := `{"reason":"` + strings.Repeat("x", 513) + `"}`
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, PathMeAccountClose, body, sessionClaims()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if backend.requestCalled {
		t.Fatal("backend must not be called on validation failure")
	}
}

func TestMeClose_Upstream409Relayed(t *testing.T) {
	backend := &fakeClosureBackend{
		requestErr: &clients.ClosureUpstreamError{StatusCode: http.StatusConflict, Code: "CLOSURE_ALREADY_EXISTS", Message: "saga exists"},
	}
	h := newClosureTestHandler(t, backend, 0)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, PathMeAccountClose, `{}`, sessionClaims()))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if got := errCode(t, rec); got != "CLOSURE_ALREADY_EXISTS" {
		t.Fatalf("code = %q", got)
	}
}

func TestMeClose_UpstreamUnavailable503(t *testing.T) {
	backend := &fakeClosureBackend{requestErr: clients.ErrClosureUpstreamUnavailable}
	h := newClosureTestHandler(t, backend, 0)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, PathMeAccountClose, `{}`, sessionClaims()))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := errCode(t, rec); got != "CLOSURE_UNAVAILABLE" {
		t.Fatalf("code = %q, want CLOSURE_UNAVAILABLE", got)
	}
}

// --- POST /api/v1/me/account/close/cancel -------------------------------------

func TestMeCancel_400MissingClosureID(t *testing.T) {
	backend := &fakeClosureBackend{}
	h := newClosureTestHandler(t, backend, 0)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, PathMeAccountCloseCancel, `{}`, sessionClaims()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if backend.statusCalled || backend.cancelCalled {
		t.Fatal("backend must not be called without closure_id")
	}
}

func TestMeCancel_404WhenSagaUnknown(t *testing.T) {
	backend := &fakeClosureBackend{
		statusErr: &clients.ClosureUpstreamError{StatusCode: http.StatusNotFound, Code: "CLOSURE_UPSTREAM_REJECTED", Message: "saga not found"},
	}
	h := newClosureTestHandler(t, backend, 0)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, PathMeAccountCloseCancel,
		`{"closure_id":"`+testSagaID+`"}`, sessionClaims()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if backend.cancelCalled {
		t.Fatal("cancel must not run when the saga is unknown")
	}
}

// "Own saga only" — a foreign saga must 404 (same envelope as not-found: no
// existence oracle) and the cancel must never reach the orchestrator.
func TestMeCancel_404OnForeignSaga_NoExistenceOracle(t *testing.T) {
	backend := &fakeClosureBackend{
		statusOut: &clients.ClosureStatusResponse{SagaID: testSagaID, GCID: "someone-else", TenantID: testTenantID, State: "closing"},
	}
	h := newClosureTestHandler(t, backend, 0)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, PathMeAccountCloseCancel,
		`{"closure_id":"`+testSagaID+`"}`, sessionClaims()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := errCode(t, rec); got != "CLOSURE_NOT_FOUND" {
		t.Fatalf("code = %q, want CLOSURE_NOT_FOUND (no oracle)", got)
	}
	if backend.cancelCalled {
		t.Fatal("cancel must not run on a foreign saga")
	}
}

func TestMeCancel_200HappyPath(t *testing.T) {
	backend := &fakeClosureBackend{
		statusOut: &clients.ClosureStatusResponse{SagaID: testSagaID, GCID: testSessionGCID, TenantID: testTenantID, State: "closing"},
		cancelOut: &clients.ClosureCancelResponse{SagaID: testSagaID, State: "active", CancelledAt: "2026-06-12T01:00:00Z"},
	}
	h := newClosureTestHandler(t, backend, 0)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, PathMeAccountCloseCancel,
		`{"closure_id":"`+testSagaID+`","reason":"changed my mind"}`, sessionClaims()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if backend.cancelID != testSagaID {
		t.Fatalf("cancel closure_id = %q", backend.cancelID)
	}
	if backend.cancelIn.ActorGCID != testSessionGCID || backend.cancelIn.Reason != "changed my mind" {
		t.Fatalf("cancel body: %#v", backend.cancelIn)
	}
	out := decodeBody(t, rec)
	if out["saga_id"] != testSagaID || out["state"] != "active" || out["gcid"] != testSessionGCID {
		t.Fatalf("unexpected body: %v", out)
	}
	if out["cancelled_at"] != "2026-06-12T01:00:00Z" {
		t.Fatalf("cancelled_at missing: %v", out)
	}
}

func TestMeCancel_409TooLateRelayed(t *testing.T) {
	backend := &fakeClosureBackend{
		statusOut: &clients.ClosureStatusResponse{SagaID: testSagaID, GCID: testSessionGCID, State: "suspended"},
		cancelErr: &clients.ClosureUpstreamError{StatusCode: http.StatusConflict, Code: "CLOSURE_CANCEL_TOO_LATE", Message: "past grace"},
	}
	h := newClosureTestHandler(t, backend, 0)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, PathMeAccountCloseCancel,
		`{"closure_id":"`+testSagaID+`"}`, sessionClaims()))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if got := errCode(t, rec); got != "CLOSURE_CANCEL_TOO_LATE" {
		t.Fatalf("code = %q", got)
	}
}

// --- GET /api/v1/me/account/closure --------------------------------------------

func TestMeStatus_404MissingClosureIDParam(t *testing.T) {
	backend := &fakeClosureBackend{}
	h := newClosureTestHandler(t, backend, 0)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodGet, PathMeAccountClosure, "", sessionClaims()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if backend.statusCalled {
		t.Fatal("backend must not be called without closure_id")
	}
}

func TestMeStatus_404OnForeignSaga(t *testing.T) {
	backend := &fakeClosureBackend{
		statusOut: &clients.ClosureStatusResponse{SagaID: testSagaID, GCID: "someone-else", State: "closing"},
	}
	h := newClosureTestHandler(t, backend, 0)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodGet, PathMeAccountClosure+"?closure_id="+testSagaID, "", sessionClaims()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestMeStatus_200HappyPath(t *testing.T) {
	backend := &fakeClosureBackend{
		statusOut: &clients.ClosureStatusResponse{
			SagaID: testSagaID, GCID: testSessionGCID, TenantID: testTenantID,
			State: "closing", GraceEndsAt: "2026-07-12T00:00:00Z", RequestedAt: "2026-06-12T00:00:00Z",
			History:    []clients.ClosureHistoryEntry{{PriorState: "active", NewState: "closing"}},
			DomainAcks: []clients.ClosureDomainAck{{Domain: "creation", AckedAt: "2026-06-12T02:00:00Z"}},
		},
	}
	h := newClosureTestHandler(t, backend, 0)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodGet, PathMeAccountClosure+"?closure_id="+testSagaID, "", sessionClaims()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if backend.statusID != testSagaID {
		t.Fatalf("status closure_id = %q", backend.statusID)
	}
	out := decodeBody(t, rec)
	if out["saga_id"] != testSagaID || out["gcid"] != testSessionGCID || out["state"] != "closing" {
		t.Fatalf("unexpected body: %v", out)
	}
	if _, ok := out["domain_acks"].([]any); !ok {
		t.Fatalf("domain_acks missing: %v", out)
	}
}

// --- POST /api/v1/admin/accounts/{gcid}/close -----------------------------------

func adminClosePath(gcid string) string {
	return "/api/v1/admin/accounts/" + gcid + "/close"
}

func TestAdminClose_403WithoutPlatformOperator(t *testing.T) {
	backend := &fakeClosureBackend{}
	h := newClosureTestHandler(t, backend, 0)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, adminClosePath(testTargetGCID),
		`{"fast_close":true,"reason":"test cleanup"}`, sessionClaims()))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if backend.requestCalled {
		t.Fatal("backend must not be called without PLATFORM_OPERATOR")
	}
}

func TestAdminClose_401WithoutClaims(t *testing.T) {
	h := newClosureTestHandler(t, &fakeClosureBackend{}, 0)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, adminClosePath(testTargetGCID), `{}`, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAdminClose_202HappyPath_FastCloseAndRoleHeader(t *testing.T) {
	backend := &fakeClosureBackend{
		requestOut: &clients.ClosureCloseResponse{
			SagaID: testSagaID, State: "closing",
			GraceEndsAt: "2026-06-12T00:00:00Z", RequestedAt: "2026-06-12T00:00:00Z",
		},
	}
	h := newClosureTestHandler(t, backend, 0)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, adminClosePath(testTargetGCID),
		`{"fast_close":true,"reason":"test account cleanup"}`, operatorClaims()))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	in := backend.requestIn
	if in.GCID != testTargetGCID {
		t.Fatalf("target gcid = %q, want path gcid %q", in.GCID, testTargetGCID)
	}
	if in.RequestedByGCID != testSessionGCID || in.TenantID != testTenantID {
		t.Fatalf("operator identity not session-derived: %#v", in)
	}
	if !in.FastClose {
		t.Fatal("fast_close from body must be forwarded on the operator route")
	}
	if backend.requestRole != "PLATFORM_OPERATOR" {
		t.Fatalf("role header = %q, want PLATFORM_OPERATOR", backend.requestRole)
	}
	out := decodeBody(t, rec)
	if out["gcid"] != testTargetGCID || out["saga_id"] != testSagaID {
		t.Fatalf("unexpected body: %v", out)
	}
}

func TestAdminClose_EmptyBodyDefaultsNoFastClose(t *testing.T) {
	backend := &fakeClosureBackend{
		requestOut: &clients.ClosureCloseResponse{SagaID: testSagaID, State: "closing"},
	}
	h := newClosureTestHandler(t, backend, 0)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, adminClosePath(testTargetGCID), "", operatorClaims()))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if backend.requestIn.FastClose {
		t.Fatal("fast_close must default to false")
	}
}

func TestAdminClose_RelaysUpstream403(t *testing.T) {
	backend := &fakeClosureBackend{
		requestErr: &clients.ClosureUpstreamError{StatusCode: http.StatusForbidden, Code: "CLOSURE_AGID_REJECTED", Message: "agents have no lifecycle"},
	}
	h := newClosureTestHandler(t, backend, 0)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, adminClosePath(testTargetGCID), `{}`, operatorClaims()))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := errCode(t, rec); got != "CLOSURE_AGID_REJECTED" {
		t.Fatalf("code = %q", got)
	}
}

// Suspend/reactivate/list/lifecycle-events siblings under
// /api/v1/admin/accounts/ must keep falling through to the base handler —
// the closure mux owns ONLY the {gcid}/close leaf.
func TestAdminAccountsSiblingsFallThrough(t *testing.T) {
	h := newClosureTestHandler(t, &fakeClosureBackend{}, 0)
	for _, target := range []string{
		"/api/v1/admin/accounts/" + testTargetGCID + "/suspend",
		"/api/v1/admin/accounts",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, closureReq(t, http.MethodPost, target, `{}`, operatorClaims()))
		if rec.Code != http.StatusTeapot {
			t.Fatalf("%s: status = %d, want fallthrough 418", target, rec.Code)
		}
	}
}

// --- degraded mode (CLOSURE_ORCHESTRATOR_URL unset) -----------------------------

func TestWithClosureRoutes_NilHandler503OnClosurePaths(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := WithClosureRoutes(base, nil)
	for _, tc := range []struct{ method, target string }{
		{http.MethodPost, PathMeAccountClose},
		{http.MethodPost, PathMeAccountCloseCancel},
		{http.MethodGet, PathMeAccountClosure},
		{http.MethodPost, adminClosePath(testTargetGCID)},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, closureReq(t, tc.method, tc.target, "", sessionClaims()))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: status = %d, want 503", tc.method, tc.target, rec.Code)
		}
		if got := errCode(t, rec); got != "CLOSURE_UNAVAILABLE" {
			t.Fatalf("%s %s: code = %q, want CLOSURE_UNAVAILABLE", tc.method, tc.target, got)
		}
	}
	// Unrelated paths still reach base.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodGet, "/api/v1/me/mana", "", sessionClaims()))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("unrelated path: status = %d, want fallthrough 418", rec.Code)
	}
}

func TestWithClosureRoutes_FallsThroughForUnrelatedPaths(t *testing.T) {
	h := newClosureTestHandler(t, &fakeClosureBackend{}, 0)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodGet, "/api/v1/me/mana", "", sessionClaims()))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want fallthrough 418", rec.Code)
	}
}

// NewClosureHandler fails loud without a backend (no silent stub — per
// feedback_no_inline_config / no-stubs rules).
func TestNewClosureHandler_RequiresBackend(t *testing.T) {
	if _, err := NewClosureHandler(ClosureHandlerConfig{}); err == nil {
		t.Fatal("want error on nil backend")
	}
}

// The me/account subtree must be JWT-gated so RequireChoraSessionJWT stamps
// the MeshClaims this handler reads.
func TestDefaultJWTGatedPrefixes_CoverMeAccount(t *testing.T) {
	covered := false
	for _, p := range DefaultJWTGatedPrefixes {
		if strings.HasPrefix(PathMeAccountClose, p) {
			covered = true
			break
		}
	}
	if !covered {
		t.Fatalf("DefaultJWTGatedPrefixes must cover %s", PathMeAccountClose)
	}
}
