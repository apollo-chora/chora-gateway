// Internal-package (white-box) test so specs can stamp MeshClaims (incl. Roles
// for the platform_operator gate) directly via withMeshClaims — the JWT
// middleware is exercised separately in jwt_auth_test.go.
package httpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
)

const (
	gcidA = "11111111-1111-7111-8111-111111111111"
	gcidB = "22222222-2222-7222-8222-222222222222"
	gcidC = "33333333-3333-7333-8333-333333333333"
)

type fakeTxClient struct {
	lastCaller      clients.Caller
	lastParams      clients.TxListParams
	lastDetScope    clients.TxScope
	lastLedgerID    string
	lastFormat      string
	lastJobID       string
	listResp        clients.TxListResponseDTO
	summaryResp     clients.TxSummaryDTO
	detailResp      clients.TxDetailResponseDTO
	exportResp      clients.TxExportJobDTO
	franchiseesResp clients.TxFranchiseesDTO
	lastQ           string
	err             error
}

func (f *fakeTxClient) ListFranchisees(_ context.Context, caller clients.Caller, q string, _ int32, _ string) (clients.TxFranchiseesDTO, error) {
	f.lastCaller, f.lastQ = caller, q
	return f.franchiseesResp, f.err
}

func (f *fakeTxClient) CreateExport(_ context.Context, caller clients.Caller, p clients.TxListParams, format string) (clients.TxExportJobDTO, error) {
	f.lastCaller, f.lastParams, f.lastFormat = caller, p, format
	return f.exportResp, f.err
}
func (f *fakeTxClient) GetExport(_ context.Context, caller clients.Caller, scope clients.TxScope, jobID string) (clients.TxExportJobDTO, error) {
	f.lastCaller, f.lastDetScope, f.lastJobID = caller, scope, jobID
	return f.exportResp, f.err
}

func (f *fakeTxClient) List(_ context.Context, caller clients.Caller, p clients.TxListParams) (clients.TxListResponseDTO, error) {
	f.lastCaller, f.lastParams = caller, p
	return f.listResp, f.err
}
func (f *fakeTxClient) Summary(_ context.Context, caller clients.Caller, p clients.TxListParams) (clients.TxSummaryDTO, error) {
	f.lastCaller, f.lastParams = caller, p
	return f.summaryResp, f.err
}
func (f *fakeTxClient) Detail(_ context.Context, caller clients.Caller, scope clients.TxScope, ledgerID, _ string, _ int32) (clients.TxDetailResponseDTO, error) {
	f.lastCaller, f.lastDetScope, f.lastLedgerID = caller, scope, ledgerID
	return f.detailResp, f.err
}

func txServeWith(t *testing.T, fake *fakeTxClient, claims *servicemesh.MeshClaims, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	h, err := NewTransactionHistoryHandler(fake)
	if err != nil {
		t.Fatalf("NewTransactionHistoryHandler: %v", err)
	}
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	req := httptest.NewRequest(method, target, nil)
	if claims != nil {
		req = req.WithContext(withMeshClaims(req.Context(), claims))
	}
	rec := httptest.NewRecorder()
	WithTransactionHistory(base, h).ServeHTTP(rec, req)
	return rec
}

func learnerClaims() *servicemesh.MeshClaims {
	return &servicemesh.MeshClaims{GCID: "g-learner", TenantID: "t-1", Roles: []string{"learner"}}
}
func adminClaims() *servicemesh.MeshClaims {
	return &servicemesh.MeshClaims{GCID: "g-admin", TenantID: "t-1", Roles: []string{"tenant_admin"}}
}
func txOperatorClaims() *servicemesh.MeshClaims {
	// Operator with NO tenant membership (ADR-165) + mixed-case role token.
	return &servicemesh.MeshClaims{GCID: "g-op", TenantID: "", Roles: []string{"Platform_Operator"}}
}

