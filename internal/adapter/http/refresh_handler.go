// refresh_handler.go — POST /api/v1/auth/session/refresh
//
// Exchanges a live refresh token for a fresh access JWT + a rotated refresh
// token. The access JWT is stateless and short-lived (default 1h); without
// this endpoint every session is a dead man's switch that silently dies at
// the access-token expiry with no renewal path — the user is bounced to
// /login with a 401 no matter how recently they signed in.
//
// Rotation: the presented refresh token is revoked AFTER the new pair is
// minted, so a stolen record cannot be replayed past its first use, while a
// failed mint never strands the caller (the old record stays valid until its
// own expiry).
//
// Frozen contract:
//
//	Request:  { "refresh_token": "..." }
//	Response: { access_token, token_type:"Bearer", expires_in,
//	            refresh_token, refresh_expires_in, gcid }
//	Failure:  401 { "error": { "code": "AUTH_REFRESH_*", ... } }
package httpadapter

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/apollo-chora/chora-gateway/internal/domain/session"
	"github.com/google/uuid"
)

// RefreshHandler holds dependencies of POST /api/v1/auth/session/refresh.
type RefreshHandler struct {
	mint         *MintHandler
	store        session.RefreshStore
	correlationFn func() string
}

// RefreshHandlerConfig is the constructor input.
type RefreshHandlerConfig struct {
	// Mint is the shared session minter — the refresh path re-mints the
	// access JWT through the SAME claim set as the password/webauthn mint
	// (single signing definition). REQUIRED.
	Mint *MintHandler
	// Store persists refresh records. REQUIRED.
	Store session.RefreshStore
	// CorrelationFn mints correlation ids for error envelopes. Optional.
	CorrelationFn func() string
}

// NewRefreshHandler constructs the handler. Fails LOUD on missing deps.
func NewRefreshHandler(cfg RefreshHandlerConfig) (*RefreshHandler, error) {
	if cfg.Mint == nil {
		return nil, errors.New("refresh: mint handler required")
	}
	if cfg.Store == nil {
		return nil, errors.New("refresh: refresh store required")
	}
	if cfg.CorrelationFn == nil {
		cfg.CorrelationFn = func() string { return uuid.Must(uuid.NewV7()).String() }
	}
	return &RefreshHandler{mint: cfg.Mint, store: cfg.Store, correlationFn: cfg.CorrelationFn}, nil
}

// RefreshRequest is the on-wire body shape.
type RefreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// Refresh is the POST handler — registered at /api/v1/auth/session/refresh.
//
// Anonymous endpoint (the caller presents the refresh credential, not an
// access JWT). CORS is handled by the gateway-wide CORSMiddleware.
func (h *RefreshHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		h.writeRefreshError(w, http.StatusMethodNotAllowed,
			"AUTH_METHOD_NOT_ALLOWED", "only POST supported on /api/v1/auth/session/refresh")
		return
	}

	var req RefreshRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		h.writeRefreshError(w, http.StatusBadRequest,
			"AUTH_BAD_REQUEST", "invalid JSON body: "+err.Error())
		return
	}
	token := strings.TrimSpace(req.RefreshToken)
	if token == "" {
		h.writeRefreshError(w, http.StatusBadRequest,
			"AUTH_BAD_REQUEST", "refresh_token required")
		return
	}

	// Pre-check for a clean error message; the authoritative atomic check
	// happens inside RotateReissue (ReplaceRefresh).
	rec, err := h.store.GetRefresh(r.Context(), token)
	if err != nil {
		h.writeRefreshError(w, http.StatusUnauthorized,
			"AUTH_REFRESH_INVALID", "unknown refresh token")
		return
	}
	if rec.Revoked {
		h.writeRefreshError(w, http.StatusUnauthorized,
			"AUTH_REFRESH_REVOKED", "refresh token has been revoked")
		return
	}
	if rec.IsExpired(h.mint.now()) {
		h.writeRefreshError(w, http.StatusUnauthorized,
			"AUTH_REFRESH_EXPIRED", "refresh token expired — sign in again")
		return
	}

	// Atomic rotation: RotateReissue mints + persists the new pair, then
	// consumes the old token and stores the replacement in ONE locked store
	// step. A failed rotation leaves the old token active (retryable), and a
	// concurrent exchange cannot mint two live descendants.
	resp, err := h.mint.RotateReissue(r.Context(), token, rec.Gcid, rec.Email, rec.TenantID, rec.Roles, rec.Memberships)
	if err != nil {
		switch {
		case errors.Is(err, session.ErrRefreshNotFound):
			h.writeRefreshError(w, http.StatusUnauthorized,
				"AUTH_REFRESH_INVALID", "unknown refresh token")
		case errors.Is(err, session.ErrRefreshAlreadyConsumed):
			h.writeRefreshError(w, http.StatusUnauthorized,
				"AUTH_REFRESH_REVOKED", "refresh token has already been used")
		case errors.Is(err, session.ErrRefreshExpired):
			h.writeRefreshError(w, http.StatusUnauthorized,
				"AUTH_REFRESH_EXPIRED", "refresh token expired — sign in again")
		default:
			h.writeRefreshError(w, http.StatusInternalServerError,
				"AUTH_SESSION_MINT_FAILED", sanitizeErr(err))
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *RefreshHandler) writeRefreshError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, mintErrorEnvelope{Error: mintErrorBody{
		Code:          code,
		Message:       msg,
		CorrelationID: h.correlationFn(),
	}})
}

// WithRefreshRoute shadows POST /api/v1/auth/session/refresh on the supplied
// handler. Anything else falls through. Like the mint route this rides
// OUTSIDE the access-JWT gate — the caller authenticates with the refresh
// token in the body, not a Bearer header.
func WithRefreshRoute(base http.Handler, refresh *RefreshHandler) http.Handler {
	if refresh == nil {
		return base
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/session/refresh", refresh.Refresh)
	mux.Handle("/", base)
	return mux
}
