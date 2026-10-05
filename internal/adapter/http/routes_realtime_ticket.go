// routes_realtime_ticket.go — APPEND-ONLY BFF route binding for the W0 (rt)
// realtime stream-ticket minter (ADR-183, CHO-1661; contract
// chora-contracts/openapi/realtime.yaml mintRealtimeTicket).
//
//	GET /api/v1/realtime/ticket  →  {ticket, expires_at}
//
// EventSource cannot send an Authorization header, so the SSE stream served
// by chora-realtime authenticates with a ~60s SINGLE-USE HS256 ticket that
// this gateway — the existing Bearer trust boundary — mints from the
// validated session identity. The claim set here is the EXACT shape
// chora-realtime's internal/adapter/ticket/hmac_validator.go verifies
// (aud "chora-realtime" / gcid / tenant_id / jti / iat / exp); both sides
// share the chora-realtime-ticket-signer secret (env
// CHORA_REALTIME_TICKET_SIGNER_SECRET, ≥32 bytes per RFC 7518 §3.2).
//
// The JWT is hand-rolled like signSessionJWT (mint_handler.go) — the gateway
// deliberately avoids github.com/golang-jwt. Composed as an outer bridge per
// the proven WithKGCanvasRoutes / WithGatewayProxy pattern (the path is NOT
// in the legacy route table, and the inner router 404s unknown paths); the
// path rides DefaultJWTGatedPrefixes so RequireChoraSessionJWT stamps
// MeshClaims BEFORE the bridge fires. nil minter (unset secret in dev) →
// clean passthrough 404, matching the established nil-bridge convention.
package httpadapter

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

// RealtimeTicketPath is the exact BFF path this bridge claims.
const RealtimeTicketPath = "/api/v1/realtime/ticket"

// EnvRealtimeTicketSigner is the env var carrying the shared HS256 signer
// (Secret Manager secret chora-realtime-ticket-signer; mounted on both the
// gateway and chora-realtime — never inline, per secrets-and-env).
const EnvRealtimeTicketSigner = "CHORA_REALTIME_TICKET_SIGNER_SECRET"

// realtimeTicketAudience matches ticket.Audience on the validator side.
const realtimeTicketAudience = "chora-realtime"

// realtimeTicketTTL is the ticket lifetime. The validator's single-use jti
// claim retention (70s) must comfortably exceed this — change BOTH together.
const realtimeTicketTTL = 60 * time.Second

// realtimeTicketMinSignerBytes mirrors the validator's HS256 floor so an
// undersized secret fails loud at boot on whichever side sees it first.
const realtimeTicketMinSignerBytes = 32

// RealtimeTicketMinter mints validator-compatible stream tickets.
type RealtimeTicketMinter struct {
	signer []byte
	now    func() time.Time
}

// NewRealtimeTicketMinter constructs a minter. Fails loud on an undersized
// signer (same floor the chora-realtime validator enforces). The signer
// slice is copied.
func NewRealtimeTicketMinter(signer []byte) (*RealtimeTicketMinter, error) {
	if len(signer) < realtimeTicketMinSignerBytes {
		return nil, fmt.Errorf("realtime ticket signer must be ≥%d bytes (got %d)",
			realtimeTicketMinSignerBytes, len(signer))
	}
	cp := make([]byte, len(signer))
	copy(cp, signer)
	return &RealtimeTicketMinter{
		signer: cp,
		now:    func() time.Time { return time.Now().UTC() },
	}, nil
}

// NewRealtimeTicketMinterFromEnv reads EnvRealtimeTicketSigner. An unset/empty
// env returns (nil, nil) — the caller logs DISABLED and the bridge passes
// through so the route 404s cleanly in unconfigured envs.
func NewRealtimeTicketMinterFromEnv() (*RealtimeTicketMinter, error) {
	sec := os.Getenv(EnvRealtimeTicketSigner)
	if strings.TrimSpace(sec) == "" {
		return nil, nil
	}
	return NewRealtimeTicketMinter([]byte(sec))
}

// Mint produces a compact HS256 JWT bound to the session identity plus its
// expiry instant. tenant may be empty (bootstrap-mode sessions, CHO-1653) —
// the validator only requires gcid. Infallible short of a dead entropy
// source: the claim set is a fixed shape of string/int64 values (marshal
// cannot fail) and the jti is a UUIDv7, which panics only where uuid.New
// would have panicked too.
func (m *RealtimeTicketMinter) Mint(gcid, tenantID string) (string, time.Time) {
	now := m.now()
	exp := now.Add(realtimeTicketTTL)

	// uuid.NewV7 errors only when the entropy source does, never on a clock
	// backstep, and uuid.New panics on that same failure. So there is no v4
	// fallback to fall back to.
	jti := uuid.Must(uuid.NewV7())

	header := map[string]string{"alg": "HS256", "typ": "JWT"}
	claims := map[string]any{
		"aud":       realtimeTicketAudience,
		"gcid":      gcid,
		"tenant_id": tenantID,
		"jti":       jti.String(),
		"iat":       now.Unix(),
		"exp":       exp.Unix(),
	}
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(hb) + "." +
		base64.RawURLEncoding.EncodeToString(cb)
	mac := hmac.New(sha256.New, m.signer)
	mac.Write([]byte(signing))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signing + "." + sig, exp
}

// realtimeTicketResponse is the contract's RealtimeTicket schema.
type realtimeTicketResponse struct {
	Ticket    string `json:"ticket"`
	ExpiresAt string `json:"expires_at"`
}

// WithRealtimeTicket claims GET RealtimeTicketPath and falls through for
// everything else. Mount INSIDE the chora-session JWT gate (the path is in
// DefaultJWTGatedPrefixes) so MeshClaims are already stamped.
func WithRealtimeTicket(next http.Handler, m *RealtimeTicketMinter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m == nil || r.URL.Path != RealtimeTicketPath {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED",
				"GET only on "+RealtimeTicketPath)
			return
		}
		claims, ok := MeshClaimsFromContext(r.Context())
		if !ok || strings.TrimSpace(claims.GCID) == "" {
			// Defence in depth — RequireChoraSessionJWT should have 401'd
			// already; a claims-less request must never mint a credential.
			writeError(w, http.StatusUnauthorized, "REALTIME_UNAUTHENTICATED",
				"authenticated Chora session required to mint a stream ticket")
			return
		}
		ticket, exp := m.Mint(claims.GCID, claims.TenantID)
		// A ticket is a credential: forbid any cache layer from retaining it.
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, realtimeTicketResponse{
			Ticket:    ticket,
			ExpiresAt: exp.Format(time.RFC3339),
		})
	})
}
