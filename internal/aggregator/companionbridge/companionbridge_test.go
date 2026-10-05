// Package companionbridge_test exercises the Companion Growth / Egg purchase
// proxy aggregator (PROD-D of ADR-149 Companion Growth rollout).
//
// The bridge proxies 9 BFF routes through to chora-consumption (6 growth
// routes) and chora-tenancy (3 egg-purchase routes), stamps mesh-trust
// headers (X-Tenant-Id, X-Chora-GCID, chora-gcid/chora-tenant-id) on every
// outbound call, applies per-route timeouts, surfaces 502 on downstream 5xx,
// 504 on timeout, and re-marshals downstream snake_case bodies to camelCase
// for the FE per the PROD-D FE growth handoff (2026-05-13) §2.1.
package companionbridge_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/companionbridge"
)

// stubUpstream is a minimal recording test server.
type stubUpstream struct {
	*httptest.Server
	calls atomic.Int64
	// mu guards every last* field below. Two handler goroutines can write
	// them for one test; see the comment in newStub.
	mu           sync.Mutex
	lastPath     string
	lastRawQuery string // these bridges REBUILD the upstream URL, so a query param
	// is dropped unless threaded through by hand; without
	// capturing it, a test cannot see that happen.
	lastMethod     string
	lastAuth       string
	lastTenant     string
	lastGCID       string
	lastLegacyGCID string // bare lowercase `gcid` — chora-consumption requireContext reads this
	lastTP         string
	lastBody       []byte
}

func newStub(t *testing.T, h http.HandlerFunc) *stubUpstream {
	t.Helper()
	s := &stubUpstream{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		// The capture writes are held under mu because a single test can put
		// TWO handler goroutines on this stub with no ordering between them:
		// a per-call timeout shorter than the handler's sleep makes the client
		// give up and issue another request while the first handler is still
		// in flight. httptest runs each on its own goroutine, so the writes
		// below raced (9 reports, TestGetCompanionGrowth_DownstreamTimeout_504
		// FAILED under -race). The lock is released BEFORE h runs: holding it
		// across the handler body would serialise handlers and silently defeat
		// the timeout tests, which need the sleep to overlap the next request.
		s.mu.Lock()
		s.lastPath = r.URL.Path
		s.lastRawQuery = r.URL.RawQuery
		s.lastMethod = r.Method
		s.lastAuth = r.Header.Get("Authorization")
		s.lastTenant = r.Header.Get("X-Tenant-Id")
		s.lastGCID = r.Header.Get("X-Chora-GCID")
		if s.lastGCID == "" {
			// Mesh-claims path uses chora-gcid (lowercase) per servicemesh contract.
			s.lastGCID = r.Header.Get("chora-gcid")
		}
		// Bare lowercase `gcid` — chora-consumption's `requireContext` in
		// services/chora-consumption/internal/adapter/http/router.go reads
		// this exact header. Captured separately so tests can assert it
		// independently of the X-Chora-GCID / chora-gcid mesh path.
		s.lastLegacyGCID = r.Header.Get("gcid")
		s.lastTP = r.Header.Get("traceparent")
		s.lastBody, _ = io.ReadAll(r.Body)
		s.mu.Unlock()
		_ = r.Body.Close()
		h(w, r)
	}))
	t.Cleanup(s.Server.Close)
	return s
}

func decodeJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode: %v body=%s", err, string(body))
	}
	return m
}

func basicAuth() companionbridge.AuthCtx {
	return companionbridge.AuthCtx{
		Bearer:      "fb-tok",
		Traceparent: "00-aaa-bbb-01",
		TenantID:    "tenant-001",
		GCID:        "gcid-001",
	}
}

// -----------------------------------------------------------------------------
// ListCompanions — GET /v1/me/companions
// -----------------------------------------------------------------------------

