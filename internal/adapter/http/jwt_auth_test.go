// jwt_auth_test.go — TDD specs for the production-ready ChoraSession JWT
// validation middleware in chora-gateway.
//
// Per S3.6 spec the BFF is the trust boundary. Inbound `/api/*` JWTs are
// CHORA SESSION TOKENS minted by POST /api/v1/auth/session/mint (NOT the
// identity tokens that arrived from chora-web's the identity provider
// sign-in). The mint endpoint validates the username/password token, then signs a
// Chora session JWT (HS256, claims: iss, aud, sub, gcid, tenant_id, email,
// role_summary, iat, exp). The session token is the canonical artifact for
// /api/* trust boundary checks.
//
// Per the 2026-05-14 probe-confirmed arch-gap: the previous
// the previous validator-backed middleware ran RS256
// verification on these HS256 session tokens, which always failed
// "unknown signing key (kid)". This file's tests gate
// the corrected ChoraSession HS256 path.
//
// We test:
//  1. Valid session JWT — accepted; claims attached to context.
//  2. Missing Authorization header — rejected 401.
//  3. Wrong scheme (Basic) — rejected 401.
//  4. Tampered signature — rejected 401.
//  5. Wrong audience — rejected 401.
//  6. Expired token — rejected 401.
//  7. Missing custom claims (gcid empty) — rejected 401.
//  8. Missing tenant_id — rejected 401.
//  9. Router integration — gated /api/* rejects, public /healthz passes.
//
// Strict TDD: tests rewritten BEFORE refactoring the middleware.
package httpadapter_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-common/auth/chorasession"
	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/adapter/inmem"
	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

const (
	chsTestIssuer   = "https://api.chora.site"
	chsTestAudience = "chora-489812"
)

var chsTestSigner = []byte("test-chora-session-signer-key-must-be-at-least-32-bytes-long")

// signChoraSession mints an HS256 Chora session JWT identical in shape to
// what mint_handler.go produces in production.
func signChoraSession(t *testing.T, key []byte, claims map[string]any) string {
	t.Helper()
	header := map[string]string{"alg": "HS256", "typ": "JWT"}
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(hb) + "." +
		base64.RawURLEncoding.EncodeToString(cb)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signing))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signing + "." + sig
}

// --- Test harness -----------------------------------------------------------

type jwtAuthFixture struct {
	validator *chorasession.Validator
	handler   http.Handler
	echoCalls *int
}

