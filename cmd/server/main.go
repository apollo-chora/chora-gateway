// Package main is the chora-gateway service entrypoint.
//
// Service: chora-gateway (BFF for the 5 CHORA Angular surfaces — A+, C+,
// H+, O+, R+).
// Project: chora-local (Team 3 / Platform).
// Surface fan-out: aggregates upstream domain services into per-surface views.
// Trace role: ROOT — generates W3C traceparent if absent, propagates to all
// upstream calls (per Tier 3 D13).
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/adapter/inmem"
	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/companionbridge"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/kgexplore"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/medashboard"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/notifications"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/readiness"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/social"
	bffgraphql "github.com/apollo-chora/chora-gateway/internal/graphql"
	"github.com/apollo-chora/chora-gateway/internal/graphql/resolvers"
	"github.com/apollo-chora/chora-gateway/internal/middleware"
	"github.com/apollo-chora/chora-gateway/internal/observability"
)

const (
	serviceName = "chora-gateway"
	version     = "0.1.0"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Bucket 4 (2026-05-14 arch-gap): boot-time fail-loud env-var gate.
	// Auth is non-negotiable in prod posture — IDP_JWT_DISABLED was REMOVED
	// in the same commit. EVERY required env var MUST be set for the
	// process to bind a port. Missing var → log.Fatalf, pod CrashLoopBackOff,
	// ops fixes config.
	if err := CheckBootEnv(); err != nil {
		log.Fatalf("boot env gate failed: %v", err)
	}

	shutdown, err := observability.Init(ctx)
	if err != nil {
		log.Printf("observability init failed (non-fatal): %v", err)
	}
	defer func() {
		if shutdown == nil {
			return
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(shutdownCtx); err != nil {
			log.Printf("trace shutdown error: %v", err)
		}
	}()

	routesRepo := inmem.NewRouteRepository()
	sessionsRepo := inmem.NewSessionRepository()

	// Phase 6 — upstream client selector. Defaults to FakeUpstream so the
	// rollback path is zero-regression. BFF_HTTPUPSTREAM_ENABLED=true wires
	// the real HTTP fan-out to chora-consumption/-creation/-sharing/-tenancy/
	// -governance/-observability/-delivery. URLs come from SVC_*_URL env vars
	// per feedback_no_inline_config.
	var up upstream.Client
	if os.Getenv("BFF_HTTPUPSTREAM_ENABLED") == "true" {
		httpUpCfg := upstream.LoadHTTPConfigFromEnv()
		up = upstream.NewHTTPUpstream(httpUpCfg)
		log.Printf("upstream: HTTPUpstream WIRED (phase 6) — cons=%s crea=%s shar=%s ten=%s gov=%s obs=%s del=%s",
			httpUpCfg.ConsumptionURL, httpUpCfg.CreationURL, httpUpCfg.SharingURL,
			httpUpCfg.TenancyURL, httpUpCfg.GovernanceURL, httpUpCfg.ObservabilityURL,
			httpUpCfg.DeliveryURL)
	} else {
		up = upstream.NewFakeUpstream()
		log.Printf("upstream: FakeUpstream (default) — set BFF_HTTPUPSTREAM_ENABLED=true to use real HTTP fan-out")
	}

	// Phyllis MVP aggregator (M13.A) — fans out 11 BFF routes to chora-identity,
	// chora-tenancy, chora-creation, chora-delivery, chora-consumption, and
	// chora-model-broker-router. Service URLs are sourced from SVC_*_URL env
	// vars per the no-inline-config rule (memory feedback_no_inline_config).
	// Per spec docs/m13/phyllis-mvp-2026-05-08.md.
	phyllisCfg := phyllis.LoadConfigFromEnv()
	phyllisAgg := phyllis.New(phyllisCfg, nil) // emit=nil until Pub/Sub publisher wired in M12

	// GraphQL federation gateway (Phase 31, CHO-1288 to CHO-1300) — wires the
	// 4 federated subschemas (engagement, gamification, companion, circle) at
	// /graphql alongside the existing REST routes. Per CLAUDE.md §4 GraphQL
	// is for learner-facing reads; REST + Pub/Sub remain unchanged.
	graphqlSchema := bffgraphql.NewSchema(
		resolvers.NewEngagementResolver(up, httpadapter.IdentityFromContext),
		resolvers.NewGamificationResolver(up, httpadapter.IdentityFromContext),
		resolvers.NewCompanionResolver(up, httpadapter.IdentityFromContext),
		resolvers.NewCircleResolver(up, httpadapter.IdentityFromContext),
	)

	// chora-session HS256 Validator — Bucket 2/3 of the 2026-05-14 arch-gap
	// closure. Validates Chora session JWTs minted by /api/v1/auth/session/mint
	// on the /api/* trust boundary. Replaces the previous JWKS-backed
	// the previous validator that produced "unknown signing key (kid)"
	// 401s for every /api/me call.
	//
	// Signer key + issuer + audience all sourced from env / Secret Manager
	// per `feedback_no_inline_config`. Fail-loud on any missing input — the
	// boot env gate above has already enforced presence so this returning
	// an error is exclusively a Secret Manager / network issue.
	choraSessionValidator, choraSessionCleanup, err := NewChoraSessionValidatorFromEnv(ctx)
	if err != nil {
		log.Fatalf("chora-session validator init failed: %v", err)
	}
	defer func() {
		if choraSessionCleanup != nil {
			if err := choraSessionCleanup(); err != nil {
				log.Printf("chora-session validator cleanup error: %v", err)
			}
		}
	}()
	log.Printf("chora-session validator wired (issuer=%s, aud=%s) — /api/* gated by HS256 chora-session",
		os.Getenv(EnvChoraSessionIssuerURL), os.Getenv(EnvChoraSessionAudienceURL))

	// C+ social aggregator (CHO-1457) — Phyllis MVP basic social
	// discovery stubs (feed, profile, share, reactions, comments,
	// public courses). Reads SVC_SHARING_URL + SVC_DELIVERY_URL env
	// vars per `feedback_no_inline_config`; fan-out to chora-sharing
	// lands when learner-facing API ships (M12+).
	socialAgg := social.New(social.Config{
		SharingURL:  os.Getenv("SVC_SHARING_URL"),
		DeliveryURL: os.Getenv("SVC_DELIVERY_URL"),
		// 90s — the profiler generate endpoint calls the interest_profiler
		// ADK agent → model-gateway → LLM (UMANS), which takes 30-60s.
		// The default 30s times out the LLM call → 504 to the frontend.
		PerCallTimeout: 90 * time.Second,
	})

	// JWT-gated mux (when validator wired) wraps the social mux. We
	// build the inner mux first so the JWT prefix-gate sees both
	// Phyllis + social paths in one handler tree.
	innerMux := httpadapter.NewRouterWithSocial(
		routesRepo, sessionsRepo, up, phyllisAgg, socialAgg, graphqlSchema.Handler(),
	)

	// Mint endpoint (POST /api/v1/auth/session/mint): exchanges a
	// username/password pair for a Chora session JWT. Wrapped OUTSIDE the IDP
	// JWT prefix gate because this IS the auth-establishing endpoint — clients
	// haven't got a Chora session yet when they hit it.
	mintHandler, mintCleanup, err := NewMintHandlerFromEnv(ctx, sessionsRepo)
	if err != nil {
		log.Fatalf("mint handler init failed: %v", err)
	}
	defer func() {
		if mintCleanup != nil {
			if err := mintCleanup(); err != nil {
				log.Printf("mint cleanup error: %v", err)
			}
		}
	}()
	if mintHandler == nil {
		log.Printf("mint handler DISABLED (MINT_DISABLED=true) — POST /api/v1/auth/session/mint will 404")
	} else {
		log.Printf("mint handler wired — POST /api/v1/auth/session/mint live")
	}

	// PROD-D (m14.iter5g) — ADR-149 Companion Growth + Egg purchase BFF routes.
	// Proxies /api/v1/me/companions/* → chora-consumption and
	// /api/v1/companion-eggs/* → chora-tenancy with mesh-trust headers, per-route
	// timeouts, snake→camel re-marshal. Stripe webhook is intentionally direct
	// to chora-tenancy (signature verification must happen at the receiving
	// end). URLs come from SVC_CONSUMPTION_URL + SVC_TENANCY_URL per
	// feedback_no_inline_config.
	bridgeCfg := companionbridge.LoadConfigFromEnv()
	var companionBridge *companionbridge.Bridge
	if bridgeCfg.ConsumptionURL != "" || bridgeCfg.TenancyURL != "" {
		companionBridge = companionbridge.New(bridgeCfg)
		log.Printf("companion-bridge: WIRED — cons=%s ten=%s hatch_timeout=%s",
			bridgeCfg.ConsumptionURL, bridgeCfg.TenancyURL, bridgeCfg.HatchTimeout)
	} else {
		log.Printf("companion-bridge: DISABLED (SVC_CONSUMPTION_URL + SVC_TENANCY_URL both unset)")
	}
	innerMuxWithBridge := httpadapter.WithCompanionBridge(innerMux, companionBridge)

	// In-app notifications BFF proxy (SS dress-rehearsal v2 paydown) — proxies
	// /api/v1/notifications/* → chora-notifications with mesh-trust headers.
	// FE in-app notifications feature was blocked because the route 404'd at
	// the gateway. Returns nil when SVC_NOTIFICATIONS_URL unset — caller skips
	// registration so callers see a clean 404 in unconfigured dev envs.
	notificationsAgg := notifications.New(notifications.LoadConfigFromEnv())
	if notificationsAgg != nil {
		log.Printf("notifications-bridge: WIRED — url=%s", os.Getenv("SVC_NOTIFICATIONS_URL"))
	} else {
		log.Printf("notifications-bridge: DISABLED (SVC_NOTIFICATIONS_URL unset)")
	}
	innerMuxWithNotifications := httpadapter.WithNotificationsBridge(innerMuxWithBridge, notificationsAgg)

	// Per-user Knowledge-Graph hexagonal exploration BFF proxy (D1.4) —
	// proxies GET /api/v1/consumption/kg/explore/{atom_id} → chora-consumption
	// fog_handler (ADR-143 §7) with mesh-trust headers. FE Discovery KG canvas
	// (P1.3) was blocked because the route 404'd at the gateway edge. Returns
	// nil when SVC_CONSUMPTION_URL unset — caller skips registration so callers
	// see a clean 404 in unconfigured dev envs. SVC_CONSUMPTION_URL is already
	// a required boot env var (CheckBootEnv) per feedback_no_inline_config.
	kgExploreAgg := kgexplore.New(kgexplore.LoadConfigFromEnv())
	if kgExploreAgg != nil {
		log.Printf("kg-explore-bridge: WIRED — url=%s", os.Getenv("SVC_CONSUMPTION_URL"))
	} else {
		log.Printf("kg-explore-bridge: DISABLED (SVC_CONSUMPTION_URL unset)")
	}
	innerMuxWithKGExplore := httpadapter.WithKGExploreBridge(innerMuxWithNotifications, kgExploreAgg)

	// Per-user Knowledge-Graph hexagon CANVAS BFF routes (ADR-143, contract
	// learner-knowledge-graph.yaml v1.1) — proxies the A+ canvas surface
	// (/api/v1/me/knowledge-graph/clusters[/...subtrees] + junctions/{jid}/decide
	// + /api/v1/tenants/{tid}/knowledge-graph/config) to chora-consumption's
	// kg_canvas_handler with mesh-trust headers. Reuses the SAME phyllis
	// aggregator (call conventions + SVC_CONSUMPTION_URL — no new deployment
	// config per feedback_no_inline_config). nil when SVC_CONSUMPTION_URL is
	// unset so the routes 404 cleanly in unconfigured dev envs. See
	// internal/adapter/http/routes_kg_canvas.go.
	var kgCanvasAgg *phyllis.Aggregator
	if phyllisCfg.ConsumptionURL != "" {
		kgCanvasAgg = phyllisAgg
		log.Printf("kg-canvas-bridge: WIRED — url=%s", phyllisCfg.ConsumptionURL)
	} else {
		log.Printf("kg-canvas-bridge: DISABLED (SVC_CONSUMPTION_URL unset)")
	}
	innerMuxWithKGCanvas := httpadapter.WithKGCanvasRoutes(innerMuxWithKGExplore, kgCanvasAgg)

	// A6 (2026-05-14) — gatewayproxy bridge: wires the missing non-auth
	// /api/* gateway routes per docs/m13/handoff-fe-to-be-service-2026-05-14.md
	// §A6. FE token-probed each route and found GATEWAY_ROUTE_NOT_FOUND — they
	// were not routed at the gateway at all. This bridge proxies each to its
	// real downstream path (chora-tenancy / -delivery / -consumption /
	// -notifications). Composed INSIDE the chora-session JWT gate (the bridge
	// prefixes are in DefaultJWTGatedPrefixes / prefix-covered by it) so
	// RequireChoraSessionJWT stamps the validated mesh claims first. Reads the
	// SAME SVC_*_URL env vars the other aggregators already use — no new
	// deployment config (feedback_no_inline_config). Returns nil when ALL four
	// URLs are unset — caller skips registration so routes 404 cleanly in
	// unconfigured dev envs.
	gatewayProxyAgg := gatewayproxy.New(gatewayproxy.LoadConfigFromEnv())
	if gatewayProxyAgg != nil {
		log.Printf("gatewayproxy-bridge: WIRED — ten=%s del=%s cons=%s notif=%s ident=%s crea=%s",
			os.Getenv("SVC_TENANCY_URL"), os.Getenv("SVC_DELIVERY_URL"),
			os.Getenv("SVC_CONSUMPTION_URL"), os.Getenv("SVC_NOTIFICATIONS_URL"),
			os.Getenv("SVC_IDENTITY_URL"), os.Getenv("SVC_CREATION_URL"))
	} else {
		log.Printf("gatewayproxy-bridge: DISABLED (SVC_TENANCY_URL + SVC_DELIVERY_URL + SVC_CONSUMPTION_URL + SVC_NOTIFICATIONS_URL + SVC_IDENTITY_URL + SVC_CREATION_URL all unset)")
	}
	innerMuxWithGatewayProxy := httpadapter.WithGatewayProxy(innerMuxWithKGCanvas, gatewayProxyAgg)

	// R+ Rhythm+ delivery proxy bridge (2026-05-26) — verbatim passthrough
	// to chora-delivery for /api/bookings, /api/certifications, /v1/campus,
	// /v1/me/applications. Reuses the SAME gatewayProxyAgg (reads
	// SVC_DELIVERY_URL — no new env var per feedback_no_inline_config).
	// Add-only composition: parallel sessions editing WithGatewayProxy
	// don't collide with R+ work because the bridge owns its own paths +
	// falls through for everything else. JWT-gated via the prefixes added
	// to DefaultJWTGatedPrefixes in jwt_auth.go. See
	// services/chora-gateway/internal/adapter/http/rplus_delivery_proxy_handler.go
	// + the R+ build-out plan at ~/.claude/plans/jaunty-strolling-key.md M1.
	if gatewayProxyAgg != nil {
		log.Printf("rplus-delivery-proxy-bridge: WIRED — del=%s", os.Getenv("SVC_DELIVERY_URL"))
	} else {
		log.Printf("rplus-delivery-proxy-bridge: DISABLED (SVC_DELIVERY_URL unset)")
	}
	innerMuxWithRplusProxy := httpadapter.WithRplusDeliveryProxy(innerMuxWithGatewayProxy, gatewayProxyAgg)

	// A6 follow-up (2026-05-14, CHO-1545) — medashboard bridge: serves the
	// composing GET /api/me/dashboard route per
	// docs/m13/handoff-fe-to-be-service-2026-05-14.md §A6 GAP #2. No
	// downstream serves /me/dashboard — this aggregator fans out to
	// chora-identity /me + chora-consumption /v1/me/streak +
	// /v1/me/learning-paths and composes the FE DashboardSummary DTO,
	// degrading gracefully on a partial downstream failure (never a
	// whole-500). For a session holding the instructor or admin role it also
	// fans out to chora-delivery /api/v1/instructors/{gcid}/courses. Composed
	// INSIDE the chora-session JWT gate (/api/me is in DefaultJWTGatedPrefixes
	// so RequireChoraSessionJWT stamps the validated mesh claims first). Reads
	// the SAME SVC_IDENTITY_URL + SVC_CONSUMPTION_URL + SVC_DELIVERY_URL env
	// vars the other aggregators use, so there is no new deployment config
	// (feedback_no_inline_config). Returns nil when all three are unset, and
	// the caller then skips registration so the route 404s cleanly in
	// unconfigured dev envs.
	meDashboardAgg := medashboard.New(medashboard.LoadConfigFromEnv())
	if meDashboardAgg != nil {
		log.Printf("medashboard-bridge: WIRED identity=%s cons=%s delivery=%s",
			os.Getenv("SVC_IDENTITY_URL"), os.Getenv("SVC_CONSUMPTION_URL"),
			os.Getenv("SVC_DELIVERY_URL"))
	} else {
		log.Printf("medashboard-bridge: DISABLED (SVC_IDENTITY_URL + SVC_CONSUMPTION_URL + SVC_DELIVERY_URL all unset)")
	}
	innerMuxWithMeDashboard := httpadapter.WithMeDashboard(innerMuxWithRplusProxy, meDashboardAgg)
	// E2: H+ instance readiness. Its own aggregator, deliberately not folded
	// into the learner dashboard above: different audience, different
	// lifecycle. New() returns nil when no downstream is configured, and the
	// bridge then leaves the route unmounted rather than serving a report that
	// could only say unknown.
	readinessAgg := readiness.New(readiness.LoadConfigFromEnv())
	if readinessAgg == nil {
		log.Printf("gateway: readiness aggregator NOT wired (no SVC_TENANCY_URL / SVC_IDENTITY_URL); /api/v1/admin/readiness unmounted")
	}
	innerMuxWithMeDashboard = httpadapter.WithReadiness(innerMuxWithMeDashboard, readinessAggOrNil(readinessAgg))

	// Stripe finalization (Iter G.5) — GET /api/v1/stripe-config exposes the
	// publishable key + checkout URLs to chora-web. Registered INSIDE the
	// chora-session JWT gate (handler shadows the route on innerMuxWithBridge
	// before WithChoraSessionOnPrefixes wraps the composite).
	stripeConfigHandler, stripeConfigCleanup, err := NewStripeConfigHandlerFromEnv(ctx)
	if err != nil {
		log.Fatalf("stripe-config handler init failed: %v", err)
	}
	defer func() {
		if stripeConfigCleanup != nil {
			if err := stripeConfigCleanup(); err != nil {
				log.Printf("stripe-config cleanup error: %v", err)
			}
		}
	}()
	if stripeConfigHandler == nil {
		log.Printf("stripe-config handler DISABLED (STRIPE_CONFIG_DISABLED=true) — GET /api/v1/stripe-config will 404")
	} else {
		log.Printf("stripe-config handler wired — GET /api/v1/stripe-config live (authed)")
	}
	innerMuxWithStripeConfig := httpadapter.WithStripeConfigRoute(innerMuxWithMeDashboard, stripeConfigHandler)

	// ADR-164 Stage B (2026-05-24) — chora-payments REST proxy.
	// /api/v1/checkout/{course|application|companion-egg|mana-topup|subscription}
	// mints Stripe Checkout Sessions via the chora-payments gRPC
	// PaymentService. Auth: behind RequireChoraSessionJWT
	// (DefaultJWTGatedPrefixes includes /api/v1/checkout) so the
	// payments_handler reads tenant_id + learner_gcid from the stamped
	// MeshClaims, never the request body. Loader fails-loud when
	// CHORA_PAYMENTS_GRPC_ADDR is unset (set CHORA_PAYMENTS_DISABLED=true
	// to opt out in local dev).
	paymentsCheckoutHandler, paymentsCleanup, err := NewPaymentsCheckoutHandlerFromEnv(ctx)
	if err != nil {
		log.Fatalf("payments handler init failed: %v", err)
	}
	defer func() {
		if paymentsCleanup != nil {
			if err := paymentsCleanup(); err != nil {
				log.Printf("payments cleanup error: %v", err)
			}
		}
	}()
	if paymentsCheckoutHandler == nil {
		log.Printf("payments handler DISABLED (CHORA_PAYMENTS_DISABLED=true) — /api/v1/checkout/* will 404")
	} else {
		log.Printf("payments handler wired — POST /api/v1/checkout/{course|application|companion-egg|mana-topup|subscription} live (authed)")
	}
	innerMuxWithPayments := httpadapter.WithPaymentsCheckout(innerMuxWithStripeConfig, paymentsCheckoutHandler)

	// ADR-205 / CHO-1940 (Wave B4) — contextual Transaction History BFF.
	// Proxies /api/v1/{me,admin}/transactions* to chora-tenancy's
	// TransactionHistoryService gRPC (scope-aware; the client stamps the mesh
	// identity on the outbound call). Wired BEFORE WithChoraSessionOnPrefixes
	// (below) so the JWT validator stamps MeshClaims on these prefixes first.
	// Additive read surface (supersedes the payments-admin purchases path in
	// Wave D): an unset CHORA_TENANCY_GRPC_ADDR degrades (routes 404) rather
	// than bricking the gateway — see transaction_history_loader.go.
	txHistoryHandler, txHistoryCleanup, err := NewTransactionHistoryHandlerFromEnv(ctx)
	if err != nil {
		log.Fatalf("transaction-history handler init failed: %v", err)
	}
	defer func() {
		if txHistoryCleanup != nil {
			if err := txHistoryCleanup(); err != nil {
				log.Printf("transaction-history cleanup error: %v", err)
			}
		}
	}()
	if txHistoryHandler == nil {
		log.Printf("transaction-history handler DISABLED (CHORA_TENANCY_GRPC_ADDR unset or CHORA_TRANSACTION_HISTORY_DISABLED=true) — /api/v1/{me,admin}/transactions* will 404")
	} else {
		log.Printf("transaction-history handler wired — GET /api/v1/{me,admin}/transactions[/summary|/{ledger_id}] live (authed)")
	}
	innerMuxWithTxHistory := httpadapter.WithTransactionHistory(innerMuxWithPayments, txHistoryHandler)

	// ADR-217 Phase 2.2 (CHO-2008) — operator franchise sub-tenant create BFF.
	// POST /api/v1/tenancy/sub-tenants proxies to chora-tenancy's
	// Tenancy/CreateSubTenant gRPC (same addr as tx-history; own conn to keep the
	// blast-radius isolated). Wired BEFORE WithChoraSessionOnPrefixes so the JWT
	// validator stamps MeshClaims (incl. Roles) on the prefix — the handler's
	// platform_operator gate is the SOLE app-layer authz (chora-tenancy does no
	// role check; it trusts the Istio allow-list + this gate). Additive surface:
	// an unset CHORA_TENANCY_GRPC_ADDR degrades (route 404), never bricks the gateway.
	tenancyAdminHandler, tenancyAdminCleanup, err := NewTenancyAdminHandlerFromEnv(ctx)
	if err != nil {
		log.Fatalf("tenancy-admin handler init failed: %v", err)
	}
	defer func() {
		if tenancyAdminCleanup != nil {
			if err := tenancyAdminCleanup(); err != nil {
				log.Printf("tenancy-admin cleanup error: %v", err)
			}
		}
	}()
	if tenancyAdminHandler == nil {
		log.Printf("tenancy-admin handler DISABLED (CHORA_TENANCY_GRPC_ADDR unset or CHORA_TENANCY_ADMIN_DISABLED=true) — POST /api/v1/tenancy/sub-tenants will 404")
	} else {
		log.Printf("tenancy-admin handler wired — POST /api/v1/tenancy/sub-tenants live (operator-gated)")
	}
	innerMuxWithTenancyAdmin := httpadapter.WithTenancyAdmin(innerMuxWithTxHistory, tenancyAdminHandler)

	// Phase C (2026-05-26, O+ hydration) — wires the 5 /bff/oplus/* routes
	// onto the inner mux BEFORE WithChoraSessionOnPrefixes so the JWT
	// validator authenticates the OPlus paths in its prefix sweep, and the
	// outer AuditorGate (chained below) sees the validated chora-session
	// claims on the request context.
	//
	// Backend endpoints sourced from env per feedback_no_inline_config:
	//   CHORA_GOVERNANCE_GRPC_ADDR, CHORA_GOVERNANCE_HTTP_ADDR,
	//   CHORA_OBSERVABILITY_HTTP_ADDR, CHORA_A2A_HTTP_ADDR (optional —
	//   empty = pending mode), CHORA_GATEWAY_UPSTREAM_FAKE (smoke fallback).
	// Loader fails loud when any required addr is empty AND fake-mode is
	// off (per feedback_no_stubs_real_wiring).
	oplusHandler, oplusCleanup, oplusRoleResolver, err := NewOPlusHandlerFromEnv(ctx)
	if err != nil {
		log.Fatalf("oplus loader failed: %v", err)
	}
	defer func() {
		if oplusCleanup != nil {
			if err := oplusCleanup(); err != nil {
				log.Printf("oplus cleanup error: %v", err)
			}
		}
	}()
	if upstream.IsFakeUpstreamAllowed() {
		log.Printf("oplus handler WIRED (fake-mode) — CHORA_GATEWAY_UPSTREAM_FAKE=true, /bff/oplus/* will return error envelopes from nil clients")
	} else {
		log.Printf("oplus handler WIRED — gov_grpc=%s gov_http=%s obs_http=%s a2a_http=%s",
			os.Getenv(EnvGovernanceGRPCAddr), os.Getenv(EnvGovernanceHTTPAddr),
			os.Getenv(EnvObservabilityHTTPAddr), os.Getenv(EnvA2AHTTPAddr))
	}
	innerMuxWithOPlus := httpadapter.WithOPlusRoutes(innerMuxWithTenancyAdmin, oplusHandler)

	// CHO-2148 — the O+ platform egress kill-switch (ADR-231 D6). Mounted at
	// /api/v1/admin/egress/kill-switch rather than /bff/oplus/* for two reasons,
	// both load-bearing: Cloud Armor's methodenforcement WAF only carves out
	// PATCH for /api/v1/... paths (there is NO /bff/... carve-out and the policy
	// is at its rule cap), and middleware.OPlusAuthorizedRoles omits
	// platform_operator — so a pure operator would be 403'd by AuditorGate before
	// reaching any /bff/oplus/ handler. JWT-gated by the /api/v1/admin/ blanket
	// prefix in DefaultJWTGatedPrefixes.
	//
	// The upstream URL is env-sourced (no inline config). Empty => the route 503s
	// rather than silently reporting "not engaged", which would be
	// indistinguishable from a real answer.
	egressKillSwitch := httpadapter.NewEgressKillSwitchHandler(
		os.Getenv(EnvObservabilityHTTPAddr), nil,
	)
	innerMuxWithEgressKillSwitch := httpadapter.WithEgressKillSwitch(innerMuxWithOPlus, egressKillSwitch)
	log.Printf("gateway: O+ egress kill-switch route WIRED (upstream=%s)", os.Getenv(EnvObservabilityHTTPAddr))

	// ADR-252 via ADR-254 D7: the Learning Companion containment control,
	// sibling of the egress kill-switch (same path family, same Cloud Armor 994
	// carve-out, same upstream). Operator / auditor / admin / owner reach it;
	// chora-observability enforces the scope matrix. Empty upstream => 503.
	companionSuspension := httpadapter.NewCompanionSuspensionHandler(
		os.Getenv(EnvObservabilityHTTPAddr), nil,
	)
	innerMuxWithCompanionSuspension := httpadapter.WithCompanionSuspension(innerMuxWithEgressKillSwitch, companionSuspension)
	log.Printf("gateway: O+ companion containment route WIRED (upstream=%s)", os.Getenv(EnvObservabilityHTTPAddr))

	innerMuxWithMint := httpadapter.WithMintRoute(innerMuxWithCompanionSuspension, mintHandler)

	// Phase A4 (ADR-181 D2, CHO-1718) — WebAuthn passkey BFF routes:
	// /api/v1/auth/webauthn/{register,login}/{begin,finish} proxy the
	// ceremonies to chora-identity's /v1/auth/passkey/* routes; login/finish
	// composes the SAME mint pipeline as the username/password path (shared
	// MintHandler internals — allowlist/REGISTRATION_MODE + ADR-165 operator
	// stamping included). register/* are JWT-gated via
	// DefaultJWTGatedPrefixes (passkeys attach to an EXISTING account);
	// login/* are anonymous like the mint (they ESTABLISH the session).
	// Reuses the CHORA_IDENTITY_URL resolution — no new deployment config
	// (feedback_no_inline_config). nil handler (unset URL in dev) →
	// clean 404 passthrough.
	var webauthnHandler *httpadapter.WebAuthnHandler
	if identityURL := resolveIdentityURL(); identityURL != "" {
		passkeyClient, err := clients.NewIdentityPasskeyClient(clients.IdentityPasskeyClientConfig{
			BaseURL: identityURL,
		})
		if err != nil {
			log.Fatalf("webauthn passkey client init failed: %v", err)
		}
		webauthnHandler, err = httpadapter.NewWebAuthnHandler(httpadapter.WebAuthnHandlerConfig{
			Passkey: passkeyClient,
			Mint:    mintHandler, // nil (MINT_DISABLED) → login/finish 503
		})
		if err != nil {
			log.Fatalf("webauthn handler init failed: %v", err)
		}
		log.Printf("webauthn handler wired — /api/v1/auth/webauthn/{register,login}/{begin,finish} live (identity=%s)", identityURL)
	} else {
		log.Printf("webauthn handler DISABLED (SVC_IDENTITY_URL unset) — /api/v1/auth/webauthn/* will 404")
	}
	innerMuxWithWebAuthn := httpadapter.WithWebAuthnRoutes(innerMuxWithMint, webauthnHandler)

	// CHO-1719 (ADR-181 D5) — account-closure saga trigger routes:
	// /api/v1/me/account/{close,close/cancel,closure} (self-service, identity
	// from session claims only) + /api/v1/admin/accounts/{gcid}/close
	// (PLATFORM_OPERATOR, fast_close, X-Chora-Role forwarded). Proxies to
	// chora-closure-orchestrator (Cloud Run, internal ingress) behind
	// CLOSURE_ORCHESTRATOR_URL; optional Cloud Run IAM ID-token auth via
	// CLOSURE_ORCHESTRATOR_ID_TOKEN_AUDIENCE. Absent URL → routes answer 503
	// CLOSURE_UNAVAILABLE (degraded, never a silent 404). JWT-gated via the
	// /api/v1/me/account + /api/v1/admin/ prefixes in DefaultJWTGatedPrefixes.
	closureHandler, closureCleanup, err := NewClosureHandlerFromEnv(ctx)
	if err != nil {
		log.Fatalf("closure handler init failed: %v", err)
	}
	defer func() {
		if closureCleanup != nil {
			if err := closureCleanup(); err != nil {
				log.Printf("closure cleanup error: %v", err)
			}
		}
	}()
	if closureHandler == nil {
		log.Printf("closure routes DEGRADED (%s unset) — closure trigger routes will 503 CLOSURE_UNAVAILABLE", EnvClosureOrchestratorURL)
	} else {
		log.Printf("closure routes wired — me/account close+cancel+status & admin {gcid}/close → %s", os.Getenv(EnvClosureOrchestratorURL))
	}
	innerMuxWithClosure := httpadapter.WithClosureRoutes(innerMuxWithWebAuthn, closureHandler)

	// CHO-1966 (ADR-205 WS-2/WS-8) — Growth-Edge HITL resume proxy:
	// POST /api/v1/me/growth-edges/uploads/{upload_id}/resume → chora-ai-kernel-
	// orchestrator POST /v1/orchestrator/weakness/{upload_id}/resume. Identity
	// (tenant + gcid) from the session JWT only; the bounded review payload
	// rides the body (ADR-205 D4). Behind AI_KERNEL_ORCHESTRATOR_URL; absent →
	// 503 WEAKNESS_RESUME_UNAVAILABLE (degraded, never a silent 404). JWT-gated
	// via the /api/v1/me/growth-edges prefix in DefaultJWTGatedPrefixes.
	weaknessResumeHandler, err := NewWeaknessResumeHandlerFromEnv(ctx)
	if err != nil {
		log.Fatalf("weakness-resume handler init failed: %v", err)
	}
	if weaknessResumeHandler == nil {
		log.Printf("weakness-resume route DEGRADED (%s unset) — %s will 503 WEAKNESS_RESUME_UNAVAILABLE",
			EnvAIKernelOrchestratorURL, httpadapter.PatternWeaknessResume)
	} else {
		log.Printf("weakness-resume route wired — %s → %s", httpadapter.PatternWeaknessResume,
			os.Getenv(EnvAIKernelOrchestratorURL))
	}
	innerMuxWithWeaknessResume := httpadapter.WithWeaknessResumeRoute(innerMuxWithClosure, weaknessResumeHandler)

	// W0 (rt) realtime SSE channel (ADR-183, CHO-1661) — GET
	// /api/v1/realtime/ticket mints the ~60s single-use HMAC stream ticket
	// chora-realtime validates (EventSource cannot send an Authorization
	// header). Path rides DefaultJWTGatedPrefixes so RequireChoraSessionJWT
	// stamps MeshClaims first; the signer secret is shared with
	// chora-realtime (SM chora-realtime-ticket-signer). nil minter (unset
	// env in dev) → clean 404 passthrough per the nil-bridge convention.
	// The /stream sibling NEVER touches this pod — it has its own 3600s
	// HTTPRoute (this pod's route caps requests at 60s).
	realtimeTicketMinter, realtimeTicketCleanup, err := NewRealtimeTicketMinterFromEnvSM(ctx)
	if err != nil {
		log.Fatalf("realtime-ticket minter init failed: %v", err)
	}
	defer func() { _ = realtimeTicketCleanup() }()
	if realtimeTicketMinter != nil {
		log.Printf("realtime-ticket minter: WIRED — GET %s live (ADR-183)", httpadapter.RealtimeTicketPath)
	} else {
		log.Printf("realtime-ticket minter: DISABLED (%s unset) — %s will 404",
			httpadapter.EnvRealtimeTicketSigner, httpadapter.RealtimeTicketPath)
	}
	innerMuxWithRealtimeTicket := httpadapter.WithRealtimeTicket(innerMuxWithWeaknessResume, realtimeTicketMinter)
	// /api/* trust-boundary gate — chora-session HS256 (NOT identity JWKS
	// RS256). The mint endpoint sits OUTSIDE this gate (registered above via
	// WithMintRoute) because it IS the auth-establishing surface: clients
	// don't have a Chora session JWT until /api/v1/auth/session/mint hands
	// them one.
	// Auditor role gate (Phase C) — inspects /bff/oplus/* paths only; every
	// other route passes through untouched. The role resolver reads the
	// validated chora-session Claims off the request context.
	//
	// ORDER IS LOAD-BEARING: AuditorGate MUST run AFTER WithChoraSessionOnPrefixes
	// stamps the Claims, so it wraps the inner mux and is itself wrapped BY the
	// JWT validator (request → JWT validate+stamp Claims → AuditorGate reads
	// Claims → handler). The previous wiring chained AuditorGate OUTSIDE the
	// validator, so it read Claims that had not been stamped yet → empty roles
	// → 403 `auditor_role_required` for EVERY O+ caller (admin AND auditor).
	// That made O+ unreachable for all roles. Per imda-governance-4-dimensions:
	// O+ audit-grade artefacts are admin/auditor-only (see middleware.HasOPlusAccess).
	auditorGated := middleware.AuditorGate(oplusRoleResolver, innerMuxWithRealtimeTicket)
	gatedHandler := httpadapter.WithChoraSessionOnPrefixes(auditorGated, choraSessionValidator)

	// ADR-164 Stage B — /v1/webhooks/stripe passthrough to chora-payments.
	// Stripe POSTs webhooks at https://api.chora.site/v1/webhooks/stripe;
	// chora-gateway forwards verbatim (body byte-identical, all headers
	// including Stripe-Signature) to the chora-payments HTTP listener at
	// CHORA_PAYMENTS_HTTP_ADDR. chora-payments does its own HMAC
	// verification + dedup. Mounted OUTSIDE the JWT gate (Stripe has no
	// JWT) and BEFORE CORS (Stripe doesn't issue preflights). Empty addr
	// disables the route per WithStripeWebhookPassthrough's nil-check.
	stripeWebhookHandler := httpadapter.WithStripeWebhookPassthrough(gatedHandler, httpadapter.StripeWebhookPassthroughConfig{
		PaymentsHTTPAddr: os.Getenv("CHORA_PAYMENTS_HTTP_ADDR"),
	})
	if os.Getenv("CHORA_PAYMENTS_HTTP_ADDR") == "" {
		log.Printf("stripe webhook passthrough DISABLED (CHORA_PAYMENTS_HTTP_ADDR unset) — POST /v1/webhooks/stripe will 404")
	} else {
		log.Printf("stripe webhook passthrough wired — POST /v1/webhooks/stripe → %s", os.Getenv("CHORA_PAYMENTS_HTTP_ADDR"))
	}

	// Pub/Sub push ingress was removed with the cloud decoupling: the platform
	// now uses chora-common/eventbus (NATS JetStream) for asynchronous
	// delivery, and the gateway holds no Pub/Sub push routing table.
	//
	// A5 — gateway-wide CORS. Applied OUTSIDE the JWT gate so every route
	// (/api/*, /bff/*, /graphql, health) is CORS-correct and OPTIONS
	// preflights are answered with 204 BEFORE the JWT gate or any downstream
	// handler runs. Previously CORS was scoped to /api/v1/auth/* only (the
	// mint handler's inline block) — every other /api/* route returned no
	// Access-Control-Allow-Origin header and the browser blocked it as
	// net::ERR_FAILED. See cors.go + docs handoff §A5.
	// CHO-1634 / ADR-171 — /webhooks/sendgrid/events passthrough to
	// chora-notifications. SendGrid's Signed Event Webhook POSTs
	// delivered/bounced events at https://api.chora.site/webhooks/sendgrid/events;
	// chora-gateway forwards verbatim (body byte-identical, the two
	// X-Twilio-Email-Event-Webhook-* signature/timestamp headers preserved) to
	// the chora-notifications HTTP listener. chora-notifications does its OWN
	// ECDSA P-256 signature verification + dedup. Mounted OUTSIDE the JWT gate
	// (SendGrid carries no Chora JWT) and BEFORE CORS (no preflights). Downstream
	// addr from CHORA_NOTIFICATIONS_HTTP_ADDR, falling back to the already-wired
	// SVC_NOTIFICATIONS_URL (both point at chora-notifications) per
	// feedback_no_inline_config. Empty → route disabled (404).
	notificationsHTTPAddr := os.Getenv("CHORA_NOTIFICATIONS_HTTP_ADDR")
	if notificationsHTTPAddr == "" {
		notificationsHTTPAddr = os.Getenv("SVC_NOTIFICATIONS_URL")
	}
	sendgridWebhookHandler := httpadapter.WithSendGridWebhookPassthrough(stripeWebhookHandler, httpadapter.SendGridWebhookPassthroughConfig{
		NotificationsHTTPAddr: notificationsHTTPAddr,
	})
	if notificationsHTTPAddr == "" {
		log.Printf("sendgrid webhook passthrough DISABLED (CHORA_NOTIFICATIONS_HTTP_ADDR + SVC_NOTIFICATIONS_URL unset) — POST /webhooks/sendgrid/events will 404")
	} else {
		log.Printf("sendgrid webhook passthrough wired — POST /webhooks/sendgrid/events → %s", notificationsHTTPAddr)
	}

	corsHandler := httpadapter.CORSMiddleware(sendgridWebhookHandler)

	// Gateway-wide security response headers (2026-08-07). Three independent
	// probes found the same gap on the same day: an authenticated OWASP ZAP
	// scan of api.chora.site, a manual curl of /healthz and a 401 path, and
	// tests/security/headers/headers_test.go, which had asserted seven headers
	// the gateway never sent. chora-contracts/openapi/gateway.yaml documented a
	// middleware chain that set them; the contract and the code disagreed and
	// the code was wrong.
	//
	// The five SPA backend buckets already carry X-Frame-Options,
	// X-Content-Type-Options and HSTS as customResponseHeaders at the CDN edge,
	// so the browser surfaces were covered. The api backend service carried
	// none, which is why the gateway was the gap. The guarantee lives in
	// middleware so it travels with the service rather than depending on load
	// balancer config.
	//
	// ORDER IS LOAD-BEARING, mirroring the chain gateway.yaml documents
	// (CorrelationID first, then the rest, then CORS):
	//
	//	request -> OTel span -> CorrelationID -> SecurityHeaders -> CORS -> ...
	//
	// SecurityHeaders sits OUTSIDE CORSMiddleware so the OPTIONS preflight
	// short-circuit (204, inner handler never runs) is covered too, and outside
	// the JWT gate so a 401 carries the headers as surely as a 200.
	// CorrelationID sits outside SecurityHeaders so the id exists before
	// anything downstream can fail.
	securedHandler := middleware.SecurityHeaders(corsHandler)
	correlatedHandler := middleware.CorrelationID(securedHandler)
	log.Printf("security headers WIRED: CSP + HSTS + X-Frame-Options + X-Content-Type-Options + Referrer-Policy + X-XSS-Protection; X-Correlation-ID generated when absent, propagated when present")

	// OTel server-side request span — OUTERMOST so CORS + auth + routes
	// all sit inside the trace. Closes FE-coord
	// E2E-BE-AI-ASSIST-TRACE-EXPORT-PERM (2026-05-17) where gateway emitted
	// zero Cloud Trace spans despite being the entry point of every
	// browser-originated chain (libs/chora-go-common/tracing.Middleware
	// was upgraded the same day to actually start spans; prior shim only
	// managed W3C headers).
	handler := observability.HTTPMiddleware()(correlatedHandler)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// kg/explore proxies a Vertex AI reasoning-engine cold call (~10-14s
		// on first-request latency); per-route PerCallTimeout was bumped to 30s
		// to match — the server WriteTimeout must exceed that.
		WriteTimeout: 45 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("service=%s version=%s listening on %s", serviceName, version, srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down...")
	drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(drainCtx); err != nil {
		log.Printf("server shutdown error: %v", err)
	}
}

// firstNonEmpty returns the first non-empty value. The Pub/Sub passthrough
// accepts either the CHORA_<SVC>_HTTP_ADDR or the SVC_<SVC>_URL spelling: the
// live Deployment sets SVC_* for consumption/sharing/identity and
// CHORA_DELIVERY_HTTP_ADDR for delivery, and a previous investigation lost half
// a day concluding the allowlists were empty because it grepped for env vars
// this binary never read. Accept both spellings so the name is not the trap.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// readinessAggOrNil converts a typed-nil *readiness.Aggregator into an
// untyped nil interface. Without this a nil pointer stored in the interface
// is NOT nil, the bridge mounts the route, and the handler's own nil check
// never fires: the classic typed-nil-in-an-interface trap, which would turn
// "unconfigured" into a panic on the first request.
func readinessAggOrNil(a *readiness.Aggregator) httpadapter.ReadinessAggregator {
	if a == nil {
		return nil
	}
	return a
}
