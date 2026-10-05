// Package middleware holds HTTP middleware that composes BEFORE the BFF
// handlers but is not tied to a specific surface or aggregate.
//
// auditor_gate.go — role-gate enforcement on `/bff/oplus/*` (the O+
// governance surface).
//
// Per the [[imda-governance-4-dimensions]] skill (three-audience
// explainability: Learner / Instructor-Admin / Auditor — role-driven
// feature visibility, NOT toggle): the O+ governance dashboard surfaces
// audit-grade artefacts (AgentDecisionLog rows + HITL queue + IMDA evidence
// projections) that only auditors may read. This middleware enforces the
// `auditor` role claim before any /bff/oplus/* handler runs.
//
// The middleware is chained AFTER the existing `RequireChoraSessionJWT`
// validator (which already validates the HS256 session JWT signature +
// stamps mesh claims on the request context). This middleware then reads
// the `roles` array from the validated mesh claims and rejects with 403
// `auditor_role_required` when the claim is absent.
//
// JWT claim shape (from chora-gateway/cmd/server MintHandler →
// libs/chora-go-common/auth/chorasession):
//
//	{
//	  "iss": "https://chora-gateway.../",
//	  "aud": "chora-bff",
//	  "sub": "<gcid>",
//	  "gcid": "<uuidv7>",
//	  "tenant_id": "<uuidv7>",
//	  "email": "...",
//	  "roles": ["learner", "admin", "auditor"],  // <-- this check
//	  "iat": 1716712345,
//	  "exp": 1716740545
//	}
//
// The middleware accepts the claim under either `roles` (canonical, per
// Bucket 4 2026-05-14 multi-tenant identity arch) or `role_summary` (legacy
// `+`-joined string — split on `+`/`,`). Tests cover both shapes.
//
// Per `feedback_no_stubs_real_wiring`: there is no role-override env flag;
// the gate is unconditionally applied to `/bff/oplus/*`. Dev usage requires
// minting a Chora session JWT with `auditor` in the roles array.
package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/apollo-chora/chora-common/auth/chorasession"
	"github.com/apollo-chora/chora-common/auth/servicemesh"
)

// AuditorRoleClaim is the canonical role identifier the gate checks for.
// Per [[imda-governance-4-dimensions]] D4 "Transparency" → three-audience
// explainability — Auditor is one of three audiences.
const AuditorRoleClaim = "auditor"

// OPlusPathPrefix is the BFF surface prefix the gate protects.
const OPlusPathPrefix = "/bff/oplus/"

// ErrAuditorRequired is returned by HasAuditorRole when the role claim is
// missing. Exported so tests + telemetry layers can match the sentinel.
var ErrAuditorRequired = errors.New("auditor role required")

// HasAuditorRole reports whether the supplied roles slice contains the
// `auditor` claim. Comparison is case-insensitive and trims whitespace.
//
// NOTE: this is the narrow "is this principal specifically an auditor"
// predicate. The O+ surface gate uses HasOPlusAccess (below), which admits
// the broader Admin-or-Auditor audience per the architecture.
func HasAuditorRole(roles []string) bool {
	for _, r := range roles {
		if strings.EqualFold(strings.TrimSpace(r), AuditorRoleClaim) {
			return true
		}
	}
	return false
}

// OPlusAuthorizedRoles is the set of tenant roles permitted to read the O+
// governance surface. Per the architecture audience definition O+ serves
// BOTH Admin and Auditor (three-audience explainability: Learner /
// Instructor-Admin / Auditor). `owner` is admitted as a strict superset of
// `admin` (the tenant owner). Learner + instructor remain excluded. Data is
// still tenant-scoped by RLS downstream, so admitting admin does not widen
// cross-tenant exposure — it aligns the gate with the documented audience.
var OPlusAuthorizedRoles = []string{"auditor", "admin", "owner"}

// HasOPlusAccess reports whether the supplied roles slice grants access to
// the O+ surface — true if it contains ANY of OPlusAuthorizedRoles.
// Comparison is case-insensitive and trims whitespace. Supersedes the
// auditor-only check the gate used before: an admin (or owner) is a valid
// O+ audience per the arch doc, not just an auditor.
func HasOPlusAccess(roles []string) bool {
	for _, r := range roles {
		rt := strings.TrimSpace(r)
		for _, allowed := range OPlusAuthorizedRoles {
			if strings.EqualFold(rt, allowed) {
				return true
			}
		}
	}
	return false
}

