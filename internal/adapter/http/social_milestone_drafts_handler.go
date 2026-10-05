// social_milestone_drafts_handler.go — gateway routing for the owner-facing C+
// Companion-milestone drafts + share-preference routes (CHO-2258).
//
// Registered on the NewRouterWithSocial mux (gateway "list 1 of 3"). Lists 2 and
// 3 need no new entry: DefaultJWTGatedPrefixes already carries "/v1/me/", and
// the ns/sharing Istio policy already allowlists /v1/me/post-drafts,
// /v1/me/post-drafts/* and /v1/me/preferences/companion-milestone-share
// (live-verified 2026-07-17).
//
// Method allow-listing is explicit and closed. PUT/PATCH/DELETE are absent by
// design, not oversight: Cloud Armor rule 1005 (OWASP methodenforcement) denies
// them at the edge, so a route accepting them would work in tests and 403 in
// production. Publish and discard are POST action sub-resources — the two
// transitions of the draft state machine — mirroring POST /v1/duels/{id}/accept.
package httpadapter

import (
	"io"
	"net/http"
	"strings"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/social"
)

// handleMilestoneDrafts serves GET /v1/me/post-drafts (the owner's queue).
func (sh *SocialHandler) handleMilestoneDrafts(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	resp, _ := sh.agg.MilestoneDraftsOp(r.Context(), social.MilestoneDraftsOpParams{
		Auth:   authCtxFromRequest(r),
		Method: http.MethodGet,
	})
	writeSocialResp(w, resp)
}

// handleMilestoneDraftScoped serves the two draft transitions:
//
//	POST /v1/me/post-drafts/{draft_id}/publish
//	POST /v1/me/post-drafts/{draft_id}/discard
//
// Anything else under the subtree is refused here rather than forwarded — an
// unknown subpath is a client error, and forwarding it would hand chora-sharing
// a path its own mux would 404 anyway, one hop later and harder to read.
func (sh *SocialHandler) handleMilestoneDraftScoped(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/me/post-drafts/"), "/")
	parts := strings.Split(rest, "/")

	if len(parts) != 2 || parts[0] == "" || (parts[1] != "publish" && parts[1] != "discard") {
		http.NotFound(w, r)
		return
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
	}
	resp, _ := sh.agg.MilestoneDraftsOp(r.Context(), social.MilestoneDraftsOpParams{
		Auth:    authCtxFromRequest(r),
		Method:  http.MethodPost,
		Subpath: "/" + parts[0] + "/" + parts[1],
		Body:    body,
	})
	writeSocialResp(w, resp)
}

// handleMilestoneSharePref serves GET/POST
// /v1/me/preferences/companion-milestone-share — the learner's auto|draft|suppress
// choice. POST (not PUT) because the edge denies PUT; the downstream upserts on
// the natural key, so it is idempotent in effect regardless.
func (sh *SocialHandler) handleMilestoneSharePref(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var body []byte
	if r.Method == http.MethodPost && r.Body != nil {
		body, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
	}
	resp, _ := sh.agg.MilestoneSharePrefOp(r.Context(), social.MilestoneSharePrefOpParams{
		Auth:   authCtxFromRequest(r),
		Method: r.Method,
		Body:   body,
	})
	writeSocialResp(w, resp)
}
