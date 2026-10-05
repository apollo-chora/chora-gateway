// Package companionbridge proxies the ADR-149 Companion Growth + Egg purchase
// routes from chora-gateway through to chora-consumption (growth) and
// chora-tenancy (egg purchase) per the PROD-D FE growth handoff (2026-05-13) §2.1.
//
// PROD-D scope (production-grade BFF routes):
//
//	GET    /api/v1/me/companions                            → chora-consumption GET /v1/me/companions
//	GET    /api/v1/me/companions/:id                        → chora-consumption GET /v1/me/companions/:id  (profile)
//	GET    /api/v1/me/companions/:id/skills                 → chora-consumption GET /v1/me/companions/:id/skills
//	POST   /api/v1/me/companions/:id/skills                 → chora-consumption POST /v1/me/companions/:id/skills
//	GET    /api/v1/me/companions/:id/growth                 → chora-consumption GET /v1/me/companions/:id/growth
//	POST   /api/v1/me/companions/:id/hatch                  → chora-consumption POST /v1/me/companions/:id/hatch
//	POST   /api/v1/me/companions/:id/resonance              → chora-consumption POST /v1/me/companions/:id/resonance
//	POST   /api/v1/me/companions/:id/source-revelation      → chora-consumption POST /v1/me/companions/:id/source-revelation
//	GET    /api/v1/me/companions/:id/growth-events          → chora-consumption GET /v1/me/companions/:id/growth-events
//	POST   /api/v1/me/companions/:id/retire                 → chora-consumption POST /v1/me/companions/:id/retire       (CHO-2033 SP3)
//	POST   /api/v1/me/companions/acquire                    → chora-consumption POST /v1/me/companions/acquire          (Discovery Graph)
//	GET    /api/v1/me/companions/bindings                   → chora-consumption GET /v1/me/companions/bindings          (Discovery Graph)
//	GET    /api/v1/me/companions/:id/memory                 → chora-consumption GET /v1/me/companions/:id/memory        (Discovery Graph)
//	GET    /api/v1/companion-eggs/catalog                   → chora-tenancy     GET /api/familiar-eggs/catalog      (tenancy upstream renames at W5)
//	GET    /api/v1/companion-eggs/:sku/odds                 → chora-tenancy     GET /api/familiar-eggs/:sku/odds    (tenancy upstream renames at W5)
//	POST   /api/v1/companion-eggs/checkout                  → chora-tenancy     POST /api/familiar-eggs/checkout    (tenancy upstream renames at W5)
//
// Stripe webhook (POST /webhooks/stripe on chora-tenancy) is INTENTIONALLY NOT
// proxied here — Stripe signature verification must happen at the receiving
// service to preserve replay protection.
//
// Wire contract:
//   - Bearer JWT enforced by chora-gateway's RequireChoraSessionJWT middleware
//     (replaces the legacy IdP-JWKS path per the 2026-05-14 ChoraSession
//     refactor). This package trusts that AuthCtx fields are already
//     populated by the BFF handler from validated mesh claims.
//   - Outbound calls stamp Authorization, traceparent, X-Tenant-Id, plus the
//     canonical mesh metadata via servicemesh.MarshalToHeaders so downstream
//     services see chora-gcid / chora-tenant-id / chora-role-summary.
//   - Per-route timeout: 30s for POST /hatch (Vertex AI Agent Engine roll
//     roundtrip through consumption); 5s default for everything else.
//   - 5xx → 502 GATEWAY_UPSTREAM_5XX. Timeout → 504 GATEWAY_UPSTREAM_TIMEOUT.
//     4xx (404, 401, 403, 422, 409, 402) pass through verbatim.
//   - Writes (POST) have NO retries. Reads (GET) get a single retry on 5xx
//     (the retry budget belongs to the BFF; downstream timeouts cap at
//     PerCallTimeout regardless).
//   - JSON responses are re-marshalled snake_case → camelCase for the FE.
//
// All service URLs come from SVC_*_URL env vars per memory
// feedback_no_inline_config — never inline.
package companionbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
)

// Default per-call timeouts.
const (
	DefaultPerCallTimeout = 5 * time.Second
	// DefaultHatchTimeout accommodates the consumption→Vertex AI Agent Engine
	// roll roundtrip per ADR-149. The hatch path consults the per-tenant
	// breed-odds distribution + creates an EventLog + may emit a Pub/Sub event.
	DefaultHatchTimeout = 30 * time.Second
	// DefaultChatTimeout accommodates the consumption→Vertex AI Agent Engine
	// streaming roundtrip per ADR-154 D1. The engine's longest documented
	// turn (full ebbinghaus_state tool call + 4-paragraph synthesis on
	// gemini-3.1-pro-preview) is ~25s; 60s leaves headroom for cold-start +
	// multi-tool round trips.
	DefaultChatTimeout = 60 * time.Second
)

// Config wires the two downstream service URLs and per-call timeouts.
// Sourced from env via LoadConfigFromEnv per memory feedback_no_inline_config.
type Config struct {
	ConsumptionURL string
	TenancyURL     string

	PerCallTimeout time.Duration
	HatchTimeout   time.Duration
	ChatTimeout    time.Duration // ADR-154 — 60s default per-call cap on SSE stream
}

// ApplyDefaults sets sensible per-route timeouts when fields are zero.
func (c *Config) ApplyDefaults() {
	if c.PerCallTimeout == 0 {
		c.PerCallTimeout = DefaultPerCallTimeout
	}
	if c.HatchTimeout == 0 {
		c.HatchTimeout = DefaultHatchTimeout
	}
	if c.ChatTimeout == 0 {
		c.ChatTimeout = DefaultChatTimeout
	}
}

// LoadConfigFromEnv reads SVC_CONSUMPTION_URL + SVC_TENANCY_URL.
func LoadConfigFromEnv() Config {
	c := Config{
		ConsumptionURL: os.Getenv("SVC_CONSUMPTION_URL"),
		TenancyURL:     os.Getenv("SVC_TENANCY_URL"),
	}
	c.ApplyDefaults()
	return c
}

// AuthCtx carries the per-request mesh-trust + tracing values stamped on
// outbound calls. Populated by the BFF handler from validated JWT claims.
type AuthCtx struct {
	Bearer      string
	Traceparent string
	TenantID    string
	GCID        string
	RoleSummary map[string]any
	// Roles is the typed role list from the VALIDATED session / mesh claims —
	// never a client-supplied header. It becomes the `x-mesh-user-roles` header.
	// Every Chora role gate reads it and FAILS CLOSED (fail-open was deleted in
	// CHO-2072), so an AuthCtx without Roles is denied 100% of the time by any
	// role-gated downstream. Guarded by upstream/mesh_roles_guard_test.go.
	Roles []string
}