func TestListCompanions_HappyPath_CamelCaseTranslated(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"companions":[{"companion_id":"f1","display_name":"Pip","growth_stage":2}]}`))
	})
	b := companionbridge.New(companionbridge.Config{
		ConsumptionURL: cons.URL,
		PerCallTimeout: 1 * time.Second,
	})
	resp, _ := b.ListCompanions(context.Background(), basicAuth())

	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.Status)
	}
	if cons.lastPath != "/v1/me/companions" {
		t.Errorf("path=%s want /v1/me/companions", cons.lastPath)
	}
	if cons.lastMethod != http.MethodGet {
		t.Errorf("method=%s want GET", cons.lastMethod)
	}
	if cons.lastTenant != "tenant-001" {
		t.Errorf("X-Tenant-Id=%s", cons.lastTenant)
	}
	if cons.lastTP != "00-aaa-bbb-01" {
		t.Errorf("traceparent=%s", cons.lastTP)
	}
	m := decodeJSON(t, resp.Body)
	fams, ok := m["companions"].([]any)
	if !ok || len(fams) == 0 {
		t.Fatalf("missing companions: %v", m)
	}
	f0 := fams[0].(map[string]any)
	if _, has := f0["companionId"]; !has {
		t.Errorf("expected camelCase companionId; got %v", f0)
	}
	if _, has := f0["display_name"]; has {
		t.Errorf("snake_case display_name leaked: %v", f0)
	}
	if _, has := f0["displayName"]; !has {
		t.Errorf("expected displayName; got %v", f0)
	}
}

// -----------------------------------------------------------------------------
// GetCompanionGrowth — GET /v1/me/companions/:id/growth
// -----------------------------------------------------------------------------

func TestGetCompanionGrowth_HappyPath_RewritesAllSnakeKeys(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"companion_id":"f1",
			"growth_stage":2,
			"stage_name":"fledgling",
			"shiny_variant":false,
			"exp_current":10,
			"exp_next_threshold":100,
			"exp_cumulative":10,
			"effective_llm_tier":"flash",
			"effective_max_output_tokens":2048,
			"unlocked_tools":["tool_a"],
			"visible_kg_neighbors":[],
			"aha_moment_consumed":false,
			"aha_moment_active_until":null,
			"hatched_at":null,
			"last_stage_up_at":null
		}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})
	resp, _ := b.GetCompanionGrowth(context.Background(), basicAuth(), "f1")
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d", resp.Status)
	}
	if cons.lastPath != "/v1/me/companions/f1/growth" {
		t.Errorf("path=%s", cons.lastPath)
	}
	// E2E-BE-FAM-GROWTH §3 — payload is wrapped in {data: T}.
	m := dataMap(t, resp.Body)
	for _, want := range []string{
		"companionId", "growthStage", "stageName", "shinyVariant",
		"expCurrent", "expNextThreshold", "expCumulative",
		"effectiveLlmTier", "effectiveMaxOutputTokens", "unlockedTools",
		"visibleKgNeighbors", "ahaMomentConsumed",
	} {
		if _, ok := m[want]; !ok {
			t.Errorf("missing camelCase key %s; got %v", want, m)
		}
	}
	for _, bad := range []string{"companion_id", "growth_stage", "stage_name"} {
		if _, ok := m[bad]; ok {
			t.Errorf("snake_case %s leaked", bad)
		}
	}
}

func TestGetCompanionGrowth_404_PassThrough(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"COMPANION_NOT_FOUND"}}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})
	resp, _ := b.GetCompanionGrowth(context.Background(), basicAuth(), "missing")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status=%d want 404", resp.Status)
	}
}

func TestGetCompanionGrowth_DownstreamTimeout_504(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: 50 * time.Millisecond})
	resp, _ := b.GetCompanionGrowth(context.Background(), basicAuth(), "f1")
	if resp.Status != http.StatusGatewayTimeout {
		t.Errorf("status=%d want 504", resp.Status)
	}
}

func TestGetCompanionGrowth_Downstream5xx_502(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})
	resp, _ := b.GetCompanionGrowth(context.Background(), basicAuth(), "f1")
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status=%d want 502", resp.Status)
	}
}

// -----------------------------------------------------------------------------
// HatchEgg — POST /v1/me/companions/:id/hatch
// -----------------------------------------------------------------------------

func TestHatchEgg_HappyPath_PassesBody_TranslatesResponse(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"state":{"companion_id":"f1","growth_stage":1,"stage_name":"baby","shiny_variant":false,"exp_current":0,"exp_next_threshold":50,"exp_cumulative":0,"effective_llm_tier":"flash-lite","effective_max_output_tokens":1024,"unlocked_tools":[],"visible_kg_neighbors":[],"aha_moment_consumed":false},
			"species":"dragon",
			"shiny_variant":true,
			"rarity":"rare",
			"rolled_probability":3.5
		}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})
	reqBody := []byte(`{"displayName":"Spark","tone":"playful","learnerPersona":"explorer","resonantAtomId":"a-1"}`)
	resp, _ := b.HatchEgg(context.Background(), basicAuth(), "f1", reqBody)
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Status, resp.Body)
	}
	if cons.lastPath != "/v1/me/companions/f1/hatch" {
		t.Errorf("path=%s", cons.lastPath)
	}
	if cons.lastMethod != http.MethodPost {
		t.Errorf("method=%s", cons.lastMethod)
	}
	if len(cons.lastBody) == 0 {
		t.Error("downstream body not forwarded")
	}
	// E2E-BE-FAM-GROWTH §3 — payload is wrapped in {data: T}.
	m := dataMap(t, resp.Body)
	if m["shinyVariant"] != true {
		t.Errorf("shinyVariant=%v", m["shinyVariant"])
	}
	if _, has := m["rolledProbability"]; !has {
		t.Errorf("rolledProbability missing; got %v", m)
	}
	st, ok := m["state"].(map[string]any)
	if !ok {
		t.Fatal("state missing")
	}
	if _, has := st["companionId"]; !has {
		t.Errorf("nested state not camelCased: %v", st)
	}
}

// -----------------------------------------------------------------------------
// RevealBreed — POST /v1/me/companions/:id/reveal (CHO-2229, reveal precedes
// naming). No request body; BFF-wrapped {data:T}; NO retries on POST.
// -----------------------------------------------------------------------------

func TestRevealBreed_PostsNoBody_WrapsEnvelope(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"state":{"companion_id":"f1","growth_stage":0,"stage_name":"egg","species":"fox","revealed_at":"2026-07-17T03:00:00Z"},
			"species":"fox",
			"shiny_variant":true,
			"rarity":"uncommon",
			"rolled_probability":12.5,
			"revealed_at":"2026-07-17T03:00:00Z",
			"already_revealed":false
		}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})
	resp, _ := b.RevealBreed(context.Background(), basicAuth(), "f1")
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Status, resp.Body)
	}
	if cons.lastPath != "/v1/me/companions/f1/reveal" {
		t.Errorf("path=%s", cons.lastPath)
	}
	if cons.lastMethod != http.MethodPost {
		t.Errorf("method=%s", cons.lastMethod)
	}
	m := dataMap(t, resp.Body)
	if m["species"] != "fox" {
		t.Errorf("species=%v", m["species"])
	}
	if _, has := m["revealedAt"]; !has {
		t.Errorf("revealedAt missing (snake→camel); got %v", m)
	}
	st, ok := m["state"].(map[string]any)
	if !ok {
		t.Fatal("state missing")
	}
	if _, has := st["revealedAt"]; !has {
		t.Errorf("nested state.revealedAt missing: %v", st)
	}
}

func TestRevealBreed_NoRetryOnWrites(t *testing.T) {
	var hits atomic.Int64
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})
	resp, _ := b.RevealBreed(context.Background(), basicAuth(), "f1")
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status=%d want 502", resp.Status)
	}
	if hits.Load() != 1 {
		t.Errorf("hits=%d want 1 (no retry on POST)", hits.Load())
	}
}

func TestHatchEgg_NoRetryOnWrites(t *testing.T) {
	// Even on 5xx, POST writes must not retry (idempotency unknown).
	var hits atomic.Int64
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})
	resp, _ := b.HatchEgg(context.Background(), basicAuth(), "f1", []byte(`{}`))
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status=%d want 502", resp.Status)
	}
	if hits.Load() != 1 {
		t.Errorf("hits=%d want 1 (no retry on POST)", hits.Load())
	}
}

func TestHatchEgg_422PassThrough(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"code":"ALREADY_HATCHED"}}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})
	resp, _ := b.HatchEgg(context.Background(), basicAuth(), "f1", []byte(`{}`))
	if resp.Status != http.StatusUnprocessableEntity {
		t.Errorf("status=%d", resp.Status)
	}
}

// -----------------------------------------------------------------------------
// PickResonantConcept — POST /v1/me/companions/:id/resonance (R3-1)
// -----------------------------------------------------------------------------

func TestPickResonantConcept_PassesBody(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"revealed":{"atom_id":"a-1","graph_distance":1,"revealed_via":"user_pick"},"state":{"companion_id":"f1","growth_stage":2}}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})
	resp, _ := b.PickResonantConcept(context.Background(), basicAuth(), "f1", []byte(`{"conceptId":"c-1"}`))
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d", resp.Status)
	}
	if cons.lastPath != "/v1/me/companions/f1/resonance" {
		t.Errorf("path=%s", cons.lastPath)
	}
	// E2E-BE-FAM-GROWTH §3 — payload is wrapped in {data: T}.
	m := dataMap(t, resp.Body)
	rev, ok := m["revealed"].(map[string]any)
	if !ok {
		t.Fatalf("revealed missing")
	}
	if _, ok := rev["atomId"]; !ok {
		t.Errorf("revealed.atomId missing: %v", rev)
	}
}

// -----------------------------------------------------------------------------
// Persona — GET/PUT /v1/me/companions/:id/persona (CHO-2015, camelCase e2e)
// -----------------------------------------------------------------------------

func TestGetPersona_PassesThroughCamelCase(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tone":"encouraging","addressStyle":"first_name","guidanceNote":"be kind","version":2}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})
	resp, _ := b.GetPersona(context.Background(), basicAuth(), "f1")
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d", resp.Status)
	}
	if cons.lastPath != "/v1/me/companions/f1/persona" || cons.lastMethod != http.MethodGet {
		t.Errorf("downstream = %s %s", cons.lastMethod, cons.lastPath)
	}
	// Persona is NOT {data:T}-wrapped (classify, not classifyWithEnvelope) —
	// the camelCase view is the response body verbatim.
	m := decodeJSON(t, resp.Body)
	if m["guidanceNote"] != "be kind" || m["version"].(float64) != 2 {
		t.Errorf("persona view not passed through: %v", m)
	}
}

func TestUpdatePersona_PassesBodyVerbatim_NoSnakeCasing(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tone":"direct","version":1}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})
	in := `{"tone":"direct","addressStyle":"nickname","guidanceNote":"keep it short"}`
	resp, _ := b.UpdatePersona(context.Background(), basicAuth(), "f1", []byte(in))
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d", resp.Status)
	}
	if cons.lastPath != "/v1/me/companions/f1/persona" || cons.lastMethod != http.MethodPut {
		t.Errorf("downstream = %s %s", cons.lastMethod, cons.lastPath)
	}
	// Persona is camelCase end-to-end — the body MUST reach consumption
	// verbatim (no CamelToSnakeJSON); consumption's updatePersonaReq decodes
	// camelCase directly.
	if !strings.Contains(string(cons.lastBody), "addressStyle") || strings.Contains(string(cons.lastBody), "address_style") {
		t.Errorf("body was snake-cased in transit: %s", cons.lastBody)
	}
}

// -----------------------------------------------------------------------------
// TriggerSourceRevelation
// -----------------------------------------------------------------------------

func TestTriggerSourceRevelation_HappyPath(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"preview_llm_tier":"pro","window_expires_at":"2026-05-14T00:00:00Z","window_duration_seconds":86400,"state":{"companion_id":"f1"}}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})
	resp, _ := b.TriggerSourceRevelation(context.Background(), basicAuth(), "f1", []byte(`{"manaTier":"premium"}`))
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d", resp.Status)
	}
	if cons.lastPath != "/v1/me/companions/f1/source-revelation" {
		t.Errorf("path=%s", cons.lastPath)
	}
	// E2E-BE-FAM-GROWTH §3 — payload is wrapped in {data: T}.
	m := dataMap(t, resp.Body)
	if _, ok := m["previewLlmTier"]; !ok {
		t.Errorf("previewLlmTier missing: %v", m)
	}
}

// -----------------------------------------------------------------------------
// ListGrowthEvents
// -----------------------------------------------------------------------------

func TestListGrowthEvents_PaginationQueryForwarded(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"events":[{"event_id":"e1","amount":10,"awarded_at":"2026-05-13T00:00:00Z"}],"next_page_token":"tok-1"}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})
	resp, _ := b.ListGrowthEvents(context.Background(), basicAuth(), "f1", "tok-0", 25)
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d", resp.Status)
	}
	if !strings.HasPrefix(cons.lastPath, "/v1/me/companions/f1/growth-events") {
		t.Errorf("path=%s", cons.lastPath)
	}
	// E2E-BE-FAM-GROWTH §3 — payload is wrapped in {data: T}.
	m := dataMap(t, resp.Body)
	if _, ok := m["nextPageToken"]; !ok {
		t.Errorf("nextPageToken missing: %v", m)
	}
}

// -----------------------------------------------------------------------------
// EggCatalog — GET /v1/companion-eggs/catalog
// -----------------------------------------------------------------------------

func TestEggCatalog_HappyPath(t *testing.T) {
	// Tenancy emits the raw `items: [...]` shape per its egg handler (tenancy
	// upstream renames at W5). The bridge reconciles items→skus + per-row
	// priceCents→priceMicros for the FE EggSku model (E2E-BE-FAM-GROWTH §3).
	ten := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/familiar-eggs/catalog" { // tenancy upstream renames at W5
			t.Errorf("downstream path=%s want /api/familiar-eggs/catalog", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"items":[{"sku":"common-001","price_cents":500}]}`))
	})
	b := companionbridge.New(companionbridge.Config{TenancyURL: ten.URL, PerCallTimeout: time.Second})
	resp, _ := b.EggCatalog(context.Background(), basicAuth())
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d", resp.Status)
	}
	if ten.lastTenant != "tenant-001" {
		t.Errorf("X-Tenant-Id=%s", ten.lastTenant)
	}
	m := dataMap(t, resp.Body)
	skus, ok := m["skus"].([]any)
	if !ok || len(skus) == 0 {
		t.Fatalf("skus missing: %v", m)
	}
	first := skus[0].(map[string]any)
	if _, ok := first["sku"]; !ok {
		t.Errorf("sku missing (camelCase): %v", first)
	}
	if pm, _ := first["priceMicros"].(float64); pm != 5000000 {
		t.Errorf("priceMicros = %v want 5000000 (500¢ × 10_000)", first["priceMicros"])
	}
}

