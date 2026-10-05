// webauthn_handler_test.go — RED-phase TDD specs for the 4 WebAuthn BFF
// routes (auth-hardening Phase A4, ADR-181 D2, CHO-1718; contract:
// chora-contracts/openapi/auth-gateway.yaml).
//
//	POST /api/v1/auth/webauthn/register/begin   (AUTHED — passkey added to an existing account)
//	POST /api/v1/auth/webauthn/register/finish  (AUTHED)
//	POST /api/v1/auth/webauthn/login/begin      (anonymous)
//	POST /api/v1/auth/webauthn/login/finish     (anonymous → composes the mint pipeline)
//
// The gateway proxies the ceremonies to chora-identity's
// /v1/auth/passkey/{challenge,register,verify} routes and, on login/finish,
// runs the SAME resolve→roles→JWT mint flow the username/password path uses (shared
// MintHandler internals — allowlist/REGISTRATION_MODE gate + ADR-165
// operator stamping included).
//
// Reuses fixtures from mint_handler_test.go (MintHandler builder) and
// jwt_auth_test.go (chora-session signer) — same package.
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

	"github.com/apollo-chora/chora-common/auth/chorasession"
	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
)

// --- fake passkey backend ------------------------------------------------------

type fakePasskeyBackend struct {
	challengeResp *clients.PasskeyChallengeResponse
	challengeErr  error
	registerResp  *clients.PasskeyRegisterResponse
	registerErr   error
	verifyResp    *clients.PasskeyVerifyResponse
	verifyErr     error

	lastChallengeUserHandle string
	lastRegister            clients.PasskeyRegisterRequest
	lastVerify              clients.PasskeyVerifyRequest
}

func (f *fakePasskeyBackend) Challenge(_ context.Context, userHandle string) (*clients.PasskeyChallengeResponse, error) {
	f.lastChallengeUserHandle = userHandle
	if f.challengeErr != nil {
		return nil, f.challengeErr
	}
	return f.challengeResp, nil
}

func (f *fakePasskeyBackend) Register(_ context.Context, req clients.PasskeyRegisterRequest) (*clients.PasskeyRegisterResponse, error) {
	f.lastRegister = req
	if f.registerErr != nil {
		return nil, f.registerErr
	}
	return f.registerResp, nil
}

func (f *fakePasskeyBackend) Verify(_ context.Context, req clients.PasskeyVerifyRequest) (*clients.PasskeyVerifyResponse, error) {
	f.lastVerify = req
	if f.verifyErr != nil {
		return nil, f.verifyErr
	}
	return f.verifyResp, nil
}

func defaultChallengeResp() *clients.PasskeyChallengeResponse {
	return &clients.PasskeyChallengeResponse{
		ChallengeID: "ch-0001",
		Challenge:   "Y2hhbGxlbmdlLWJ5dGVzLTMyLWJ5dGVzLXBhZGRpbmc",
		RPID:        "chora.site",
		ExpiresAt:   "2026-06-11T12:05:00Z",
	}
}

// --- fixture ---------------------------------------------------------------------

const (
	waGCID  = "01970000-0000-7000-8000-0000000000aa"
	waEmail = "alice@example.com"
)

type webauthnFixture struct {
	passkey *fakePasskeyBackend
	mint    *mintFixture
	handler http.Handler // WithChoraSessionOnPrefixes(WithWebAuthnRoutes(404, wa))
}

// newWebAuthnFixture builds the full gate+routes tree. mintMutate customises
// the MintHandler config (registration mode, operator secret...). When
// withMint is false the WebAuthnHandler is built with a nil MintHandler.
func newWebAuthnFixture(t *testing.T, withMint bool, mintMutate func(*httpadapter.MintHandlerConfig)) *webauthnFixture {
	t.Helper()
	pk := &fakePasskeyBackend{
		challengeResp: defaultChallengeResp(),
		registerResp: &clients.PasskeyRegisterResponse{
			CredentialID: "Y3JlZC1pZA",
			GCID:         waGCID,
			CreatedAt:    "2026-06-11T12:00:00Z",
		},
		verifyResp: &clients.PasskeyVerifyResponse{
			GCID:  waGCID,
			Email: waEmail,
		},
	}

	mf := newMintFixtureWith(t, mintMutate)
	// Align the resolver's GCID with the verify response so the
	// verify→resolve consistency check passes on happy paths.
	mf.identity.resp.GCID = waGCID

	var mh *httpadapter.MintHandler
	if withMint {
		mh = mf.handler
	}
	wa, err := httpadapter.NewWebAuthnHandler(httpadapter.WebAuthnHandlerConfig{
		Passkey:       pk,
		Mint:          mh,
		CorrelationFn: func() string { return "wa-test-corr" },
	})
	if err != nil {
		t.Fatalf("NewWebAuthnHandler: %v", err)
	}

	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	routes := httpadapter.WithWebAuthnRoutes(base, wa)
	v, err := chorasession.NewValidator(chsTestSigner, chsTestIssuer, chsTestAudience)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return &webauthnFixture{
		passkey: pk,
		mint:    mf,
		handler: httpadapter.WithChoraSessionOnPrefixes(routes, v),
	}
}

