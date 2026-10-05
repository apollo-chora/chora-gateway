// identity_resolve_client_test.go — Bucket 3 client coverage.
//
// The IdentityResolveClient is the gateway-side HTTP client adapter for
// chora-identity's POST /v1/identity/resolve. It supersedes the demo
// deriveGCID(email) UUIDv5 stub in mint_handler.go.
//
// Tests use httptest.Server for the chora-identity stub — exercises the
// HTTP wire (request body shape + response decode + error mapping)
// without spinning up the real service.
package clients_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
)

func newStubIdentityServer(t *testing.T, status int, body any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/v1/identity/resolve" {
			http.Error(w, "wrong path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestIdentityResolveClient_HappyPath(t *testing.T) {
	t.Parallel()
	srv := newStubIdentityServer(t, http.StatusOK, map[string]any{
		"gcid":              "01970000-0000-7000-8000-0000000000aa",
		"default_tenant_id": "01970000-0000-7000-8000-0000000000bb",
		"email":             "alice@example.com",
		"subject":           "subject-1",
		"memberships": []map[string]any{{
			"tenant_id":   "01970000-0000-7000-8000-0000000000bb",
			"tenant_slug": "academy-A",
			"roles":       []string{"learner"},
			"surfaces":    []string{"aplus", "cplus"},
			"is_default":  true,
		}},
	})
	c, err := clients.NewIdentityResolveClient(clients.IdentityResolveClientConfig{
		BaseURL: srv.URL,
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewIdentityResolveClient: %v", err)
	}
	out, err := c.Resolve(context.Background(), clients.ResolveRequest{
		Email:   "alice@example.com",
		Subject: "subject-1",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if out.GCID != "01970000-0000-7000-8000-0000000000aa" {
		t.Errorf("GCID = %q", out.GCID)
	}
	if out.DefaultTenantID != "01970000-0000-7000-8000-0000000000bb" {
		t.Errorf("DefaultTenantID = %q", out.DefaultTenantID)
	}
	if len(out.Memberships) != 1 {
		t.Fatalf("Memberships len = %d, want 1", len(out.Memberships))
	}
	if got, want := out.Memberships[0].Roles, []string{"learner"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("roles = %v want %v", got, want)
	}
}

func TestIdentityResolveClient_404_NoMemberships(t *testing.T) {
	t.Parallel()
	srv := newStubIdentityServer(t, http.StatusNotFound, map[string]any{
		"code":    "IDENTITY_NO_TENANT_MEMBERSHIP",
		"message": "no tenants",
	})
	c, _ := clients.NewIdentityResolveClient(clients.IdentityResolveClientConfig{
		BaseURL: srv.URL,
		Timeout: 5 * time.Second,
	})
	_, err := c.Resolve(context.Background(), clients.ResolveRequest{
		Email:   "alice@example.com",
		Subject: "subject-1",
	})
	if err == nil {
		t.Fatalf("expected error for 404")
	}
	if !errors.Is(err, clients.ErrNoTenantMembership) {
		t.Errorf("expected ErrNoTenantMembership, got %v", err)
	}
	// A GENUINE no-membership 404 must NOT be conflated with a route-missing
	// 404 — the two have different operational meanings + remediation.
	if errors.Is(err, clients.ErrIdentityResolveRouteMissing) {
		t.Errorf("genuine no-membership 404 must NOT map to ErrIdentityResolveRouteMissing, got %v", err)
	}
}

// TestIdentityResolveClient_404_HTMLBody_IsRouteMissing — an Envoy / infra
// 404 (no route, mesh misconfig, path-prefix issue) has an HTML or
// non-JSON body. The client MUST NOT silently claim "user has no tenant
// membership" — that is a different failure with a different remediation.
// It must surface a distinct loud error so the gateway returns 503, not a
// misleading 403 AUTH_NO_TENANT_MEMBERSHIP. (Latent conflation bug fixed
// alongside D0.1c.)
func TestIdentityResolveClient_404_HTMLBody_IsRouteMissing(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<html><body>404 Not Found</body></html>"))
	}))
	t.Cleanup(srv.Close)
	c, _ := clients.NewIdentityResolveClient(clients.IdentityResolveClientConfig{
		BaseURL: srv.URL,
		Timeout: 5 * time.Second,
	})
	_, err := c.Resolve(context.Background(), clients.ResolveRequest{
		Email:   "alice@example.com",
		Subject: "subject-1",
	})
	if err == nil {
		t.Fatalf("expected error for HTML 404")
	}
	if errors.Is(err, clients.ErrNoTenantMembership) {
		t.Errorf("an HTML/infra 404 must NOT map to ErrNoTenantMembership (conflation bug), got %v", err)
	}
	if !errors.Is(err, clients.ErrIdentityResolveRouteMissing) {
		t.Errorf("expected ErrIdentityResolveRouteMissing for HTML 404, got %v", err)
	}
}

// TestIdentityResolveClient_404_UnrecognizedCode_IsRouteMissing — a JSON
// 404 whose body code is NOT the recognised IDENTITY_NO_TENANT_MEMBERSHIP
// sentinel is treated as an unexpected upstream 404, not a no-membership.
func TestIdentityResolveClient_404_UnrecognizedCode_IsRouteMissing(t *testing.T) {
	t.Parallel()
	srv := newStubIdentityServer(t, http.StatusNotFound, map[string]any{
		"code":    "SOME_OTHER_404",
		"message": "not the no-membership case",
	})
	c, _ := clients.NewIdentityResolveClient(clients.IdentityResolveClientConfig{
		BaseURL: srv.URL,
		Timeout: 5 * time.Second,
	})
	_, err := c.Resolve(context.Background(), clients.ResolveRequest{
		Email:   "alice@example.com",
		Subject: "subject-1",
	})
	if err == nil {
		t.Fatalf("expected error for unrecognized 404 code")
	}
	if errors.Is(err, clients.ErrNoTenantMembership) {
		t.Errorf("an unrecognized-code 404 must NOT map to ErrNoTenantMembership, got %v", err)
	}
	if !errors.Is(err, clients.ErrIdentityResolveRouteMissing) {
		t.Errorf("expected ErrIdentityResolveRouteMissing for unrecognized 404 code, got %v", err)
	}
}

// TestIdentityResolveClient_404_EmptyBody_IsRouteMissing — a bodyless 404
// (the bare Envoy NR case) is also an unexpected upstream 404.
func TestIdentityResolveClient_404_EmptyBody_IsRouteMissing(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	c, _ := clients.NewIdentityResolveClient(clients.IdentityResolveClientConfig{
		BaseURL: srv.URL,
		Timeout: 5 * time.Second,
	})
	_, err := c.Resolve(context.Background(), clients.ResolveRequest{
		Email:   "alice@example.com",
		Subject: "subject-1",
	})
	if err == nil {
		t.Fatalf("expected error for empty-body 404")
	}
	if errors.Is(err, clients.ErrNoTenantMembership) {
		t.Errorf("an empty-body 404 must NOT map to ErrNoTenantMembership, got %v", err)
	}
	if !errors.Is(err, clients.ErrIdentityResolveRouteMissing) {
		t.Errorf("expected ErrIdentityResolveRouteMissing for empty-body 404, got %v", err)
	}
}

func TestIdentityResolveClient_503_TenancyUnavailable(t *testing.T) {
	t.Parallel()
	srv := newStubIdentityServer(t, http.StatusServiceUnavailable, map[string]any{
		"code":    "IDENTITY_TENANCY_UNAVAILABLE",
		"message": "tenancy gRPC down",
	})
	c, _ := clients.NewIdentityResolveClient(clients.IdentityResolveClientConfig{
		BaseURL: srv.URL,
		Timeout: 5 * time.Second,
	})
	_, err := c.Resolve(context.Background(), clients.ResolveRequest{
		Email:   "alice@example.com",
		Subject: "subject-1",
	})
	if err == nil {
		t.Fatalf("expected error for 503")
	}
	if !errors.Is(err, clients.ErrIdentityUpstreamUnavailable) {
		t.Errorf("expected ErrIdentityUpstreamUnavailable, got %v", err)
	}
}

func TestIdentityResolveClient_500_InternalError(t *testing.T) {
	t.Parallel()
	srv := newStubIdentityServer(t, http.StatusInternalServerError, map[string]any{
		"code":    "IDENTITY_REPO_ERROR",
		"message": "pgx pool dead",
	})
	c, _ := clients.NewIdentityResolveClient(clients.IdentityResolveClientConfig{
		BaseURL: srv.URL,
		Timeout: 5 * time.Second,
	})
	_, err := c.Resolve(context.Background(), clients.ResolveRequest{
		Email:   "alice@example.com",
		Subject: "subject-1",
	})
	if err == nil {
		t.Fatalf("expected error for 500")
	}
}

func TestNewIdentityResolveClient_FailsLoudOnEmptyBaseURL(t *testing.T) {
	t.Parallel()
	_, err := clients.NewIdentityResolveClient(clients.IdentityResolveClientConfig{
		BaseURL: "",
		Timeout: 5 * time.Second,
	})
	if err == nil {
		t.Fatalf("expected error for empty BaseURL")
	}
}

func TestIdentityResolveClient_SendsCorrectBody(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"gcid":              "01970000-0000-7000-8000-0000000000aa",
			"default_tenant_id": "01970000-0000-7000-8000-0000000000bb",
			"email":             "alice@example.com",
			"subject":           "subject-1",
			"memberships": []map[string]any{{
				"tenant_id":   "01970000-0000-7000-8000-0000000000bb",
				"tenant_slug": "academy-A",
				"roles":       []string{"learner"},
				"surfaces":    []string{"aplus", "cplus"},
				"is_default":  true,
			}},
		})
	}))
	t.Cleanup(srv.Close)
	c, _ := clients.NewIdentityResolveClient(clients.IdentityResolveClientConfig{
		BaseURL: srv.URL,
		Timeout: 5 * time.Second,
	})
	_, _ = c.Resolve(context.Background(), clients.ResolveRequest{
		Email:          "alice@example.com",
		Subject:        "subject-1",
		ActiveTenantID: "01970000-0000-7000-8000-0000000000bb",
	})
	if gotBody["email"] != "alice@example.com" {
		t.Errorf("email = %v", gotBody["email"])
	}
	if gotBody["subject"] != "subject-1" {
		t.Errorf("subject = %v", gotBody["subject"])
	}
	if gotBody["active_tenant_id"] != "01970000-0000-7000-8000-0000000000bb" {
		t.Errorf("active_tenant_id = %v", gotBody["active_tenant_id"])
	}
}

func TestIdentityResolveClient_TimeoutTriggersError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	c, _ := clients.NewIdentityResolveClient(clients.IdentityResolveClientConfig{
		BaseURL: srv.URL,
		Timeout: 10 * time.Millisecond,
	})
	_, err := c.Resolve(context.Background(), clients.ResolveRequest{
		Email:   "alice@example.com",
		Subject: "subject-1",
	})
	if err == nil {
		t.Fatalf("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timeout") &&
		!strings.Contains(err.Error(), "deadline") &&
		!strings.Contains(err.Error(), "context") {
		t.Errorf("expected timeout-related error, got %q", err.Error())
	}
}
