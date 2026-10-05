// kg_canvas.go — per-user Knowledge-Graph hexagon-fog CANVAS aggregator
// methods (ADR-143; contract chora-contracts/openapi/learner-knowledge-graph.yaml
// v1.1 E2E-BE-KG-CANVAS block).
//
// APPEND-ONLY companion to phyllis.go: the sibling GetKGClusters /
// CreateKGCluster pair (§1.1+§1.2 of the kg-fog FE handoff) already proxies
// the cluster list/create; this file adds the 8 canvas + management +
// tenant-config fan-outs the A+ canvas screens (kg-fog.service.ts §1.3-§1.7,
// §5-§8) call. Every method reuses the SAME downstream-call helper (a.call),
// identity-header forwarding (Authorization bearer + X-Tenant-Id + lowercase
// gcid + canonical chora-gcid/chora-tenant-id/x-mesh-user-roles mesh metadata
// + traceparent), per-call timeout (withBudget + PerCallTimeout) and
// error-translation conventions (classify: 2xx/4xx pass through verbatim —
// incl. the 404 FOG_CACHE_MISS envelope the FE retries on — 5xx → 502
// GATEWAY_UPSTREAM_5XX, timeout → 504 GATEWAY_UPSTREAM_TIMEOUT).
//
// Downstream paths target chora-consumption's EXT mux mounts
// (services/chora-consumption/internal/adapter/http/ext_server.go →
// kg_canvas_handler.go):
//
//	GET    /v1/me/knowledge-graph/clusters/{cid}/explorations/{eid}/hexagon
//	POST   /v1/me/knowledge-graph/clusters/{cid}/explorations/{eid}/focal:move
//	POST   /v1/me/knowledge-graph/clusters/{cid}/archive
//	POST   /v1/me/knowledge-graph/clusters/{cid}/convert   (ADR-223)
//	POST   /v1/me/knowledge-graph/junctions/{jid}/decide
//	GET    /v1/me/knowledge-graph/clusters/management
//	PATCH  /v1/me/knowledge-graph/clusters/{cid}
//	GET    /v1/tenants/{tid}/knowledge-graph/config
//	PATCH  /v1/tenants/{tid}/knowledge-graph/config
//
// Path params are forwarded verbatim (url.PathEscape on the id segments so a
// malformed id can never break the upstream URL; the literal `focal:move`
// colon segment is appended UNESCAPED — escaping it to focal%3Amove would
// 404 on chora-consumption's dispatcher, which string-matches "focal:move").
package phyllis

import (
	"context"
	"net/http"
	"net/url"
)

// kgCanvasClustersBase is the chora-consumption clusters mount the canvas
// routes fan out to. Identical to the sibling GetKGClusters target plus a
// trailing path segment per route.
const kgCanvasClustersBase = "/v1/me/knowledge-graph/clusters"

// kgCanvasJunctionsBase is the chora-consumption junctions mount.
const kgCanvasJunctionsBase = "/v1/me/knowledge-graph/junctions"

// GetKGCanvasHexagon — GET /api/v1/me/knowledge-graph/clusters/{cid}/explorations/{eid}/hexagon
// → chora-consumption GET /v1/me/knowledge-graph/clusters/{cid}/explorations/{eid}/hexagon.
//
// Returns the FE-shaped {data: HexagonLayout} envelope; a downstream 404
// FOG_CACHE_MISS passes through verbatim so the FE can offer a retry (fog
// regen is triggered via the separate /api/v1/consumption/kg/explore path).
func (a *Aggregator) GetKGCanvasHexagon(ctx context.Context, auth AuthCtx, clusterID, explorationID string) (Response, error) {
	if clusterID == "" || explorationID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_KG_IDS_REQUIRED",
			"cluster id + exploration id required in path: /api/v1/me/knowledge-graph/clusters/{cid}/explorations/{eid}/hexagon"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.ConsumptionURL + kgCanvasClustersBase + "/" + url.PathEscape(clusterID) +
			"/explorations/" + url.PathEscape(explorationID) + "/hexagon"
		return classify(a.call(c, http.MethodGet, u, nil, auth))
	}), nil
}

// MoveKGCanvasFocal — POST /api/v1/me/knowledge-graph/clusters/{cid}/explorations/{eid}/focal:move
// → chora-consumption POST .../focal:move (click-to-graduate). Body
// ({"targetAtomId": ...}) is forwarded verbatim; the colon segment is a
// literal — never path-escaped.
func (a *Aggregator) MoveKGCanvasFocal(ctx context.Context, auth AuthCtx, clusterID, explorationID string, body []byte) (Response, error) {
	if clusterID == "" || explorationID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_KG_IDS_REQUIRED",
			"cluster id + exploration id required in path: /api/v1/me/knowledge-graph/clusters/{cid}/explorations/{eid}/focal:move"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.ConsumptionURL + kgCanvasClustersBase + "/" + url.PathEscape(clusterID) +
			"/explorations/" + url.PathEscape(explorationID) + "/focal:move"
		return classify(a.call(c, http.MethodPost, u, body, auth))
	}), nil
}

