package httpadapter_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/adapter/inmem"
	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
	"github.com/apollo-chora/chora-gateway/internal/domain/session"
)

// fakeJWT is a 3-segment placeholder JWT used by tests that mint a session.
const (
	fakeJWT      = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ0ZXN0In0.signature"
	fakeAdminJWT = "eyJhbGciOiJIUzI1NiJ9.adminPayload.signature"
)

// newServer wires the standard skeleton stack with optional FailMethods.
func newServer(t *testing.T, failMethods map[string]bool) (handler *httptest.Server, sessionsRepo *inmem.SessionRepository) {
	t.Helper()
	routesRepo := inmem.NewRouteRepository()
	sessionsRepo = inmem.NewSessionRepository()
	up := upstream.NewFakeUpstream()
	if failMethods != nil {
		up.FailMethods = failMethods
	}
	router := httpadapter.NewRouter(routesRepo, sessionsRepo, up)
	return httptest.NewServer(router), sessionsRepo
}

// mintSession is a helper that registers a session via the public POST endpoint.
func mintSession(t *testing.T, srv *httptest.Server, jwt string) string {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/api/auth/session", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("session mint err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("session mint status = %d; want 201", resp.StatusCode)
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	return body.Token
}

// -----------------------------------------------------------------------------
// Health + readiness + version
// -----------------------------------------------------------------------------

func TestHealthz(t *testing.T) {
	srv, _ := newServer(t, nil)
	defer srv.Close()
	for _, p := range []string{"/healthz", "/healthz/", "/health"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("%s err: %v", p, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d; want 200", p, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

func TestReadyz(t *testing.T) {
	srv, _ := newServer(t, nil)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ready" {
		t.Errorf("status = %v", body["status"])
	}
}

func TestVersion(t *testing.T) {
	srv, _ := newServer(t, nil)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/version")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestIndex(t *testing.T) {
	srv, _ := newServer(t, nil)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

// -----------------------------------------------------------------------------
// Trace context propagation
// -----------------------------------------------------------------------------

func TestTraceparent_GeneratedWhenMissing(t *testing.T) {
	srv, _ := newServer(t, nil)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("traceparent"); got == "" {
		t.Error("expected traceparent response header to be set")
	}
}

func TestTraceparent_PreservedFromInbound(t *testing.T) {
	srv, _ := newServer(t, nil)
	defer srv.Close()
	in := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/healthz", nil)
	req.Header.Set("traceparent", in)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("traceparent"); got != in {
		t.Errorf("traceparent = %q; want %q", got, in)
	}
}

// -----------------------------------------------------------------------------
// Session: mint + sign-out
// -----------------------------------------------------------------------------

func TestMintSession_InvalidJWT(t *testing.T) {
	srv, _ := newServer(t, nil)
	defer srv.Close()
	cases := []string{"", "two.parts", "four.parts.too.many"}
	for _, jwt := range cases {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/api/auth/session", nil)
		if jwt != "" {
			req.Header.Set("Authorization", "Bearer "+jwt)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("jwt=%q status=%d; want 401", jwt, resp.StatusCode)
		}
	}
}

func TestMintSession_Valid(t *testing.T) {
	srv, _ := newServer(t, nil)
	defer srv.Close()
	tok := mintSession(t, srv, fakeJWT)
	if tok == "" {
		t.Error("expected non-empty token")
	}
}

func TestSignOut_Idempotent(t *testing.T) {
	srv, _ := newServer(t, nil)
	defer srv.Close()
	// No bearer present.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete, srv.URL+"/api/auth/session", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d; want 204", resp.StatusCode)
	}
}

func TestSession_MethodNotAllowed(t *testing.T) {
	srv, _ := newServer(t, nil)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/auth/session")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405", resp.StatusCode)
	}
}

// -----------------------------------------------------------------------------
// Auth enforcement
// -----------------------------------------------------------------------------

func TestAplusHome_Public_NoSession_OK(t *testing.T) {
	srv, _ := newServer(t, nil)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/bff/aplus/home")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("public guest preview status = %d; want 200", resp.StatusCode)
	}
}

func TestCplusFeed_Auth_NoSession_401(t *testing.T) {
	srv, _ := newServer(t, nil)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/bff/cplus/feed")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401", resp.StatusCode)
	}
}

func TestCplusFeed_Auth_WithSession_200(t *testing.T) {
	srv, _ := newServer(t, nil)
	defer srv.Close()
	tok := mintSession(t, srv, fakeJWT)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/bff/cplus/feed", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
}

func TestHplusTenant_Admin_NonAdminSession_403(t *testing.T) {
	srv, _ := newServer(t, nil)
	defer srv.Close()
	tok := mintSession(t, srv, fakeJWT) // learner role
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/bff/hplus/tenant", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d; want 403", resp.StatusCode)
	}
}