func TestList_Learner_BindsScopeAndIdentity(t *testing.T) {
	fake := &fakeTxClient{listResp: clients.TxListResponseDTO{HasMore: false}}
	rec := txServeWith(t, fake, learnerClaims(), http.MethodGet, txLearnerPrefix+"?page_size=20&kind=purchase")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if fake.lastParams.Scope != clients.TxScopeLearner {
		t.Errorf("scope = %v; want learner", fake.lastParams.Scope)
	}
	if fake.lastCaller.GCID != "g-learner" || fake.lastCaller.TenantID != "t-1" {
		t.Errorf("caller = %+v", fake.lastCaller)
	}
	if fake.lastParams.Kind != "purchase" || fake.lastParams.PageSize != 20 {
		t.Errorf("params = %+v", fake.lastParams)
	}
}

func TestList_Learner_MissingTenant_401(t *testing.T) {
	fake := &fakeTxClient{}
	claims := &servicemesh.MeshClaims{GCID: "g-learner", TenantID: ""}
	rec := txServeWith(t, fake, claims, http.MethodGet, txLearnerPrefix)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", rec.Code)
	}
}

func TestList_Admin_NonOperator_TenantScope(t *testing.T) {
	fake := &fakeTxClient{}
	rec := txServeWith(t, fake, adminClaims(), http.MethodGet, txAdminPrefix)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if fake.lastParams.Scope != clients.TxScopeTenant {
		t.Errorf("scope = %v; want tenant", fake.lastParams.Scope)
	}
}

func TestList_Admin_Operator_MasterScope_NoTenantRequired(t *testing.T) {
	fake := &fakeTxClient{}
	rec := txServeWith(t, fake, txOperatorClaims(), http.MethodGet, txAdminPrefix+"?managed_tenant_id="+gcidA)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if fake.lastParams.Scope != clients.TxScopeMaster {
		t.Errorf("scope = %v; want master (operator)", fake.lastParams.Scope)
	}
	if fake.lastCaller.GCID != "g-op" {
		t.Errorf("operator caller gcid = %q", fake.lastCaller.GCID)
	}
	if len(fake.lastParams.ManagedTenantIDs) != 1 || fake.lastParams.ManagedTenantIDs[0] != gcidA {
		t.Errorf("franchisee selector not forwarded: %v", fake.lastParams.ManagedTenantIDs)
	}
	// roles must reach the client so it can stamp x-mesh-user-roles.
	if len(fake.lastCaller.Roles) != 1 || fake.lastCaller.Roles[0] != "Platform_Operator" {
		t.Errorf("roles not forwarded to client: %v", fake.lastCaller.Roles)
	}
}

func TestList_Admin_MultiLearner_ForwardsRepeatedAndCommaJoined(t *testing.T) {
	// Repeated params + a comma-joined value both flatten into the slice.
	fake := &fakeTxClient{}
	u := txAdminPrefix + "?learner_gcid=" + gcidA + "&learner_gcid=" + gcidB + "," + gcidC
	rec := txServeWith(t, fake, adminClaims(), http.MethodGet, u)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	got := fake.lastParams.LearnerGCIDs
	if len(got) != 3 || got[0] != gcidA || got[1] != gcidB || got[2] != gcidC {
		t.Errorf("learner set = %v; want [%s %s %s]", got, gcidA, gcidB, gcidC)
	}
}

func TestList_Admin_MalformedLearnerGcid_400(t *testing.T) {
	fake := &fakeTxClient{}
	rec := txServeWith(t, fake, adminClaims(), http.MethodGet, txAdminPrefix+"?learner_gcid=not-a-uuid")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", rec.Code)
	}
}

func TestList_Admin_MultiFranchisee_ForwardsRepeatedAndCommaJoined(t *testing.T) {
	// Repeated franchisee params + a comma-joined value both flatten into the
	// slice forwarded to the tenancy server (operator/MASTER scope).
	fake := &fakeTxClient{}
	u := txAdminPrefix + "?managed_tenant_id=" + gcidA + "&managed_tenant_id=" + gcidB + "," + gcidC
	rec := txServeWith(t, fake, txOperatorClaims(), http.MethodGet, u)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	got := fake.lastParams.ManagedTenantIDs
	if len(got) != 3 || got[0] != gcidA || got[1] != gcidB || got[2] != gcidC {
		t.Errorf("franchisee set = %v; want [%s %s %s]", got, gcidA, gcidB, gcidC)
	}
}

