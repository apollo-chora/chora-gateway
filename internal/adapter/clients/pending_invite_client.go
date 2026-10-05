// pending_invite_client.go — HTTP client for chora-identity's read-only
// GET /api/v1/internal/pending-invite?email=X (CHO-2205 / ADR-194 D2).
//
// The gateway mint allowlist calls this during POST /api/v1/auth/session/mint
// when an email is NOT in the static idp-email-allowlist secret: an admin-
// invited email carries a live pending tenant invite, and the invite IS the
// authorization. Reads through so the invited user signs in immediately —
// no per-invite Secret Manager patch.
//
// Trust model: mesh-internal (chora-gateway → chora-identity across Cloud
// Service Mesh). Cloud Service Mesh handles mTLS at L4 so this client speaks
// plain HTTP to its sidecar. The endpoint is a PURE READ (MatchByEmail) — it
// mints no GCID and writes no membership.
//
// Per `feedback_no_inline_config`: BaseURL MUST come from env
// (SVC_IDENTITY_URL — the same base the resolve client uses); the constructor
// fails-loud on empty input.
package clients

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Default per-call timeout. Production callers can override via Config.Timeout.
const defaultPendingInviteTimeout = 5 * time.Second

// PendingInviteClientConfig is the constructor input.
type PendingInviteClientConfig struct {
	// BaseURL is the chora-identity service base URL (e.g. via mesh DNS:
	// "http://chora-identity:8080"). REQUIRED — constructor fails-loud on empty.
	BaseURL string
	// Timeout is the per-call deadline. Defaults to 5s if zero.
	Timeout time.Duration
	// HTTPClient — injected by tests. Defaults to a fresh http.Client with the
	// supplied timeout.
	HTTPClient *http.Client
}

// PendingInviteClient is the HTTP client adapter for chora-identity's
// GET /api/v1/internal/pending-invite.
type PendingInviteClient struct {
	baseURL string
	timeout time.Duration
	client  *http.Client
}

// NewPendingInviteClient constructs the client. Returns an error when BaseURL
// is empty — boot must fail loud per the no-inline-config rule.
func NewPendingInviteClient(cfg PendingInviteClientConfig) (*PendingInviteClient, error) {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		return nil, errors.New("clients.pendinginvite: BaseURL required (set SVC_IDENTITY_URL)")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultPendingInviteTimeout
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &PendingInviteClient{
		baseURL: strings.TrimRight(base, "/"),
		timeout: timeout,
		client:  client,
	}, nil
}

// pendingInviteResponse is the response envelope (matches the chora-identity
// handler's {"has_pending_invite": bool, "is_known_user": bool}).
type pendingInviteResponse struct {
	HasPendingInvite bool `json:"has_pending_invite"`
	IsKnownUser      bool `json:"is_known_user"` // CHO-2207 — already-onboarded member
}

// IsAuthorized calls GET /api/v1/internal/pending-invite?email=X and reports
// whether the email may mint beyond the static allowlist — true if it has a
// live pending tenant invite (CHO-2205, first sign-in) OR is an already-known
// identity user (CHO-2207, re-authentication of an accepted-invite member).
//
// Fail-closed contract: ANY non-200 status or transport error returns a
// non-nil error, and the mint gate treats a non-nil error as NOT authorized —
// an identity outage must never open the gate. The email is lower-cased +
// trimmed and query-encoded (a "+tag" sub-address survives as %2B, not a
// space).
func (c *PendingInviteClient) IsAuthorized(ctx context.Context, email string) (bool, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return false, errors.New("clients.pendinginvite: email required")
	}

	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	q := url.Values{}
	q.Set("email", email)
	target := c.baseURL + "/api/v1/internal/pending-invite?" + q.Encode()

	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodGet, target, nil)
	if err != nil {
		return false, fmt.Errorf("clients.pendinginvite: new request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return false, fmt.Errorf("clients.pendinginvite: call failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("clients.pendinginvite: unexpected status %d: %s",
			resp.StatusCode, sniffErrorMessage(respBody))
	}

	var out pendingInviteResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return false, fmt.Errorf("clients.pendinginvite: decode 200 body: %w", err)
	}
	return out.HasPendingInvite || out.IsKnownUser, nil
}
