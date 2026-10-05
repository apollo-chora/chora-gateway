// companion_bridge_handler.go — HTTP route bindings for the ADR-149
// Companion Growth + Egg purchase BFF routes per
// the PROD-D FE growth handoff (2026-05-13) §2.1 (PROD-D scope).
//
// Routes mounted (all require Bearer JWT via Phase 4 enforcement —
// `DefaultJWTGatedPrefixes` is updated to include /api/v1/me/companions
// + /api/v1/companion-eggs, plus the pre-rename /api/v1/me/familiars +
// /api/v1/familiar-eggs prefixes served as ADR-254 D9 aliases for one deploy
// window, drop after the SPA cut (WP-X go)):
//
//	GET    /api/v1/me/companions                            → chora-consumption
//	GET    /api/v1/me/companions/:id                        → chora-consumption (profile)
//	GET    /api/v1/me/companions/:id/skills                 → chora-consumption
//	POST   /api/v1/me/companions/:id/skills                 → chora-consumption
//	GET    /api/v1/me/companions/:id/growth                 → chora-consumption
//	POST   /api/v1/me/companions/:id/reveal                 → chora-consumption (CHO-2229)
//	POST   /api/v1/me/companions/:id/hatch                  → chora-consumption (30s)
//	POST   /api/v1/me/companions/:id/resonance              → chora-consumption
//	POST   /api/v1/me/companions/:id/source-revelation      → chora-consumption
//	GET    /api/v1/me/companions/:id/growth-events          → chora-consumption
//	POST   /api/v1/me/companions/:id/retire                 → chora-consumption (CHO-2033 SP3)
//	POST   /api/v1/me/companions/acquire                    → chora-consumption (Discovery Graph)
//	GET    /api/v1/me/companions/bindings                   → chora-consumption (Discovery Graph)
//	GET    /api/v1/me/companions/:id/memory                 → chora-consumption (Discovery Graph)
//	GET    /api/v1/companion-eggs/catalog                   → chora-tenancy
//	GET    /api/v1/companion-eggs/:sku/odds                 → chora-tenancy
//	POST   /api/v1/companion-eggs/checkout                  → chora-tenancy
//	GET    /api/v1/companion-eggs/admin/catalog/:sku        → chora-tenancy (H+ editor read)
//	POST   /api/v1/companion-eggs/admin/catalog             → chora-tenancy (H+ editor save)
//
// Stripe webhook (POST /webhooks/stripe on chora-tenancy) is intentionally
// NOT proxied through chora-gateway — Stripe signature verification must
// happen at the receiving end (chora-tenancy) so the BFF cannot replay or
// alter the webhook body. The webhook is exposed directly via GCLB at
// chora-tenancy's `/webhooks/stripe` ingress rule.
package httpadapter

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/apollo-chora/chora-common/auth/chorasession"
	"github.com/apollo-chora/chora-common/auth/servicemesh"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/companionbridge"
)

// CompanionBridgePathPrefixes lists the BFF path prefixes the CompanionBridge
// mux serves. Exposed for inclusion in DefaultJWTGatedPrefixes.
var CompanionBridgePathPrefixes = []string{
	"/api/v1/me/companions",
	"/api/v1/companion-eggs",
	// ADR-254 D9 alias, drop after the SPA cut (WP-X go)
	"/api/v1/me/familiars",
	"/api/v1/familiar-eggs",
}

// companionBridgeAliasPrefixes maps each pre-rename public prefix onto its
// companion-named prefix. A request on an alias prefix is re-pathed onto the
// canonical prefix BEFORE the mux dispatches (see adr254_alias.go), so the
// alias is by construction the same handler and the same upstream path.
// ADR-254 D9 alias, drop after the SPA cut (WP-X go)
var companionBridgeAliasPrefixes = map[string]string{
	"/api/v1/me/familiars":  "/api/v1/me/companions",
	"/api/v1/familiar-eggs": "/api/v1/companion-eggs",
}

// CompanionBridgeHandler binds the companionbridge aggregator to the BFF mux.
type CompanionBridgeHandler struct {
	bridge *companionbridge.Bridge
}

// NewCompanionBridgeHandler constructs the handler.
func NewCompanionBridgeHandler(b *companionbridge.Bridge) *CompanionBridgeHandler {
	return &CompanionBridgeHandler{bridge: b}
}

