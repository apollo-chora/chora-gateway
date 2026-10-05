// mint_handler_test.go — specs for POST /api/v1/auth/session/mint under the
// frozen username/password contract.
//
// Shared fixtures (stubIdentityResolver, mintFixture) are reused by
// webauthn_handler_test.go.
package httpadapter_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
)

const (
	mintDefaultTenantID = "01970000-0000-7000-8000-0000000000bb"
	mintOtherTenantID   = "01970000-0000-7000-8000-0000000000cc"
	mintTestEmail       = "alice@example.com"
	// 32+ byte HS256 signer.
	mintTestSigner = "test-mint-session-signer-0123456789abcdef"
	mintTestIssuer = "https://api.chora.site"
	mintTestAud    = "chora-local"
	mintTestPasswd = "correct horse battery staple"
	mintTestUser   = "alice"
)

// --- stubs -------------------------------------------------------------------

// stubIdentityResolver serves the WebAuthn resolve path.
type stubIdentityResolver struct {
	resp *clients.ResolveResponse
	err  error
}

func (s *stubIdentityResolver) Resolve(_ context.Context, _ clients.ResolveRequest) (*clients.ResolveResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.resp, nil
}

// stubCredentialsVerifier serves the username/password mint path.
type stubCredentialsVerifier struct {
	resp         *clients.VerifyCredentialsResponse
	err          error
	lastUsername string
	lastPassword string
	calls        int
}

func (s *stubCredentialsVerifier) VerifyCredentials(_ context.Context, username, password string) (*clients.VerifyCredentialsResponse, error) {
	s.calls++
	s.lastUsername = username
	s.lastPassword = password
	if s.err != nil {
		return nil, s.err
	}
	return s.resp, nil
}

func defaultVerifyResponse() *clients.VerifyCredentialsResponse {
	return &clients.VerifyCredentialsResponse{
		GCID:              "01970000-0000-7000-8000-0000000000aa",
		Email:             mintTestEmail,
		ActiveTenantID:    mintDefaultTenantID,
		ActiveTenantRoles: []string{"learner"},
		Memberships: []clients.IdentityMembership{
			{TenantID: mintDefaultTenantID, Roles: []string{"learner"}},
		},
	}
}

func defaultResolveResponse() *clients.ResolveResponse {
	return &clients.ResolveResponse{
		GCID:            "01970000-0000-7000-8000-0000000000aa",
		Email:           mintTestEmail,
		DefaultTenantID: mintDefaultTenantID,
		Memberships: []clients.ResolveTenantMembership{
			{TenantID: mintDefaultTenantID, Roles: []string{"learner"}, IsDefault: true},
		},
	}
}

// --- fixture -----------------------------------------------------------------

type mintFixture struct {
	handler  *httpadapter.MintHandler
	creds    *stubCredentialsVerifier
	identity *stubIdentityResolver
}

