// closure_handler.go — account-closure saga trigger BFF routes (CHO-1719,
// ADR-181 D5; contract: chora-contracts/openapi/auth-gateway.yaml Closure tag):
//
//	POST /api/v1/me/account/close         AUTHED → orchestrator POST /v1/closure/request
//	POST /api/v1/me/account/close/cancel  AUTHED → orchestrator POST /v1/closure/{id}/cancel
//	GET  /api/v1/me/account/closure       AUTHED → orchestrator GET  /v1/closure/{id}/status
//	POST /api/v1/admin/accounts/{gcid}/close
//	                                      PLATFORM_OPERATOR → POST /v1/closure/request
//	                                      (fast_close allowed; X-Chora-Role forwarded)
//
// Identity rules (the load-bearing part):
//   - me-routes: gcid + tenant_id come EXCLUSIVELY from the validated session
//     JWT claims (RequireChoraSessionJWT stamps MeshClaims; the
//     /api/v1/me/account prefix is in DefaultJWTGatedPrefixes). Body-supplied
//     gcid/tenant_id/fast_close are ignored — fast_close is NEVER settable on
//     the self-close route.
//   - "Own saga only": the orchestrator's status/cancel APIs are
//     closure_id-keyed and expose NO by-gcid lookup (closure-orchestrator.yaml
//     v2), so the FE persists the saga_id echoed by the close 202 and passes
//     it back (?closure_id= / body closure_id). The handler ALWAYS fetches the
//     saga status first and 404s CLOSURE_NOT_FOUND when the saga's gcid does
//     not match the session gcid — same envelope as a true not-found, so the
//     route is not an existence oracle for other users' sagas.
//   - admin route: target gcid from the path, operator identity
//     (requested_by_gcid + tenant_id) from the session claims. The
//     PLATFORM_OPERATOR role is compared case-insensitively (mint stamps it
//     per ADR-165/A1; '-' and '_' both accepted, mirroring
//     gatewayproxy.canonicaliseAdminRole). On success the canonical
//     "PLATFORM_OPERATOR" token is forwarded as X-Chora-Role — the
//     orchestrator independently 403s fast_close without it (defence in
//     depth).
//
// Degraded mode: when CLOSURE_ORCHESTRATOR_URL is unset the loader passes a
// nil handler and WithClosureRoutes registers all 4 routes as 503
// CLOSURE_UNAVAILABLE (mirrors the WebAuthn login/finish MINT_DISABLED 503
// posture — visible failure, never a silent 404).
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

// Closure BFF route paths (contract: auth-gateway.yaml).
const (
	PathMeAccountClose       = "/api/v1/me/account/close"
	PathMeAccountCloseCancel = "/api/v1/me/account/close/cancel"
	PathMeAccountClosure     = "/api/v1/me/account/closure"
	// PatternAdminAccountClose is a Go 1.22+ ServeMux wildcard pattern —
	// it owns ONLY the {gcid}/close leaf; the sibling admin-account routes
	// (suspend / reactivate / list / lifecycle-events) keep falling through
	// to the base handler.
	PatternAdminAccountClose = "/api/v1/admin/accounts/{gcid}/close"
)

// defaultClosureGraceDays is the gateway-side default for the orchestrator's
// REQUIRED grace_period_days (1-365). Overridable via CLOSURE_GRACE_PERIOD_DAYS
// in the loader.
const defaultClosureGraceDays = 30

// maxClosureReasonLen mirrors the auth-gateway.yaml reason maxLength.
const maxClosureReasonLen = 512

// ClosureBackend is the minimal port this handler needs against the closure
// orchestrator. Production wire-up is clients.ClosureClient; tests inject a
// fake.
type ClosureBackend interface {
	RequestClosure(ctx context.Context, req clients.ClosureRequest, roleHeader string) (*clients.ClosureCloseResponse, error)
	CancelClosure(ctx context.Context, closureID string, req clients.ClosureCancelRequest) (*clients.ClosureCancelResponse, error)
	GetClosureStatus(ctx context.Context, closureID string) (*clients.ClosureStatusResponse, error)
}

// OwnershipLookup answers "which tenants does this GCID belong to, and with
// which roles" across every tenant, for the closure ownership pre-flight
// (first-launch spec 13.7.2). Production wire-up is clients.TenancyAdminClient
// over the tenancy ListMembershipsByGCID RPC; tests inject a fake.
type OwnershipLookup interface {
	ListMemberships(ctx context.Context, caller clients.Caller, gcid string, includeSuspended bool) ([]clients.TenantMembershipDTO, error)
}

