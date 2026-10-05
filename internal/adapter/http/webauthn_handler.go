// webauthn_handler.go — the 4 WebAuthn passkey BFF routes (auth-hardening
// Phase A4, ADR-181 D2, CHO-1718; contract:
// chora-contracts/openapi/auth-gateway.yaml):
//
//	POST /api/v1/auth/webauthn/register/begin   AUTHED  → identity /v1/auth/passkey/challenge
//	POST /api/v1/auth/webauthn/register/finish  AUTHED  → identity /v1/auth/passkey/register
//	POST /api/v1/auth/webauthn/login/begin      anon    → identity /v1/auth/passkey/challenge
//	POST /api/v1/auth/webauthn/login/finish     anon    → identity /v1/auth/passkey/verify
//	                                                      then MintHandler.completeSessionMint
//
// Route-mapping decision: the gateway maps the contract paths onto
// chora-identity's EXISTING /v1/auth/passkey/* routes (challenge/verify) plus
// the new /v1/auth/passkey/register — identity keeps its own stable internal
// API, the gateway owns the W3C JSON envelope (creation/request options
// composition + base64url field conventions). No aliased identity routes.
//
// Register ceremonies REQUIRE an authenticated Chora session (passkeys are
// added to an existing account post-first-login): the register/* paths are
// in DefaultJWTGatedPrefixes so RequireChoraSessionJWT stamps the validated
// claims before this handler runs; the handler then enforces body-gcid ==
// session-gcid.
//
// Login/finish composes the SAME session-mint pipeline as the username/password path
// (allowlist/REGISTRATION_MODE gate, ADR-165 operator stamping, tenant-scoped
// HS256 session JWT) — the identity-side dev HS256 token is dead.
package httpadapter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
	"github.com/google/uuid"
)

// WebAuthn BFF route paths (contract: auth-gateway.yaml).
const (
	PathWebAuthnRegisterBegin  = "/api/v1/auth/webauthn/register/begin"
	PathWebAuthnRegisterFinish = "/api/v1/auth/webauthn/register/finish"
	PathWebAuthnLoginBegin     = "/api/v1/auth/webauthn/login/begin"
	PathWebAuthnLoginFinish    = "/api/v1/auth/webauthn/login/finish"
)

// webauthnRPDisplayName is the human-readable relying-party name shown by
// authenticator UI (brand constant, not deployment config).
const webauthnRPDisplayName = "Chora"

// webauthnCeremonyTimeoutMS is the W3C options timeout hint — matches the
// identity-side 5-minute challenge TTL.
const webauthnCeremonyTimeoutMS = 300_000

// PasskeyBackend is the minimal port the WebAuthn handler needs against
// chora-identity. Production wire-up is clients.IdentityPasskeyClient; tests
// inject a fake.
type PasskeyBackend interface {
	Challenge(ctx context.Context, userHandle string) (*clients.PasskeyChallengeResponse, error)
	Register(ctx context.Context, req clients.PasskeyRegisterRequest) (*clients.PasskeyRegisterResponse, error)
	Verify(ctx context.Context, req clients.PasskeyVerifyRequest) (*clients.PasskeyVerifyResponse, error)
}

// WebAuthnHandler serves the 4 BFF ceremony routes.
type WebAuthnHandler struct {
	passkey       PasskeyBackend
	mint          *MintHandler // nil → login/finish 503 (MINT_DISABLED dev path)
	correlationFn func() string
}

// WebAuthnHandlerConfig is the constructor input.
type WebAuthnHandlerConfig struct {
	Passkey       PasskeyBackend
	Mint          *MintHandler  // optional — nil disables login/finish minting
	CorrelationFn func() string // optional; defaults to uuid.NewString
}

// NewWebAuthnHandler constructs the handler. Fails loud when the passkey
// backend is missing.
func NewWebAuthnHandler(cfg WebAuthnHandlerConfig) (*WebAuthnHandler, error) {
	if cfg.Passkey == nil {
		return nil, errors.New("webauthn: passkey backend required (set SVC_IDENTITY_URL)")
	}
	if cfg.CorrelationFn == nil {
		cfg.CorrelationFn = uuid.NewString
	}
	return &WebAuthnHandler{
		passkey:       cfg.Passkey,
		mint:          cfg.Mint,
		correlationFn: cfg.CorrelationFn,
	}, nil
}

