// logout_handler.go — POST /api/v1/auth/logout
//
// Revokes a refresh record so a sign-out actually ends the durable half of
// the session. Before this route existed the endpoint 404'd and a "logout"
// left the refresh token live — anyone holding it could keep minting
// access JWTs for the full refresh TTL.
//
// Frozen contract:
//
//	Request:  { "refresh_token": "..." }   (empty body is accepted)
//	Response: 200 { "revoked": true }
package httpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/apollo-chora/chora-gateway/internal/domain/session"
)

// LogoutHandler holds dependencies of POST /api/v1/auth/logout.
type LogoutHandler struct {
	store session.RefreshStore
}

// NewLogoutHandler constructs the handler.
func NewLogoutHandler(store session.RefreshStore) (*LogoutHandler, error) {
	if store == nil {
		return nil, errors.New("logout: refresh store required")
	}
	return &LogoutHandler{store: store}, nil
}

// LogoutRequest is the on-wire body shape. The refresh token is optional —
// an empty body is a successful no-op so legacy clients keep working.
type LogoutRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// Logout revokes the presented refresh token. Idempotent.
func (h *LogoutHandler) Logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "AUTH_METHOD_NOT_ALLOWED",
			"only POST supported on /api/v1/auth/logout")
		return
	}
	var req LogoutRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	// A malformed body is a no-op success: logout must never strand the
	// user's local session clear on a server 4xx.
	_ = dec.Decode(&req)
	if token := strings.TrimSpace(req.RefreshToken); token != "" {
		if err := h.store.RevokeRefresh(context.WithoutCancel(r.Context()), token); err != nil {
			// Never claim revocation succeeded when it failed: the browser
			// would drop its credential while the server-side token stays live.
			writeError(w, http.StatusInternalServerError, "AUTH_LOGOUT_FAILED",
				"failed to revoke refresh token: "+sanitizeErr(err))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
}

// WithLogoutRoute shadows POST /api/v1/auth/logout on the supplied handler.
// Anything else falls through. Anonymous like the mint/refresh routes —
// revocation is driven by the presented refresh token, not a Bearer header.
func WithLogoutRoute(base http.Handler, logout *LogoutHandler) http.Handler {
	if logout == nil {
		return base
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/logout", logout.Logout)
	mux.Handle("/", base)
	return mux
}