// ClosureHandler serves the 4 closure trigger routes.
type ClosureHandler struct {
	backend   ClosureBackend
	ownership OwnershipLookup
	graceDays int
}

// ClosureHandlerConfig is the constructor input.
type ClosureHandlerConfig struct {
	Backend ClosureBackend
	// Ownership is REQUIRED. Without it the ownership guarantee cannot be
	// upheld, and a closure that skips it can orphan an organisation
	// permanently, so a missing lookup is a construction failure rather than
	// a check that quietly does not run.
	Ownership OwnershipLookup
	// GracePeriodDays is the grace window requested on every saga start.
	// 0 → defaultClosureGraceDays.
	GracePeriodDays int
}

// NewClosureHandler constructs the handler. Fails loud when the backend or the
// ownership lookup is missing. Degraded mode is expressed by passing a nil
// *ClosureHandler to WithClosureRoutes, never by a stubbed dependency.
func NewClosureHandler(cfg ClosureHandlerConfig) (*ClosureHandler, error) {
	if cfg.Backend == nil {
		return nil, errors.New("closure: backend required (set CLOSURE_ORCHESTRATOR_URL)")
	}
	if cfg.Ownership == nil {
		return nil, errors.New("closure: ownership lookup required (set CHORA_TENANCY_GRPC_ADDR)")
	}
	graceDays := cfg.GracePeriodDays
	if graceDays <= 0 {
		graceDays = defaultClosureGraceDays
	}
	return &ClosureHandler{backend: cfg.Backend, ownership: cfg.Ownership, graceDays: graceDays}, nil
}

// WithClosureRoutes shadows the 4 closure routes on the supplied handler.
// Anything else falls through to base. nil handler → the routes answer 503
// CLOSURE_UNAVAILABLE (env-degraded posture per CHO-1719 spec) so callers see
// a diagnosable failure instead of a 404.
//
// Composition (main.go): applied INSIDE WithChoraSessionOnPrefixes — the
// /api/v1/me/account subtree + /api/v1/admin/ prefix are JWT-gated, so
// MeshClaims are stamped before these handlers run.
func WithClosureRoutes(base http.Handler, h *ClosureHandler) http.Handler {
	mux := http.NewServeMux()
	if h == nil {
		unavailable := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusServiceUnavailable, "CLOSURE_UNAVAILABLE",
				"closure orchestrator not configured (CLOSURE_ORCHESTRATOR_URL unset)")
		})
		mux.Handle(PathMeAccountClose, unavailable)
		mux.Handle(PathMeAccountCloseCancel, unavailable)
		mux.Handle(PathMeAccountClosure, unavailable)
		mux.Handle(PatternAdminAccountClose, unavailable)
		mux.Handle("/", base)
		return mux
	}
	mux.HandleFunc(PathMeAccountClose, h.MeClose)
	mux.HandleFunc(PathMeAccountCloseCancel, h.MeCancel)
	mux.HandleFunc(PathMeAccountClosure, h.MeStatus)
	mux.HandleFunc(PatternAdminAccountClose, h.AdminClose)
	mux.Handle("/", base)
	return mux
}

// --- wire shapes -----------------------------------------------------------------

// meCloseRequest is the self-close body. gcid / tenant_id / fast_close are
// deliberately NOT modelled: identity comes from the session, fast_close is
// operator-only — unknown body fields are ignored, never forwarded.
type meCloseRequest struct {
	Reason string `json:"reason"`
}

// meCancelRequest is the self-cancel body. closure_id is REQUIRED — the
// orchestrator's cancel API is closure_id-keyed (no by-gcid lookup).
type meCancelRequest struct {
	ClosureID string `json:"closure_id"`
	Reason    string `json:"reason"`
}

// adminCloseRequest is the operator close body (auth-gateway.yaml).
type adminCloseRequest struct {
	FastClose bool   `json:"fast_close"`
	Reason    string `json:"reason"`
}

// closureStatusResponse is the gateway's ClosureStatusResponse envelope
// (auth-gateway.yaml) — saga identity + state, composed from the orchestrator
// response plus the session/path gcid.
type closureStatusResponse struct {
	SagaID      string                        `json:"saga_id"`
	GCID        string                        `json:"gcid"`
	State       string                        `json:"state"`
	GraceEndsAt string                        `json:"grace_ends_at,omitempty"`
	RequestedAt string                        `json:"requested_at,omitempty"`
	CancelledAt string                        `json:"cancelled_at,omitempty"`
	History     []clients.ClosureHistoryEntry `json:"history,omitempty"`
	DomainAcks  []clients.ClosureDomainAck    `json:"domain_acks,omitempty"`
}

