package clients_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	tenancyv1 "github.com/apollo-chora/chora-contracts/gen/go/chora/services/tenancy/v1"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
)

// mustTime is a fixed timestamp for deterministic response assertions.
func mustTime() time.Time { return time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC) }

// fakeTenancyRPC implements the narrow clients.TenancyGRPCClient port.
type fakeTenancyRPC struct {
	gotCtx context.Context
	gotReq *tenancyv1.CreateSubTenantRequest
	resp   *tenancyv1.CreateSubTenantResponse
	err    error

	// hierarchy (Phase 2.3b, CHO-2010)
	gotHierCtx context.Context
	gotHierReq *tenancyv1.GetTenantHierarchyRequest
	hierResp   *tenancyv1.GetTenantHierarchyResponse
	hierErr    error

	// cross-tenant membership directory (E3 closure ownership pre-flight)
	gotMembershipsCtx context.Context
	gotMembershipsReq *tenancyv1.ListMembershipsByGCIDRequest
	membershipsResp   *tenancyv1.ListMembershipsByGCIDResponse
	membershipsErr    error
}

func (f *fakeTenancyRPC) CreateSubTenant(ctx context.Context, in *tenancyv1.CreateSubTenantRequest, _ ...grpc.CallOption) (*tenancyv1.CreateSubTenantResponse, error) {
	f.gotCtx, f.gotReq = ctx, in
	return f.resp, f.err
}

func (f *fakeTenancyRPC) GetTenantHierarchy(ctx context.Context, in *tenancyv1.GetTenantHierarchyRequest, _ ...grpc.CallOption) (*tenancyv1.GetTenantHierarchyResponse, error) {
	f.gotHierCtx, f.gotHierReq = ctx, in
	return f.hierResp, f.hierErr
}

func (f *fakeTenancyRPC) ListMembershipsByGCID(ctx context.Context, in *tenancyv1.ListMembershipsByGCIDRequest, _ ...grpc.CallOption) (*tenancyv1.ListMembershipsByGCIDResponse, error) {
	f.gotMembershipsCtx, f.gotMembershipsReq = ctx, in
	return f.membershipsResp, f.membershipsErr
}

func newTenancyAdmin(t *testing.T, rpc clients.TenancyGRPCClient) *clients.TenancyAdminClient {
	t.Helper()
	c, err := clients.NewTenancyAdminClient(clients.TenancyAdminClientConfig{RPC: rpc})
	if err != nil {
		t.Fatalf("NewTenancyAdminClient: %v", err)
	}
	return c
}

const (
	parentID = "11111111-1111-7111-8111-111111111111"
	ownerID  = "99999999-9999-7999-8999-999999999999"
	childID  = "44444444-4444-7444-8444-444444444444"
)

func TestCreateSubTenant_MapsRequest_StampsMesh(t *testing.T) {
	fake := &fakeTenancyRPC{resp: &tenancyv1.CreateSubTenantResponse{Tenant: &tenancyv1.Tenant{
		TenantId:       childID,
		ParentTenantId: parentID,
		DisplayName:    "Acme Franchise",
		HostingMode:    tenancyv1.HostingMode_HOSTING_MODE_FRANCHISE,
		OwnerGcid:      ownerID,
		State:          tenancyv1.TenantState_TENANT_STATE_ACTIVE,
		CreatedAt:      timestamppb.New(mustTime()),
	}}}
	c := newTenancyAdmin(t, fake)

	dto, err := c.CreateSubTenant(context.Background(),
		clients.Caller{GCID: "g-op", TenantID: "", Roles: []string{"platform_operator"}},
		clients.SubTenantParams{ParentTenantID: parentID, DisplayName: "Acme Franchise", HostingMode: "FRANCHISE", OwnerGCID: ownerID})
	if err != nil {
		t.Fatalf("CreateSubTenant: %v", err)
	}

	// Outbound request mapped correctly.
	if fake.gotReq.GetParentTenantId() != parentID {
		t.Errorf("ParentTenantId = %q, want %q", fake.gotReq.GetParentTenantId(), parentID)
	}
	if fake.gotReq.GetDisplayName() != "Acme Franchise" {
		t.Errorf("DisplayName = %q", fake.gotReq.GetDisplayName())
	}
	if fake.gotReq.GetHostingMode() != tenancyv1.HostingMode_HOSTING_MODE_FRANCHISE {
		t.Errorf("HostingMode = %v, want FRANCHISE", fake.gotReq.GetHostingMode())
	}
	if fake.gotReq.GetOwnerGcid() != ownerID {
		t.Errorf("OwnerGcid = %q", fake.gotReq.GetOwnerGcid())
	}

	// Mesh identity stamped on outbound metadata.
	md, ok := metadata.FromOutgoingContext(fake.gotCtx)
	if !ok {
		t.Fatal("no outgoing metadata stamped")
	}
	if got := md.Get(servicemesh.HeaderGCID); len(got) != 1 || got[0] != "g-op" {
		t.Errorf("%s = %v, want [g-op]", servicemesh.HeaderGCID, got)
	}

	// Response mapped to DTO.
	if dto.TenantID != childID || dto.HostingMode != "FRANCHISE" || dto.State != "ACTIVE" {
		t.Errorf("dto = %+v", dto)
	}
	if dto.ParentTenantID == nil || *dto.ParentTenantID != parentID {
		t.Errorf("dto.ParentTenantID = %v, want %q", dto.ParentTenantID, parentID)
	}
	if dto.CreatedAt == "" {
		t.Error("dto.CreatedAt empty")
	}
}

