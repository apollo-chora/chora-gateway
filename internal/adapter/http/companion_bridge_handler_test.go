// companion_bridge_handler_test.go — HTTP route binding tests for the
// CompanionBridge handlers (PROD-D of ADR-149 Companion Growth rollout).
package httpadapter_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/companionbridge"
)

// newConsumptionStub stands up a consumption fake at /v1/me/companions/...
func newConsumptionStub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/me/companions" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"companions":[{"companion_id":"f1","display_name":"Pip"}]}`))
		// Skills routes MUST be matched BEFORE the bare-instance GET below so
		// the longer `/{id}/skills` suffix wins over the `/{id}` prefix.
		case strings.HasSuffix(r.URL.Path, "/skills") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"companion_id":"f1","skill_grants":["focus_burst"],"skill_slots_unlocked":3,"evolution_tier":"adept"}`))
		case strings.HasSuffix(r.URL.Path, "/skills") && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			// The "full" sku triggers the 409 SKILL_SLOTS_FULL contract per
			// the consumption skill-grant handler. writeError on consumption
			// emits the raw {code, message} (no `error` wrapper).
			if strings.Contains(string(body), "full") {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"code":"SKILL_SLOTS_FULL","message":"all skill slots are occupied"}`))
				return
			}
			// Echo body so the test can assert camel→snake on the request.
			_, _ = w.Write([]byte(`{"companion_id":"f1","skill_grants":["focus_burst","echoed_skill"],"skill_slots_unlocked":3,"evolution_tier":"adept","echoed":` + string(body) + `}`))
		// Bare-instance profile GET — falls through ONLY when no further
		// subpath segment follows the companion id. Nested objects exercise
		// the recursive snake→camel re-marshal.
		case r.URL.Path == "/v1/me/companions/f1" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"companion_id":"f1","display_name":"Pip","growth_stage":2,"growth_state":{"current_xp":120,"next_threshold":200},"cosmetic":{"primary_color":"#abc"},"roster_context":{"slot_index":0},"memory_summary":"likes algebra"}`))
		case strings.HasSuffix(r.URL.Path, "/growth") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"companion_id":"f1","growth_stage":2,"stage_name":"fledgling"}`))
		case strings.HasSuffix(r.URL.Path, "/hatch") && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			// Echo body so the test can assert pass-through.
			_, _ = w.Write([]byte(`{"state":{"companion_id":"f1","growth_stage":1},"species":"dragon","shiny_variant":false,"rarity":"common","rolled_probability":50.0,"echoed":` + string(body) + `}`))
		// CHO-2229 — the reveal leaf (roll persists ahead of naming). POST, no body.
		case strings.HasSuffix(r.URL.Path, "/reveal") && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"state":{"companion_id":"f1","growth_stage":0,"species":"fox","revealed_at":"2026-07-17T03:00:00Z"},"species":"fox","shiny_variant":true,"rarity":"uncommon","rolled_probability":12.5,"revealed_at":"2026-07-17T03:00:00Z","already_revealed":false}`))
		case strings.HasSuffix(r.URL.Path, "/resonance") && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"revealed":{"atom_id":"a1"},"state":{"companion_id":"f1"}}`))
		case strings.HasSuffix(r.URL.Path, "/source-revelation") && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"preview_llm_tier":"pro","window_expires_at":"2026-05-14T00:00:00Z","window_duration_seconds":86400,"state":{"companion_id":"f1"}}`))
		// CHO-2033 SP3 — retire (soft-release) leaf. No request body.
		case strings.HasSuffix(r.URL.Path, "/retire") && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"companion_id":"f1","retired":true}`))
		case strings.HasSuffix(r.URL.Path, "/growth-events") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"events":[],"next_page_token":""}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTenancyStub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/familiar-eggs/catalog" && r.Method == http.MethodGet: // tenancy upstream renames at W5
			// Real chora-tenancy emits `items: [...]` with `price_cents`
			// per chora-tenancy's egg handlers (tenancy upstream renames at W5).
			// Bridge reconciles items→skus + priceCents→priceMicros per
			// E2E-BE-FAM-GROWTH §3.
			_, _ = w.Write([]byte(`{"items":[{"sku":"common-001","price_cents":500}],"total":1}`))
		case strings.HasSuffix(r.URL.Path, "/odds") && r.Method == http.MethodGet:
			// Real tenancy emits `sku` (not `egg_sku`); bridge reconciles
			// sku→eggSku to match the FE PreviewEggOddsResponse model.
			_, _ = w.Write([]byte(`{"sku":"golden-001","odds":[],"total_weight":0,"distribution_updated_at":"2026-05-13T00:00:00Z"}`))
		case r.URL.Path == "/api/familiar-eggs/checkout" && r.Method == http.MethodPost: // tenancy upstream renames at W5
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"stripe_checkout_url":"https://stripe/abc","purchase_id":"p1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newBridgeHandler(t *testing.T, cons, ten *httptest.Server) http.Handler {
	t.Helper()
	cfg := companionbridge.Config{
		ConsumptionURL: cons.URL,
		TenancyURL:     ten.URL,
		PerCallTimeout: 1 * time.Second,
		HatchTimeout:   2 * time.Second,
	}
	b := companionbridge.New(cfg)
	return httpadapter.NewCompanionBridgeMux(b)
}

