// PE-14 — A+ GDPR consent center HTTP handlers.
//
// Wraps three BFF routes onto the Phyllis aggregator's GDPR fan-out methods:
//
//	GET    /api/me/consents           — chora-identity:/me/consents (list)
//	POST   /api/me/consents/grant     — chora-identity:/me/consents (toggle)
//	POST   /api/me/data-export        — chora-identity:/me/portability/export
//
// Account closure (GDPR Art. 17) is served by the canonical closure saga
// me-route POST /api/v1/me/account/close (CHO-1719, closure_handler.go); the
// legacy POST /api/me/account-closure → orchestrator:/sagas route was dead
// and is removed (CHO-1790 D12).
//
// Bearer + traceparent + tenant headers propagate via authCtxFromRequest as
// every other Phyllis route.
package httpadapter

import (
	"net/http"
)

// handleGdprConsents — GET /api/me/consents (list active consent posture).
func (p *PhyllisHandler) handleGdprConsents(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	resp, _ := p.agg.GetConsents(r.Context(), authCtxFromRequest(r))
	writePhyllisResp(w, resp)
}

// handleGdprConsentGrant — POST /api/me/consents/grant (per-purpose toggle).
func (p *PhyllisHandler) handleGdprConsentGrant(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	body := readBody(r)
	resp, _ := p.agg.GrantConsent(r.Context(), authCtxFromRequest(r), body)
	writePhyllisResp(w, resp)
}

// handleGdprDataExport — POST /api/me/data-export (GDPR Art. 15 / 20).
func (p *PhyllisHandler) handleGdprDataExport(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	body := readBody(r)
	resp, _ := p.agg.RequestDataExport(r.Context(), authCtxFromRequest(r), body)
	writePhyllisResp(w, resp)
}
