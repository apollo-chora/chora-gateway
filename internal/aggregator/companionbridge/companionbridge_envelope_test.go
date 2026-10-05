// companionbridge_envelope_test.go — BFF envelope wrap + FE shape
// reconciliation tests for the 8 E2E-BE-FAM-GROWTH endpoints
// (5 growth subpaths + 3 egg-marketplace).
//
// Background (2026-05-16):
//
//	chora-web FE stripped 8 stub fallbacks at commit e335c364 per the
//	no-debts directive. The CompanionGrowthService now propagates real
//	errors and consumes the BFF envelope shape `{ data: T }` on each
//	call. The raw chora-consumption + chora-tenancy services emit
//	un-enveloped responses (internal mesh callers — gRPC, Pub/Sub —
//	bypass the BFF). The envelope wrap is therefore the gateway's
//	responsibility per its BFF role.
//
// Field-name reconciliations (gateway, per the FE model types):
//   - catalog response: `items` → `skus`
//   - catalog rows: `price_cents` (→camel `priceCents`) → `priceMicros`
//     (×10_000 numeric promotion so $9.99 SGD `999` ¢ → `9990000` µunits)
//   - odds response: `sku` → `eggSku`
//
// Routes excluded from envelope wrap:
//   - GET /v1/me/companions — raw `{ items: [...] }` shape (FE service
//     consumes `env.items` directly per companion-growth.service.ts
//     `listMyCompanions`).
//   - POST /chat (SSE pass-through, body is a stream).
package companionbridge_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/companionbridge"
)

// dataMap unwraps the {"data": {...}} envelope from the bridge response.
// Returns nil + t.Fatal if the envelope is absent.
func dataMap(t *testing.T, body []byte) map[string]any {
	t.Helper()
	env := decodeJSON(t, body)
	d, ok := env["data"].(map[string]any)
	if !ok {
		t.Fatalf("envelope `data` key missing or not object; got %v", env)
	}
	return d
}

// dataArray unwraps {"data": [...]} for list-style envelopes (none expected
// today but kept symmetric).
func dataArray(t *testing.T, body []byte) []any {
	t.Helper()
	env := decodeJSON(t, body)
	d, ok := env["data"].([]any)
	if !ok {
		t.Fatalf("envelope `data` key missing or not array; got %v", env)
	}
	return d
}

// -----------------------------------------------------------------------------
// Envelope on the 5 chora-consumption growth subpaths
// -----------------------------------------------------------------------------

func TestGetCompanionGrowth_WrapsInDataEnvelope(t *testing.T) {
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
			"aha_moment_consumed":false
		}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})
	resp, _ := b.GetCompanionGrowth(context.Background(), basicAuth(), "f1")
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Status, resp.Body)
	}
	d := dataMap(t, resp.Body)
	// All inner keys should be camelCase + reachable under `data`.
	for _, key := range []string{
		"companionId", "growthStage", "stageName", "expCurrent",
		"expNextThreshold", "visibleKgNeighbors", "ahaMomentConsumed",
	} {
		if _, ok := d[key]; !ok {
			t.Errorf("data.%s missing: %v", key, d)
		}
	}
}

func TestHatchEgg_WrapsInDataEnvelope(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"state":{"companion_id":"f1","growth_stage":1,"stage_name":"baby","shiny_variant":false,"exp_current":0,"exp_next_threshold":50,"exp_cumulative":0,"effective_llm_tier":"flash-lite","effective_max_output_tokens":1024,"unlocked_tools":[],"visible_kg_neighbors":[],"aha_moment_consumed":false},
			"species":"owl",
			"shiny_variant":false,
			"rarity":"common",
			"rolled_probability":25.0
		}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, HatchTimeout: 2 * time.Second})
	resp, _ := b.HatchEgg(context.Background(), basicAuth(), "f1",
		[]byte(`{"displayName":"Pip","tone":"playful","resonantAtomId":"a1","learnerPersona":"x"}`))
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d", resp.Status)
	}
	d := dataMap(t, resp.Body)
	if _, ok := d["state"]; !ok {
		t.Errorf("data.state missing: %v", d)
	}
	if v, _ := d["rolledProbability"].(float64); v != 25.0 {
		t.Errorf("data.rolledProbability = %v want 25.0", d["rolledProbability"])
	}
}

