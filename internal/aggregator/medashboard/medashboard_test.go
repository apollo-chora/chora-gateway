// medashboard_test.go — RED-phase TDD specs for the A+ dashboard
// composing aggregator (A6 follow-up CHO-1545; Phyllis steps 1b/6).
//
// No downstream serves GET /me/dashboard — this aggregator fans out to
// the REAL endpoints that DO hold each piece of the FE-expected
// DashboardSummary DTO (chora-web/src/app/features/surfaces/aplus/
// dashboard/dashboard.model.ts is the contract):
//
//	userDisplayName      → chora-identity     GET /me            (.display_name)
//	currentStreakDays    → chora-consumption  GET /v1/me/streak  (.count)
//	learnerCourses[]     → chora-consumption  GET /v1/me/learning-paths (.items[])
//	instructorCourses[]  → chora-delivery GET /api/v1/instructors/{gcid}/courses
//	                       (role-gated; see medashboard_instructor_test.go)
//	gcidPillLabel        → synthesised gateway-side (UI label, not domain data)
//
// Graceful degradation (mirrors phyllis.GetAtom): a partial downstream
// failure omits/empties that part — it is NEVER a whole-500. The
// response carries a `partial: true` flag + a `part_errors` map so the
// caller can tell empty-because-failed from empty-because-no-data.
//
// Strict TDD: tests written BEFORE the aggregator implementation.
package medashboard_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/medashboard"
)

// stubBackend records the inbound request + returns a fixed status/body.
type stubBackend struct {
	srv    *httptest.Server
	hdr    http.Header
	path   string
	method string
	calls  int
}

func newStub(t *testing.T, status int, body string) *stubBackend {
	t.Helper()
	sb := &stubBackend{}
	sb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sb.calls++
		sb.hdr = r.Header.Clone()
		sb.path = r.URL.Path
		sb.method = r.Method
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(sb.srv.Close)
	return sb
}

