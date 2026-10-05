// companionbridge_residual_test.go — residual fan-out coverage for the
// companionbridge methods the original TDD suite never reached: the skill
// life-cycle (list/grant/equip/invoke), the ceremony edge-scout pair,
// proofing-test start + companion retire, the rituals surface, and the bare
// instance GET. All are single-downstream proxies to chora-consumption that
// stamp mesh headers + translate camelCase bodies where documented.
package companionbridge_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/companionbridge"
)

func bridgeWith(consURL string) *companionbridge.Bridge {
	return companionbridge.New(companionbridge.Config{
		ConsumptionURL: consURL,
		PerCallTimeout: time.Second,
	})
}

func TestGetCompanionInstance_ProxiesUnwrapped(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"companion_id":"f1","display_name":"Pip","growth_stage":2}`))
	})
	b := bridgeWith(cons.URL)
	resp, err := b.GetCompanionInstance(context.Background(), basicAuth(), "f1")
	if err != nil {
		t.Fatalf("GetCompanionInstance: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.Status)
	}
	if cons.lastPath != "/v1/me/companions/f1" {
		t.Errorf("path=%s want /v1/me/companions/f1", cons.lastPath)
	}
	if cons.lastMethod != http.MethodGet {
		t.Errorf("method=%s want GET", cons.lastMethod)
	}
}

func TestListCompanionSkills_Proxies(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"companion_id":"f1","skill_grants":[],"skill_slots_unlocked":3}`))
	})
	b := bridgeWith(cons.URL)
	resp, _ := b.ListCompanionSkills(context.Background(), basicAuth(), "f1")
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.Status)
	}
	if cons.lastPath != "/v1/me/companions/f1/skills" {
		t.Errorf("path=%s", cons.lastPath)
	}
}

func TestGrantCompanionSkill_TranslatesBody(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"skill_key":"memory_boost","granted":true}`))
	})
	b := bridgeWith(cons.URL)
	resp, _ := b.GrantCompanionSkill(context.Background(), basicAuth(), "f1",
		[]byte(`{"skillId":"memory_boost"}`))
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.Status)
	}
	if cons.lastMethod != http.MethodPost {
		t.Errorf("method=%s want POST", cons.lastMethod)
	}
	// camels→snake translation on the outbound body.
	if string(cons.lastBody) != `{"skill_id":"memory_boost"}` {
		t.Errorf("body=%s want camel→snake translated", string(cons.lastBody))
	}
}

func TestSetCompanionSkillEquipped_PutAndDelete(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"skill_key":"memory_boost","equipped":true}`))
	})
	b := bridgeWith(cons.URL)

	// equip → PUT
	_, err := b.SetCompanionSkillEquipped(context.Background(), basicAuth(), "f1", "memory_boost", true)
	if err != nil {
		t.Fatalf("equip: %v", err)
	}
	if cons.lastMethod != http.MethodPut || cons.lastPath != "/v1/me/companions/f1/skills/memory_boost/equip" {
		t.Errorf("equip → %s %s", cons.lastMethod, cons.lastPath)
	}

	// unequip → DELETE
	_, err = b.SetCompanionSkillEquipped(context.Background(), basicAuth(), "f1", "memory_boost", false)
	if err != nil {
		t.Fatalf("unequip: %v", err)
	}
	if cons.lastMethod != http.MethodDelete {
		t.Errorf("unequip method=%s want DELETE", cons.lastMethod)
	}
}

func TestInvokeCompanionSkill_PostsTranslatedBody(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"skill_key":"memory_boost","reply":"done","mana_charged":5}`))
	})
	b := bridgeWith(cons.URL)
	resp, _ := b.InvokeCompanionSkill(context.Background(), basicAuth(), "f1", "memory_boost",
		[]byte(`{"params":{"topic":"math"}}`))
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.Status)
	}
	if cons.lastPath != "/v1/me/companions/f1/skills/memory_boost/invoke" {
		t.Errorf("path=%s", cons.lastPath)
	}
	if string(cons.lastBody) != `{"params":{"topic":"math"}}` {
		t.Errorf("body=%s; invoke params are single-word, translation must be a no-op", string(cons.lastBody))
	}
}

func TestCeremonyEdgeScout_ProposeAndConfirm(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"edge_scout":{"runs":3}}`))
	})
	b := bridgeWith(cons.URL)

	_, err := b.ProposeCeremonyEdgeScout(context.Background(), basicAuth(), "f1", []byte(`{"skillId":"x"}`))
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if cons.lastPath != "/v1/me/companions/f1/ceremony/edge-scout" {
		t.Errorf("propose path=%s", cons.lastPath)
	}

	_, err = b.ConfirmCeremonyEdgeScout(context.Background(), basicAuth(), "f1", []byte(`{"resultId":"r1"}`))
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if cons.lastPath != "/v1/me/companions/f1/ceremony/edge-scout/confirm" {
		t.Errorf("confirm path=%s", cons.lastPath)
	}
}

