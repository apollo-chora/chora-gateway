// weakness_resume_loader.go — env-driven bootstrap for the Growth-Edge HITL
// resume BFF route (ADR-205 WS-2/WS-8, CHO-1966).
//
// Wires POST /api/v1/me/growth-edges/uploads/{upload_id}/resume
// (weakness_resume_handler.go) to chora-ai-kernel-orchestrator's
// /v1/orchestrator/weakness/{upload_id}/resume over clients.WeaknessResumeClient.
//
// Env contract (per feedback_no_inline_config — no inline URLs):
//
//	AI_KERNEL_ORCHESTRATOR_URL              orchestrator base URL.
//	                                        ABSENT → loader returns (nil, nil)
//	                                        and the resume route answers 503
//	                                        WEAKNESS_RESUME_UNAVAILABLE
//	                                        (degraded, never a silent 404,
//	                                        never a fatal boot).
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
)

const (
	// EnvAIKernelOrchestratorURL supplies the chora-ai-kernel-orchestrator
	// base URL.
	EnvAIKernelOrchestratorURL = "AI_KERNEL_ORCHESTRATOR_URL"
)

// NewWeaknessResumeHandlerFromEnv constructs the Growth-Edge resume handler
// from env. Returns (nil, nil) when AI_KERNEL_ORCHESTRATOR_URL is unset — the
// caller still mounts WithWeaknessResumeRoute so the route 503s instead of
// 404ing.
func NewWeaknessResumeHandlerFromEnv(ctx context.Context) (*httpadapter.WeaknessResumeHandler, error) {
	baseURL := strings.TrimSpace(os.Getenv(EnvAIKernelOrchestratorURL))
	if baseURL == "" {
		return nil, nil
	}

	cc, err := clients.NewWeaknessResumeClient(clients.WeaknessResumeClientConfig{
		BaseURL: baseURL,
	})
	if err != nil {
		return nil, fmt.Errorf("weakness_resume: client: %w", err)
	}
	h, err := httpadapter.NewWeaknessResumeHandler(cc)
	if err != nil {
		return nil, fmt.Errorf("weakness_resume: handler: %w", err)
	}
	return h, nil
}
