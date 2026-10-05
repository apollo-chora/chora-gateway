// Package kgexplore proxies the BFF GET /api/v1/consumption/kg/explore/{atom_id}
// route through to the chora-consumption service so the FE Discovery KG
// canvas (P1.3) can drive per-user Knowledge-Graph hexagonal exploration
// (ADR-143 §7) from the gateway edge.
//
// D1.4 scope: chora-consumption owns the handler
// (services/chora-consumption/internal/adapter/http/fog_handler.go, mounted
// at /api/v1/consumption/kg/explore/) but chora-gateway did not proxy it —
// the FE call 404'd at the gateway edge. This aggregator closes that gap.
//
// Route mounted on the BFF (JWT-gated via DefaultJWTGatedPrefixes):
//
//	GET  /api/v1/consumption/kg/explore/{atom_id}
//	     → chora-consumption GET /api/v1/consumption/kg/explore/{atom_id}
//
// Response shape (passed through verbatim — chora-consumption already emits
// the ADR-143 §7 envelope the FE consumes):
//
//	{"focal_node": "<atom_id>",
//	 "neighbors": [{"atom_id","relation","confidence","fog_label","is_junction"}, ...]}
//
// Wire contract:
//   - Bearer JWT enforced by chora-gateway's RequireChoraSessionJWT
//     middleware before the request reaches this aggregator. AuthCtx fields
//     are populated by the BFF handler from validated mesh claims.
//   - Outbound calls stamp Authorization, traceparent, X-Tenant-Id, the
//     lowercase `gcid` header (chora-consumption's extRequireContext reads
//     X-Tenant-Id + lowercase gcid — both are mandatory or the handler
//     400s MISSING_CONTEXT), plus the canonical mesh metadata via
//     servicemesh.MarshalToHeaders (chora-gcid / chora-tenant-id /
//     chora-role-summary) for services on the mesh-claims path.
//   - 5xx → 502 GATEWAY_UPSTREAM_5XX. Timeout → 504 GATEWAY_UPSTREAM_TIMEOUT.
//     2xx + 4xx pass through verbatim — the kg/explore endpoint is backed by
//     the fog_orchestrator engine which a concurrent track is still fixing;
//     a downstream 4xx (e.g. FOG_VALIDATE_FAILED) proves the request reached
//     chora-consumption's handler and must surface unchanged.
//
// SVC_CONSUMPTION_URL is read at cmd/server boot per memory
// feedback_no_inline_config — never inline. The BFF handler skips
// registration when the URL is empty so the route remains 404 in
// unconfigured envs (clearer signal than a 502 fan-out failure on every
// request).
package kgexplore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
)

// ExplorePathPrefix is the chora-consumption mount the BFF forwards to. It
// is identical to the BFF-facing prefix — the gateway is a transparent proxy
// for this route (no path rewrite).
const ExplorePathPrefix = "/api/v1/consumption/kg/explore/"

// DefaultPerCallTimeout caps the downstream call. The kg/explore handler
// invokes the fog_orchestrator P8 LangGraph reasoning engine on Vertex AI;
// warm calls land in ~2-3s but a cold Vertex AI engine takes 10-14s on
// first-request latency. The BFF deadline covers worst-case cold-call plus
// buffer — engine warmup (M14) will bring this back down.
const DefaultPerCallTimeout = 30 * time.Second

// Config wires the chora-consumption base URL + per-call timeout. Sourced
// from env via LoadConfigFromEnv per memory feedback_no_inline_config.
type Config struct {
	// ConsumptionURL is the chora-consumption service base. Empty disables
	// the aggregator (route returns 404 from the FE perspective because the
	// bridge mux does not register).
	ConsumptionURL string

	// PerCallTimeout caps the downstream call. Defaults to DefaultPerCallTimeout.
	PerCallTimeout time.Duration
}

// ApplyDefaults sets the default per-call timeout when zero.
func (c *Config) ApplyDefaults() {
	if c.PerCallTimeout == 0 {
		c.PerCallTimeout = DefaultPerCallTimeout
	}
}

// LoadConfigFromEnv reads SVC_CONSUMPTION_URL per feedback_no_inline_config.
func LoadConfigFromEnv() Config {
	c := Config{
		ConsumptionURL: os.Getenv("SVC_CONSUMPTION_URL"),
	}
	c.ApplyDefaults()
	return c
}

// AuthCtx carries the per-request mesh-trust + tracing values stamped on
// the outbound call. Populated by the BFF handler from validated JWT claims.
type AuthCtx struct {
	Bearer      string
	Traceparent string
	TenantID    string
	GCID        string
	RoleSummary map[string]any
	// Roles is the typed role list from the VALIDATED session / mesh claims —
	// never a client-supplied header. It becomes the `x-mesh-user-roles` header.
	// Every Chora role gate reads it and FAILS CLOSED (fail-open was deleted in
	// CHO-2072), so an AuthCtx without Roles is denied 100% of the time by any
	// role-gated downstream. Guarded by upstream/mesh_roles_guard_test.go.
	Roles []string
}

