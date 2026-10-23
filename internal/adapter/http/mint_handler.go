// mint_handler.go — POST /api/v1/auth/session/mint
//
// Exchanges a username/password pair for a Chora session JWT. The gateway
// calls chora-identity POST /v1/auth/verify-credentials and mints the HS256
// Chora session JWT from the authoritative claims identity returns.
//
// Frozen contract:
//
//	Request:  { "username": "...", "password": "..." }
//	Response: { access_token, token_type:"Bearer", expires_in, gcid,
//	            memberships }
//	Failure:  401 { "error": { "code": "INVALID_CREDENTIALS", ... } }
//
// Flow (short-circuiting):
//  1. Body decode (400 on JSON / missing required fields)
//  2. chora-identity POST /v1/auth/verify-credentials (username/password)
//     - 401 → 401 INVALID_CREDENTIALS
//     - transport / 5xx → 503 AUTH_IDENTITY_UPSTREAM_UNAVAILABLE
//  3. Consistency check on the returned identity (gcid + active_tenant_id)
//  4. Membership-derived training_admin label (instructor → training_admin)
//  5. Mint HS256 session JWT using EXACTLY the returned gcid,
//     active_tenant_id and active_tenant_roles
//  6. 200 { access_token, token_type:"Bearer", expires_in, gcid, memberships }
//
// The platform_operator role is no longer stamped from a secret manager
// email list: it is a membership-backed role that arrives in
// active_tenant_roles like any other, so it can still be granted (by
// chora-identity) and flows through verbatim.
//
// Per `feedback_no_inline_config`: every value comes from env.
package httpadapter

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
	"github.com/apollo-chora/chora-gateway/internal/domain/session"
	"github.com/google/uuid"
)

// PlatformOperatorRole is the membership-backed cross-tenant role. It is NOT
// stamped by the gateway any more — it must arrive in the identity-returned
// active_tenant_roles. Kept as the canonical spelling for the surface-widening
// check below.
const PlatformOperatorRole = "platform_operator"

// InstructorRole / TrainingAdminRole (CHO-1870 / WS1): a member holding the
// `instructor` membership_role in the active tenant also carries the
// `training_admin` label role on the minted JWT. MEMBERSHIP-BACKED (derived
// from the instructor row) — the entitlement is the instructor membership
// itself, so this stays.
const (
	InstructorRole    = "instructor"
	TrainingAdminRole = "training_admin"
)

// IdentityResolver is the minimal port the WebAuthn login/finish path needs
// to look up a GCID + fetch tenant memberships after chora-identity has
// verified the passkey assertion. Production wire-up is the HTTP-based
// clients.IdentityResolveClient; tests inject a stub.
//
// The username/password mint path does NOT use this — chora-identity's
// verify-credentials response is already the authority there.
type IdentityResolver interface {
	Resolve(ctx context.Context, req clients.ResolveRequest) (*clients.ResolveResponse, error)
}

// CredentialsVerifier verifies a username/password pair against
// chora-identity and returns the authoritative identity. Production wire-up
// is clients.IdentityCredentialsClient.
type CredentialsVerifier interface {
	VerifyCredentials(ctx context.Context, username, password string) (*clients.VerifyCredentialsResponse, error)
}

// MintHandler holds the dependencies of POST /api/v1/auth/session/mint.
type MintHandler struct {
	creds         CredentialsVerifier
	identity      IdentityResolver // WebAuthn login/finish only
	sessions      session.Repository
	refreshStore  session.RefreshStore
	sessionSigner []byte // HS256 signing key
	sessionIssuer string
	sessionAud    string
	sessionTTL    time.Duration
	refreshTTL    time.Duration
	now           func() time.Time
	correlationFn func() string
}

