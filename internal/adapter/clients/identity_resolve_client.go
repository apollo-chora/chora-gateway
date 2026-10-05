// Package clients owns chora-gateway's outbound client adapters for
// internal SVC_* HTTP services.
//
// identity_resolve_client.go — HTTP client for chora-identity's POST
// /v1/identity/resolve (multi-tenant identity).
//
// chora-gateway uses this client on the WebAuthn login/finish path AFTER
// chora-identity has verified the passkey assertion, to look up or create the
// GCID + fetch the user's tenant memberships. The username/password mint path
// uses identity_credentials_client.go instead.
//
// Trust model: this client is service-internal (chora-gateway → chora-identity).
// The HTTP client speaks plain HTTP on the broker-neutral local network.
//
// Per `feedback_no_inline_config`: BaseURL MUST come from env
// (CHORA_IDENTITY_URL); the constructor fails-loud on empty input.
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
const defaultIdentityResolveTimeout = 5 * time.Second

// identityNoTenantMembershipCode is the body `code` chora-identity's
// resolve handler stamps on its GENUINE no-membership 404
// (resolve_handler.go writeError(404, "IDENTITY_NO_TENANT_MEMBERSHIP")).
// Only a 404 carrying this exact code is a real no-membership signal —
// any other 404 is an unexpected/infra failure.
const identityNoTenantMembershipCode = "IDENTITY_NO_TENANT_MEMBERSHIP"

// Sentinel errors callers can errors.Is against to map to HTTP responses.
var (
	// ErrNoTenantMembership — chora-identity returned a 404 whose body code
	// is IDENTITY_NO_TENANT_MEMBERSHIP, i.e. the user genuinely has zero
	// tenant memberships. Caller surfaces as 403 AUTH_NO_TENANT_MEMBERSHIP
	// per the FE error contract.
	ErrNoTenantMembership = errors.New("clients.identity: user has no tenant membership")

	// ErrIdentityResolveRouteMissing — chora-identity returned a 404 that is
	// NOT a genuine no-membership response: an HTML / Envoy "no route" body,
	// an unrecognised JSON error code, or an empty body. This means the
	// /v1/identity/resolve hop is broken at the routing/mesh layer (or the
	// service genuinely isn't serving the route) — it is NOT the same as
	// "this user has no memberships". Conflating the two produces a
	// misleading 403 AUTH_NO_TENANT_MEMBERSHIP for what is really an infra
	// outage. Caller surfaces as 503 (upstream unavailable) so the failure
	// is loud, not silently attributed to the user's account state.
	ErrIdentityResolveRouteMissing = errors.New("clients.identity: resolve route returned an unexpected 404 (mesh/route misconfig — NOT a no-membership signal)")

	// ErrIdentityUpstreamUnavailable — chora-identity returned 503 or the
	// HTTP transport failed. Caller surfaces as 503.
	ErrIdentityUpstreamUnavailable = errors.New("clients.identity: upstream unavailable")
)

// IdentityResolveClientConfig is the constructor input.
type IdentityResolveClientConfig struct {
	// BaseURL is the chora-identity service base URL (e.g. via mesh DNS:
	// "http://chora-identity:8080"). REQUIRED — constructor fails-loud
	// on empty.
	BaseURL string

	// Timeout is the per-call deadline. Defaults to 5s if zero.
	Timeout time.Duration

	// HTTPClient — injected by tests. Defaults to a fresh http.Client
	// with the supplied timeout.
	HTTPClient *http.Client
}

// IdentityResolveClient is the HTTP client adapter for chora-identity's
// POST /v1/identity/resolve.
type IdentityResolveClient struct {
	baseURL string
	timeout time.Duration
	client  *http.Client
}

// NewIdentityResolveClient constructs the client. Returns an error when
// BaseURL is empty — boot must fail loud per the no-inline-config rule.
func NewIdentityResolveClient(cfg IdentityResolveClientConfig) (*IdentityResolveClient, error) {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		return nil, errors.New("clients.identity: BaseURL required (set SVC_IDENTITY_URL)")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultIdentityResolveTimeout
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &IdentityResolveClient{
		baseURL: strings.TrimRight(base, "/"),
		timeout: timeout,
		client:  client,
	}, nil
}

// ResolveRequest is the request body shape (matches the chora-identity
// OpenAPI schema).
type ResolveRequest struct {
	Email          string `json:"email"`
	Subject        string `json:"subject"`
	ActiveTenantID string `json:"active_tenant_id,omitempty"`
}