// Response is the normalised aggregator output forwarded to the FE. Body is
// camelCase JSON; Status mirrors the downstream status (with 5xx normalised
// to 502 and timeouts surfaced as 504).
type Response struct {
	Status  int
	Headers http.Header
	Body    []byte
}

// Bridge fans out the Companion BFF routes (PROD-D growth + egg purchase +
// the bare instance profile GET + /skills grants) to chora-consumption +
// chora-tenancy.
type Bridge struct {
	cfg    Config
	client *http.Client
}

// New constructs a Bridge with defaults applied.
func New(cfg Config) *Bridge {
	cfg.ApplyDefaults()
	// http.Client.Timeout is the upper bound across all routes (use the
	// longest of Hatch / Chat). Per-route timeouts apply within via context
	// deadlines so each route stays bounded to its own budget.
	upper := cfg.HatchTimeout
	if cfg.ChatTimeout > upper {
		upper = cfg.ChatTimeout
	}
	return &Bridge{
		cfg:    cfg,
		client: &http.Client{Timeout: upper + 5*time.Second},
	}
}

// -----------------------------------------------------------------------------
// HTTP plumbing
// -----------------------------------------------------------------------------

type callResult struct {
	status int
	body   []byte
	header http.Header
	err    error
}

// call performs one outbound request with mesh-trust headers stamped, honouring
// the supplied per-call timeout.
func (b *Bridge) call(ctx context.Context, method, urlStr string, body []byte, auth AuthCtx, timeout time.Duration) callResult {
	return b.callWithExtraHeaders(ctx, method, urlStr, body, auth, timeout, nil)
}

// callWithExtraHeaders is `call` plus per-call additional headers (e.g.,
// `X-GCID` for chora-tenancy egg-checkout per its checkout handler contract).
func (b *Bridge) callWithExtraHeaders(ctx context.Context, method, urlStr string, body []byte, auth AuthCtx, timeout time.Duration, extra map[string]string) callResult {
	if urlStr == "" {
		return callResult{err: fmt.Errorf("companionbridge: empty url for %s", method)}
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(callCtx, method, urlStr, bodyReader)
	if err != nil {
		return callResult{err: fmt.Errorf("companionbridge: build request: %w", err)}
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+auth.Bearer)
	}
	if auth.Traceparent != "" {
		req.Header.Set("traceparent", auth.Traceparent)
	}
	if auth.TenantID != "" {
		req.Header.Set("X-Tenant-Id", auth.TenantID)
	}
	if auth.GCID != "" {
		// X-Chora-GCID is the explicit BFF→downstream mesh hint per S3.6.
		req.Header.Set("X-Chora-GCID", auth.GCID)
		// Bare lowercase `gcid` — chora-consumption's legacy `requireContext`
		// (services/chora-consumption/internal/adapter/http/router.go:659)
		// reads this exact header. The mesh-trust `chora-gcid` set by
		// servicemesh.MarshalToHeaders below DOES NOT satisfy it because
		// consumption's requireContext predates the mesh-trust integration
		// and looks up the bare key. Stamping both keeps the canonical mesh
		// path intact AND unblocks the consumption-bound routes
		// (ListCompanions + 5 sibling growth handlers) per the Surface B fix
		// 2026-05-15. Harmless for chora-tenancy egg routes (they read
		// X-GCID via the per-call extras path, not bare gcid).
		req.Header.Set("gcid", auth.GCID)
	}
	// Mesh-trust headers (canonical chora-gcid / chora-tenant-id / chora-role-summary).
	mesh := servicemesh.MarshalToHeaders(servicemesh.MeshClaims{
		GCID:        auth.GCID,
		TenantID:    auth.TenantID,
		Roles:       auth.Roles,
		RoleSummary: auth.RoleSummary,
	})
	for k, vs := range mesh {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	// Per-call extra headers (e.g., X-GCID for chora-tenancy egg-checkout —
	// distinct from X-Chora-GCID + mesh-trust chora-gcid).
	for k, v := range extra {
		if v == "" {
			continue
		}
		req.Header.Set(k, v)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return callResult{err: err}
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return callResult{status: resp.StatusCode, body: out, header: resp.Header}
}

// classify converts a callResult into a Response with snake→camel applied.
// 5xx → 502; timeout → 504; 2xx + 4xx pass through with body re-marshalled.
func classify(cr callResult) Response {
	if cr.err != nil {
		if errors.Is(cr.err, context.DeadlineExceeded) {
			return errResp(http.StatusGatewayTimeout, "GATEWAY_UPSTREAM_TIMEOUT", cr.err.Error())
		}
		if errors.Is(cr.err, context.Canceled) {
			return errResp(http.StatusGatewayTimeout, "GATEWAY_REQUEST_CANCELED", cr.err.Error())
		}
		var urlErr *url.Error
		if errors.As(cr.err, &urlErr) && urlErr.Timeout() {
			return errResp(http.StatusGatewayTimeout, "GATEWAY_UPSTREAM_TIMEOUT", cr.err.Error())
		}
		return errResp(http.StatusBadGateway, "GATEWAY_UPSTREAM_ERROR", cr.err.Error())
	}
	if cr.status >= 500 {
		return errResp(http.StatusBadGateway, "GATEWAY_UPSTREAM_5XX",
			fmt.Sprintf("upstream returned %d", cr.status))
	}
	camel := SnakeToCamelJSON(cr.body)
	return Response{Status: cr.status, Body: camel, Headers: cr.header}
}

// envelopeFixup is a post-camel-case hook to reshape the body BEFORE
// the {data: T} envelope wrap. Used for FE field-name reconciliations
// (e.g., catalog `items` → `skus`, egg `priceCents` → `priceMicros`).
// `body` is already camelCase JSON. Return value is the reshaped body.
type envelopeFixup func(body []byte) []byte

// classifyWithEnvelope is `classify` plus the BFF {data: T} envelope
// wrap on 2xx responses. 4xx error bodies are reshaped into the BFF
// {error: {code, message}} envelope so the FE `unwrap()` helper can
// read `env.error.message`. 5xx + timeout + transport errors continue
// to surface via the existing error envelope shape produced by `errResp`
// (already `{error: {code, message}}`).
//
// The optional `fixup` parameter rewrites the camelCased body BEFORE
// the wrap — used for catalog (items→skus, priceCents→priceMicros) and
// odds (sku→eggSku) reconciliations.
//
// Per E2E-BE-FAM-GROWTH §3 (2026-05-16): the 8 endpoints all return
// {data: T} on success and {error: {code, message}} on failure so the
// FE CompanionGrowthService.unwrap() helper can read `env.data` or
// `env.error?.message` unambiguously. The raw chora-consumption /
// chora-tenancy services emit unwrapped shapes; the envelope is the
// gateway's BFF responsibility.
func classifyWithEnvelope(cr callResult, fixup envelopeFixup) Response {
	resp := classify(cr)
	// 4xx — reshape raw {code, message} into {error: {code, message}}.
	if resp.Status >= 400 && resp.Status < 500 {
		resp.Body = ensureErrorEnvelope(resp.Body)
		return resp
	}
	// 5xx + timeout — `errResp` already produced {error: {code, message}}.
	if resp.Status >= 500 {
		return resp
	}
	body := resp.Body
	if fixup != nil {
		body = fixup(body)
	}
	wrapped := wrapInData(body)
	if wrapped == nil {
		return resp // non-JSON body — leave untouched.
	}
	resp.Body = wrapped
	return resp
}

// ensureErrorEnvelope guarantees the 4xx body has the canonical
// `{error: {code, message}}` shape. chora-consumption's `writeError`
// helper emits the raw `{code, message}` form (no `error` wrapper); the
// FE BffEnvelope contract expects the wrapped form. If the body already
// has an `error` key (chora-tenancy errors), it passes through. Returns
// the input unchanged on non-JSON.
func ensureErrorEnvelope(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	if _, hasError := m["error"]; hasError {
		return body
	}
	// Only wrap if both `code` + `message` are present at the top level.
	_, hasCode := m["code"]
	_, hasMsg := m["message"]
	if !hasCode && !hasMsg {
		return body
	}
	wrapped := map[string]any{"error": m}
	out, err := json.Marshal(wrapped)
	if err != nil {
		return body
	}
	return out
}

// wrapInData wraps a JSON value in `{"data": <value>}`. Returns nil
// when the input is not valid JSON (caller decides what to do).
func wrapInData(body []byte) []byte {
	if len(body) == 0 {
		return []byte(`{"data":null}`)
	}
	// Validate as JSON; bail to nil if not parseable.
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil
	}
	out, err := json.Marshal(map[string]any{"data": v})
	if err != nil {
		return nil
	}
	return out
}

// renameKey produces an envelopeFixup that renames a top-level key in
// the camelCased JSON body. No-op when `from` is absent.
func renameKey(from, to string) envelopeFixup {
	return func(body []byte) []byte {
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			return body
		}
		if v, ok := m[from]; ok {
			if _, collide := m[to]; !collide {
				m[to] = v
				delete(m, from)
			}
		}
		out, err := json.Marshal(m)
		if err != nil {
			return body
		}
		return out
	}
}