func TestFranchisees_Master_ReturnsList(t *testing.T) {
	fake := &fakeTxClient{franchiseesResp: clients.TxFranchiseesDTO{
		Franchisees:   []clients.TxFranchiseeDTO{{TenantID: gcidA, Name: "Acme Franchise"}},
		NextPageToken: "50",
	}}
	rec := txServeWith(t, fake, txOperatorClaims(), http.MethodGet, txAdminPrefix+"/franchisees?q=acm")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if fake.lastQ != "acm" {
		t.Errorf("q forwarded = %q; want acm", fake.lastQ)
	}
	if fake.lastCaller.GCID != "g-op" {
		t.Errorf("operator caller gcid = %q", fake.lastCaller.GCID)
	}
}

func TestFranchisees_NonMaster_403(t *testing.T) {
	// A tenant admin (non-operator) may NOT list cross-tenant franchisees.
	fake := &fakeTxClient{}
	rec := txServeWith(t, fake, adminClaims(), http.MethodGet, txAdminPrefix+"/franchisees")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 (non-operator)", rec.Code)
	}
}

func TestFranchisees_MethodNotGet_405(t *testing.T) {
	fake := &fakeTxClient{}
	rec := txServeWith(t, fake, txOperatorClaims(), http.MethodPost, txAdminPrefix+"/franchisees")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d; want 405", rec.Code)
	}
}

func TestList_Admin_MalformedFranchisee_400(t *testing.T) {
	fake := &fakeTxClient{}
	rec := txServeWith(t, fake, txOperatorClaims(), http.MethodGet, txAdminPrefix+"?managed_tenant_id=not-a-uuid")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", rec.Code)
	}
}

func TestList_Learner_LearnerGcidParam_Ignored(t *testing.T) {
	// A learner-scope caller's learner_gcid param is not forwarded (own rows).
	fake := &fakeTxClient{}
	rec := txServeWith(t, fake, learnerClaims(), http.MethodGet, txLearnerPrefix+"?learner_gcid="+gcidA)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(fake.lastParams.LearnerGCIDs) != 0 {
		t.Errorf("learner scope must not forward a learner_gcid selector, got %v", fake.lastParams.LearnerGCIDs)
	}
}

func TestSummary_Learner_RoutesToSummary(t *testing.T) {
	fake := &fakeTxClient{summaryResp: clients.TxSummaryDTO{TotalCount: 7}}
	rec := txServeWith(t, fake, learnerClaims(), http.MethodGet, txLearnerPrefix+"/summary")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got clients.TxSummaryDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TotalCount != 7 {
		t.Errorf("summary total = %d", got.TotalCount)
	}
}

func TestDetail_Learner_RoutesWithLedgerID(t *testing.T) {
	fake := &fakeTxClient{detailResp: clients.TxDetailResponseDTO{Parent: clients.TxLedgerItemDTO{LedgerID: "L9"}}}
	rec := txServeWith(t, fake, learnerClaims(), http.MethodGet, txLearnerPrefix+"/L9")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if fake.lastLedgerID != "L9" || fake.lastDetScope != clients.TxScopeLearner {
		t.Errorf("detail routed wrong: id=%q scope=%v", fake.lastLedgerID, fake.lastDetScope)
	}
}

func TestDetail_NotFound_Maps404(t *testing.T) {
	fake := &fakeTxClient{err: fmt.Errorf("client: %w", status.Error(codes.NotFound, "no row"))}
	rec := txServeWith(t, fake, learnerClaims(), http.MethodGet, txLearnerPrefix+"/missing")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404", rec.Code)
	}
}

func TestExportCreate_202(t *testing.T) {
	fake := &fakeTxClient{exportResp: clients.TxExportJobDTO{JobID: "EJ1", Status: "pending", Format: "csv"}}
	rec := txServeWith(t, fake, learnerClaims(), http.MethodGet, txLearnerPrefix+"/export?format=csv&kind=purchase")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202, body=%s", rec.Code, rec.Body.String())
	}
	if fake.lastFormat != "csv" || fake.lastParams.Scope != clients.TxScopeLearner || fake.lastParams.Kind != "purchase" {
		t.Errorf("export create params wrong: fmt=%q %+v", fake.lastFormat, fake.lastParams)
	}
	var job clients.TxExportJobDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil || job.JobID != "EJ1" {
		t.Errorf("export job body wrong: %v / %+v", err, job)
	}
}