// WithWebAuthnRoutes shadows the 4 WebAuthn routes on the supplied handler.
// Anything else falls through to base. nil handler = passthrough (keeps
// main.go boot-resilient when the identity URL is unset in dev).
//
// Composition (main.go): applied NEXT TO WithMintRoute, INSIDE
// WithChoraSessionOnPrefixes — the register/* paths are JWT-gated via
// DefaultJWTGatedPrefixes; the login/* paths are anonymous (they ESTABLISH
// the session, like the mint).
func WithWebAuthnRoutes(base http.Handler, h *WebAuthnHandler) http.Handler {
	if h == nil {
		return base
	}
	mux := http.NewServeMux()
	mux.HandleFunc(PathWebAuthnRegisterBegin, h.RegisterBegin)
	mux.HandleFunc(PathWebAuthnRegisterFinish, h.RegisterFinish)
	mux.HandleFunc(PathWebAuthnLoginBegin, h.LoginBegin)
	mux.HandleFunc(PathWebAuthnLoginFinish, h.LoginFinish)
	mux.Handle("/", base)
	return mux
}

// --- wire shapes ---------------------------------------------------------------

// webAuthnOptionsResponse is the begin-route envelope (contract
// WebAuthnOptionsResponse): W3C options + the server challenge handle the
// SPA must echo back at finish.
type webAuthnOptionsResponse struct {
	ChallengeID string         `json:"challenge_id"`
	Options     map[string]any `json:"options"`
}

type webAuthnRegisterBeginRequest struct {
	GCID string `json:"gcid"`
}

// authenticatorPayload mirrors the serialised PublicKeyCredential the SPA
// produces (WebAuthn JSON conventions — binary fields base64url).
type authenticatorPayload struct {
	ID       string `json:"id"`
	RawID    string `json:"rawId"`
	Type     string `json:"type"`
	Response struct {
		// registration
		AttestationObject string `json:"attestationObject"`
		// login
		AuthenticatorData string  `json:"authenticatorData"`
		Signature         string  `json:"signature"`
		UserHandle        *string `json:"userHandle"`
		// both
		ClientDataJSON string `json:"clientDataJSON"`
	} `json:"response"`
}

type webAuthnRegisterFinishRequest struct {
	GCID                string               `json:"gcid"`
	ChallengeID         string               `json:"challenge_id"`
	AttestationResponse authenticatorPayload `json:"attestation_response"`
}

type webAuthnLoginBeginRequest struct {
	Email string `json:"email"`
}

type webAuthnLoginFinishRequest struct {
	ChallengeID       string               `json:"challenge_id"`
	AssertionResponse authenticatorPayload `json:"assertion_response"`
	Email             string               `json:"email"`
}

// webAuthnCredentialResponse mirrors the contract WebAuthnCredentialResponse.
type webAuthnCredentialResponse struct {
	ID        string `json:"id"` // base64url credential id
	GCID      string `json:"gcid"`
	CreatedAt string `json:"created_at"`
}

// --- register/begin ---------------------------------------------------------------

// RegisterBegin mints creation options for the SIGNED-IN user.
func (h *WebAuthnHandler) RegisterBegin(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	claims, ok := h.requireSession(w, r)
	if !ok {
		return
	}
	var req webAuthnRegisterBeginRequest
	if err := decodeLenientJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "AUTH_BAD_REQUEST", "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.GCID) == "" {
		writeError(w, http.StatusBadRequest, "AUTH_BAD_REQUEST", "gcid required")
		return
	}
	if req.GCID != claims.GCID {
		writeError(w, http.StatusForbidden, "AUTH_GCID_MISMATCH",
			"gcid must match the session's gcid claim")
		return
	}

	ch, err := h.passkey.Challenge(r.Context(), claims.Email)
	if err != nil {
		h.writeUpstreamError(w, err)
		return
	}

	// W3C PublicKeyCredentialCreationOptions (binary fields base64url).
	// user.id is the opaque WebAuthn user handle — the gcid bytes; never an
	// email (W3C §14.6.1 privacy).
	writeJSON(w, http.StatusOK, webAuthnOptionsResponse{
		ChallengeID: ch.ChallengeID,
		Options: map[string]any{
			"challenge": ch.Challenge,
			"rp":        map[string]any{"id": ch.RPID, "name": webauthnRPDisplayName},
			"user": map[string]any{
				"id":          base64.RawURLEncoding.EncodeToString([]byte(claims.GCID)),
				"name":        claims.Email,
				"displayName": claims.Email,
			},
			"pubKeyCredParams": []map[string]any{
				{"type": "public-key", "alg": -7},   // ES256
				{"type": "public-key", "alg": -257}, // RS256
			},
			"timeout":     webauthnCeremonyTimeoutMS,
			"attestation": "none",
			"authenticatorSelection": map[string]any{
				"residentKey":      "preferred",
				"userVerification": "preferred",
			},
		},
	})
}