// newJWTAuthFixture wires the new RequireChoraSessionJWT middleware in front
// of a tiny echo handler. The echo handler reads the claims from context and
// writes them back as JSON so tests can verify propagation end-to-end.
func newJWTAuthFixture(t *testing.T) *jwtAuthFixture {
	t.Helper()
	v, err := chorasession.NewValidator(chsTestSigner, chsTestIssuer, chsTestAudience)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	calls := 0
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		c, ok := httpadapter.ChoraSessionClaimsFromContext(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		body := map[string]any{"claims_present": ok}
		if ok {
			body["gcid"] = c.GCID
			body["tenant_id"] = c.TenantID
			body["email"] = c.Email
			body["role_summary"] = c.RoleSummary
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	mw := httpadapter.RequireChoraSessionJWT(v)
	return &jwtAuthFixture{
		validator: v,
		handler:   mw(echo),
		echoCalls: &calls,
	}
}

func validSessionClaims(now time.Time, overrides map[string]any) map[string]any {
	c := map[string]any{
		"iss":          chsTestIssuer,
		"aud":          chsTestAudience,
		"sub":          "gcid-phyllis",
		"gcid":         "01970000-0000-7000-8000-0000000000aa",
		"tenant_id":    "01970000-0000-7000-8000-0000000000bb",
		"email":        "phyllis@mightymind.sg",
		"role_summary": "",
		"iat":          now.Add(-30 * time.Second).Unix(),
		"exp":          now.Add(time.Hour).Unix(),
	}
	for k, v := range overrides {
		c[k] = v
	}
	return c
}

func (f *jwtAuthFixture) validJWT(t *testing.T) string {
	t.Helper()
	return signChoraSession(t, chsTestSigner, validSessionClaims(time.Now().UTC(), nil))
}

// --- Tests ------------------------------------------------------------------

func TestRequireChoraSessionJWT_AcceptsValidToken(t *testing.T) {
	t.Parallel()
	f := newJWTAuthFixture(t)

	tok := f.validJWT(t)
	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["claims_present"] != true {
		t.Errorf("claims_present = %v, want true", got["claims_present"])
	}
	if got["gcid"] != "01970000-0000-7000-8000-0000000000aa" {
		t.Errorf("gcid = %v", got["gcid"])
	}
	if got["tenant_id"] != "01970000-0000-7000-8000-0000000000bb" {
		t.Errorf("tenant_id = %v", got["tenant_id"])
	}
	if got["email"] != "phyllis@mightymind.sg" {
		t.Errorf("email = %v", got["email"])
	}
}

func TestRequireChoraSessionJWT_RejectsMissingAuthorizationHeader(t *testing.T) {
	t.Parallel()
	f := newJWTAuthFixture(t)
	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); !strings.Contains(strings.ToLower(got), "bearer") {
		t.Errorf("WWW-Authenticate = %q; want Bearer realm", got)
	}
	if *f.echoCalls != 0 {
		t.Errorf("echo handler should NOT have been called")
	}
}

func TestRequireChoraSessionJWT_RejectsWrongScheme(t *testing.T) {
	t.Parallel()
	f := newJWTAuthFixture(t)
	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	r.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestRequireChoraSessionJWT_RejectsTamperedSignature(t *testing.T) {
	t.Parallel()
	f := newJWTAuthFixture(t)
	tok := f.validJWT(t)
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3-part JWT")
	}
	parts[2] = parts[2][:len(parts[2])-2] + "AA"
	tampered := strings.Join(parts, ".")

	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	r.Header.Set("Authorization", "Bearer "+tampered)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestRequireChoraSessionJWT_RejectsWrongAudience(t *testing.T) {
	t.Parallel()
	f := newJWTAuthFixture(t)
	tok := signChoraSession(t, chsTestSigner, validSessionClaims(time.Now().UTC(), map[string]any{
		"aud": "chora-other",
	}))
	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestRequireChoraSessionJWT_RejectsExpiredToken(t *testing.T) {
	t.Parallel()
	f := newJWTAuthFixture(t)
	past := time.Now().UTC().Add(-2 * time.Hour)
	tok := signChoraSession(t, chsTestSigner, validSessionClaims(past, map[string]any{
		"iat": past.Add(-time.Hour).Unix(),
		"exp": past.Unix(),
	}))
	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestRequireChoraSessionJWT_RejectsTokenMissingGCIDClaim(t *testing.T) {
	t.Parallel()
	f := newJWTAuthFixture(t)
	claims := validSessionClaims(time.Now().UTC(), nil)
	delete(claims, "gcid")
	tok := signChoraSession(t, chsTestSigner, claims)
	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for missing gcid claim", w.Code)
	}
}

// CHO-1653 — Phase 5 bootstrap-mode JWTs (CHO-1648) carry tenant_id=""
// so a user with no memberships can call /api/v1/tenants/bootstrap. The
// gateway's JWT middleware must accept these and let downstream handlers
// enforce tenant context. Pre-CHO-1653 this test asserted 401 for missing
// tenant_id; the strict gate defeated Phase 5 once CHO-1652 added the
// bootstrap route to DefaultJWTGatedPrefixes.
func TestRequireChoraSessionJWT_AcceptsTokenMissingTenantClaim_BootstrapMode(t *testing.T) {
	t.Parallel()
	f := newJWTAuthFixture(t)
	claims := validSessionClaims(time.Now().UTC(), nil)
	delete(claims, "tenant_id")
	tok := signChoraSession(t, chsTestSigner, claims)
	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (bootstrap-mode JWT accepted)", w.Code)
	}
}

// --- Mesh metadata propagation (Stage B) -----------------------------------

func TestRouterWithJWT_GatedAPIRoutesRejectMissingToken(t *testing.T) {
	t.Parallel()
	v, _ := chorasession.NewValidator(chsTestSigner, chsTestIssuer, chsTestAudience)

	routes := inmem.NewRouteRepository()
	sessions := inmem.NewSessionRepository()
	up := upstream.NewFakeUpstream()
	router := httpadapter.NewRouterWithPhyllisGraphQLAndChoraSession(
		routes, sessions, up, nil, nil, v,
	)

	r1 := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w1 := httptest.NewRecorder()
	router.ServeHTTP(w1, r1)
	if w1.Code != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", w1.Code)
	}

	r2 := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, r2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("/api/me without auth status = %d, want 401", w2.Code)
	}
}