// NewCompanionBridgeMux returns a mux that serves the PROD-D routes plus the
// bare instance profile GET + the /skills grant routes. Anything off the
// bridge prefixes 404s — combine with WithCompanionBridge to compose with a
// base handler that owns non-bridge paths. The catch-all subtree handler
// (handleCompanionSubpath) dispatches the bare /{id} profile GET and the
// suffixed subpaths; the longer suffixed paths are matched first so the bare
// GET never shadows them.
func NewCompanionBridgeMux(b *companionbridge.Bridge) http.Handler {
	h := NewCompanionBridgeHandler(b)
	mux := http.NewServeMux()
	// Exact-path routes register BEFORE the catch-all subtree paths so they win.
	mux.HandleFunc("/api/v1/me/companions", h.handleListCompanions)
	mux.HandleFunc("/api/v1/me/companions/", h.handleCompanionSubpath)
	mux.HandleFunc("/api/v1/companion-eggs/catalog", h.handleEggCatalog)
	// D6 admin round-trip. Registered as their own patterns so the
	// /api/v1/companion-eggs/ subtree below cannot swallow them: that handler
	// would read "admin" as a SKU and 404 the pair.
	mux.HandleFunc("/api/v1/companion-eggs/admin/catalog", h.handleEggAdminCatalog)
	mux.HandleFunc("/api/v1/companion-eggs/admin/catalog/", h.handleEggAdminCatalogBySKU)
	mux.HandleFunc("/api/v1/companion-eggs/checkout", h.handleEggCheckout)
	mux.HandleFunc("/api/v1/companion-eggs/", h.handleEggSubpath)
	// ADR-254 D9 alias, drop after the SPA cut (WP-X go): the pre-rename
	// /api/v1/me/familiars* + /api/v1/familiar-eggs/* paths are re-pathed onto
	// the companion routes above (same handler, same upstream).
	return withAliasPrefixes(mux, companionBridgeAliasPrefixes)
}

// WithCompanionBridge composes a CompanionBridge mux with a base handler:
// routes under /api/v1/me/companions or /api/v1/companion-eggs are served by
// the bridge; everything else falls through to `base`. Passes through when
// `b` is nil so cmd/server/main.go can opt out during env-driven dev mode.
func WithCompanionBridge(base http.Handler, b *companionbridge.Bridge) http.Handler {
	if b == nil {
		return base
	}
	bridgeMux := NewCompanionBridgeMux(b)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, p := range CompanionBridgePathPrefixes {
			if r.URL.Path == p || strings.HasPrefix(r.URL.Path, p+"/") {
				bridgeMux.ServeHTTP(w, r)
				return
			}
		}
		base.ServeHTTP(w, r)
	})
}

// -----------------------------------------------------------------------------
// Companion routes
// -----------------------------------------------------------------------------

// handleListCompanions — GET /api/v1/me/companions
func (h *CompanionBridgeHandler) handleListCompanions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	resp, _ := h.bridge.ListCompanions(r.Context(), bridgeAuthFromRequest(r))
	writeBridgeResp(w, resp)
}

