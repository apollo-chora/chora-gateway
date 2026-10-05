// oplus_loader.go — env-driven bootstrap for the O+ (Observability+) BFF
// surface introduced in Phase C of the O+ hydration plan
// (`docs/architecture/poc/atomic-napping-spring.md` §"Phase C — BFF wiring").
//
// The loader composes three narrow real-wire upstream clients
// (chora-governance gRPC + HTTP, chora-observability HTTP, chora-a2a HTTP)
// into a single `*httpadapter.OPlusHandler` that serves the 5 new BFF routes:
//
//	GET /bff/oplus/dashboard
//	GET /bff/oplus/dimensions
//	GET /bff/oplus/agents
//	GET /bff/oplus/governance     — Phase C replaces the FakeUpstream variant
//	GET /bff/oplus/a2a
//
// Env contract (per `feedback_no_inline_config` + `feedback_no_stubs_real_wiring`):
//
//	CHORA_GOVERNANCE_GRPC_ADDR     — chora-governance gRPC dial address.
//	                                 REQUIRED unless CHORA_GATEWAY_UPSTREAM_FAKE=true.
//	CHORA_GOVERNANCE_HTTP_ADDR     — chora-governance HTTP base URL for the
//	                                 HITL + per-dimension-rubric endpoints.
//	                                 REQUIRED unless fake-mode.
//	CHORA_OBSERVABILITY_HTTP_ADDR  — chora-observability HTTP base URL.
//	                                 REQUIRED unless fake-mode. Falls back to
//	                                 SVC_OBSERVABILITY_URL inside the client
//	                                 (LoadObservabilityConfigFromEnv) but the
//	                                 loader still enforces presence so ops
//	                                 has an explicit signal.
//	CHORA_A2A_HTTP_ADDR            — chora-a2a HTTP base URL. EMPTY is a
//	                                 valid signal: chora-a2a backend is
//	                                 not yet deployed; the BFF handler emits
//	                                 {mode:'pending', mock:{...}}. Loader
//	                                 NEVER fails on empty.
//	CHORA_GATEWAY_UPSTREAM_FAKE    — "true" skips all real wiring; the
//	                                 OPlusHandler is built with nil clients
//	                                 (each call returns an error envelope)
//	                                 so smoke tests can run without backends.
//	CHORA_GATEWAY_DATA_LINEAGE_PATH — optional YAML path for the Data
//	                                 Governance tab projection.
//
// Per `feedback_no_stubs_real_wiring`: real wiring is the production posture.
// The fake-mode gate is opt-in and surfaces a warning log so the drift is
// visible. Outside fake-mode the loader fails loud — main.go log.Fatalfs the
// returned error so the pod CrashLoopBackOffs until ops fixes the config.
//
// Trust model: mesh-internal hops use the Cloud Service Mesh sidecar for
// mTLS (PeerAuthentication=PERMISSIVE per the chora-prod policy). The gRPC
// dial uses `insecure.NewCredentials()` (consistent with chora-payments /
// chora-delivery / chora-identity gRPC clients) because TLS is terminated
// by the sidecar, not in-process.
//
// Caller pattern in main():
//
//	oplusHandler, oplusCleanup, oplusResolver, err := NewOPlusHandlerFromEnv(ctx)
//	if err != nil {
//	    log.Fatalf("oplus loader failed: %v", err)
//	}
//	defer oplusCleanup()
//	mux = httpadapter.WithOPlusRoutes(mux, oplusHandler)
//	... wrap with WithChoraSessionOnPrefixes(mux, validator) ...
//	gated = middleware.AuditorGate(oplusResolver, jwtGated)
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/apollo-chora/chora-common/auth/chorasession"
	"github.com/apollo-chora/chora-common/auth/servicemesh"
	governancev1 "github.com/apollo-chora/chora-contracts/gen/go/chora/services/governance/v1"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
	"github.com/apollo-chora/chora-gateway/internal/middleware"
)

// Env var names — exported so the k8s manifest agent + ops tooling can match
// against a single canonical list.
const (
	EnvGovernanceGRPCAddr     = "CHORA_GOVERNANCE_GRPC_ADDR"
	EnvGovernanceHTTPAddr     = "CHORA_GOVERNANCE_HTTP_ADDR"
	EnvObservabilityHTTPAddr  = "CHORA_OBSERVABILITY_HTTP_ADDR"
	EnvA2AHTTPAddr            = "CHORA_A2A_HTTP_ADDR"
	EnvGatewayUpstreamFake    = "CHORA_GATEWAY_UPSTREAM_FAKE"
	EnvGatewayDataLineagePath = "CHORA_GATEWAY_DATA_LINEAGE_PATH"
)

// ErrOPlusBackendMissing is returned when one of the required Phase-C
// upstream env vars is empty AND fake-mode is not enabled. The caller
// log.Fatalfs on this so the pod CrashLoopBackOffs until ops sets the var.
var ErrOPlusBackendMissing = errors.New("oplus: required backend env var missing")

