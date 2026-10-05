// identity_passkey_client_test.go — behaviour specs for the chora-identity
// passkey ceremony client (auth-hardening Phase A4, CHO-1718).
package clients_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
)

func newPasskeyServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *clients.IdentityPasskeyClient) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := clients.NewIdentityPasskeyClient(clients.IdentityPasskeyClientConfig{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewIdentityPasskeyClient: %v", err)
	}
	return srv, c
}

func TestPasskeyClient_RequiresBaseURL(t *testing.T) {
	if _, err := clients.NewIdentityPasskeyClient(clients.IdentityPasskeyClientConfig{}); err == nil {
		t.Fatalf("expected error on empty BaseURL")
	}
}

func TestPasskeyClient_Challenge_HappyPath(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	_, c := newPasskeyServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"challenge_id": "ch-1",
			"challenge":    "Y2hhbGxlbmdl",
			"rp_id":        "chora.site",
			"expires_at":   "2026-06-11T12:05:00Z",
		})
	})
	resp, err := c.Challenge(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("Challenge: %v", err)
	}
	if gotPath != "/v1/auth/passkey/challenge" {
		t.Errorf("path = %q", gotPath)
	}
	if gotBody["user_handle"] != "alice@example.com" {
		t.Errorf("user_handle = %v", gotBody["user_handle"])
	}
	if resp.ChallengeID != "ch-1" || resp.RPID != "chora.site" {
		t.Errorf("resp = %+v", resp)
	}
}

func TestPasskeyClient_Challenge_OmitsEmptyUserHandle(t *testing.T) {
	var gotBody map[string]any
	_, c := newPasskeyServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"challenge_id": "ch-2", "challenge": "eA", "rp_id": "chora.site"})
	})
	if _, err := c.Challenge(context.Background(), "  "); err != nil {
		t.Fatalf("Challenge: %v", err)
	}
	if _, present := gotBody["user_handle"]; present {
		t.Errorf("user_handle must be omitted when empty; got %v", gotBody)
	}
}

func TestPasskeyClient_Register_HappyPath(t *testing.T) {
	var gotPath string
	var gotReq clients.PasskeyRegisterRequest
	_, c := newPasskeyServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"credential_id": "Y3JlZA",
			"gcid":          "g-1",
			"created_at":    "2026-06-11T12:00:00Z",
		})
	})
	resp, err := c.Register(context.Background(), clients.PasskeyRegisterRequest{
		ChallengeID:       "ch-1",
		GCID:              "g-1",
		CredentialID:      "Y3JlZA",
		ClientDataJSON:    "Y2Q",
		AttestationObject: "YW8",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if gotPath != "/v1/auth/passkey/register" {
		t.Errorf("path = %q", gotPath)
	}
	if gotReq.AttestationObject != "YW8" || gotReq.GCID != "g-1" {
		t.Errorf("forwarded req = %+v", gotReq)
	}
	if resp.CredentialID != "Y3JlZA" {
		t.Errorf("resp = %+v", resp)
	}
}

func TestPasskeyClient_Verify_HappyPath(t *testing.T) {
	var gotPath string
	_, c := newPasskeyServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{
			"gcid":          "g-9",
			"email":         "alice@example.com",
			"credential_id": "Y3JlZA",
		})
	})
	resp, err := c.Verify(context.Background(), clients.PasskeyVerifyRequest{
		ChallengeID:       "ch-1",
		CredentialID:      "Y3JlZA",
		ClientDataJSON:    "Y2Q",
		AuthenticatorData: "YWQ",
		Signature:         "c2ln",
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if gotPath != "/v1/auth/passkey/verify" {
		t.Errorf("path = %q", gotPath)
	}
	if resp.GCID != "g-9" || resp.Email != "alice@example.com" {
		t.Errorf("resp = %+v", resp)
	}
}

func TestPasskeyClient_4xxMapsToUpstreamError(t *testing.T) {
	_, c := newPasskeyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":    "PASSKEY_SIGNATURE_REJECTED",
			"message": "WebAuthn signature did not verify",
		})
	})
	_, err := c.Verify(context.Background(), clients.PasskeyVerifyRequest{ChallengeID: "x", CredentialID: "y"})
	var upstream *clients.PasskeyUpstreamError
	if !errors.As(err, &upstream) {
		t.Fatalf("err = %v want *PasskeyUpstreamError", err)
	}
	if upstream.StatusCode != http.StatusUnauthorized || upstream.Code != "PASSKEY_SIGNATURE_REJECTED" {
		t.Errorf("upstream = %+v", upstream)
	}
}

func TestPasskeyClient_4xxWithoutCode_GetsFallbackCode(t *testing.T) {
	_, c := newPasskeyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("not json"))
	})
	_, err := c.Challenge(context.Background(), "")
	var upstream *clients.PasskeyUpstreamError
	if !errors.As(err, &upstream) {
		t.Fatalf("err = %v want *PasskeyUpstreamError", err)
	}
	if upstream.Code != "PASSKEY_UPSTREAM_REJECTED" {
		t.Errorf("fallback code = %q", upstream.Code)
	}
}

func TestPasskeyClient_5xxMapsToUnavailable(t *testing.T) {
	_, c := newPasskeyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	_, err := c.Challenge(context.Background(), "")
	if !errors.Is(err, clients.ErrPasskeyUpstreamUnavailable) {
		t.Fatalf("err = %v want ErrPasskeyUpstreamUnavailable", err)
	}
}

func TestPasskeyClient_TransportErrorMapsToUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // dead endpoint
	c, err := clients.NewIdentityPasskeyClient(clients.IdentityPasskeyClientConfig{BaseURL: url})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if _, err := c.Challenge(context.Background(), ""); !errors.Is(err, clients.ErrPasskeyUpstreamUnavailable) {
		t.Fatalf("err = %v want ErrPasskeyUpstreamUnavailable", err)
	}
}
