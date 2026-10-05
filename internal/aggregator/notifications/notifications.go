// Package notifications proxies the BFF /api/v1/notifications/* routes
// through to the chora-notifications service so the FE in-app notifications
// feature can drive feed + mark-read from a single BFF surface.
//
// Routes mounted on the BFF (all require Bearer JWT via DefaultJWTGatedPrefixes):
//
//	GET    /api/v1/notifications                → chora-notifications GET /api/notifications
//	POST   /api/v1/notifications                → chora-notifications POST /api/notifications
//	GET    /api/v1/notifications/{id}           → chora-notifications GET /api/notifications/{id}
//	POST   /api/v1/notifications/mark-read      → chora-notifications POST /api/notifications/mark-read
//
// Wire contract:
//   - Bearer JWT enforced by chora-gateway's RequireChoraSessionJWT
//     middleware before the request reaches this aggregator. AuthCtx fields
//     are populated by the BFF handler from validated mesh claims.
//   - Outbound calls stamp Authorization, traceparent, X-Tenant-Id, plus the
//     canonical mesh metadata via servicemesh.MarshalToHeaders so the
//     downstream service sees chora-gcid / chora-tenant-id / chora-role-summary.
//   - 5xx → 502 GATEWAY_UPSTREAM_5XX. Timeout → 504 GATEWAY_UPSTREAM_TIMEOUT.
//     4xx (404, 401, 403, 422, 409) pass through verbatim.
//   - Query strings (recipient_gcid, channel, from, to) pass through unchanged.
//
// SVC_NOTIFICATIONS_URL is read at cmd/server boot per memory
// feedback_no_inline_config — never inline. The BFF handler skips registration
// when URL is empty so the route remains 404 in unconfigured envs (clearer
// signal than a 502 fan-out failure on every request).
package notifications

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

// DefaultPerCallTimeout is the upstream call deadline. Notifications fan-out
// is cheap (DB lookup + emit); 5s is generous.
const DefaultPerCallTimeout = 5 * time.Second

// Config wires the chora-notifications base URL + per-call timeout. Sourced
// from env via LoadConfigFromEnv.
type Config struct {
	// NotificationsURL is the chora-notifications service base. Empty
	// disables the aggregator (route returns 404 from the FE perspective
	// because the bridge mux does not register).
	NotificationsURL string

	// PerCallTimeout caps each downstream call. Defaults to 5s.
	PerCallTimeout time.Duration
}

// ApplyDefaults sets the default per-call timeout when zero.
func (c *Config) ApplyDefaults() {
	if c.PerCallTimeout == 0 {
		c.PerCallTimeout = DefaultPerCallTimeout
	}
}

// LoadConfigFromEnv reads SVC_NOTIFICATIONS_URL.
func LoadConfigFromEnv() Config {
	c := Config{
		NotificationsURL: os.Getenv("SVC_NOTIFICATIONS_URL"),
	}
	c.ApplyDefaults()
	return c
}

// AuthCtx carries the per-request mesh-trust + tracing values stamped on
// outbound calls. Populated by the BFF handler from validated JWT claims.
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

// Aggregator is the stateless notifications BFF proxy.
type Aggregator struct {
	cfg    Config
	client *http.Client
}

// New constructs an Aggregator. Returns nil when NotificationsURL is unset so
// callers can route-skip in unconfigured envs.
func New(cfg Config) *Aggregator {
	cfg.ApplyDefaults()
	if cfg.NotificationsURL == "" {
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

// call performs one outbound request with mesh-trust headers stamped.
func (a *Aggregator) call(ctx context.Context, method, urlStr string, body []byte, auth AuthCtx) callResult {
	if urlStr == "" {
		return callResult{err: fmt.Errorf("notifications: empty url for %s", method)}
	}
	callCtx, cancel := context.WithTimeout(ctx, a.cfg.PerCallTimeout)
	defer cancel()

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(callCtx, method, urlStr, bodyReader)
	if err != nil {
		return callResult{err: fmt.Errorf("notifications: build request: %w", err)}
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
		// chora-notifications reads gcid (lowercase) via tenantContext middleware
		// on the recipient path. Stamp it explicitly so the downstream's existing
		// header convention is preserved.
		req.Header.Set("gcid", auth.GCID)
	}
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
// 2xx + 4xx pass through verbatim.
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
// Notifications routes
// -----------------------------------------------------------------------------

// List proxies GET /api/v1/notifications?recipient_gcid=&channel=&from=&to=
// through to chora-notifications GET /api/notifications. The raw query string
// is forwarded verbatim so the downstream filter parsing stays canonical.
func (a *Aggregator) List(ctx context.Context, auth AuthCtx, rawQuery string) (Response, error) {
	u := a.cfg.NotificationsURL + "/api/notifications"
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// Enqueue proxies POST /api/v1/notifications → chora-notifications
// POST /api/notifications (enqueue + suppression-check).
func (a *Aggregator) Enqueue(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	u := a.cfg.NotificationsURL + "/api/notifications"
	return classify(a.call(ctx, http.MethodPost, u, body, auth)), nil
}

// Get proxies GET /api/v1/notifications/{id} → chora-notifications
// GET /api/notifications/{id}.
func (a *Aggregator) Get(ctx context.Context, auth AuthCtx, id string) (Response, error) {
	u := a.cfg.NotificationsURL + "/api/notifications/" + url.PathEscape(id)
	return classify(a.call(ctx, http.MethodGet, u, nil, auth)), nil
}

// MarkRead proxies POST /api/v1/notifications/mark-read →
// chora-notifications POST /api/notifications/mark-read. The downstream
// shape (e.g. {notification_ids: [...]}) is determined by the FE-driven
// contract; the bridge passes the body verbatim so the BFF stays decoupled
// from the chora-notifications mark-read schema as it evolves.
func (a *Aggregator) MarkRead(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	u := a.cfg.NotificationsURL + "/api/notifications/mark-read"
	return classify(a.call(ctx, http.MethodPost, u, body, auth)), nil
}

// PushSubscriptions proxies POST/DELETE /api/v1/notifications/push-subscriptions
// → chora-notifications /api/notifications/push-subscriptions (ADR-172 Web Push
// token register / unregister). The raw query string (DELETE carries ?token=)
// is forwarded; the POST body is passed through.
func (a *Aggregator) PushSubscriptions(ctx context.Context, auth AuthCtx, method, rawQuery string, body []byte) (Response, error) {
	u := a.cfg.NotificationsURL + "/api/notifications/push-subscriptions"
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	var reqBody []byte
	if method == http.MethodPost {
		reqBody = body
	}
	return classify(a.call(ctx, method, u, reqBody, auth)), nil
}