func doReq(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequestWithContext(context.Background(), method, path, nil)
	}
	r.Header.Set("Authorization", "Bearer testtoken")
	r.Header.Set("X-Tenant-Id", "tenant-001")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	return m
}

// decodeData unwraps the BFF `{ data: T }` envelope. Returns nil when
// the envelope is missing. Per E2E-BE-FAM-GROWTH §3 (2026-05-16) the
// chora-gateway companionbridge wraps every 2xx response for the 8 FE
// endpoints (growth subpaths + egg marketplace) in this envelope so
// the chora-web CompanionGrowthService.unwrap() helper can read it.
func decodeData(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	env := decodeBody(t, w)
	d, ok := env["data"].(map[string]any)
	if !ok {
		t.Fatalf("envelope `data` key missing or not object; body=%s", w.Body.String())
	}
	return d
}

// -----------------------------------------------------------------------------
// Companion growth routes
// -----------------------------------------------------------------------------

func TestCompanionBridge_ListCompanions(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	w := doReq(t, h, http.MethodGet, "/api/v1/me/companions", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	m := decodeBody(t, w)
	if _, ok := m["companions"]; !ok {
		t.Errorf("companions missing: %v", m)
	}
}

func TestCompanionBridge_GetGrowth_CamelCaseInResponse(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	w := doReq(t, h, http.MethodGet, "/api/v1/me/companions/f1/growth", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	// E2E-BE-FAM-GROWTH §3 — payload is wrapped in {data: T}.
	m := decodeData(t, w)
	if _, ok := m["growthStage"]; !ok {
		t.Errorf("data.growthStage missing: %v", m)
	}
	if _, ok := m["growth_stage"]; ok {
		t.Errorf("snake_case leaked: %v", m)
	}
}

// -----------------------------------------------------------------------------
// Bare-instance profile GET — GET /api/v1/me/companions/{id}
// -----------------------------------------------------------------------------

func TestCompanionBridge_GetInstance_ProfileUnwrappedCamelCase(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	w := doReq(t, h, http.MethodGet, "/api/v1/me/companions/f1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// The bare instance GET returns the unwrapped object (NOT {data: T}),
	// mirroring ListCompanions. The top-level object IS the profile.
	m := decodeBody(t, w)
	if _, wrapped := m["data"]; wrapped {
		t.Fatalf("profile must be unwrapped (no {data:T} envelope); body=%s", w.Body.String())
	}
	if m["companionId"] != "f1" {
		t.Errorf("companionId=%v want f1", m["companionId"])
	}
	if m["displayName"] != "Pip" {
		t.Errorf("displayName=%v want Pip", m["displayName"])
	}
	if _, ok := m["growth_stage"]; ok {
		t.Errorf("snake_case leaked at top level: %v", m)
	}
	// Verify SnakeToCamelJSON recurses into the nested objects.
	gs, ok := m["growthState"].(map[string]any)
	if !ok {
		t.Fatalf("growthState missing or not object: %v", m)
	}
	if _, ok := gs["currentXp"]; !ok {
		t.Errorf("nested growthState.currentXp not camelized: %v", gs)
	}
	if _, ok := gs["next_threshold"]; ok {
		t.Errorf("nested snake_case leaked in growthState: %v", gs)
	}
	cos, ok := m["cosmetic"].(map[string]any)
	if !ok {
		t.Fatalf("cosmetic missing or not object: %v", m)
	}
	if _, ok := cos["primaryColor"]; !ok {
		t.Errorf("nested cosmetic.primaryColor not camelized: %v", cos)
	}
	rc, ok := m["rosterContext"].(map[string]any)
	if !ok {
		t.Fatalf("rosterContext missing or not object: %v", m)
	}
	if _, ok := rc["slotIndex"]; !ok {
		t.Errorf("nested rosterContext.slotIndex not camelized: %v", rc)
	}
	if m["memorySummary"] != "likes algebra" {
		t.Errorf("memorySummary=%v want \"likes algebra\"", m["memorySummary"])
	}
}

func TestCompanionBridge_GetInstance_DownstreamPathAndMethod(t *testing.T) {
	var gotPath, gotMethod string
	cons := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"companion_id":"abc"}`))
	}))
	t.Cleanup(cons.Close)
	cfg := companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second}
	h := httpadapter.NewCompanionBridgeMux(companionbridge.New(cfg))

	w := doReq(t, h, http.MethodGet, "/api/v1/me/companions/abc", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/me/companions/abc" {
		t.Errorf("downstream path=%q want /v1/me/companions/abc", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("downstream method=%q want GET", gotMethod)
	}
}

func TestCompanionBridge_GetInstance_WrongMethod_405(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	w := doReq(t, h, http.MethodDelete, "/api/v1/me/companions/f1", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status=%d want 405", w.Code)
	}
}

// The bare-instance GET must NOT shadow the existing subpath routes — the
// longer suffixed paths win.
func TestCompanionBridge_GetInstance_DoesNotShadowGrowth(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	w := doReq(t, h, http.MethodGet, "/api/v1/me/companions/f1/growth", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// /growth is still {data: T}-wrapped, proving the bare GET did not capture it.
	m := decodeData(t, w)
	if _, ok := m["growthStage"]; !ok {
		t.Errorf("growth subpath shadowed by bare GET: %v", m)
	}
}

// -----------------------------------------------------------------------------
// Skill grants — GET + POST /api/v1/me/companions/{id}/skills
// -----------------------------------------------------------------------------

func TestCompanionBridge_ListSkills_CamelCase(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	w := doReq(t, h, http.MethodGet, "/api/v1/me/companions/f1/skills", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	m := decodeBody(t, w)
	if _, ok := m["skillGrants"]; !ok {
		t.Errorf("skillGrants missing: %v", m)
	}
	if _, ok := m["skillSlotsUnlocked"]; !ok {
		t.Errorf("skillSlotsUnlocked missing (snake→camel skipped): %v", m)
	}
	if _, ok := m["skill_grants"]; ok {
		t.Errorf("snake_case leaked: %v", m)
	}
	if m["companionId"] != "f1" {
		t.Errorf("companionId=%v want f1", m["companionId"])
	}
}

func TestCompanionBridge_ListSkills_DownstreamPathAndMethod(t *testing.T) {
	var gotPath, gotMethod string
	cons := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"companion_id":"abc","skill_grants":[]}`))
	}))
	t.Cleanup(cons.Close)
	cfg := companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second}
	h := httpadapter.NewCompanionBridgeMux(companionbridge.New(cfg))

	w := doReq(t, h, http.MethodGet, "/api/v1/me/companions/abc/skills", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/me/companions/abc/skills" {
		t.Errorf("downstream path=%q want /v1/me/companions/abc/skills", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("downstream method=%q want GET", gotMethod)
	}
}