// -----------------------------------------------------------------------------
// EggOdds — GET /v1/companion-eggs/:sku/odds
// -----------------------------------------------------------------------------

func TestEggOdds_HappyPath(t *testing.T) {
	// Tenancy emits `sku` on the odds response; bridge reconciles to
	// `eggSku` to match the FE PreviewEggOddsResponse model
	// (E2E-BE-FAM-GROWTH §3).
	ten := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/familiar-eggs/golden-001/odds" { // tenancy upstream renames at W5
			t.Errorf("downstream path=%s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"sku":"golden-001","odds":[{"species":"dragon","probability":1.5,"rarity":"legendary"}],"total_weight":1000,"distribution_updated_at":"2026-05-13T00:00:00Z"}`))
	})
	b := companionbridge.New(companionbridge.Config{TenancyURL: ten.URL, PerCallTimeout: time.Second})
	resp, _ := b.EggOdds(context.Background(), basicAuth(), "golden-001")
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d", resp.Status)
	}
	m := dataMap(t, resp.Body)
	if _, ok := m["eggSku"]; !ok {
		t.Errorf("eggSku missing (sku→eggSku reconciliation skipped): %v", m)
	}
	if _, ok := m["distributionUpdatedAt"]; !ok {
		t.Errorf("distributionUpdatedAt missing: %v", m)
	}
}