func TestRouterWithJWT_NilValidatorBypassesGate(t *testing.T) {
	t.Parallel()
	routes := inmem.NewRouteRepository()
	sessions := inmem.NewSessionRepository()
	up := upstream.NewFakeUpstream()
	router := httpadapter.NewRouterWithPhyllisGraphQLAndChoraSession(
		routes, sessions, up, nil, nil, nil, // validator nil
	)

	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", w.Code)
	}
}

func TestRequireChoraSessionJWT_PropagatesMeshClaimsToContext(t *testing.T) {
	t.Parallel()
	v, _ := chorasession.NewValidator(chsTestSigner, chsTestIssuer, chsTestAudience)

	var capturedGCID, capturedTenant, capturedRoles string
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mc, ok := httpadapter.MeshClaimsFromContext(r.Context())
		if !ok {
			t.Errorf("MeshClaimsFromContext: not present")
		} else {
			capturedGCID = mc.GCID
			capturedTenant = mc.TenantID
			// chora-session role_summary is a plain string per Bucket 1
			// (downstream readers parse it as opaque); we keep the
			// servicemesh.MeshClaims role_summary as a map[string]any but
			// just check non-existence here.
			if mc.RoleSummary != nil {
				capturedRoles = fmt.Sprintf("%v", mc.RoleSummary)
			}
		}
		w.WriteHeader(http.StatusOK)
	})
	h := httpadapter.RequireChoraSessionJWT(v)(echo)

	tok := signChoraSession(t, chsTestSigner, validSessionClaims(time.Now().UTC(), map[string]any{
		"gcid":      "01970000-0000-7000-8000-0000000000cc",
		"tenant_id": "01970000-0000-7000-8000-0000000000dd",
	}))
	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	r = r.WithContext(context.Background())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if capturedGCID != "01970000-0000-7000-8000-0000000000cc" {
		t.Errorf("captured gcid = %q", capturedGCID)
	}
	if capturedTenant != "01970000-0000-7000-8000-0000000000dd" {
		t.Errorf("captured tenant_id = %q", capturedTenant)
	}
	_ = capturedRoles // role_summary is empty string in this fixture; just ensure no panic.
}

// --- WithChoraSessionOnPrefixes wrapper ------------------------------------

// TestWithChoraSessionOnPrefixes_GatesOnlyConfiguredPaths — the canonical
// wrapper that main.go uses. /api/me MUST be gated; /healthz MUST pass
// through unauthenticated.
func TestWithChoraSessionOnPrefixes_GatesOnlyConfiguredPaths(t *testing.T) {
	t.Parallel()
	v, err := chorasession.NewValidator(chsTestSigner, chsTestIssuer, chsTestAudience)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	wrapped := httpadapter.WithChoraSessionOnPrefixes(echo, v)

	// Unauthenticated /api/me → 401
	r1 := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	w1 := httptest.NewRecorder()
	wrapped.ServeHTTP(w1, r1)
	if w1.Code != http.StatusUnauthorized {
		t.Errorf("/api/me unauthed status = %d, want 401", w1.Code)
	}

	// Unauthenticated /healthz → 200 (public)
	r2 := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w2 := httptest.NewRecorder()
	wrapped.ServeHTTP(w2, r2)
	if w2.Code != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", w2.Code)
	}
}

// Regression (CHO-1612): the A+ open-course curriculum read
// /api/v1/me/courses/{id}/content MUST be JWT-gated so RequireChoraSessionJWT
// stamps tenant_id + gcid mesh claims — gatewayProxyAuthFromRequest reads them
// and gatewayproxy.call() forwards X-Tenant-Id + gcid to chora-consumption
// (whose extRequireContext 400s without them). The route was registered, in
// GatewayProxyPathPrefixes, and bridge-owned, but was MISSING from
// DefaultJWTGatedPrefixes — so the gate skipped it, mesh claims were empty, and
// the downstream returned 400 MISSING_CONTEXT. Mirrors the learning-paths sibling.
func TestWithChoraSessionOnPrefixes_GatesCourseContentRead(t *testing.T) {
	t.Parallel()
	v, err := chorasession.NewValidator(chsTestSigner, chsTestIssuer, chsTestAudience)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	wrapped := httpadapter.WithChoraSessionOnPrefixes(echo, v)

	// Unauthenticated GET /api/v1/me/courses/{id}/content → 401 (gated).
	// Pre-fix this passed through to the echo handler (200) because the path
	// was absent from DefaultJWTGatedPrefixes.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me/courses/e2ec0000-0000-7000-8000-0000000000c1/content", nil)
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("/api/v1/me/courses/{id}/content unauthed status = %d, want 401 (JWT-gated)", w.Code)
	}
}

