// Package httpadapter wires the BFF gateway HTTP layer.
//
// Middleware chain (per chora-contracts/openapi/gateway.yaml):
//
//	traceContext -> logging -> sessionAuth (where required) -> handler
//
// traceContext: ensures a W3C traceparent on every request and stores it on
// the request context so handlers can forward it to upstream calls. The BFF
// is the trace ROOT — if no inbound traceparent exists, one is minted.
//
// sessionAuth: extracts the Bearer session token, looks it up in the Session
// repository, and stores gcid + tenant_id on the request context for downstream
// handlers + upstream-call header stamping. Public routes skip this middleware.
package httpadapter

import (
	"context"
	"log"
	"net/http"
	"strings"

	"github.com/apollo-chora/chora-gateway/internal/domain/route"
	"github.com/apollo-chora/chora-gateway/internal/domain/session"
	"github.com/apollo-chora/chora-gateway/internal/observability"
)

type ctxKey string

const (
	ctxKeyTraceparent ctxKey = "traceparent"
	ctxKeyGcid        ctxKey = "gcid"
	ctxKeyTenantID    ctxKey = "tenant_id"
	ctxKeySession     ctxKey = "session"
	ctxKeyAuthMode    ctxKey = "auth_mode"
)

// HeaderTraceparent is the W3C trace-context header.
const HeaderTraceparent = "traceparent"

// traceContext stamps a traceparent on the request context + the response
// header. The BFF mints one when absent.
func traceContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tp := observability.EnsureTraceparent(r.Header.Get(HeaderTraceparent))
		w.Header().Set(HeaderTraceparent, tp)
		ctx := context.WithValue(r.Context(), ctxKeyTraceparent, tp)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// logging logs each request once dispatched.
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gcid := r.Header.Get("gcid")
		if gcid == "" {
			gcid = r.Header.Get("X-Chora-GCID")
		}
		tp := r.Context().Value(ctxKeyTraceparent)
		if gcid != "" {
			log.Printf("method=%s path=%s gcid=%s traceparent=%v", r.Method, r.URL.Path, gcid, tp)
		} else {
			log.Printf("method=%s path=%s traceparent=%v", r.Method, r.URL.Path, tp)
		}
		next.ServeHTTP(w, r)
	})
}

// authMiddleware resolves the session for protected routes and rejects
// requests that fail the route's AuthMode. Public routes are admitted with no
// session (gcid + tenant remain empty for guest views).
//
// The matched BFFRoute is looked up via the route repository; the AuthMode is
// then enforced.
func (h *Handler) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health and version paths bypass the route matcher.
		if isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		matched, err := h.routes.Match(r.Context(), r.URL.Path)
		if err != nil {
			// Unknown path -> 404 before auth, so we don't leak which routes
			// require auth.
			writeError(w, http.StatusNotFound, "GATEWAY_ROUTE_NOT_FOUND", "route not found")
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyAuthMode, matched.AuthMode)

		token := bearerToken(r)
		if token != "" {
			s, err := h.sessions.Get(r.Context(), token)
			if err == nil && !s.IsExpired() {
				ctx = context.WithValue(ctx, ctxKeySession, s)
				ctx = context.WithValue(ctx, ctxKeyGcid, s.Gcid)
				ctx = context.WithValue(ctx, ctxKeyTenantID, s.TenantID)
			}
		}

		// Enforce AuthMode.
		switch matched.AuthMode {
		case route.AuthModePublic:
			// fine — session optional
		case route.AuthModeAuthenticated:
			if _, ok := ctx.Value(ctxKeySession).(*session.Session); !ok {
				writeError(w, http.StatusUnauthorized, "GATEWAY_SESSION_REQUIRED",
					"valid session token required")
				return
			}
		case route.AuthModeAdmin:
			s, ok := ctx.Value(ctxKeySession).(*session.Session)
			if !ok {
				writeError(w, http.StatusUnauthorized, "GATEWAY_SESSION_REQUIRED",
					"valid session token required")
				return
			}
			if !s.HasAdmin() {
				writeError(w, http.StatusForbidden, "GATEWAY_ADMIN_REQUIRED",
					"admin role required")
				return
			}
		}

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// bearerToken extracts the Bearer token from the Authorization header.
// Returns empty string when missing.
func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	return ""
}

func isPublicPath(p string) bool {
	switch p {
	case "/", "/healthz", "/healthz/", "/health", "/readyz", "/version":
		return true
	}
	return false
}

func gcidFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyGcid).(string)
	return v
}

func tenantFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyTenantID).(string)
	return v
}

func traceparentFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyTraceparent).(string)
	return v
}

func sessionFromContext(ctx context.Context) *session.Session {
	v, _ := ctx.Value(ctxKeySession).(*session.Session)
	return v
}

// IdentityFromContext returns the (tenantID, gcid) pair the upstream auth
// middleware stamped on the request context, or empty strings when the
// caller is unauthenticated. Exported for use by sibling packages (e.g.,
// the /graphql resolvers package) without leaking the unexported context
// keys.
func IdentityFromContext(ctx context.Context) (tenantID, gcid string) {
	return tenantFromContext(ctx), gcidFromContext(ctx)
}
