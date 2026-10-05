// companionbridge_learner_sovereign_test.go — aggregator-level specs for the
// learner-sovereign Discovery Graph Companion routes (2026-07-01):
//
//	POST /api/v1/me/companions/acquire     → chora-consumption POST /v1/me/companions/acquire
//	GET  /api/v1/me/companions/bindings    → chora-consumption GET  /v1/me/companions/bindings
//	GET  /api/v1/me/companions/:id/memory  → chora-consumption GET  /v1/me/companions/:id/memory
//
// Mirrors the sibling method specs in companionbridge_test.go: assert the
// downstream path + method, mesh-trust header stamping, the acquire camelCase→
// snake_case request translation, the snake_case→camelCase response re-marshal,
// and the 5xx→502 normalisation. Reuses newStub / basicAuth / decodeJSON.
//
// Strict TDD: tests written BEFORE the bridge implementation.
package companionbridge_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/companionbridge"
)

// -----------------------------------------------------------------------------
// AcquireCompanion — POST /v1/me/companions/acquire
// -----------------------------------------------------------------------------

func TestAcquireCompanion_HappyPath_VerbatimRequest_CamelResponse(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		// The sovereign consumption handler already returns camelCase.
		_, _ = w.Write([]byte(`{"companionId":"f-new","name":"Sprint","mapTheme":"Scrum","bindingId":"b-1","acquisition":"dev_hatched"}`))
	})
	b := companionbridge.New(companionbridge.Config{
		ConsumptionURL: cons.URL,
		PerCallTimeout: 1 * time.Second,
	})

	// The FE posts camelCase; the sovereign consumption handler decodes camelCase
	// (like concept-graph + goals), so the body is forwarded VERBATIM.
	resp, _ := b.AcquireCompanion(context.Background(), basicAuth(), []byte(`{"mapTheme":"Scrum","companionName":"Sprint","mode":"dev_hatched"}`))

	if resp.Status != http.StatusCreated {
		t.Fatalf("status=%d want 201", resp.Status)
	}
	if cons.lastPath != "/v1/me/companions/acquire" {
		t.Errorf("path=%s want /v1/me/companions/acquire", cons.lastPath)
	}
	if cons.lastMethod != http.MethodPost {
		t.Errorf("method=%s want POST", cons.lastMethod)
	}
	// Mesh-trust headers the downstream extRequireContext reads.
	if cons.lastTenant != "tenant-001" {
		t.Errorf("X-Tenant-Id=%s want tenant-001", cons.lastTenant)
	}
	if cons.lastLegacyGCID != "gcid-001" {
		t.Errorf("lowercase gcid=%s want gcid-001", cons.lastLegacyGCID)
	}
	// Request body forwarded VERBATIM — the camelCase key reaches consumption
	// unchanged (NOT snake-cased), matching the sovereign camelCase contract.
	if !strings.Contains(string(cons.lastBody), `"mapTheme"`) {
		t.Errorf("request body not forwarded verbatim (mapTheme missing): %s", cons.lastBody)
	}
	if strings.Contains(string(cons.lastBody), `"map_theme"`) {
		t.Errorf("acquire body was snake-cased; want verbatim camelCase: %s", cons.lastBody)
	}
	// Response returned UNWRAPPED (no {data:T}); already-camelCase preserved.
	m := decodeJSON(t, resp.Body)
	if _, wrapped := m["data"]; wrapped {
		t.Errorf("acquire must be unwrapped (no {data:T}); got %v", m)
	}
	if _, ok := m["companionId"]; !ok {
		t.Errorf("expected camelCase companionId; got %v", m)
	}
	if _, ok := m["bindingId"]; !ok {
		t.Errorf("expected camelCase bindingId; got %v", m)
	}
}

func TestAcquireCompanion_Downstream5xxNormalisesTo502(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})

	resp, _ := b.AcquireCompanion(context.Background(), basicAuth(), []byte(`{}`))
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status=%d want 502 (downstream 5xx normalised)", resp.Status)
	}
}

