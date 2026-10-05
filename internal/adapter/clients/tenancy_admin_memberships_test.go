// tenancy_admin_memberships_test.go, specs for the cross-tenant membership read
// that the closure ownership pre-flight depends on (E3, first-launch spec 13.7.2).
//
// ListMemberships is a directory read: "given a GCID, which tenants does it
// belong to, and with which roles". chora-tenancy answers it through the
// SECURITY DEFINER function list_memberships_by_gcid (migration 0011), which is
// the sanctioned cross-tenant path, so no new query and no RLS bypass is
// introduced here.
package clients_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	tenancyv1 "github.com/apollo-chora/chora-contracts/gen/go/chora/services/tenancy/v1"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
)

const subjectGCID = "77777777-7777-7777-8777-777777777777"

func TestListMemberships_MapsRolesAndSlug_StampsMesh(t *testing.T) {
	fake := &fakeTenancyRPC{membershipsResp: &tenancyv1.ListMembershipsByGCIDResponse{
		Memberships: []*tenancyv1.TenantMembership{
			{TenantId: parentID, TenantSlug: "northwind-academy", Roles: []string{"owner", "admin"}},
			{TenantId: childID, TenantSlug: "acme-franchise", Roles: []string{"learner"}},
		},
		DefaultTenantId: parentID,
	}}
	c := newTenancyAdmin(t, fake)

	out, err := c.ListMemberships(context.Background(),
		clients.Caller{GCID: "g-op", TenantID: parentID, Roles: []string{"platform_operator"}},
		subjectGCID, false)
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}

	// The SUBJECT of the read is the gcid argument, never the caller.
	if fake.gotMembershipsReq.GetGcid() != subjectGCID {
		t.Errorf("Gcid = %q, want %q", fake.gotMembershipsReq.GetGcid(), subjectGCID)
	}

	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2 (%+v)", len(out), out)
	}
	if out[0].TenantID != parentID || out[0].TenantSlug != "northwind-academy" {
		t.Errorf("out[0] = %+v", out[0])
	}
	if len(out[0].Roles) != 2 || out[0].Roles[0] != "owner" || out[0].Roles[1] != "admin" {
		t.Errorf("out[0].Roles = %v, want [owner admin]", out[0].Roles)
	}
	if len(out[1].Roles) != 1 || out[1].Roles[0] != "learner" {
		t.Errorf("out[1].Roles = %v, want [learner]", out[1].Roles)
	}

	md, ok := metadata.FromOutgoingContext(fake.gotMembershipsCtx)
	if !ok {
		t.Fatal("no outgoing metadata stamped")
	}
	if got := md.Get(servicemesh.HeaderGCID); len(got) != 1 || got[0] != "g-op" {
		t.Errorf("%s = %v, want [g-op]", servicemesh.HeaderGCID, got)
	}
}

// An empty subject must not reach the wire. list_memberships_by_gcid casts its
// argument to uuid, so an empty string is a server-side cast error; refusing
// here keeps the failure named and local.
func TestListMemberships_EmptyGCID_DoesNotDialRPC(t *testing.T) {
	fake := &fakeTenancyRPC{}
	c := newTenancyAdmin(t, fake)

	if _, err := c.ListMemberships(context.Background(), clients.Caller{GCID: "g-op"}, "   ", false); err == nil {
		t.Fatal("want an error for an empty subject gcid")
	}
	if fake.gotMembershipsReq != nil {
		t.Error("RPC was dialled for an empty subject gcid")
	}
}

// A transport failure must propagate. The closure pre-flight turns it into a
// 503 rather than allowing the closure, so swallowing it here would silently
// drop the guarantee.
func TestListMemberships_RPCError_Propagates(t *testing.T) {
	fake := &fakeTenancyRPC{membershipsErr: status.Error(codes.Unavailable, "tenancy down")}
	c := newTenancyAdmin(t, fake)

	_, err := c.ListMemberships(context.Background(), clients.Caller{GCID: "g-op"}, subjectGCID, false)
	if err == nil {
		t.Fatal("want the RPC error to propagate")
	}
	if got := status.Code(err); got != codes.Unavailable {
		t.Errorf("status code = %v, want Unavailable (err %v, errors.Is check %v)",
			got, err, errors.Is(err, err))
	}
}

// Zero memberships is a valid answer, not an error: a GCID with no tenants owns
// nothing and its closure must proceed.
func TestListMemberships_NoMemberships_ReturnsEmptyNotError(t *testing.T) {
	fake := &fakeTenancyRPC{membershipsResp: &tenancyv1.ListMembershipsByGCIDResponse{}}
	c := newTenancyAdmin(t, fake)

	out, err := c.ListMemberships(context.Background(), clients.Caller{GCID: "g-op"}, subjectGCID, false)
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("out = %+v, want empty", out)
	}
}