// Epic-1b W8: the A+ Growth-Edges read + upload routes MUST be JWT-gated so
// RequireChoraSessionJWT stamps tenant_id + gcid mesh claims before the
// gatewayproxy bridge forwards X-Tenant-Id + lowercase gcid to chora-consumption
// (extRequireContext 400s MISSING_CONTEXT without them). The single
// /api/v1/me/growth-edges prefix must cover the list leaf + the /{id} +
// /uploads[/{id}] subtree. Mirrors the courses-content sibling.
func TestWithChoraSessionOnPrefixes_GatesGrowthEdges(t *testing.T) {
	t.Parallel()
	v, err := chorasession.NewValidator(chsTestSigner, chsTestIssuer, chsTestAudience)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	wrapped := httpadapter.WithChoraSessionOnPrefixes(echo, v)

	for _, path := range []string{
		"/api/v1/me/growth-edges",
		"/api/v1/me/growth-edges/019e30db-692f-7d10-8ce0-59669fe9298d",
		"/api/v1/me/growth-edges/uploads",
		"/api/v1/me/growth-edges/uploads/019e30db-692f-7d10-8ce0-59669fe9298d",
	} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s unauthed status = %d, want 401 (JWT-gated so mesh claims stamp)", path, w.Code)
		}
	}
}

// CHO-1652 — H+ Setup-Tenant Phase 3 BFF route /api/v1/tenants/bootstrap
// MUST be JWT-gated so RequireChoraSessionJWT stamps MeshClaims onto the
// context. authCtxFromRequest reads MeshClaims.GCID, and
// phyllis.call() forwards `gcid` to chora-tenancy (whose bootstrap
// handler 401s without it). Pre-fix the route was registered on the
// mux in phyllis_handler.go but missing from DefaultJWTGatedPrefixes —
// the gate skipped it, MeshClaims was empty, and chora-tenancy rejected
// the upstream request with `gateway_unauthenticated: X-GCID header
// required`. End-to-end form submission only began surfacing this once
// CHO-1651 fixed the FE FormGroup binding so the POST actually fired.
func TestDefaultJWTGatedPrefixes_CoversTenantsBootstrap(t *testing.T) {
	t.Parallel()
	const path = "/api/v1/tenants/bootstrap"
	covered := false
	for _, p := range httpadapter.DefaultJWTGatedPrefixes {
		if strings.HasPrefix(path, p) {
			covered = true
			break
		}
	}
	if !covered {
		t.Fatalf("DefaultJWTGatedPrefixes does not cover %q — RequireChoraSessionJWT would skip the request, MeshClaims would not stamp, and chora-tenancy would 401 the upstream call. Add the prefix to jwt_auth.go alongside the other Phase 3 entries.", path)
	}
}

// CHO-2040 R8-6 — the Virgin Proofing Test list route GET
// /api/v1/me/proofing-tests is in GatewayProxyPathPrefixes and bridge-owned,
// so it MUST also be in DefaultJWTGatedPrefixes — else RequireChoraSessionJWT
// skips it, MeshClaims never stamp, and chora-consumption's extRequireContext
// 400s MISSING_CONTEXT. Guards the exact class of bug the line-478 comment
// records for the /api/v1/me/courses/ sibling.
func TestDefaultJWTGatedPrefixes_CoversProofingTests(t *testing.T) {
	t.Parallel()
	const path = "/api/v1/me/proofing-tests"
	covered := false
	for _, p := range httpadapter.DefaultJWTGatedPrefixes {
		if strings.HasPrefix(path, p) {
			covered = true
			break
		}
	}
	if !covered {
		t.Fatalf("DefaultJWTGatedPrefixes does not cover %q — RequireChoraSessionJWT would skip the request, MeshClaims would not stamp, and chora-consumption would 400 MISSING_CONTEXT the upstream call. Add the prefix to jwt_auth.go alongside the other /api/v1/me/* entries.", path)
	}
}