// --- register/finish ----------------------------------------------------------------

// RegisterFinish forwards the attestation to chora-identity, which parses the
// attestationObject and persists the REAL COSE key.
func (h *WebAuthnHandler) RegisterFinish(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	claims, ok := h.requireSession(w, r)
	if !ok {
		return
	}
	var req webAuthnRegisterFinishRequest
	if err := decodeLenientJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "AUTH_BAD_REQUEST", "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.GCID) == "" || req.GCID != claims.GCID {
		writeError(w, http.StatusForbidden, "AUTH_GCID_MISMATCH",
			"gcid must match the session's gcid claim")
		return
	}
	if strings.TrimSpace(req.ChallengeID) == "" {
		writeError(w, http.StatusBadRequest, "AUTH_BAD_REQUEST", "challenge_id required")
		return
	}
	rawID := firstNonEmpty(req.AttestationResponse.RawID, req.AttestationResponse.ID)
	if rawID == "" || req.AttestationResponse.Response.AttestationObject == "" ||
		req.AttestationResponse.Response.ClientDataJSON == "" {
		writeError(w, http.StatusBadRequest, "AUTH_BAD_REQUEST",
			"attestation_response requires rawId, response.attestationObject and response.clientDataJSON")
		return
	}

	resp, err := h.passkey.Register(r.Context(), clients.PasskeyRegisterRequest{
		ChallengeID:       req.ChallengeID,
		GCID:              req.GCID,
		CredentialID:      rawID,
		ClientDataJSON:    req.AttestationResponse.Response.ClientDataJSON,
		AttestationObject: req.AttestationResponse.Response.AttestationObject,
	})
	if err != nil {
		h.writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, webAuthnCredentialResponse{
		ID:        resp.CredentialID,
		GCID:      resp.GCID,
		CreatedAt: resp.CreatedAt,
	})
}

// --- login/begin ---------------------------------------------------------------------

// LoginBegin mints request options. Anonymous; the email hint is optional —
// discoverable credentials need none.
func (h *WebAuthnHandler) LoginBegin(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	var req webAuthnLoginBeginRequest
	if err := decodeLenientJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "AUTH_BAD_REQUEST", "invalid JSON body: "+err.Error())
		return
	}

	ch, err := h.passkey.Challenge(r.Context(), strings.TrimSpace(req.Email))
	if err != nil {
		h.writeUpstreamError(w, err)
		return
	}

	// W3C PublicKeyCredentialRequestOptions. allowCredentials intentionally
	// omitted — discoverable (resident-key) credentials let the
	// authenticator pick; an email→credential enumeration endpoint would be
	// a user-existence oracle.
	writeJSON(w, http.StatusOK, webAuthnOptionsResponse{
		ChallengeID: ch.ChallengeID,
		Options: map[string]any{
			"challenge":        ch.Challenge,
			"rpId":             ch.RPID,
			"timeout":          webauthnCeremonyTimeoutMS,
			"userVerification": "preferred",
		},
	})
}

// --- login/finish ----------------------------------------------------------------------