// catalogFixup reshapes the chora-tenancy egg catalog response so the
// chora-web `CompanionGrowthService.getEggCatalog` consumer sees the FE
// model shape:
//
//	items     → skus
//	priceCents (each row) → priceMicros (×10_000)
//	total     dropped (FE doesn't model it)
//
// This is the only place in the codebase where the FE CompanionEgg.EggSku
// shape diverges from the tenancy wire — the bridge owns the translation
// per its BFF role.
func catalogFixup(body []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	itemsAny, ok := m["items"]
	if !ok {
		return body
	}
	items, ok := itemsAny.([]any)
	if !ok {
		return body
	}
	for i, raw := range items {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if pc, present := row["priceCents"]; present {
			if pcf, ok := pc.(float64); ok {
				row["priceMicros"] = pcf * 10000
				delete(row, "priceCents")
			}
		}
		items[i] = row
	}
	delete(m, "items")
	delete(m, "total")
	m["skus"] = items
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func errResp(status int, code, message string) Response {
	body := fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, code, message)
	return Response{Status: status, Body: []byte(body)}
}

// -----------------------------------------------------------------------------
// Companion Growth routes (chora-consumption)
// -----------------------------------------------------------------------------

// ListCompanions proxies GET /api/v1/me/companions → chora-consumption.
func (b *Bridge) ListCompanions(ctx context.Context, auth AuthCtx) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions"
	cr := b.callWithGetRetry(ctx, http.MethodGet, u, nil, auth, b.cfg.PerCallTimeout)
	return classify(cr), nil
}

// GetCompanionInstance proxies the bare instance GET
// GET /api/v1/me/companions/:id → chora-consumption GET /v1/me/companions/:id,
// which returns the full Companion profile (companion_id, display_name,
// growth_stage, nested growth_state{…}, cosmetic{…}, roster_context{…}, and
// optional memory_summary).
//
// Unlike the /growth + egg routes, the profile is returned UNWRAPPED (no
// {data: T} envelope) — it mirrors ListCompanions where the list items are
// likewise unwrapped. The response is re-marshalled snake_case→camelCase via
// classify (which calls SnakeToCamelJSON; that helper recurses into the nested
// growth_state / cosmetic / roster_context objects).
func (b *Bridge) GetCompanionInstance(ctx context.Context, auth AuthCtx, companionID string) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID)
	cr := b.callWithGetRetry(ctx, http.MethodGet, u, nil, auth, b.cfg.PerCallTimeout)
	return classify(cr), nil
}

// ListCompanionSkills proxies GET /api/v1/me/companions/:id/skills →
// chora-consumption GET /v1/me/companions/:id/skills (skill-grant projection:
// companion_id, skill_grants[], skill_slots_unlocked, evolution_tier).
// Response re-marshalled snake_case→camelCase; returned unwrapped.
func (b *Bridge) ListCompanionSkills(ctx context.Context, auth AuthCtx, companionID string) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) + "/skills"
	cr := b.callWithGetRetry(ctx, http.MethodGet, u, nil, auth, b.cfg.PerCallTimeout)
	return classify(cr), nil
}

// GrantCompanionSkill proxies POST /api/v1/me/companions/:id/skills →
// chora-consumption POST /v1/me/companions/:id/skills. NO retries on POST.
//
// Request body translation: the FE posts camelCase ({skillId}); chora-consumption
// decodes snake_case ({skill_id}). CamelToSnakeJSON bridges the contract.
//
// The downstream status is preserved through classify — in particular the
// 409 SKILL_SLOTS_FULL conflict passes through verbatim (4xx bodies are
// re-marshalled snake→camel but the status + shape are preserved) so the FE
// can render the slots-full state. Response snake_case→camelCase.
func (b *Bridge) GrantCompanionSkill(ctx context.Context, auth AuthCtx, companionID string, body []byte) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) + "/skills"
	return classify(b.call(ctx, http.MethodPost, u, CamelToSnakeJSON(body), auth, b.cfg.PerCallTimeout)), nil
}

