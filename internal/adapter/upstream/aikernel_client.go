package upstream

// aikernel_client.go - CHO-2368 gateway → chora-ai-kernel-orchestrator prompt
// catalogue reads (ADR-197 M-D read slice).
//
// Family-1 O+ narrow client (the handlers_oplus.go family): raw JSON
// passthrough of the orchestrator's /v1/prompt-registry read API, never
// re-marshalled, so upstream additions reach the FE without a gateway deploy.
// The base URL rides AI_KERNEL_ORCHESTRATOR_URL - the SAME env the weakness
// resume loader uses (one gateway→orchestrator upstream address, two client
// families). Mesh edge: the ai-kernel namespace default-deny
// AuthorizationPolicy allows the gateway SA onto
// /v1/prompt-registry/agents/* (chora-infra authz-allow-gateway.yaml).

import (
	"context"
	"encoding/json"
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

	"github.com/apollo-chora/chora-common/auth/servicemesh"
)

// DefaultAIKernelCallTimeout bounds one catalogue read. The orchestrator
// answers from indexed registry tables; 5s matches the sibling O+ clients.
const DefaultAIKernelCallTimeout = 5 * time.Second

// EnvAIKernelHTTPAddr is the canonical base-URL env var (shared with the
// weakness resume loader - see cmd/server/weakness_resume_loader.go).
const EnvAIKernelHTTPAddr = "AI_KERNEL_ORCHESTRATOR_URL"

// AIKernelConfig configures the ai-kernel orchestrator upstream.
type AIKernelConfig struct {
	HTTPAddr       string
	PerCallTimeout time.Duration
}

// LoadAIKernelConfigFromEnv reads AI_KERNEL_ORCHESTRATOR_URL. An empty addr is
// legal - NewAIKernelClient still constructs and every call returns
// ErrUpstream (the O+ envelope collapses it to state:error).
func LoadAIKernelConfigFromEnv() AIKernelConfig {
	return AIKernelConfig{
		HTTPAddr:       strings.TrimSpace(os.Getenv(EnvAIKernelHTTPAddr)),
		PerCallTimeout: DefaultAIKernelCallTimeout,
	}
}

// AIKernelClient is the narrow O+ read client for the prompt catalogue.
type AIKernelClient struct {
	cfg    AIKernelConfig
	http   *http.Client
	tracer trace.Tracer
}

// NewAIKernelClient constructs the client. A nil httpClient gets a per-call
// timeout default, mirroring NewObservabilityClient.
func NewAIKernelClient(cfg AIKernelConfig, httpClient *http.Client) *AIKernelClient {
	if cfg.PerCallTimeout <= 0 {
		cfg.PerCallTimeout = DefaultAIKernelCallTimeout
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: cfg.PerCallTimeout}
	}
	return &AIKernelClient{
		cfg:    cfg,
		http:   httpClient,
		tracer: otel.Tracer("chora-gateway/upstream/aikernel"),
	}
}

// GetPromptVersions returns the orchestrator's per-agent catalogue version
// list verbatim. (nil, nil) when the agent has no catalogue rows (404).
func (c *AIKernelClient) GetPromptVersions(ctx context.Context, tenantID, agentID string) (json.RawMessage, error) {
	path := "/v1/prompt-registry/agents/" + url.PathEscape(agentID) + "/versions"
	return c.getJSON(ctx, "aikernel.GetPromptVersions", path, tenantID)
}

// GetPromptVersionContent returns one version's segment content verbatim.
// (nil, nil) when the agent/version pair is unknown (404).
func (c *AIKernelClient) GetPromptVersionContent(ctx context.Context, tenantID, agentID, version string) (json.RawMessage, error) {
	path := "/v1/prompt-registry/agents/" + url.PathEscape(agentID) +
		"/versions/" + url.PathEscape(version)
	return c.getJSON(ctx, "aikernel.GetPromptVersionContent", path, tenantID)
}

// getJSON mirrors the observability client's httpGetJSON: OTel client span,
// per-call timeout, traceparent + Bearer + tenant + canonical mesh-claim
// headers, 404 → (nil, nil), other >=400 → ErrUpstream, body verbatim.
func (c *AIKernelClient) getJSON(ctx context.Context, spanName, path, tenantID string) (json.RawMessage, error) {
	if strings.TrimSpace(c.cfg.HTTPAddr) == "" {
		return nil, fmt.Errorf("%w: aikernel upstream unconfigured (set %s)", ErrUpstream, EnvAIKernelHTTPAddr)
	}
	urlStr := strings.TrimRight(c.cfg.HTTPAddr, "/") + path

	ctx, span := c.tracer.Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("http.request.method", http.MethodGet),
			attribute.String("server.address", urlHost(urlStr)),
			attribute.String("chora.tenant_id", tenantID),
		),
	)
	defer span.End()

	callCtx, cancel := context.WithTimeout(ctx, c.cfg.PerCallTimeout)
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
	// Canonical mesh metadata - absent x-mesh-user-roles denies 100% on any
	// role-gated upstream (fails closed), so stamp it like every O+ client.
	mesh := servicemesh.MarshalToHeaders(servicemesh.MeshClaims{
		GCID:        auth.GCID,
		TenantID:    tenantID,
		Roles:       auth.Roles,
		RoleSummary: auth.RoleSummary,
	})
	for k, vs := range mesh {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	resp, err := c.http.Do(req)
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
	if !json.Valid(out) {
		span.SetStatus(codes.Error, "invalid upstream JSON")
		return nil, fmt.Errorf("%w: invalid JSON body", ErrUpstream)
	}
	return json.RawMessage(out), nil
}
