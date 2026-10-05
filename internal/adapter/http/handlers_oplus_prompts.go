package httpadapter

// handlers_oplus_prompts.go - CHO-2368 (ADR-197 M-D read slice): the
// /bff/oplus/prompts/{agent_id}/versions[/{version}] catalogue passthrough.
//
// Sits UNDER the existing exact route /bff/oplus/prompts (CHO-2364 evidence
// panel, chora-observability upstream). This subtree serves the CONTENT
// catalogue instead - the chora-ai-kernel-orchestrator prompt registry
// (baselines + gated override plans, segment bodies with locked flags) - so
// the O+ modal can show the actual instructions each version runs.
//
// Raw passthrough (never re-marshal) under the OPlusEnvelope, mirroring
// AgentPrompts: upstream additions reach the FE without a gateway deploy.
// Inherits the /bff/oplus/ prefix auditor gate + JWT prefix like every
// sibling. A 404 from the registry (unknown agent/version) surfaces as a real
// 404 so the FE can distinguish not_found from an upstream outage
// (state:error).

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// errAIKernelUnconfigured surfaces through the O+ envelope when the gateway
// boots without AI_KERNEL_ORCHESTRATOR_URL (degraded, never fatal).
var errAIKernelUnconfigured = errors.New(
	"aikernel upstream unconfigured (set AI_KERNEL_ORCHESTRATOR_URL)")

// PromptCatalogueBodyResponse wraps the orchestrator's registry payload
// verbatim under the O+ envelope.
type PromptCatalogueBodyResponse struct {
	OPlusEnvelope
	Registry json.RawMessage `json:"registry"` // RAW passthrough - never re-marshal
}

// PromptCatalogue serves GET /bff/oplus/prompts/{agent_id}/versions and
// GET /bff/oplus/prompts/{agent_id}/versions/{version}.
func (h *OPlusHandler) PromptCatalogue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	agentID, version, ok := parsePromptCataloguePath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "GATEWAY_NOT_FOUND",
			"expected /bff/oplus/prompts/{agent_id}/versions[/{version}]")
		return
	}
	if h.AIKernel == nil {
		writeOPlusError(w, "PromptCatalogue", errAIKernelUnconfigured, h.now())
		return
	}
	ctx := withUpstreamAuthCtx(r)
	tenantID, _ := upstreamCtx(ctx)

	var (
		raw []byte
		err error
	)
	if version == "" {
		raw, err = h.AIKernel.GetPromptVersions(ctx, tenantID, agentID)
	} else {
		raw, err = h.AIKernel.GetPromptVersionContent(ctx, tenantID, agentID, version)
	}
	if err != nil {
		writeOPlusError(w, "PromptCatalogue", err, h.now())
		return
	}
	if raw == nil {
		// The registry answered 404 - a real not-found, not an outage.
		writeError(w, http.StatusNotFound, "GATEWAY_NOT_FOUND",
			"no catalogue entry for that agent/version")
		return
	}
	writeJSON(w, http.StatusOK, PromptCatalogueBodyResponse{
		OPlusEnvelope: OPlusEnvelope{State: "live", FetchedAt: h.now()},
		Registry:      json.RawMessage(raw),
	})
}

// parsePromptCataloguePath extracts (agent_id, version) from the subtree path.
// version is "" for the list form. Any other shape is not-found.
func parsePromptCataloguePath(path string) (agentID, version string, ok bool) {
	rest := strings.TrimPrefix(path, "/bff/oplus/prompts/")
	if rest == path { // prefix absent - not this subtree
		return "", "", false
	}
	parts := strings.Split(strings.TrimSuffix(rest, "/"), "/")
	switch {
	case len(parts) == 2 && parts[0] != "" && parts[1] == "versions":
		return parts[0], "", true
	case len(parts) == 3 && parts[0] != "" && parts[1] == "versions" && parts[2] != "":
		return parts[0], parts[2], true
	}
	return "", "", false
}