// -----------------------------------------------------------------------------
// EggCheckout — POST /v1/companion-eggs/checkout
// -----------------------------------------------------------------------------

func TestEggCheckout_HappyPath_PassesBody(t *testing.T) {
	ten := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/familiar-eggs/checkout" { // tenancy upstream renames at W5
			t.Errorf("downstream path=%s", r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"stripe_checkout_url":"https://stripe/sess/abc","purchase_id":"p-1"}`))
	})
	b := companionbridge.New(companionbridge.Config{TenancyURL: ten.URL, PerCallTimeout: time.Second})
	resp, _ := b.EggCheckout(context.Background(), basicAuth(), []byte(`{"eggSku":"golden-001"}`))
	if resp.Status != http.StatusCreated {
		t.Fatalf("status=%d", resp.Status)
	}
	// E2E-BE-FAM-GROWTH §3 — payload is wrapped in {data: T}.
	m := dataMap(t, resp.Body)
	if _, ok := m["stripeCheckoutUrl"]; !ok {
		t.Errorf("stripeCheckoutUrl missing: %v", m)
	}
	if _, ok := m["purchaseId"]; !ok {
		t.Errorf("purchaseId missing: %v", m)
	}
}

// EggCheckout — contract-reconciliation tests (Wave D(c) / Phyllis blocker).
//
// The FE sends camelCase ({sku, eggSku, returnUrl}); chora-tenancy expects
// snake_case ({egg_sku, return_url}) per its handler at
// chora-tenancy's egg checkout handler (tenancy upstream renames at W5).
// The BFF MUST translate the request body before forwarding, and MUST
// stamp the X-GCID header tenancy reads (separate from X-Chora-GCID).

func TestEggCheckout_TranslatesCamelEggSkuToSnakeCase(t *testing.T) {
	ten := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"stripe_checkout_url":"https://stripe/x","purchase_id":"p1"}`))
	})
	b := companionbridge.New(companionbridge.Config{TenancyURL: ten.URL, PerCallTimeout: time.Second})
	resp, _ := b.EggCheckout(context.Background(), basicAuth(),
		[]byte(`{"eggSku":"golden-001","returnUrl":"https://chora.site/a/companion/egg/p1"}`))
	if resp.Status != http.StatusCreated {
		t.Fatalf("status=%d body=%s", resp.Status, resp.Body)
	}
	var forwarded map[string]any
	if err := json.Unmarshal(ten.lastBody, &forwarded); err != nil {
		t.Fatalf("downstream body invalid JSON: %v body=%s", err, string(ten.lastBody))
	}
	if forwarded["egg_sku"] != "golden-001" {
		t.Errorf("downstream egg_sku = %v; want golden-001 (camel→snake xlate missing)", forwarded["egg_sku"])
	}
	if _, leaked := forwarded["eggSku"]; leaked {
		t.Errorf("camel eggSku leaked to downstream: %v", forwarded)
	}
	if forwarded["return_url"] != "https://chora.site/a/companion/egg/p1" {
		t.Errorf("downstream return_url = %v; want translated", forwarded["return_url"])
	}
	if _, leaked := forwarded["returnUrl"]; leaked {
		t.Errorf("camel returnUrl leaked to downstream: %v", forwarded)
	}
}