// routingStub answers different bodies per path prefix so a single
// httptest server can stand in for identity + consumption.
func routingStub(t *testing.T, routes map[string]struct {
	status int
	body   string
}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for prefix, resp := range routes {
			if strings.HasPrefix(r.URL.Path, prefix) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(resp.status)
				_, _ = w.Write([]byte(resp.body))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"stub: no route"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sampleAuth() medashboard.AuthCtx {
	return medashboard.AuthCtx{
		Bearer:      "raw-session-jwt",
		Traceparent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		TenantID:    "01970000-0000-7000-8000-0000000000bb",
		GCID:        "01970000-0000-7000-8000-0000000000aa",
	}
}

const (
	meBody = `{"gcid":"01970000-0000-7000-8000-0000000000aa","email":"phyllis@mtm.sg","display_name":"Phyllis Tan","created_at":"2026-01-01T00:00:00Z","auth_methods":["password","google_oidc"],"linked_idps":["google","singpass"]}`

	streakBody = `{"learner_gcid":"01970000-0000-7000-8000-0000000000aa","count":7,"last_activity_at":"2026-05-14T08:00:00Z"}`

	// Both items are COURSE-provenanced (ADR-233 D2). source_type/source_id
	// were added to this fixture when chora-consumption c56292633 began
	// projecting them: both items carry a course_id, so migration 0093's
	// backfill (`SET source_type='course', source_id=course_id WHERE course_id
	// IS NOT NULL`) stamps exactly this. The fixture models the CURRENT wire —
	// a path with NO source_type is the stale-producer case and is covered
	// deliberately, in isolation, by
	// TestGetDashboard_AbsentSourceType_ExcludedAndRecordedLoudly_ADR233_D2.
	learningPathsBody = `{"items":[{"path_id":"path-1","course_id":"course-csm-prep","enrollment_id":"enr-1","source_type":"course","source_id":"course-csm-prep","title":"CSM Prep","atom_ids":["a1","a2","a3"],"current_index":1,"total_atoms":3,"progress_percent":33.3,"completed":false},{"path_id":"path-2","course_id":"course-cspo-ref","source_type":"course","source_id":"course-cspo-ref","title":"CSPO Refresher","atom_ids":["b1","b2"],"current_index":0,"total_atoms":2,"progress_percent":0,"completed":false}]}`
)

// -----------------------------------------------------------------------------
// New / config
// -----------------------------------------------------------------------------

func TestNew_NilWhenAllURLsUnset(t *testing.T) {
	if a := medashboard.New(medashboard.Config{}); a != nil {
		t.Fatal("New with no URLs must return nil so callers can route-skip")
	}
}

func TestNew_NonNilWhenAnyURLSet(t *testing.T) {
	if a := medashboard.New(medashboard.Config{IdentityURL: "http://identity"}); a == nil {
		t.Fatal("New with a URL set must return a non-nil aggregator")
	}
}

// -----------------------------------------------------------------------------
// Happy path — full fan-out composes the FE DashboardSummary DTO
// -----------------------------------------------------------------------------

func TestGetDashboard_HappyPath_ComposesFullDTO(t *testing.T) {
	identity := routingStub(t, map[string]struct {
		status int
		body   string
	}{
		"/me": {http.StatusOK, meBody},
	})
	consumption := routingStub(t, map[string]struct {
		status int
		body   string
	}{
		"/v1/me/streak":         {http.StatusOK, streakBody},
		"/v1/me/learning-paths": {http.StatusOK, learningPathsBody},
		// WS-5b: aggregator now also fans out to /v1/me/companions. Empty
		// roster keeps this test focused on the legacy field assertions
		// without churning happy-path expectations.
		"/v1/me/companions": {http.StatusOK, `{"items":[]}`},
	})
	a := medashboard.New(medashboard.Config{
		IdentityURL:    identity.URL,
		ConsumptionURL: consumption.URL,
		PerCallTimeout: time.Second,
	})

	resp, err := a.GetDashboard(context.Background(), sampleAuth())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.Status)
	}

	var dto map[string]any
	if err := json.Unmarshal(resp.Body, &dto); err != nil {
		t.Fatalf("response body not JSON: %v — body=%s", err, resp.Body)
	}

	// userDisplayName ← chora-identity /me .display_name
	if dto["userDisplayName"] != "Phyllis Tan" {
		t.Errorf("userDisplayName = %v; want Phyllis Tan", dto["userDisplayName"])
	}
	// gcidPillLabel — synthesised gateway-side (UI label).
	if pill, _ := dto["gcidPillLabel"].(string); pill == "" {
		t.Errorf("gcidPillLabel = %q; want a non-empty synthesised UI label", pill)
	}
	// currentStreakDays ← chora-consumption /v1/me/streak .count
	if got, _ := dto["currentStreakDays"].(float64); got != 7 {
		t.Errorf("currentStreakDays = %v; want 7", dto["currentStreakDays"])
	}
	// learnerCourses[] ← chora-consumption /v1/me/learning-paths .items[]
	learnerCourses, ok := dto["learnerCourses"].([]any)
	if !ok {
		t.Fatalf("learnerCourses not an array: %T", dto["learnerCourses"])
	}
	if len(learnerCourses) != 2 {
		t.Fatalf("learnerCourses len = %d; want 2", len(learnerCourses))
	}
	lc0, _ := learnerCourses[0].(map[string]any)
	if lc0["courseId"] != "course-csm-prep" {
		t.Errorf("learnerCourses[0].courseId = %v; want course-csm-prep", lc0["courseId"])
	}
	if lc0["title"] != "CSM Prep" {
		t.Errorf("learnerCourses[0].title = %v; want CSM Prep", lc0["title"])
	}
	if got, _ := lc0["progressPct"].(float64); got < 33 || got > 34 {
		t.Errorf("learnerCourses[0].progressPct = %v; want ~33.3", lc0["progressPct"])
	}
	if got, _ := lc0["remainingAtoms"].(float64); got != 2 {
		// total_atoms 3 − current_index 1 = 2 remaining.
		t.Errorf("learnerCourses[0].remainingAtoms = %v; want 2", lc0["remainingAtoms"])
	}
	// instructorCourses[]: this session holds no instructor role and no
	// DeliveryURL is wired, so the array is empty. NEVER null, since the FE DTO
	// types it as a non-null readonly array.
	ic, ok := dto["instructorCourses"].([]any)
	if !ok {
		t.Fatalf("instructorCourses not an array: %T (must be [], never null)", dto["instructorCourses"])
	}
	if len(ic) != 0 {
		t.Errorf("instructorCourses len = %d; want 0 for a non-instructor session", len(ic))
	}
	// A clean full fan-out → partial=false.
	if p, _ := dto["partial"].(bool); p {
		t.Errorf("partial = %v; want false on a clean full fan-out", p)
	}
}

// -----------------------------------------------------------------------------
// Graceful degradation — partial downstream failure
// -----------------------------------------------------------------------------