// newMintFixtureWith builds a MintHandler with the default stubs; mutate may
// override the config (e.g. a custom credentials verifier).
func newMintFixtureWith(t *testing.T, mutate func(*httpadapter.MintHandlerConfig)) *mintFixture {
	t.Helper()
	creds := &stubCredentialsVerifier{resp: defaultVerifyResponse()}
	identity := &stubIdentityResolver{resp: defaultResolveResponse()}
	cfg := httpadapter.MintHandlerConfig{
		Credentials:   creds,
		Identity:      identity,
		SessionSigner: []byte(mintTestSigner),
		SessionIssuer: mintTestIssuer,
		SessionAud:    mintTestAud,
		SessionTTL:    time.Hour,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	h, err := httpadapter.NewMintHandler(cfg)
	if err != nil {
		t.Fatalf("NewMintHandler: %v", err)
	}
	return &mintFixture{handler: h, creds: creds, identity: identity}
}

func newMintFixture(t *testing.T) *mintFixture {
	t.Helper()
	return newMintFixtureWith(t, nil)
}

func (f *mintFixture) doRequest(t *testing.T, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body == nil {
		rd = strings.NewReader("")
	} else if s, ok := body.(string); ok {
		rd = strings.NewReader(s)
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rd = strings.NewReader(string(b))
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/session/mint", rd)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.handler.Mint(w, r)
	return w
}

func decodeMintBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return out
}

func mintErrCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	body := decodeMintBody(t, w)
	e, _ := body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func decodeJWTClaims(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body := decodeMintBody(t, w)
	tok, _ := body["access_token"].(string)
	if tok == "" {
		t.Fatalf("no access_token in %s", w.Body.String())
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", tok)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	return claims
}

func jwtRoles(t *testing.T, claims map[string]any) []string {
	t.Helper()
	raw, _ := claims["roles"].([]any)
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		s, _ := r.(string)
		out = append(out, s)
	}
	return out
}

func rolesContain(roles []string, want string) bool {
	for _, r := range roles {
		if r == want {
			return true
		}
	}
	return false
}

// --- happy path --------------------------------------------------------------

func TestMint_HappyPath(t *testing.T) {
	f := newMintFixture(t)
	w := f.doRequest(t, map[string]any{"username": mintTestUser, "password": mintTestPasswd})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if f.creds.lastUsername != mintTestUser || f.creds.lastPassword != mintTestPasswd {
		t.Errorf("verify got (%q,%q) want (%q,%q)", f.creds.lastUsername, f.creds.lastPassword, mintTestUser, mintTestPasswd)
	}
	body := decodeMintBody(t, w)
	if body["token_type"] != "Bearer" {
		t.Errorf("token_type = %v", body["token_type"])
	}
	if body["expires_in"] != float64(3600) {
		t.Errorf("expires_in = %v want 3600", body["expires_in"])
	}
	if body["gcid"] != defaultVerifyResponse().GCID {
		t.Errorf("gcid = %v want %v", body["gcid"], defaultVerifyResponse().GCID)
	}
	if _, ok := body["memberships"].([]any); !ok {
		t.Errorf("memberships missing/not array: %v", body["memberships"])
	}
	claims := decodeJWTClaims(t, w)
	if claims["gcid"] != defaultVerifyResponse().GCID {
		t.Errorf("gcid claim = %v", claims["gcid"])
	}
	if claims["sub"] != defaultVerifyResponse().GCID {
		t.Errorf("sub claim = %v", claims["sub"])
	}
	if claims["tenant_id"] != mintDefaultTenantID {
		t.Errorf("tenant_id claim = %v want %v", claims["tenant_id"], mintDefaultTenantID)
	}
	if claims["email"] != mintTestEmail {
		t.Errorf("email claim = %v", claims["email"])
	}
	if claims["iss"] != mintTestIssuer || claims["aud"] != mintTestAud {
		t.Errorf("iss/aud = %v/%v", claims["iss"], claims["aud"])
	}
	if !rolesContain(jwtRoles(t, claims), "learner") {
		t.Errorf("roles = %v want learner", jwtRoles(t, claims))
	}
}

// The active tenant/roles come from the verify-credentials response, NOT from
// "the first membership". Here the first membership is a DIFFERENT tenant.
func TestMint_UsesAuthoritativeActiveTenantAndRoles(t *testing.T) {
	f := newMintFixtureWith(t, func(cfg *httpadapter.MintHandlerConfig) {
		cfg.Credentials = &stubCredentialsVerifier{resp: &clients.VerifyCredentialsResponse{
			GCID:              "01970000-0000-7000-8000-0000000000aa",
			Email:             mintTestEmail,
			ActiveTenantID:    mintOtherTenantID,
			ActiveTenantRoles: []string{"admin", "instructor"},
			Memberships: []clients.IdentityMembership{
				{TenantID: mintDefaultTenantID, Roles: []string{"learner"}},
				{TenantID: mintOtherTenantID, Roles: []string{"admin", "instructor"}},
			},
		}}
	})
	w := f.doRequest(t, map[string]any{"username": mintTestUser, "password": mintTestPasswd})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	claims := decodeJWTClaims(t, w)
	if claims["tenant_id"] != mintOtherTenantID {
		t.Errorf("tenant_id = %v want authoritative active tenant %v", claims["tenant_id"], mintOtherTenantID)
	}
	roles := jwtRoles(t, claims)
	if !rolesContain(roles, "admin") {
		t.Errorf("roles = %v want admin", roles)
	}
	// training_admin is derived from the instructor membership role.
	if !rolesContain(roles, "training_admin") {
		t.Errorf("roles = %v want derived training_admin", roles)
	}
	if rolesContain(roles, "learner") {
		t.Errorf("roles = %v must NOT include the non-active membership's learner role", roles)
	}
}

// platform_operator arrives as a membership role (no Secret Manager list).
func TestMint_PlatformOperatorMembership_WidensSurfaces(t *testing.T) {
	f := newMintFixtureWith(t, func(cfg *httpadapter.MintHandlerConfig) {
		cfg.Credentials = &stubCredentialsVerifier{resp: &clients.VerifyCredentialsResponse{
			GCID:              "01970000-0000-7000-8000-0000000000aa",
			Email:             mintTestEmail,
			ActiveTenantID:    mintDefaultTenantID,
			ActiveTenantRoles: []string{"platform_operator"},
			Memberships: []clients.IdentityMembership{
				{TenantID: mintDefaultTenantID, Roles: []string{"platform_operator"}},
			},
		}}
	})
	w := f.doRequest(t, map[string]any{"username": mintTestUser, "password": mintTestPasswd})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if !rolesContain(jwtRoles(t, decodeJWTClaims(t, w)), "platform_operator") {
		t.Errorf("platform_operator role missing from JWT")
	}
	body := decodeMintBody(t, w)
	ms, _ := body["memberships"].([]any)
	if len(ms) != 1 {
		t.Fatalf("memberships = %v", body["memberships"])
	}
	m0, _ := ms[0].(map[string]any)
	surfaces, _ := m0["surfaces"].([]any)
	if len(surfaces) != 5 {
		t.Errorf("operator membership surfaces = %v want full 5-surface rail", m0["surfaces"])
	}
}

// --- failure paths -----------------------------------------------------------

func TestMint_401_InvalidCredentials(t *testing.T) {
	f := newMintFixtureWith(t, func(cfg *httpadapter.MintHandlerConfig) {
		cfg.Credentials = &stubCredentialsVerifier{err: clients.ErrInvalidCredentials}
	})
	w := f.doRequest(t, map[string]any{"username": mintTestUser, "password": "wrong"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d want 401 body=%s", w.Code, w.Body.String())
	}
	if got := mintErrCode(t, w); got != "INVALID_CREDENTIALS" {
		t.Errorf("code = %q want INVALID_CREDENTIALS", got)
	}
}

func TestMint_503_UpstreamUnavailable(t *testing.T) {
	f := newMintFixtureWith(t, func(cfg *httpadapter.MintHandlerConfig) {
		cfg.Credentials = &stubCredentialsVerifier{err: clients.ErrCredentialsUpstreamUnavailable}
	})
	w := f.doRequest(t, map[string]any{"username": mintTestUser, "password": mintTestPasswd})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d want 503 body=%s", w.Code, w.Body.String())
	}
	if got := mintErrCode(t, w); got != "AUTH_IDENTITY_UPSTREAM_UNAVAILABLE" {
		t.Errorf("code = %q", got)
	}
}

func TestMint_502_IncompleteIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp *clients.VerifyCredentialsResponse
	}{
		{"missing gcid", &clients.VerifyCredentialsResponse{ActiveTenantID: mintDefaultTenantID}},
		{"missing tenant", &clients.VerifyCredentialsResponse{GCID: "g"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMintFixtureWith(t, func(cfg *httpadapter.MintHandlerConfig) {
				cfg.Credentials = &stubCredentialsVerifier{resp: tc.resp}
			})
			w := f.doRequest(t, map[string]any{"username": mintTestUser, "password": mintTestPasswd})
			if w.Code != http.StatusBadGateway {
				t.Fatalf("status = %d want 502 body=%s", w.Code, w.Body.String())
			}
			if got := mintErrCode(t, w); got != "AUTH_RESOLVE_INCONSISTENT" {
				t.Errorf("code = %q", got)
			}
		})
	}
}

