// egress_killswitch_handler.go — the O+ platform egress kill-switch BFF route
// (CHO-2148; ADR-231 D6).
//
//	GET   /api/v1/admin/egress/kill-switch
//	PATCH /api/v1/admin/egress/kill-switch
//
// Engaging the switch makes chora-model-gateway deny EVERY grounded web-egress
// call, for every tenant, with no deploy. Operator-only.
//
// WHY THIS PATH, NOT /bff/oplus/… — two independent reasons, both load-bearing:
//
//  1. Cloud Armor. the preconfigured methodenforcement WAF 403s "unusual"
//     methods (PATCH/PUT/DELETE) at the edge; every such route in this repo is
//     carved out by the rule-994 regex, which matches only `/api/v1/...` paths.
//     There is no `/bff/...` carve-out at all, and the policy is at its 20-rule
//     cap with rule 994 at its 5-expression cap — so a PATCH under /bff/oplus/
//     would be edge-403'd with no room to fix it. An edge 403 surfaces in the
//     browser as an opaque CORS failure, so this would also be miserable to
//     diagnose.
//
//  2. AuditorGate. middleware.OPlusAuthorizedRoles is {auditor, admin, owner} —
//     it does NOT include platform_operator. A pure operator hitting /bff/oplus/
//     would be 403'd by that gate BEFORE reaching any handler.
//
// The O+ surface still owns the UI; only the BFF path namespace differs.
//
// AUTHZ — defence in depth, both ends fail closed:
//   - HERE: hasPlatformOperatorRole on the VALIDATED session claims (mesh claims
//     first, ChoraSession as fallback) — never a client-supplied header.
//   - UPSTREAM: chora-observability re-gates on x-mesh-user-roles, fail-closed.
//
// That upstream gate is why this handler MUST stamp the mesh headers itself.
// ObservabilityClient does not (its AuthCtx has no Roles field at all, and
// httpGetJSON stamps only Accept/traceparent/X-Tenant-Id/Authorization), so
// proxying through it would send NO roles header and the upstream — correctly
// failing closed — would deny every call. Phyllis carries a comment about
// exactly this bug biting once already (CHO-1708).
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

// egressKillSwitchUpstreamPath is the chora-observability route this proxies to.
// It is enumerated verbatim in chora-observability's Istio AuthorizationPolicy
// (that policy has NO wildcards — an unlisted path is a mesh 403).
const egressKillSwitchUpstreamPath = "/api/v1/admin/egress/kill-switch"

// EgressKillSwitchHandler proxies the O+ platform egress kill-switch to
// chora-observability.
type EgressKillSwitchHandler struct {
	// observabilityURL is the upstream base URL, sourced from env by the caller
	// (no inline config — feedback_no_inline_config).
	observabilityURL string
	http             *http.Client
}

// NewEgressKillSwitchHandler constructs the handler. An empty observabilityURL
// leaves the route 503 rather than silently answering "not engaged" — a stubbed
// answer here is indistinguishable from a real one and would hide the fact that
// the platform has no working override.
func NewEgressKillSwitchHandler(observabilityURL string, client *http.Client) *EgressKillSwitchHandler {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &EgressKillSwitchHandler{
		observabilityURL: strings.TrimRight(strings.TrimSpace(observabilityURL), "/"),
		http:             client,
	}
}

// WithEgressKillSwitch composes the handler with a base handler: the single
// kill-switch path is served here; everything else falls through. Passes through
// when h is nil (env-driven dev opt-out) — mirrors WithTenancyAdmin.
func WithEgressKillSwitch(base http.Handler, h *EgressKillSwitchHandler) http.Handler {
	if h == nil {
		return base
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == egressKillSwitchUpstreamPath {
			h.ServeHTTP(w, r)
			return
		}
		base.ServeHTTP(w, r)
	})
}

func (h *EgressKillSwitchHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.observabilityURL == "" {
		writeError(w, http.StatusServiceUnavailable, "GATEWAY_OBSERVABILITY_UNWIRED",
			"observability upstream not configured")
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodPatch:
	default:
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET or PATCH only on /api/v1/admin/egress/kill-switch")
		return
	}

	gcid, tenant := txIdentity(r)
	if gcid == "" {
		writeError(w, http.StatusUnauthorized, "GATEWAY_UNAUTHENTICATED",
			"gcid missing from session")
		return
	}

	// Fail CLOSED. The kill-switch disables web egress for every tenant on the
	// platform — it is operator-only, and an absent or unrecognised role denies.
	if !hasPlatformOperatorRole(r) {
		writeError(w, http.StatusForbidden, "GATEWAY_FORBIDDEN",
			"the platform egress kill-switch requires the platform_operator role")
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

func (h *EgressKillSwitchHandler) proxy(ctx context.Context, r *http.Request, gcid, tenant string) ([]byte, int, error) {
	var body io.Reader
	if r.Method == http.MethodPatch {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			return nil, 0, fmt.Errorf("read body: %w", err)
		}
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, r.Method, h.observabilityURL+egressKillSwitchUpstreamPath, body)
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

	// chora-observability's tenantContext middleware requires X-Tenant-Id on every
	// request (it predates this route and gates the whole service). The kill-switch
	// itself is platform-global — the singleton row has no tenant and no RLS — but
	// the middleware still demands the header, so stamp the operator's own tenant
	// to satisfy it. Without this the upstream answers OBS_TENANT_REQUIRED (400)
	// and the switch is unreachable.
	if tenant != "" {
		req.Header.Set("X-Tenant-Id", tenant)
	}

	// Stamp the canonical mesh headers. chora-observability re-gates on
	// x-mesh-user-roles fail-closed, so WITHOUT this the upstream denies every
	// call and the kill-switch is unreachable. Roles come from the validated
	// session, never from a client-supplied header.
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

	// #nosec G704 — h.observabilityURL is env-config; the path is the fixed
	// egressKillSwitchUpstreamPath constant, so no user-controlled destination.
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