func TestEggCheckout_AcceptsFEBareSku_TranslatesToEggSku(t *testing.T) {
	// FE CompanionGrowthService.checkout() at chora-web/.../companion-growth.service.ts:154
	// posts {sku} (NOT {eggSku}). The BFF MUST accept that shape and translate
	// the field name to {egg_sku} so tenancy doesn't 422 on validation_failed.
	ten := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"stripe_checkout_url":"https://stripe/y","purchase_id":"p2"}`))
	})
	b := companionbridge.New(companionbridge.Config{TenancyURL: ten.URL, PerCallTimeout: time.Second})
	resp, _ := b.EggCheckout(context.Background(), basicAuth(), []byte(`{"sku":"egg.standard.v1"}`))
	if resp.Status != http.StatusCreated {
		t.Fatalf("status=%d body=%s", resp.Status, resp.Body)
	}
	var forwarded map[string]any
	if err := json.Unmarshal(ten.lastBody, &forwarded); err != nil {
		t.Fatalf("downstream body invalid JSON: %v body=%s", err, string(ten.lastBody))
	}
	if forwarded["egg_sku"] != "egg.standard.v1" {
		t.Errorf("downstream egg_sku = %v; want egg.standard.v1 (FE bare-sku alias missing)", forwarded["egg_sku"])
	}
}

func TestEggCheckout_StampsXGCIDHeaderForTenancy(t *testing.T) {
	// chora-tenancy checkout reads X-GCID (its egg checkout handler; tenancy upstream renames at W5)
	// — distinct from mesh-trust chora-gcid. Without it tenancy 400s
	// `x_gcid_required`.
	var gotXGCID, gotMeshGCID string
	ten := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotXGCID = r.Header.Get("X-GCID")
		gotMeshGCID = r.Header.Get("chora-gcid")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"stripe_checkout_url":"https://stripe/z","purchase_id":"p3"}`))
	})
	b := companionbridge.New(companionbridge.Config{TenancyURL: ten.URL, PerCallTimeout: time.Second})
	auth := basicAuth()
	auth.GCID = "gcid-phyllis-001"
	_, _ = b.EggCheckout(context.Background(), auth, []byte(`{"sku":"egg.standard.v1"}`))
	if gotXGCID != "gcid-phyllis-001" {
		t.Errorf("X-GCID header = %q; want gcid-phyllis-001", gotXGCID)
	}
	if gotMeshGCID == "" {
		t.Errorf("chora-gcid (mesh-trust) header dropped; want gcid-phyllis-001")
	}
}

