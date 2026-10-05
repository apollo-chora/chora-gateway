// weakness_resume_client.go — HTTP client for the chora-ai-kernel-orchestrator
// Growth-Edge HITL resume route (ADR-205 WS-2/WS-8; reshaped to the FE
// panel-shape per CHO-1973):
//
//	POST /v1/orchestrator/weakness/{upload_id}/resume → drive the checkpointed
//	     graph forward with the learner's BOUNDED panel-shape decision (200);
//	     the orchestrator returns a job/panel (AWAITING_REVIEW + refreshed panel
//	     on reiterate, completed job on confirm) which this client forwards
//	     through to the SPA VERBATIM.
//
// Contract: services/chora-ai-kernel-orchestrator weakness_router.py. The
// orchestrator rebuilds a DETERMINISTIC thread_id keyed by {tenant}:{upload}
// (there is NO run_id — the resume contract dropped it) and reads tenant scope
// from the X-Tenant-Id HEADER (NOT the body) — so this client forwards
// X-Tenant-Id (the load-bearing identity, taken from the validated session JWT
// at the BFF), plus gcid + W3C traceparent/tracestate for propagation (the
// orchestrator has no otelhttp auto-injection, so trace context is forwarded
// explicitly, as gatewayproxy.callWithTimeout does). The review payload carries
// NO identity (ADR-205 D4 — bounded controls only).
//
// Trust model mirrors closure_client.go: mesh-internal plain HTTP by default;
// when the orchestrator's ingress requires a Google-signed ID token the loader
// injects an idtoken-authorised *http.Client via Config.HTTPClient. Per
// feedback_no_inline_config: BaseURL MUST come from env
// (AI_KERNEL_ORCHESTRATOR_URL); the constructor fails loud on empty input.
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

// defaultWeaknessResumeTimeout: the HITL resume runs the diagnosis graph
// SYNCHRONOUSLY, generating up to four LLM outputs (dose/coaching/test/aids), so
// 15s was too short and timed out real reviews (CHO-2338). Bumped toward the
// gateway's 45s server WriteTimeout ceiling (LB backend is 3600s, not a cap).
// STOPGAP: the proper fix is an async resume (return 202, the SPA already polls);
// generation past ~40s still cannot fit a synchronous response. Overridable via
// Config.Timeout.
const defaultWeaknessResumeTimeout = 40 * time.Second

// ErrWeaknessResumeUpstreamUnavailable — transport failure / 5xx from the
// orchestrator. Callers surface as 503 WEAKNESS_RESUME_UNAVAILABLE.
var ErrWeaknessResumeUpstreamUnavailable = errors.New("clients.weakness_resume: upstream unavailable")

// WeaknessResumeUpstreamError carries a structured 4xx rejection so the BFF can
// mirror status + code/detail to the SPA (e.g. 400 invalid_decision).
type WeaknessResumeUpstreamError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *WeaknessResumeUpstreamError) Error() string {
	return fmt.Sprintf("clients.weakness_resume: upstream %d %s: %s", e.StatusCode, e.Code, e.Message)
}

// --- wire shapes (the canonical FE panel-shape) --------------------------------

// WeaknessResumeIdentity is the session-derived context forwarded as HEADERS
// (never body). TenantID is REQUIRED by the orchestrator (X-Tenant-Id); GCID and
// the W3C trace fields are forwarded for defence-in-depth + end-to-end tracing.
type WeaknessResumeIdentity struct {
	TenantID    string
	GCID        string
	Traceparent string
	Tracestate  string
}

// WeaknessEdgeDecision is a single bounded per-edge decision (ADR-205 D4): the
// learner accepts / rejects / merges a proposed Growth Edge, with an optional
// difficulty tri-toggle. MergeIntoID is set only when Decision == "merge", and
// Difficulty only when accepted/merged — both omitempty so the wire shape
// matches the chora-web WeaknessReviewDecision.EdgeDecision optionals exactly.
type WeaknessEdgeDecision struct {
	ProposedEdgeID string `json:"proposed_edge_id"`
	Decision       string `json:"decision"`
	MergeIntoID    string `json:"merge_into_id,omitempty"`
	Difficulty     string `json:"difficulty,omitempty"`
}

// WeaknessResumeRequest is the canonical FE panel-shape — the literal
// Command(resume=…) value (ADR-205 D1/D4; zero free text). It mirrors the
// chora-web WeaknessReviewDecision exactly: an Action discriminator
// (confirm | reiterate), the bounded per-edge decisions, the "add a struggle"
// concept_keys, and the chosen output kinds. There is NO run_id — the
// orchestrator thread is keyed by {tenant, upload}. The slices are emitted even
// when empty (the FE always sends arrays) so an empty selection is [] not null.
type WeaknessResumeRequest struct {
	Action          string                 `json:"action"`
	Edges           []WeaknessEdgeDecision `json:"edges"`
	AddedStruggles  []string               `json:"added_struggles"`
	SelectedOutputs []string               `json:"selected_outputs"`
}