// RolesFromAny normalises a `roles` claim that may arrive as either a
// []string (preferred, post-Bucket-4) or a `+`/`,` joined string. Empty
// input returns nil.
//
// Accepted shapes:
//   - []string{"learner","auditor"}             -> {"learner","auditor"}
//   - "learner+auditor"                         -> {"learner","auditor"}
//   - "learner,auditor"                         -> {"learner","auditor"}
//   - "auditor"                                 -> {"auditor"}
//   - "" / nil                                  -> nil
func RolesFromAny(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case []string:
		out := make([]string, 0, len(t))
		for _, s := range t {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
		}
		return out
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return nil
		}
		// Split on either `+` or `,`. Most legacy callers used `+`.
		parts := strings.FieldsFunc(s, func(r rune) bool {
			return r == '+' || r == ','
		})
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	default:
		return nil
	}
}

// RoleResolver is the strategy that pulls the roles slice out of the
// request context. Tests inject their own (e.g. an in-memory session
// resolver); production wires the chorasession.Claims + servicemesh.MeshClaims
// extractor via NewChoraSessionRoleResolver.
type RoleResolver func(*http.Request) []string

// NewChoraSessionRoleResolver returns a RoleResolver that reads roles in
// this priority order:
//
//  1. validated `chorasession.Claims` on the request context — the
//     canonical post-Bucket-4 source
//  2. `servicemesh.MeshClaims` on the request context — used by the legacy
//     in-process session-based auth path on the /bff/* surface
//  3. raw `Claims.Raw["roles"]` map — escape hatch for non-typed claim
//     populations
//
// All three paths normalise via RolesFromAny so the canonical []string and
// legacy `+`-joined string shapes are both honoured.
func NewChoraSessionRoleResolver(
	choraSessionFromCtx func(*http.Request) (*chorasession.Claims, bool),
	meshClaimsFromCtx func(*http.Request) (*servicemesh.MeshClaims, bool),
) RoleResolver {
	return func(r *http.Request) []string {
		if choraSessionFromCtx != nil {
			if c, ok := choraSessionFromCtx(r); ok && c != nil {
				if len(c.Roles) > 0 {
					return RolesFromAny(c.Roles)
				}
				if c.Raw != nil {
					if v, ok := c.Raw["roles"]; ok {
						if roles := RolesFromAny(v); len(roles) > 0 {
							return roles
						}
					}
				}
				if c.RoleSummary != "" {
					return RolesFromAny(c.RoleSummary)
				}
			}
		}
		if meshClaimsFromCtx != nil {
			if c, ok := meshClaimsFromCtx(r); ok && c != nil {
				if len(c.Roles) > 0 {
					return RolesFromAny(c.Roles)
				}
				if c.RoleSummary != nil {
					if v, ok := c.RoleSummary["roles"]; ok {
						if roles := RolesFromAny(v); len(roles) > 0 {
							return roles
						}
					}
				}
			}
		}
		return nil
	}
}

// AuditorGate wraps `next` and enforces the auditor-role claim on requests
// whose path matches the OPlusPathPrefix (`/bff/oplus/`). Non-matching paths
// pass through untouched. On rejection writes 403 with body
// `{"error":"auditor_role_required"}`.
//
// The middleware does NOT validate the JWT itself — it expects the
// validated claims to already be on the context (typically via
// `RequireChoraSessionJWT` upstream). When the role resolver returns no
// roles (e.g. unauthenticated request), the gate emits 401-style
// `auditor_role_required` because the OAuth flow already rejected the
// request earlier in the chain; the only way to reach this middleware
// without roles is if upstream auth was misconfigured.
func AuditorGate(resolver RoleResolver, next http.Handler) http.Handler {
	if resolver == nil {
		// Defensive: a nil resolver means we cannot enforce the gate. Fail
		// loud at boot rather than silently allow.
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isOPlusPath(r.URL.Path) {
				writeAuditorRequired(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isOPlusPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		roles := resolver(r)
		if !HasOPlusAccess(roles) {
			writeAuditorRequired(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isOPlusPath returns true iff path falls under the protected /bff/oplus/
// prefix. Exact `/bff/oplus` (no trailing slash) is intentionally NOT
// matched — there is no handler at that exact path; the convention is
// that callers hit a sub-route (e.g. /bff/oplus/dashboard).
func isOPlusPath(p string) bool {
	return strings.HasPrefix(p, OPlusPathPrefix)
}

// writeAuditorRequired emits 403 with a stable JSON body that the SPA can
// match. Per `IMDA D4 transparency`: error responses on governance views
// must clearly identify the missing role so the FE can render a friendly
// "Access restricted — auditor role required" page rather than a generic
// 403.
func writeAuditorRequired(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"error":"auditor_role_required"}`))
}
