// Internal-package (white-box) test so specs can stamp MeshClaims (incl. Roles
// for the platform_operator gate) directly via withMeshClaims. Mirrors
// transaction_history_handler_test.go.
package httpadapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
)

const (
	stParent = "11111111-1111-7111-8111-111111111111"
	stOwner  = "99999999-9999-7999-8999-999999999999"
	stChild  = "44444444-4444-7444-8444-444444444444"
)

// fakeSubTenantCreator satisfies the combined TenancyAdminClient port: both the
// create-path (Phase 2.2) and the hierarchy read-path (Phase 2.3b).
type fakeSubTenantCreator struct {
	called     bool
	lastCaller clients.Caller
	lastParams clients.SubTenantParams
	resp       clients.TenantDTO
	err        error

	// hierarchy read (Phase 2.3b, CHO-2010)
	hierCalled     bool
	hierLastCaller clients.Caller
	hierLastTenant string
	hierResp       clients.TenantHierarchyDTO
	hierErr        error
}

func (f *fakeSubTenantCreator) CreateSubTenant(_ context.Context, caller clients.Caller, p clients.SubTenantParams) (clients.TenantDTO, error) {
	f.called, f.lastCaller, f.lastParams = true, caller, p
	return f.resp, f.err
}

func (f *fakeSubTenantCreator) GetTenantHierarchy(_ context.Context, caller clients.Caller, tenantID string) (clients.TenantHierarchyDTO, error) {
	f.hierCalled, f.hierLastCaller, f.hierLastTenant = true, caller, tenantID
	return f.hierResp, f.hierErr
}

const subTenantsTarget = "/api/v1/tenancy/sub-tenants"

func stServeWith(t *testing.T, fake *fakeSubTenantCreator, claims *servicemesh.MeshClaims, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	h, err := NewTenancyAdminHandler(fake)
	if err != nil {
		t.Fatalf("NewTenancyAdminHandler: %v", err)
	}
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if claims != nil {
		req = req.WithContext(withMeshClaims(req.Context(), claims))
	}
	rec := httptest.NewRecorder()
	WithTenancyAdmin(base, h).ServeHTTP(rec, req)
	return rec
}

func stOperatorClaims() *servicemesh.MeshClaims {
	// Operator with NO tenant membership (ADR-165) + mixed-case/dash role token.
	return &servicemesh.MeshClaims{GCID: "g-op", TenantID: "", Roles: []string{"Platform-Operator"}}
}
func stTenantAdminClaims() *servicemesh.MeshClaims {
	return &servicemesh.MeshClaims{GCID: "g-admin", TenantID: "t-1", Roles: []string{"tenant_admin"}}
}

func validBody() string {
	return `{"parent_tenant_id":"` + stParent + `","display_name":"Acme Franchise","hosting_mode":"FRANCHISE","owner_gcid":"` + stOwner + `"}`
}

func TestSubTenant_OperatorCreate_201(t *testing.T) {
	fake := &fakeSubTenantCreator{resp: clients.TenantDTO{
		TenantID: stChild, DisplayName: "Acme Franchise", HostingMode: "FRANCHISE",
		OwnerGCID: stOwner, State: "ACTIVE", CreatedAt: "2026-07-02T12:00:00Z",
	}}
	rec := stServeWith(t, fake, stOperatorClaims(), http.MethodPost, subTenantsTarget, validBody())
	if rec.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if !fake.called {
		t.Fatal("client not called")
	}
	if fake.lastCaller.GCID != "g-op" {
		t.Errorf("caller.GCID = %q, want g-op", fake.lastCaller.GCID)
	}
	if fake.lastParams.ParentTenantID != stParent || fake.lastParams.OwnerGCID != stOwner || fake.lastParams.HostingMode != "FRANCHISE" {
		t.Errorf("params = %+v", fake.lastParams)
	}
	var dto clients.TenantDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.TenantID != stChild || dto.State != "ACTIVE" {
		t.Errorf("dto = %+v", dto)
	}
}

