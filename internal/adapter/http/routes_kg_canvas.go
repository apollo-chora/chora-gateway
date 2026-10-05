// routes_kg_canvas.go — APPEND-ONLY BFF route bindings for the per-user
// Knowledge-Graph hexagon-fog canvas (ADR-143; contract
// chora-contracts/openapi/learner-knowledge-graph.yaml v1.1
// E2E-BE-KG-CANVAS block).
//
// The A+ FE (chora-web kg-fog.service.ts) drives the Discovery canvas via
// 8 endpoints that chora-consumption already serves (kg_canvas_handler.go)
// but the gateway never proxied — every call 404'd at the BFF edge:
//
//	GET    /api/v1/me/knowledge-graph/clusters/{cid}/explorations/{eid}/hexagon
//	POST   /api/v1/me/knowledge-graph/clusters/{cid}/explorations/{eid}/focal:move
//	POST   /api/v1/me/knowledge-graph/clusters/{cid}/archive
//	POST   /api/v1/me/knowledge-graph/junctions/{jid}/decide
//	GET    /api/v1/me/knowledge-graph/clusters/management
//	PATCH  /api/v1/me/knowledge-graph/clusters/{cid}
//	GET    /api/v1/tenants/{tid}/knowledge-graph/config
//	PATCH  /api/v1/tenants/{tid}/knowledge-graph/config
//
// Composed as an OUTER bridge (WithKGCanvasRoutes) per the proven
// WithGatewayProxy / WithKGExploreBridge pattern — these paths are NOT in the
// legacy route table (internal/adapter/inmem/route_repository.go), and the
// inner router's authMiddleware 404s any path the table doesn't advertise.
// That same gap is why the SIBLING cluster list/create registration in
// phyllis_handler.go (mux.HandleFunc("/api/v1/me/knowledge-graph/clusters",
// ph.handleKGClusters)) was dead at the edge: mux + handler existed but the
// route table never matched, so authMiddleware returned 404
// GATEWAY_ROUTE_NOT_FOUND before dispatch. The bridge therefore ALSO claims
// the exact list/create path and dispatches to the SAME phyllis aggregator
// methods (GetKGClusters / CreateKGCluster) with the same method discipline
// — resurrecting §1.1+§1.2 without reshaping the existing registration.
//
// Identity + error conventions are the sibling's, verbatim: handlers extract
// AuthCtx via authCtxFromRequest (Bearer passthrough + X-Tenant-Id fallback +
// validated MeshClaims when RequireChoraSessionJWT ran), the phyllis
// aggregator stamps X-Tenant-Id / lowercase gcid / chora-gcid /
// chora-tenant-id / x-mesh-user-roles / traceparent on the outbound call, and
// classify passes 2xx/4xx through verbatim (incl. the 404 FOG_CACHE_MISS the
// FE retries on) while normalising 5xx → 502 and timeouts → 504.
//
// JWT gating: KGCanvasJWTGatedPrefix + KGCanvasTenantsJWTGatedPrefix are
// registered in DefaultJWTGatedPrefixes (jwt_auth.go) so
// WithChoraSessionOnPrefixes validates the Chora session JWT + stamps mesh
// claims BEFORE the bridge fires — without that, chora-consumption's
// extRequireContext 400s MISSING_CONTEXT on every call.
package httpadapter