func (f *webauthnFixture) sessionJWT(t *testing.T) string {
	t.Helper()
	return signChoraSession(t, chsTestSigner, validSessionClaims(time.Now().UTC(), map[string]any{
		"gcid":  waGCID,
		"email": waEmail,
	}))
}

func (f *webauthnFixture) do(t *testing.T, path string, body any, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body == nil {
		rd = strings.NewReader("")
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rd = strings.NewReader(string(b))
	}
	r := httptest.NewRequest(http.MethodPost, path, rd)
	r.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func decodeWABody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return out
}

func errCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	body := decodeWABody(t, w)
	e, _ := body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

// --- register/begin -----------------------------------------------------------------

func TestWebAuthn_RegisterBegin_RequiresSession(t *testing.T) {
	f := newWebAuthnFixture(t, true, nil)
	w := f.do(t, "/api/v1/auth/webauthn/register/begin", map[string]any{"gcid": waGCID}, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d want 401 (register/begin must be JWT-gated); body=%s", w.Code, w.Body.String())
	}
}

func TestWebAuthn_RegisterBegin_HappyPath(t *testing.T) {
	f := newWebAuthnFixture(t, true, nil)
	w := f.do(t, "/api/v1/auth/webauthn/register/begin",
		map[string]any{"gcid": waGCID}, f.sessionJWT(t))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	body := decodeWABody(t, w)
	if body["challenge_id"] != "ch-0001" {
		t.Errorf("challenge_id = %v want ch-0001", body["challenge_id"])
	}
	opts, _ := body["options"].(map[string]any)
	if opts == nil {
		t.Fatalf("missing options")
	}
	if opts["challenge"] != defaultChallengeResp().Challenge {
		t.Errorf("options.challenge = %v", opts["challenge"])
	}
	rp, _ := opts["rp"].(map[string]any)
	if rp == nil || rp["id"] != "chora.site" {
		t.Errorf("options.rp = %v want id chora.site", opts["rp"])
	}
	user, _ := opts["user"].(map[string]any)
	if user == nil || user["name"] != waEmail {
		t.Errorf("options.user = %v want name %s", opts["user"], waEmail)
	}
	if user["id"] == nil || user["id"] == "" {
		t.Errorf("options.user.id missing (must be base64url user handle)")
	}
	params, _ := opts["pubKeyCredParams"].([]any)
	if len(params) != 2 {
		t.Fatalf("pubKeyCredParams = %v want ES256 + RS256", opts["pubKeyCredParams"])
	}
	algs := map[float64]bool{}
	for _, p := range params {
		pm, _ := p.(map[string]any)
		a, _ := pm["alg"].(float64)
		algs[a] = true
	}
	if !algs[-7] || !algs[-257] {
		t.Errorf("pubKeyCredParams algs = %v want -7 and -257", algs)
	}
	// Challenge minted with the SESSION email as user handle.
	if f.passkey.lastChallengeUserHandle != waEmail {
		t.Errorf("challenge user_handle = %q want session email", f.passkey.lastChallengeUserHandle)
	}
}

func TestWebAuthn_RegisterBegin_GcidMismatch_403(t *testing.T) {
	f := newWebAuthnFixture(t, true, nil)
	w := f.do(t, "/api/v1/auth/webauthn/register/begin",
		map[string]any{"gcid": "01970000-0000-7000-8000-00000000beef"}, f.sessionJWT(t))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d want 403; body=%s", w.Code, w.Body.String())
	}
	if errCode(t, w) != "AUTH_GCID_MISMATCH" {
		t.Errorf("code = %q want AUTH_GCID_MISMATCH", errCode(t, w))
	}
}

// --- register/finish ----------------------------------------------------------------

func TestWebAuthn_RegisterFinish_RequiresSession(t *testing.T) {
	f := newWebAuthnFixture(t, true, nil)
	w := f.do(t, "/api/v1/auth/webauthn/register/finish", map[string]any{"gcid": waGCID}, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d want 401; body=%s", w.Code, w.Body.String())
	}
}