// LoginFinish verifies the assertion at chora-identity, then composes the
// SAME session-mint pipeline as the username/password path (ADR-181 A4).
func (h *WebAuthnHandler) LoginFinish(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	if h.mint == nil {
		writeError(w, http.StatusServiceUnavailable, "AUTH_MINT_UNAVAILABLE",
			"session mint is disabled — passkey login cannot complete")
		return
	}
	var req webAuthnLoginFinishRequest
	if err := decodeLenientJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "AUTH_BAD_REQUEST", "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.ChallengeID) == "" {
		writeError(w, http.StatusBadRequest, "AUTH_BAD_REQUEST", "challenge_id required")
		return
	}
	rawID := firstNonEmpty(req.AssertionResponse.RawID, req.AssertionResponse.ID)
	if rawID == "" || req.AssertionResponse.Response.AuthenticatorData == "" ||
		req.AssertionResponse.Response.ClientDataJSON == "" ||
		req.AssertionResponse.Response.Signature == "" {
		writeError(w, http.StatusBadRequest, "AUTH_BAD_REQUEST",
			"assertion_response requires rawId, response.authenticatorData, response.clientDataJSON and response.signature")
		return
	}

	userHandle := ""
	if req.AssertionResponse.Response.UserHandle != nil {
		userHandle = *req.AssertionResponse.Response.UserHandle
	}
	verified, err := h.passkey.Verify(r.Context(), clients.PasskeyVerifyRequest{
		ChallengeID:       req.ChallengeID,
		CredentialID:      rawID,
		ClientDataJSON:    req.AssertionResponse.Response.ClientDataJSON,
		AuthenticatorData: req.AssertionResponse.Response.AuthenticatorData,
		Signature:         req.AssertionResponse.Response.Signature,
		UserHandle:        userHandle,
	})
	if err != nil {
		h.writeUpstreamError(w, err)
		return
	}
	email := strings.ToLower(strings.TrimSpace(verified.Email))
	if email == "" || strings.TrimSpace(verified.GCID) == "" {
		writeError(w, http.StatusBadGateway, "AUTH_RESOLVE_INCONSISTENT",
			"identity verify returned an incomplete identity (gcid/email missing)")
		return
	}

	// Compose the shared mint pipeline (allowlist/REGISTRATION_MODE gate +
	// resolve + auto-enrol invariant + ADR-165 operator stamping + HS256
	// session JWT). The federated subject mirrors the webauthn-minted user
	// rows ("webauthn|"+email); identity's resolve falls back to the
	// by-email lookup for accounts created via other providers. The
	// verified gcid is passed as the consistency expectation.
	h.mint.completeSessionMint(w, r, email, "webauthn|"+email, "", verified.GCID)
}

// --- helpers -------------------------------------------------------------------------

// requireSession reads the chora-session claims stamped by
// RequireChoraSessionJWT (register paths are in DefaultJWTGatedPrefixes).
// Defence in depth: if the gate was bypassed (misconfigured prefix list),
// reject rather than serve an unauthenticated ceremony.
func (h *WebAuthnHandler) requireSession(w http.ResponseWriter, r *http.Request) (claims sessionIdentity, ok bool) {
	c, present := ChoraSessionClaimsFromContext(r.Context())
	if !present || strings.TrimSpace(c.GCID) == "" {
		writeError(w, http.StatusUnauthorized, "GATEWAY_UNAUTHENTICATED",
			"authenticated Chora session required for passkey registration")
		return sessionIdentity{}, false
	}
	return sessionIdentity{GCID: c.GCID, Email: strings.ToLower(strings.TrimSpace(c.Email))}, true
}

// sessionIdentity is the slice of session claims the ceremonies need.
type sessionIdentity struct {
	GCID  string
	Email string
}

// writeUpstreamError maps a passkey-backend error onto the response:
// structured 4xx rejections are mirrored verbatim (status + code), everything
// else is a 503.
func (h *WebAuthnHandler) writeUpstreamError(w http.ResponseWriter, err error) {
	var upstream *clients.PasskeyUpstreamError
	if errors.As(err, &upstream) {
		writeError(w, upstream.StatusCode, upstream.Code, upstream.Message)
		return
	}
	log.Printf("webauthn: identity passkey upstream error: %v", err)
	writeError(w, http.StatusServiceUnavailable, "AUTH_IDENTITY_UPSTREAM_UNAVAILABLE",
		"chora-identity passkey backend unavailable")
}

// requirePost writes a 405 + Allow header for non-POST methods.
func requirePost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "AUTH_METHOD_NOT_ALLOWED", "only POST supported")
		return false
	}
	return true
}

// decodeLenientJSON decodes WITHOUT DisallowUnknownFields — the WebAuthn
// payloads carry browser-version-dependent extra fields
// (authenticatorAttachment, clientExtensionResults, transports...) that must
// not 400 the ceremony. Empty bodies yield io.EOF, which callers may treat
// as "no hints".
func decodeLenientJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return io.EOF
	}
	return json.NewDecoder(r.Body).Decode(v)
}

// firstNonEmpty returns the first non-empty trimmed string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
