// refresh_handler_test.go — specs for POST /api/v1/auth/session/refresh and
// POST /api/v1/auth/logout.
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

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/adapter/inmem"
	"github.com/apollo-chora/chora-gateway/internal/domain/session"
)

// refreshFixture wires a MintHandler + RefreshHandler over the same in-memory
// stores, mirroring cmd/server/main.go.
type refreshFixture struct {
	mint    *httpadapter.MintHandler
	refresh *httpadapter.RefreshHandler
	store   *inmem.RefreshStore
	creds   *stubCredentialsVerifier
}

func newRefreshFixture(t *testing.T) *refreshFixture {
	t.Helper()
	creds := &stubCredentialsVerifier{resp: defaultVerifyResponse()}
	identity := &stubIdentityResolver{resp: defaultResolveResponse()}
	store := inmem.NewRefreshStore()
	mint, err := httpadapter.NewMintHandler(httpadapter.MintHandlerConfig{
		Credentials:   creds,
		Identity:      identity,
		Sessions:      inmem.NewSessionRepository(),
		RefreshStore:  store,
		SessionSigner: []byte(mintTestSigner),
		SessionIssuer: mintTestIssuer,
		SessionAud:    mintTestAud,
		SessionTTL:    time.Hour,
	})
	if err != nil {
		t.Fatalf("NewMintHandler: %v", err)
	}
	refresh, err := httpadapter.NewRefreshHandler(httpadapter.RefreshHandlerConfig{
		Mint:  mint,
		Store: store,
	})
	if err != nil {
		t.Fatalf("NewRefreshHandler: %v", err)
	}
	return &refreshFixture{mint: mint, refresh: refresh, store: store, creds: creds}
}

// mintSession runs the password mint and returns the decoded 200 body.
func (f *refreshFixture) mintSession(t *testing.T) map[string]any {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/session/mint",
		strings.NewReader(`{"username":"`+mintTestUser+`","password":"`+mintTestPasswd+`"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.mint.Mint(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("mint status = %d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode mint body: %v", err)
	}
	return body
}

func postRefresh(t *testing.T, f *refreshFixture, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/session/refresh", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.refresh.Refresh(w, r)
	return w
}

func refreshErrCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	e, _ := body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func decodeRefreshBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return out
}

func jwtClaimsOf(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", token)
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

func TestRefresh_HappyPathRotatesSession(t *testing.T) {
	f := newRefreshFixture(t)
	mint := f.mintSession(t)
	oldRefresh, _ := mint["refresh_token"].(string)
	if oldRefresh == "" {
		t.Fatalf("mint response has no refresh_token: %v", mint)
	}
	if mint["refresh_expires_in"] != float64(7*24*3600) {
		t.Errorf("refresh_expires_in = %v want %d", mint["refresh_expires_in"], 7*24*3600)
	}

	w := postRefresh(t, f, `{"refresh_token":"`+oldRefresh+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	body := decodeRefreshBody(t, w)
	if body["token_type"] != "Bearer" {
		t.Errorf("token_type = %v", body["token_type"])
	}
	if body["expires_in"] != float64(3600) {
		t.Errorf("expires_in = %v want 3600", body["expires_in"])
	}
	newAccess, _ := body["access_token"].(string)
	newRefresh, _ := body["refresh_token"].(string)
	if newAccess == "" || newRefresh == "" || newRefresh == oldRefresh {
		t.Fatalf("rotation failed: access=%q refresh=%q oldRefresh=%q", newAccess, newRefresh, oldRefresh)
	}

	claims := jwtClaimsOf(t, newAccess)
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
	exp, _ := claims["exp"].(float64)
	if time.Until(time.Unix(int64(exp), 0)) < 50*time.Minute {
		t.Errorf("exp = %v — new access token should live ~1h", claims["exp"])
	}

	// Rotation: the presented refresh token is now revoked.
	w2 := postRefresh(t, f, `{"refresh_token":"`+oldRefresh+`"}`)
	if w2.Code != http.StatusUnauthorized || refreshErrCode(t, w2) != "AUTH_REFRESH_REVOKED" {
		t.Fatalf("replayed refresh token: status=%d code=%s", w2.Code, refreshErrCode(t, w2))
	}

	// The rotated token still works.
	w3 := postRefresh(t, f, `{"refresh_token":"`+newRefresh+`"}`)
	if w3.Code != http.StatusOK {
		t.Fatalf("rotated refresh failed: status=%d body=%s", w3.Code, w3.Body.String())
	}
}

func TestRefresh_UnknownToken(t *testing.T) {
	f := newRefreshFixture(t)
	w := postRefresh(t, f, `{"refresh_token":"does-not-exist"}`)
	if w.Code != http.StatusUnauthorized || refreshErrCode(t, w) != "AUTH_REFRESH_INVALID" {
		t.Fatalf("status=%d code=%s", w.Code, refreshErrCode(t, w))
	}
}

func TestRefresh_MissingToken(t *testing.T) {
	f := newRefreshFixture(t)
	w := postRefresh(t, f, `{}`)
	if w.Code != http.StatusBadRequest || refreshErrCode(t, w) != "AUTH_BAD_REQUEST" {
		t.Fatalf("status=%d code=%s", w.Code, refreshErrCode(t, w))
	}
	w2 := postRefresh(t, f, `not json`)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("malformed body status=%d", w2.Code)
	}
}

func TestRefresh_ExpiredRecord(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()
	if err := f.store.SaveRefresh(ctx, &session.RefreshRecord{
		Token:     "expired-token",
		Gcid:      defaultVerifyResponse().GCID,
		Email:     mintTestEmail,
		TenantID:  mintDefaultTenantID,
		Roles:     []string{"learner"},
		IssuedAt:  time.Now().UTC().Add(-8 * 24 * time.Hour),
		ExpiresAt: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seed expired record: %v", err)
	}
	w := postRefresh(t, f, `{"refresh_token":"expired-token"}`)
	if w.Code != http.StatusUnauthorized || refreshErrCode(t, w) != "AUTH_REFRESH_EXPIRED" {
		t.Fatalf("status=%d code=%s", w.Code, refreshErrCode(t, w))
	}
}

func TestRefresh_MethodNotAllowed(t *testing.T) {
	f := newRefreshFixture(t)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session/refresh", nil)
	w := httptest.NewRecorder()
	f.refresh.Refresh(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d want 405", w.Code)
	}
}