func TestEggCheckout_TenancyValidationError_PassesThrough_422(t *testing.T) {
	ten := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"code":"validation_failed","message":"egg_sku required"}}`))
	})
	b := companionbridge.New(companionbridge.Config{TenancyURL: ten.URL, PerCallTimeout: time.Second})
	resp, _ := b.EggCheckout(context.Background(), basicAuth(), []byte(`{}`))
	if resp.Status != http.StatusUnprocessableEntity {
		t.Errorf("status=%d want 422 pass-through", resp.Status)
	}
}

func TestEggCheckout_TenancyDown_Returns502(t *testing.T) {
	ten := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	b := companionbridge.New(companionbridge.Config{TenancyURL: ten.URL, PerCallTimeout: time.Second})
	resp, _ := b.EggCheckout(context.Background(), basicAuth(), []byte(`{"sku":"x"}`))
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status=%d want 502 (5xx → 502)", resp.Status)
	}
}

func TestEggCheckout_TenancyTimeout_Returns504(t *testing.T) {
	ten := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusCreated)
	})
	b := companionbridge.New(companionbridge.Config{TenancyURL: ten.URL, PerCallTimeout: 50 * time.Millisecond})
	resp, _ := b.EggCheckout(context.Background(), basicAuth(), []byte(`{"sku":"x"}`))
	if resp.Status != http.StatusGatewayTimeout {
		t.Errorf("status=%d want 504 (timeout)", resp.Status)
	}
}

func TestEggCheckout_NonJSONBody_PassesThroughAsIs(t *testing.T) {
	// Defensive: non-JSON body must not panic — tenancy will validate and
	// return its own 400. The bridge MUST forward the raw bytes.
	ten := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_body"}}`))
	})
	b := companionbridge.New(companionbridge.Config{TenancyURL: ten.URL, PerCallTimeout: time.Second})
	resp, _ := b.EggCheckout(context.Background(), basicAuth(), []byte(`not json`))
	if resp.Status != http.StatusBadRequest {
		t.Errorf("status=%d want 400 pass-through", resp.Status)
	}
	if string(ten.lastBody) != "not json" {
		t.Errorf("body mutated; got=%q want=%q", string(ten.lastBody), "not json")
	}
}