import (
	"net/http"
	"strings"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

// KGCanvasClustersPath is the exact cluster list/create path (sibling §1.1 +
// §1.2). The canvas subtrees hang off it.
const KGCanvasClustersPath = "/api/v1/me/knowledge-graph/clusters"

// KGCanvasClustersSubtree is the per-cluster canvas subtree (hexagon /
// focal:move / archive / management / rename).
const KGCanvasClustersSubtree = KGCanvasClustersPath + "/"

// KGCanvasJunctionsSubtree is the junction-decision subtree.
const KGCanvasJunctionsSubtree = "/api/v1/me/knowledge-graph/junctions/"

// KGCanvasJWTGatedPrefix covers the whole learner-side KG canvas surface
// (clusters exact + clusters/ + junctions/ subtrees) in
// DefaultJWTGatedPrefixes.
const KGCanvasJWTGatedPrefix = "/api/v1/me/knowledge-graph"

// KGCanvasTenantsJWTGatedPrefix covers the parametric H+ tenant KG-config
// route (/api/v1/tenants/{tid}/knowledge-graph/config) in
// DefaultJWTGatedPrefixes. Prefix-gating cannot express the mid-path {tid}
// wildcard, so the whole /api/v1/tenants/ namespace is gated — every
// REGISTERED route under it (bootstrap / me / me/branding / me/addons /
// setup / me/idp-providers) was already individually gated, so the only
// observable change is 401-instead-of-404 for tokenless calls to unrouted
// paths (information hiding, consistent with the blanket /api/tenants/
// non-v1 entry).
const KGCanvasTenantsJWTGatedPrefix = "/api/v1/tenants/"

// kgCanvasTenantsPrefix is the inbound prefix the tenant-config shape matcher
// parses ({tid}/knowledge-graph/config).
const kgCanvasTenantsPrefix = "/api/v1/tenants/"

// KGCanvasHandler binds the phyllis canvas aggregator methods to the bridge.
type KGCanvasHandler struct {
	agg *phyllis.Aggregator
}

// NewKGCanvasHandler constructs the handler.
func NewKGCanvasHandler(agg *phyllis.Aggregator) *KGCanvasHandler {
	return &KGCanvasHandler{agg: agg}
}

// WithKGCanvasRoutes composes the KG canvas routes with a base handler:
// paths the canvas bridge owns are served here; everything else falls
// through to `base`. Passes through when `agg` is nil so
// cmd/server/main.go can opt out in unconfigured envs (mirrors
// WithKGExploreBridge / WithGatewayProxy).
func WithKGCanvasRoutes(base http.Handler, agg *phyllis.Aggregator) http.Handler {
	if agg == nil {
		return base
	}
	h := NewKGCanvasHandler(agg)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == KGCanvasClustersPath:
			h.handleClustersCollection(w, r)
		case strings.HasPrefix(r.URL.Path, KGCanvasClustersSubtree):
			h.handleClustersSubtree(w, r)
		case strings.HasPrefix(r.URL.Path, KGCanvasJunctionsSubtree):
			h.handleJunctionsSubtree(w, r)
		case isKGCanvasTenantConfigPath(r.URL.Path):
			h.handleTenantConfig(w, r)
		default:
			base.ServeHTTP(w, r)
		}
	})
}

// isKGCanvasTenantConfigPath reports whether p is EXACTLY the parametric
// tenant KG-config shape /api/v1/tenants/{tid}/knowledge-graph/config with a
// single non-empty {tid} segment. Anything else under /api/v1/tenants/
// (e.g. the phyllis-owned /me routes) falls through to the base router —
// the bridge claims only this one shape so the blast radius stays zero.
func isKGCanvasTenantConfigPath(p string) bool {
	rest, ok := strings.CutPrefix(p, kgCanvasTenantsPrefix)
	if !ok {
		return false
	}
	parts := strings.Split(rest, "/")
	return len(parts) == 3 && parts[0] != "" && parts[1] == "knowledge-graph" && parts[2] == "config"
}

// kgCanvasAuth extracts the phyllis AuthCtx exactly like the sibling KG
// cluster route (authCtxFromRequest), with one bridge-specific addition: the
// bridge runs OUTSIDE the inner router's traceContext middleware, so when the
// context carries no traceparent the inbound header is used directly —
// preserving W3C trace propagation parity with the mux-internal sibling.
func kgCanvasAuth(r *http.Request) phyllis.AuthCtx {
	ac := authCtxFromRequest(r)
	if ac.Traceparent == "" {
		ac.Traceparent = r.Header.Get(HeaderTraceparent)
	}
	return ac
}

// handleClustersCollection — GET (list) + POST (create) on the exact
// clusters path. Same dispatch as the phyllis handleKGClusters handler; the
// bridge resurrects it at the edge (see the package comment for why the
// mux-internal registration was dead).
func (h *KGCanvasHandler) handleClustersCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		resp, _ := h.agg.GetKGClusters(r.Context(), kgCanvasAuth(r))
		writePhyllisResp(w, resp)
	case http.MethodPost:
		resp, _ := h.agg.CreateKGCluster(r.Context(), kgCanvasAuth(r), readBody(r))
		writePhyllisResp(w, resp)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"method "+r.Method+" not allowed; want GET or POST")
	}
}

