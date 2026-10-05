package httpadapter_test

// L1 CHO-1705 walk-caught: the gatewayproxy bridge claimed
// /api/v1/admin/tenant-members (B6.1 GET-only search) and shadowed the
// phyllis handler that serves the FULL family (GET search + POST
// add-by-email + PATCH {gcid}/role, CHO-1708) — every POST/PATCH through
// the composed router 405'd at the bridge. The family must fall through
// to the base (phyllis) for ALL methods, like the /api/tenants/me
// carve-out precedent.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

func TestWithGatewayProxy_TenantMembersFamily_FallsThroughToPhyllis(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	agg := gatewayproxy.New(gatewayproxy.Config{
		IdentityURL:    "http://identity.invalid",
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithGatewayProxy(base, agg)

	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/tenant-members"},
		{http.MethodGet, "/api/v1/admin/tenant-members?q=dale"},
		{http.MethodPost, "/api/v1/admin/tenant-members"},
		{http.MethodPatch, "/api/v1/admin/tenant-members/0197-gcid/role"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusTeapot {
			t.Errorf("%s %s status = %d; want 418 — tenant-members family must fall through to the phyllis handler", c.method, c.path, w.Code)
		}
	}
}
