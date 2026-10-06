// PE-14 — GDPR consent center BFF aggregator wrappers.
//
// Surfaces three routes for the A+ learner-facing consent center, wrapping
// existing chora-identity (consent, portability) endpoints. All URLs come from
// SVC_*_URL env vars per the no-inline-config rule. Authorization +
// traceparent propagate via AuthCtx as on every Phyllis route.
//
// Account closure (GDPR Art. 17) is NOT here — it is served by the canonical
// closure saga me-route POST /api/v1/me/account/close (CHO-1719); the legacy
// TriggerAccountClosure → chora-closure-orchestrator:/sagas wrapper was dead
// (no live caller; /sagas never implemented) and is removed (CHO-1790 D12).
package phyllis

import (
	"context"
	"net/http"
	"net/url"
)

// GetConsents — GET /api/me/consents — chora-identity:/me/consents.
// Returns the GCID's current consent posture across all consent-toggle types
// (companion_memory_consent, marketing_consent, social_visibility_consent,
// analytics_consent, behavioural_personalisation_consent — see
// tools/compliance/gdpr/gdpr-flow.md §2).
func (a *Aggregator) GetConsents(ctx context.Context, auth AuthCtx) (Response, error) {
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodGet, a.cfg.IdentityURL+"/me/consents", nil, auth))
	}), nil
}

// GrantConsent — POST /api/me/consents — chora-identity:/me/consents.
// Records (or withdraws) a single per-purpose consent toggle. Body shape:
//
//	{ "consent_type": "marketing_consent", "granted": true, "version": "2026-05-07" }
//
// Withdrawal sends `granted: false`. Identity emits
// chora.identity.consent.recorded.v1 / consent.withdrawn.v1 events.
func (a *Aggregator) GrantConsent(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	return a.withBudget(ctx, func(c context.Context) Response {
		return classify(a.call(c, http.MethodPost, a.cfg.IdentityURL+"/me/consents", body, auth))
	}), nil
}

// RequestDataExport — POST /api/me/data-export — chora-identity
// POST /api/users/{gcid}/portability/export.
// GDPR Art. 15 (Right of Access) + Art. 20 (Portability).
//
// chora-identity serves the portability export at
// POST /api/users/{gcid}/portability/export (internal/adapter/http/handler.go:201),
// NOT /me/portability/export — the latter is a domain-package doc reference
// (internal/domain/portability/export.go:6) with no HTTP route, so the old
// path 404'd. Identity resolves the GCID from the path, so the caller's GCID
// is embedded here. Identity appends a PortableSnapshot row (skeleton mode).
func (a *Aggregator) RequestDataExport(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	if auth.GCID == "" {
		return errResp(http.StatusBadRequest, "GATEWAY_GCID_NOT_RESOLVED",
			"gcid missing from auth context"), nil
	}
	return a.withBudget(ctx, func(c context.Context) Response {
		u := a.cfg.IdentityURL + "/api/users/" + url.PathEscape(auth.GCID) + "/portability/export"
		return classify(a.call(c, http.MethodPost, u, body, auth))
	}), nil
}