// chora-consumption streak 5xx → currentStreakDays defaults to 0, the
// rest of the DTO still composes, partial=true + part_errors records it.
// NEVER a whole-500.
func TestGetDashboard_StreakUpstream5xx_DegradesGracefully(t *testing.T) {
	identity := routingStub(t, map[string]struct {
		status int
		body   string
	}{
		"/me": {http.StatusOK, meBody},
	})
	consumption := routingStub(t, map[string]struct {
		status int
		body   string
	}{
		"/v1/me/streak":         {http.StatusInternalServerError, `{"error":"boom"}`},
		"/v1/me/learning-paths": {http.StatusOK, learningPathsBody},
		"/v1/me/companions":     {http.StatusOK, `{"items":[]}`},
	})
	a := medashboard.New(medashboard.Config{
		IdentityURL:    identity.URL,
		ConsumptionURL: consumption.URL,
		PerCallTimeout: time.Second,
	})

	resp, err := a.GetDashboard(context.Background(), sampleAuth())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d; want 200 — a partial failure must NOT 500", resp.Status)
	}
	var dto map[string]any
	_ = json.Unmarshal(resp.Body, &dto)

	if got, _ := dto["currentStreakDays"].(float64); got != 0 {
		t.Errorf("currentStreakDays = %v; want 0 on streak failure", dto["currentStreakDays"])
	}
	// The healthy parts still render.
	if dto["userDisplayName"] != "Phyllis Tan" {
		t.Errorf("userDisplayName = %v; healthy part must still render", dto["userDisplayName"])
	}
	if lc, _ := dto["learnerCourses"].([]any); len(lc) != 2 {
		t.Errorf("learnerCourses len = %v; healthy part must still render", dto["learnerCourses"])
	}
	if p, _ := dto["partial"].(bool); !p {
		t.Errorf("partial = %v; want true when a part failed", p)
	}
	partErrors, _ := dto["part_errors"].(map[string]any)
	if _, ok := partErrors["currentStreakDays"]; !ok {
		t.Errorf("part_errors = %v; want a currentStreakDays entry", partErrors)
	}
}

// chora-identity /me 5xx → userDisplayName empty, the rest still
// composes, partial=true.
func TestGetDashboard_IdentityUpstream5xx_DegradesGracefully(t *testing.T) {
	identity := newStub(t, http.StatusInternalServerError, `{"error":"identity down"}`)
	consumption := routingStub(t, map[string]struct {
		status int
		body   string
	}{
		"/v1/me/streak":         {http.StatusOK, streakBody},
		"/v1/me/learning-paths": {http.StatusOK, learningPathsBody},
		"/v1/me/companions":     {http.StatusOK, `{"items":[]}`},
	})
	a := medashboard.New(medashboard.Config{
		IdentityURL:    identity.srv.URL,
		ConsumptionURL: consumption.URL,
		PerCallTimeout: time.Second,
	})

	resp, _ := a.GetDashboard(context.Background(), sampleAuth())
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d; want 200 — identity failure must NOT 500", resp.Status)
	}
	var dto map[string]any
	_ = json.Unmarshal(resp.Body, &dto)
	if dn, _ := dto["userDisplayName"].(string); dn != "" {
		t.Errorf("userDisplayName = %q; want empty on identity failure", dn)
	}
	if got, _ := dto["currentStreakDays"].(float64); got != 7 {
		t.Errorf("currentStreakDays = %v; healthy part must still render", dto["currentStreakDays"])
	}
	if p, _ := dto["partial"].(bool); !p {
		t.Errorf("partial = %v; want true when identity failed", p)
	}
}

// chora-consumption learning-paths 5xx → learnerCourses empty array
// (never null), partial=true.
func TestGetDashboard_LearningPathsUpstream5xx_DegradesGracefully(t *testing.T) {
	identity := routingStub(t, map[string]struct {
		status int
		body   string
	}{
		"/me": {http.StatusOK, meBody},
	})
	consumption := routingStub(t, map[string]struct {
		status int
		body   string
	}{
		"/v1/me/streak":         {http.StatusOK, streakBody},
		"/v1/me/learning-paths": {http.StatusInternalServerError, `{"error":"boom"}`},
		"/v1/me/companions":     {http.StatusOK, `{"items":[]}`},
	})
	a := medashboard.New(medashboard.Config{
		IdentityURL:    identity.URL,
		ConsumptionURL: consumption.URL,
		PerCallTimeout: time.Second,
	})

	resp, _ := a.GetDashboard(context.Background(), sampleAuth())
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.Status)
	}
	var dto map[string]any
	_ = json.Unmarshal(resp.Body, &dto)
	lc, ok := dto["learnerCourses"].([]any)
	if !ok {
		t.Fatalf("learnerCourses must be [] (never null) even on failure: %T", dto["learnerCourses"])
	}
	if len(lc) != 0 {
		t.Errorf("learnerCourses len = %d; want 0 on failure", len(lc))
	}
	if p, _ := dto["partial"].(bool); !p {
		t.Errorf("partial = %v; want true", p)
	}
}

// All downstreams down → still 200 with a fully-degraded (empty) DTO +
// partial=true. The dashboard NEVER hard-fails — it is the A+ landing
// surface.
func TestGetDashboard_AllUpstreamsDown_Still200(t *testing.T) {
	a := medashboard.New(medashboard.Config{
		IdentityURL:    "http://127.0.0.1:1",
		ConsumptionURL: "http://127.0.0.1:1",
		PerCallTimeout: 300 * time.Millisecond,
	})
	resp, _ := a.GetDashboard(context.Background(), sampleAuth())
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d; want 200 even when all downstreams are down", resp.Status)
	}
	var dto map[string]any
	if err := json.Unmarshal(resp.Body, &dto); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if p, _ := dto["partial"].(bool); !p {
		t.Errorf("partial = %v; want true when everything failed", p)
	}
	if _, ok := dto["learnerCourses"].([]any); !ok {
		t.Errorf("learnerCourses must still be [] — never null")
	}
	if _, ok := dto["instructorCourses"].([]any); !ok {
		t.Errorf("instructorCourses must still be [] — never null")
	}
}