// handleCompanionSubpath dispatches the per-companion routes under
// /api/v1/me/companions/{id}[/...] :
//
//	POST   /acquire              (learner-sovereign Discovery Graph — collection action)
//	GET    /bindings             (learner-sovereign Discovery Graph — collection list)
//	GET    /{id}                 (bare instance profile)
//	GET    /{id}/skills
//	POST   /{id}/skills
//	GET    /{id}/growth
//	POST   /{id}/reveal          (CHO-2229 — the breed roll, ahead of naming)
//	POST   /{id}/hatch
//	POST   /{id}/resonance
//	POST   /{id}/source-revelation
//	GET    /{id}/growth-events
//	POST   /{id}/retire          (soft-release; frees the roster slot — CHO-2033 SP3)
//	GET    /{id}/memory          (learner-sovereign Discovery Graph — per-Companion RAG memory)
//
// Dispatch ordering: the longer suffixed subpaths (/{id}/{leaf}) are matched
// FIRST via the `len(parts) == 2` switch. The single-segment `acquire` +
// `bindings` COLLECTION actions are special-cased in the `len(parts) == 1`
// branch BEFORE the bare instance GET (else they'd be mis-parsed as a {id}).
// The bare instance GET (/{id}) is served ONLY when there is no further subpath
// segment, so it never shadows the suffixed routes.
func (h *CompanionBridgeHandler) handleCompanionSubpath(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/me/companions/")
	rest = strings.TrimSuffix(rest, "/")
	parts := strings.Split(rest, "/")
	// Reject empty companion id (e.g. /api/v1/me/companions// or //growth).
	if parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	companionID := parts[0]

	// Single-segment routes. `acquire` + `bindings` are learner-sovereign
	// Discovery Graph COLLECTION actions (2026-07-01) that would OTHERWISE be
	// mis-parsed as a bare {id} instance GET — special-case them first so the
	// bare instance GET only ever sees a real companion id.
	if len(parts) == 1 {
		switch parts[0] {
		case "acquire":
			// POST /api/v1/me/companions/acquire — acquire a Companion.
			if r.Method != http.MethodPost {
				writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "POST only")
				return
			}
			body := readBody(r)
			resp, _ := h.bridge.AcquireCompanion(r.Context(), bridgeAuthFromRequest(r), body)
			writeBridgeResp(w, resp)
			return
		case "bindings":
			// GET /api/v1/me/companions/bindings — list the learner's bindings.
			if r.Method != http.MethodGet {
				writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
				return
			}
			resp, _ := h.bridge.ListCompanionBindings(r.Context(), bridgeAuthFromRequest(r))
			writeBridgeResp(w, resp)
			return
		}
		// Bare instance profile GET — no further subpath segment.
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
			return
		}
		resp, _ := h.bridge.GetCompanionInstance(r.Context(), bridgeAuthFromRequest(r), companionID)
		writeBridgeResp(w, resp)
		return
	}

	// Skills-subtree deep routes (CHO-2013 P1.B):
	//
	//	PUT/DELETE /{id}/skills/{key}/equip   equip / unequip
	//	POST       /{id}/skills/{key}/invoke  single-step Skill invoke runner
	if len(parts) == 4 && parts[1] == "skills" && parts[2] != "" {
		skillKey := parts[2]
		switch parts[3] {
		case "equip":
			switch r.Method {
			case http.MethodPut:
				resp, _ := h.bridge.SetCompanionSkillEquipped(r.Context(), bridgeAuthFromRequest(r), companionID, skillKey, true)
				writeBridgeResp(w, resp)
			case http.MethodDelete:
				resp, _ := h.bridge.SetCompanionSkillEquipped(r.Context(), bridgeAuthFromRequest(r), companionID, skillKey, false)
				writeBridgeResp(w, resp)
			default:
				writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "PUT or DELETE only")
			}
		case "invoke":
			if r.Method != http.MethodPost {
				writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "POST only")
				return
			}
			body := readBody(r)
			resp, _ := h.bridge.InvokeCompanionSkill(r.Context(), bridgeAuthFromRequest(r), companionID, skillKey, body)
			writeBridgeResp(w, resp)
		default:
			http.NotFound(w, r)
		}
		return
	}

	// Ceremony learning-edges routes (CHO-2040 CR §8 R7-3 / R8-5):
	//
	//	POST /{id}/ceremony/edge-scout          propose runner (crawl + turn)
	//	POST /{id}/ceremony/edge-scout/confirm  effect (c) confirm hook
	if len(parts) >= 3 && parts[1] == "ceremony" && parts[2] == "edge-scout" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "POST only")
			return
		}
		body := readBody(r)
		if len(parts) == 3 {
			resp, _ := h.bridge.ProposeCeremonyEdgeScout(r.Context(), bridgeAuthFromRequest(r), companionID, body)
			writeBridgeResp(w, resp)
			return
		}
		if len(parts) == 4 && parts[3] == "confirm" {
			resp, _ := h.bridge.ConfirmCeremonyEdgeScout(r.Context(), bridgeAuthFromRequest(r), companionID, body)
			writeBridgeResp(w, resp)
			return
		}
		http.NotFound(w, r)
		return
	}

	// CHO-2016 Grimoire Rituals routes (camelCase end-to-end):
	//	GET/POST /{id}/rituals               list / create draft
	//	GET      /{id}/rituals/{rid}         get one
	//	POST     /{id}/rituals/{rid}/publish publish a revision
	//	POST     /{id}/rituals/{rid}/run     run now
	//	GET      /{id}/rituals/{rid}/runs    run history
	if len(parts) >= 2 && parts[1] == "rituals" {
		auth := bridgeAuthFromRequest(r)
		switch len(parts) {
		case 2:
			switch r.Method {
			case http.MethodGet:
				resp, _ := h.bridge.ListRituals(r.Context(), auth, companionID)
				writeBridgeResp(w, resp)
			case http.MethodPost:
				resp, _ := h.bridge.CreateRitual(r.Context(), auth, companionID, readBody(r))
				writeBridgeResp(w, resp)
			default:
				writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET or POST only")
			}
		case 3:
			if r.Method != http.MethodGet {
				writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
				return
			}
			resp, _ := h.bridge.GetRitual(r.Context(), auth, companionID, parts[2])
			writeBridgeResp(w, resp)
		case 4:
			ritualID := parts[2]
			switch parts[3] {
			case "publish":
				if r.Method != http.MethodPost {
					writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "POST only")
					return
				}
				resp, _ := h.bridge.PublishRitual(r.Context(), auth, companionID, ritualID, readBody(r))
				writeBridgeResp(w, resp)
			case "run":
				if r.Method != http.MethodPost {
					writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "POST only")
					return
				}
				resp, _ := h.bridge.RunRitual(r.Context(), auth, companionID, ritualID, readBody(r))
				writeBridgeResp(w, resp)
			case "runs":
				if r.Method != http.MethodGet {
					writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
					return
				}
				resp, _ := h.bridge.ListRitualRuns(r.Context(), auth, companionID, ritualID)
				writeBridgeResp(w, resp)
			default:
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
		return
	}

	// All remaining suffixed routes are exactly /{id}/{leaf}.
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	leaf := parts[1]

	switch leaf {
	case "proofing-test":
		// CHO-2040 R8-6: the Virgin Proofing Test composed runner.
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "POST only")
			return
		}
		body := readBody(r)
		resp, _ := h.bridge.StartProofingTest(r.Context(), bridgeAuthFromRequest(r), companionID, body)
		writeBridgeResp(w, resp)
	case "retire":
		// CHO-2033 SP3: learner-initiated soft-release, frees a cap-3
		// roster slot. No request body.
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "POST only")
			return
		}
		resp, _ := h.bridge.RetireCompanion(r.Context(), bridgeAuthFromRequest(r), companionID)
		writeBridgeResp(w, resp)
	case "skills":
		switch r.Method {
		case http.MethodGet:
			resp, _ := h.bridge.ListCompanionSkills(r.Context(), bridgeAuthFromRequest(r), companionID)
			writeBridgeResp(w, resp)
		case http.MethodPost:
			body := readBody(r)
			resp, _ := h.bridge.GrantCompanionSkill(r.Context(), bridgeAuthFromRequest(r), companionID, body)
			writeBridgeResp(w, resp)
		default:
			writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET or POST only")
		}
	case "growth":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
			return
		}
		resp, _ := h.bridge.GetCompanionGrowth(r.Context(), bridgeAuthFromRequest(r), companionID)
		writeBridgeResp(w, resp)
	case "reveal":
		// CHO-2229: the breed roll, ahead of naming. POST with no body.
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "POST only")
			return
		}
		resp, _ := h.bridge.RevealBreed(r.Context(), bridgeAuthFromRequest(r), companionID)
		writeBridgeResp(w, resp)
	case "hatch":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "POST only")
			return
		}
		body := readBody(r)
		resp, _ := h.bridge.HatchEgg(r.Context(), bridgeAuthFromRequest(r), companionID, body)
		writeBridgeResp(w, resp)
	case "resonance":
		// CHO-2013 P1 (R3-1): the awakening resonant-concept pick; replaces
		// the retired kg-neighbors surface.
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "POST only")
			return
		}
		body := readBody(r)
		resp, _ := h.bridge.PickResonantConcept(r.Context(), bridgeAuthFromRequest(r), companionID, body)
		writeBridgeResp(w, resp)
	case "persona":
		// CHO-2015 (ADR-219 D2): the learner-designed Persona sheet. GET reads
		// the current editable view; PUT applies a full edit. camelCase
		// end-to-end (like rituals); the free-text note is Model-Armor-screened
		// downstream at save time.
		switch r.Method {
		case http.MethodGet:
			resp, _ := h.bridge.GetPersona(r.Context(), bridgeAuthFromRequest(r), companionID)
			writeBridgeResp(w, resp)
		case http.MethodPut:
			body := readBody(r)
			resp, _ := h.bridge.UpdatePersona(r.Context(), bridgeAuthFromRequest(r), companionID, body)
			writeBridgeResp(w, resp)
		default:
			writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET or PUT only")
		}
	case "source-revelation":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "POST only")
			return
		}
		body := readBody(r)
		resp, _ := h.bridge.TriggerSourceRevelation(r.Context(), bridgeAuthFromRequest(r), companionID, body)
		writeBridgeResp(w, resp)
	case "growth-events":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
			return
		}
		pageToken := r.URL.Query().Get("pageToken")
		// Allow snake_case query param too for backwards compatibility.
		if pageToken == "" {
			pageToken = r.URL.Query().Get("page_token")
		}
		pageSizeStr := r.URL.Query().Get("pageSize")
		if pageSizeStr == "" {
			pageSizeStr = r.URL.Query().Get("page_size")
		}
		pageSize := 0
		if pageSizeStr != "" {
			if n, err := strconv.Atoi(pageSizeStr); err == nil && n > 0 {
				pageSize = n
			}
		}
		resp, _ := h.bridge.ListGrowthEvents(r.Context(), bridgeAuthFromRequest(r), companionID, pageToken, pageSize)
		writeBridgeResp(w, resp)
	case "memory":
		// GET /api/v1/me/companions/{id}/memory — learner-sovereign Discovery
		// Graph per-Companion RAG memory read (2026-07-01).
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
			return
		}
		// ?goal_id= scopes the visible-neighbour block to one map (ADR-214).
		// Forwarded EXPLICITLY: the bridge rebuilds the upstream URL, so a
		// query param is dropped unless it is threaded through by hand.
		resp, _ := h.bridge.GetCompanionMemory(
			r.Context(), bridgeAuthFromRequest(r), companionID,
			strings.TrimSpace(r.URL.Query().Get("goal_id")),
		)
		writeBridgeResp(w, resp)
	case "chat":
		// ADR-154 — conversational chat (SSE pass-through). The proxy
		// owns the wire response (body is streamed; no Response envelope).
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "POST only")
			return
		}
		// Buffer the request body once so the proxy can re-read it (chat
		// requests are <1KB JSON — safe to fully buffer).
		bodyBytes := readBody(r)
		_ = h.bridge.ChatStream(r.Context(), bridgeAuthFromRequest(r), companionID, bytes.NewReader(bodyBytes), w)
	default:
		http.NotFound(w, r)
	}
}

