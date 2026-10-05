// admin_tenant_invites_test.go — TDD RED for WS3 / CHO-1873 (ADR-194 D2):
// gateway proxies for the chora-identity cold-invite admin surface.
//
//	POST   /api/v1/admin/tenant-invites              CreateTenantInvite
//	GET    /api/v1/admin/tenant-invites              ListTenantInvites
//	DELETE /api/v1/admin/tenant-invites/{inviteId}   RevokeTenantInvite
//
// Create does NOT requireTenant (the operator path carries the target tenant in
// the body; identity gates operator-vs-admin). List + revoke requireTenant.
// Backend mount paths are hard-coded per feedback_bff_aggregator_path_test.
package phyllis_test

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

func TestCreateTenantInvite_postsBodyVerbatim_noTenantRequired(t *testing.T) {
	var seenPath, seenMethod, seenBody, seenRoles string
	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath, seenMethod = r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		seenRoles = r.Header.Get("x-mesh-user-roles")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"kind":"invited","invite_id":"i"}`))
	})

	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: time.Second}, nil)
	// Operator session has NO tenant — the cold-invite must still forward.
	auth := phyllis.AuthCtx{Bearer: "phyllis", GCID: l1Gcid, TenantID: "", Roles: []string{"platform_operator"}}
	body := []byte(`{"email":"newcomer@studio.sg","tenant_id":"` + l1Tenant + `","roles":["TRAINING_ADMIN"]}`)
	res, _ := a.CreateTenantInvite(context.Background(), auth, body)

	if res.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201 (body=%s)", res.Status, res.Body)
	}
	if seenMethod != http.MethodPost || seenPath != "/api/v1/admin/tenant-invites" {
		t.Errorf("upstream = %s %s; want POST /api/v1/admin/tenant-invites", seenMethod, seenPath)
	}
	if seenBody != string(body) {
		t.Errorf("body = %q; must forward verbatim", seenBody)
	}
	if seenRoles != "platform_operator" {
		t.Errorf("x-mesh-user-roles = %q; want platform_operator", seenRoles)
	}
}

func TestCreateTenantInvite_passesThrough403And409(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusConflict} {
		identity := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":"X","message":"y"}`))
		})
		a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: time.Second}, nil)
		res, _ := a.CreateTenantInvite(context.Background(), l1Auth(), []byte(`{}`))
		if res.Status != status {
			t.Errorf("status = %d; want %d pass-through", res.Status, status)
		}
	}
}

func TestListTenantInvites_forwardsGetAndRoles(t *testing.T) {
	var seenPath, seenMethod, seenRoles, seenTenant string
	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath, seenMethod = r.URL.Path, r.Method
		seenRoles = r.Header.Get("x-mesh-user-roles")
		seenTenant = r.Header.Get("X-Tenant-Id")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[]}`))
	})

	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: time.Second}, nil)
	res, _ := a.ListTenantInvites(context.Background(), l1Auth())

	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200 (body=%s)", res.Status, res.Body)
	}
	if seenMethod != http.MethodGet || seenPath != "/api/v1/admin/tenant-invites" {
		t.Errorf("upstream = %s %s; want GET /api/v1/admin/tenant-invites", seenMethod, seenPath)
	}
	if seenTenant != l1Tenant {
		t.Errorf("X-Tenant-Id = %q; want %q", seenTenant, l1Tenant)
	}
	if seenRoles == "" {
		t.Errorf("x-mesh-user-roles must be stamped")
	}
}

func TestRevokeTenantInvite_forwardsDelete(t *testing.T) {
	var seenPath, seenMethod string
	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath, seenMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	})

	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: time.Second}, nil)
	const inviteID = "0199aaaa-0000-7000-8000-00000000bbbb"
	res, _ := a.RevokeTenantInvite(context.Background(), l1Auth(), inviteID)

	if res.Status != http.StatusNoContent {
		t.Errorf("status = %d; want 204 (body=%s)", res.Status, res.Body)
	}
	if seenMethod != http.MethodDelete || seenPath != "/api/v1/admin/tenant-invites/"+inviteID {
		t.Errorf("upstream = %s %s; want DELETE /api/v1/admin/tenant-invites/%s", seenMethod, seenPath, inviteID)
	}
}

func TestTenantInvites_missingTenant_listRevoke400_createForwards(t *testing.T) {
	identity := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: time.Second}, nil)
	auth := l1Auth()
	auth.TenantID = ""

	// List + revoke require a tenant.
	if res, _ := a.ListTenantInvites(context.Background(), auth); res.Status != http.StatusBadRequest {
		t.Errorf("list status = %d; want 400", res.Status)
	}
	if res, _ := a.RevokeTenantInvite(context.Background(), auth, "x"); res.Status != http.StatusBadRequest {
		t.Errorf("revoke status = %d; want 400", res.Status)
	}
	// Create must NOT 400 on a missing tenant (operator path carries it in the body).
	if res, _ := a.CreateTenantInvite(context.Background(), auth, []byte(`{}`)); res.Status == http.StatusBadRequest {
		t.Errorf("create status = %d; must NOT 400 on missing tenant", res.Status)
	}
}