func TestCompanionBridge_GrantSkill_RequestCamelToSnake_ResponseCamel(t *testing.T) {
	var gotBody, gotPath, gotMethod string
	cons := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"companion_id":"f1","skill_grants":["focus_burst"],"skill_slots_unlocked":2,"evolution_tier":"adept"}`))
	}))
	t.Cleanup(cons.Close)
	cfg := companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second}
	h := httpadapter.NewCompanionBridgeMux(companionbridge.New(cfg))

	w := doReq(t, h, http.MethodPost, "/api/v1/me/companions/f1/skills", `{"skillId":"focus_burst"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotMethod != http.MethodPost {
		t.Errorf("downstream method=%q want POST", gotMethod)
	}
	if gotPath != "/v1/me/companions/f1/skills" {
		t.Errorf("downstream path=%q want /v1/me/companions/f1/skills", gotPath)
	}
	// Request body camelCase→snake_case: skillId → skill_id.
	if !strings.Contains(gotBody, `"skill_id"`) {
		t.Errorf("request body not camel→snake translated: %s", gotBody)
	}
	if strings.Contains(gotBody, `"skillId"`) {
		t.Errorf("camelCase key leaked downstream: %s", gotBody)
	}
	// Response snake_case→camelCase.
	m := decodeBody(t, w)
	if _, ok := m["skillGrants"]; !ok {
		t.Errorf("response skillGrants missing (snake→camel skipped): %v", m)
	}
	if _, ok := m["evolutionTier"]; !ok {
		t.Errorf("response evolutionTier missing: %v", m)
	}
}

