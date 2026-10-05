// notifications_handler.go — HTTP route bindings for the BFF
// /api/v1/notifications/* proxy that fans out to chora-notifications.
//
// SS dress-rehearsal v2 (commit 05a359d6) flagged this BFF route as 404 (not
// 504/F7 as the v1 dress-rehearsal saw). FE in-app notifications feature
// blocked until this aggregator lands.
//
// Routes mounted (all require Bearer JWT via Phase 4 enforcement —
// DefaultJWTGatedPrefixes is updated to include /api/v1/notifications):
//
//	GET    /api/v1/notifications                 → chora-notifications
//	POST   /api/v1/notifications                 → chora-notifications
//	GET    /api/v1/notifications/{id}            → chora-notifications
//	POST   /api/v1/notifications/mark-read       → chora-notifications
//
// The bridge passes downstream JSON bodies through verbatim (no
// snake→camel re-marshal) — chora-notifications already returns
// snake_case shapes the FE consumes directly.
package httpadapter

import (
	"net/http"
	"strings"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/notifications"
)

// NotificationsBridgePathPrefix is the single BFF prefix the notifications mux
// serves. Exposed for inclusion in DefaultJWTGatedPrefixes.
const NotificationsBridgePathPrefix = "/api/v1/notifications"

// NotificationsHandler binds the notifications aggregator to the BFF mux.
type NotificationsHandler struct {
	agg *notifications.Aggregator
}

// NewNotificationsHandler constructs the handler.
func NewNotificationsHandler(agg *notifications.Aggregator) *NotificationsHandler {
	return &NotificationsHandler{agg: agg}
}

// NewNotificationsMux returns a mux that serves the /api/v1/notifications/*
// routes. Combine with WithNotificationsBridge to compose with a base
// handler that owns non-bridge paths.
func NewNotificationsMux(agg *notifications.Aggregator) http.Handler {
	h := NewNotificationsHandler(agg)
	mux := http.NewServeMux()
	// Exact-path collection (GET list, POST enqueue).
	mux.HandleFunc("/api/v1/notifications", h.handleCollection)
	// Subtree dispatcher handles /api/v1/notifications/mark-read +
	// /api/v1/notifications/{id}. Trailing-slash registration is required for
	// net/http subtree matching; the handler distinguishes mark-read from
	// item-by-id explicitly.
	mux.HandleFunc("/api/v1/notifications/", h.handleSubpath)
	return mux
}

// WithNotificationsBridge composes a notifications mux with a base handler:
// routes under /api/v1/notifications are served by the bridge; everything
// else falls through to `base`. Passes through when `agg` is nil so
// cmd/server/main.go can opt out in unconfigured envs.
func WithNotificationsBridge(base http.Handler, agg *notifications.Aggregator) http.Handler {
	if agg == nil {
		return base
	}
	bridgeMux := NewNotificationsMux(agg)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == NotificationsBridgePathPrefix ||
			strings.HasPrefix(r.URL.Path, NotificationsBridgePathPrefix+"/") {
			bridgeMux.ServeHTTP(w, r)
			return
		}
		base.ServeHTTP(w, r)
	})
}

// -----------------------------------------------------------------------------
// /api/v1/notifications  (collection)
// -----------------------------------------------------------------------------

func (h *NotificationsHandler) handleCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		resp, _ := h.agg.List(r.Context(), notificationsAuthFromRequest(r), r.URL.RawQuery)
		writeNotificationsResp(w, resp)
	case http.MethodPost:
		body := readBody(r)
		resp, _ := h.agg.Enqueue(r.Context(), notificationsAuthFromRequest(r), body)
		writeNotificationsResp(w, resp)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"only GET and POST supported on /api/v1/notifications")
	}
}

// -----------------------------------------------------------------------------
// /api/v1/notifications/{mark-read|{id}}
// -----------------------------------------------------------------------------

func (h *NotificationsHandler) handleSubpath(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/notifications/")
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" || strings.Contains(rest, "/") {
		http.NotFound(w, r)
		return
	}
	if rest == "mark-read" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
				"POST only on /api/v1/notifications/mark-read")
			return
		}
		body := readBody(r)
		resp, _ := h.agg.MarkRead(r.Context(), notificationsAuthFromRequest(r), body)
		writeNotificationsResp(w, resp)
		return
	}
	if rest == "push-subscriptions" {
		// Web Push token register (POST) / unregister (DELETE) — ADR-172.
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
				"POST and DELETE only on /api/v1/notifications/push-subscriptions")
			return
		}
		body := readBody(r)
		resp, _ := h.agg.PushSubscriptions(r.Context(), notificationsAuthFromRequest(r), r.Method, r.URL.RawQuery, body)
		writeNotificationsResp(w, resp)
		return
	}
	// /api/v1/notifications/{id}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
			"GET only on /api/v1/notifications/{id}")
		return
	}
	resp, _ := h.agg.Get(r.Context(), notificationsAuthFromRequest(r), rest)
	writeNotificationsResp(w, resp)
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// notificationsAuthFromRequest mirrors bridgeAuthFromRequest for the
// notifications aggregator's AuthCtx shape. Pulls Bearer + traceparent +
// tenant + mesh-claims onto outbound calls so chora-notifications sees the
// canonical mesh-trust headers.
func notificationsAuthFromRequest(r *http.Request) notifications.AuthCtx {
	tp, _ := r.Context().Value(ctxKeyTraceparent).(string)
	ac := notifications.AuthCtx{
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

// writeNotificationsResp emits the aggregator Response with canonical
// Content-Type / Cache-Control headers.
func writeNotificationsResp(w http.ResponseWriter, resp notifications.Response) {
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
// added to notificationsAuthFromRequest do not need a separate import bump.
var _ = servicemesh.MarshalToHeaders