// MintHandlerConfig is the constructor input.
type MintHandlerConfig struct {
	// Credentials verifies the username/password pair (REQUIRED).
	Credentials CredentialsVerifier
	// Identity resolves GCID + memberships for the WebAuthn path (REQUIRED).
	Identity IdentityResolver
	// Sessions persists the minted session so the BFF's session-based auth
	// middleware can resolve identity from the returned token. REQUIRED: the
	// middleware looks sessions up by token, so a mint that does not Save one
	// leaves /bff/* and /api/v1/graphql upstream calls carrying no
	// tenant/gcid.
	Sessions      session.Repository
	// RefreshStore persists the durable refresh record paired with every
	// minted access JWT. REQUIRED: without it there is no renewal path and
	// every session silently dies at the access-token expiry.
	RefreshStore  session.RefreshStore
	SessionSigner []byte // HS256 secret bytes — must be ≥32 bytes
	SessionIssuer string // e.g. "https://api.chora.site"
	SessionAud    string // e.g. "chora-local"
	SessionTTL    time.Duration
	// RefreshTTL is the refresh-record lifetime. Defaults to 7 days when
	// <= 0. Sliding: every refresh mints a record with a fresh expiry.
	RefreshTTL    time.Duration
	Now           func() time.Time
	CorrelationFn func() string
}

// NewMintHandler constructs the handler. Returns error on missing required
// deps so the binary fails fast at startup.
func NewMintHandler(cfg MintHandlerConfig) (*MintHandler, error) {
	if cfg.Credentials == nil {
		return nil, errors.New("mint: credentials verifier required (set CHORA_IDENTITY_URL)")
	}
	if cfg.Identity == nil {
		return nil, errors.New("mint: identity resolver required (set CHORA_IDENTITY_URL)")
	}
	if len(cfg.SessionSigner) < 32 {
		return nil, errors.New("mint: session signer must be ≥32 bytes (HS256)")
	}
	if strings.TrimSpace(cfg.SessionIssuer) == "" {
		return nil, errors.New("mint: session issuer required (no silent default)")
	}
	if strings.TrimSpace(cfg.SessionAud) == "" {
		return nil, errors.New("mint: session audience required (no silent default)")
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = time.Hour
	}
	if cfg.RefreshTTL <= 0 {
		cfg.RefreshTTL = 7 * 24 * time.Hour
	}
	if cfg.Sessions == nil {
		return nil, errors.New("mint: session repository required (the BFF auth middleware resolves tenant/gcid from the persisted session)")
	}
	if cfg.RefreshStore == nil {
		return nil, errors.New("mint: refresh store required (every minted session needs a durable renewal record)")
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.CorrelationFn == nil {
		cfg.CorrelationFn = func() string { return uuid.Must(uuid.NewV7()).String() }
	}
	return &MintHandler{
		creds:         cfg.Credentials,
		identity:      cfg.Identity,
		sessions:      cfg.Sessions,
		refreshStore:  cfg.RefreshStore,
		sessionSigner: append([]byte(nil), cfg.SessionSigner...),
		sessionIssuer: cfg.SessionIssuer,
		sessionAud:    cfg.SessionAud,
		sessionTTL:    cfg.SessionTTL,
		refreshTTL:    cfg.RefreshTTL,
		now:           cfg.Now,
		correlationFn: cfg.CorrelationFn,
	}, nil
}

// MintRequest is the on-wire body shape for POST /api/v1/auth/session/mint.
type MintRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// MintResponse matches the Angular AuthTokenResponse interface so the FE
// auth.service.handleAuthResponse can consume it unchanged. The refresh
// half lets the SPA renew the session without a password (POST
// /api/v1/auth/session/refresh) and survive reloads.
type MintResponse struct {
	AccessToken      string                            `json:"access_token"`
	TokenType        string                            `json:"token_type"`
	ExpiresIn        int                               `json:"expires_in"`
	RefreshToken     string                            `json:"refresh_token"`
	RefreshExpiresIn int                               `json:"refresh_expires_in"`
	GCID             string                            `json:"gcid"`
	Memberships      []clients.ResolveTenantMembership `json:"memberships"`
}

// mintErrorEnvelope matches the front-end's expected
// `error.code` / `error.message` / `error.correlation_id` path.
type mintErrorEnvelope struct {
	Error mintErrorBody `json:"error"`
}

type mintErrorBody struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	CorrelationID string `json:"correlation_id"`
}