func TestWebAuthn_RegisterFinish_HappyPath(t *testing.T) {
	f := newWebAuthnFixture(t, true, nil)
	w := f.do(t, "/api/v1/auth/webauthn/register/finish", map[string]any{
		"gcid":         waGCID,
		"challenge_id": "ch-0001",
		"attestation_response": map[string]any{
			"id":    "Y3JlZC1pZA",
			"rawId": "Y3JlZC1pZA",
			"type":  "public-key",
			"response": map[string]any{
				"attestationObject": "YXR0LW9iag",
				"clientDataJSON":    "Y2xpZW50LWRhdGE",
			},
		},
	}, f.sessionJWT(t))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	body := decodeWABody(t, w)
	if body["id"] != "Y3JlZC1pZA" {
		t.Errorf("id = %v want credential id", body["id"])
	}
	if body["gcid"] != waGCID {
		t.Errorf("gcid = %v", body["gcid"])
	}
	if body["created_at"] == nil {
		t.Errorf("missing created_at")
	}
	// The proxy forwarded the ceremony fields to chora-identity.
	got := f.passkey.lastRegister
	if got.ChallengeID != "ch-0001" || got.GCID != waGCID ||
		got.CredentialID != "Y3JlZC1pZA" ||
		got.AttestationObject != "YXR0LW9iag" || got.ClientDataJSON != "Y2xpZW50LWRhdGE" {
		t.Errorf("upstream register request = %+v", got)
	}
}

func TestWebAuthn_RegisterFinish_MissingFields_400(t *testing.T) {
	f := newWebAuthnFixture(t, true, nil)
	w := f.do(t, "/api/v1/auth/webauthn/register/finish",
		map[string]any{"gcid": waGCID, "challenge_id": "ch-0001"}, f.sessionJWT(t))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestWebAuthn_RegisterFinish_UpstreamErrorPassthrough(t *testing.T) {
	f := newWebAuthnFixture(t, true, nil)
	f.passkey.registerErr = &clients.PasskeyUpstreamError{
		StatusCode: http.StatusConflict,
		Code:       "PASSKEY_CREDENTIAL_EXISTS",
		Message:    "credential is already registered",
	}
	w := f.do(t, "/api/v1/auth/webauthn/register/finish", map[string]any{
		"gcid":         waGCID,
		"challenge_id": "ch-0001",
		"attestation_response": map[string]any{
			"rawId": "Y3JlZC1pZA",
			"response": map[string]any{
				"attestationObject": "YXR0LW9iag",
				"clientDataJSON":    "Y2xpZW50LWRhdGE",
			},
		},
	}, f.sessionJWT(t))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d want 409; body=%s", w.Code, w.Body.String())
	}
	if errCode(t, w) != "PASSKEY_CREDENTIAL_EXISTS" {
		t.Errorf("code = %q want PASSKEY_CREDENTIAL_EXISTS", errCode(t, w))
	}
}

// --- login/begin ---------------------------------------------------------------------

