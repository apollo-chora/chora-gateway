// kg_explore_handler.go — HTTP route bindings for the BFF
// GET /api/v1/consumption/kg/explore/{atom_id} proxy that fans out to
// chora-consumption.
//
// D1.4: the FE Discovery KG canvas (P1.3) consumes the per-user
// Knowledge-Graph hexagonal exploration endpoint (ADR-143 §7).
// chora-consumption owns the handler (fog_handler.go, mounted at
// /api/v1/consumption/kg/explore/) but chora-gateway did not proxy it — the
// FE call 404'd at the gateway edge. This handler closes that gap.
//
// Route mounted (requires Bearer JWT — DefaultJWTGatedPrefixes includes
// /api/v1/consumption/kg/explore/):
//
//	GET  /api/v1/consumption/kg/explore/{atom_id}  → chora-consumption
//
// The bridge passes the downstream JSON body through verbatim —
// chora-consumption already returns the ADR-143 §7 {focal_node, neighbors[]}
// shape the FE consumes directly.
package httpadapter

import (
	"net/http"
	"strings"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/kgexplore"
)

// KGExploreBridgePathPrefix is the single BFF prefix the kg/explore mux
// serves. Exposed for inclusion in DefaultJWTGatedPrefixes.
const KGExploreBridgePathPrefix = "/api/v1/consumption/kg/explore/"

// KGExploreHandler binds the kgexplore aggregator to the BFF mux.
type KGExploreHandler struct {
	agg *kgexplore.Aggregator
}

// NewKGExploreHandler constructs the handler.
func NewKGExploreHandler(agg *kgexplore.Aggregator) *KGExploreHandler {
	return &KGExploreHandler{agg: agg}
}

// NewKGExploreMux returns a mux that serves the
// /api/v1/consumption/kg/explore/* routes. Combine with WithKGExploreBridge
// to compose with a base handler that owns non-bridge paths.
func NewKGExploreMux(agg *kgexplore.Aggregator) http.Handler {
	h := NewKGExploreHandler(agg)
	mux := http.NewServeMux()
	// Subtree registration — the {atom_id} path segment is parsed by the
	// handler. Trailing-slash registration is required for net/http subtree
	// matching.
	mux.HandleFunc(KGExploreBridgePathPrefix, h.handleExplore)
	return mux
}

// WithKGExploreBridge composes a kg/explore mux with a base handler: routes
// under /api/v1/consumption/kg/explore/ are served by the bridge; everything
// else falls through to `base`. Passes through when `agg` is nil so
// cmd/server/main.go can opt out in unconfigured envs.
func WithKGExploreBridge(base http.Handler, agg *kgexplore.Aggregator) http.Handler {
	if agg == nil {
		return base
	}
	bridgeMux := NewKGExploreMux(agg)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, KGExploreBridgePathPrefix) {
			bridgeMux.ServeHTTP(w, r)
			return
		}
		base.ServeHTTP(w, r)
	})
}

// -----------------------------------------------------------------------------
// GET /api/v1/consumption/kg/explore/{atom_id}
// -----------------------------------------------------------------------------

// handleExplore dispatches GET /api/v1/consumption/kg/explore/{atom_id}.
// Non-GET methods return 405; a missing atom_id (bare prefix) returns 404.
func (h *KGExploreHandler) handleExplore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/v1/consumption/kg/explore/{atom_id}")
		return
	}
	atomID := strings.TrimSpace(
		strings.TrimSuffix(
			strings.TrimPrefix(r.URL.Path, KGExploreBridgePathPrefix),
			"/",
		),
	)
	if atomID == "" || strings.Contains(atomID, "/") {
		writeError(w, http.StatusNotFound, "GATEWAY_ATOM_ID_REQUIRED",
			"path must include the atom id: /api/v1/consumption/kg/explore/{atom_id}")
		return
	}
	resp, _ := h.agg.Explore(r.Context(), kgExploreAuthFromRequest(r), atomID)
	writeKGExploreResp(w, resp)
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// kgExploreAuthFromRequest mirrors notificationsAuthFromRequest for the
// kgexplore aggregator's AuthCtx shape. Pulls Bearer + traceparent + tenant +
// mesh-claims onto the outbound call so chora-consumption's fog handler sees
// the X-Tenant-Id + gcid context it requires.
func kgExploreAuthFromRequest(r *http.Request) kgexplore.AuthCtx {
	tp, _ := r.Context().Value(ctxKeyTraceparent).(string)
	ac := kgexplore.AuthCtx{
		Bearer:      bearerToken(r),
		Traceparent: tp,
		TenantID:    r.Header.Get("X-Tenant-Id"),
	}
	if mc, ok := MeshClaimsFromContext(r.Context()); ok {
		ac.GCID = mc.GCID
		if mc.TenantID != "" {
			ac.TenantID = mc.TenantID
		}
		ac.RoleSummary = mc.RoleSummary
		// Typed roles → x-mesh-user-roles downstream. Every Chora role gate fails
		// CLOSED on that header, so dropping it denies 100% of role-gated calls
		// (CHO-2148). Validated claims only — never a client-supplied header.
		if len(mc.Roles) > 0 {
			ac.Roles = append([]string(nil), mc.Roles...)
		}
	}
	return ac
}

// writeKGExploreResp emits the aggregator Response with canonical
// Content-Type / Cache-Control headers.
func writeKGExploreResp(w http.ResponseWriter, resp kgexplore.Response) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if resp.Status == 0 {
		resp.Status = http.StatusInternalServerError
	}
	w.WriteHeader(resp.Status)
	if len(resp.Body) > 0 {
		_, _ = w.Write(resp.Body)
	}
}

// _ keeps the servicemesh import alive so future signed-mesh-claims helpers
// added to kgExploreAuthFromRequest do not need a separate import bump
// (mirrors notifications_handler.go).
var _ = servicemesh.MarshalToHeaders