func TestCamelToSnakeJSON_NestedAndArrays(t *testing.T) {
	in := []byte(`{
		"eggSku":"golden-001",
		"returnUrl":"https://x",
		"already_snake":"y",
		"nested":{"barBaz":1,"list":[{"alphaBeta":2}]}
	}`)
	out := companionbridge.CamelToSnakeJSON(in)
	m := decodeJSON(t, out)
	if m["egg_sku"] != "golden-001" {
		t.Errorf("egg_sku missing/wrong: %v", m)
	}
	if m["return_url"] != "https://x" {
		t.Errorf("return_url missing/wrong: %v", m)
	}
	if _, leaked := m["eggSku"]; leaked {
		t.Errorf("eggSku leaked")
	}
	if m["already_snake"] != "y" {
		t.Errorf("already_snake should pass through unchanged (no uppercase): %v", m)
	}
	nested, ok := m["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested missing: %v", m)
	}
	if _, ok := nested["bar_baz"]; !ok {
		t.Errorf("nested.bar_baz missing: %v", nested)
	}
	list, _ := nested["list"].([]any)
	first, _ := list[0].(map[string]any)
	if _, ok := first["alpha_beta"]; !ok {
		t.Errorf("nested array element not converted: %v", first)
	}
}

func TestCamelToSnakeJSON_PreservesNonJSON(t *testing.T) {
	in := []byte(`not json at all`)
	out := companionbridge.CamelToSnakeJSON(in)
	if string(out) != string(in) {
		t.Errorf("non-JSON should pass through unchanged; got %s", string(out))
	}
}

// -----------------------------------------------------------------------------
// Configuration safety: missing URL → 502 (not nil deref)
// -----------------------------------------------------------------------------

func TestUnconfiguredService_Returns502(t *testing.T) {
	b := companionbridge.New(companionbridge.Config{PerCallTimeout: 100 * time.Millisecond})
	resp, _ := b.GetCompanionGrowth(context.Background(), basicAuth(), "f1")
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status=%d want 502 when ConsumptionURL unset", resp.Status)
	}
	resp2, _ := b.EggCatalog(context.Background(), basicAuth())
	if resp2.Status != http.StatusBadGateway {
		t.Errorf("egg-catalog status=%d want 502 when TenancyURL unset", resp2.Status)
	}
}

// -----------------------------------------------------------------------------
// Per-route timeout: HatchEgg uses extended timeout (30s)
// -----------------------------------------------------------------------------

func TestPerRouteTimeouts_HatchUsesExtendedBudget(t *testing.T) {
	cfg := companionbridge.Config{ConsumptionURL: "http://example", TenancyURL: "http://example"}
	cfg.ApplyDefaults()
	if cfg.PerCallTimeout != companionbridge.DefaultPerCallTimeout {
		t.Errorf("PerCallTimeout default = %v", cfg.PerCallTimeout)
	}
	if cfg.HatchTimeout != companionbridge.DefaultHatchTimeout {
		t.Errorf("HatchTimeout default = %v want %v", cfg.HatchTimeout, companionbridge.DefaultHatchTimeout)
	}
	if cfg.HatchTimeout <= cfg.PerCallTimeout {
		t.Errorf("hatch timeout must exceed per-call default")
	}
}

// -----------------------------------------------------------------------------
// Env loader
// -----------------------------------------------------------------------------

func TestLoadConfigFromEnv_ReadsSVCUrls(t *testing.T) {
	t.Setenv("SVC_CONSUMPTION_URL", "http://cons:8080")
	t.Setenv("SVC_TENANCY_URL", "http://ten:8080")
	cfg := companionbridge.LoadConfigFromEnv()
	if cfg.ConsumptionURL != "http://cons:8080" {
		t.Errorf("ConsumptionURL=%s", cfg.ConsumptionURL)
	}
	if cfg.TenancyURL != "http://ten:8080" {
		t.Errorf("TenancyURL=%s", cfg.TenancyURL)
	}
}

// -----------------------------------------------------------------------------
// camelCase converter — direct unit
// -----------------------------------------------------------------------------

func TestSnakeToCamelJSON_NestedAndArrays(t *testing.T) {
	in := []byte(`{
		"foo_bar":"x",
		"nested":{"baz_qux":1,"list":[{"alpha_beta":2}]},
		"already_camel":"y",
		"plainArr":[1,2,3]
	}`)
	out := companionbridge.SnakeToCamelJSON(in)
	m := decodeJSON(t, out)
	if _, ok := m["fooBar"]; !ok {
		t.Errorf("fooBar missing: %v", m)
	}
	if _, ok := m["foo_bar"]; ok {
		t.Errorf("foo_bar leaked")
	}
	nested, ok := m["nested"].(map[string]any)
	if !ok {
		t.Fatal("nested missing")
	}
	if _, ok := nested["bazQux"]; !ok {
		t.Errorf("nested.bazQux missing: %v", nested)
	}
	list, _ := nested["list"].([]any)
	first, _ := list[0].(map[string]any)
	if _, ok := first["alphaBeta"]; !ok {
		t.Errorf("array element not converted: %v", first)
	}
	if _, ok := m["alreadyCamel"]; !ok {
		t.Errorf("alreadyCamel missing")
	}
}