func TestHplusTenant_Admin_AdminSession_200(t *testing.T) {
	srv, _ := newServer(t, nil)
	defer srv.Close()
	tok := mintSession(t, srv, fakeAdminJWT)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/bff/hplus/tenant", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
}

func TestUnknownPath_404(t *testing.T) {
	srv, _ := newServer(t, nil)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/bff/no-such-surface")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d; want 404", resp.StatusCode)
	}
}

// -----------------------------------------------------------------------------
// Composition: graceful degradation
// -----------------------------------------------------------------------------

func TestAplusHome_Composition_AllOK(t *testing.T) {
	srv, _ := newServer(t, nil)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/bff/aplus/home")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("status = %v; want ok", body["status"])
	}
	parts, _ := body["parts"].(map[string]any)
	for _, key := range []string{"learning_path", "recent_atoms", "companion"} {
		if _, ok := parts[key]; !ok {
			t.Errorf("expected part %q present", key)
		}
	}
	errs, _ := body["part_errors"].(map[string]any)
	if len(errs) != 0 {
		t.Errorf("expected no part_errors, got %v", errs)
	}
}

func TestAplusHome_GracefulDegradation_OneUpstreamFails(t *testing.T) {
	srv, _ := newServer(t, map[string]bool{"GetRecentAtoms": true})
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/bff/aplus/home")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200 even when one upstream fails", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "partial" {
		t.Errorf("status = %v; want partial", body["status"])
	}
	parts, _ := body["parts"].(map[string]any)
	if _, ok := parts["learning_path"]; !ok {
		t.Error("expected successful part learning_path to remain")
	}
	if _, ok := parts["companion"]; !ok {
		t.Error("expected successful part companion to remain")
	}
	if _, ok := parts["recent_atoms"]; ok {
		t.Error("did not expect failed part to be present")
	}
	errs, _ := body["part_errors"].(map[string]any)
	if _, ok := errs["recent_atoms"]; !ok {
		t.Error("expected recent_atoms error marker")
	}
}

func TestOplusGovernance_GracefulDegradation_Multi(t *testing.T) {
	srv, _ := newServer(t, map[string]bool{"GetAuditEvents": true})
	defer srv.Close()
	tok := mintSession(t, srv, fakeAdminJWT)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/bff/oplus/governance", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()

	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "partial" {
		t.Errorf("status = %v; want partial", body["status"])
	}
}

func TestRplusCourses_NotInstructor_StillAuthOK(t *testing.T) {
	srv, _ := newServer(t, nil)
	defer srv.Close()
	tok := mintSession(t, srv, fakeJWT)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/bff/rplus/courses", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
}

// -----------------------------------------------------------------------------
// Generic proxy: header stamping
// -----------------------------------------------------------------------------

func TestGenericProxy_StampsGcidAndTenant(t *testing.T) {
	srv, _ := newServer(t, nil)
	defer srv.Close()
	tok := mintSession(t, srv, fakeJWT)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/proxy/chora-creation/atoms", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	stamped, ok := body["stamped_headers"].(map[string]any)
	if !ok {
		t.Fatalf("missing stamped_headers")
	}
	if got, _ := stamped["gcid"].(string); !strings.HasPrefix(got, "gcid-skel-") {
		t.Errorf("gcid header = %q; expected prefix gcid-skel-", got)
	}
	if got, _ := stamped["X-Tenant-Id"].(string); !strings.HasPrefix(got, "tenant-skel-") {
		t.Errorf("X-Tenant-Id header = %q; expected prefix tenant-skel-", got)
	}
	if got, _ := stamped["traceparent"].(string); got == "" {
		t.Error("expected traceparent stamped")
	}
}

func TestGenericProxy_WithoutSession_401(t *testing.T) {
	srv, _ := newServer(t, nil)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/proxy/chora-creation/atoms")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401 on unauthenticated proxy", resp.StatusCode)
	}
}

// -----------------------------------------------------------------------------
// Method enforcement
// -----------------------------------------------------------------------------

func TestAplusHome_PostNotAllowed(t *testing.T) {
	srv, _ := newServer(t, nil)
	defer srv.Close()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/bff/aplus/home", strings.NewReader(""))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405", resp.StatusCode)
	}
}

// -----------------------------------------------------------------------------
// Expired session = 401 on protected
// -----------------------------------------------------------------------------

func TestExpiredSession_401(t *testing.T) {
	srv, sessionsRepo := newServer(t, nil)
	defer srv.Close()
	tok := mintSession(t, srv, fakeJWT)
	// Force-expire.
	s, err := sessionsRepo.Get(context.Background(), tok)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	s.ExpiresAt = s.IssuedAt.Add(-1)
	_ = sessionsRepo.Save(context.Background(), s)
	// Sanity check the helper recognises expiry.
	if !(&session.Session{ExpiresAt: s.ExpiresAt}).IsExpired() {
		t.Error("session should now be expired")
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/bff/cplus/feed", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401 on expired session", resp.StatusCode)
	}
}