// Mint is the POST handler — registered at /api/v1/auth/session/mint.
//
// Anonymous endpoint (this IS the auth-establishing surface); does NOT
// require an Authorization header. CORS is handled by the gateway-wide
// CORSMiddleware.
func (h *MintHandler) Mint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		h.writeMintError(w, http.StatusMethodNotAllowed,
			"AUTH_METHOD_NOT_ALLOWED", "only POST supported on /api/v1/auth/session/mint")
		return
	}

	// 1. Decode body
	var req MintRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		h.writeMintError(w, http.StatusBadRequest,
			"AUTH_BAD_REQUEST", "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Username) == "" {
		h.writeMintError(w, http.StatusBadRequest,
			"AUTH_BAD_REQUEST", "username required")
		return
	}
	if req.Password == "" {
		h.writeMintError(w, http.StatusBadRequest,
			"AUTH_BAD_REQUEST", "password required")
		return
	}

	// 2. Verify credentials at chora-identity.
	identity, err := h.creds.VerifyCredentials(r.Context(), strings.TrimSpace(req.Username), req.Password)
	if err != nil {
		switch {
		case errors.Is(err, clients.ErrInvalidCredentials):
			h.writeMintError(w, http.StatusUnauthorized,
				"INVALID_CREDENTIALS", "invalid username or password")
		case errors.Is(err, clients.ErrCredentialsUpstreamUnavailable):
			h.writeMintError(w, http.StatusServiceUnavailable,
				"AUTH_IDENTITY_UPSTREAM_UNAVAILABLE",
				"chora-identity credentials check unavailable: "+sanitizeErr(err))
		default:
			h.writeMintError(w, http.StatusServiceUnavailable,
				"AUTH_IDENTITY_UPSTREAM_UNAVAILABLE",
				"chora-identity credentials check failed: "+sanitizeErr(err))
		}
		return
	}
	if identity == nil {
		h.writeMintError(w, http.StatusBadGateway,
			"AUTH_RESOLVE_INCONSISTENT", "chora-identity returned an empty identity")
		return
	}

	// 3. Consistency check: the authoritative claims must be present.
	gcid := strings.TrimSpace(identity.GCID)
	tenantID := strings.TrimSpace(identity.ActiveTenantID)
	if gcid == "" || tenantID == "" {
		h.writeMintError(w, http.StatusBadGateway,
			"AUTH_RESOLVE_INCONSISTENT",
			"chora-identity returned an incomplete identity (gcid/active_tenant_id missing)")
		return
	}
	email := strings.ToLower(strings.TrimSpace(identity.Email))

	// 4-6. Shared mint from the authoritative identity.
	memberships := make([]clients.ResolveTenantMembership, 0, len(identity.Memberships))
	for _, m := range identity.Memberships {
		memberships = append(memberships, clients.ResolveTenantMembership{
			TenantID: m.TenantID,
			Roles:    append([]string(nil), m.Roles...),
			Surfaces: append([]string(nil), m.Surfaces...),
		})
	}

	h.mintFromIdentity(w, r, gcid, email, tenantID,
		append([]string(nil), identity.ActiveTenantRoles...), memberships)
}

