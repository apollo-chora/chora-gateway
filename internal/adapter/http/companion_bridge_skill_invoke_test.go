// companion_bridge_skill_invoke_test.go — CHO-2013 P1.B (R4-4): gateway
// proxies for the skills/{key} subtree —
//
//	POST   /api/v1/me/companions/:id/skills/:key/invoke  → consumption (Skill invoke runner)
//	PUT    /api/v1/me/companions/:id/skills/:key/equip   → consumption (equip)
//	DELETE /api/v1/me/companions/:id/skills/:key/equip   → consumption (unequip)
//
// Unwrapped snake→camel responses (the skills family convention); 4xx
// statuses + codes pass through so the FE can render SKILL_NOT_EQUIPPED /
// SKILL_SLOTS_FULL / insufficient_mana states verbatim.
package httpadapter_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newSkillInvokeConsumptionStub fakes the consumption skills subtree,
// recording the exact downstream method/path/body for assertions.
type skillDownstreamCapture struct {
	method string
	path   string
	body   string
}

func newSkillInvokeConsumptionStub(t *testing.T, capture *skillDownstreamCapture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		capture.method = r.Method
		capture.path = r.URL.Path
		capture.body = string(body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/invoke") && r.Method == http.MethodPost:
			if strings.Contains(r.URL.Path, "not_equipped_skill") {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"code":"SKILL_NOT_EQUIPPED","message":"skill is owned but not equipped"}`))
				return
			}
			// CHO-2016: an ANSWERABLE skill (quiz_me/socratic_drill) returns the
			// extra result_kind discriminator + items[] channel (atom references).
			// The bridge must pass BOTH through, camelCased recursively (incl. the
			// nested item keys), unchanged in shape.
			if strings.Contains(r.URL.Path, "quiz_me") {
				_, _ = w.Write([]byte(`{"skill_key":"quiz_me","reply":"Answer these in the app.","recorded":false,"mana_charged":0,"turn_id":"turn-9","result_kind":"answerable","items":[{"atom_id":"atom-1","title":"Long division","topic":"arithmetic","difficulty":2,"reason":"weak_spot"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"skill_key":"progress_mirror","reply":"You are doing well.","recorded":false,"mana_charged":0,"turn_id":"turn-1"}`))
		case strings.HasSuffix(r.URL.Path, "/equip") && (r.Method == http.MethodPut || r.Method == http.MethodDelete):
			_, _ = w.Write([]byte(`{"companion_id":"f1","skill_grants":["progress_mirror"],"equipped_skills":["progress_mirror"],"skill_slots_unlocked":3,"slots_used":1,"evolution_tier":"adept","growth_stage":2}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCompanionBridge_InvokeSkill_ProxiesPOST(t *testing.T) {
	var cap skillDownstreamCapture
	cons := newSkillInvokeConsumptionStub(t, &cap)
	h := newBridgeHandler(t, cons, newTenancyStub(t))

	w := doReq(t, h, http.MethodPost, "/api/v1/me/companions/f1/skills/progress_mirror/invoke",
		`{"params":{"window":"week"}}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if cap.method != http.MethodPost || cap.path != "/v1/me/companions/f1/skills/progress_mirror/invoke" {
		t.Errorf("downstream = %s %s; want POST /v1/me/companions/f1/skills/progress_mirror/invoke", cap.method, cap.path)
	}
	if !strings.Contains(cap.body, `"window":"week"`) {
		t.Errorf("downstream body = %s; want the params forwarded", cap.body)
	}
	// Unwrapped snake→camel response (skills family convention).
	resp := decodeBody(t, w)
	if resp["skillKey"] != "progress_mirror" {
		t.Errorf("skillKey = %v; body=%s", resp["skillKey"], w.Body.String())
	}
	if resp["manaCharged"].(float64) != 0 || resp["turnId"] != "turn-1" {
		t.Errorf("manaCharged/turnId not camelized: %s", w.Body.String())
	}
	if resp["reply"] != "You are doing well." {
		t.Errorf("reply = %v", resp["reply"])
	}
}

// TestCompanionBridge_InvokeSkill_AnswerableFieldsPassThroughCamelCased confirms
// the CHO-2016 answerable seam survives the bridge: result_kind + items[] (with
// nested atom_id/difficulty/reason) are forwarded and camelCased recursively —
// the generic SnakeToCamelJSON transform needs no typed DTO for the new fields.
func TestCompanionBridge_InvokeSkill_AnswerableFieldsPassThroughCamelCased(t *testing.T) {
	var cap skillDownstreamCapture
	cons := newSkillInvokeConsumptionStub(t, &cap)
	h := newBridgeHandler(t, cons, newTenancyStub(t))

	w := doReq(t, h, http.MethodPost, "/api/v1/me/companions/f1/skills/quiz_me/invoke",
		`{"params":{"scope":"weak"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	resp := decodeBody(t, w)
	if resp["resultKind"] != "answerable" {
		t.Errorf("resultKind = %v, want answerable; body=%s", resp["resultKind"], w.Body.String())
	}
	items, ok := resp["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items not forwarded as a 1-element array: %v; body=%s", resp["items"], w.Body.String())
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item is not an object: %T", items[0])
	}
	if item["atomId"] != "atom-1" {
		t.Errorf("item.atomId = %v, want atom-1 (snake atom_id must camelize)", item["atomId"])
	}
	if item["difficulty"].(float64) != 2 || item["reason"] != "weak_spot" {
		t.Errorf("item difficulty/reason not passed through: %v", item)
	}
	if item["title"] != "Long division" || item["topic"] != "arithmetic" {
		t.Errorf("item title/topic not passed through: %v", item)
	}
}

func TestCompanionBridge_InvokeSkill_ConflictPassesThrough(t *testing.T) {
	var cap skillDownstreamCapture
	cons := newSkillInvokeConsumptionStub(t, &cap)
	h := newBridgeHandler(t, cons, newTenancyStub(t))

	w := doReq(t, h, http.MethodPost, "/api/v1/me/companions/f1/skills/not_equipped_skill/invoke", `{}`)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "SKILL_NOT_EQUIPPED") {
		t.Errorf("409 code not preserved: %s", w.Body.String())
	}
}

func TestCompanionBridge_InvokeSkill_MethodGuard(t *testing.T) {
	var cap skillDownstreamCapture
	cons := newSkillInvokeConsumptionStub(t, &cap)
	h := newBridgeHandler(t, cons, newTenancyStub(t))

	w := doReq(t, h, http.MethodGet, "/api/v1/me/companions/f1/skills/progress_mirror/invoke", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body=%s", w.Code, w.Body.String())
	}
}

func TestCompanionBridge_EquipSkill_PutAndDelete(t *testing.T) {
	var cap skillDownstreamCapture
	cons := newSkillInvokeConsumptionStub(t, &cap)
	h := newBridgeHandler(t, cons, newTenancyStub(t))

	w := doReq(t, h, http.MethodPut, "/api/v1/me/companions/f1/skills/progress_mirror/equip", "")
	if w.Code != http.StatusOK {
		t.Fatalf("PUT equip status = %d, body=%s", w.Code, w.Body.String())
	}
	if cap.method != http.MethodPut || cap.path != "/v1/me/companions/f1/skills/progress_mirror/equip" {
		t.Errorf("downstream = %s %s; want PUT .../skills/progress_mirror/equip", cap.method, cap.path)
	}
	resp := decodeBody(t, w)
	if resp["skillSlotsUnlocked"].(float64) != 3 {
		t.Errorf("loadout not camelized: %s", w.Body.String())
	}

	w = doReq(t, h, http.MethodDelete, "/api/v1/me/companions/f1/skills/progress_mirror/equip", "")
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE equip status = %d, body=%s", w.Code, w.Body.String())
	}
	if cap.method != http.MethodDelete {
		t.Errorf("downstream method = %s; want DELETE", cap.method)
	}

	w = doReq(t, h, http.MethodPost, "/api/v1/me/companions/f1/skills/progress_mirror/equip", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST equip status = %d, want 405", w.Code)
	}
}

func TestCompanionBridge_SkillSubtree_UnknownLeaf404(t *testing.T) {
	var cap skillDownstreamCapture
	cons := newSkillInvokeConsumptionStub(t, &cap)
	h := newBridgeHandler(t, cons, newTenancyStub(t))

	w := doReq(t, h, http.MethodPost, "/api/v1/me/companions/f1/skills/progress_mirror/detonate", `{}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", w.Code, w.Body.String())
	}
}