// --- handlers --------------------------------------------------------------------

// MeClose — POST /api/v1/me/account/close (202). Starts the federated saga
// for the CALLER's account. NEVER hard-deletes; the orchestrator walks
// CLOSING (grace) → SUSPENDED → PSEUDONYMIZED → COLD_ARCHIVED →
// CRYPTO_SHREDDED per Tier 3 D11.
func (h *ClosureHandler) MeClose(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	tenantID, gcid, ok := requireMeshIdentity(w, r)
	if !ok {
		return
	}
	var body meCloseRequest
	if !decodeClosureBody(w, r, &body) {
		return
	}
	if len(body.Reason) > maxClosureReasonLen {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST",
			"reason exceeds 512 characters")
		return
	}
	// Ownership pre-flight (spec 13.7.2). The subject is the SESSION gcid; a
	// body-supplied gcid is ignored here for the same reason it is ignored
	// below.
	if !h.ownershipCleared(w, r, clients.Caller{GCID: gcid, TenantID: tenantID, Roles: sessionRoles(r)}, gcid) {
		return
	}
	out, err := h.backend.RequestClosure(r.Context(), clients.ClosureRequest{
		GCID:            gcid,
		TenantID:        tenantID,
		GracePeriodDays: h.graceDays,
		Reason:          body.Reason,
		RequestedByGCID: gcid,
		FastClose:       false, // NEVER settable on the self-close route
	}, "")
	if err != nil {
		writeClosureUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, closureStatusResponse{
		SagaID:      out.SagaID,
		GCID:        gcid,
		State:       out.State,
		GraceEndsAt: out.GraceEndsAt,
		RequestedAt: out.RequestedAt,
	})
}

// MeCancel — POST /api/v1/me/account/close/cancel (200). Cancels the
// caller's OWN saga during the grace window. Ownership is verified against
// the orchestrator's status API before the cancel fires.
func (h *ClosureHandler) MeCancel(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	_, gcid, ok := requireMeshIdentity(w, r)
	if !ok {
		return
	}
	var body meCancelRequest
	if !decodeClosureBody(w, r, &body) {
		return
	}
	closureID := strings.TrimSpace(body.ClosureID)
	if closureID == "" {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST",
			"closure_id required (returned by POST /api/v1/me/account/close)")
		return
	}
	if len(body.Reason) > maxClosureReasonLen {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST",
			"reason exceeds 512 characters")
		return
	}
	if _, ok := h.fetchOwnSaga(w, r, closureID, gcid); !ok {
		return
	}
	out, err := h.backend.CancelClosure(r.Context(), closureID, clients.ClosureCancelRequest{
		ActorGCID: gcid,
		Reason:    body.Reason,
	})
	if err != nil {
		writeClosureUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, closureStatusResponse{
		SagaID:      out.SagaID,
		GCID:        gcid,
		State:       out.State,
		CancelledAt: out.CancelledAt,
	})
}

// MeStatus — GET /api/v1/me/account/closure?closure_id={id} (200/404). Reads
// the caller's OWN saga. A missing/foreign/unknown closure_id is a uniform
// 404 CLOSURE_NOT_FOUND — no existence oracle.
func (h *ClosureHandler) MeStatus(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	_, gcid, ok := requireMeshIdentity(w, r)
	if !ok {
		return
	}
	closureID := strings.TrimSpace(r.URL.Query().Get("closure_id"))
	if closureID == "" {
		// The orchestrator has no by-gcid lookup (closure-orchestrator.yaml
		// v2) — without the FE-persisted closure_id there is no saga to
		// show. 404 matches the contract's "No closure saga for this gcid".
		writeError(w, http.StatusNotFound, "CLOSURE_NOT_FOUND",
			"no closure saga known (closure_id query param required)")
		return
	}
	saga, ok := h.fetchOwnSaga(w, r, closureID, gcid)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, closureStatusResponse{
		SagaID:      saga.SagaID,
		GCID:        saga.GCID,
		State:       saga.State,
		GraceEndsAt: saga.GraceEndsAt,
		RequestedAt: saga.RequestedAt,
		History:     saga.History,
		DomainAcks:  saga.DomainAcks,
	})
}