func TestSubTenant_DefaultHostingMode_PassesEmpty(t *testing.T) {
	fake := &fakeSubTenantCreator{resp: clients.TenantDTO{TenantID: stChild, HostingMode: "FRANCHISE"}}
	body := `{"parent_tenant_id":"` + stParent + `","display_name":"Acme","owner_gcid":"` + stOwner + `"}`
	rec := stServeWith(t, fake, stOperatorClaims(), http.MethodPost, subTenantsTarget, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if fake.lastParams.HostingMode != "" {
		t.Errorf("HostingMode = %q, want empty (server defaults FRANCHISE)", fake.lastParams.HostingMode)
	}
}

func TestSubTenant_NonOperator_403_NoCall(t *testing.T) {
	fake := &fakeSubTenantCreator{}
	rec := stServeWith(t, fake, stTenantAdminClaims(), http.MethodPost, subTenantsTarget, validBody())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rec.Code)
	}
	if fake.called {
		t.Error("client must NOT be called for a non-operator (sole app-layer authz gate)")
	}
}

func TestSubTenant_Unauthenticated_401_NoCall(t *testing.T) {
	fake := &fakeSubTenantCreator{}
	rec := stServeWith(t, fake, nil, http.MethodPost, subTenantsTarget, validBody())
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
	if fake.called {
		t.Error("client must NOT be called when unauthenticated")
	}
}