// Only IdentityURL configured (ConsumptionURL unset) → the streak +
// learnerCourses parts degrade to empty (the gateway never builds a call
// against an empty URL), identity still composes, partial=true. This is
// the unconfigured-downstream degradation path.
func TestGetDashboard_ConsumptionURLUnset_DegradesGracefully(t *testing.T) {
	identity := newStub(t, http.StatusOK, meBody)
	a := medashboard.New(medashboard.Config{
		IdentityURL:    identity.srv.URL,
		PerCallTimeout: time.Second,
	})
	if a == nil {
		t.Fatal("aggregator nil — IdentityURL set should have wired it")
	}

	resp, _ := a.GetDashboard(context.Background(), sampleAuth())
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d; want 200 — unset ConsumptionURL must NOT 500", resp.Status)
	}
	var dto map[string]any
	_ = json.Unmarshal(resp.Body, &dto)
	if dto["userDisplayName"] != "Phyllis Tan" {
		t.Errorf("userDisplayName = %v; identity part must still render", dto["userDisplayName"])
	}
	if got, _ := dto["currentStreakDays"].(float64); got != 0 {
		t.Errorf("currentStreakDays = %v; want 0 when ConsumptionURL unset", dto["currentStreakDays"])
	}
	if lc, ok := dto["learnerCourses"].([]any); !ok || len(lc) != 0 {
		t.Errorf("learnerCourses = %v; want [] when ConsumptionURL unset", dto["learnerCourses"])
	}
	if p, _ := dto["partial"].(bool); !p {
		t.Errorf("partial = %v; want true when ConsumptionURL unset", p)
	}
	partErrors, _ := dto["part_errors"].(map[string]any)
	if partErrors["currentStreakDays"] != "upstream_unavailable" {
		t.Errorf("part_errors[currentStreakDays] = %v; want upstream_unavailable", partErrors["currentStreakDays"])
	}
}

// known_gaps carries the genuine structural gaps and must NEVER flip `partial`
// (a permanent gap is not a transient failure). The instructorCourses entry
// used to be one of them; it was retired when the by-instructor endpoint was
// wired, and its replacement assertions live in medashboard_instructor_test.go.
func TestGetDashboard_KnownGapsNeverFlipPartial(t *testing.T) {
	identity := routingStub(t, map[string]struct {
		status int
		body   string
	}{
		"/me": {http.StatusOK, meBody},
	})
	consumption := routingStub(t, map[string]struct {
		status int
		body   string
	}{
		"/v1/me/streak":         {http.StatusOK, streakBody},
		"/v1/me/learning-paths": {http.StatusOK, learningPathsBody},
		"/v1/me/companions":     {http.StatusOK, `{"items":[]}`},
	})
	a := medashboard.New(medashboard.Config{
		IdentityURL:    identity.URL,
		ConsumptionURL: consumption.URL,
		PerCallTimeout: time.Second,
	})
	resp, _ := a.GetDashboard(context.Background(), sampleAuth())
	var dto map[string]any
	_ = json.Unmarshal(resp.Body, &dto)

	knownGaps, ok := dto["known_gaps"].(map[string]any)
	if !ok {
		t.Fatalf("known_gaps missing or wrong type: %T", dto["known_gaps"])
	}
	gap, _ := knownGaps["learnerCourses.retention_state"].(string)
	if !strings.Contains(gap, "no_downstream_source") {
		t.Errorf("known_gaps[learnerCourses.retention_state] = %q; want a no_downstream_source reason", gap)
	}
	// Clean fan-out of the wireable parts means partial=false despite the gaps.
	if p, _ := dto["partial"].(bool); p {
		t.Errorf("partial = %v; want false, a structural gap must NOT flip partial", p)
	}
}

// -----------------------------------------------------------------------------
// Header propagation — downstreams require X-Tenant-Id + lowercase gcid
// -----------------------------------------------------------------------------