// Response is the normalised aggregator output forwarded to the FE.
type Response struct {
	Status  int
	Headers http.Header
	Body    []byte
}

// Aggregator is the stateless kg/explore BFF proxy.
type Aggregator struct {
	cfg    Config
	client *http.Client
}

// New constructs an Aggregator. Returns nil when ConsumptionURL is unset so
// callers can route-skip in unconfigured envs.
func New(cfg Config) *Aggregator {
	cfg.ApplyDefaults()
	if cfg.ConsumptionURL == "" {
		return nil
	}
	return &Aggregator{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.PerCallTimeout + 1*time.Second},
	}
}

// -----------------------------------------------------------------------------
// HTTP plumbing
// -----------------------------------------------------------------------------

type callResult struct {
	status int
	body   []byte
	header http.Header
	err    error
}

// call performs one outbound request with mesh-trust headers stamped,
// honouring PerCallTimeout.
func (a *Aggregator) call(ctx context.Context, method, urlStr string, body []byte, auth AuthCtx) callResult {
	if urlStr == "" {
		return callResult{err: fmt.Errorf("kgexplore: empty url for %s", method)}
	}
	callCtx, cancel := context.WithTimeout(ctx, a.cfg.PerCallTimeout)
	defer cancel()

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(callCtx, method, urlStr, bodyReader)
	if err != nil {
		return callResult{err: fmt.Errorf("kgexplore: build request: %w", err)}
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+auth.Bearer)
	}
	if auth.Traceparent != "" {
		req.Header.Set("traceparent", auth.Traceparent)
	}
	if auth.TenantID != "" {
		req.Header.Set("X-Tenant-Id", auth.TenantID)
	}
	if auth.GCID != "" {
		req.Header.Set("X-Chora-GCID", auth.GCID)
		// chora-consumption fog_handler.extRequireContext reads the lowercase
		// `gcid` header (NOT chora-gcid). Stamp it explicitly so the
		// downstream's existing header convention is preserved — without it
		// the fog handler 400s MISSING_CONTEXT.
		req.Header.Set("gcid", auth.GCID)
	}
	// Canonical mesh-trust headers (chora-gcid / chora-tenant-id /
	// chora-role-summary) for services on the mesh-claims path.
	mesh := servicemesh.MarshalToHeaders(servicemesh.MeshClaims{
		GCID:        auth.GCID,
		TenantID:    auth.TenantID,
		Roles:       auth.Roles,
		RoleSummary: auth.RoleSummary,
	})
	for k, vs := range mesh {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return callResult{err: err}
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return callResult{status: resp.StatusCode, body: out, header: resp.Header}
}

// classify converts a callResult into a Response. 5xx → 502; timeout → 504;
// 2xx + 4xx pass through verbatim (a downstream handler 4xx proves the
// request reached chora-consumption's fog handler).
func classify(cr callResult) Response {
	if cr.err != nil {
		if errors.Is(cr.err, context.DeadlineExceeded) {
			return errResp(http.StatusGatewayTimeout, "GATEWAY_UPSTREAM_TIMEOUT", cr.err.Error())
		}
		if errors.Is(cr.err, context.Canceled) {
			return errResp(http.StatusGatewayTimeout, "GATEWAY_REQUEST_CANCELED", cr.err.Error())
		}
		var urlErr *url.Error
		if errors.As(cr.err, &urlErr) && urlErr.Timeout() {
			return errResp(http.StatusGatewayTimeout, "GATEWAY_UPSTREAM_TIMEOUT", cr.err.Error())
		}
		return errResp(http.StatusBadGateway, "GATEWAY_UPSTREAM_ERROR", cr.err.Error())
	}
	if cr.status >= 500 {
		return errResp(http.StatusBadGateway, "GATEWAY_UPSTREAM_5XX",
			fmt.Sprintf("upstream returned %d", cr.status))
	}
	return Response{Status: cr.status, Body: cr.body, Headers: cr.header}
}

func errResp(status int, code, message string) Response {
	body := fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, code, message)
	return Response{Status: status, Body: []byte(body)}
}

// -----------------------------------------------------------------------------
// kg/explore route
// -----------------------------------------------------------------------------

// Explore proxies GET /api/v1/consumption/kg/explore/{atom_id} through to
// chora-consumption's identical path. atomID is path-escaped so a malformed
// id cannot break the upstream URL. An empty atomID short-circuits with a
// 400 (defensive — the BFF handler guards too, but the aggregator must never
// build a bad URL).
func (a *Aggregator) Explore(ctx context.Context, auth AuthCtx, atomID string) (Response, error) {
	if atomID == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_ATOM_ID_REQUIRED",
			"atom_id required in path: /api/v1/consumption/kg/explore/{atom_id}"), nil
	}
	u := a.cfg.ConsumptionURL + ExplorePathPrefix + url.PathEscape(atomID)
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}