func TestMint_400_MissingFields(t *testing.T) {
	f := newMintFixture(t)
	for _, body := range []map[string]any{
		{"password": mintTestPasswd},
		{"username": mintTestUser},
	} {
		w := f.doRequest(t, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body %v: status = %d want 400", body, w.Code)
		}
	}
}

func TestMint_400_InvalidJSON(t *testing.T) {
	f := newMintFixture(t)
	w := f.doRequest(t, "{not json")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want 400", w.Code)
	}
}

func TestMint_400_UnknownField(t *testing.T) {
	f := newMintFixture(t)
	w := f.doRequest(t, map[string]any{"username": mintTestUser, "password": mintTestPasswd, "legacy_token": "x"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want 400 (DisallowUnknownFields)", w.Code)
	}
}

func TestMint_405_MethodNotAllowed(t *testing.T) {
	f := newMintFixture(t)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session/mint", nil)
	w := httptest.NewRecorder()
	f.handler.Mint(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d want 405", w.Code)
	}
}

// --- constructor guards ------------------------------------------------------

func TestNewMintHandler_FailsLoudOnMissingCredentials(t *testing.T) {
	_, err := httpadapter.NewMintHandler(httpadapter.MintHandlerConfig{
		Identity:      &stubIdentityResolver{resp: defaultResolveResponse()},
		SessionSigner: []byte(mintTestSigner),
		SessionIssuer: mintTestIssuer,
		SessionAud:    mintTestAud,
	})
	if err == nil {
		t.Fatal("expected error when Credentials is nil")
	}
}

func TestNewMintHandler_FailsLoudOnMissingIdentity(t *testing.T) {
	_, err := httpadapter.NewMintHandler(httpadapter.MintHandlerConfig{
		Credentials:   &stubCredentialsVerifier{resp: defaultVerifyResponse()},
		SessionSigner: []byte(mintTestSigner),
		SessionIssuer: mintTestIssuer,
		SessionAud:    mintTestAud,
	})
	if err == nil {
		t.Fatal("expected error when Identity is nil")
	}
}

func TestNewMintHandler_FailsLoudOnShortSigner(t *testing.T) {
	_, err := httpadapter.NewMintHandler(httpadapter.MintHandlerConfig{
		Credentials:   &stubCredentialsVerifier{resp: defaultVerifyResponse()},
		Identity:      &stubIdentityResolver{resp: defaultResolveResponse()},
		SessionSigner: []byte("short"),
		SessionIssuer: mintTestIssuer,
		SessionAud:    mintTestAud,
	})
	if err == nil {
		t.Fatal("expected error on <32-byte signer")
	}
}