func TestCompanionBridge_GrantSkill_Conflict409PassedThrough(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	w := doReq(t, h, http.MethodPost, "/api/v1/me/companions/f1/skills", `{"skillId":"full_slot_skill"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d want 409 (SKILL_SLOTS_FULL must pass through)", w.Code)
	}
	m := decodeBody(t, w)
	// The 409 conflict body passes through via classify (NOT envelope-wrapped),
	// so the FE reads the raw {code, message} shape downstream emitted.
	if m["code"] != "SKILL_SLOTS_FULL" {
		t.Errorf("conflict code=%v want SKILL_SLOTS_FULL; body=%s", m["code"], w.Body.String())
	}
}

func TestCompanionBridge_Skills_WrongMethod_405(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	w := doReq(t, h, http.MethodDelete, "/api/v1/me/companions/f1/skills", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status=%d want 405", w.Code)
	}
}

func TestCompanionBridge_HatchEgg_BodyPassedThrough(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	w := doReq(t, h, http.MethodPost, "/api/v1/me/companions/f1/hatch", `{"displayName":"Spark"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// E2E-BE-FAM-GROWTH §3 — payload is wrapped in {data: T}.
	m := decodeData(t, w)
	if m["species"] != "dragon" {
		t.Errorf("data.species=%v", m["species"])
	}
}

func TestCompanionBridge_Reveal_WrapsEnvelope(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	w := doReq(t, h, http.MethodPost, "/api/v1/me/companions/f1/reveal", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// E2E-BE-FAM-GROWTH §3 — payload is wrapped in {data: T}, snake→camel.
	m := decodeData(t, w)
	if m["species"] != "fox" {
		t.Errorf("data.species=%v", m["species"])
	}
	if _, ok := m["revealedAt"]; !ok {
		t.Errorf("data.revealedAt missing (camelisation): %v", m)
	}
	if _, ok := m["alreadyRevealed"]; !ok {
		t.Errorf("data.alreadyRevealed missing: %v", m)
	}
}

func TestCompanionBridge_Resonance(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	w := doReq(t, h, http.MethodPost, "/api/v1/me/companions/f1/resonance", `{"conceptId":"c1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestCompanionBridge_SourceRevelation(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	w := doReq(t, h, http.MethodPost, "/api/v1/me/companions/f1/source-revelation", `{"manaTier":"premium"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	// E2E-BE-FAM-GROWTH §3 — payload is wrapped in {data: T}.
	m := decodeData(t, w)
	if _, ok := m["previewLlmTier"]; !ok {
		t.Errorf("data.previewLlmTier missing: %v", m)
	}
}

// -----------------------------------------------------------------------------
// Retire (soft-release) — POST /api/v1/me/companions/{id}/retire (CHO-2033 SP3)
// -----------------------------------------------------------------------------

// TestCompanionBridge_Retire_HappyPath mirrors the StartProofingTest /
// skills-grant convention (unwrapped, snake_case→camelCase re-marshal) —
// NOT the older PROD-D {data:T} envelope (hatch/resonance/source-revelation)
// — because retire is a brand-new (2026-07-05) leaf mirroring the sibling
// proofing-test action added the same week.
func TestCompanionBridge_Retire_HappyPath(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	w := doReq(t, h, http.MethodPost, "/api/v1/me/companions/f1/retire", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	m := decodeBody(t, w)
	if _, wrapped := m["data"]; wrapped {
		t.Fatalf("retire must be unwrapped (no {data:T} envelope); body=%s", w.Body.String())
	}
	if m["companionId"] != "f1" {
		t.Errorf("companionId=%v want f1", m["companionId"])
	}
	if m["retired"] != true {
		t.Errorf("retired=%v want true", m["retired"])
	}
	if _, ok := m["companion_id"]; ok {
		t.Errorf("snake_case leaked: %v", m)
	}
}

// TestCompanionBridge_Retire_DownstreamPathAndMethod asserts the exact
// downstream call shape: POST /v1/me/companions/{id}/retire, no request body.
func TestCompanionBridge_Retire_DownstreamPathAndMethod(t *testing.T) {
	var gotPath, gotMethod string
	var gotBodyLen int
	cons := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		gotBodyLen = len(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"companion_id":"abc","retired":true}`))
	}))
	t.Cleanup(cons.Close)
	cfg := companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second}
	h := httpadapter.NewCompanionBridgeMux(companionbridge.New(cfg))

	w := doReq(t, h, http.MethodPost, "/api/v1/me/companions/abc/retire", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/me/companions/abc/retire" {
		t.Errorf("downstream path=%q want /v1/me/companions/abc/retire", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("downstream method=%q want POST", gotMethod)
	}
	if gotBodyLen != 0 {
		t.Errorf("downstream body len=%d want 0 (retire takes no request body)", gotBodyLen)
	}
}

// TestCompanionBridge_Retire_UnknownCompanion404PassesThrough proves the
// consumption 404 (double-retire / foreign id, no-leak per ddd-enforcement)
// passes through verbatim rather than being swallowed as a 502. Asserts the
// downstream stub was actually reached (via the echoed code/message) so this
// cannot false-pass on an unwired route's own 404 (which shares the status
// code but never touches the stub).
func TestCompanionBridge_Retire_UnknownCompanion404PassesThrough(t *testing.T) {
	reached := false
	cons := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"NOT_FOUND","message":"companion not found"}`))
	}))
	t.Cleanup(cons.Close)
	cfg := companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second}
	h := httpadapter.NewCompanionBridgeMux(companionbridge.New(cfg))

	w := doReq(t, h, http.MethodPost, "/api/v1/me/companions/ghost/retire", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 (pass-through)", w.Code)
	}
	if !reached {
		t.Fatalf("downstream stub was never reached — route is not wired (would false-pass on the mux's own 404)")
	}
	m := decodeBody(t, w)
	if m["code"] != "NOT_FOUND" {
		t.Errorf("body not passed through from downstream: %v", m)
	}
}