func TestWebAuthn_LoginBegin_Anonymous_HappyPath(t *testing.T) {
	f := newWebAuthnFixture(t, true, nil)
	w := f.do(t, "/api/v1/auth/webauthn/login/begin", map[string]any{"email": waEmail}, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	body := decodeWABody(t, w)
	if body["challenge_id"] != "ch-0001" {
		t.Errorf("challenge_id = %v", body["challenge_id"])
	}
	opts, _ := body["options"].(map[string]any)
	if opts == nil || opts["rpId"] != "chora.site" {
		t.Errorf("options = %v want rpId chora.site", body["options"])
	}
	if opts["challenge"] != defaultChallengeResp().Challenge {
		t.Errorf("options.challenge = %v", opts["challenge"])
	}
	if f.passkey.lastChallengeUserHandle != waEmail {
		t.Errorf("user_handle = %q want %q", f.passkey.lastChallengeUserHandle, waEmail)
	}
}

func TestWebAuthn_LoginBegin_NoBody_OK(t *testing.T) {
	f := newWebAuthnFixture(t, true, nil)
	w := f.do(t, "/api/v1/auth/webauthn/login/begin", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if f.passkey.lastChallengeUserHandle != "" {
		t.Errorf("user_handle = %q want empty (discoverable)", f.passkey.lastChallengeUserHandle)
	}
}

// --- login/finish ----------------------------------------------------------------------

func loginFinishBody() map[string]any {
	return map[string]any{
		"challenge_id": "ch-0001",
		"assertion_response": map[string]any{
			"id":    "Y3JlZC1pZA",
			"rawId": "Y3JlZC1pZA",
			"type":  "public-key",
			"response": map[string]any{
				"authenticatorData": "YXV0aC1kYXRh",
				"clientDataJSON":    "Y2xpZW50LWRhdGE",
				"signature":         "c2lnbmF0dXJl",
				"userHandle":        nil,
			},
		},
	}
}

func TestWebAuthn_LoginFinish_ComposesMintPipeline(t *testing.T) {
	f := newWebAuthnFixture(t, true, nil)
	w := f.do(t, "/api/v1/auth/webauthn/login/finish", loginFinishBody(), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	body := decodeWABody(t, w)
	// Standard AuthTokenResponse — same shape the username/password mint emits.
	tok, _ := body["access_token"].(string)
	if strings.Count(tok, ".") != 2 {
		t.Fatalf("access_token not a JWT: %q", tok)
	}
	if body["token_type"] != "Bearer" {
		t.Errorf("token_type = %v", body["token_type"])
	}
	if body["gcid"] != waGCID {
		t.Errorf("gcid = %v want %s", body["gcid"], waGCID)
	}
	if _, ok := body["memberships"].([]any); !ok {
		t.Errorf("memberships missing/not array: %v", body["memberships"])
	}
	// JWT carries tenant + roles claims (the dev tenant-less token is dead).
	payload, err := decodeJWTPayloadSegment(tok)
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	if payload["tenant_id"] != mintDefaultTenantID {
		t.Errorf("tenant_id claim = %v want %s", payload["tenant_id"], mintDefaultTenantID)
	}
	if payload["email"] != waEmail {
		t.Errorf("email claim = %v", payload["email"])
	}
	roles, _ := payload["roles"].([]any)
	if len(roles) == 0 {
		t.Errorf("roles claim empty")
	}
	// The verify proxy got the assertion fields.
	got := f.passkey.lastVerify
	if got.ChallengeID != "ch-0001" || got.CredentialID != "Y3JlZC1pZA" ||
		got.AuthenticatorData != "YXV0aC1kYXRh" || got.ClientDataJSON != "Y2xpZW50LWRhdGE" ||
		got.Signature != "c2lnbmF0dXJl" {
		t.Errorf("upstream verify request = %+v", got)
	}
}

func TestWebAuthn_LoginFinish_ResolveGCIDMismatch_502(t *testing.T) {
	// identity verify says the credential belongs to GCID A but the resolve
	// pipeline lands on GCID B — minting B's session off A's signature would
	// be an account-confusion hole. Fail loud.
	f := newWebAuthnFixture(t, true, nil)
	f.mint.identity.resp.GCID = "01970000-0000-7000-8000-00000000dead"
	w := f.do(t, "/api/v1/auth/webauthn/login/finish", loginFinishBody(), "")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d want 502; body=%s", w.Code, w.Body.String())
	}
	if errCode(t, w) != "AUTH_RESOLVE_INCONSISTENT" {
		t.Errorf("code = %q want AUTH_RESOLVE_INCONSISTENT", errCode(t, w))
	}
}

func TestWebAuthn_LoginFinish_UpstreamRejectionPassthrough(t *testing.T) {
	f := newWebAuthnFixture(t, true, nil)
	f.passkey.verifyErr = &clients.PasskeyUpstreamError{
		StatusCode: http.StatusUnauthorized,
		Code:       "PASSKEY_SIGNATURE_REJECTED",
		Message:    "WebAuthn signature did not verify",
	}
	w := f.do(t, "/api/v1/auth/webauthn/login/finish", loginFinishBody(), "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d want 401; body=%s", w.Code, w.Body.String())
	}
	if errCode(t, w) != "PASSKEY_SIGNATURE_REJECTED" {
		t.Errorf("code = %q", errCode(t, w))
	}
}

func TestWebAuthn_LoginFinish_MintUnavailable_503(t *testing.T) {
	f := newWebAuthnFixture(t, false, nil) // nil MintHandler (MINT_DISABLED)
	w := f.do(t, "/api/v1/auth/webauthn/login/finish", loginFinishBody(), "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d want 503; body=%s", w.Code, w.Body.String())
	}
	if errCode(t, w) != "AUTH_MINT_UNAVAILABLE" {
		t.Errorf("code = %q want AUTH_MINT_UNAVAILABLE", errCode(t, w))
	}
}

func TestWebAuthn_LoginFinish_MissingAssertion_400(t *testing.T) {
	f := newWebAuthnFixture(t, true, nil)
	w := f.do(t, "/api/v1/auth/webauthn/login/finish", map[string]any{"challenge_id": "ch-0001"}, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want 400; body=%s", w.Code, w.Body.String())
	}
}

// --- composition --------------------------------------------------------------------------

func TestWithWebAuthnRoutes_NilHandlerPassesThrough(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := httpadapter.WithWebAuthnRoutes(base, nil)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/webauthn/login/begin", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTeapot {
		t.Fatalf("nil handler must pass through; got %d", w.Code)
	}
}

func TestWebAuthn_MethodNotAllowed(t *testing.T) {
	f := newWebAuthnFixture(t, true, nil)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/webauthn/login/begin", nil)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d want 405", w.Code)
	}
}

// decodeJWTPayloadSegment decodes the claims segment of a compact JWT.
func decodeJWTPayloadSegment(tok string) (map[string]any, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, &json.SyntaxError{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, err
	}
	return claims, nil
}