// SetCompanionSkillEquipped proxies the loadout equip/unequip pair
// (CHO-2013 P1.B — the P1.C Loadout tab's write surface):
//
//	PUT    /api/v1/me/companions/:id/skills/:key/equip → consumption PUT (equip)
//	DELETE /api/v1/me/companions/:id/skills/:key/equip → consumption DELETE (unequip)
//
// No body either way; NO retries (mutating). 409 SKILL_SLOTS_FULL /
// SKILL_NOT_OWNED / SKILL_NOT_ACTIVE / CRAFT_SKILL_ALWAYS_ON pass through
// verbatim (status + code) so the FE can render the conflict states.
// Response = the loadout view, unwrapped snake→camel (skills family).
func (b *Bridge) SetCompanionSkillEquipped(ctx context.Context, auth AuthCtx, companionID, skillKey string, equip bool) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) +
		"/skills/" + url.PathEscape(skillKey) + "/equip"
	method := http.MethodPut
	if !equip {
		method = http.MethodDelete
	}
	return classify(b.call(ctx, method, u, nil, auth, b.cfg.PerCallTimeout)), nil
}

// InvokeCompanionSkill proxies POST /api/v1/me/companions/:id/skills/:key/invoke
// → chora-consumption POST /v1/me/companions/:id/skills/:key/invoke — the
// CHO-2013 P1.B (R4-4) single-step Skill invoke runner. NO retries (the run
// drives a metered LLM turn — a blind retry could double-charge). Uses the
// extended HatchTimeout budget: the runner's turn is a full consumption →
// companion-agent → model-gateway round trip, not a 5s proxy hop.
//
// Request body camelCase→snake_case translated ({params:{...}} keys are
// single-word and pass through unchanged). Response unwrapped snake→camel
// ({skill_key, reply, recorded, mana_charged, turn_id} → camel); 4xx pass
// through (402 insufficient_mana upsell + the 409 family) status-preserved.
func (b *Bridge) InvokeCompanionSkill(ctx context.Context, auth AuthCtx, companionID, skillKey string, body []byte) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) +
		"/skills/" + url.PathEscape(skillKey) + "/invoke"
	return classify(b.call(ctx, http.MethodPost, u, CamelToSnakeJSON(body), auth, b.cfg.HatchTimeout)), nil
}

// ProposeCeremonyEdgeScout proxies POST /api/v1/me/companions/:id/ceremony/edge-scout
// (CHO-2040 CR §8 R7-3): the binding-ceremony propose runner — 3-source crawl
// + ONE extraction turn, so it rides the extended HatchTimeout budget like
// invoke/hatch. NO retries on POST (the mana turn must not double-fire).
func (b *Bridge) ProposeCeremonyEdgeScout(ctx context.Context, auth AuthCtx, companionID string, body []byte) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) + "/ceremony/edge-scout"
	return classify(b.call(ctx, http.MethodPost, u, CamelToSnakeJSON(body), auth, b.cfg.HatchTimeout)), nil
}

// ConfirmCeremonyEdgeScout proxies POST /api/v1/me/companions/:id/ceremony/edge-scout/confirm
// (CHO-2040 R8-5 effect (c)): no LLM turn — per-call timeout; NO retries on
// POST (the confirm emits events; idempotency lives server-side).
func (b *Bridge) ConfirmCeremonyEdgeScout(ctx context.Context, auth AuthCtx, companionID string, body []byte) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) + "/ceremony/edge-scout/confirm"
	return classify(b.call(ctx, http.MethodPost, u, CamelToSnakeJSON(body), auth, b.cfg.PerCallTimeout)), nil
}

// StartProofingTest proxies POST /api/v1/me/companions/:id/proofing-test
// (CHO-2040 R8-6): reserve + publish only (generation is async) — per-call
// timeout; NO retries on POST (the mana reservation must not double-fire).
func (b *Bridge) StartProofingTest(ctx context.Context, auth AuthCtx, companionID string, body []byte) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) + "/proofing-test"
	return classify(b.call(ctx, http.MethodPost, u, CamelToSnakeJSON(body), auth, b.cfg.PerCallTimeout)), nil
}

// RetireCompanion proxies POST /api/v1/me/companions/:id/retire (CHO-2033
// SP3): learner-initiated soft-release of a roster companion, freeing a
// cap-3 slot (pseudonymise-not-delete — ddd-enforcement #4; the row +
// growth ledger persist server-side). No request body — mirrors
// SetCompanionSkillEquipped's no-body convention. Per-call timeout; NO
// retries on POST (the mana-free retire still emits companion.retired.v1 —
// a blind retry could double-publish the audit event, and consumption
// already renders a double-retire as 404 since Get excludes soft-deleted
// rows, so a retry would surface as a spurious not-found rather than a
// safe no-op).
func (b *Bridge) RetireCompanion(ctx context.Context, auth AuthCtx, companionID string) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) + "/retire"
	return classify(b.call(ctx, http.MethodPost, u, nil, auth, b.cfg.PerCallTimeout)), nil
}

// GetCompanionGrowth proxies GET /api/v1/me/companions/:id/growth.
// BFF-wrapped in {data: T} per E2E-BE-FAM-GROWTH §3.
func (b *Bridge) GetCompanionGrowth(ctx context.Context, auth AuthCtx, companionID string) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) + "/growth"
	cr := b.callWithGetRetry(ctx, http.MethodGet, u, nil, auth, b.cfg.PerCallTimeout)
	return classifyWithEnvelope(cr, nil), nil
}

// ---------------------------------------------------------------------------
// CHO-2015 Grimoire Persona (ADR-219 D2). Persona is camelCase END-TO-END:
// chora-consumption's persona handlers decode + emit camelCase directly, so
// (like rituals) these methods pass the request body through VERBATIM (no
// CamelToSnakeJSON) and classify the response. classify preserves the
// downstream status (200 view / 404 COMPANION_NOT_FOUND / 422 PERSONA_INVALID |
// PERSONA_NOTE_BLOCKED / 502 PERSONA_SCREEN_FAILED / 503 PERSONA_NOT_WIRED |
// PERSONA_GUARDRAIL_NOT_WIRED) and is a no-op on the already-camel keys.
// ---------------------------------------------------------------------------

// GetPersona proxies GET /api/v1/me/companions/:id/persona.
func (b *Bridge) GetPersona(ctx context.Context, auth AuthCtx, companionID string) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) + "/persona"
	return classify(b.callWithGetRetry(ctx, http.MethodGet, u, nil, auth, b.cfg.PerCallTimeout)), nil
}

