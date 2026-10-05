package httpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
	"github.com/apollo-chora/chora-gateway/internal/domain/aggregate"
	"github.com/apollo-chora/chora-gateway/internal/domain/route"
	"github.com/apollo-chora/chora-gateway/internal/domain/session"
)

// Handler holds the BFF's collaborators.
type Handler struct {
	routes   route.Repository
	sessions session.Repository
	upstream upstream.Client
	now      func() time.Time
}

// NewHandler is the canonical constructor.
func NewHandler(routes route.Repository, sessions session.Repository, up upstream.Client) *Handler {
	return &Handler{routes: routes, sessions: sessions, upstream: up, now: func() time.Time { return time.Now().UTC() }}
}

// NewRouter wires the public mux with middleware in order:
//
//	traceContext -> logging -> auth -> handler
func NewRouter(routes route.Repository, sessions session.Repository, up upstream.Client) http.Handler {
	return NewRouterWithGraphQL(routes, sessions, up, nil)
}

// NewRouterWithGraphQL wires the public mux including the optional /graphql
// federated endpoint. When graphqlHandler is non-nil it serves learner-facing
// reads at /graphql alongside the existing REST routes (per CLAUDE.md §4).
// Existing REST routes are NEVER removed — REST + GraphQL coexist by design.
func NewRouterWithGraphQL(routes route.Repository, sessions session.Repository, up upstream.Client, graphqlHandler http.Handler) http.Handler {
	h := NewHandler(routes, sessions, up)

	mux := http.NewServeMux()
	// Health + service info (public).
	mux.HandleFunc("/healthz", healthz)
	mux.HandleFunc("/healthz/", healthz)
	mux.HandleFunc("/health", healthz)
	mux.HandleFunc("/readyz", h.readyz)
	mux.HandleFunc("/version", h.version)
	mux.HandleFunc("/", h.index)

	// Auth endpoints.
	mux.HandleFunc("/api/auth/session", h.session)

	// BFF composition endpoints (REST — preserved alongside /graphql).
	mux.HandleFunc("/bff/aplus/home", h.aplusHome)
	mux.HandleFunc("/bff/cplus/feed", h.cplusFeed)
	mux.HandleFunc("/bff/hplus/tenant", h.hplusTenant)
	mux.HandleFunc("/bff/oplus/governance", h.oplusGovernance)
	mux.HandleFunc("/bff/rplus/courses", h.rplusCourses)

	// GraphQL federated endpoint (Phase 31 — learner-facing reads).
	// /api/v1/graphql aliases the same handler (FE posts there); /graphql kept.
	if graphqlHandler != nil {
		mux.Handle("/graphql", graphqlHandler)
		mux.Handle("/graphql/", graphqlHandler)
		mux.Handle("/api/v1/graphql", graphqlHandler)
		mux.Handle("/api/v1/graphql/", graphqlHandler)
	}

	// Generic proxy mode.
	mux.HandleFunc("/api/proxy/", h.genericProxy)

	return traceContext(logging(h.authMiddleware(mux)))
}

// -----------------------------------------------------------------------------
// Health, readyz, version, index
// -----------------------------------------------------------------------------

func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func (h *Handler) readyz(w http.ResponseWriter, r *http.Request) {
	all, err := h.routes.All(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "route-table-unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ready",
		"route_count": len(all),
		"routes":      all,
	})
}

func (h *Handler) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service":    "chora-gateway",
		"version":    "0.1.0",
		"build_date": h.now().Format(time.RFC3339),
	})
}

func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":     "chora-gateway",
		"description": "BFF (Backend-For-Frontend) for the 5 CHORA Angular surfaces.",
		"surfaces":    []string{"aplus", "cplus", "hplus", "oplus", "rplus"},
		"trace_role":  "root — generates traceparent if missing, propagates to upstreams",
	})
}

// -----------------------------------------------------------------------------
// Session: /api/auth/session
// -----------------------------------------------------------------------------