// -----------------------------------------------------------------------------
// ListCompanionBindings — GET /v1/me/companions/bindings
// -----------------------------------------------------------------------------

func TestListCompanionBindings_HappyPath_CamelCaseUnwrapped(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"bindings":[{"companion_id":"f1","concept_id":"c1"}]}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})

	resp, _ := b.ListCompanionBindings(context.Background(), basicAuth())

	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.Status)
	}
	if cons.lastPath != "/v1/me/companions/bindings" {
		t.Errorf("path=%s want /v1/me/companions/bindings", cons.lastPath)
	}
	if cons.lastMethod != http.MethodGet {
		t.Errorf("method=%s want GET", cons.lastMethod)
	}
	if cons.lastTenant != "tenant-001" {
		t.Errorf("X-Tenant-Id=%s want tenant-001", cons.lastTenant)
	}
	m := decodeJSON(t, resp.Body)
	if _, wrapped := m["data"]; wrapped {
		t.Errorf("bindings must be unwrapped (no {data:T}); got %v", m)
	}
	binds, ok := m["bindings"].([]any)
	if !ok || len(binds) == 0 {
		t.Fatalf("missing bindings: %v", m)
	}
	b0 := binds[0].(map[string]any)
	if _, has := b0["companionId"]; !has {
		t.Errorf("expected camelCase companionId; got %v", b0)
	}
	if _, has := b0["concept_id"]; has {
		t.Errorf("snake_case concept_id leaked: %v", b0)
	}
}

// -----------------------------------------------------------------------------
// GetCompanionMemory — GET /v1/me/companions/:id/memory
// -----------------------------------------------------------------------------

func TestGetCompanionMemory_ForwardsGoalScope(t *testing.T) {
	// ADR-214 regression guard. This bridge REBUILDS the upstream URL rather
	// than proxying the raw request, so ?goal_id= does not ride along on its
	// own. When it was dropped here the browser sent the scope, consumption
	// never saw it, and the panel silently served the learner's WHOLE concept
	// space: unit tests on both sides passed while the live UI still leaked.
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"companion_id":"f1"}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})

	_, _ = b.GetCompanionMemory(context.Background(), basicAuth(), "f1", "goal-9")

	if cons.lastRawQuery != "goal_id=goal-9" {
		t.Errorf("upstream query=%q; want goal_id=goal-9 (scope dropped at the gateway)", cons.lastRawQuery)
	}
}

func TestGetCompanionMemory_OmitsEmptyGoalScope(t *testing.T) {
	// No goal ⇒ no param at all, so a non-map caller keeps the unscoped read
	// rather than sending goal_id= and tripping the fail-closed 503.
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"companion_id":"f1"}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})

	_, _ = b.GetCompanionMemory(context.Background(), basicAuth(), "f1", "  ")

	if cons.lastRawQuery != "" {
		t.Errorf("upstream query=%q; want empty", cons.lastRawQuery)
	}
}

func TestGetCompanionMemory_HappyPath_PathEscapedCamelCase(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"companion_id":"f1","memory_items":[{"atom_id":"a1"}]}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})

	resp, _ := b.GetCompanionMemory(context.Background(), basicAuth(), "f1", "")

	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.Status)
	}
	if cons.lastPath != "/v1/me/companions/f1/memory" {
		t.Errorf("path=%s want /v1/me/companions/f1/memory", cons.lastPath)
	}
	if cons.lastMethod != http.MethodGet {
		t.Errorf("method=%s want GET", cons.lastMethod)
	}
	m := decodeJSON(t, resp.Body)
	if _, wrapped := m["data"]; wrapped {
		t.Errorf("memory must be unwrapped (no {data:T}); got %v", m)
	}
	if _, ok := m["memoryItems"]; !ok {
		t.Errorf("expected camelCase memoryItems; got %v", m)
	}
	if _, ok := m["memory_items"]; ok {
		t.Errorf("snake_case memory_items leaked: %v", m)
	}
}