// UpdatePersona proxies PUT /api/v1/me/companions/:id/persona.
func (b *Bridge) UpdatePersona(ctx context.Context, auth AuthCtx, companionID string, body []byte) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) + "/persona"
	return classify(b.call(ctx, http.MethodPut, u, body, auth, b.cfg.PerCallTimeout)), nil
}

// ---------------------------------------------------------------------------
// CHO-2016 Grimoire Rituals v1 (ADR-219). The rituals contract is camelCase
// END-TO-END: chora-consumption's ritual handlers decode + emit camelCase
// directly, so — unlike the skill/hatch routes — these methods do NOT apply
// CamelToSnakeJSON to the request body (pass-through). classify preserves the
// downstream status (403 RITUALS_LOCKED / 404 RITUAL_NOT_FOUND / 409
// RITUAL_QUOTA_REACHED / 422 RITUAL_INVALID / 200 skipped_budget) and is a
// no-op on the already-camel response keys.
// ---------------------------------------------------------------------------

// ListRituals proxies GET /api/v1/me/companions/:id/rituals.
func (b *Bridge) ListRituals(ctx context.Context, auth AuthCtx, companionID string) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) + "/rituals"
	return classify(b.callWithGetRetry(ctx, http.MethodGet, u, nil, auth, b.cfg.PerCallTimeout)), nil
}

// CreateRitual proxies POST /api/v1/me/companions/:id/rituals (create draft).
func (b *Bridge) CreateRitual(ctx context.Context, auth AuthCtx, companionID string, body []byte) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) + "/rituals"
	return classify(b.call(ctx, http.MethodPost, u, body, auth, b.cfg.PerCallTimeout)), nil
}

// GetRitual proxies GET /api/v1/me/companions/:id/rituals/:rid.
func (b *Bridge) GetRitual(ctx context.Context, auth AuthCtx, companionID, ritualID string) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) + "/rituals/" + url.PathEscape(ritualID)
	return classify(b.callWithGetRetry(ctx, http.MethodGet, u, nil, auth, b.cfg.PerCallTimeout)), nil
}

// PublishRitual proxies POST /api/v1/me/companions/:id/rituals/:rid/publish.
func (b *Bridge) PublishRitual(ctx context.Context, auth AuthCtx, companionID, ritualID string, body []byte) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) + "/rituals/" + url.PathEscape(ritualID) + "/publish"
	return classify(b.call(ctx, http.MethodPost, u, body, auth, b.cfg.PerCallTimeout)), nil
}

// RunRitual proxies POST /api/v1/me/companions/:id/rituals/:rid/run. NO retries.
func (b *Bridge) RunRitual(ctx context.Context, auth AuthCtx, companionID, ritualID string, body []byte) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) + "/rituals/" + url.PathEscape(ritualID) + "/run"
	return classify(b.call(ctx, http.MethodPost, u, body, auth, b.cfg.PerCallTimeout)), nil
}

// ListRitualRuns proxies GET /api/v1/me/companions/:id/rituals/:rid/runs.
func (b *Bridge) ListRitualRuns(ctx context.Context, auth AuthCtx, companionID, ritualID string) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) + "/rituals/" + url.PathEscape(ritualID) + "/runs"
	return classify(b.callWithGetRetry(ctx, http.MethodGet, u, nil, auth, b.cfg.PerCallTimeout)), nil
}

// HatchEgg proxies POST /api/v1/me/companions/:id/hatch with the extended 30s
// budget (the consumption→Vertex AI Agent Engine roll). NO retries on POST.
// BFF-wrapped in {data: T}.
//
// Request body translation: FE posts camelCase ({displayName, resonantAtomId,
// tone, learnerPersona}); chora-consumption decodes snake_case ({display_name,
// resonant_atom_id, tone, learner_persona}) per companion_growth_handlers.go
// hatchEggReq. Apply CamelToSnakeJSON outbound to bridge the contract.
func (b *Bridge) HatchEgg(ctx context.Context, auth AuthCtx, companionID string, body []byte) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) + "/hatch"
	return classifyWithEnvelope(b.call(ctx, http.MethodPost, u, CamelToSnakeJSON(body), auth, b.cfg.HatchTimeout), nil), nil
}

// RevealBreed proxies POST /api/v1/me/companions/:id/reveal (CHO-2229 —
// the breed roll, split ahead of naming per the ADR-228 Phase 2 amendment).
// No request body. BFF-wrapped in {data: T}. NO retries on POST — the
// endpoint is idempotent server-side (a replay returns the persisted roll),
// but the write posture mirrors HatchEgg's.
func (b *Bridge) RevealBreed(ctx context.Context, auth AuthCtx, companionID string) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) + "/reveal"
	return classifyWithEnvelope(b.call(ctx, http.MethodPost, u, nil, auth, b.cfg.PerCallTimeout), nil), nil
}

// PickResonantConcept proxies POST /api/v1/me/companions/:id/resonance
// (CHO-2013 P1, R3-1 — replaces the retired kg-neighbors pick). BFF-wrapped
// in {data: T}. Request body camelCase→snake_case translated for the
// consumption pickResonanceReq decoder ({conceptId} → {concept_id}).
func (b *Bridge) PickResonantConcept(ctx context.Context, auth AuthCtx, companionID string, body []byte) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) + "/resonance"
	return classifyWithEnvelope(b.call(ctx, http.MethodPost, u, CamelToSnakeJSON(body), auth, b.cfg.PerCallTimeout), nil), nil
}

// TriggerSourceRevelation proxies POST /api/v1/me/companions/:id/source-revelation.
// BFF-wrapped in {data: T}. Request body camelCase→snake_case translated.
func (b *Bridge) TriggerSourceRevelation(ctx context.Context, auth AuthCtx, companionID string, body []byte) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) + "/source-revelation"
	return classifyWithEnvelope(b.call(ctx, http.MethodPost, u, CamelToSnakeJSON(body), auth, b.cfg.PerCallTimeout), nil), nil
}

// ListGrowthEvents proxies GET /api/v1/me/companions/:id/growth-events.
// BFF-wrapped in {data: T}.
func (b *Bridge) ListGrowthEvents(ctx context.Context, auth AuthCtx, companionID, pageToken string, pageSize int) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) + "/growth-events"
	q := url.Values{}
	if pageToken != "" {
		q.Set("page_token", pageToken)
	}
	if pageSize > 0 {
		q.Set("page_size", strconv.Itoa(pageSize))
	}
	if encoded := q.Encode(); encoded != "" {
		u += "?" + encoded
	}
	cr := b.callWithGetRetry(ctx, http.MethodGet, u, nil, auth, b.cfg.PerCallTimeout)
	return classifyWithEnvelope(cr, nil), nil
}