func TestExportCreate_MissingOrBadFormat_400(t *testing.T) {
	for _, qs := range []string{"/export", "/export?format=xml"} {
		rec := txServeWith(t, &fakeTxClient{}, learnerClaims(), http.MethodGet, txLearnerPrefix+qs)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d; want 400", qs, rec.Code)
		}
	}
}

func TestExportPoll_200(t *testing.T) {
	url := "https://signed/x"
	fake := &fakeTxClient{exportResp: clients.TxExportJobDTO{JobID: "EJ9", Status: "ready", Format: "json", DownloadURL: &url}}
	rec := txServeWith(t, fake, learnerClaims(), http.MethodGet, txLearnerPrefix+"/export/jobs/EJ9")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if fake.lastJobID != "EJ9" || fake.lastDetScope != clients.TxScopeLearner {
		t.Errorf("export poll routed wrong: job=%q scope=%v", fake.lastJobID, fake.lastDetScope)
	}
}

func TestExportPoll_AdminOperatorMasterScope(t *testing.T) {
	fake := &fakeTxClient{exportResp: clients.TxExportJobDTO{JobID: "EJ", Status: "building"}}
	rec := txServeWith(t, fake, txOperatorClaims(), http.MethodGet, txAdminPrefix+"/export/jobs/EJ")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if fake.lastDetScope != clients.TxScopeMaster {
		t.Errorf("operator export poll scope = %v; want master", fake.lastDetScope)
	}
}

func TestStream_501(t *testing.T) {
	rec := txServeWith(t, &fakeTxClient{}, adminClaims(), http.MethodGet, txAdminPrefix+"/stream")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501", rec.Code)
	}
}

func TestInvalidParams_400(t *testing.T) {
	cases := []string{"?kind=bogus", "?status=bogus", "?sort=bogus", "?page_size=7", "?from=not-a-time"}
	for _, qs := range cases {
		rec := txServeWith(t, &fakeTxClient{}, learnerClaims(), http.MethodGet, txLearnerPrefix+qs)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d; want 400", qs, rec.Code)
		}
	}
}

func TestStatusAll_NotForwardedAsFilter(t *testing.T) {
	fake := &fakeTxClient{}
	rec := txServeWith(t, fake, learnerClaims(), http.MethodGet, txLearnerPrefix+"?status=all")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if fake.lastParams.Status != "" {
		t.Errorf("status=all should map to no filter, got %q", fake.lastParams.Status)
	}
}

func TestList_GRPCErrorMapping(t *testing.T) {
	cases := []struct {
		code codes.Code
		want int
	}{
		{codes.PermissionDenied, http.StatusForbidden},     // non-operator → master gate
		{codes.InvalidArgument, http.StatusBadRequest},     // bad franchisee id
		{codes.Unauthenticated, http.StatusUnauthorized},   // missing mesh claim
		{codes.Unavailable, http.StatusServiceUnavailable}, // tenancy down
		{codes.Internal, http.StatusInternalServerError},   // default arm
	}
	for _, c := range cases {
		fake := &fakeTxClient{err: fmt.Errorf("wrapped: %w", status.Error(c.code, "x"))}
		rec := txServeWith(t, fake, learnerClaims(), http.MethodGet, txLearnerPrefix)
		if rec.Code != c.want {
			t.Errorf("gRPC %v → HTTP %d; want %d", c.code, rec.Code, c.want)
		}
	}
	// A non-gRPC error collapses to 500.
	fake := &fakeTxClient{err: errors.New("plain boom")}
	rec := txServeWith(t, fake, learnerClaims(), http.MethodGet, txLearnerPrefix)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("non-grpc err → %d; want 500", rec.Code)
	}
}

func TestNonGET_405(t *testing.T) {
	rec := txServeWith(t, &fakeTxClient{}, learnerClaims(), http.MethodPost, txLearnerPrefix)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d; want 405", rec.Code)
	}
}

func TestUnrelatedPath_FallsThrough(t *testing.T) {
	rec := txServeWith(t, &fakeTxClient{}, learnerClaims(), http.MethodGet, "/api/v1/something/else")
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d; want 418 (fell through to base)", rec.Code)
	}
}