// Ensure the io import stays anchored when only ChatStream uses it via
// bytes.NewReader. (bytes is also imported above.)
var _ io.Reader = bytes.NewReader(nil)

// -----------------------------------------------------------------------------
// Egg purchase routes
// -----------------------------------------------------------------------------

// handleEggCatalog — GET /api/v1/companion-eggs/catalog
func (h *CompanionBridgeHandler) handleEggCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	resp, _ := h.bridge.EggCatalog(r.Context(), bridgeAuthFromRequest(r))
	writeBridgeResp(w, resp)
}

// handleEggCheckout — POST /api/v1/companion-eggs/checkout
func (h *CompanionBridgeHandler) handleEggCheckout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "POST only")
		return
	}
	body := readBody(r)
	resp, _ := h.bridge.EggCheckout(r.Context(), bridgeAuthFromRequest(r), body)
	writeBridgeResp(w, resp)
}

// handleEggAdminCatalog — POST /api/v1/companion-eggs/admin/catalog (D6).
// The H+ pod-catalogue editor's save. chora-tenancy gates it on the
// gateway-stamped x-mesh-user-roles header, so a caller without a tenant-admin
// role is refused downstream with a 403 rather than here.
func (h *CompanionBridgeHandler) handleEggAdminCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "POST only")
		return
	}
	body := readBody(r)
	resp, _ := h.bridge.AdminEggUpsert(r.Context(), bridgeAuthFromRequest(r), body)
	writeBridgeResp(w, resp)
}