// -----------------------------------------------------------------------------
// Learner-sovereign Discovery Graph — Companion acquire / bindings / memory
// (2026-07-01). These proxy the new chora-consumption learner routes:
//
//	POST /api/v1/me/companions/acquire     → chora-consumption POST /v1/me/companions/acquire
//	GET  /api/v1/me/companions/bindings    → chora-consumption GET  /v1/me/companions/bindings
//	GET  /api/v1/me/companions/:id/memory  → chora-consumption GET  /v1/me/companions/:id/memory
//
// They mirror the sibling growth reads: acquire is a POST (NO retries, like
// HatchEgg — camelCase request body translated to snake_case for the
// consumption decoder); bindings + memory are GETs (single 5xx retry, like
// ListCompanions / GetCompanionInstance). All three return UNWRAPPED
// snake→camel bodies (via classify) — they are the learner-sovereign family,
// distinct from the ADR-149 {data:T}-enveloped gacha/growth family.
// -----------------------------------------------------------------------------

// AcquireCompanion proxies POST /api/v1/me/companions/acquire → chora-consumption
// POST /v1/me/companions/acquire (learner acquires a Companion for the Discovery
// Graph). NO retries on POST. The sovereign consumption handler decodes
// camelCase ({mapTheme, companionName, mode}) — like the concept-graph + goals
// sovereign endpoints — so the body is forwarded VERBATIM (NOT snake-cased like
// the legacy ADR-149 HatchEgg/GrantCompanionSkill family). classify() on the
// response is a no-op on the already-camelCase acquire result.
func (b *Bridge) AcquireCompanion(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/acquire"
	return classify(b.call(ctx, http.MethodPost, u, body, auth, b.cfg.PerCallTimeout)), nil
}

// ListCompanionBindings proxies GET /api/v1/me/companions/bindings →
// chora-consumption GET /v1/me/companions/bindings (the learner's Companion⇄map
// bindings). Response re-marshalled snake_case→camelCase; returned UNWRAPPED,
// mirroring ListCompanions.
func (b *Bridge) ListCompanionBindings(ctx context.Context, auth AuthCtx) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/bindings"
	cr := b.callWithGetRetry(ctx, http.MethodGet, u, nil, auth, b.cfg.PerCallTimeout)
	return classify(cr), nil
}

// GetCompanionMemory proxies GET /api/v1/me/companions/:id/memory →
// chora-consumption GET /v1/me/companions/:id/memory (the per-Companion RAG
// memory projection). Response re-marshalled snake_case→camelCase; returned
// UNWRAPPED, mirroring GetCompanionInstance / ListCompanionSkills.
// goalID scopes the visible-neighbour block to one map's concept subtree
// (ADR-214). It MUST be forwarded explicitly: this bridge rebuilds the upstream
// URL from scratch rather than proxying the raw request, so a query param does
// NOT ride along on its own. Dropping it silently reverts consumption to the
// unscoped read, which lists every one of the learner's other maps.
func (b *Bridge) GetCompanionMemory(ctx context.Context, auth AuthCtx, companionID, goalID string) (Response, error) {
	u := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) + "/memory"
	if g := strings.TrimSpace(goalID); g != "" {
		u += "?goal_id=" + url.QueryEscape(g)
	}
	cr := b.callWithGetRetry(ctx, http.MethodGet, u, nil, auth, b.cfg.PerCallTimeout)
	return classify(cr), nil
}

// -----------------------------------------------------------------------------
// ADR-154 — conversational chat (SSE) — streaming pass-through
// -----------------------------------------------------------------------------

// ChatStream proxies POST /api/v1/me/companions/:id/chat → chora-consumption
// /v1/me/companions/:id/chat with full SSE pass-through semantics.
//
// The signature differs from the other 9 companionbridge methods because SSE
// streaming requires direct ResponseWriter access — frames flow from the
// engine through chora-consumption through chora-gateway to the FE
// byte-for-byte (no buffering at the gateway layer beyond per-line scan).
//
// Per ADR-154 D1: Content-Type: text/event-stream + Cache-Control: no-cache
// + X-Accel-Buffering: no headers are forwarded verbatim from the upstream
// response so intermediate proxies (Envoy / GCLB) do not buffer.
//
// Per-route timeout: cfg.ChatTimeout (default 60s).
//
// Error handling:
//   - Empty ConsumptionURL                 → 502 GATEWAY_UPSTREAM_NOT_CONFIGURED
//   - Transport error before headers       → 502 GATEWAY_UPSTREAM_ERROR
//   - Context deadline before headers      → 504 GATEWAY_UPSTREAM_TIMEOUT
//   - Upstream 5xx                          → 502 GATEWAY_UPSTREAM_5XX (JSON body)
//   - Upstream 4xx                          → pass-through verbatim (JSON body)
//   - Upstream 2xx (text/event-stream)      → flush headers, stream body
//
// The caller (CompanionBridgeHandler) MUST pre-validate the JWT + extract the
// AuthCtx; this method assumes those upstream invariants. body is the
// pre-buffered chat request JSON (typically <1KB; safe to buffer).
//
// Returns nil when the upstream response was written (success OR upstream
// 4xx/5xx with a body). Returns a non-nil error only when the proxy itself
// could not produce any output (e.g. malformed URL).
func (b *Bridge) ChatStream(ctx context.Context, auth AuthCtx, companionID string, body io.Reader, w http.ResponseWriter) error {
	if b.cfg.ConsumptionURL == "" {
		writeErrJSON(w, http.StatusBadGateway, "GATEWAY_UPSTREAM_NOT_CONFIGURED",
			"consumption service URL empty (SVC_CONSUMPTION_URL unset)")
		return nil
	}
	upstreamURL := b.cfg.ConsumptionURL + "/v1/me/companions/" + url.PathEscape(companionID) + "/chat"

	callCtx, cancel := context.WithTimeout(ctx, b.cfg.ChatTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, upstreamURL, body)
	if err != nil {
		writeErrJSON(w, http.StatusBadGateway, "GATEWAY_UPSTREAM_ERROR",
			fmt.Sprintf("build request: %v", err))
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	// FE consumes SSE; downstream emits SSE — but standard HTTP intermediaries
	// require the Accept header to acknowledge the streaming wire format.
	req.Header.Set("Accept", "text/event-stream")
	if auth.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+auth.Bearer)
	}
	if auth.Traceparent != "" {
		req.Header.Set("traceparent", auth.Traceparent)
	}
	if auth.TenantID != "" {
		req.Header.Set("X-Tenant-Id", auth.TenantID)
	}
	if auth.GCID != "" {
		req.Header.Set("X-Chora-GCID", auth.GCID)
		// Legacy gcid header — chora-consumption requireContext reads this.
		req.Header.Set("gcid", auth.GCID)
	}
	mesh := servicemesh.MarshalToHeaders(servicemesh.MeshClaims{
		GCID:        auth.GCID,
		TenantID:    auth.TenantID,
		Roles:       auth.Roles,
		RoleSummary: auth.RoleSummary,
	})
	for k, vs := range mesh {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}

	resp, err := b.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, context.Canceled) {
			writeErrJSON(w, http.StatusGatewayTimeout, "GATEWAY_UPSTREAM_TIMEOUT", err.Error())
			return nil
		}
		var urlErr *url.Error
		if errors.As(err, &urlErr) && urlErr.Timeout() {
			writeErrJSON(w, http.StatusGatewayTimeout, "GATEWAY_UPSTREAM_TIMEOUT", err.Error())
			return nil
		}
		writeErrJSON(w, http.StatusBadGateway, "GATEWAY_UPSTREAM_ERROR", err.Error())
		return nil
	}
	defer resp.Body.Close()

	// 5xx → 502 JSON; do NOT pass through the upstream body since the
	// content-type may be HTML / text-plain (Envoy default error pages).
	if resp.StatusCode >= 500 {
		writeErrJSON(w, http.StatusBadGateway, "GATEWAY_UPSTREAM_5XX",
			fmt.Sprintf("upstream returned %d", resp.StatusCode))
		return nil
	}

	// 4xx pass-through verbatim — preserves the 402 InsufficientManaUpsell
	// envelope shape so the FE can render the universal upsell component.
	if resp.StatusCode >= 400 {
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		} else {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return nil
	}

	// 2xx SSE pass-through — forward Content-Type + the streaming control
	// headers, then copy frames byte-for-byte with periodic flushes so the
	// FE EventSource receives them in real time.
	for _, h := range []string{"Content-Type", "Cache-Control", "X-Accel-Buffering"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	}
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if w.Header().Get("X-Accel-Buffering") == "" {
		w.Header().Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				// FE disconnected — stop forwarding. Upstream context
				// cancellation will close the engine stream.
				return nil
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			break // io.EOF or stream-end
		}
	}
	return nil
}

