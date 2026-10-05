// identity_passkey_client.go — HTTP client for chora-identity's passkey
// ceremony routes (auth-hardening Phase A4, ADR-181 D2, CHO-1718):
//
//	POST /v1/auth/passkey/challenge  → mint a single-use WebAuthn challenge
//	POST /v1/auth/passkey/register   → registration finish (attestation parse)
//	POST /v1/auth/passkey/verify     → login finish (assertion verify; returns
//	                                   gcid+email — NO token: the gateway
//	                                   composes the session-mint pipeline)
//
// chora-gateway's /api/v1/auth/webauthn/* BFF routes proxy through this
// client. Trust model mirrors identity_resolve_client.go: mesh-internal
// plain HTTP (Cloud Service Mesh mTLS at L4).
//
// Per `feedback_no_inline_config`: BaseURL MUST come from env
// (SVC_IDENTITY_URL); the constructor fails-loud on empty input.
package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Default per-call timeout. Production callers can override via Config.Timeout.
const defaultIdentityPasskeyTimeout = 5 * time.Second

// ErrPasskeyUpstreamUnavailable — transport failure / 5xx from chora-identity.
// Callers surface as 503.
var ErrPasskeyUpstreamUnavailable = errors.New("clients.passkey: upstream unavailable")

// PasskeyUpstreamError carries a structured 4xx rejection from
// chora-identity's passkey routes so the BFF can mirror status + code to the
// SPA verbatim (e.g. 401 PASSKEY_SIGNATURE_REJECTED, 409
// PASSKEY_CREDENTIAL_EXISTS).
type PasskeyUpstreamError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *PasskeyUpstreamError) Error() string {
	return fmt.Sprintf("clients.passkey: upstream %d %s: %s", e.StatusCode, e.Code, e.Message)
}

// --- wire shapes (mirror chora-identity's passkey_handler.go) -----------------

// PasskeyChallengeResponse is identity's challenge mint envelope.
type PasskeyChallengeResponse struct {
	ChallengeID string `json:"challenge_id"`
	Challenge   string `json:"challenge"` // base64url
	RPID        string `json:"rp_id"`
	UserHandle  string `json:"user_handle,omitempty"`
	ExpiresAt   string `json:"expires_at"`
}

// PasskeyRegisterRequest is the registration-finish body. All binary fields
// are base64 strings passed through verbatim from the SPA (identity decodes
// leniently — std or url-safe, padded or raw).
type PasskeyRegisterRequest struct {
	ChallengeID       string `json:"challenge_id"`
	GCID              string `json:"gcid"`
	CredentialID      string `json:"credential_id"`
	ClientDataJSON    string `json:"client_data_json"`
	AttestationObject string `json:"attestation_object"`
}

// PasskeyRegisterResponse is identity's 201 envelope.
type PasskeyRegisterResponse struct {
	CredentialID string `json:"credential_id"` // base64url
	GCID         string `json:"gcid"`
	CreatedAt    string `json:"created_at"`
}

// PasskeyVerifyRequest is the login-finish (assertion) body.
type PasskeyVerifyRequest struct {
	ChallengeID       string `json:"challenge_id"`
	CredentialID      string `json:"credential_id"`
	ClientDataJSON    string `json:"client_data_json"`
	AuthenticatorData string `json:"authenticator_data"`
	Signature         string `json:"signature"`
	UserHandle        string `json:"user_handle,omitempty"`
}

// PasskeyVerifyResponse is identity's 200 envelope — the verified identity.
// The gateway composes the standard session mint from these fields.
type PasskeyVerifyResponse struct {
	GCID         string `json:"gcid"`
	Email        string `json:"email"`
	CredentialID string `json:"credential_id"`
}

// --- client ---------------------------------------------------------------------

// IdentityPasskeyClientConfig is the constructor input.
type IdentityPasskeyClientConfig struct {
	// BaseURL is the chora-identity service base URL (mesh DNS). REQUIRED.
	BaseURL string
	// Timeout is the per-call deadline. Defaults to 5s if zero.
	Timeout time.Duration
	// HTTPClient — injected by tests.
	HTTPClient *http.Client
}

// IdentityPasskeyClient is the HTTP client adapter for chora-identity's
// /v1/auth/passkey/* routes.
type IdentityPasskeyClient struct {
	baseURL string
	timeout time.Duration
	client  *http.Client
}

// NewIdentityPasskeyClient constructs the client. Fails loud on empty BaseURL
// per the no-inline-config rule.
func NewIdentityPasskeyClient(cfg IdentityPasskeyClientConfig) (*IdentityPasskeyClient, error) {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		return nil, errors.New("clients.passkey: BaseURL required (set SVC_IDENTITY_URL)")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultIdentityPasskeyTimeout
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &IdentityPasskeyClient{
		baseURL: strings.TrimRight(base, "/"),
		timeout: timeout,
		client:  client,
	}, nil
}

// Challenge calls POST /v1/auth/passkey/challenge. userHandle is optional
// (empty = discoverable-credential flow).
func (c *IdentityPasskeyClient) Challenge(ctx context.Context, userHandle string) (*PasskeyChallengeResponse, error) {
	body := map[string]string{}
	if strings.TrimSpace(userHandle) != "" {
		body["user_handle"] = strings.TrimSpace(userHandle)
	}
	var out PasskeyChallengeResponse
	if err := c.post(ctx, "/v1/auth/passkey/challenge", body, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Register calls POST /v1/auth/passkey/register (registration finish).
func (c *IdentityPasskeyClient) Register(ctx context.Context, req PasskeyRegisterRequest) (*PasskeyRegisterResponse, error) {
	var out PasskeyRegisterResponse
	if err := c.post(ctx, "/v1/auth/passkey/register", req, http.StatusCreated, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Verify calls POST /v1/auth/passkey/verify (login finish).
func (c *IdentityPasskeyClient) Verify(ctx context.Context, req PasskeyVerifyRequest) (*PasskeyVerifyResponse, error) {
	var out PasskeyVerifyResponse
	if err := c.post(ctx, "/v1/auth/passkey/verify", req, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// post issues the request and maps the response:
//   - wantStatus → decode into out
//   - 4xx        → *PasskeyUpstreamError (status + body code/message mirrored)
//   - 5xx / transport → wrapped ErrPasskeyUpstreamUnavailable
func (c *IdentityPasskeyClient) post(ctx context.Context, path string, body any, wantStatus int, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("clients.passkey: marshal body: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost,
		c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("clients.passkey: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPasskeyUpstreamUnavailable, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	switch {
	case resp.StatusCode == wantStatus:
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("clients.passkey: decode %d body: %w", wantStatus, err)
		}
		return nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		var env struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(respBody, &env)
		if env.Code == "" {
			env.Code = "PASSKEY_UPSTREAM_REJECTED"
		}
		if env.Message == "" {
			env.Message = truncateBody(respBody, 200)
		}
		return &PasskeyUpstreamError{StatusCode: resp.StatusCode, Code: env.Code, Message: env.Message}
	default:
		return fmt.Errorf("%w: status %d: %s", ErrPasskeyUpstreamUnavailable,
			resp.StatusCode, truncateBody(respBody, 200))
	}
}

func truncateBody(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