func TestStartProofingTest_PostsBody(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"test_id":"pt-1"}`))
	})
	b := bridgeWith(cons.URL)
	resp, _ := b.StartProofingTest(context.Background(), basicAuth(), "f1", []byte(`{"scope":"math"}`))
	if resp.Status != http.StatusCreated {
		t.Fatalf("status=%d want 201", resp.Status)
	}
	if cons.lastPath != "/v1/me/companions/f1/proofing-test" {
		t.Errorf("path=%s", cons.lastPath)
	}
}

func TestRetireCompanion_PostsNoBody(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"companion_id":"f1","retired":true}`))
	})
	b := bridgeWith(cons.URL)
	resp, _ := b.RetireCompanion(context.Background(), basicAuth(), "f1")
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.Status)
	}
	if cons.lastPath != "/v1/me/companions/f1/retire" {
		t.Errorf("path=%s", cons.lastPath)
	}
	if len(cons.lastBody) != 0 {
		t.Errorf("body=%s want empty (no request body)", string(cons.lastBody))
	}
}

func TestRituals_ListCreateGetPublish(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"rituals":[]}`))
	})
	b := bridgeWith(cons.URL)

	// GET list
	_, err := b.ListRituals(context.Background(), basicAuth(), "f1")
	if err != nil {
		t.Fatalf("ListRituals: %v", err)
	}
	if cons.lastPath != "/v1/me/companions/f1/rituals" || cons.lastMethod != http.MethodGet {
		t.Errorf("ListRituals → %s %s", cons.lastMethod, cons.lastPath)
	}

	// POST create
	_, err = b.CreateRitual(context.Background(), basicAuth(), "f1", []byte(`{"name":"Morning"}`))
	if err != nil {
		t.Fatalf("CreateRitual: %v", err)
	}
	if cons.lastMethod != http.MethodPost {
		t.Errorf("CreateRitual method=%s want POST", cons.lastMethod)
	}

	// GET single
	_, err = b.GetRitual(context.Background(), basicAuth(), "f1", "rit-1")
	if err != nil {
		t.Fatalf("GetRitual: %v", err)
	}
	if cons.lastPath != "/v1/me/companions/f1/rituals/rit-1" {
		t.Errorf("GetRitual path=%s", cons.lastPath)
	}

	// POST publish
	_, err = b.PublishRitual(context.Background(), basicAuth(), "f1", "rit-1", []byte(`{}`))
	if err != nil {
		t.Fatalf("PublishRitual: %v", err)
	}
	if cons.lastPath != "/v1/me/companions/f1/rituals/rit-1/publish" {
		t.Errorf("PublishRitual path=%s", cons.lastPath)
	}
}

// TestRitualRunAndRuns — the run runner + runs list complete the rituals
// surface (routes already proxied elsewhere; pins path correctness).
func TestRitualRunAndRuns(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"runs":[]}`))
	})
	b := bridgeWith(cons.URL)

	_, err := b.RunRitual(context.Background(), basicAuth(), "f1", "rit-1", []byte(`{"focus":"math"}`))
	if err != nil {
		t.Fatalf("RunRitual: %v", err)
	}
	if cons.lastPath != "/v1/me/companions/f1/rituals/rit-1/run" {
		t.Errorf("RunRitual path=%s", cons.lastPath)
	}

	_, err = b.ListRitualRuns(context.Background(), basicAuth(), "f1", "rit-1")
	if err != nil {
		t.Fatalf("ListRitualRuns: %v", err)
	}
	if cons.lastPath != "/v1/me/companions/f1/rituals/rit-1/runs" {
		t.Errorf("ListRitualRuns path=%s", cons.lastPath)
	}
}