func TestGetDashboard_StampsMeshHeadersOnFanOut(t *testing.T) {
	var streakHdr http.Header
	consumption := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/me/streak") {
			streakHdr = r.Header.Clone()
		}
		w.WriteHeader(http.StatusOK)
		if strings.HasPrefix(r.URL.Path, "/v1/me/learning-paths") {
			_, _ = w.Write([]byte(`{"items":[]}`))
			return
		}
		_, _ = w.Write([]byte(streakBody))
	}))
	t.Cleanup(consumption.Close)
	identity := newStub(t, http.StatusOK, meBody)

	a := medashboard.New(medashboard.Config{
		IdentityURL:    identity.srv.URL,
		ConsumptionURL: consumption.URL,
		PerCallTimeout: time.Second,
	})
	_, _ = a.GetDashboard(context.Background(), sampleAuth())

	// chora-consumption requireContext reads X-Tenant-Id + lowercase gcid.
	if streakHdr.Get("X-Tenant-Id") != sampleAuth().TenantID {
		t.Errorf("X-Tenant-Id = %q; want %q", streakHdr.Get("X-Tenant-Id"), sampleAuth().TenantID)
	}
	if streakHdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("lowercase gcid header = %q; want %q", streakHdr.Get("gcid"), sampleAuth().GCID)
	}
	if streakHdr.Get("Authorization") != "Bearer raw-session-jwt" {
		t.Errorf("Authorization = %q", streakHdr.Get("Authorization"))
	}
	if streakHdr.Get("traceparent") != sampleAuth().Traceparent {
		t.Errorf("traceparent = %q; want propagated", streakHdr.Get("traceparent"))
	}
}

// -----------------------------------------------------------------------------
// LoadConfigFromEnv — reads the SAME SVC_*_URL vars the other aggregators use
// -----------------------------------------------------------------------------

func TestLoadConfigFromEnv_ReadsSvcURLs(t *testing.T) {
	t.Setenv("SVC_IDENTITY_URL", "http://identity:8080")
	t.Setenv("SVC_CONSUMPTION_URL", "http://consumption:8080")
	cfg := medashboard.LoadConfigFromEnv()
	if cfg.IdentityURL != "http://identity:8080" {
		t.Errorf("IdentityURL = %q", cfg.IdentityURL)
	}
	if cfg.ConsumptionURL != "http://consumption:8080" {
		t.Errorf("ConsumptionURL = %q", cfg.ConsumptionURL)
	}
	if cfg.PerCallTimeout == 0 {
		t.Error("PerCallTimeout default not applied")
	}
}

// -----------------------------------------------------------------------------
// WS-5b — streak structured summary + companions[] + learnerCourses[].retention_state
// -----------------------------------------------------------------------------

const (
	// streakBodyExt mirrors the chora-consumption streakResp with both
	// `count` and `last_activity_at` populated — WS-5b folds these into a
	// nested `streak` block per the FE StreakSummary contract.
	streakBodyExt = `{"learner_gcid":"01970000-0000-7000-8000-0000000000aa","count":12,"last_activity_at":"2026-05-25T07:30:00Z"}`

	// companionsBody mirrors chora-consumption `GET /v1/me/companions`
	// rosterEnvelope. Each item is an instanceResp with inline growth_state
	// (per debt #44 / A24 / B5 enrichment). Item[0] (Eira) has a REVEALED
	// breed (current_breed:"owl") over a "PSLE Science" specialization; item[1]
	// (Blaze) has NO revealed breed (current_breed:"") so species must fall
	// back to its "Sec 3 Math" specialization.
	companionsBody = `{"items":[
		{"companion_id":"01957c8c-1111-7000-aaaa-1111aaaa1111","tenant_id":"t-1","owner_gcid":"01970000-0000-7000-8000-0000000000aa","name":"Eira","specialization":"PSLE Science","evolution_tier":"stage_2","skill_slots_unlocked":2,"memory_context_capacity":4,"skill_grants":[],"configured_rules":{},"memory_bank_app_name":"companion:01957c8c-1111-7000-aaaa-1111aaaa1111","created_at":"2026-05-01T00:00:00.000000Z","updated_at":"2026-05-01T00:00:00.000000Z","growth_state":{"stage":2,"stage_name":"fledgling","exp":120,"exp_to_next_stage":80,"current_breed":"owl","breed_revealed_at":"2026-05-01T00:00:00.000000Z","effective_llm_tier":"","resonant_atom_id":"","aha_moment_consumed":false,"aha_moment_active_until":null}},
		{"companion_id":"01957c8c-2222-7000-bbbb-2222bbbb2222","tenant_id":"t-1","owner_gcid":"01970000-0000-7000-8000-0000000000aa","name":"Blaze","specialization":"Sec 3 Math","evolution_tier":"stage_3","skill_slots_unlocked":3,"memory_context_capacity":6,"skill_grants":[],"configured_rules":{},"memory_bank_app_name":"companion:01957c8c-2222-7000-bbbb-2222bbbb2222","created_at":"2026-05-02T00:00:00.000000Z","updated_at":"2026-05-02T00:00:00.000000Z","growth_state":{"stage":3,"stage_name":"apprentice","exp":210,"exp_to_next_stage":40,"current_breed":"","breed_revealed_at":null,"effective_llm_tier":"","resonant_atom_id":"","aha_moment_consumed":false,"aha_moment_active_until":null}}
	]}`
)