// mintFromIdentity mints the session JWT from an already-verified identity and
// writes the AuthTokenResponse (or an error envelope). It is shared by the
// username/password path and the WebAuthn login/finish path.
//
// The caller passes the AUTHORITATIVE gcid, active tenant id and active roles.
func (h *MintHandler) mintFromIdentity(w http.ResponseWriter, r *http.Request, gcid, email, tenantID string, activeRoles []string, memberships []clients.ResolveTenantMembership) {
	// Membership-derived training_admin label (instructor → training_admin).
	activeRoles = expandTrainingAdmin(activeRoles)

	// platform_operator is membership-backed now: if the identity carries it,
	// widen surfaces[] to the full CHORA rail (operator god-mode). No secret
	// stamping, no email allowlist.
	isOperator := mintRolesContainFold(activeRoles, PlatformOperatorRole)
	if isOperator {
		memberships = widenSurfacesToFullRail(memberships)
	}

	now := h.now()
	access, err := h.signSessionJWT(gcid, email, tenantID, activeRoles, h.sessionTTL, now)
	if err != nil {
		h.writeMintError(w, http.StatusInternalServerError,
			"AUTH_SESSION_MINT_FAILED", err.Error())
		return
	}
	resp, err := h.Reissue(r.Context(), access, now, gcid, email, tenantID, activeRoles, memberships)
	if err != nil {
		h.writeMintError(w, http.StatusInternalServerError,
			"AUTH_SESSION_PERSIST_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// Reissue mints a rotated session pair for an ALREADY-authorised identity:
// persists the access-token session row (so the BFF session middleware keeps
// resolving tenant/gcid), issues a durable refresh record, and returns the
// wire response. Shared by the mint handler (password / webAuthn paths) and
// the refresh handler so the minted claim set has exactly one definition.
//
// `access` is the already-signed session JWT; mint passes its own fresh
// signature while refresh re-mints through signSessionJWT via reissueAccess.
func (h *MintHandler) Reissue(ctx context.Context, access string, issuedAt time.Time, gcid, email, tenantID string, activeRoles []string, memberships []clients.ResolveTenantMembership) (MintResponse, error) {
	if err := h.sessions.Save(ctx, &session.Session{
		Token:     access,
		Gcid:      gcid,
		TenantID:  tenantID,
		Roles:     append([]string(nil), activeRoles...),
		IssuedAt:  issuedAt,
		ExpiresAt: issuedAt.Add(h.sessionTTL),
	}); err != nil {
		return MintResponse{}, err
	}
	refreshTok, err := newOpaqueToken()
	if err != nil {
		return MintResponse{}, err
	}
	if err := h.refreshStore.SaveRefresh(ctx, &session.RefreshRecord{
		Token:       refreshTok,
		Gcid:        gcid,
		Email:       email,
		TenantID:    tenantID,
		Roles:       append([]string(nil), activeRoles...),
		Memberships: toSessionMemberships(memberships),
		IssuedAt:    issuedAt,
		ExpiresAt:   issuedAt.Add(h.refreshTTL),
	}); err != nil {
		return MintResponse{}, err
	}
	if memberships == nil {
		memberships = []clients.ResolveTenantMembership{}
	}
	return MintResponse{
		AccessToken:      access,
		TokenType:        "Bearer",
		ExpiresIn:        int(h.sessionTTL.Seconds()),
		RefreshToken:     refreshTok,
		RefreshExpiresIn: int(h.refreshTTL.Seconds()),
		GCID:             gcid,
		Memberships:      memberships,
	}, nil
}

// ReissueFor signs a fresh access JWT for an ALREADY-authorised identity and
// persists the rotated pair (session row + refresh record). It is the refresh
// handler's entry point — the mint paths call signSessionJWT + Reissue
// directly because they mint from the authoritative identity response.
func (h *MintHandler) ReissueFor(ctx context.Context, gcid, email, tenantID string, roles []string) (MintResponse, error) {
	now := h.now()
	access, err := h.signSessionJWT(gcid, email, tenantID, roles, h.sessionTTL, now)
	if err != nil {
		return MintResponse{}, err
	}
	return h.Reissue(ctx, access, now, gcid, email, tenantID, roles, nil)
}

// RotateReissue is the refresh handler's rotation. It first consumes the
// presented refresh token and stores its replacement in ONE atomic store step
// (ReplaceRefresh), and only then persists the access session row. Doing the
// consume-and-replace first means a failed rotation writes no access session
// (nothing to clean up) and leaves the old token active and retryable — no
// stranded session, no orphaned access row. A concurrent exchange still cannot
// mint two live descendants, because ReplaceRefresh's consume is atomic.
//
// Atomicity scope: the consume-and-replace is atomic WITHIN the refresh store.
// The access-session row lives in a separate store, so the full mint is not
// atomic across the two. For the single-instance demo both stores are in-memory
// and never fail, so the partial-write path is unreachable here; production
// multi-instance wiring must back both with one transactional store (e.g.
// Valkey WATCH/MULTI or a single Postgres row).
//
// The caller has already pre-checked the presented token (GetRefresh) for a
// clean error message; ReplaceRefresh re-checks atomically and is authoritative.
func (h *MintHandler) RotateReissue(ctx context.Context, oldToken, gcid, email, tenantID string, roles []string, priorMemberships []session.Membership) (MintResponse, error) {
	now := h.now()
	newRefresh, err := newOpaqueToken()
	if err != nil {
		return MintResponse{}, err
	}
	// Carry the prior snapshot forward: the refresh record is the only durable
	// place the caller's surfaces live, and the BFF seeds its surface rail from
	// memberships[].surfaces. Re-resolving here is not an option for a
	// password-authenticated session — chora-identity's resolve route requires a
	// firebase_uid, which those identities do not carry.
	memberships := priorMemberships
	if memberships == nil {
		memberships = []session.Membership{}
	}
	if err := h.refreshStore.ReplaceRefresh(ctx, oldToken, &session.RefreshRecord{
		Token:       newRefresh,
		Gcid:        gcid,
		Email:       email,
		TenantID:    tenantID,
		Roles:       append([]string(nil), roles...),
		Memberships: memberships,
		IssuedAt:    now,
		ExpiresAt:   now.Add(h.refreshTTL),
	}); err != nil {
		return MintResponse{}, err
	}
	access, err := h.signSessionJWT(gcid, email, tenantID, roles, h.sessionTTL, now)
	if err != nil {
		return MintResponse{}, err
	}
	if err := h.sessions.Save(ctx, &session.Session{
		Token:     access,
		Gcid:      gcid,
		TenantID:  tenantID,
		Roles:     append([]string(nil), roles...),
		IssuedAt:  now,
		ExpiresAt: now.Add(h.sessionTTL),
	}); err != nil {
		return MintResponse{}, err
	}
	return MintResponse{
		AccessToken:      access,
		TokenType:        "Bearer",
		ExpiresIn:        int(h.sessionTTL.Seconds()),
		RefreshToken:     newRefresh,
		RefreshExpiresIn: int(h.refreshTTL.Seconds()),
		GCID:             gcid,
		Memberships:      fromSessionMemberships(memberships),
	}, nil
}

// toSessionMemberships snapshots the wire membership DTOs onto the refresh
// record; fromSessionMemberships projects them back for the response.
func toSessionMemberships(in []clients.ResolveTenantMembership) []session.Membership {
	if in == nil {
		return nil
	}
	out := make([]session.Membership, 0, len(in))
	for _, m := range in {
		out = append(out, session.Membership{
			TenantID:   m.TenantID,
			TenantSlug: m.TenantSlug,
			Roles:      append([]string(nil), m.Roles...),
			Surfaces:   append([]string(nil), m.Surfaces...),
			IsDefault:  m.IsDefault,
		})
	}
	return out
}

func fromSessionMemberships(in []session.Membership) []clients.ResolveTenantMembership {
	if in == nil {
		return nil
	}
	out := make([]clients.ResolveTenantMembership, 0, len(in))
	for _, m := range in {
		out = append(out, clients.ResolveTenantMembership{
			TenantID:   m.TenantID,
			TenantSlug: m.TenantSlug,
			Roles:      append([]string(nil), m.Roles...),
			Surfaces:   append([]string(nil), m.Surfaces...),
			IsDefault:  m.IsDefault,
		})
	}
	return out
}

// newOpaqueToken mints a 256-bit url-safe opaque token for refresh records.
func newOpaqueToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// completeSessionMint runs the mint pipeline for an ALREADY-VERIFIED identity
// that still needs GCID + membership resolution (the WebAuthn login/finish
// path). The username/password path does NOT use this — it mints directly
// from the verify-credentials response.
//
// expectedGCID == "" disables the consistency check.
func (h *MintHandler) completeSessionMint(w http.ResponseWriter, r *http.Request, email, federatedSubject, activeTenantIDHint, expectedGCID string) {
	jwtEmail := strings.ToLower(strings.TrimSpace(email))

	resolution, err := h.identity.Resolve(r.Context(), clients.ResolveRequest{
		Email:          jwtEmail,
		Subject:        strings.TrimSpace(federatedSubject),
		ActiveTenantID: strings.TrimSpace(activeTenantIDHint),
	})
	if err != nil {
		switch {
		case errors.Is(err, clients.ErrNoTenantMembership):
			h.writeMintError(w, http.StatusForbidden,
				"AUTH_NO_TENANT_MEMBERSHIP",
				"User has no tenant membership — complete H+ Setup-Tenant onboarding")
		case errors.Is(err, clients.ErrIdentityResolveRouteMissing):
			h.writeMintError(w, http.StatusServiceUnavailable,
				"AUTH_IDENTITY_UPSTREAM_UNAVAILABLE",
				"chora-identity resolve route unavailable: "+sanitizeErr(err))
		case errors.Is(err, clients.ErrIdentityUpstreamUnavailable):
			h.writeMintError(w, http.StatusServiceUnavailable,
				"AUTH_IDENTITY_UPSTREAM_UNAVAILABLE",
				"chora-identity upstream unavailable: "+sanitizeErr(err))
		default:
			h.writeMintError(w, http.StatusServiceUnavailable,
				"AUTH_IDENTITY_UPSTREAM_UNAVAILABLE",
				"chora-identity call failed: "+sanitizeErr(err))
		}
		return
	}

	if expectedGCID != "" && resolution.GCID != expectedGCID {
		h.writeMintError(w, http.StatusBadGateway,
			"AUTH_RESOLVE_INCONSISTENT",
			"identity resolve returned a different gcid than the verified credential owner")
		return
	}
	if len(resolution.Memberships) == 0 {
		h.writeMintError(w, http.StatusBadGateway,
			"AUTH_RESOLVE_INCONSISTENT",
			"identity resolve returned zero tenant memberships — upstream inconsistency")
		return
	}

	activeTenantID := strings.TrimSpace(resolution.DefaultTenantID)
	if activeTenantIDHint != "" && isResolveMemberOf(resolution.Memberships, activeTenantIDHint) {
		activeTenantID = activeTenantIDHint
	}
	if activeTenantID == "" {
		h.writeMintError(w, http.StatusForbidden,
			"AUTH_NO_TENANT_MEMBERSHIP",
			"No active tenant could be resolved for this user (memberships present but none active)")
		return
	}

	activeRoles := rolesForTenant(resolution.Memberships, activeTenantID)
	h.mintFromIdentity(w, r, resolution.GCID, jwtEmail, activeTenantID, activeRoles, resolution.Memberships)
}

// isResolveMemberOf reports whether tenantID matches any of the resolution
// memberships.
func isResolveMemberOf(memberships []clients.ResolveTenantMembership, tenantID string) bool {
	for _, m := range memberships {
		if m.TenantID == tenantID {
			return true
		}
	}
	return false
}

// rolesForTenant returns the roles[] of the membership matching tenantID.
func rolesForTenant(memberships []clients.ResolveTenantMembership, tenantID string) []string {
	for _, m := range memberships {
		if m.TenantID == tenantID {
			return append([]string(nil), m.Roles...)
		}
	}
	return nil
}

// expandTrainingAdmin appends TrainingAdminRole when the active-tenant roles
// include InstructorRole (case-insensitive). Idempotent — never duplicates an
// existing training_admin. Membership-backed (CHO-1870 / WS1).
func expandTrainingAdmin(roles []string) []string {
	if !mintRolesContainFold(roles, InstructorRole) {
		return roles
	}
	if mintRolesContainFold(roles, TrainingAdminRole) {
		return roles
	}
	return append(append([]string(nil), roles...), TrainingAdminRole)
}

// fullCHORARail is the complete surface rail (A+/C+/H+/O+/R+). Used ONLY to
// widen a platform_operator's surfaces[] to god-mode.
var fullCHORARail = []string{"aplus", "cplus", "hplus", "oplus", "rplus"}

// mintRolesContainFold reports whether roles contains target
// (case-insensitive, whitespace-trimmed).
func mintRolesContainFold(roles []string, target string) bool {
	for _, r := range roles {
		if strings.EqualFold(strings.TrimSpace(r), target) {
			return true
		}
	}
	return false
}

// widenSurfacesToFullRail returns a copy of ms with every membership's
// Surfaces set to the full CHORA rail (operator god-mode). Inputs are NOT
// mutated.
func widenSurfacesToFullRail(ms []clients.ResolveTenantMembership) []clients.ResolveTenantMembership {
	out := make([]clients.ResolveTenantMembership, len(ms))
	for i, m := range ms {
		m.Surfaces = append([]string(nil), fullCHORARail...)
		out[i] = m
	}
	return out
}

// signSessionJWT produces a compact HS256-signed JWT carrying the canonical
// Chora-session claim set consumed by chorasession.Validator on the /api/*
// trust boundary.
func (h *MintHandler) signSessionJWT(gcid, email, tenantID string, roles []string, ttl time.Duration, now time.Time) (string, error) {
	header := map[string]string{"alg": "HS256", "typ": "JWT"}
	cleanRoles := make([]string, 0, len(roles))
	for _, r := range roles {
		r = strings.TrimSpace(r)
		if r != "" {
			cleanRoles = append(cleanRoles, r)
		}
	}
	claims := map[string]any{
		"iss":          h.sessionIssuer,
		"aud":          h.sessionAud,
		"sub":          gcid,
		"gcid":         gcid,
		"tenant_id":    tenantID,
		"email":        email,
		"roles":        cleanRoles,
		"role_summary": strings.Join(cleanRoles, "+"),
		"iat":          now.Unix(),
		"exp":          now.Add(ttl).Unix(),
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := base64.RawURLEncoding.EncodeToString(hb) + "." +
		base64.RawURLEncoding.EncodeToString(cb)
	mac := hmac.New(sha256.New, h.sessionSigner)
	mac.Write([]byte(signing))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signing + "." + sig, nil
}

// sanitizeErr strips the wrapping noise from errors so the response body is
// concise + safe to surface to the front-end.
func sanitizeErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if i := strings.Index(s, ":"); i >= 0 && i < len(s)-1 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

// writeMintError emits the {error:{code, message, correlation_id}} envelope.
func (h *MintHandler) writeMintError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, mintErrorEnvelope{Error: mintErrorBody{
		Code: code, Message: msg, CorrelationID: h.correlationFn(),
	}})
}

// WithMintRoute shadows POST /api/v1/auth/session/mint on the supplied
// handler. Anything else falls through.
func WithMintRoute(base http.Handler, mint *MintHandler) http.Handler {
	if mint == nil {
		return base
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/session/mint", mint.Mint)
	mux.Handle("/", base)
	return mux
}
