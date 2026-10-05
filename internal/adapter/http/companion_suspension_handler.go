// companion_suspension_handler.go: the O+ Learning Companion containment BFF
// route (ADR-252 via ADR-254 D7; sibling of egress_killswitch_handler.go).
//
//	GET   /api/v1/admin/companion/suspension
//	PATCH /api/v1/admin/companion/suspension
//
// An operator contains the companion platform-wide, per tenant, or per skill
// with no deploy; chora-model-gateway reads the result uncached on every
// companion turn and refuses a contained turn before any debit.
//
// Same path family and same reasons as the egress kill-switch: Cloud Armor rule
// 994 carves PATCH out only under /api/v1/... (added 2026-08-22 for this path),
// and the /bff/oplus/ AuditorGate excludes platform_operator.
//
// AUTHZ, defence in depth, both ends fail closed:
//   - HERE: the VALIDATED session roles must include one of platform_operator /
//     auditor / admin / owner (mesh claims first, ChoraSession fallback); never a
//     client-supplied header. Anything else is refused before any upstream call.
//   - UPSTREAM: chora-observability re-gates on x-mesh-user-roles and enforces
//     the SCOPE matrix itself (platform = operator only; tenant = that tenant's
//     admin / owner, or the operator; auditors read only), so this proxy MUST
//     stamp the mesh headers and X-Tenant-Id on every call.
package httpadapter

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
)

// companionSuspensionUpstreamPath is the chora-observability route this proxies
// to. Enumerated verbatim in chora-observability's Istio AuthorizationPolicy
// (no wildcards: an unlisted path is a mesh 403) and in Cloud Armor rule 994.
const companionSuspensionUpstreamPath = "/api/v1/admin/companion/suspension"

// companionSuspensionRoles are the roles that may reach the control at all
// (ADR-252 D3 read matrix); the scope matrix is enforced upstream.
var companionSuspensionRoles = map[string]struct{}{
	"platform_operator": {},
	"auditor":           {},
	"admin":             {},
	"owner":             {},
}

// CompanionSuspensionHandler proxies the containment control to chora-observability.
type CompanionSuspensionHandler struct {
	observabilityURL string
	http             *http.Client
}

// NewCompanionSuspensionHandler constructs the handler. An empty
// observabilityURL leaves the route 503 rather than answering a plausible
// "nobody is suspended".
func NewCompanionSuspensionHandler(observabilityURL string, client *http.Client) *CompanionSuspensionHandler {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &CompanionSuspensionHandler{
		observabilityURL: strings.TrimRight(strings.TrimSpace(observabilityURL), "/"),
		http:             client,
	}
}

// WithCompanionSuspension composes the handler with a base handler: the single
// containment path is served here; everything else falls through. Passes
// through when h is nil (env-driven dev opt-out), mirroring WithEgressKillSwitch.
func WithCompanionSuspension(base http.Handler, h *CompanionSuspensionHandler) http.Handler {
	if h == nil {
		return base
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == companionSuspensionUpstreamPath {
			h.ServeHTTP(w, r)
			return
		}
		base.ServeHTTP(w, r)
	})
}

func (h *CompanionSuspensionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.observabilityURL == "" {
		writeError(w, http.StatusServiceUnavailable, "GATEWAY_OBSERVABILITY_UNWIRED",
			"observability upstream not configured")
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodPatch:
	default:
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET or PATCH only on "+companionSuspensionUpstreamPath)
		return
	}

	gcid, tenant := txIdentity(r)
	if gcid == "" {
		writeError(w, http.StatusUnauthorized, "GATEWAY_UNAUTHENTICATED", "gcid missing from session")
		return
	}
	// Fail CLOSED on the validated roles; the scope matrix lives upstream.
	if !hasAnyCompanionSuspensionRole(r) {
		writeError(w, http.StatusForbidden, "GATEWAY_FORBIDDEN",
			"companion containment requires platform_operator, auditor, admin or owner")
		return
	}

	resp, status, err := h.proxy(r.Context(), r, gcid, tenant)
	if err != nil {
		writeError(w, http.StatusBadGateway, "GATEWAY_UPSTREAM_ERROR", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(resp)
}

// hasAnyCompanionSuspensionRole reports whether the validated session roles
// carry one of the reading roles. Case-insensitive, '-' folded to '_', exact per
// entry (a superset name never passes).
func hasAnyCompanionSuspensionRole(r *http.Request) bool {
	for _, role := range sessionRoles(r) {
		norm := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(role), "-", "_"))
		if _, ok := companionSuspensionRoles[norm]; ok {
			return true
		}
	}
	return false
}

func (h *CompanionSuspensionHandler) proxy(ctx context.Context, r *http.Request, gcid, tenant string) ([]byte, int, error) {
	var body io.Reader
	if r.Method == http.MethodPatch {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			return nil, 0, fmt.Errorf("read body: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, h.observabilityURL+companionSuspensionUpstreamPath, body)
	if err != nil {
		return nil, 0, fmt.Errorf("build upstream request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if r.Method == http.MethodPatch {
		req.Header.Set("Content-Type", "application/json")
	}
	if tp := strings.TrimSpace(r.Header.Get("traceparent")); tp != "" {
		req.Header.Set("traceparent", tp)
	}
	// chora-observability's tenantContext middleware requires X-Tenant-Id on
	// every request, and the tenant-scope rows are RLS-keyed on it: the caller's
	// validated tenant is what a tenant admin may contain.
	if tenant != "" {
		req.Header.Set("X-Tenant-Id", tenant)
	}
	// Stamp the canonical mesh headers: the upstream re-gates on the roles
	// fail-closed, so without this every call would be denied.
	mesh := servicemesh.MarshalToHeaders(servicemesh.MeshClaims{
		GCID:     gcid,
		TenantID: tenant,
		Roles:    sessionRoles(r),
	})
	for k, vs := range mesh {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	// #nosec G704: h.observabilityURL is env-config; the path is the fixed
	// companionSuspensionUpstreamPath constant, so no user-controlled destination.
	res, err := h.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("upstream call: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	out, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, 0, fmt.Errorf("read upstream response: %w", err)
	}
	return out, res.StatusCode, nil
}