// writeErrJSON writes a JSON error envelope on the wire. Used for error
// status codes returned BEFORE the SSE body has been started (otherwise
// the headers can no longer be modified).
func writeErrJSON(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	body := fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, code, message)
	_, _ = w.Write([]byte(body))
}

// -----------------------------------------------------------------------------
// Egg purchase routes (chora-tenancy)
// -----------------------------------------------------------------------------

// EggCatalog proxies GET /api/v1/companion-eggs/catalog → chora-tenancy.
// BFF-wrapped in {data: T} with FE reconciliations applied:
//   - items → skus
//   - per row priceCents → priceMicros (×10_000 unit conversion)
//
// per E2E-BE-FAM-GROWTH §3 contract spec.
func (b *Bridge) EggCatalog(ctx context.Context, auth AuthCtx) (Response, error) {
	u := b.cfg.TenancyURL + "/api/familiar-eggs/catalog" // tenancy upstream renames at W5
	cr := b.callWithGetRetry(ctx, http.MethodGet, u, nil, auth, b.cfg.PerCallTimeout)
	return classifyWithEnvelope(cr, catalogFixup), nil
}

// EggOdds proxies GET /api/v1/companion-eggs/:sku/odds.
// BFF-wrapped in {data: T} with FE reconciliation applied:
//   - sku → eggSku
//
// per E2E-BE-FAM-GROWTH §3 contract spec. IMDA D2 transparency: the
// numeric `odds[].probability` values pass through unchanged.
func (b *Bridge) EggOdds(ctx context.Context, auth AuthCtx, sku string) (Response, error) {
	u := b.cfg.TenancyURL + "/api/familiar-eggs/" + url.PathEscape(sku) + "/odds" // tenancy upstream renames at W5
	cr := b.callWithGetRetry(ctx, http.MethodGet, u, nil, auth, b.cfg.PerCallTimeout)
	return classifyWithEnvelope(cr, renameKey("sku", "eggSku")), nil
}

// EggCheckout proxies POST /api/v1/companion-eggs/checkout. NO retries.
//
// Contract reconciliation (Wave D(c) / Phyllis demo blocker, 2026-05-14):
//   - FE posts camelCase {sku|eggSku, returnUrl}; tenancy expects snake_case
//     {egg_sku, return_url} per chora-tenancy's egg checkout handler (tenancy upstream renames at W5).
//     The bridge translates camel→snake on outbound and aliases the FE's bare
//     {sku} → {egg_sku} so chora-web stays decoupled from the tenancy
//     contract while staying CHORA-domain-vocabulary clean.
//   - chora-tenancy reads `X-GCID` (NOT `X-Chora-GCID` and NOT the mesh-trust
//     `chora-gcid` header) on the checkout handler. The bridge stamps
//     `X-GCID` in addition to the canonical mesh-trust headers.
//
// Non-JSON bodies pass through verbatim (tenancy will 400 with `invalid_body`).
// BFF-wrapped in {data: T} on 2xx success per E2E-BE-FAM-GROWTH §3.
func (b *Bridge) EggCheckout(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	u := b.cfg.TenancyURL + "/api/familiar-eggs/checkout" // tenancy upstream renames at W5
	translated := translateCheckoutBody(body)
	return classifyWithEnvelope(b.callWithExtraHeaders(ctx, http.MethodPost, u, translated, auth,
		b.cfg.PerCallTimeout, map[string]string{"X-GCID": auth.GCID}), nil), nil
}

// AdminEggEntry proxies GET /api/v1/companion-eggs/admin/catalog/{sku} →
// chora-tenancy GET /api/admin/familiar-eggs/catalog/{sku} (D6).
//
// Deliberately NOT run through catalogFixup. That fixup serves the LEARNER
// catalogue: it renames items→skus and rewrites priceCents into priceMicros.
// This route feeds the H+ editor, which hands the entry straight back to the
// full-entry upsert, so it must be a faithful mirror of the row. An editor that
// read priceMicros would have to convert back before saving, and a unit slip
// there writes a wrong price onto a live SKU.
//
// Role-gated downstream: chora-tenancy checks the gateway-stamped
// x-mesh-user-roles header, which the mesh metadata below carries.
func (b *Bridge) AdminEggEntry(ctx context.Context, auth AuthCtx, sku string) (Response, error) {
	u := b.cfg.TenancyURL + "/api/admin/familiar-eggs/catalog/" + url.PathEscape(sku)
	cr := b.callWithGetRetry(ctx, http.MethodGet, u, nil, auth, b.cfg.PerCallTimeout)
	return classifyWithEnvelope(cr, nil), nil
}