// handleClustersSubtree dispatches the per-cluster canvas routes:
//
//	GET    .../clusters/management                          (list summaries)
//	PATCH  .../clusters/{cid}                               (rename)
//	POST   .../clusters/{cid}/archive
//	GET    .../clusters/{cid}/explorations/{eid}/hexagon
//	POST   .../clusters/{cid}/explorations/{eid}/focal:move
//
// Unknown shapes 404 GATEWAY_ROUTE_NOT_FOUND (same status the edge returned
// before this bridge existed); known shapes with a wrong method 405.
func (h *KGCanvasHandler) handleClustersSubtree(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, KGCanvasClustersSubtree), "/")

	if rest == "management" {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		resp, _ := h.agg.ListKGCanvasManagement(r.Context(), kgCanvasAuth(r))
		writePhyllisResp(w, resp)
		return
	}

	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "GATEWAY_ROUTE_NOT_FOUND", "route not found")
		return
	}
	clusterID := parts[0]

	switch {
	case len(parts) == 1:
		// PATCH {cid} = canvas rename. The GET cluster-detail read is a v1.0
		// route chora-consumption serves but the canvas FE does not call —
		// it stays un-proxied (404 at the edge, as before).
		if r.Method != http.MethodPatch {
			writeError(w, http.StatusNotFound, "GATEWAY_ROUTE_NOT_FOUND", "route not found")
			return
		}
		resp, _ := h.agg.RenameKGCanvasCluster(r.Context(), kgCanvasAuth(r), clusterID, readBody(r))
		writePhyllisResp(w, resp)

	case len(parts) == 2 && parts[1] == "archive":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		resp, _ := h.agg.ArchiveKGCanvasCluster(r.Context(), kgCanvasAuth(r), clusterID, readBody(r))
		writePhyllisResp(w, resp)

	case len(parts) == 2 && parts[1] == "convert":
		// ADR-223 MapCluster→Goal projection (fog retirement). Re-projects
		// a retired fog cluster into a sovereign Goal; downstream returns
		// {data:{goalId}} the FE navigates to (/a/knowledge/{goalId}).
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		resp, _ := h.agg.ConvertKGCanvasCluster(r.Context(), kgCanvasAuth(r), clusterID, readBody(r))
		writePhyllisResp(w, resp)

	case len(parts) == 4 && parts[1] == "explorations" && parts[2] != "" && parts[3] == "hexagon":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		resp, _ := h.agg.GetKGCanvasHexagon(r.Context(), kgCanvasAuth(r), clusterID, parts[2])
		writePhyllisResp(w, resp)

	case len(parts) == 4 && parts[1] == "explorations" && parts[2] != "" && parts[3] == "focal:move":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		resp, _ := h.agg.MoveKGCanvasFocal(r.Context(), kgCanvasAuth(r), clusterID, parts[2], readBody(r))
		writePhyllisResp(w, resp)

	default:
		writeError(w, http.StatusNotFound, "GATEWAY_ROUTE_NOT_FOUND", "route not found")
	}
}

// handleJunctionsSubtree dispatches POST .../junctions/{jid}/decide. Other
// junction sub-paths (v1.0 /accept + /reject) are not canvas routes and 404
// at the edge as before.
func (h *KGCanvasHandler) handleJunctionsSubtree(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, KGCanvasJunctionsSubtree), "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "decide" {
		writeError(w, http.StatusNotFound, "GATEWAY_ROUTE_NOT_FOUND", "route not found")
		return
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	resp, _ := h.agg.DecideKGCanvasJunction(r.Context(), kgCanvasAuth(r), parts[0], readBody(r))
	writePhyllisResp(w, resp)
}

// handleTenantConfig dispatches GET + PATCH on
// /api/v1/tenants/{tid}/knowledge-graph/config (H+ ADR-143 §8 dials). The
// shape matcher guarantees the path parses; tenant isolation (path tenant ==
// header tenant) is enforced downstream, which 404s on mismatch.
func (h *KGCanvasHandler) handleTenantConfig(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, kgCanvasTenantsPrefix)
	tenantID := strings.Split(rest, "/")[0]
	switch r.Method {
	case http.MethodGet:
		resp, _ := h.agg.GetTenantKGConfig(r.Context(), kgCanvasAuth(r), tenantID)
		writePhyllisResp(w, resp)
	case http.MethodPatch:
		resp, _ := h.agg.PatchTenantKGConfig(r.Context(), kgCanvasAuth(r), tenantID, readBody(r))
		writePhyllisResp(w, resp)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"method "+r.Method+" not allowed; want GET or PATCH")
	}
}
