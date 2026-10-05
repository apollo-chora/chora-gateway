// bootstrap.go — H+ Setup-Tenant Phase 3 BFF aggregator wrapper
// (CHO-1632).
//
// Surfaces `POST /api/v1/tenants/bootstrap` for the H+ Setup-Tenant
// wizard (CHO-1405). Proxies the request body verbatim to chora-tenancy
// `/v1/tenants/bootstrap` with the canonical AuthCtx propagation
// (Authorization, traceparent, gcid, X-Tenant-Id, mesh metadata).
//
// Sibling stories:
//   - CHO-1628 — Phase 1, chora-tenancy bootstrap endpoint
//   - CHO-1630 — Phase 2, outbox event + chora-identity mirror
//   - CHO-1632 — Phase 3, this file (BFF route)
//   - CHO-1405 — Phase 4, H+ FE Setup-Tenant wizard
package phyllis

import (
	"context"
	"net/http"
)

// BootstrapTenant — POST /api/v1/tenants/bootstrap →
// chora-tenancy `/api/v1/tenants/bootstrap`. The caller's GCID is
// taken from the validated Chora session JWT via AuthCtx (NEVER
// trusted from the request body — chora-tenancy's handler rejects
// body-supplied GCID per the trust boundary established in CHO-1628).
//
// Upstream path note: chora-tenancy mounts the bootstrap handler at
// `/api/v1/tenants/bootstrap` (see services/chora-tenancy/cmd/server/
// main.go — `root.Handle("/api/v1/tenants/bootstrap", ...)`). The same
// `/api/` prefix is preserved for every other tenancy aggregator (see
// `GetMyTenant` → `a.cfg.TenancyURL+"/api/tenants/..."`). The early
// CHO-1632 build stripped the `/api/` prefix here and returned 404
// after deploy — this comment documents the convention so a future
// edit doesn't regress.
//
// Body shape (forwarded verbatim):
//
//	{ "name": "<3..256 char display name>" }
//
// Status codes pass through:
//   - 201 Created     — tenant + member + entitlement persisted
//   - 400 Bad Request — invalid name / missing fields
//   - 401             — JWT gate rejected upstream
//   - 409 Conflict    — caller already holds a TenantMembership
//
// Upstream 5xx normalises to 502 via classify(); upstream timeout to 504.
func (a *Aggregator) BootstrapTenant(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodPost, a.cfg.TenancyURL+"/api/v1/tenants/bootstrap", body, auth))
	}), nil
}
