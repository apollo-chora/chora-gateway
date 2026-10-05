// weakness_resume_handler.go — Growth-Edge HITL resume BFF route (ADR-205
// WS-2/WS-8; reshaped to the FE panel-shape per CHO-1973):
//
//	POST /api/v1/me/growth-edges/uploads/{upload_id}/resume
//	     AUTHED → chora-ai-kernel-orchestrator
//	              POST /v1/orchestrator/weakness/{upload_id}/resume
//
// The learner reviews their OWN diagnosis through BOUNDED controls (an
// action — confirm | reiterate — plus per-edge accept/reject/merge decisions, a
// difficulty tri-toggle, an "add a struggle" picker, and an output chooser) —
// the A+ panel state IS the resume payload (the canonical FE
// WeaknessReviewDecision; ADR-205 D4, zero free-form reprompt). There is NO
// run_id: the orchestrator thread is keyed by {tenant, upload}. Identity is
// load-bearing: tenant_id (the orchestrator's deterministic thread-id scope) and
// gcid come EXCLUSIVELY from the validated session JWT MeshClaims; the body
// carries NO identity (any smuggled tenant_id / gcid is silently ignored).
// traceparent/tracestate are forwarded so the resume joins the upload's trace.
// The orchestrator's job/panel response (AWAITING_REVIEW + refreshed panel on
// reiterate; completed job on confirm) is forwarded back to the SPA VERBATIM.
//
// The route is a Go 1.22 method+wildcard pattern, more specific than the
// gatewayproxy /api/v1/me/growth-edges/ subtree, so it captures ONLY the
// /uploads/{id}/resume leaf and everything else falls through to the
// consumption proxy. It is JWT-gated by the /api/v1/me/growth-edges prefix in
// DefaultJWTGatedPrefixes (MeshClaims are stamped before this handler runs).
//
// Degraded mode: when AI_KERNEL_ORCHESTRATOR_URL is unset the loader passes a
// nil handler and WithWeaknessResumeRoute answers 503
// WEAKNESS_RESUME_UNAVAILABLE — the FE calls this real path (no stub) and must
// see a diagnosable failure, never a silent 404.
package httpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
)

// PatternWeaknessResume is the Go 1.22+ method+wildcard ServeMux pattern. The
// method is pinned in the pattern, so a non-POST request is not captured here
// (it falls through to the consumption proxy via the base handler).
const PatternWeaknessResume = "POST /api/v1/me/growth-edges/uploads/{upload_id}/resume"

// maxWeaknessResumeBodyBytes caps the bounded review payload (panel state only).
const maxWeaknessResumeBodyBytes = 1 << 20

// WeaknessResumeBackend is the minimal port against the orchestrator resume
// route. Production wire-up is clients.WeaknessResumeClient; tests inject a fake.
// The orchestrator's job/panel response is returned (and forwarded) as a raw
// JSON body — the gateway is a pure proxy and does not interpret its shape.
type WeaknessResumeBackend interface {
	ResumeWeakness(ctx context.Context, uploadID string, req clients.WeaknessResumeRequest, id clients.WeaknessResumeIdentity) (json.RawMessage, error)
}

// WeaknessResumeHandler serves the single resume route.
type WeaknessResumeHandler struct {
	backend WeaknessResumeBackend
}

// NewWeaknessResumeHandler constructs the handler. Fails loud when the backend
// is missing — degraded mode is a nil *WeaknessResumeHandler passed to
// WithWeaknessResumeRoute, never a stubbed backend.
func NewWeaknessResumeHandler(backend WeaknessResumeBackend) (*WeaknessResumeHandler, error) {
	if backend == nil {
		return nil, errors.New("weakness_resume: backend required (set AI_KERNEL_ORCHESTRATOR_URL)")
	}
	return &WeaknessResumeHandler{backend: backend}, nil
}

// WithWeaknessResumeRoute shadows the resume route on the supplied handler.
// Anything else falls through to base. A nil handler → the route answers 503
// WEAKNESS_RESUME_UNAVAILABLE (env-degraded posture) so the FE sees a
// diagnosable failure instead of a 404.
//
// Composition (main.go): applied INSIDE the session gate — the
// /api/v1/me/growth-edges prefix is JWT-gated, so MeshClaims are stamped first.
func WithWeaknessResumeRoute(base http.Handler, h *WeaknessResumeHandler) http.Handler {
	mux := http.NewServeMux()
	if h == nil {
		mux.HandleFunc(PatternWeaknessResume, func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusServiceUnavailable, "WEAKNESS_RESUME_UNAVAILABLE",
				"Growth-Edge analyser resume not configured (AI_KERNEL_ORCHESTRATOR_URL unset)")
		})
		mux.Handle("/", base)
		return mux
	}
	mux.HandleFunc(PatternWeaknessResume, h.MeResume)
	mux.Handle("/", base)
	return mux
}

// MeResume — POST /api/v1/me/growth-edges/uploads/{upload_id}/resume (200).
// Drives the checkpointed graph to completion with the learner's bounded review.
func (h *WeaknessResumeHandler) MeResume(w http.ResponseWriter, r *http.Request) {
	tenantID, gcid, ok := requireMeshIdentity(w, r)
	if !ok {
		return
	}
	uploadID := strings.TrimSpace(r.PathValue("upload_id"))
	if uploadID == "" {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "upload_id path param required")
		return
	}
	var req clients.WeaknessResumeRequest
	if !decodeWeaknessResumeBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Action) == "" {
		// The bounded action set (confirm/reiterate) is validated upstream — the
		// gateway only enforces presence, never duplicates the set. There is NO
		// run_id any more: the orchestrator thread is keyed by {tenant, upload}.
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "action required")
		return
	}
	out, err := h.backend.ResumeWeakness(r.Context(), uploadID, req, clients.WeaknessResumeIdentity{
		TenantID:    tenantID,
		GCID:        gcid,
		Traceparent: r.Header.Get("traceparent"),
		Tracestate:  r.Header.Get("tracestate"),
	})
	if err != nil {
		writeWeaknessResumeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// decodeWeaknessResumeBody decodes the 1 MiB-capped JSON review payload into v.
// An empty body leaves v zero (the action presence check then 400). Unknown
// fields are IGNORED (encoding/json default) so a smuggled tenant_id/gcid (or a
// legacy run_id) is dropped — identity is session-only and the thread is keyed
// by {tenant, upload}.
func decodeWeaknessResumeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Body == nil {
		return true
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxWeaknessResumeBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "read body: "+err.Error())
		return false
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return true
	}
	if err := json.Unmarshal(raw, v); err != nil {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "malformed json: "+err.Error())
		return false
	}
	return true
}

// writeWeaknessResumeUpstreamError maps a backend error onto the wire: a
// structured 4xx rejection is mirrored verbatim (status + code + message — e.g.
// 400 invalid_decision); everything else is a 503 WEAKNESS_RESUME_UNAVAILABLE.
func writeWeaknessResumeUpstreamError(w http.ResponseWriter, err error) {
	var ue *clients.WeaknessResumeUpstreamError
	if errors.As(err, &ue) {
		writeError(w, ue.StatusCode, ue.Code, ue.Message)
		return
	}
	// The wire message stays generic; the wrapped transport detail is
	// operator-only (mirrors closure's undiagnosable-blackhole lesson).
	log.Printf("weakness_resume: upstream unreachable: %v", err)
	writeError(w, http.StatusServiceUnavailable, "WEAKNESS_RESUME_UNAVAILABLE",
		"Growth-Edge analyser unreachable")
}