// ResolveResponse is the response envelope (matches the chora-identity
// OpenAPI schema).
type ResolveResponse struct {
	GCID            string                    `json:"gcid"`
	Memberships     []ResolveTenantMembership `json:"memberships"`
	DefaultTenantID string                    `json:"default_tenant_id"`
	Email           string                    `json:"email"`
	Subject         string                    `json:"subject"`
}

// ResolveTenantMembership is the per-membership envelope.
type ResolveTenantMembership struct {
	TenantID   string   `json:"tenant_id"`
	TenantSlug string   `json:"tenant_slug"`
	Roles      []string `json:"roles"`
	Surfaces   []string `json:"surfaces"`
	IsDefault  bool     `json:"is_default"`
}

// Resolve calls POST /v1/identity/resolve.
//
// Status-code mapping (per chora-identity OpenAPI):
//   - 200 → ResolveResponse on success.
//   - 400 → wrapped error (caller treats as 400).
//   - 404 with body code IDENTITY_NO_TENANT_MEMBERSHIP → ErrNoTenantMembership
//     (the GENUINE no-membership case; caller surfaces as 403).
//   - 404 with any other body (HTML, unrecognised code, empty) →
//     ErrIdentityResolveRouteMissing (an infra/route failure, NOT a
//     no-membership signal; caller surfaces as 503). These two 404 cases
//     are deliberately NOT conflated — see the sentinel doc above.
//   - 503 → wrapped ErrIdentityUpstreamUnavailable.
//   - 5xx / transport errors → wrapped error.
//
// Trace context: the http.Request inherits ctx; the OTLP HTTP middleware
// on chora-identity reads traceparent from the active span automatically.
func (c *IdentityResolveClient) Resolve(ctx context.Context, req ResolveRequest) (*ResolveResponse, error) {
	if strings.TrimSpace(req.Email) == "" {
		return nil, errors.New("clients.identity.Resolve: email required")
	}
	if strings.TrimSpace(req.Subject) == "" {
		return nil, errors.New("clients.identity.Resolve: subject required")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("clients.identity: marshal body: %w", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost,
		c.baseURL+"/v1/identity/resolve", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("clients.identity: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrIdentityUpstreamUnavailable, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusOK:
		var out ResolveResponse
		if err := json.Unmarshal(respBody, &out); err != nil {
			return nil, fmt.Errorf("clients.identity: decode 200 body: %w", err)
		}
		return &out, nil
	case http.StatusNotFound:
		// Distinguish a GENUINE no-membership 404 (body code
		// IDENTITY_NO_TENANT_MEMBERSHIP) from an unexpected/infra 404
		// (HTML, Envoy "no route", unrecognised code, empty body). Only
		// the former is a real "user has no tenant" signal — conflating
		// them turns a mesh/route outage into a misleading 403
		// AUTH_NO_TENANT_MEMBERSHIP.
		if sniffErrorCode(respBody) == identityNoTenantMembershipCode {
			return nil, fmt.Errorf("%w: %s", ErrNoTenantMembership, sniffErrorMessage(respBody))
		}
		return nil, fmt.Errorf("%w: status 404 body=%q", ErrIdentityResolveRouteMissing,
			sniffErrorMessage(respBody))
	case http.StatusServiceUnavailable:
		return nil, fmt.Errorf("%w: %s", ErrIdentityUpstreamUnavailable, sniffErrorMessage(respBody))
	default:
		return nil, fmt.Errorf("clients.identity: unexpected status %d: %s",
			resp.StatusCode, sniffErrorMessage(respBody))
	}
}

// sniffErrorCode extracts the `code` field from an error response body
// shaped as {code, message}. Returns "" when the body is not JSON, has no
// code field, or is empty — exactly the cases that must NOT be treated as
// the recognised IDENTITY_NO_TENANT_MEMBERSHIP sentinel.
func sniffErrorCode(body []byte) string {
	var env struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(body, &env) != nil {
		return ""
	}
	return env.Code
}

// sniffErrorMessage attempts to extract a human-readable message from an
// error response body shaped as {code, message}. Falls back to the raw
// body trimmed to a sensible length.
func sniffErrorMessage(body []byte) string {
	var env struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &env) == nil && env.Message != "" {
		return env.Message
	}
	s := string(body)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