// AdminEggUpsert proxies POST /api/v1/companion-eggs/admin/catalog →
// chora-tenancy POST /api/admin/familiar-eggs/catalog (D6). NO retries: it is a
// write, and the upsert is not idempotent across concurrent editors.
//
// The FE speaks camelCase and chora-tenancy's upsert reads snake_case, so the
// bridge owns the translation, exactly as it already does for checkout. A
// validation refusal (an unknown species, a distribution that does not total
// 100) passes through verbatim so the editor can say WHY it was rejected.
func (b *Bridge) AdminEggUpsert(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	u := b.cfg.TenancyURL + "/api/admin/familiar-eggs/catalog"
	cr := b.call(ctx, http.MethodPost, u, CamelToSnakeJSON(body), auth, b.cfg.PerCallTimeout)
	return classifyWithEnvelope(cr, nil), nil
}

// translateCheckoutBody normalises the FE checkout body into the snake_case
// shape chora-tenancy expects. Handles three input variants:
//
//	{"sku":"egg.x"}          → {"egg_sku":"egg.x"}
//	{"eggSku":"egg.x"}       → {"egg_sku":"egg.x"}
//	{"returnUrl":"https://"} → {"return_url":"https://"}
//
// Other camelCase keys are also converted via CamelToSnakeJSON. Non-JSON
// bodies pass through unchanged.
func translateCheckoutBody(in []byte) []byte {
	if len(in) == 0 {
		return in
	}
	var m map[string]any
	if err := json.Unmarshal(in, &m); err != nil {
		return in // non-JSON — let tenancy reject
	}
	// FE bare-sku alias: {sku} → {egg_sku}. Only when egg_sku/eggSku not already set.
	if _, hasEgg := m["eggSku"]; !hasEgg {
		if _, hasSnake := m["egg_sku"]; !hasSnake {
			if v, ok := m["sku"]; ok {
				m["eggSku"] = v
				delete(m, "sku")
			}
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return CamelToSnakeJSON(out)
}

// callWithGetRetry performs a GET with one retry on 5xx (per spec). POST writes
// MUST NOT use this — they call b.call directly.
func (b *Bridge) callWithGetRetry(ctx context.Context, method, urlStr string, body []byte, auth AuthCtx, timeout time.Duration) callResult {
	cr := b.call(ctx, method, urlStr, body, auth, timeout)
	if cr.err == nil && cr.status < 500 {
		return cr
	}
	// Transport error or 5xx — retry once.
	return b.call(ctx, method, urlStr, body, auth, timeout)
}

// -----------------------------------------------------------------------------
// snake_case → camelCase JSON re-marshaller
// -----------------------------------------------------------------------------

// SnakeToCamelJSON re-marshals a JSON object body, converting snake_case keys
// to camelCase recursively. Non-JSON inputs are returned unchanged so error
// envelopes and binary content pass through unaffected.
func SnakeToCamelJSON(in []byte) []byte {
	if len(in) == 0 {
		return in
	}
	var v any
	if err := json.Unmarshal(in, &v); err != nil {
		return in
	}
	converted := convertKeys(v)
	out, err := json.Marshal(converted)
	if err != nil {
		return in
	}
	return out
}

// convertKeys walks the decoded JSON value, snake→camel converting every
// object key. Nested objects and arrays are processed recursively.
func convertKeys(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[snakeToCamel(k)] = convertKeys(val)
		}
		return out
	case []any:
		for i, x := range t {
			t[i] = convertKeys(x)
		}
		return t
	default:
		return v
	}
}

// CamelToSnakeJSON re-marshals a JSON object body, converting camelCase keys
// to snake_case recursively. Non-JSON inputs are returned unchanged so error
// envelopes and binary content pass through unaffected.
//
// Inverse of SnakeToCamelJSON — used for outbound bodies where the FE
// posts camelCase but a downstream snake_case-only service (e.g.,
// chora-tenancy egg-checkout) needs snake_case.
func CamelToSnakeJSON(in []byte) []byte {
	if len(in) == 0 {
		return in
	}
	var v any
	if err := json.Unmarshal(in, &v); err != nil {
		return in
	}
	converted := convertKeysCamelToSnake(v)
	out, err := json.Marshal(converted)
	if err != nil {
		return in
	}
	return out
}

// convertKeysCamelToSnake walks the decoded JSON value, camel→snake converting
// every object key. Nested objects and arrays are processed recursively.
func convertKeysCamelToSnake(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[camelToSnake(k)] = convertKeysCamelToSnake(val)
		}
		return out
	case []any:
		for i, x := range t {
			t[i] = convertKeysCamelToSnake(x)
		}
		return t
	default:
		return v
	}
}

// camelToSnake converts fooBarBaz → foo_bar_baz. Already-snake keys pass
// through unchanged (no uppercase = no work). Edge cases: leading
// uppercase becomes underscore-less lowercase (FooBar → foo_bar).
func camelToSnake(s string) string {
	if s == "" {
		return s
	}
	hasUpper := false
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			hasUpper = true
			break
		}
	}
	if !hasUpper {
		return s
	}
	out := make([]rune, 0, len(s)+4)
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				out = append(out, '_')
			}
			out = append(out, unicode.ToLower(r))
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

// snakeToCamel converts foo_bar_baz → fooBarBaz. Already-camel keys pass through
// unchanged (no underscore = no work). Edge cases: leading/trailing
// underscores, double underscores, and ALL-CAPS segments are preserved.
func snakeToCamel(s string) string {
	if s == "" {
		return s
	}
	hasUnderscore := false
	for i := 0; i < len(s); i++ {
		if s[i] == '_' {
			hasUnderscore = true
			break
		}
	}
	if !hasUnderscore {
		return s
	}
	out := make([]rune, 0, len(s))
	upperNext := false
	first := true
	for _, r := range s {
		if r == '_' {
			if first {
				out = append(out, r)
				continue
			}
			upperNext = true
			continue
		}
		if upperNext {
			out = append(out, unicode.ToUpper(r))
			upperNext = false
		} else {
			out = append(out, r)
		}
		first = false
	}
	return string(out)
}
