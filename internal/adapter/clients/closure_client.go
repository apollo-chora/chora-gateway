// closure_client.go — HTTP client for chora-closure-orchestrator's saga
// trigger routes (account-closure lane, ADR-181 D5, CHO-1719):
//
//	POST /v1/closure/request               → start a federated closure saga (201)
//	POST /v1/closure/{closure_id}/cancel   → cancel during the grace window (200)
//	GET  /v1/closure/{closure_id}/status   → saga state + history + acks (200)
//
// Contract: chora-contracts/openapi/closure-orchestrator.yaml v2.0.0. The
// orchestrator has NO by-gcid lookup — status/cancel are closure_id-keyed;
// the gateway's closure_handler.go enforces "own saga only" by comparing the
// returned saga gcid against the session gcid.
//
// fast_close (PLATFORM_OPERATOR-only grace collapse) is authorised upstream
// via the X-Chora-Role header this client forwards on RequestClosure — the
// orchestrator independently 403s fast_close without it (defence in depth;
// same comma-joined-roles convention chora-payments admin routes trust).
//
// Trust model mirrors identity_passkey_client.go: mesh-internal plain HTTP by
// default. The orchestrator is a Cloud Run service with INTERNAL ingress —
// when its IAM requires a Google-signed ID token, the loader injects an
// idtoken-authorised *http.Client via Config.HTTPClient
// (CLOSURE_ORCHESTRATOR_ID_TOKEN_AUDIENCE); this client is transport-agnostic.
//
// Per `feedback_no_inline_config`: BaseURL MUST come from env
// (CLOSURE_ORCHESTRATOR_URL); the constructor fails-loud on empty input.
package clients

import (
	"bytes"
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

// Default per-call timeout. The orchestrator runs on Cloud Run — first-call
// cold starts of the Python service can take seconds, so this is deliberately
// looser than the in-cluster 5s defaults. Production callers can override
// via Config.Timeout.
const defaultClosureTimeout = 15 * time.Second

// ErrClosureUpstreamUnavailable — transport failure / 5xx from the closure
// orchestrator. Callers surface as 503 CLOSURE_UNAVAILABLE.
var ErrClosureUpstreamUnavailable = errors.New("clients.closure: upstream unavailable")

// ClosureUpstreamError carries a structured 4xx rejection from the closure
// orchestrator so the BFF can mirror status + code to the SPA (e.g. 409
// CLOSURE_CANCEL_TOO_LATE, 403 CLOSURE_FAST_CLOSE_OPERATOR_REQUIRED).
type ClosureUpstreamError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *ClosureUpstreamError) Error() string {
	return fmt.Sprintf("clients.closure: upstream %d %s: %s", e.StatusCode, e.Code, e.Message)
}

// --- wire shapes (mirror closure-orchestrator.yaml v2 schemas) -----------------

// ClosureRequest is the POST /v1/closure/request body (CloseRequest schema).
type ClosureRequest struct {
	GCID            string `json:"gcid"`
	TenantID        string `json:"tenant_id"`
	GracePeriodDays int    `json:"grace_period_days"`
	Reason          string `json:"reason,omitempty"`
	RequestedByGCID string `json:"requested_by_gcid"`
	// FastClose collapses the grace window (test accounts; ADR-181 ruling
	// 12). PLATFORM_OPERATOR-only — enforced upstream via X-Chora-Role.
	FastClose bool `json:"fast_close,omitempty"`
}

// ClosureCloseResponse is the orchestrator's 201 envelope (CloseResponse).
type ClosureCloseResponse struct {
	SagaID      string `json:"saga_id"`
	State       string `json:"state"`
	GraceEndsAt string `json:"grace_ends_at"`
	RequestedAt string `json:"requested_at"`
}

// ClosureCancelRequest is the POST /v1/closure/{id}/cancel body.
type ClosureCancelRequest struct {
	ActorGCID string `json:"actor_gcid"`
	Reason    string `json:"reason,omitempty"`
}

// ClosureCancelResponse is the orchestrator's 200 cancel envelope.
type ClosureCancelResponse struct {
	SagaID      string `json:"saga_id"`
	State       string `json:"state"`
	CancelledAt string `json:"cancelled_at,omitempty"`
}

// ClosureHistoryEntry is one append-only saga transition (HistoryEntry).
type ClosureHistoryEntry struct {
	PriorState     string `json:"prior_state,omitempty"`
	NewState       string `json:"new_state,omitempty"`
	Reason         string `json:"reason,omitempty"`
	ActorGCID      string `json:"actor_gcid,omitempty"`
	TransitionedAt string `json:"transitioned_at,omitempty"`
}

// ClosureDomainAck is one per-domain pseudonymisation ack (DomainAck).
type ClosureDomainAck struct {
	Domain  string `json:"domain"`
	AckedAt string `json:"acked_at,omitempty"`
}