func TestPickResonantConcept_WrapsInDataEnvelope(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"state":{"companion_id":"f1","growth_stage":2,"stage_name":"fledgling","shiny_variant":false,"exp_current":10,"exp_next_threshold":100,"exp_cumulative":10,"effective_llm_tier":"flash","effective_max_output_tokens":1024,"unlocked_tools":[],"visible_kg_neighbors":[],"aha_moment_consumed":false},
			"revealed":{"atom_id":"a1","graph_distance":1,"revealed_via":"user_pick","revealed_at":"2026-05-16T00:00:00Z"}
		}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})
	resp, _ := b.PickResonantConcept(context.Background(), basicAuth(), "f1", []byte(`{"conceptId":"c1"}`))
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d", resp.Status)
	}
	d := dataMap(t, resp.Body)
	if _, ok := d["state"]; !ok {
		t.Errorf("data.state missing: %v", d)
	}
	revealed, ok := d["revealed"].(map[string]any)
	if !ok {
		t.Fatalf("data.revealed missing: %v", d)
	}
	if _, ok := revealed["atomId"]; !ok {
		t.Errorf("data.revealed.atomId missing: %v", revealed)
	}
}

func TestTriggerSourceRevelation_WrapsInDataEnvelope(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"state":{"companion_id":"f1","growth_stage":3,"stage_name":"awakened","shiny_variant":false,"exp_current":10,"exp_next_threshold":500,"exp_cumulative":210,"effective_llm_tier":"flash-reasoning","effective_max_output_tokens":2048,"unlocked_tools":[],"visible_kg_neighbors":[],"aha_moment_consumed":true},
			"preview_llm_tier":"pro",
			"window_expires_at":"2026-05-17T00:00:00Z",
			"window_duration_seconds":86400
		}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})
	resp, _ := b.TriggerSourceRevelation(context.Background(), basicAuth(), "f1", []byte(`{}`))
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d", resp.Status)
	}
	d := dataMap(t, resp.Body)
	if _, ok := d["previewLlmTier"]; !ok {
		t.Errorf("data.previewLlmTier missing: %v", d)
	}
	if _, ok := d["windowExpiresAt"]; !ok {
		t.Errorf("data.windowExpiresAt missing: %v", d)
	}
}

func TestListGrowthEvents_WrapsInDataEnvelope(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"events":[{"event_id":"e1","exp_delta":10,"awarded_at":"2026-05-16T00:00:00Z"}],
			"next_page_token":"tok-1"
		}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})
	resp, _ := b.ListGrowthEvents(context.Background(), basicAuth(), "f1", "", 25)
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d", resp.Status)
	}
	d := dataMap(t, resp.Body)
	if _, ok := d["events"]; !ok {
		t.Errorf("data.events missing: %v", d)
	}
	if _, ok := d["nextPageToken"]; !ok {
		t.Errorf("data.nextPageToken missing: %v", d)
	}
}

// -----------------------------------------------------------------------------
// Envelope + field-name reconciliations on the 3 chora-tenancy egg routes
// -----------------------------------------------------------------------------

func TestEggCatalog_WrapsAndReconcilesItemsToSkus(t *testing.T) {
	ten := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		// Raw tenancy wire: items[] + price_cents per egg row.
		_, _ = w.Write([]byte(`{
			"items":[
				{"sku":"egg.standard.v1","display_name":"Standard","description":"","price_cents":999,"currency":"SGD","purchasable":true},
				{"sku":"egg.premium.v1","display_name":"Premium","description":"","price_cents":2999,"currency":"SGD","purchasable":true}
			],
			"total":2
		}`))
	})
	b := companionbridge.New(companionbridge.Config{TenancyURL: ten.URL, PerCallTimeout: time.Second})
	resp, _ := b.EggCatalog(context.Background(), basicAuth())
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Status, resp.Body)
	}
	d := dataMap(t, resp.Body)
	skus, ok := d["skus"].([]any)
	if !ok {
		t.Fatalf("data.skus missing (items→skus reconciliation skipped): %v", d)
	}
	if _, leak := d["items"]; leak {
		t.Errorf("data.items still present; expected items→skus rename")
	}
	if len(skus) != 2 {
		t.Fatalf("expected 2 skus; got %d", len(skus))
	}
	first := skus[0].(map[string]any)
	pm, ok := first["priceMicros"].(float64)
	if !ok {
		t.Fatalf("skus[0].priceMicros missing (priceCents→priceMicros reconciliation skipped): %v", first)
	}
	if pm != 9990000 {
		t.Errorf("skus[0].priceMicros = %v want 9990000 (=999¢ × 10_000)", pm)
	}
	if _, leak := first["priceCents"]; leak {
		t.Errorf("skus[0].priceCents still present; expected priceCents→priceMicros rename")
	}
	second := skus[1].(map[string]any)
	if pm, _ := second["priceMicros"].(float64); pm != 29990000 {
		t.Errorf("skus[1].priceMicros = %v want 29990000", pm)
	}
}