// W6 StudentTranscript read-model — GET /api/v1/me/transcript is in
// GatewayProxyPathPrefixes and bridge-owned, so it MUST also be in
// DefaultJWTGatedPrefixes — else RequireChoraSessionJWT skips it, MeshClaims
// never stamp, and chora-consumption's extRequireContext 400s MISSING_CONTEXT.
// Mirrors TestDefaultJWTGatedPrefixes_CoversProofingTests.
func TestDefaultJWTGatedPrefixes_CoversMeTranscript(t *testing.T) {
	t.Parallel()
	const path = "/api/v1/me/transcript"
	covered := false
	for _, p := range httpadapter.DefaultJWTGatedPrefixes {
		if strings.HasPrefix(path, p) {
			covered = true
			break
		}
	}
	if !covered {
		t.Fatalf("DefaultJWTGatedPrefixes does not cover %q — RequireChoraSessionJWT would skip the request, MeshClaims would not stamp, and chora-consumption would 400 MISSING_CONTEXT the upstream call. Add the prefix to jwt_auth.go alongside the other /api/v1/me/* entries.", path)
	}
}

// W6 StudentTranscript read-model — GET /api/v1/transcript/by-assessments is
// in GatewayProxyPathPrefixes and bridge-owned, so it MUST also be in
// DefaultJWTGatedPrefixes — else RequireChoraSessionJWT skips it, MeshClaims
// (incl. Roles) never stamp, x-mesh-user-roles is empty downstream, and
// chora-consumption's fail-closed role gate (hasTranscriptAdminRole) 403s
// EVERY caller — even real instructors/admins — instead of just unauthorized
// ones.
func TestDefaultJWTGatedPrefixes_CoversTranscriptByAssessments(t *testing.T) {
	t.Parallel()
	const path = "/api/v1/transcript/by-assessments"
	covered := false
	for _, p := range httpadapter.DefaultJWTGatedPrefixes {
		if strings.HasPrefix(path, p) {
			covered = true
			break
		}
	}
	if !covered {
		t.Fatalf("DefaultJWTGatedPrefixes does not cover %q — RequireChoraSessionJWT would skip the request, MeshClaims (incl. Roles) would not stamp, and chora-consumption would 403 every caller. Add the prefix to jwt_auth.go.", path)
	}
}

// CHO-1655 — Setup Wizard Phase A BFF route /api/v1/tenants/me/branding
// MUST be JWT-gated so RequireChoraSessionJWT stamps MeshClaims onto
// the context. authCtxFromRequest reads MeshClaims.TenantID, and
// phyllis.call() forwards `X-Tenant-Id` to chora-tenancy (whose
// /me/branding handler 401s without it). Mirrors the CHO-1652 lesson:
// every Phyllis-fanned-out tenant-scoped route must be registered here
// before merge, or the gate skips and the upstream rejects.
func TestDefaultJWTGatedPrefixes_CoversTenantsMeBranding(t *testing.T) {
	t.Parallel()
	const path = "/api/v1/tenants/me/branding"
	covered := false
	for _, p := range httpadapter.DefaultJWTGatedPrefixes {
		if strings.HasPrefix(path, p) {
			covered = true
			break
		}
	}
	if !covered {
		t.Fatalf("DefaultJWTGatedPrefixes does not cover %q — RequireChoraSessionJWT would skip the request, MeshClaims would not stamp, and chora-tenancy would 401 the upstream call. Add the prefix to jwt_auth.go alongside the other Setup Wizard entries.", path)
	}
}

// TestWithChoraSessionOnPrefixes_NilValidatorPassesThrough — when the
// validator is nil (boot didn't construct one) the wrapper returns the
// underlying handler unchanged. Production runs WILL have a validator
// (boot env gate enforces) but this preserves the test/dev contract.
func TestWithChoraSessionOnPrefixes_NilValidatorPassesThrough(t *testing.T) {
	t.Parallel()
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := httpadapter.WithChoraSessionOnPrefixes(echo, nil)
	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("/api/me with nil validator status = %d, want 200 (passthrough)", w.Code)
	}
}

