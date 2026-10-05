// ownership_test.go: the aggregator's own guards for the ownership handover
// (UX Track U, E3 slice 6).
//
// The three me-wrappers refuse an empty tenant before dialling. That arm is
// unreachable through the BFF routes, because requireActiveTenant answers first,
// and it is here anyway: the guard exists so a future caller that mounts these
// aggregators without that wrapper fails loudly instead of proxying an
// unscoped request. A guard nothing exercises is a guard nobody knows is
// broken, so it gets its own specs.
package phyllis_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

const (
	ownAggTenant  = "aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaaa"
	ownAggGCID    = "bbbbbbbb-bbbb-7bbb-8bbb-bbbbbbbbbbbb"
	ownAggOffer   = "dddddddd-dddd-7ddd-8ddd-dddddddddddd"
	ownAggTenant2 = "eeeeeeee-eeee-7eee-8eee-eeeeeeeeeeee"
)

func ownAgg(t *testing.T) (*phyllis.Aggregator, *string) {
	t.Helper()
	var lastPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// RequestURI is the RAW wire form. r.URL.Path is decoded, so %2F
		// reads back as "/" there and an escaped segment is indistinguishable
		// from an unescaped one: asserting on it would pass either way.
		lastPath = r.RequestURI
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return phyllis.New(phyllis.Config{TenancyURL: srv.URL}, nil), &lastPath
}

func TestOwnershipAggregator_MeWrappersRefuseAnEmptyTenant(t *testing.T) {
	agg, lastPath := ownAgg(t)
	auth := phyllis.AuthCtx{GCID: ownAggGCID} // no TenantID

	for name, call := range map[string]func() (phyllis.Response, error){
		"create": func() (phyllis.Response, error) {
			return agg.CreateMeOwnershipOffer(context.Background(), auth, []byte(`{}`))
		},
		"get": func() (phyllis.Response, error) {
			return agg.GetMeOwnershipOffer(context.Background(), auth)
		},
		"settle": func() (phyllis.Response, error) {
			return agg.SettleMeOwnershipOffer(context.Background(), auth, ownAggOffer, "accept")
		},
	} {
		*lastPath = ""
		resp, err := call()
		if err != nil {
			t.Errorf("%s: unexpected error %v", name, err)
			continue
		}
		if resp.Status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, resp.Status)
		}
		if *lastPath != "" {
			t.Errorf("%s: dialled %q with no tenant in the auth context", name, *lastPath)
		}
	}
}

// The override takes its tenant from the path, so an empty auth tenant is
// NORMAL there: the operator is tenant-less by design and refusing would make
// the route unusable by the only role allowed to call it.
func TestOwnershipAggregator_OverrideDialsWithNoTenantInTheAuthContext(t *testing.T) {
	agg, lastPath := ownAgg(t)

	resp, err := agg.AssignTenantOwner(context.Background(),
		phyllis.AuthCtx{GCID: ownAggGCID, Roles: []string{"platform_operator"}},
		ownAggTenant2, []byte(`{"to_gcid":"x","reason":"r"}`))
	if err != nil {
		t.Fatalf("AssignTenantOwner: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	want := "/api/v1/admin/tenants/" + ownAggTenant2 + "/ownership/offers"
	if *lastPath != want {
		t.Errorf("dialled %q, want %q", *lastPath, want)
	}
}

// Path segments are escaped, so a segment that somehow arrived unvalidated
// cannot walk out of the offers subtree.
func TestOwnershipAggregator_EscapesPathSegments(t *testing.T) {
	agg, lastPath := ownAgg(t)

	if _, err := agg.SettleMeOwnershipOffer(context.Background(),
		phyllis.AuthCtx{GCID: ownAggGCID, TenantID: ownAggTenant},
		"../../../admin/tenants", "accept"); err != nil {
		t.Fatalf("SettleMeOwnershipOffer: %v", err)
	}
	want := "/api/v1/tenants/me/ownership/offers/..%2F..%2F..%2Fadmin%2Ftenants/accept"
	if *lastPath != want {
		t.Errorf("wire path = %q, want the escaped form %q", *lastPath, want)
	}
}