// WS-5b: GET /v1/me/streak → BFF folds into a structured `streak` block
// (current_streak_days + last_completion_at) alongside the legacy flat
// currentStreakDays scalar. The flat scalar is preserved for backwards-
// compat per the FE WS-5 fallback contract.
func TestGetDashboard_StreakBlockMirrorsUpstreamWS5b(t *testing.T) {
	identity := routingStub(t, map[string]struct {
		status int
		body   string
	}{
		"/me": {http.StatusOK, meBody},
	})
	consumption := routingStub(t, map[string]struct {
		status int
		body   string
	}{
		"/v1/me/streak":         {http.StatusOK, streakBodyExt},
		"/v1/me/learning-paths": {http.StatusOK, learningPathsBody},
		"/v1/me/companions":     {http.StatusOK, companionsBody},
	})
	a := medashboard.New(medashboard.Config{
		IdentityURL:    identity.URL,
		ConsumptionURL: consumption.URL,
		PerCallTimeout: time.Second,
	})
	resp, _ := a.GetDashboard(context.Background(), sampleAuth())
	var dto map[string]any
	if err := json.Unmarshal(resp.Body, &dto); err != nil {
		t.Fatalf("body not JSON: %v — body=%s", err, resp.Body)
	}

	streak, ok := dto["streak"].(map[string]any)
	if !ok {
		t.Fatalf("streak block missing or wrong type: %T", dto["streak"])
	}
	if got, _ := streak["current_streak_days"].(float64); got != 12 {
		t.Errorf("streak.current_streak_days = %v; want 12", streak["current_streak_days"])
	}
	if got, _ := streak["last_completion_at"].(string); got != "2026-05-25T07:30:00Z" {
		t.Errorf("streak.last_completion_at = %q; want 2026-05-25T07:30:00Z", got)
	}
	// Flat scalar mirrors the WS-5 fallback contract.
	if got, _ := dto["currentStreakDays"].(float64); got != 12 {
		t.Errorf("currentStreakDays = %v; want 12 (mirrors streak.current_streak_days)", got)
	}
	if p, _ := dto["partial"].(bool); p {
		t.Errorf("partial = %v; want false on clean fan-out", p)
	}
}

// WS-5b: GET /v1/me/companions → BFF projects each instanceResp into a
// CompanionRosterItem (companion_id, name, species, subject, evolution_level,
// stage_label). species ← growth_state.current_breed (the revealed breed),
// falling back to specialization when the breed is not yet revealed;
// subject ← specialization always.
func TestGetDashboard_CompanionsBlockProjectsRosterWS5b(t *testing.T) {
	identity := routingStub(t, map[string]struct {
		status int
		body   string
	}{
		"/me": {http.StatusOK, meBody},
	})
	consumption := routingStub(t, map[string]struct {
		status int
		body   string
	}{
		"/v1/me/streak":         {http.StatusOK, streakBody},
		"/v1/me/learning-paths": {http.StatusOK, learningPathsBody},
		"/v1/me/companions":     {http.StatusOK, companionsBody},
	})
	a := medashboard.New(medashboard.Config{
		IdentityURL:    identity.URL,
		ConsumptionURL: consumption.URL,
		PerCallTimeout: time.Second,
	})
	resp, _ := a.GetDashboard(context.Background(), sampleAuth())
	var dto map[string]any
	_ = json.Unmarshal(resp.Body, &dto)

	companions, ok := dto["companions"].([]any)
	if !ok {
		t.Fatalf("companions missing or wrong type: %T", dto["companions"])
	}
	if len(companions) != 2 {
		t.Fatalf("companions len = %d; want 2", len(companions))
	}
	f0, _ := companions[0].(map[string]any)
	if f0["companion_id"] != "01957c8c-1111-7000-aaaa-1111aaaa1111" {
		t.Errorf("companions[0].companion_id = %v", f0["companion_id"])
	}
	if f0["name"] != "Eira" {
		t.Errorf("companions[0].name = %v; want Eira", f0["name"])
	}
	// Eira has a REVEALED breed — species is the breed (owl), NOT the
	// specialization. subject carries the specialization axis for topic-match.
	if f0["species"] != "owl" {
		t.Errorf("companions[0].species = %v; want owl (← growth_state.current_breed, the revealed breed — NOT the specialization)", f0["species"])
	}
	if f0["subject"] != "PSLE Science" {
		t.Errorf("companions[0].subject = %v; want PSLE Science (← specialization, for topic-matched Ask-Companion)", f0["subject"])
	}
	if got, _ := f0["evolution_level"].(float64); got != 2 {
		t.Errorf("companions[0].evolution_level = %v; want 2 (from growth_state.stage)", got)
	}
	if f0["stage_label"] != "fledgling" {
		t.Errorf("companions[0].stage_label = %v; want fledgling", f0["stage_label"])
	}
	f1, _ := companions[1].(map[string]any)
	if f1["name"] != "Blaze" {
		t.Errorf("companions[1].name = %v; want Blaze", f1["name"])
	}
	// Blaze has NO revealed breed (current_breed:"") → species FALLS BACK to
	// the specialization so the FE keeps a stable icon axis.
	if f1["species"] != "Sec 3 Math" {
		t.Errorf("companions[1].species = %v; want Sec 3 Math (fallback to specialization when breed unrevealed)", f1["species"])
	}
	if f1["subject"] != "Sec 3 Math" {
		t.Errorf("companions[1].subject = %v; want Sec 3 Math (← specialization)", f1["subject"])
	}
}

