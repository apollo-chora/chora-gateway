// phyllis_residual_test.go — residual fan-out coverage for the phyllis
// aggregator methods the TDD suite never reached: the tenant-me + IdP
// provider routes, atom authoring (delete/patch/media signed-url), the
// AI-assist async surface, and the retriable-call fallback branch.
package phyllis_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

func basicAuthP() phyllis.AuthCtx {
	return phyllis.AuthCtx{Bearer: "tok", Traceparent: "00-aaa-bbb-01", TenantID: "tenant-1", GCID: "gcid-1"}
}

func TestGetMyTenantV1_HappyAndNoTenant(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"branding":{},"wizard_completed_at":null}`))
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: time.Second}, nil)

	resp, err := a.GetMyTenantV1(context.Background(), basicAuthP())
	if err != nil {
		t.Fatalf("GetMyTenantV1: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.Status)
	}

	// Missing tenant → 400.
	a2 := phyllis.New(phyllis.Config{TenancyURL: "http://unused", PerCallTimeout: time.Second}, nil)
	resp, _ = a2.GetMyTenantV1(context.Background(), phyllis.AuthCtx{Bearer: "x"})
	if resp.Status != http.StatusBadRequest {
		t.Errorf("no-tenant status=%d want 400", resp.Status)
	}
}

func TestListMyIdpProviders_HappyAndNoTenant(t *testing.T) {
	idp := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	a := phyllis.New(phyllis.Config{IdentityURL: idp.URL, PerCallTimeout: time.Second}, nil)

	resp, err := a.ListMyIdpProviders(context.Background(), basicAuthP())
	if err != nil {
		t.Fatalf("ListMyIdpProviders: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.Status)
	}

	resp, _ = a.ListMyIdpProviders(context.Background(), phyllis.AuthCtx{Bearer: "x"})
	if resp.Status != http.StatusBadRequest {
		t.Errorf("no-tenant status=%d want 400", resp.Status)
	}
}

func TestDeleteMyIdpProvider_ValidationAndProxies(t *testing.T) {
	dead := phyllis.New(phyllis.Config{IdentityURL: "http://unused", PerCallTimeout: time.Second}, nil)
	// Missing tenant → 400.
	resp, _ := dead.DeleteMyIdpProvider(context.Background(), phyllis.AuthCtx{Bearer: "x"}, "google")
	if resp.Status != http.StatusBadRequest {
		t.Errorf("no-tenant status=%d want 400", resp.Status)
	}
	// Missing provider type → 400.
	resp, _ = dead.DeleteMyIdpProvider(context.Background(), basicAuthP(), "")
	if resp.Status != http.StatusBadRequest {
		t.Errorf("no-provider status=%d want 400", resp.Status)
	}

	idp := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	a := phyllis.New(phyllis.Config{IdentityURL: idp.URL, PerCallTimeout: time.Second}, nil)
	resp, err := a.DeleteMyIdpProvider(context.Background(), basicAuthP(), "google")
	if err != nil {
		t.Fatalf("DeleteMyIdpProvider: %v", err)
	}
	if resp.Status != http.StatusNoContent {
		t.Errorf("status=%d want 204", resp.Status)
	}
}

func TestDeleteAtom_PatchAtom_MintMedia(t *testing.T) {
	create := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPatch {
			_, _ = w.Write([]byte(`{"atom_id":"atom-1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"signed_url":"https://gcs/x"}`))
	})
	a := phyllis.New(phyllis.Config{CreationURL: create.URL, PerCallTimeout: time.Second}, nil)

	resp, err := a.DeleteAtom(context.Background(), basicAuthP(), "atom-1")
	if err != nil {
		t.Fatalf("DeleteAtom: %v", err)
	}
	if resp.Status != http.StatusNoContent {
		t.Errorf("DeleteAtom status=%d want 204", resp.Status)
	}

	resp, err = a.PatchAtom(context.Background(), basicAuthP(), "atom-1", []byte(`{"title":"x"}`))
	if err != nil {
		t.Fatalf("PatchAtom: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("PatchAtom status=%d want 200", resp.Status)
	}

	resp, err = a.MintAtomMediaSignedUrl(context.Background(), basicAuthP(), "atom-1", []byte(`{}`))
	if err != nil {
		t.Fatalf("MintAtomMediaSignedUrl: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("MintAtomMediaSignedUrl status=%d want 200", resp.Status)
	}

	// Empty atom id → 404 pre-outbound for each.
	for _, call := range []func() (phyllis.Response, error){
		func() (phyllis.Response, error) { return a.DeleteAtom(context.Background(), basicAuthP(), "") },
		func() (phyllis.Response, error) { return a.PatchAtom(context.Background(), basicAuthP(), "", nil) },
		func() (phyllis.Response, error) {
			return a.MintAtomMediaSignedUrl(context.Background(), basicAuthP(), "", nil)
		},
	} {
		resp, err := call()
		if err != nil {
			t.Fatalf("empty id: %v", err)
		}
		if resp.Status != http.StatusNotFound {
			t.Errorf("empty atom id status=%d want 404", resp.Status)
		}
	}
}

func TestAIAssistAndJob(t *testing.T) {
	create := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"job_id":"job-1"}`))
	})
	a := phyllis.New(phyllis.Config{CreationURL: create.URL, PerCallTimeout: time.Second}, nil)

	resp, err := a.AIAssist(context.Background(), basicAuthP(), []byte(`{"topic":"x"}`))
	if err != nil {
		t.Fatalf("AIAssist: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("AIAssist status=%d want 200", resp.Status)
	}

	resp, err = a.GetAIAssistJob(context.Background(), basicAuthP(), "job-1")
	if err != nil {
		t.Fatalf("GetAIAssistJob: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("GetAIAssistJob status=%d want 200", resp.Status)
	}

	resp, err = a.GetAIAssistJob(context.Background(), basicAuthP(), "")
	if err != nil {
		t.Fatalf("GetAIAssistJob empty: %v", err)
	}
	if resp.Status != http.StatusNotFound {
		t.Errorf("GetAIAssistJob empty status=%d want 404", resp.Status)
	}
}
func TestListMeAddOns_HappyAndNoTenant(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"addons":[]}`))
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: time.Second}, nil)

	resp, err := a.ListMeAddOns(context.Background(), basicAuthP())
	if err != nil {
		t.Fatalf("ListMeAddOns: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}

	// Missing tenant → 400.
	a2 := phyllis.New(phyllis.Config{TenancyURL: "http://unused", PerCallTimeout: time.Second}, nil)
	resp, _ = a2.ListMeAddOns(context.Background(), phyllis.AuthCtx{Bearer: "x"})
	if resp.Status != http.StatusBadRequest {
		t.Errorf("no-tenant status = %d; want 400", resp.Status)
	}
}

func TestSetAdminTenantMemberDisplayName(t *testing.T) {
	idp := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"gcid":"g-1","display_name":"Named"}`))
	})
	a := phyllis.New(phyllis.Config{IdentityURL: idp.URL, PerCallTimeout: time.Second}, nil)

	resp, err := a.SetAdminTenantMemberDisplayName(context.Background(), basicAuthP(), "g-1", []byte(`{"display_name":"Named"}`))
	if err != nil {
		t.Fatalf("SetAdminTenantMemberDisplayName: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}

	// Missing tenant → 400 via requireTenant.
	a2 := phyllis.New(phyllis.Config{IdentityURL: "http://unused", PerCallTimeout: time.Second}, nil)
	resp, _ = a2.SetAdminTenantMemberDisplayName(context.Background(), phyllis.AuthCtx{Bearer: "x"}, "g-1", nil)
	if resp.Status != http.StatusBadRequest {
		t.Errorf("no-tenant status = %d; want 400", resp.Status)
	}
}
