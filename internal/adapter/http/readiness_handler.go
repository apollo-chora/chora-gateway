// readiness_handler.go: GET /api/v1/admin/readiness
// (UX refactor Phase E, package E2).
//
// The H+ instance-readiness and go-live screens read this in ONE round trip.
// Its own handler and its own aggregator, deliberately NOT folded into
// medashboard: that serves the learner home, a different audience with a
// different lifecycle.
//
// Audience is operator OR tenant-admin, fail-closed. The rows count
// administrators and read tenant configuration, so a learner has no business
// seeing them and a missing role header is a refusal rather than a silent
// narrow. This gate is the SOLE application-layer authorisation for the
// endpoint, mirroring the sub-tenant create route above it.
package httpadapter

import (
	"context"
	"log"
	"net/http"
	"strings"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/readiness"
)

// ReadinessAggregator is the port the handler needs.
type ReadinessAggregator interface {
	GetReadiness(ctx context.Context, auth readiness.AuthCtx) (readiness.Response, error)
}

// ReadinessHandler serves the readiness aggregate. A nil Agg answers 503
// rather than an empty report, because "we cannot look" and "your instance is
// empty" are different answers and only one of them is safe to show.
type ReadinessHandler struct {
	Agg ReadinessAggregator
}

// ServeHTTP: method, then audience, then tenant scope, then the aggregate.
//
// The audience gate runs BEFORE the tenant check so a learner cannot use the
// shape of the error to learn whether a tenant header was accepted.
//
// The audience gate asks "are you an admin of something". Until 2026-09-06
// nothing asked "of WHICH tenant": the tenant came verbatim from the
// client-supplied X-Tenant-Id header, so any tenant admin of any tenant could
// read any other tenant's readiness posture by changing one request header.
// That was a fifth cross-tenant path, outside the four ADR-scoped surfaces,
// unaudited, and reachable by an ordinary tenant owner. requireReadinessTenant
// now joins the two questions.
func (h *ReadinessHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if !hasPlatformOperatorRole(r) && !hasTenantAdminRole(r) {
		writeError(w, http.StatusForbidden, "GATEWAY_FORBIDDEN",
			"instance readiness requires the platform_operator or tenant_admin role")
		return
	}
	tenantID := strings.TrimSpace(r.Header.Get("X-Tenant-Id"))
	if tenantID == "" {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST",
			"X-Tenant-Id required: every readiness row is scoped to one organisation")
		return
	}
	if !requireReadinessTenant(w, r, tenantID) {
		return
	}
	if h.Agg == nil {
		writeError(w, http.StatusServiceUnavailable, "READINESS_UNAVAILABLE",
			"the readiness aggregator is not wired in this deployment")
		return
	}

	resp, err := h.Agg.GetReadiness(r.Context(), readiness.AuthCtx{
		TenantID: tenantID,
		GCID:     strings.TrimSpace(r.Header.Get("gcid")),
		Roles:    sessionRoles(r),
		Bearer:   r.Header.Get("Authorization"),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "READINESS_FAILED", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// requireReadinessTenant refuses a caller asking for a tenant its validated
// session does not hold. Returns false having written the refusal.
//
// PLATFORM_OPERATOR is the documented exception: ADR-165 makes it the sole
// tenant-less cross-tenant role, it holds no tenant_memberships row by design,
// and its whole purpose is to look across tenants. Everyone else is pinned to
// the tenant on their own session.
//
// A session with NO tenant is refused rather than allowed to fall through to
// the header. That case is not reachable through the mint today (a resolve
// with zero memberships is a 502 and an unresolvable active tenant is a 403,
// so every minted session carries a tenant), but the header must not become
// the answer if that ever stops being true: phyllis_handler.go carries the
// post-mortem of exactly that fall-through.
//
// The comparison is against the caller's OWN session tenant and never touches
// the requested one, so the refusal is not an existence oracle: a tenant that
// exists and one that does not are indistinguishable from the outside.
func requireReadinessTenant(w http.ResponseWriter, r *http.Request, requested string) bool {
	if callerMayReadTenant(r, requested) {
		return true
	}
	// A tenant admin reaching for a tenant it does not hold is a privilege
	// probe, not a typo, so it is worth a line in the log even though the
	// caller only sees a flat refusal.
	log.Printf("readiness: refused cross-tenant read: gcid=%s session_tenant=%q requested_tenant=%q",
		strings.TrimSpace(r.Header.Get("gcid")), sessionTenantID(r), requested)
	writeError(w, http.StatusForbidden, "GATEWAY_FORBIDDEN",
		"readiness is scoped to your own organisation")
	return false
}

// WithReadiness composes the readiness endpoint as an OUTER bridge, the same
// shape WithMeDashboard uses: a nil aggregator returns the base handler
// untouched, so an unconfigured deployment does not mount a route that could
// only ever answer unknown for everything.
func WithReadiness(base http.Handler, agg ReadinessAggregator) http.Handler {
	if agg == nil {
		return base
	}
	h := &ReadinessHandler{Agg: agg}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/admin/readiness" {
			h.ServeHTTP(w, r)
			return
		}
		base.ServeHTTP(w, r)
	})
}
