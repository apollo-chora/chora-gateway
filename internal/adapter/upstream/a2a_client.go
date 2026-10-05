// Package upstream — Phase C A2A client.
//
// a2a_client.go is the real-wire HTTP client for chora-a2a's read endpoints
// powering the O+ Agent-to-Agent console:
//
//   - `GET /api/v1/a2a/contracts`        — Per-tenant contracts list
//   - `GET /api/v1/a2a/identities`       — Registered external agent
//     identities (AGIDs)
//   - `GET /api/v1/a2a/invocations`      — Recent A2A invocation rows
//
// Per the Phase B6 plan: chora-a2a may not be deployed yet on chora-prod-
// cluster. When `CHORA_A2A_HTTP_ADDR` is unset (caller signal that the
// backend is not LIVE), the handler in handlers_oplus.go will return
// `{mode:'pending', mock: {...}}` so the FE renders the explicit
// deployment-pending banner. The client itself simply returns an
// `ErrA2APending` sentinel — the handler decides the response shape.
//
// All HTTP transport goes through the Cloud Service Mesh sidecar.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// DefaultA2ACallTimeout matches the governance + observability defaults.
const DefaultA2ACallTimeout = 5 * time.Second

// ErrA2APending is returned by every A2AClient method when the HTTP address
// is unset (the canonical "chora-a2a backend not yet deployed" signal). The
// BFF handler maps this to a `{mode:'pending', mock: {...}}` response.
var ErrA2APending = errors.New("a2a: backend pending deployment")

// A2AConfig wires the chora-a2a endpoint.
type A2AConfig struct {
	HTTPAddr       string
	PerCallTimeout time.Duration
}

// LoadA2AConfigFromEnv reads `CHORA_A2A_HTTP_ADDR` per the no-inline-config
// rule. Empty addr is the explicit "pending deployment" signal (see
// handlers_oplus.go for the corresponding response shape).
func LoadA2AConfigFromEnv() A2AConfig {
	c := A2AConfig{
		HTTPAddr: os.Getenv("CHORA_A2A_HTTP_ADDR"),
	}
	if c.PerCallTimeout == 0 {
		c.PerCallTimeout = DefaultA2ACallTimeout
	}
	return c
}

// A2AContract is the BFF-facing slice of a chora-a2a contract row.
// Phase-D enrichment (audit finding #4): chora-a2a producer may attach
// PartnerName, AGID, Scope, LastInvocationAt, Invocations30d when the
// projector has cached them. BFF transformer (in handlers_oplus.go) maps
// these to the FE-canonical A2aContract shape (`id`, `partner`, `agid`,
// `scope[]`, `status`, `last_invocation`, `invocations_30d`, `created_at`).
type A2AContract struct {
	ContractID string    `json:"contract_id"`
	TenantID   string    `json:"tenant_id"`
	PartnerID  string    `json:"partner_id,omitempty"`
	Status     string    `json:"status,omitempty"`
	CreatedAt  time.Time `json:"created_at,omitempty"`
	UpdatedAt  time.Time `json:"updated_at,omitempty"`

	// --- Phase-D enrichment (audit finding #4) ----------------------------
	// PartnerName is the human-readable partner display name. Maps to FE
	// `partner`. When empty, BFF transformer falls back to PartnerID.
	PartnerName string `json:"partner_name,omitempty"`
	// AGID is the external agent identity bound to this contract.
	AGID string `json:"agid,omitempty"`
	// Scope is the OAuth-style scope grants array (e.g. `read:atoms`).
	Scope []string `json:"scope,omitempty"`
	// LastInvocationAt is the most-recent invocation timestamp, RFC3339.
	// Empty when the contract has never been invoked.
	LastInvocationAt string `json:"last_invocation,omitempty"`
	// Invocations30d is the rolling 30-day invocation count.
	Invocations30d int `json:"invocations_30d,omitempty"`
}