func TestEggOdds_WrapsAndReconcilesSkuToEggSku(t *testing.T) {
	ten := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"sku":"egg.standard.v1",
			"odds":[
				{"species":"owl","probability":25,"rarity":"common"},
				{"species":"fox","probability":20,"rarity":"common"}
			],
			"total_weight":100,
			"distribution_updated_at":"2026-05-13T05:59:14Z",
			"chora_imda_dimension":"transparency"
		}`))
	})
	b := companionbridge.New(companionbridge.Config{TenancyURL: ten.URL, PerCallTimeout: time.Second})
	resp, _ := b.EggOdds(context.Background(), basicAuth(), "egg.standard.v1")
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d", resp.Status)
	}
	d := dataMap(t, resp.Body)
	if _, ok := d["eggSku"]; !ok {
		t.Fatalf("data.eggSku missing (sku→eggSku reconciliation skipped): %v", d)
	}
	if _, leak := d["sku"]; leak {
		t.Errorf("data.sku still present; expected sku→eggSku rename")
	}
	odds, ok := d["odds"].([]any)
	if !ok || len(odds) != 2 {
		t.Fatalf("data.odds missing or wrong length: %v", d)
	}
	// IMDA D2: numeric probabilities preserved.
	first := odds[0].(map[string]any)
	if p, _ := first["probability"].(float64); p != 25 {
		t.Errorf("odds[0].probability = %v want 25", first["probability"])
	}
	if r, _ := first["rarity"].(string); r != "common" {
		t.Errorf("odds[0].rarity = %v want common", first["rarity"])
	}
}

func TestEggCheckout_WrapsInDataEnvelope(t *testing.T) {
	ten := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"purchase_id":"p-1","stripe_checkout_url":"https://stripe/sess/abc"}`))
	})
	b := companionbridge.New(companionbridge.Config{TenancyURL: ten.URL, PerCallTimeout: time.Second})
	resp, _ := b.EggCheckout(context.Background(), basicAuth(), []byte(`{"sku":"egg.standard.v1"}`))
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d", resp.Status)
	}
	d := dataMap(t, resp.Body)
	if _, ok := d["purchaseId"]; !ok {
		t.Errorf("data.purchaseId missing: %v", d)
	}
	if _, ok := d["stripeCheckoutUrl"]; !ok {
		t.Errorf("data.stripeCheckoutUrl missing: %v", d)
	}
}

// -----------------------------------------------------------------------------
// Negative path — error envelopes are NOT double-wrapped
// -----------------------------------------------------------------------------

// When chora-consumption returns 4xx with an `{error: {...}}` body, the bridge
// MUST pass that envelope through unchanged (no nesting under data). The FE
// `unwrap()` helper checks `env.error?.message` first; double-wrapping would
// hide that message inside `data.error`.
func TestGetCompanionGrowth_404Body_PassesThroughErrorEnvelope(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"COMPANION_NOT_FOUND","message":"missing"}}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})
	resp, _ := b.GetCompanionGrowth(context.Background(), basicAuth(), "missing")
	if resp.Status != http.StatusNotFound {
		t.Fatalf("status=%d want 404", resp.Status)
	}
	env := decodeJSON(t, resp.Body)
	if _, ok := env["error"]; !ok {
		t.Errorf("error envelope missing: %v", env)
	}
	if _, ok := env["data"]; ok {
		t.Errorf("error response should NOT be wrapped in data: %v", env)
	}
}

// When chora-consumption's `writeError` helper emits the RAW {code,
// message} shape (no `error` wrapper — its current behaviour per
// services/chora-consumption/internal/adapter/http/router.go), the
// bridge MUST reshape into the BFF {error: {code, message}} contract
// so the FE `unwrap()` can read `env.error.message`. Otherwise the FE
// throws with the generic fallback string and the real cause is lost.
func TestGetCompanionGrowth_400RawCodeMessage_ReshapedIntoErrorEnvelope(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"INVALID_ARGUMENT","message":"atom_id required"}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})
	resp, _ := b.GetCompanionGrowth(context.Background(), basicAuth(), "f1")
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", resp.Status)
	}
	env := decodeJSON(t, resp.Body)
	errObj, ok := env["error"].(map[string]any)
	if !ok {
		t.Fatalf("error envelope reshape skipped; got %v", env)
	}
	if errObj["code"] != "INVALID_ARGUMENT" {
		t.Errorf("error.code = %v want INVALID_ARGUMENT", errObj["code"])
	}
	if errObj["message"] != "atom_id required" {
		t.Errorf("error.message = %v want 'atom_id required'", errObj["message"])
	}
	if _, ok := env["code"]; ok {
		t.Errorf("top-level `code` leaked after reshape: %v", env)
	}
}

// -----------------------------------------------------------------------------
// Outbound POST body camelCase→snake_case translation (FE → consumption)
// -----------------------------------------------------------------------------
//
// FE sends camelCase JSON ({displayName, resonantAtomId, ...});
// chora-consumption handlers decode the snake_case form
// ({display_name, resonant_atom_id, ...}). The bridge translates
// outbound on POST so the consumption handler can read the body.