// ArchiveKGCanvasCluster — POST /api/v1/me/knowledge-graph/clusters/{cid}/archive
// → chora-consumption POST /v1/me/knowledge-graph/clusters/{cid}/archive.
// 204 on success, idempotent on re-archive (downstream semantics pass through).
func (a *Aggregator) ArchiveKGCanvasCluster(ctx context.Context, auth AuthCtx, clusterID string, body []byte) (Response, error) {
	if clusterID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_KG_CLUSTER_ID_REQUIRED",
			"cluster id required in path: /api/v1/me/knowledge-graph/clusters/{cid}/archive"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.ConsumptionURL + kgCanvasClustersBase + "/" + url.PathEscape(clusterID) + "/archive"
		return classify(a.call(c, http.MethodPost, u, body, auth))
	}), nil
}

// ConvertKGCanvasCluster — POST /api/v1/me/knowledge-graph/clusters/{cid}/convert
// → chora-consumption POST /v1/me/knowledge-graph/clusters/{cid}/convert
// (ADR-223 MapCluster→Goal projection / fog retirement). Re-projects a retired
// fog cluster into a sovereign Goal; the downstream {data:{goalId}} envelope
// passes through so the FE can navigate to /a/knowledge/{goalId}. Downstream
// 404 (not-owned/not-found) + 409 (already merged) pass through verbatim.
func (a *Aggregator) ConvertKGCanvasCluster(ctx context.Context, auth AuthCtx, clusterID string, body []byte) (Response, error) {
	if clusterID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_KG_CLUSTER_ID_REQUIRED",
			"cluster id required in path: /api/v1/me/knowledge-graph/clusters/{cid}/convert"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.ConsumptionURL + kgCanvasClustersBase + "/" + url.PathEscape(clusterID) + "/convert"
		return classify(a.call(c, http.MethodPost, u, body, auth))
	}), nil
}

// DecideKGCanvasJunction — POST /api/v1/me/knowledge-graph/junctions/{jid}/decide
// → chora-consumption POST /v1/me/knowledge-graph/junctions/{jid}/decide.
// Body carries {"decision": "accept"|"decline"}; on accept the surviving
// cluster's HexagonLayout envelope passes through.
func (a *Aggregator) DecideKGCanvasJunction(ctx context.Context, auth AuthCtx, junctionID string, body []byte) (Response, error) {
	if junctionID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_KG_JUNCTION_ID_REQUIRED",
			"junction id required in path: /api/v1/me/knowledge-graph/junctions/{jid}/decide"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.ConsumptionURL + kgCanvasJunctionsBase + "/" + url.PathEscape(junctionID) + "/decide"
		return classify(a.call(c, http.MethodPost, u, body, auth))
	}), nil
}

// ListKGCanvasManagement — GET /api/v1/me/knowledge-graph/clusters/management
// → chora-consumption GET /v1/me/knowledge-graph/clusters/management.
// One ClusterManagementSummary per ACTIVE cluster for the caller.
func (a *Aggregator) ListKGCanvasManagement(ctx context.Context, auth AuthCtx) (Response, error) {
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.ConsumptionURL + kgCanvasClustersBase + "/management"
		return classify(a.call(c, http.MethodGet, u, nil, auth))
	}), nil
}

// RenameKGCanvasCluster — PATCH /api/v1/me/knowledge-graph/clusters/{cid}
// → chora-consumption PATCH /v1/me/knowledge-graph/clusters/{cid}.
// Body carries {"displayName": ...}; 204 on success.
func (a *Aggregator) RenameKGCanvasCluster(ctx context.Context, auth AuthCtx, clusterID string, body []byte) (Response, error) {
	if clusterID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_KG_CLUSTER_ID_REQUIRED",
			"cluster id required in path: /api/v1/me/knowledge-graph/clusters/{cid}"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.ConsumptionURL + kgCanvasClustersBase + "/" + url.PathEscape(clusterID)
		return classify(a.call(c, http.MethodPatch, u, body, auth))
	}), nil
}

// GetTenantKGConfig — GET /api/v1/tenants/{tid}/knowledge-graph/config
// → chora-consumption GET /v1/tenants/{tid}/knowledge-graph/config.
// H+ admin read of the two ADR-143 §8 dials (maxConcurrentKgClustersPerUser
// + kgFogInvalidationGraceSeconds). The downstream 404s when the path tenant
// does not match the caller's header tenant (no cross-tenant info leak).
func (a *Aggregator) GetTenantKGConfig(ctx context.Context, auth AuthCtx, tenantID string) (Response, error) {
	if tenantID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_TENANT_ID_REQUIRED",
			"tenant id required in path: /api/v1/tenants/{tid}/knowledge-graph/config"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.ConsumptionURL + "/v1/tenants/" + url.PathEscape(tenantID) + "/knowledge-graph/config"
		return classify(a.call(c, http.MethodGet, u, nil, auth))
	}), nil
}

// PatchTenantKGConfig — PATCH /api/v1/tenants/{tid}/knowledge-graph/config
// → chora-consumption PATCH /v1/tenants/{tid}/knowledge-graph/config.
// Bounds-checking (422 OUT_OF_BOUNDS) happens downstream and passes through.
func (a *Aggregator) PatchTenantKGConfig(ctx context.Context, auth AuthCtx, tenantID string, body []byte) (Response, error) {
	if tenantID == "" {
		return errResp(http.StatusNotFound, "GATEWAY_TENANT_ID_REQUIRED",
			"tenant id required in path: /api/v1/tenants/{tid}/knowledge-graph/config"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.ConsumptionURL + "/v1/tenants/" + url.PathEscape(tenantID) + "/knowledge-graph/config"
		return classify(a.call(c, http.MethodPatch, u, body, auth))
	}), nil
}