// AdminClose — POST /api/v1/admin/accounts/{gcid}/close (202).
// PLATFORM_OPERATOR-only; fast_close collapses the grace window for
// designated TEST accounts (ADR-181 D5). The saga itself is identical — no
// hard-delete shortcut exists.
func (h *ClosureHandler) AdminClose(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	tenantID, operatorGCID, ok := requireMeshIdentity(w, r)
	if !ok {
		return
	}
	if !hasPlatformOperatorRole(r) {
		writeError(w, http.StatusForbidden, "GATEWAY_OPERATOR_REQUIRED",
			"PLATFORM_OPERATOR role required (ADR-165)")
		return
	}
	targetGCID := strings.TrimSpace(r.PathValue("gcid"))
	if targetGCID == "" {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "gcid path param required")
		return
	}
	var body adminCloseRequest
	if !decodeClosureBody(w, r, &body) {
		return
	}
	if len(body.Reason) > maxClosureReasonLen {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST",
			"reason exceeds 512 characters")
		return
	}
	// Ownership pre-flight (spec 13.7.2), on the operator route too. The
	// guarantee is about the ORGANISATION, not about who asked, and the
	// designed escape hatch is the ownership override (S7c), not a close
	// route that skips the check. The subject is the TARGET, never the
	// operator.
	if !h.ownershipCleared(w, r,
		clients.Caller{GCID: operatorGCID, TenantID: tenantID, Roles: sessionRoles(r)}, targetGCID) {
		return
	}
	out, err := h.backend.RequestClosure(r.Context(), clients.ClosureRequest{
		GCID:            targetGCID,
		TenantID:        tenantID,
		GracePeriodDays: h.graceDays,
		Reason:          body.Reason,
		RequestedByGCID: operatorGCID,
		FastClose:       body.FastClose,
	}, "PLATFORM_OPERATOR")
	if err != nil {
		writeClosureUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, closureStatusResponse{
		SagaID:      out.SagaID,
		GCID:        targetGCID,
		State:       out.State,
		GraceEndsAt: out.GraceEndsAt,
		RequestedAt: out.RequestedAt,
	})
}

// --- helpers ---------------------------------------------------------------------

// fetchOwnSaga loads the saga and enforces the "own saga only" gate. On any
// failure it writes the response (uniform 404 for unknown AND foreign sagas
// — no existence oracle; 503 on transport) and returns ok=false.
func (h *ClosureHandler) fetchOwnSaga(w http.ResponseWriter, r *http.Request, closureID, gcid string) (*clients.ClosureStatusResponse, bool) {
	saga, err := h.backend.GetClosureStatus(r.Context(), closureID)
	if err != nil {
		var ue *clients.ClosureUpstreamError
		if errors.As(err, &ue) && ue.StatusCode == http.StatusNotFound {
			writeError(w, http.StatusNotFound, "CLOSURE_NOT_FOUND", "no closure saga for this account")
			return nil, false
		}
		writeClosureUpstreamError(w, err)
		return nil, false
	}
	if saga == nil || !strings.EqualFold(strings.TrimSpace(saga.GCID), strings.TrimSpace(gcid)) {
		// Foreign saga → SAME envelope as not-found.
		writeError(w, http.StatusNotFound, "CLOSURE_NOT_FOUND", "no closure saga for this account")
		return nil, false
	}
	return saga, true
}

// ownedTenantRef names one organisation the subject owns, so the refusal can
// say which handover unblocks the closure. tenant_slug is chora-tenancy's slug
// (or the hyphenated name when no slug is set), NOT a display name.
type ownedTenantRef struct {
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
}

// closureOwnershipErrBody is the canonical error envelope plus the owned-tenant
// list. errBody is embedded anonymously so code / message / correlation_id stay
// at the same JSON depth every other GATEWAY_* refusal uses, and a generic
// client classifier keeps working unchanged.
type closureOwnershipErrBody struct {
	errBody
	OwnedTenants []ownedTenantRef `json:"owned_tenants"`
}