// The orchestrator's 200 response (the async job/panel) is forwarded VERBATIM as
// a json.RawMessage — the gateway is a pure proxy and adds no coupling to the
// job/panel field shape, which the FE (WeaknessUploadJob) and orchestrator own.

// --- client ---------------------------------------------------------------------

// WeaknessResumeClientConfig is the constructor input.
type WeaknessResumeClientConfig struct {
	// BaseURL is the chora-ai-kernel-orchestrator base URL. REQUIRED
	// (AI_KERNEL_ORCHESTRATOR_URL).
	BaseURL string
	// Timeout is the per-call deadline. Defaults to 15s if zero.
	Timeout time.Duration
	// HTTPClient — injected by tests, or by the loader as an idtoken-authorised
	// client when the orchestrator ingress requires Cloud Run IAM.
	HTTPClient *http.Client
}

// WeaknessResumeClient is the HTTP adapter for the orchestrator resume route.
type WeaknessResumeClient struct {
	baseURL string
	timeout time.Duration
	client  *http.Client
}

// NewWeaknessResumeClient constructs the client. Fails loud on empty BaseURL per
// the no-inline-config rule.
func NewWeaknessResumeClient(cfg WeaknessResumeClientConfig) (*WeaknessResumeClient, error) {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		return nil, errors.New("clients.weakness_resume: BaseURL required (set AI_KERNEL_ORCHESTRATOR_URL)")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultWeaknessResumeTimeout
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &WeaknessResumeClient{
		baseURL: strings.TrimRight(base, "/"),
		timeout: timeout,
		client:  client,
	}, nil
}

// ResumeWeakness calls POST /v1/orchestrator/weakness/{upload_id}/resume and
// returns the orchestrator's job/panel response body VERBATIM. The gateway is a
// pure proxy here — it does not interpret the job/panel shape (status +
// refreshed review panel on reiterate; completed job on confirm), which the FE
// and orchestrator own — so the body flows straight through to the SPA.
func (c *WeaknessResumeClient) ResumeWeakness(ctx context.Context, uploadID string, req WeaknessResumeRequest, id WeaknessResumeIdentity) (json.RawMessage, error) {
	path := "/v1/orchestrator/weakness/" + url.PathEscape(uploadID) + "/resume"
	return c.do(ctx, path, req, id)
}

// do issues the request and maps the response:
//   - 200 → the raw (JSON-validated) job/panel body, forwarded verbatim
//   - 4xx → *WeaknessResumeUpstreamError (FastAPI {error, detail} envelope folded)
//   - 5xx / transport → wrapped ErrWeaknessResumeUpstreamUnavailable
func (c *WeaknessResumeClient) do(ctx context.Context, path string, body WeaknessResumeRequest, id WeaknessResumeIdentity) (json.RawMessage, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("clients.weakness_resume: marshal body: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("clients.weakness_resume: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	// Identity from the session JWT, forwarded as headers (never trusted from
	// the body). X-Tenant-Id is REQUIRED by the orchestrator's thread-id scope.
	httpReq.Header.Set("X-Tenant-Id", id.TenantID)
	if id.GCID != "" {
		httpReq.Header.Set("gcid", id.GCID)         // lowercase — what the Python services read
		httpReq.Header.Set("X-Chora-GCID", id.GCID) // canonical mesh header (defence in depth)
	}
	if id.Traceparent != "" {
		httpReq.Header.Set("traceparent", id.Traceparent) // W3C trace context (mandatory invariant)
	}
	if id.Tracestate != "" {
		httpReq.Header.Set("tracestate", id.Tracestate)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWeaknessResumeUpstreamUnavailable, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	switch {
	case resp.StatusCode == http.StatusOK:
		// Validate it is syntactically valid JSON, then forward the job/panel
		// body VERBATIM — the gateway adds no coupling to its field shape.
		if !json.Valid(respBody) {
			return nil, fmt.Errorf("%w: orchestrator returned non-JSON 200 body",
				ErrWeaknessResumeUpstreamUnavailable)
		}
		return json.RawMessage(respBody), nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// weakness_router emits {"error": <code>, "detail": <message>}.
		var env struct {
			Error  string `json:"error"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(respBody, &env)
		code := env.Error
		if code == "" {
			code = "WEAKNESS_RESUME_UPSTREAM_REJECTED"
		}
		msg := env.Detail
		if msg == "" {
			msg = truncateBody(respBody, 200)
		}
		return nil, &WeaknessResumeUpstreamError{StatusCode: resp.StatusCode, Code: code, Message: msg}
	default:
		return nil, fmt.Errorf("%w: status %d: %s", ErrWeaknessResumeUpstreamUnavailable,
			resp.StatusCode, truncateBody(respBody, 200))
	}
}