func TestSnakeToCamelJSON_PreservesNonJSON(t *testing.T) {
	in := []byte(`not json at all`)
	out := companionbridge.SnakeToCamelJSON(in)
	if string(out) != string(in) {
		t.Errorf("non-JSON should pass through unchanged; got %s", string(out))
	}
}

// -----------------------------------------------------------------------------
// Legacy bare-`gcid` header stamping (Surface B fix, 2026-05-15)
// -----------------------------------------------------------------------------
//
// chora-consumption's `requireContext` (see services/chora-consumption/
// internal/adapter/http/router.go:659) reads the BARE lowercase `gcid`
// header — NOT `X-Chora-GCID` and NOT the mesh-trust `chora-gcid` (despite
// the name collision the latter is set via servicemesh.MarshalToHeaders
// alongside chora-tenant-id; consumption's older requireContext path
// pre-dates the mesh-trust integration). Every consumption-bound route in
// this package MUST stamp the bare `gcid` header on the outbound request
// so listCompanionInstances + the 5 sibling growth handlers do not return
// 401 MISSING_CONTEXT "gcid required".
//
// The ChatStream route (ADR-154 / commit be0fe6d) added this stamping
// explicitly; this test fixes the omission for the other 6 routes by
// asserting the bare header on every outbound consumption call.

func TestConsumptionRoutes_StampLegacyGCIDHeader(t *testing.T) {
	const wantGCID = "00000000-0000-7000-8000-000000001999"

	// Each subtest invokes one bridge method against a fresh upstream stub
	// and asserts the bare `gcid` header was set to the AuthCtx.GCID.
	cases := []struct {
		name string
		exec func(b *companionbridge.Bridge, auth companionbridge.AuthCtx)
	}{
		{"ListCompanions", func(b *companionbridge.Bridge, auth companionbridge.AuthCtx) {
			_, _ = b.ListCompanions(context.Background(), auth)
		}},
		{"GetCompanionGrowth", func(b *companionbridge.Bridge, auth companionbridge.AuthCtx) {
			_, _ = b.GetCompanionGrowth(context.Background(), auth, "f1")
		}},
		{"HatchEgg", func(b *companionbridge.Bridge, auth companionbridge.AuthCtx) {
			_, _ = b.HatchEgg(context.Background(), auth, "f1", []byte(`{}`))
		}},
		{"PickResonantConcept", func(b *companionbridge.Bridge, auth companionbridge.AuthCtx) {
			_, _ = b.PickResonantConcept(context.Background(), auth, "f1", []byte(`{}`))
		}},
		{"TriggerSourceRevelation", func(b *companionbridge.Bridge, auth companionbridge.AuthCtx) {
			_, _ = b.TriggerSourceRevelation(context.Background(), auth, "f1", []byte(`{}`))
		}},
		{"ListGrowthEvents", func(b *companionbridge.Bridge, auth companionbridge.AuthCtx) {
			_, _ = b.ListGrowthEvents(context.Background(), auth, "f1", "", 0)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			})
			b := companionbridge.New(companionbridge.Config{
				ConsumptionURL: cons.URL,
				PerCallTimeout: time.Second,
				HatchTimeout:   time.Second,
			})
			auth := companionbridge.AuthCtx{
				Bearer:      "fb-tok",
				Traceparent: "00-aaa-bbb-01",
				TenantID:    "tenant-001",
				GCID:        wantGCID,
			}
			tc.exec(b, auth)

			if got := cons.lastLegacyGCID; got != wantGCID {
				t.Errorf("upstream missing bare `gcid` header: got %q, want %q "+
					"(chora-consumption requireContext reads bare `gcid`)", got, wantGCID)
			}
			// Mesh-trust path must still be intact alongside the legacy header.
			if cons.lastGCID != wantGCID {
				t.Errorf("upstream X-Chora-GCID/chora-gcid = %q, want %q",
					cons.lastGCID, wantGCID)
			}
		})
	}
}

// TestChatStream_StampsLegacyGCIDHeader pins the existing ADR-154 chat
// route's bare `gcid` stamping so a future refactor cannot regress it.
func TestChatStream_StampsLegacyGCIDHeader(t *testing.T) {
	const wantGCID = "00000000-0000-7000-8000-000000001999"
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: end\ndata: {}\n\n"))
	})
	b := companionbridge.New(companionbridge.Config{
		ConsumptionURL: cons.URL,
		ChatTimeout:    2 * time.Second,
	})
	auth := companionbridge.AuthCtx{
		Bearer:   "fb-tok",
		TenantID: "tenant-001",
		GCID:     wantGCID,
	}
	w := httptest.NewRecorder()
	_ = b.ChatStream(context.Background(), auth, "f1",
		strings.NewReader(`{"message":"hi"}`), w)

	if got := cons.lastLegacyGCID; got != wantGCID {
		t.Errorf("ChatStream upstream missing bare `gcid` header: got %q, want %q",
			got, wantGCID)
	}
}
