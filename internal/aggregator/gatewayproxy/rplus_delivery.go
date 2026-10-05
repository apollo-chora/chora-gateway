// rplus_delivery.go — R+ (Rhythm+) surface BFF proxy methods for the
// chora-delivery resource groups the existing FE doesn't yet have BFF
// coverage for. Closes the gap surfaced by the R+ wave-1/2/3 services
// that have been holding hardcoded in-memory fixtures (MTM / DSA-101 /
// CSPO / Mr. Chen) instead of real BFF wiring per feedback_no_stubs_real_wiring.
//
// Each method is a verbatim passthrough — FE path == chora-delivery path,
// method + body + Content-Type forwarded byte-for-byte. The proxy matches
// the existing ProxyTestSets / ProxyAssessments / ProxyMeAssessments /
// ProxyCourses pattern (Lane D, 2026-05-16). One generic method covers
// all four resource groups because the proxy logic is identical — the
// HTTP layer registers separate prefixes for clarity + JWT-gate scoping.
//
// Resource groups owned (each has a real chora-delivery HTTP handler):
//
//	/api/bookings[/{...}]            → chora-delivery (legacy /api/bookings)
//	/api/certifications[/{...}]      → chora-delivery (legacy /api/certifications)
//	/v1/campus[/{...}]               → chora-delivery (legacy /v1/campus — campusops)
//	/v1/me/applications[/{...}]      → chora-delivery (ADR-164 Stage C — chora-payments
//	                                   owns the accept-offer Stripe Checkout mint
//	                                   internally via gRPC, but the REST surface
//	                                   stays in chora-delivery and the gateway proxies
//	                                   this path verbatim)
//
// Resource groups deliberately NOT registered here (chora-delivery has no
// HTTP handler — they will land in Stage C of the R+ build-out plan):
//
//	rosters / exams / skillsfutures-claims / project-groups /
//	wbl-placements / surveys / live-quiz / live-poll / classroom
//
// Authoring those backends + wiring the FE separately keeps M1 (this work)
// add-only + parallel-session-safe per the locked R+ plan.
//
// Auth: every prefix is JWT-gated upstream by RequireChoraSessionJWT
// (DefaultJWTGatedPrefixes carries them). The downstream chora-delivery
// handlers all use tenantRequired middleware, which reads X-Tenant-Id from
// the validated mesh claims the gateway stamps via the canonical call()
// helper. No new env vars — reuses the existing SVC_DELIVERY_URL.

package gatewayproxy

import (
	"context"
	"net/http"
)

// ProxyDeliveryVerbatim is a generic verbatim passthrough to chora-delivery
// at the SAME path the FE sent. The R+ HTTP handler layer registers
// per-resource-group prefixes so this single method serves /api/bookings,
// /api/certifications, /v1/campus, /v1/me/applications, and any future
// R+ delivery resource group that needs a pure passthrough.
//
// path MUST be the full FE-incoming path (e.g. "/api/bookings/" or
// "/v1/me/applications/{id}/accept-offer"). chora-delivery serves these
// paths at the same path the FE hits — pure verbatim proxy, no rewrite.
//
// rawQuery is forwarded verbatim so pagination + filter params flow to
// the downstream handler's parser. body is forwarded byte-for-byte;
// contentType preserves the inbound Content-Type so the downstream's
// body parser sees the original payload shape (defaults to application/json
// inside callWithContentType when empty).
//
// Status passthrough is the canonical envelope from classify():
//   - 2xx + 4xx flow through verbatim (a downstream 4xx proves the request
//     reached the handler and is a real semantic signal the FE must render).
//   - 5xx → 502 GATEWAY_UPSTREAM_5XX (the only normalisation).
//   - Timeout → 504 GATEWAY_UPSTREAM_TIMEOUT.
func (a *Aggregator) ProxyDeliveryVerbatim(ctx context.Context, auth AuthCtx, method, path, rawQuery string, body []byte, contentType string) (Response, error) {
	u := a.cfg.DeliveryURL + path
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return classify(a.callWithContentType(ctx, method, u, contentType, body, auth)), nil
}

// -----------------------------------------------------------------------------
// WebSocket upgrade passthrough support (ADR-168 classroom realtime / ADR-166
// §D5). The R+ live-quiz / live-poll FE opens WebSockets at
// /api/v1/live-quizzes/{sessionId}/ws and /api/v1/live-polls/{pollId}/ws. The
// REST verbatim path (ProxyDeliveryVerbatim) cannot carry a 101 Switching
// Protocols handshake, so the HTTP-adapter layer hijacks the client conn and
// dials the chora-delivery downstream directly. These two methods expose the
// SAME env-sourced config + mesh-trust header stamping the REST path uses so
// the adapter never hard-codes a downstream addr (feedback_no_inline_config)
// and never diverges from the canonical mesh header set.
// -----------------------------------------------------------------------------

// DeliveryBaseURL returns the chora-delivery downstream base URL the verbatim
// REST proxy dials (sourced from SVC_DELIVERY_URL at boot per
// feedback_no_inline_config). The adapter-layer WS proxy parses host:port from
// this base to dial the raw TCP conn for the upgrade. Empty when chora-delivery
// is unconfigured — the WS proxy must treat that as "not routable".
func (a *Aggregator) DeliveryBaseURL() string {
	return a.cfg.DeliveryURL
}

// StampDownstreamHeaders stamps the canonical mesh-trust + tracing headers
// (Authorization / traceparent / X-Tenant-Id / X-Chora-GCID / lowercase gcid /
// chora-* mesh claims / x-mesh-user-roles) onto an outbound request. The WS
// upgrade proxy reuses this so the upgrade request the gateway replays to
// chora-delivery carries the SAME trust headers a verbatim REST call would —
// chora-delivery's tenantRequired + gcid context middleware read them
// identically on the WS handshake.
func (a *Aggregator) StampDownstreamHeaders(req *http.Request, auth AuthCtx) {
	stampAuthHeaders(req, auth)
}