// --- D0.3: /api/catalog public exemption (Phyllis demo step 5) ---------------
//
// The Phyllis demo step 5 "public discovery" is anonymous by spec:
// GET /api/catalog must return 200 without any Authorization header.
// GET /api/catalog/<anything> (subtree) must also return 200 unauthenticated.
//
// At the same time, a known gated prefix such as /api/atoms/new MUST still
// return 401 without a JWT — this guards against accidentally un-gating too
// much. Both tests use WithChoraSessionOnPrefixes (the canonical wrapper that
// main.go wires) so they cover the same code path the production gateway uses.

// TestWithChoraSessionOnPrefixes_CatalogIsPublicUnauthed asserts that
// /api/catalog and /api/catalog/* pass through WITHOUT a JWT.
func TestWithChoraSessionOnPrefixes_CatalogIsPublicUnauthed(t *testing.T) {
	t.Parallel()
	v, err := chorasession.NewValidator(chsTestSigner, chsTestIssuer, chsTestAudience)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	wrapped := httpadapter.WithChoraSessionOnPrefixes(echo, v)

	paths := []string{
		"/api/catalog",
		"/api/catalog/",
		"/api/catalog/abc123",
		"/api/catalog/atoms?q=go",
	}
	for _, p := range paths {
		p := p
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, p, nil)
			// No Authorization header — anonymous request.
			w := httptest.NewRecorder()
			wrapped.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Errorf("GET %s (unauthed) status = %d, want 200 (public path)", p, w.Code)
			}
		})
	}
}

// TestWithChoraSessionOnPrefixes_AtomsNewRemainsGated asserts that
// /api/atoms/new (a gated BFF path) still returns 401 without a JWT.
// This is a regression guard — ensures D0.3 did not accidentally un-gate
// more than /api/catalog.
func TestWithChoraSessionOnPrefixes_AtomsNewRemainsGated(t *testing.T) {
	t.Parallel()
	v, err := chorasession.NewValidator(chsTestSigner, chsTestIssuer, chsTestAudience)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	wrapped := httpadapter.WithChoraSessionOnPrefixes(echo, v)

	r := httptest.NewRequest(http.MethodGet, "/api/atoms/new", nil)
	// No Authorization header — should be rejected.
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/atoms/new (unauthed) status = %d, want 401 (gated path)", w.Code)
	}
}

// --- ADR-168 WS auth: browser WS handshake carries the JWT in a query param --
//
// Browsers cannot set the Authorization header on a native WebSocket handshake
// (the WebSocket constructor takes only a URL + subprotocols per RFC 6455). The
// gate therefore accepts the session JWT via the `access_token` query param FOR
// WS-UPGRADE REQUESTS ONLY — restricting it to upgrades keeps REST tokens out
// of URLs / access logs.

// TestRequireChoraSessionJWT_AcceptsWSUpgradeTokenViaQueryParam — a WS-upgrade
// request with the token in ?access_token= and NO Authorization header is
// accepted and stamps claims.
func TestRequireChoraSessionJWT_AcceptsWSUpgradeTokenViaQueryParam(t *testing.T) {
	t.Parallel()
	f := newJWTAuthFixture(t)
	tok := f.validJWT(t)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/live-quizzes/sess-1/ws?access_token="+tok, nil)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	// NO Authorization header — emulates a browser WebSocket handshake.
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (WS upgrade token via query param); body=%s", w.Code, w.Body.String())
	}
	if *f.echoCalls != 1 {
		t.Errorf("echo handler call count = %d, want 1", *f.echoCalls)
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["claims_present"] != true {
		t.Errorf("claims_present = %v, want true", got["claims_present"])
	}
}

// TestRequireChoraSessionJWT_RejectsQueryParamTokenForNonUpgrade — a regular
// (non-upgrade) request carrying ?access_token= is still 401. Guards against
// honouring query-param tokens on REST routes (token-in-URL leakage).
func TestRequireChoraSessionJWT_RejectsQueryParamTokenForNonUpgrade(t *testing.T) {
	t.Parallel()
	f := newJWTAuthFixture(t)
	tok := f.validJWT(t)
	r := httptest.NewRequest(http.MethodGet, "/api/me?access_token="+tok, nil)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (query-param token only valid for WS upgrades)", w.Code)
	}
	if *f.echoCalls != 0 {
		t.Errorf("echo handler should NOT be called; got %d calls", *f.echoCalls)
	}
}