// WS-5b: chora-consumption /v1/me/companions 5xx → empty companions[]
// (NEVER null) + partial=true + part_errors records the failure.
func TestGetDashboard_CompanionsUpstream5xx_DegradesGracefullyWS5b(t *testing.T) {
	identity := routingStub(t, map[string]struct {
		status int
		body   string
	}{
		"/me": {http.StatusOK, meBody},
	})
	consumption := routingStub(t, map[string]struct {
		status int
		body   string
	}{
		"/v1/me/streak":         {http.StatusOK, streakBody},
		"/v1/me/learning-paths": {http.StatusOK, learningPathsBody},
		"/v1/me/companions":     {http.StatusInternalServerError, `{"error":"boom"}`},
	})
	a := medashboard.New(medashboard.Config{
		IdentityURL:    identity.URL,
		ConsumptionURL: consumption.URL,
		PerCallTimeout: time.Second,
	})
	resp, _ := a.GetDashboard(context.Background(), sampleAuth())
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d; want 200 — companions failure must NOT 500", resp.Status)
	}
	var dto map[string]any
	_ = json.Unmarshal(resp.Body, &dto)
	companions, ok := dto["companions"].([]any)
	if !ok {
		t.Fatalf("companions must be [] (never null) on failure: %T", dto["companions"])
	}
	if len(companions) != 0 {
		t.Errorf("companions len = %d; want 0 on failure", len(companions))
	}
	if p, _ := dto["partial"].(bool); !p {
		t.Errorf("partial = %v; want true when companions failed", p)
	}
	partErrors, _ := dto["part_errors"].(map[string]any)
	if _, ok := partErrors["companions"]; !ok {
		t.Errorf("part_errors = %v; want a companions entry", partErrors)
	}
}

// WS-5b: pre-hatch / empty roster — chora-consumption returns
// items:[] → companions is an empty array (never null) + partial=false
// (no failure occurred). This is the Mystery-Egg-only state.
func TestGetDashboard_CompanionsEmpty_NoFailureWS5b(t *testing.T) {
	identity := routingStub(t, map[string]struct {
		status int
		body   string
	}{
		"/me": {http.StatusOK, meBody},
	})
	consumption := routingStub(t, map[string]struct {
		status int
		body   string
	}{
		"/v1/me/streak":         {http.StatusOK, streakBody},
		"/v1/me/learning-paths": {http.StatusOK, learningPathsBody},
		"/v1/me/companions":     {http.StatusOK, `{"items":[]}`},
	})
	a := medashboard.New(medashboard.Config{
		IdentityURL:    identity.URL,
		ConsumptionURL: consumption.URL,
		PerCallTimeout: time.Second,
	})
	resp, _ := a.GetDashboard(context.Background(), sampleAuth())
	var dto map[string]any
	_ = json.Unmarshal(resp.Body, &dto)
	companions, ok := dto["companions"].([]any)
	if !ok {
		t.Fatalf("companions must be [] (never null) on empty roster: %T", dto["companions"])
	}
	if len(companions) != 0 {
		t.Errorf("companions len = %d; want 0 on empty roster", len(companions))
	}
	if p, _ := dto["partial"].(bool); p {
		t.Errorf("partial = %v; want false when companions empty but call succeeded", p)
	}
	partErrors, _ := dto["part_errors"].(map[string]any)
	if _, ok := partErrors["companions"]; ok {
		t.Errorf("part_errors[companions] should be absent on a clean empty response: %v", partErrors)
	}
}