// NewOPlusHandlerFromEnv constructs the O+ BFF handler + cleanup +
// auditor-role resolver entirely from env vars.
//
// Returns:
//   - handler:  the wired *httpadapter.OPlusHandler (never nil on success)
//   - cleanup:  closes the underlying gRPC conn (no-op when fake-mode)
//   - resolver: a middleware.RoleResolver that reads the auditor claim from
//     the validated chora-session JWT context (set by RequireChoraSessionJWT)
//     — caller wires this into middleware.AuditorGate
//   - err:      ErrOPlusBackendMissing when a required env var is empty and
//     fake-mode is off; nil otherwise.
//
// Behaviour matrix:
//
//	CHORA_GATEWAY_UPSTREAM_FAKE=true  →  nil clients, handler returns error
//	                                     envelopes via the existing nil-safe
//	                                     paths; routes still registered.
//	                                     (Loader logs the drift warning.)
//
//	fake-mode off, all 3 required envs set:
//	                                  →  real wiring; cleanup closes gRPC conn.
//
//	fake-mode off, ANY required env unset:
//	                                  →  (nil, no-op, nil-resolver,
//	                                     ErrOPlusBackendMissing)
//
//	A2A addr always optional (empty = pending mode).
func NewOPlusHandlerFromEnv(_ context.Context) (*httpadapter.OPlusHandler, func() error, middleware.RoleResolver, error) {
	resolver := middleware.NewChoraSessionRoleResolver(
		choraSessionClaimsFromRequest,
		meshClaimsFromRequest,
	)

	if upstream.IsFakeUpstreamAllowed() {
		// Fake mode: build the handler with nil clients. Each call returns
		// the discriminated-union error envelope, which is the explicit
		// "no backend" signal for smoke tests. Routes still register.
		h := httpadapter.NewOPlusHandler(nil, nil, nil, httpadapter.LoadDataLineageYAMLFromEnv())
		h.DisplayCurrency, h.FxRate = httpadapter.LoadCostDisplayConfigFromEnv()
		return h, func() error { return nil }, resolver, nil
	}

	govGRPCAddr := strings.TrimSpace(os.Getenv(EnvGovernanceGRPCAddr))
	govHTTPAddr := strings.TrimSpace(os.Getenv(EnvGovernanceHTTPAddr))
	obsHTTPAddr := strings.TrimSpace(os.Getenv(EnvObservabilityHTTPAddr))
	a2aHTTPAddr := strings.TrimSpace(os.Getenv(EnvA2AHTTPAddr))

	// Boot-fail-loud on the three REQUIRED addresses. A2A may be empty
	// (pending-deployment signal — handler renders {mode:'pending'}).
	if govGRPCAddr == "" {
		return nil, func() error { return nil }, nil,
			fmt.Errorf("%w: %s", ErrOPlusBackendMissing, EnvGovernanceGRPCAddr)
	}
	if govHTTPAddr == "" {
		return nil, func() error { return nil }, nil,
			fmt.Errorf("%w: %s", ErrOPlusBackendMissing, EnvGovernanceHTTPAddr)
	}
	if obsHTTPAddr == "" {
		return nil, func() error { return nil }, nil,
			fmt.Errorf("%w: %s", ErrOPlusBackendMissing, EnvObservabilityHTTPAddr)
	}

	// Dial chora-governance gRPC. Cloud Service Mesh sidecar handles mTLS;
	// in-process channel uses insecure credentials per the canonical pattern
	// (mirrors payments_loader.go).
	govConn, err := grpc.NewClient(
		govGRPCAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, func() error { return nil }, nil,
			fmt.Errorf("oplus: dial governance gRPC %s: %w", govGRPCAddr, err)
	}

	govClient := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{
			GRPCAddr: govGRPCAddr,
			HTTPAddr: govHTTPAddr,
		},
		RPC:  governancev1.NewGovernanceClient(govConn),
		HTTP: &http.Client{Timeout: upstream.DefaultGovernanceCallTimeout},
	})

	obsClient := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: obsHTTPAddr,
	}, nil)

	a2aClient := upstream.NewA2AClient(upstream.A2AConfig{
		HTTPAddr: a2aHTTPAddr, // empty is legal — handler emits pending mode
	}, nil)

	dataLineageYAML := httpadapter.LoadDataLineageYAMLFromEnv()

	h := httpadapter.NewOPlusHandler(govClient, obsClient, a2aClient, dataLineageYAML)
	h.DisplayCurrency, h.FxRate = httpadapter.LoadCostDisplayConfigFromEnv()

	// CHO-2368 prompt catalogue upstream. Shares AI_KERNEL_ORCHESTRATOR_URL
	// with the weakness-resume loader; an empty addr degrades those routes to
	// the state:error envelope (never fatal, never 404), mirroring that
	// loader's posture.
	if aikCfg := upstream.LoadAIKernelConfigFromEnv(); aikCfg.HTTPAddr != "" {
		h.AIKernel = upstream.NewAIKernelClient(aikCfg, nil)
		log.Printf("oplus aikernel catalogue upstream WIRED (%s)", aikCfg.HTTPAddr)
	} else {
		log.Printf("oplus aikernel catalogue upstream NOT configured (%s empty) - catalogue routes degrade to error envelopes", upstream.EnvAIKernelHTTPAddr)
	}

	cleanup := func() error {
		return govConn.Close()
	}
	return h, cleanup, resolver, nil
}

// choraSessionClaimsFromRequest adapts httpadapter.ChoraSessionClaimsFromContext
// (which takes context.Context) to middleware.NewChoraSessionRoleResolver's
// expected `func(*http.Request)` signature. Trivial shim — kept here so the
// http adapter package does not need a parallel Request-flavoured extractor.
func choraSessionClaimsFromRequest(r *http.Request) (*chorasession.Claims, bool) {
	if r == nil {
		return nil, false
	}
	return httpadapter.ChoraSessionClaimsFromContext(r.Context())
}

// meshClaimsFromRequest mirrors choraSessionClaimsFromRequest for the
// servicemesh.MeshClaims fallback path.
func meshClaimsFromRequest(r *http.Request) (*servicemesh.MeshClaims, bool) {
	if r == nil {
		return nil, false
	}
	return httpadapter.MeshClaimsFromContext(r.Context())
}
