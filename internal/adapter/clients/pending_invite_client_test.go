// pending_invite_client_test.go — behaviour specs for the chora-identity
// pending-invite read-through client (CHO-2205). The gateway mint allowlist
// calls GET /api/v1/internal/pending-invite?email=X to authorize an admin-
// invited email at first sign-in without a static-allowlist secret patch.
package clients_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
)

func newPendingInviteClient(t *testing.T, h http.HandlerFunc) *clients.PendingInviteClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := clients.NewPendingInviteClient(clients.PendingInviteClientConfig{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewPendingInviteClient: %v", err)
	}
	return c
}

func TestPendingInviteClient_ConstructorRequiresBaseURL(t *testing.T) {
	if _, err := clients.NewPendingInviteClient(clients.PendingInviteClientConfig{BaseURL: "   "}); err == nil {
		t.Fatal("want error on empty BaseURL (no-inline-config fail-loud), got nil")
	}
}

func TestPendingInviteClient_HasPendingInvite_True(t *testing.T) {
	var gotMethod, gotPath, gotEmail string
	c := newPendingInviteClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotEmail = r.URL.Query().Get("email")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"has_pending_invite":true}`))
	})
	got, err := c.IsAuthorized(context.Background(), "Bob+Tag@Example.COM")
	if err != nil {
		t.Fatalf("HasPendingInvite: %v", err)
	}
	if !got {
		t.Error("want has_pending_invite=true")
	}
	if gotMethod != http.MethodGet {
		t.Errorf("want GET got %s", gotMethod)
	}
	if gotPath != "/api/v1/internal/pending-invite" {
		t.Errorf("unexpected path %q", gotPath)
	}
	// Email is lower-cased + trimmed by the client, and a plus-tag must
	// survive the query round trip (encoded %2B, NOT swallowed as a space) —
	// the dale+dodlearner@ sub-addressing case.
	if gotEmail != "bob+tag@example.com" {
		t.Errorf("server saw email %q, want bob+tag@example.com", gotEmail)
	}
}

func TestPendingInviteClient_HasPendingInvite_False(t *testing.T) {
	c := newPendingInviteClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"has_pending_invite":false}`))
	})
	got, err := c.IsAuthorized(context.Background(), "nobody@example.com")
	if err != nil {
		t.Fatalf("IsAuthorized: %v", err)
	}
	if got {
		t.Error("want authorized=false")
	}
}

func TestPendingInviteClient_KnownUser_Authorized(t *testing.T) {
	// CHO-2207: no pending invite, but the email is an already-known member →
	// authorized (the client ORs has_pending_invite with is_known_user).
	c := newPendingInviteClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"has_pending_invite":false,"is_known_user":true}`))
	})
	got, err := c.IsAuthorized(context.Background(), "member@example.com")
	if err != nil {
		t.Fatalf("IsAuthorized: %v", err)
	}
	if !got {
		t.Error("want authorized=true for an existing member (is_known_user)")
	}
}

func TestPendingInviteClient_EmptyEmail_Errors(t *testing.T) {
	c := newPendingInviteClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("server must not be called for an empty email")
		w.WriteHeader(http.StatusOK)
	})
	if _, err := c.IsAuthorized(context.Background(), "   "); err == nil {
		t.Fatal("want error on empty email, got nil")
	}
}

func TestPendingInviteClient_Non200_Errors(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusNotFound, http.StatusInternalServerError} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			c := newPendingInviteClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"code":"X","message":"nope"}`))
			})
			got, err := c.IsAuthorized(context.Background(), "x@example.com")
			if err == nil {
				t.Fatalf("want error on %d, got nil", status)
			}
			if got {
				t.Error("a non-200 must return false (the mint gate fails closed at the caller)")
			}
		})
	}
}

func TestPendingInviteClient_BadJSON200_Errors(t *testing.T) {
	c := newPendingInviteClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	})
	if _, err := c.IsAuthorized(context.Background(), "x@example.com"); err == nil {
		t.Fatal("want decode error on malformed 200 body, got nil")
	}
}

func TestPendingInviteClient_TransportError_Errors(t *testing.T) {
	c, err := clients.NewPendingInviteClient(clients.PendingInviteClientConfig{
		BaseURL: "http://127.0.0.1:1", // nothing listening
	})
	if err != nil {
		t.Fatalf("NewPendingInviteClient: %v", err)
	}
	got, err := c.IsAuthorized(context.Background(), "x@example.com")
	if err == nil {
		t.Fatal("want transport error, got nil")
	}
	if got {
		t.Error("transport error must return false (fail-closed)")
	}
}