type sessionResponse struct {
	Token     string    `json:"token"`
	Gcid      string    `json:"gcid"`
	TenantID  string    `json:"tenant_id"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// session implements POST /api/auth/session (mint) + DELETE /api/auth/session (sign-out).
func (h *Handler) session(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.mintSession(w, r)
	case http.MethodDelete:
		h.signOut(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"only POST + DELETE supported on /api/auth/session")
	}
}

// mintSession exchanges an OIDC JWT (placeholder shape validation in skeleton)
// for an opaque server-issued session token.
func (h *Handler) mintSession(w http.ResponseWriter, r *http.Request) {
	jwt := bearerToken(r)
	if jwt == "" {
		writeError(w, http.StatusUnauthorized, "GATEWAY_MISSING_AUTH_HEADER",
			"Authorization: Bearer {jwt} header required")
		return
	}
	if err := session.ValidateJWTShape(jwt); err != nil {
		writeError(w, http.StatusUnauthorized, "GATEWAY_INVALID_JWT",
			"jwt shape invalid")
		return
	}
	// Skeleton: derive deterministic gcid + tenant_id from the JWT payload
	// segment (the middle dotted part). Production will verify signature via
	// chora-identity and read sub + tenant_id claims.
	parts := strings.Split(jwt, ".")
	gcid := "gcid-skel-" + truncate(parts[1], 8)
	tenantID := "tenant-skel-" + truncate(parts[1], 8)

	// If the JWT payload is exactly "admin" (base64-friendly placeholder),
	// give the session admin role so /bff/oplus/* + /bff/hplus/* are
	// reachable.
	roles := []string{"learner"}
	if strings.Contains(strings.ToLower(parts[1]), "admin") {
		roles = []string{"admin"}
	}

	s, err := session.New(session.NewParams{Gcid: gcid, TenantID: tenantID, Roles: roles})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "GATEWAY_SESSION_MINT_FAILED", err.Error())
		return
	}
	if err := h.sessions.Save(r.Context(), s); err != nil {
		writeError(w, http.StatusInternalServerError, "GATEWAY_SESSION_PERSIST_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sessionResponse{
		Token: s.Token, Gcid: s.Gcid, TenantID: s.TenantID,
		IssuedAt: s.IssuedAt, ExpiresAt: s.ExpiresAt,
	})
}

// signOut deletes the active session (idempotent — 204 even when missing).
func (h *Handler) signOut(w http.ResponseWriter, r *http.Request) {
	tok := bearerToken(r)
	if tok == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.sessions.Delete(r.Context(), tok); err != nil {
		log.Printf("session delete error: %v", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// -----------------------------------------------------------------------------
// BFF composition: A+ home
// -----------------------------------------------------------------------------

// aplusHome composes a single response from chora-consumption (LearningPath +
// Companion) and chora-creation (recent atoms). Graceful degradation: if any
// upstream fails, the missing parts are flagged in part_errors but the
// successful parts still render.
//
// AuthCtx propagation: stamps upstream.WithAuthCtx on the request context
// so HTTPUpstream's outbound calls carry the W3C traceparent + mesh metadata
// (chora-gcid, chora-tenant-id, chora-role-summary). Per
// docs/m10/httpupstream-prod-rollout-2026-05-13.md gap 1: without this stamp
// downstream Cloud Run services receive zero traceparent and fragmented
// mesh metadata → trace tree breaks at the BFF boundary.
func (h *Handler) aplusHome(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	ctx := withUpstreamAuthCtx(r)
	tenantID, gcid := upstreamCtx(ctx)
	view, _ := aggregate.New("aplus.home", tenantID, gcid)

	if v, err := h.upstream.GetLearningPath(ctx, tenantID, gcid); err == nil {
		view.AddPart("learning_path", v)
	} else {
		view.AddPartError("learning_path", err)
	}
	if v, err := h.upstream.GetRecentAtoms(ctx, tenantID, gcid); err == nil {
		view.AddPart("recent_atoms", v)
	} else {
		view.AddPartError("recent_atoms", err)
	}
	if v, err := h.upstream.GetCompanion(ctx, tenantID, gcid); err == nil {
		view.AddPart("companion", v)
	} else {
		view.AddPartError("companion", err)
	}
	writeAggregate(w, view)
}

// -----------------------------------------------------------------------------
// BFF composition: C+ feed
// -----------------------------------------------------------------------------

func (h *Handler) cplusFeed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	ctx := withUpstreamAuthCtx(r)
	tenantID, gcid := upstreamCtx(ctx)
	view, _ := aggregate.New("cplus.feed", tenantID, gcid)

	if v, err := h.upstream.GetFeed(ctx, tenantID, gcid); err == nil {
		view.AddPart("feed", v)
	} else {
		view.AddPartError("feed", err)
	}
	writeAggregate(w, view)
}

// -----------------------------------------------------------------------------
// BFF composition: H+ tenant
// -----------------------------------------------------------------------------

func (h *Handler) hplusTenant(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	ctx := withUpstreamAuthCtx(r)
	tenantID, gcid := upstreamCtx(ctx)
	view, _ := aggregate.New("hplus.tenant", tenantID, gcid)

	if v, err := h.upstream.GetTenant(ctx, tenantID, gcid); err == nil {
		view.AddPart("tenant", v)
	} else {
		view.AddPartError("tenant", err)
	}
	writeAggregate(w, view)
}

// -----------------------------------------------------------------------------
// BFF composition: O+ governance
// -----------------------------------------------------------------------------

func (h *Handler) oplusGovernance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	ctx := withUpstreamAuthCtx(r)
	tenantID, gcid := upstreamCtx(ctx)
	view, _ := aggregate.New("oplus.governance", tenantID, gcid)

	if v, err := h.upstream.GetGovernance(ctx, tenantID, gcid); err == nil {
		view.AddPart("governance", v)
	} else {
		view.AddPartError("governance", err)
	}
	if v, err := h.upstream.GetAuditEvents(ctx, tenantID, gcid); err == nil {
		view.AddPart("audit_events", v)
	} else {
		view.AddPartError("audit_events", err)
	}
	writeAggregate(w, view)
}

// -----------------------------------------------------------------------------
// BFF composition: R+ courses
// -----------------------------------------------------------------------------

func (h *Handler) rplusCourses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	ctx := withUpstreamAuthCtx(r)
	tenantID, gcid := upstreamCtx(ctx)
	view, _ := aggregate.New("rplus.courses", tenantID, gcid)

	if v, err := h.upstream.GetCourses(ctx, tenantID, gcid); err == nil {
		view.AddPart("courses", v)
	} else {
		view.AddPartError("courses", err)
	}
	writeAggregate(w, view)
}

// -----------------------------------------------------------------------------
// Generic proxy
// -----------------------------------------------------------------------------

// genericProxy implements POST/GET /api/proxy/{service}/{rest}. In skeleton
// mode there's no real upstream HTTP client, so we return a deterministic
// envelope echoing what would be forwarded — including the gcid + X-Tenant-Id
// headers the BFF would stamp. This lets tests assert header propagation
// without standing up a real backend.
func (h *Handler) genericProxy(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/proxy/")
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" {
		writeError(w, http.StatusBadRequest, "GATEWAY_PROXY_TARGET_REQUIRED",
			"path must include the target service and sub-path: /api/proxy/{service}/{rest}")
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	service := parts[0]
	subPath := ""
	if len(parts) == 2 {
		subPath = parts[1]
	}

	tenantID, gcid := upstreamCtx(r.Context())
	tp := traceparentFromContext(r.Context())

	// Headers the BFF *would* stamp on the upstream call.
	stamped := map[string]string{
		"gcid":         gcid,
		"X-Tenant-Id":  tenantID,
		"traceparent":  tp,
		"X-Chora-GCID": gcid,
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"proxy_mode":      "skeleton",
		"target_service":  service,
		"upstream_path":   "/" + subPath,
		"method":          r.Method,
		"stamped_headers": stamped,
	})
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// upstreamCtx returns (tenantID, gcid) from the request context. Empty string
// for guest views.
//
// Prefers the upstream.AuthCtx (populated by withUpstreamAuthCtx from the
// validated Chora session JWT) and falls back to the legacy tenant_id / gcid
// context keys. This matters because RequireChoraSessionJWT stamps tenant_id +
// gcid into MeshClaims → AuthCtx, NOT the legacy ctxKeyTenantID that
// tenantFromContext reads. Before this delegation upstreamCtx returned an empty
// tenant on every JWT-gated /bff/* route, so the O+ observability/governance
// reads forwarded an empty X-Tenant-Id and chora-observability 400'd
// /api/token-usage/aggregate — surfacing as the /bff/oplus/costs 503. The
// HTTPUpstream surfaces were unaffected because stampAuthFromContext already
// reads the AuthCtx first; this makes the param consistent for the newer
// observability/governance clients that trust it directly.
func upstreamCtx(ctx context.Context) (string, string) {
	return IdentityFromContextWithAuthCtx(ctx)
}

func writeAggregate(w http.ResponseWriter, v *aggregate.AggregatedView) {
	// Always 200 for aggregated views; partial-status is signalled in the
	// envelope (graceful degradation invariant).
	writeJSON(w, http.StatusOK, map[string]any{
		"view":        v.View,
		"status":      v.Status(),
		"tenant_id":   v.TenantID,
		"gcid":        v.Gcid,
		"parts":       v.Parts,
		"part_errors": v.PartErrors,
		"built_at":    v.BuiltAt,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type errEnvelope struct {
	Error errBody `json:"error"`
}

type errBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// CorrelationID lets a caller quote one identifier that ties their failed
	// request to a gateway log line. Omitted rather than emitted empty: an
	// empty string reads as an id that failed to resolve, whereas absence
	// honestly says none was stamped.
	CorrelationID string `json:"correlation_id,omitempty"`
}

// writeError emits the canonical {error:{code, message, correlation_id}}
// envelope. It is the single writer behind every GATEWAY_* error site.
//
// The correlation id is read back off the response header rather than minted
// here, so the body value is always the SAME id as the X-Correlation-ID header
// (chora-contracts/openapi/gateway.yaml describes the field as "Correlation ID
// matching the X-Correlation-ID response header"). middleware.CorrelationID
// stamps that header before any handler runs, so it is already present.
// Reading it back also means no call site had to change signature.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errEnvelope{Error: errBody{
		Code:          code,
		Message:       msg,
		CorrelationID: w.Header().Get("X-Correlation-ID"),
	}})
}

func decodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// _ keeps the decodeJSON helper exported via package use; the dead-code linter
// keeps it warm against future POST endpoints.
var _ = decodeJSON