// A2AIdentity is the BFF-facing slice of a chora-a2a ExternalAgentIdentity.
// Phase-D enrichment (audit finding #4): chora-a2a producer may attach
// PartnerName, TrustLevel, KeyFingerprint, RotatedAt when the projector
// has cached them. BFF transformer renames `identities → external_agents`
// + maps each row to the FE-canonical A2aExternalAgent shape.
type A2AIdentity struct {
	AGID        string    `json:"agid"`
	TenantID    string    `json:"tenant_id,omitempty"`
	DisplayName string    `json:"display_name,omitempty"`
	Status      string    `json:"status,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitempty"`

	// --- Phase-D enrichment (audit finding #4) ----------------------------
	// PartnerName is the human-readable partner this agent belongs to. Maps
	// to FE `partner`. Falls back to DisplayName.
	PartnerName string `json:"partner_name,omitempty"`
	// TrustLevel is one of `verified` / `pilot` / `experimental` — derived
	// by chora-a2a-gateway from real Registration state via
	// partner.Registration.DeriveTrustLevelView(now, invocations). See
	// services/chora-a2a-gateway/internal/domain/partner/trust_level.go
	// for the rule. The BFF transformer pass-through is honest about
	// empty values per [[feedback-no-stubs-real-wiring]].
	TrustLevel string `json:"trust_level,omitempty"`
	// KeyFingerprint is the SHA-256 hex of the registered Ed25519 PEM
	// computed at registration time in agent_identity.New. Empty when no
	// ExternalAgentIdentity is on file for the AGID — the BFF never
	// fabricates sha256(agid).
	KeyFingerprint string `json:"key_fingerprint,omitempty"`
	// RotatedAt is the last key-rotation timestamp, RFC3339.
	RotatedAt string `json:"rotated_at,omitempty"`
}

// A2AInvocation is the BFF-facing slice of a chora-a2a invocation row.
// Phase-D enrichment (audit finding #4): chora-a2a producer may attach
// PartnerName, Endpoint, LatencyMS when the projector has cached them.
// BFF transformer maps `outcome → status`, `started_at → timestamp`.
type A2AInvocation struct {
	InvocationID string    `json:"invocation_id"`
	TenantID     string    `json:"tenant_id"`
	ContractID   string    `json:"contract_id,omitempty"`
	AGID         string    `json:"agid,omitempty"`
	Outcome      string    `json:"outcome,omitempty"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	EndedAt      time.Time `json:"ended_at,omitempty"`
	TraceID      string    `json:"trace_id,omitempty"`

	// --- Phase-D enrichment (audit finding #4) ----------------------------
	// PartnerName is the human-readable partner. Maps to FE `partner`.
	PartnerName string `json:"partner_name,omitempty"`
	// Endpoint is the dispatch URL chora-a2a routed this invocation to
	// (e.g. `https://a2a.chora.site/a2a/invoke#recommend_content`). Empty
	// for legacy rows / scope-denied invocations without resolved
	// dispatch — BFF passes the empty string through verbatim (no
	// placeholder substitution).
	Endpoint string `json:"endpoint,omitempty"`
	// LatencyMS is the invocation latency in milliseconds (EndedAt - StartedAt).
	// Computed BFF-side when both timestamps are present.
	LatencyMS int `json:"latency_ms,omitempty"`
}

// A2AClient is the real-wire HTTP client. Construct via NewA2AClient.
type A2AClient struct {
	cfg    A2AConfig
	http   *http.Client
	tracer trace.Tracer
}

// NewA2AClient constructs the client. When cfg.HTTPAddr is empty, all
// methods return ErrA2APending — the handler renders the FE-pending banner.
func NewA2AClient(cfg A2AConfig, httpClient *http.Client) *A2AClient {
	if cfg.PerCallTimeout <= 0 {
		cfg.PerCallTimeout = DefaultA2ACallTimeout
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: cfg.PerCallTimeout}
	}
	return &A2AClient{
		cfg:    cfg,
		http:   httpClient,
		tracer: otel.Tracer("chora-gateway/upstream/a2a"),
	}
}

// Pending returns true when the client has no configured backend. Callers
// use this to skip every fetch in one branch and emit the pending response.
func (a *A2AClient) Pending() bool {
	return a == nil || strings.TrimSpace(a.cfg.HTTPAddr) == ""
}