func TestCompanionBridge_GrowthEvents(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	w := doReq(t, h, http.MethodGet, "/api/v1/me/companions/f1/growth-events?pageSize=10", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	// E2E-BE-FAM-GROWTH §3 — payload is wrapped in {data: T}.
	m := decodeData(t, w)
	if _, ok := m["nextPageToken"]; !ok {
		t.Errorf("data.nextPageToken missing: %v", m)
	}
}

// -----------------------------------------------------------------------------
// Egg purchase routes
// -----------------------------------------------------------------------------

func TestCompanionBridge_EggCatalog(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	w := doReq(t, h, http.MethodGet, "/api/v1/companion-eggs/catalog", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// E2E-BE-FAM-GROWTH §3 — payload is wrapped in {data: T}, items→skus,
	// per-row priceCents→priceMicros (×10_000).
	m := decodeData(t, w)
	skus, ok := m["skus"].([]any)
	if !ok || len(skus) == 0 {
		t.Fatalf("data.skus missing or empty: %v", m)
	}
	first := skus[0].(map[string]any)
	if pm, _ := first["priceMicros"].(float64); pm != 5000000 {
		t.Errorf("data.skus[0].priceMicros = %v want 5000000", first["priceMicros"])
	}
}

func TestCompanionBridge_EggOdds(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	w := doReq(t, h, http.MethodGet, "/api/v1/companion-eggs/golden-001/odds", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	// E2E-BE-FAM-GROWTH §3 — payload is wrapped in {data: T}, sku→eggSku.
	m := decodeData(t, w)
	if _, ok := m["eggSku"]; !ok {
		t.Errorf("data.eggSku missing (sku→eggSku reconciliation skipped): %v", m)
	}
	if _, ok := m["distributionUpdatedAt"]; !ok {
		t.Errorf("data.distributionUpdatedAt missing: %v", m)
	}
}

func TestCompanionBridge_EggCheckout(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	w := doReq(t, h, http.MethodPost, "/api/v1/companion-eggs/checkout", `{"eggSku":"golden-001"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// E2E-BE-FAM-GROWTH §3 — payload is wrapped in {data: T}.
	m := decodeData(t, w)
	if _, ok := m["stripeCheckoutUrl"]; !ok {
		t.Errorf("data.stripeCheckoutUrl missing: %v", m)
	}
	if _, ok := m["purchaseId"]; !ok {
		t.Errorf("data.purchaseId missing: %v", m)
	}
}

// -----------------------------------------------------------------------------
// Method enforcement + path edge cases
// -----------------------------------------------------------------------------

func TestCompanionBridge_WrongMethod_405(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	w := doReq(t, h, http.MethodDelete, "/api/v1/me/companions", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status=%d want 405", w.Code)
	}
}

func TestCompanionBridge_UnknownPath_404(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	w := doReq(t, h, http.MethodGet, "/api/v1/me/companions/f1/nonsense", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404", w.Code)
	}
}

func TestCompanionBridge_MissingCompanionID_404(t *testing.T) {
	// Empty companion id (trailing-slash-only path /api/v1/me/companions/) must
	// 404 — there is no instance id segment. NB: this is distinct from a single
	// id segment (/api/v1/me/companions/{id}) which is now the valid bare
	// instance profile GET (see TestCompanionBridge_GetInstance_*). A
	// double-slash subpath (//growth) likewise has an empty id and 404s.
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	w := doReq(t, h, http.MethodGet, "/api/v1/me/companions/", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404 (empty companion id)", w.Code)
	}
}

// -----------------------------------------------------------------------------
// WithCompanionBridge composition: passthrough when nil
// -----------------------------------------------------------------------------

func TestWithCompanionBridge_NilPassThrough(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := httpadapter.WithCompanionBridge(base, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me/companions", nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTeapot {
		t.Errorf("status=%d; want passthrough to base", w.Code)
	}
}

// -----------------------------------------------------------------------------
// Method enforcement per route (405 coverage)
// -----------------------------------------------------------------------------

func TestCompanionBridge_MethodNotAllowed_Routes(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	cases := []struct {
		method, path string
	}{
		{http.MethodPost, "/api/v1/me/companions/f1/growth"},
		{http.MethodGet, "/api/v1/me/companions/f1/hatch"},
		{http.MethodGet, "/api/v1/me/companions/f1/reveal"},
		{http.MethodGet, "/api/v1/me/companions/f1/resonance"},
		{http.MethodGet, "/api/v1/me/companions/f1/source-revelation"},
		{http.MethodGet, "/api/v1/me/companions/f1/retire"},
		{http.MethodPost, "/api/v1/me/companions/f1/growth-events"},
		{http.MethodPost, "/api/v1/companion-eggs/catalog"},
		{http.MethodPost, "/api/v1/companion-eggs/golden-001/odds"},
		{http.MethodGet, "/api/v1/companion-eggs/checkout"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := doReq(t, h, tc.method, tc.path, "")
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("status=%d want 405", w.Code)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Egg subpath validation — only `/odds` is supported
// -----------------------------------------------------------------------------

func TestCompanionBridge_EggSubpath_UnknownLeaf_404(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	w := doReq(t, h, http.MethodGet, "/api/v1/companion-eggs/golden-001/nonsense", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404", w.Code)
	}
}

// -----------------------------------------------------------------------------
// Pagination param: snake_case fallback works for legacy callers
// -----------------------------------------------------------------------------

func TestCompanionBridge_GrowthEvents_SnakeCaseQueryFallback(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	w := doReq(t, h, http.MethodGet,
		"/api/v1/me/companions/f1/growth-events?page_token=abc&page_size=5", "")
	if w.Code != http.StatusOK {
		t.Errorf("status=%d want 200", w.Code)
	}
}

func TestCompanionBridge_GrowthEvents_InvalidPageSize_Ignored(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	w := doReq(t, h, http.MethodGet, "/api/v1/me/companions/f1/growth-events?pageSize=nope", "")
	if w.Code != http.StatusOK {
		t.Errorf("status=%d want 200 (invalid pageSize ignored)", w.Code)
	}
}

// -----------------------------------------------------------------------------
// MeshClaims path: gcid is sourced from validated JWT mesh-claims
// -----------------------------------------------------------------------------

func TestCompanionBridge_StampsGCIDFromMeshClaims(t *testing.T) {
	var gotGCID, gotTenant string
	cons := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotGCID = r.Header.Get("X-Chora-GCID")
		gotTenant = r.Header.Get("X-Tenant-Id")
		_, _ = w.Write([]byte(`{"companions":[]}`))
	}))
	t.Cleanup(cons.Close)

	cfg := companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second}
	b := companionbridge.New(cfg)
	h := httpadapter.NewCompanionBridgeMux(b)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/me/companions", nil)
	r.Header.Set("Authorization", "Bearer t")
	// Stamp mesh claims directly on context (simulates RequireChoraSessionJWT having run).
	ctx := httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-test", "tenant-test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r.WithContext(ctx))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotGCID != "gcid-test" {
		t.Errorf("X-Chora-GCID = %q; want gcid-test", gotGCID)
	}
	if gotTenant != "tenant-test" {
		t.Errorf("X-Tenant-Id = %q; want tenant-test (mesh override)", gotTenant)
	}
}

func TestWithCompanionBridge_NonBridgePath_Passthrough(t *testing.T) {
	baseCalled := false
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		baseCalled = true
		w.WriteHeader(http.StatusOK)
	})
	cfg := companionbridge.Config{
		ConsumptionURL: "http://example",
		TenancyURL:     "http://example",
		PerCallTimeout: time.Second,
	}
	h := httpadapter.WithCompanionBridge(base, companionbridge.New(cfg))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	h.ServeHTTP(w, r)
	if !baseCalled {
		t.Errorf("expected base handler called for non-bridge path")
	}
}