func TestSubTenant_MissingFields_400(t *testing.T) {
	cases := map[string]string{
		"no parent":  `{"display_name":"A","owner_gcid":"` + stOwner + `"}`,
		"no name":    `{"parent_tenant_id":"` + stParent + `","owner_gcid":"` + stOwner + `"}`,
		"no owner":   `{"parent_tenant_id":"` + stParent + `","display_name":"A"}`,
		"empty body": `{}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &fakeSubTenantCreator{}
			rec := stServeWith(t, fake, stOperatorClaims(), http.MethodPost, subTenantsTarget, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400", rec.Code)
			}
			if fake.called {
				t.Error("client must NOT be called on a validation error")
			}
		})
	}
}

func TestSubTenant_MalformedUUID_400(t *testing.T) {
	fake := &fakeSubTenantCreator{}
	body := `{"parent_tenant_id":"not-a-uuid","display_name":"A","owner_gcid":"` + stOwner + `"}`
	rec := stServeWith(t, fake, stOperatorClaims(), http.MethodPost, subTenantsTarget, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
}

func TestSubTenant_InvalidHostingMode_400(t *testing.T) {
	fake := &fakeSubTenantCreator{}
	body := `{"parent_tenant_id":"` + stParent + `","display_name":"A","hosting_mode":"BOGUS","owner_gcid":"` + stOwner + `"}`
	rec := stServeWith(t, fake, stOperatorClaims(), http.MethodPost, subTenantsTarget, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
	if fake.called {
		t.Error("client must NOT be called on an invalid hosting_mode")
	}
}

func TestSubTenant_MethodGet_405(t *testing.T) {
	fake := &fakeSubTenantCreator{}
	rec := stServeWith(t, fake, stOperatorClaims(), http.MethodGet, subTenantsTarget, "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code = %d, want 405", rec.Code)
	}
}

func TestSubTenant_UpstreamFailedPrecondition_409(t *testing.T) {
	fake := &fakeSubTenantCreator{err: status.Error(codes.FailedPrecondition, "foreign root")}
	rec := stServeWith(t, fake, stOperatorClaims(), http.MethodPost, subTenantsTarget, validBody())
	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", rec.Code)
	}
}

func TestSubTenant_UpstreamInvalidArgument_400(t *testing.T) {
	fake := &fakeSubTenantCreator{err: status.Error(codes.InvalidArgument, "bad")}
	rec := stServeWith(t, fake, stOperatorClaims(), http.MethodPost, subTenantsTarget, validBody())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
}

func TestWithTenancyAdmin_NilHandler_PassesThrough(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	req := httptest.NewRequest(http.MethodPost, subTenantsTarget, strings.NewReader(validBody()))
	rec := httptest.NewRecorder()
	WithTenancyAdmin(base, nil).ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("code = %d, want 418 (pass-through)", rec.Code)
	}
}

func TestWithTenancyAdmin_UnknownPath_FallsThrough(t *testing.T) {
	fake := &fakeSubTenantCreator{}
	rec := stServeWith(t, fake, stOperatorClaims(), http.MethodPost, "/api/v1/tenancy/other", "{}")
	if rec.Code != http.StatusTeapot {
		t.Fatalf("code = %d, want 418 (fall-through to base)", rec.Code)
	}
	if fake.called {
		t.Error("must not handle unknown tenancy paths")
	}
}

// -----------------------------------------------------------------------------
// Phase 2.3b (CHO-2010) — GET /api/v1/tenancy/tenants/current/hierarchy
// -----------------------------------------------------------------------------

const hierarchyTarget = "/api/v1/tenancy/tenants/current/hierarchy"

func stOperatorWithTenantClaims() *servicemesh.MeshClaims {
	// Operator whose active session tenant IS resolvable (e.g. chora-master).
	return &servicemesh.MeshClaims{GCID: "g-op", TenantID: "00000000-0000-7000-8000-000000000001", Roles: []string{"platform_operator"}}
}
func stLearnerClaims() *servicemesh.MeshClaims {
	return &servicemesh.MeshClaims{GCID: "g-learner", TenantID: "00000000-0000-7000-8000-000000000001", Roles: []string{"learner"}}
}

func TestHierarchy_TenantAdmin_200(t *testing.T) {
	fake := &fakeSubTenantCreator{hierResp: clients.TenantHierarchyDTO{
		IsParent: true,
		Children: []clients.ChildTenantSummaryDTO{{TenantID: stChild, TenantName: "Bishan Branch", UserCount: 7, AtomCount: 0}},
	}}
	rec := stServeWith(t, fake, stTenantAdminClaims(), http.MethodGet, hierarchyTarget, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !fake.hierCalled {
		t.Fatal("client not called")
	}
	// current tenant is resolved from the session, never client-supplied.
	if fake.hierLastTenant != "t-1" || fake.hierLastCaller.GCID != "g-admin" {
		t.Errorf("caller/tenant = %q / %+v", fake.hierLastTenant, fake.hierLastCaller)
	}
	var dto clients.TenantHierarchyDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !dto.IsParent || len(dto.Children) != 1 || dto.Children[0].TenantID != stChild || dto.Children[0].UserCount != 7 {
		t.Errorf("dto = %+v", dto)
	}
}

func TestHierarchy_Operator_200(t *testing.T) {
	fake := &fakeSubTenantCreator{hierResp: clients.TenantHierarchyDTO{IsParent: false, Children: []clients.ChildTenantSummaryDTO{}}}
	rec := stServeWith(t, fake, stOperatorWithTenantClaims(), http.MethodGet, hierarchyTarget, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !fake.hierCalled {
		t.Fatal("operator must be allowed to read hierarchy")
	}
}

func TestHierarchy_NonAdmin_403_NoCall(t *testing.T) {
	// A plain learner (possibly auto-enrolled into chora-master per ADR-182)
	// must NOT enumerate child tenants + user counts.
	fake := &fakeSubTenantCreator{}
	rec := stServeWith(t, fake, stLearnerClaims(), http.MethodGet, hierarchyTarget, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rec.Code)
	}
	if fake.hierCalled {
		t.Error("client must NOT be called for a non-admin (leak guard)")
	}
}

func TestHierarchy_Unauthenticated_401_NoCall(t *testing.T) {
	fake := &fakeSubTenantCreator{}
	rec := stServeWith(t, fake, nil, http.MethodGet, hierarchyTarget, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
	if fake.hierCalled {
		t.Error("client must NOT be called when unauthenticated")
	}
}

func TestHierarchy_NoCurrentTenant_401_NoCall(t *testing.T) {
	// Operator with NO active tenant (ADR-165) — "current" is unresolvable.
	fake := &fakeSubTenantCreator{}
	rec := stServeWith(t, fake, stOperatorClaims(), http.MethodGet, hierarchyTarget, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
	if fake.hierCalled {
		t.Error("client must NOT be called when the current tenant is missing")
	}
}

func TestHierarchy_MethodPost_405(t *testing.T) {
	fake := &fakeSubTenantCreator{}
	rec := stServeWith(t, fake, stTenantAdminClaims(), http.MethodPost, hierarchyTarget, "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code = %d, want 405", rec.Code)
	}
}

func TestHierarchy_UpstreamInternal_500(t *testing.T) {
	fake := &fakeSubTenantCreator{hierErr: status.Error(codes.Internal, "boom")}
	rec := stServeWith(t, fake, stTenantAdminClaims(), http.MethodGet, hierarchyTarget, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500", rec.Code)
	}
}