// GetContracts fetches the contracts list for a tenant.
func (a *A2AClient) GetContracts(ctx context.Context, tenantID string) ([]A2AContract, error) {
	if a.Pending() {
		return nil, ErrA2APending
	}
	q := url.Values{}
	if tenantID != "" {
		q.Set("tenant_id", tenantID)
	}
	u := strings.TrimRight(a.cfg.HTTPAddr, "/") + "/api/v1/a2a/contracts"
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	body, err := a.httpGetJSON(ctx, "upstream.a2a.GetContracts", u, tenantID)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return []A2AContract{}, nil
	}
	var envelope struct {
		Contracts []A2AContract `json:"contracts"`
		Items     []A2AContract `json:"items"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		if envelope.Contracts != nil {
			return envelope.Contracts, nil
		}
		if envelope.Items != nil {
			return envelope.Items, nil
		}
	}
	var rows []A2AContract
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("%w: GetContracts: malformed JSON: %v", ErrUpstream, err)
	}
	return rows, nil
}

// GetIdentities fetches the registered AGIDs for a tenant.
func (a *A2AClient) GetIdentities(ctx context.Context, tenantID string) ([]A2AIdentity, error) {
	if a.Pending() {
		return nil, ErrA2APending
	}
	q := url.Values{}
	if tenantID != "" {
		q.Set("tenant_id", tenantID)
	}
	u := strings.TrimRight(a.cfg.HTTPAddr, "/") + "/api/v1/a2a/identities"
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	body, err := a.httpGetJSON(ctx, "upstream.a2a.GetIdentities", u, tenantID)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return []A2AIdentity{}, nil
	}
	var envelope struct {
		Identities []A2AIdentity `json:"identities"`
		Items      []A2AIdentity `json:"items"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		if envelope.Identities != nil {
			return envelope.Identities, nil
		}
		if envelope.Items != nil {
			return envelope.Items, nil
		}
	}
	var rows []A2AIdentity
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("%w: GetIdentities: malformed JSON: %v", ErrUpstream, err)
	}
	return rows, nil
}

// GetInvocations fetches recent invocations; `since` is RFC3339 or empty.
func (a *A2AClient) GetInvocations(ctx context.Context, tenantID, since string, limit int) ([]A2AInvocation, error) {
	if a.Pending() {
		return nil, ErrA2APending
	}
	if limit <= 0 {
		limit = 50
	}
	q := url.Values{}
	if tenantID != "" {
		q.Set("tenant_id", tenantID)
	}
	if since != "" {
		q.Set("since", since)
	}
	q.Set("limit", fmt.Sprintf("%d", limit))
	u := strings.TrimRight(a.cfg.HTTPAddr, "/") + "/api/v1/a2a/invocations?" + q.Encode()
	body, err := a.httpGetJSON(ctx, "upstream.a2a.GetInvocations", u, tenantID)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return []A2AInvocation{}, nil
	}
	var envelope struct {
		Invocations []A2AInvocation `json:"invocations"`
		Items       []A2AInvocation `json:"items"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		if envelope.Invocations != nil {
			return envelope.Invocations, nil
		}
		if envelope.Items != nil {
			return envelope.Items, nil
		}
	}
	var rows []A2AInvocation
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("%w: GetInvocations: malformed JSON: %v", ErrUpstream, err)
	}
	return rows, nil
}

func (a *A2AClient) httpGetJSON(ctx context.Context, spanName, urlStr, tenantID string) ([]byte, error) {
	ctx, span := a.tracer.Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("http.request.method", http.MethodGet),
			attribute.String("server.address", urlHost(urlStr)),
			attribute.String("chora.tenant_id", tenantID),
		),
	)
	defer span.End()

	callCtx, cancel := context.WithTimeout(ctx, a.cfg.PerCallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, urlStr, nil)
	if err != nil {
		span.SetStatus(codes.Error, "build request")
		span.RecordError(err)
		return nil, fmt.Errorf("%w: build request: %v", ErrUpstream, err)
	}
	req.Header.Set("Accept", "application/json")
	auth := AuthCtxFromContext(ctx)
	if auth.Traceparent != "" {
		req.Header.Set("traceparent", auth.Traceparent)
	}
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	if auth.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+auth.Bearer)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		span.SetStatus(codes.Error, "transport error")
		span.RecordError(err)
		return nil, fmt.Errorf("%w: transport: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		span.SetStatus(codes.Error, fmt.Sprintf("upstream %d", resp.StatusCode))
		return nil, fmt.Errorf("%w: %d", ErrUpstream, resp.StatusCode)
	}
	return out, nil
}