// TestRequireChoraSessionJWT_RejectsWSUpgradeWithoutToken — a WS upgrade with
// neither an Authorization header nor an access_token query param is 401.
func TestRequireChoraSessionJWT_RejectsWSUpgradeWithoutToken(t *testing.T) {
	t.Parallel()
	f := newJWTAuthFixture(t)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/live-quizzes/sess-1/ws", nil)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (WS upgrade with no token)", w.Code)
	}
}

// TestWithChoraSessionOnPrefixes_GatesClassroomRealtimeSubtrees — the ADR-168
// classroom realtime subtrees (live-quizzes / live-polls / classroom-sessions)
// are now JWT-gated. Before this entry the WS fan-out was reachable by any
// caller who knew a session UUID, across tenant boundaries.
func TestWithChoraSessionOnPrefixes_GatesClassroomRealtimeSubtrees(t *testing.T) {
	t.Parallel()
	v, err := chorasession.NewValidator(chsTestSigner, chsTestIssuer, chsTestAudience)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	wrapped := httpadapter.WithChoraSessionOnPrefixes(echo, v)

	for _, p := range []string{
		"/api/v1/live-quizzes",
		"/api/v1/live-quizzes/abc123/ws",
		"/api/v1/live-polls/abc123/ws",
		"/api/v1/classroom-sessions/xyz",
	} {
		p := p
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, p, nil)
			// No Authorization header — unauthenticated.
			w := httptest.NewRecorder()
			wrapped.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("GET %s (unauthed) status = %d, want 401 (gated subtree)", p, w.Code)
			}
		})
	}
}

// TestWithChoraSessionOnPrefixes_GatesRplusVerticalSubtrees — the R+ Stage
// C-lite training-admin verticals (surveys / wbl-placements / rosters / exams /
// project-groups / skillsfutures-claims) are now JWT-gated. Before this, they
// were proxied to chora-delivery but ungated, so the gateway never stamped the
// X-Tenant-Id/gcid mesh-trust headers the downstream tenantRequired needs.
func TestWithChoraSessionOnPrefixes_GatesRplusVerticalSubtrees(t *testing.T) {
	t.Parallel()
	v, err := chorasession.NewValidator(chsTestSigner, chsTestIssuer, chsTestAudience)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	wrapped := httpadapter.WithChoraSessionOnPrefixes(echo, v)

	for _, p := range []string{
		"/api/v1/surveys",
		"/api/v1/surveys/abc",
		"/api/v1/wbl-placements",
		"/api/v1/wbl-placements/abc",
		"/api/v1/rosters/course-1",
		"/api/v1/exams",
		"/api/v1/project-groups/pg-1",
		"/api/v1/skillsfutures-claims/sf-1",
	} {
		p := p
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, p, nil)
			w := httptest.NewRecorder()
			wrapped.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("GET %s (unauthed) status = %d, want 401 (gated subtree)", p, w.Code)
			}
		})
	}
}

// B-lite.2 relationship subtree — every /v1/connections/* write + pending
// read rides the existing "/v1/connections" gated prefix (HasPrefix match).
// If that entry were ever narrowed or removed, MeshClaims would not stamp
// and chora-sharing's requireIdentity would 400 every relationship op.
// Mirrors TestDefaultJWTGatedPrefixes_CoversProofingTests.
func TestDefaultJWTGatedPrefixes_CoversConnectionsSubtree(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"/v1/connections",
		"/v1/connections/follows",
		"/v1/connections/friend-requests/g-1/accept",
	} {
		covered := false
		for _, p := range httpadapter.DefaultJWTGatedPrefixes {
			if strings.HasPrefix(path, p) {
				covered = true
				break
			}
		}
		if !covered {
			t.Fatalf("DefaultJWTGatedPrefixes does not cover %q — RequireChoraSessionJWT would skip the request, MeshClaims would not stamp, and chora-sharing requireIdentity would 400 the relationship op.", path)
		}
	}
}

// Silences "imported and not used" if a future refactor drops fmt usage.
var _ = fmt.Sprintf