// handleEggAdminCatalogBySKU — GET /api/v1/companion-eggs/admin/catalog/{sku}
// (D6). The full entry the editor round-trips back through the upsert above.
func (h *CompanionBridgeHandler) handleEggAdminCatalogBySKU(w http.ResponseWriter, r *http.Request) {
	sku := strings.TrimPrefix(r.URL.Path, "/api/v1/companion-eggs/admin/catalog/")
	sku = strings.Trim(sku, "/")
	if sku == "" || strings.Contains(sku, "/") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	resp, _ := h.bridge.AdminEggEntry(r.Context(), bridgeAuthFromRequest(r), sku)
	writeBridgeResp(w, resp)
}

// handleEggSubpath dispatches /api/v1/companion-eggs/{sku}/odds (only odds is
// valid; anything else under the subtree → 404).
func (h *CompanionBridgeHandler) handleEggSubpath(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/companion-eggs/")
	rest = strings.TrimSuffix(rest, "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "odds" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	resp, _ := h.bridge.EggOdds(r.Context(), bridgeAuthFromRequest(r), parts[0])
	writeBridgeResp(w, resp)
}

// -----------------------------------------------------------------------------
// Shared helpers
// -----------------------------------------------------------------------------

// bridgeAuthFromRequest pulls the request's mesh-claims (validated by
// RequireChoraSessionJWT upstream of this mux) into a companionbridge.AuthCtx.
// When the JWT middleware has NOT run (test paths / legacy dev mode), GCID
// + role summary remain empty; X-Tenant-Id + Bearer fall back from inbound
// headers.
func bridgeAuthFromRequest(r *http.Request) companionbridge.AuthCtx {
	tp, _ := r.Context().Value(ctxKeyTraceparent).(string)
	ac := companionbridge.AuthCtx{
		Bearer:      bearerToken(r),
		Traceparent: tp,
		TenantID:    r.Header.Get("X-Tenant-Id"),
	}
	if mc, ok := MeshClaimsFromContext(r.Context()); ok {
		ac.GCID = mc.GCID
		if mc.TenantID != "" {
			ac.TenantID = mc.TenantID
		}
		ac.RoleSummary = mc.RoleSummary
		// Typed roles → x-mesh-user-roles downstream. Every Chora role gate fails
		// CLOSED on that header, so dropping it denies 100% of role-gated calls
		// (CHO-2148). Validated claims only — never a client-supplied header.
		if len(mc.Roles) > 0 {
			ac.Roles = append([]string(nil), mc.Roles...)
		}
	}
	return ac
}

// InjectMeshClaimsForTest is a test-only helper that stamps a MeshClaims
// onto the context as if RequireChoraSessionJWT had run. Exported for use
// by the companion_bridge_handler_test.go without re-implementing the
// mesh-claims extraction logic.
func InjectMeshClaimsForTest(ctx context.Context, gcid, tenantID string) context.Context {
	mc := &servicemesh.MeshClaims{GCID: gcid, TenantID: tenantID}
	return withMeshClaims(ctx, mc)
}

// InjectMeshClaimsWithRolesForTest is InjectMeshClaimsForTest with the
// validated ROLES under the test's control. The roles-free variant above
// cannot express a role gate, so a handler that gained one could only be
// tested through its refusal arm, which passes whether the accept-list is
// right or wrong.
func InjectMeshClaimsWithRolesForTest(ctx context.Context, gcid, tenantID string, roles ...string) context.Context {
	mc := &servicemesh.MeshClaims{GCID: gcid, TenantID: tenantID}
	if len(roles) > 0 {
		mc.Roles = append([]string(nil), roles...)
	}
	return withMeshClaims(ctx, mc)
}

// InjectChoraSessionClaimsForTest is a test-only helper that stamps the raw
// validated ChoraSession claims onto the context as if RequireChoraSessionJWT
// had run — WITHOUT also stamping MeshClaims. Used by the D1.5 regression
// tests to verify authCtxFromRequest's defensive fallback to the raw
// ChoraSession claims for identity-needing routes (e.g. daily-dose).
func InjectChoraSessionClaimsForTest(ctx context.Context, gcid, tenantID, email string) context.Context {
	cs := &chorasession.Claims{GCID: gcid, TenantID: tenantID, Email: email}
	return withChsClaims(ctx, cs)
}

// writeBridgeResp emits the aggregator Response on the wire with the
// canonical Content-Type / Cache-Control headers.
func writeBridgeResp(w http.ResponseWriter, resp companionbridge.Response) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if resp.Status == 0 {
		resp.Status = http.StatusInternalServerError
	}
	w.WriteHeader(resp.Status)
	if len(resp.Body) > 0 {
		_, _ = w.Write(resp.Body)
	}
}