func TestHatchEgg_OutboundBody_CamelToSnake(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"state":{"companion_id":"f1","growth_stage":1,"stage_name":"baby","shiny_variant":false,"exp_current":0,"exp_next_threshold":50,"exp_cumulative":0,"effective_llm_tier":"flash-lite","effective_max_output_tokens":1024,"unlocked_tools":[],"visible_kg_neighbors":[],"aha_moment_consumed":false},"species":"owl","shiny_variant":false,"rarity":"common","rolled_probability":25.0}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, HatchTimeout: 2 * time.Second})
	feBody := []byte(`{"displayName":"Spark","tone":"playful","learnerPersona":"explorer","resonantAtomId":"a-1"}`)
	_, _ = b.HatchEgg(context.Background(), basicAuth(), "f1", feBody)
	got := string(cons.lastBody)
	for _, want := range []string{`"display_name"`, `"learner_persona"`, `"resonant_atom_id"`} {
		if !strings.Contains(got, want) {
			t.Errorf("outbound body missing snake_case key %s; got=%s", want, got)
		}
	}
	for _, bad := range []string{`"displayName"`, `"learnerPersona"`, `"resonantAtomId"`} {
		if strings.Contains(got, bad) {
			t.Errorf("outbound body still has camelCase key %s; got=%s", bad, got)
		}
	}
}

func TestPickResonantConcept_OutboundBody_CamelToSnake(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"state":{"companion_id":"f1","growth_stage":2,"stage_name":"fledgling","shiny_variant":false,"exp_current":10,"exp_next_threshold":100,"exp_cumulative":10,"effective_llm_tier":"flash","effective_max_output_tokens":1024,"unlocked_tools":[],"visible_kg_neighbors":[],"aha_moment_consumed":false},"revealed":{"atom_id":"a1","graph_distance":1,"revealed_via":"user_pick","revealed_at":"2026-05-16T00:00:00Z"}}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})
	feBody := []byte(`{"atomId":"00000000-0000-7000-8000-00000000a0a3"}`)
	_, _ = b.PickResonantConcept(context.Background(), basicAuth(), "f1", feBody)
	got := string(cons.lastBody)
	if !strings.Contains(got, `"atom_id"`) {
		t.Errorf("outbound body missing snake_case `atom_id`; got=%s", got)
	}
	if strings.Contains(got, `"atomId"`) {
		t.Errorf("outbound body still has camelCase `atomId`; got=%s", got)
	}
}

func TestTriggerSourceRevelation_OutboundBody_CamelToSnake(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"state":{"companion_id":"f1","growth_stage":3,"stage_name":"awakened","shiny_variant":false,"exp_current":10,"exp_next_threshold":500,"exp_cumulative":210,"effective_llm_tier":"flash-reasoning","effective_max_output_tokens":2048,"unlocked_tools":[],"visible_kg_neighbors":[],"aha_moment_consumed":true},"preview_llm_tier":"pro","window_expires_at":"2026-05-17T00:00:00Z","window_duration_seconds":86400}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})
	feBody := []byte(`{"manaTier":"premium","windowDurationSecondsOverride":3600}`)
	_, _ = b.TriggerSourceRevelation(context.Background(), basicAuth(), "f1", feBody)
	got := string(cons.lastBody)
	if !strings.Contains(got, `"mana_tier"`) {
		t.Errorf("outbound body missing snake_case `mana_tier`; got=%s", got)
	}
	if !strings.Contains(got, `"window_duration_seconds_override"`) {
		t.Errorf("outbound body missing snake_case `window_duration_seconds_override`; got=%s", got)
	}
	if strings.Contains(got, `"manaTier"`) {
		t.Errorf("outbound body still has camelCase `manaTier`; got=%s", got)
	}
}

// -----------------------------------------------------------------------------
// ListCompanions — explicit NO-wrap (FE consumes raw {items:[]})
// -----------------------------------------------------------------------------
//
// `CompanionGrowthService.listMyCompanions()` in chora-web reads `env.items`
// directly — it does NOT go through `unwrap(env)`. If the bridge wraps this
// route the FE sees `{data: {items: []}}` and `env.items` becomes undefined,
// rendering empty rosters. This test pins the raw-shape contract.
func TestListCompanions_DoesNotWrapInData(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"companion_id":"f1","name":"Pip"}]}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})
	resp, _ := b.ListCompanions(context.Background(), basicAuth())
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d", resp.Status)
	}
	env := decodeJSON(t, resp.Body)
	if _, ok := env["items"]; !ok {
		t.Fatalf("ListCompanions raw `items` shape lost: %v", env)
	}
	if _, leak := env["data"]; leak {
		t.Errorf("ListCompanions should NOT be wrapped in data: %v", env)
	}
}