// ClosureStatusResponse is the orchestrator's 200 status envelope
// (StatusResponse) — includes the saga's gcid, which the gateway compares
// against the session gcid for the "own saga only" gate.
type ClosureStatusResponse struct {
	SagaID      string                `json:"saga_id"`
	GCID        string                `json:"gcid"`
	TenantID    string                `json:"tenant_id"`
	State       string                `json:"state"`
	GraceEndsAt string                `json:"grace_ends_at,omitempty"`
	RequestedAt string                `json:"requested_at,omitempty"`
	History     []ClosureHistoryEntry `json:"history,omitempty"`
	DomainAcks  []ClosureDomainAck    `json:"domain_acks,omitempty"`
}

// --- client ---------------------------------------------------------------------

// ClosureClientConfig is the constructor input.
type ClosureClientConfig struct {
	// BaseURL is the closure-orchestrator base URL. REQUIRED
	// (CLOSURE_ORCHESTRATOR_URL).
	BaseURL string
	// Timeout is the per-call deadline. Defaults to 15s if zero.
	Timeout time.Duration
	// HTTPClient — injected by tests, or by the loader as an
	// idtoken-authorised client for Cloud Run IAM (internal ingress).
	HTTPClient *http.Client
}

// ClosureClient is the HTTP client adapter for the closure orchestrator's
// /v1/closure/* routes.
type ClosureClient struct {
	baseURL string
	timeout time.Duration
	client  *http.Client
}

// NewClosureClient constructs the client. Fails loud on empty BaseURL per
// the no-inline-config rule.
func NewClosureClient(cfg ClosureClientConfig) (*ClosureClient, error) {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		return nil, errors.New("clients.closure: BaseURL required (set CLOSURE_ORCHESTRATOR_URL)")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultClosureTimeout
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &ClosureClient{
		baseURL: strings.TrimRight(base, "/"),
		timeout: timeout,
		client:  client,
	}, nil
}

// RequestClosure calls POST /v1/closure/request. roleHeader, when non-empty,
// is forwarded as X-Chora-Role (comma-joined canonical roles — required by
// the orchestrator's fast_close PLATFORM_OPERATOR gate).
func (c *ClosureClient) RequestClosure(ctx context.Context, req ClosureRequest, roleHeader string) (*ClosureCloseResponse, error) {
	var out ClosureCloseResponse
	if err := c.do(ctx, http.MethodPost, "/v1/closure/request", req, roleHeader, http.StatusCreated, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelClosure calls POST /v1/closure/{closure_id}/cancel.
func (c *ClosureClient) CancelClosure(ctx context.Context, closureID string, req ClosureCancelRequest) (*ClosureCancelResponse, error) {
	var out ClosureCancelResponse
	path := "/v1/closure/" + url.PathEscape(closureID) + "/cancel"
	if err := c.do(ctx, http.MethodPost, path, req, "", http.StatusOK, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetClosureStatus calls GET /v1/closure/{closure_id}/status.
func (c *ClosureClient) GetClosureStatus(ctx context.Context, closureID string) (*ClosureStatusResponse, error) {
	var out ClosureStatusResponse
	path := "/v1/closure/" + url.PathEscape(closureID) + "/status"
	if err := c.do(ctx, http.MethodGet, path, nil, "", http.StatusOK, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// do issues the request and maps the response:
//   - wantStatus → decode into out
//   - 4xx        → *ClosureUpstreamError (status + body code/message mirrored;
//     FastAPI's {"detail": ...} envelope is folded into Message)
//   - 5xx / transport → wrapped ErrClosureUpstreamUnavailable
func (c *ClosureClient) do(ctx context.Context, method, path string, body any, roleHeader string, wantStatus int, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("clients.closure: marshal body: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(callCtx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("clients.closure: new request: %w", err)
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	httpReq.Header.Set("Accept", "application/json")
	if roleHeader != "" {
		httpReq.Header.Set("X-Chora-Role", roleHeader)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrClosureUpstreamUnavailable, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	switch {
	case resp.StatusCode == wantStatus:
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("clients.closure: decode %d body: %w", wantStatus, err)
		}
		return nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		var env struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			// FastAPI HTTPException / validation envelope.
			Detail any `json:"detail"`
		}
		_ = json.Unmarshal(respBody, &env)
		if env.Code == "" {
			env.Code = "CLOSURE_UPSTREAM_REJECTED"
		}
		if env.Message == "" {
			if s, ok := env.Detail.(string); ok && s != "" {
				env.Message = s
			} else {
				env.Message = truncateBody(respBody, 200)
			}
		}
		return &ClosureUpstreamError{StatusCode: resp.StatusCode, Code: env.Code, Message: env.Message}
	default:
		return fmt.Errorf("%w: status %d: %s", ErrClosureUpstreamUnavailable,
			resp.StatusCode, truncateBody(respBody, 200))
	}
}