// WS-5b: learnerCourses[].retention_state — there is no chora-consumption
// retention endpoint TODAY (WS-5c-BE-A1 follow-up). The aggregator MUST
// omit `retention_state` from each learner-course entry (NOT fabricate a
// value, NOT emit "unknown") so the FE explicitly renders the
// `unknown` dot from its own fallback per the WS-5 contract. This is
// the graceful-MISSING path per `feedback_no_stubs_real_wiring`.
func TestGetDashboard_RetentionState_OmittedWhenUpstreamMissingWS5b(t *testing.T) {
	identity := routingStub(t, map[string]struct {
		status int
		body   string
	}{
		"/me": {http.StatusOK, meBody},
	})
	consumption := routingStub(t, map[string]struct {
		status int
		body   string
	}{
		"/v1/me/streak":         {http.StatusOK, streakBody},
		"/v1/me/learning-paths": {http.StatusOK, learningPathsBody},
		"/v1/me/companions":     {http.StatusOK, `{"items":[]}`},
		// No /v1/me/retention-states route exists (graceful MISSING).
	})
	a := medashboard.New(medashboard.Config{
		IdentityURL:    identity.URL,
		ConsumptionURL: consumption.URL,
		PerCallTimeout: time.Second,
	})
	resp, _ := a.GetDashboard(context.Background(), sampleAuth())
	var dto map[string]any
	_ = json.Unmarshal(resp.Body, &dto)
	courses, _ := dto["learnerCourses"].([]any)
	if len(courses) == 0 {
		t.Fatal("learnerCourses empty — needed to assert retention_state behaviour")
	}
	c0, _ := courses[0].(map[string]any)
	if _, present := c0["retention_state"]; present {
		t.Errorf("learnerCourses[0].retention_state = %v; want field OMITTED when upstream MISSING — never fabricate", c0["retention_state"])
	}
	// Partial stays false — graceful MISSING is a structural state, NOT a
	// transient failure. (Mirrors the instructorCourses known_gaps pattern.)
	if p, _ := dto["partial"].(bool); p {
		t.Errorf("partial = %v; want false — graceful-MISSING is not a transient failure", p)
	}
}

// WS-5b: companion fan-out propagates mesh-trust headers (X-Tenant-Id +
// lowercase gcid + Authorization + traceparent) — chora-consumption's
// requireContext requires X-Tenant-Id + lowercase gcid (returns "gcid
// required" without it), and trace context MUST propagate per CLAUDE.md
// §6 cross-cutting rule.
func TestGetDashboard_CompanionsStampsMeshHeadersWS5b(t *testing.T) {
	var companionsHdr http.Header
	consumption := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/me/companions") {
			companionsHdr = r.Header.Clone()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"items":[]}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		if strings.HasPrefix(r.URL.Path, "/v1/me/learning-paths") {
			_, _ = w.Write([]byte(`{"items":[]}`))
			return
		}
		_, _ = w.Write([]byte(streakBody))
	}))
	t.Cleanup(consumption.Close)
	identity := newStub(t, http.StatusOK, meBody)

	a := medashboard.New(medashboard.Config{
		IdentityURL:    identity.srv.URL,
		ConsumptionURL: consumption.URL,
		PerCallTimeout: time.Second,
	})
	_, _ = a.GetDashboard(context.Background(), sampleAuth())

	if companionsHdr.Get("X-Tenant-Id") != sampleAuth().TenantID {
		t.Errorf("companions X-Tenant-Id = %q; want %q", companionsHdr.Get("X-Tenant-Id"), sampleAuth().TenantID)
	}
	if companionsHdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("companions lowercase gcid header = %q; want %q", companionsHdr.Get("gcid"), sampleAuth().GCID)
	}
	if companionsHdr.Get("Authorization") != "Bearer raw-session-jwt" {
		t.Errorf("companions Authorization = %q", companionsHdr.Get("Authorization"))
	}
	if companionsHdr.Get("traceparent") != sampleAuth().Traceparent {
		t.Errorf("companions traceparent = %q; want propagated", companionsHdr.Get("traceparent"))
	}
}

// WS-5b: when ConsumptionURL is unset, the companions + streak block are
// degraded to absent (NEVER fabricated). part_errors records the
// upstream_unavailable reason on each.
func TestGetDashboard_ConsumptionURLUnset_CompanionsAndStreakAbsentWS5b(t *testing.T) {
	identity := newStub(t, http.StatusOK, meBody)
	a := medashboard.New(medashboard.Config{
		IdentityURL:    identity.srv.URL,
		PerCallTimeout: time.Second,
	})
	resp, _ := a.GetDashboard(context.Background(), sampleAuth())
	var dto map[string]any
	_ = json.Unmarshal(resp.Body, &dto)
	companions, ok := dto["companions"].([]any)
	if !ok {
		t.Fatalf("companions must be [] (never null) when ConsumptionURL unset: %T", dto["companions"])
	}
	if len(companions) != 0 {
		t.Errorf("companions len = %d; want 0 when ConsumptionURL unset", len(companions))
	}
	if _, ok := dto["streak"]; ok {
		t.Errorf("streak block should be ABSENT when ConsumptionURL unset (never fabricate): %v", dto["streak"])
	}
	partErrors, _ := dto["part_errors"].(map[string]any)
	if partErrors["companions"] != "upstream_unavailable" {
		t.Errorf("part_errors[companions] = %v; want upstream_unavailable", partErrors["companions"])
	}
}
