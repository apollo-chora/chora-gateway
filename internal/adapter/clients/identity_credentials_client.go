// identity_credentials_client.go — HTTP client for chora-identity's
// POST /v1/auth/verify-credentials (username/password exchange).
//
// This replaces the username/password / the identity provider token exchange as the
// identity proof for POST /api/v1/auth/session/mint. chora-gateway calls
// this endpoint with the caller-supplied username + password and, on 200,
// mints the Chora session JWT from the authoritative claims chora-identity
// returns (gcid, active_tenant_id, active_tenant_roles).
//
// Trust model: this is a service-to-service hop on the local/broker-neutral
// network. The base URL comes from CHORA_IDENTITY_URL (default
// http://identity:8080) per the frozen auth contract.
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

// DefaultVerifyCredentialsTimeout bounds a single verify-credentials call.
const DefaultVerifyCredentialsTimeout = 5 * time.Second

// Sentinels callers map onto HTTP responses.
var (
	// ErrInvalidCredentials — chora-identity rejected the username/password
	// pair (401). Caller surfaces 401 INVALID_CREDENTIALS.
	ErrInvalidCredentials = errors.New("clients.identity: invalid credentials")

	// ErrCredentialsUpstreamUnavailable — transport failure or 5xx. Caller
	// surfaces 503.
	ErrCredentialsUpstreamUnavailable = errors.New("clients.identity: credentials upstream unavailable")
)

// IdentityCredentialsClientConfig is the constructor input.
type IdentityCredentialsClientConfig struct {
	// BaseURL is the chora-identity service base URL, e.g.
	// "http://identity:8080". REQUIRED — constructor fails loud on empty.
	BaseURL string

	// Timeout is the per-call deadline. Defaults to
	// DefaultVerifyCredentialsTimeout when zero.
	Timeout time.Duration

	// HTTPClient lets tests inject a stub.
	HTTPClient *http.Client
}

// IdentityCredentialsClient is the HTTP adapter for chora-identity's
// POST /v1/auth/verify-credentials.
type IdentityCredentialsClient struct {
	baseURL string
	timeout time.Duration
	client  *http.Client
}

// NewIdentityCredentialsClient constructs the client. Fails loud when the
// base URL is empty.
func NewIdentityCredentialsClient(cfg IdentityCredentialsClientConfig) (*IdentityCredentialsClient, error) {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		return nil, errors.New("clients.identity: credentials BaseURL required (set CHORA_IDENTITY_URL)")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultVerifyCredentialsTimeout
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &IdentityCredentialsClient{
		baseURL: strings.TrimRight(base, "/"),
		timeout: timeout,
		client:  client,
	}, nil
}

// VerifyCredentialsRequest is the on-wire body.
type VerifyCredentialsRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// IdentityMembership is one (tenant, roles) membership row returned by
// chora-identity. Mirrors ResolveTenantMembership so the mint response keeps
// the frontend's memberships[] contract intact.
type IdentityMembership struct {
	TenantID string   `json:"tenant_id"`
	Roles    []string `json:"roles"`
}

// VerifyCredentialsResponse is the authoritative identity returned by
// chora-identity on a successful credential check. active_tenant_id and
// active_tenant_roles are the authoritative claims for the minted session
// JWT — the gateway MUST NOT re-derive them.
type VerifyCredentialsResponse struct {
	GCID              string               `json:"gcid"`
	Email             string               `json:"email"`
	ActiveTenantID    string               `json:"active_tenant_id"`
	ActiveTenantRoles []string             `json:"active_tenant_roles"`
	Memberships       []IdentityMembership `json:"memberships"`
}

// VerifyCredentials calls POST /v1/auth/verify-credentials.
//
// Status mapping:
//   - 200 → VerifyCredentialsResponse
//   - 401 → ErrInvalidCredentials
//   - 503 / 5xx / transport → ErrCredentialsUpstreamUnavailable
//   - other 4xx → wrapped error
func (c *IdentityCredentialsClient) VerifyCredentials(ctx context.Context, username, password string) (*VerifyCredentialsResponse, error) {
	if strings.TrimSpace(username) == "" || password == "" {
		return nil, errors.New("clients.identity.VerifyCredentials: username and password required")
	}
	body, err := json.Marshal(VerifyCredentialsRequest{Username: username, Password: password})
	if err != nil {
		return nil, fmt.Errorf("clients.identity: marshal body: %w", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost,
		c.baseURL+"/v1/auth/verify-credentials", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("clients.identity: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCredentialsUpstreamUnavailable, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	switch {
	case resp.StatusCode == http.StatusOK:
		var out VerifyCredentialsResponse
		if err := json.Unmarshal(respBody, &out); err != nil {
			return nil, fmt.Errorf("clients.identity: decode 200 body: %w", err)
		}
		return &out, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("%w: %s", ErrInvalidCredentials, sniffErrorMessage(respBody))
	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("%w: status %d", ErrCredentialsUpstreamUnavailable, resp.StatusCode)
	default:
		return nil, fmt.Errorf("clients.identity: unexpected status %d: %s",
			resp.StatusCode, sniffErrorMessage(respBody))
	}
}