// ownershipCleared runs the closure ownership pre-flight for one subject GCID.
// It returns true only when the subject provably owns no organisation.
//
// Fail-closed on purpose: a lookup that errors writes 503 and returns false, so
// closure never proceeds on an unverified subject. Allowing it would trade a
// permanent orphaned organisation for a transient availability blip.
func (h *ClosureHandler) ownershipCleared(w http.ResponseWriter, r *http.Request, caller clients.Caller, subjectGCID string) bool {
	// includeSuspended=true, deliberately. A suspended owner still holds the
	// tenant: the one-live-owner index and the membership write guards ignore
	// suspension, so a pre-flight blind to them would clear the subject and
	// orphan the organisation this check exists to protect. Mint is the caller
	// that must keep passing false.
	memberships, err := h.ownership.ListMemberships(r.Context(), caller, subjectGCID, true)
	if err != nil {
		// Operator-only detail in the log; the wire message stays generic.
		log.Printf("closure: ownership pre-flight failed for %s: %v", subjectGCID, err)
		writeError(w, http.StatusServiceUnavailable, "CLOSURE_OWNERSHIP_CHECK_UNAVAILABLE",
			"cannot check tenant ownership right now, so the closure was not started")
		return false
	}

	owned := ownedTenants(memberships)
	if len(owned) == 0 {
		return true
	}
	writeJSON(w, http.StatusConflict, errEnvelope2{Error: closureOwnershipErrBody{
		errBody: errBody{
			Code: "CLOSURE_OWNS_TENANT",
			Message: "this account owns an organisation, so it cannot be closed yet: " +
				"hand ownership over first",
			CorrelationID: w.Header().Get("X-Correlation-ID"),
		},
		OwnedTenants: owned,
	}})
	return false
}

// errEnvelope2 mirrors errEnvelope for the richer closure-ownership body.
type errEnvelope2 struct {
	Error closureOwnershipErrBody `json:"error"`
}

// ownedTenants filters the memberships down to the ones where the subject holds
// the owner role. The match is EXACT after trimming and lower-casing: the
// tenancy enum label is lowercase `owner`, role-capabilities.ts also maps
// `OWNER`, and a substring match would make a future `owner_delegate` role
// block every closure.
func ownedTenants(memberships []clients.TenantMembershipDTO) []ownedTenantRef {
	out := make([]ownedTenantRef, 0, len(memberships))
	for _, m := range memberships {
		for _, role := range m.Roles {
			if strings.EqualFold(strings.TrimSpace(role), "owner") {
				out = append(out, ownedTenantRef{TenantID: m.TenantID, TenantSlug: m.TenantSlug})
				break
			}
		}
	}
	return out
}

// hasPlatformOperatorRole reports whether the validated session claims carry
// PLATFORM_OPERATOR (case-insensitive; '-'/'_' interchangeable — mirrors
// gatewayproxy.canonicaliseAdminRole's liberal-in posture).
func hasPlatformOperatorRole(r *http.Request) bool {
	roles := sessionRoles(r)
	for _, role := range roles {
		norm := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(role), "-", "_"))
		if norm == "platform_operator" {
			return true
		}
	}
	return false
}

// sessionRoles reads the validated roles off the request context — MeshClaims
// first, raw ChoraSession claims as the defence-in-depth fallback (same
// precedence as requireMeshIdentity).
func sessionRoles(r *http.Request) []string {
	if mc, ok := MeshClaimsFromContext(r.Context()); ok && mc != nil {
		return mc.Roles
	}
	if cs, ok := ChoraSessionClaimsFromContext(r.Context()); ok && cs != nil {
		return cs.Roles
	}
	return nil
}

// decodeClosureBody decodes an OPTIONAL 1 MiB-capped JSON body into v. An
// empty body is valid (all closure bodies have only optional fields at the
// gateway layer — required fields are checked by the callers). Unknown
// fields are deliberately IGNORED, not rejected: the me-close route must
// never honour smuggled gcid/tenant_id/fast_close keys, and silently
// dropping them keeps the identity-from-session rule absolute.
func decodeClosureBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Body == nil {
		return true
	}
	const maxBytes = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBytes))
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

// writeClosureUpstreamError maps a backend error onto the wire: structured
// 4xx rejections are mirrored verbatim (status + code + message — e.g. 409
// CLOSURE_CANCEL_TOO_LATE, 403 CLOSURE_AGID_REJECTED); everything else is a
// 503 CLOSURE_UNAVAILABLE.
func writeClosureUpstreamError(w http.ResponseWriter, err error) {
	var ue *clients.ClosureUpstreamError
	if errors.As(err, &ue) {
		writeError(w, ue.StatusCode, ue.Code, ue.Message)
		return
	}
	// The wire message stays generic; the wrapped transport detail is
	// operator-only. Without this line the first live wall (sidecar
	// REGISTRY_ONLY blackhole, 2026-06-12) was undiagnosable from logs.
	log.Printf("closure: upstream unreachable: %v", err)
	writeError(w, http.StatusServiceUnavailable, "CLOSURE_UNAVAILABLE",
		"closure orchestrator unreachable")
}