func TestCreateSubTenant_EmptyHostingMode_MapsUnspecified(t *testing.T) {
	fake := &fakeTenancyRPC{resp: &tenancyv1.CreateSubTenantResponse{Tenant: &tenancyv1.Tenant{TenantId: childID}}}
	c := newTenancyAdmin(t, fake)
	if _, err := c.CreateSubTenant(context.Background(), clients.Caller{GCID: "g-op"},
		clients.SubTenantParams{ParentTenantID: parentID, DisplayName: "X", HostingMode: "", OwnerGCID: ownerID}); err != nil {
		t.Fatalf("CreateSubTenant: %v", err)
	}
	if fake.gotReq.GetHostingMode() != tenancyv1.HostingMode_HOSTING_MODE_UNSPECIFIED {
		t.Errorf("HostingMode = %v, want UNSPECIFIED (server applies default)", fake.gotReq.GetHostingMode())
	}
}

func TestCreateSubTenant_BadHostingMode_Errors(t *testing.T) {
	fake := &fakeTenancyRPC{}
	c := newTenancyAdmin(t, fake)
	if _, err := c.CreateSubTenant(context.Background(), clients.Caller{GCID: "g-op"},
		clients.SubTenantParams{ParentTenantID: parentID, DisplayName: "X", HostingMode: "BOGUS", OwnerGCID: ownerID}); err == nil {
		t.Fatal("want error for unrecognised hosting_mode, got nil")
	}
	if fake.gotReq != nil {
		t.Error("rpc must not be called on a client-side validation error")
	}
}

func TestCreateSubTenant_PropagatesGRPCError(t *testing.T) {
	fake := &fakeTenancyRPC{err: status.Error(codes.FailedPrecondition, "foreign root")}
	c := newTenancyAdmin(t, fake)
	_, err := c.CreateSubTenant(context.Background(), clients.Caller{GCID: "g-op"},
		clients.SubTenantParams{ParentTenantID: parentID, DisplayName: "X", HostingMode: "FRANCHISE", OwnerGCID: ownerID})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
}

func TestNewTenancyAdminClient_NilRPC(t *testing.T) {
	if _, err := clients.NewTenancyAdminClient(clients.TenancyAdminClientConfig{RPC: nil}); !errors.Is(err, clients.ErrNilTenancyRPC) {
		t.Fatalf("err = %v, want ErrNilTenancyRPC", err)
	}
}

func TestGetTenantHierarchy_MapsRequest_StampsMesh(t *testing.T) {
	fake := &fakeTenancyRPC{hierResp: &tenancyv1.GetTenantHierarchyResponse{
		IsParent: true,
		Children: []*tenancyv1.ChildTenantSummary{
			{TenantId: childID, TenantName: "Bishan Branch", UserCount: 7, AtomCount: 0},
		},
	}}
	c := newTenancyAdmin(t, fake)

	dto, err := c.GetTenantHierarchy(context.Background(),
		clients.Caller{GCID: "g-admin", TenantID: parentID, Roles: []string{"tenant_admin"}}, parentID)
	if err != nil {
		t.Fatalf("GetTenantHierarchy: %v", err)
	}
	if fake.gotHierReq.GetTenantId() != parentID {
		t.Errorf("TenantId = %q, want %q", fake.gotHierReq.GetTenantId(), parentID)
	}
	// Mesh identity stamped on the outbound gRPC metadata.
	md, ok := metadata.FromOutgoingContext(fake.gotHierCtx)
	if !ok {
		t.Fatal("no outgoing metadata stamped")
	}
	if got := md.Get(servicemesh.HeaderGCID); len(got) != 1 || got[0] != "g-admin" {
		t.Errorf("%s = %v, want [g-admin]", servicemesh.HeaderGCID, got)
	}
	// Response mapped to DTO.
	if !dto.IsParent || len(dto.Children) != 1 || dto.Children[0].TenantID != childID ||
		dto.Children[0].TenantName != "Bishan Branch" || dto.Children[0].UserCount != 7 || dto.Children[0].AtomCount != 0 {
		t.Errorf("dto = %+v", dto)
	}
}

func TestGetTenantHierarchy_EmptyChildren_NonNilSlice(t *testing.T) {
	// No children → Children must serialise as [] (non-nil), never null.
	fake := &fakeTenancyRPC{hierResp: &tenancyv1.GetTenantHierarchyResponse{IsParent: false}}
	c := newTenancyAdmin(t, fake)
	dto, err := c.GetTenantHierarchy(context.Background(), clients.Caller{GCID: "g-admin", TenantID: parentID}, parentID)
	if err != nil {
		t.Fatalf("GetTenantHierarchy: %v", err)
	}
	if dto.Children == nil {
		t.Error("Children must be non-nil (JSON []), got nil")
	}
	if len(dto.Children) != 0 || dto.IsParent {
		t.Errorf("dto = %+v", dto)
	}
}

func TestGetTenantHierarchy_PropagatesGRPCError(t *testing.T) {
	fake := &fakeTenancyRPC{hierErr: status.Error(codes.Internal, "boom")}
	c := newTenancyAdmin(t, fake)
	_, err := c.GetTenantHierarchy(context.Background(), clients.Caller{GCID: "g-admin", TenantID: parentID}, parentID)
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}
}
