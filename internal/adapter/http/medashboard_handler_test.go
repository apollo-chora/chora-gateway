// medashboard_handler_test.go — HTTP route binding tests for the A+
// dashboard composing aggregator route (A6 follow-up CHO-1545; Phyllis
// steps 1b/6).
//
//	GET /api/me/dashboard → medashboard.Aggregator.GetDashboard
//
// Strict TDD: tests written BEFORE the handler implementation.
package httpadapter_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/medashboard"
)

// newMeDashboardMux wires the medashboard mux with both downstreams
// pointed at one routing stub.
func newMeDashboardMux(t *testing.T, stub *httptest.Server) http.Handler {
	t.Helper()
	agg := medashboard.New(medashboard.Config{
		IdentityURL:    stub.URL,
		ConsumptionURL: stub.URL,
		PerCallTimeout: time.Second,
	})
	if agg == nil {
		t.Fatal("aggregator nil — stub URL should have wired it")
	}
	return httpadapter.NewMeDashboardMux(agg)
}

// meDashboardStub answers per-path so a single server stands in for
// chora-identity (/me) + chora-consumption (/v1/me/*).
func meDashboardStub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me":
			_, _ = w.Write([]byte(`{"display_name":"Phyllis Tan"}`))
		case r.URL.Path == "/v1/me/streak":
			_, _ = w.Write([]byte(`{"count":7,"last_activity_at":"2026-05-26T09:00:00Z"}`))
		case r.URL.Path == "/v1/me/learning-paths":
			// source_type "course" — ADR-233 D2 provenance, projected onto the
			// wire by chora-consumption c56292633. The path carries a course_id,
			// so migration 0093's backfill stamps exactly this. The aggregator
			// admits ONLY source_type=="course" into learnerCourses[], so a
			// fixture without it models a wire that no longer exists and would
			// (correctly) yield an empty list.
			_, _ = w.Write([]byte(`{"items":[{"course_id":"c-1","source_type":"course","source_id":"c-1","title":"CSM Prep","current_index":1,"total_atoms":3,"progress_percent":33.3}]}`))
		case r.URL.Path == "/v1/me/companions":
			// WS-5b: chora-consumption rosterEnvelope with one Companion +
			// inline growth_state.
			_, _ = w.Write([]byte(`{"items":[{"companion_id":"01957c8c-1111-7000-aaaa-1111aaaa1111","name":"Eira","specialization":"fox","growth_state":{"stage":2,"stage_name":"fledgling"}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func doMeDashboardReq(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), method, path, nil)
	r.Header.Set("Authorization", "Bearer testtoken")
	r.Header.Set("X-Tenant-Id", "tenant-001")
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// -----------------------------------------------------------------------------
// GET /api/me/dashboard
// -----------------------------------------------------------------------------

func TestMeDashboard_200_ComposesDTO(t *testing.T) {
	stub := meDashboardStub(t)
	h := newMeDashboardMux(t, stub)
	w := doMeDashboardReq(t, h, http.MethodGet, "/api/me/dashboard")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	var dto map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &dto); err != nil {
		t.Fatalf("body not JSON: %v — body=%s", err, w.Body.String())
	}
	if dto["userDisplayName"] != "Phyllis Tan" {
		t.Errorf("userDisplayName = %v; want Phyllis Tan", dto["userDisplayName"])
	}
	if got, _ := dto["currentStreakDays"].(float64); got != 7 {
		t.Errorf("currentStreakDays = %v; want 7", dto["currentStreakDays"])
	}
	if lc, _ := dto["learnerCourses"].([]any); len(lc) != 1 {
		t.Errorf("learnerCourses len = %v; want 1", dto["learnerCourses"])
	}
	if _, ok := dto["instructorCourses"].([]any); !ok {
		t.Errorf("instructorCourses must be [] (never null)")
	}
}

func TestMeDashboard_405_OnPost(t *testing.T) {
	stub := meDashboardStub(t)
	h := newMeDashboardMux(t, stub)
	w := doMeDashboardReq(t, h, http.MethodPost, "/api/me/dashboard")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 on POST /api/me/dashboard", w.Code)
	}
}

// Content-Type is set so the FE HttpClient parses the body as JSON.
func TestMeDashboard_SetsJSONContentType(t *testing.T) {
	stub := meDashboardStub(t)
	h := newMeDashboardMux(t, stub)
	w := doMeDashboardReq(t, h, http.MethodGet, "/api/me/dashboard")
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q; want application/json", ct)
	}
}

// -----------------------------------------------------------------------------
// WithMeDashboard composition
// -----------------------------------------------------------------------------

func TestWithMeDashboard_NilAggregator_PassesThrough(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := httpadapter.WithMeDashboard(base, nil)
	r := httptest.NewRequest(http.MethodGet, "/api/me/dashboard", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTeapot {
		t.Errorf("status = %d; want 418 — nil aggregator must pass through to base", w.Code)
	}
}

func TestWithMeDashboard_NonDashboardPath_FallsThroughToBase(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := meDashboardStub(t)
	agg := medashboard.New(medashboard.Config{
		IdentityURL:    stub.URL,
		ConsumptionURL: stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithMeDashboard(base, agg)
	// /api/me (Phyllis-owned) and /api/me/companions (gatewayproxy-owned)
	// must fall through — the bridge owns ONLY /api/me/dashboard.
	for _, p := range []string{"/api/me", "/api/me/companions", "/api/me/roles"} {
		r := httptest.NewRequest(http.MethodGet, p, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusTeapot {
			t.Errorf("path %q: status = %d; want 418 — non-dashboard path must fall through", p, w.Code)
		}
	}
}

func TestWithMeDashboard_DashboardPath_HandledByBridge(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := meDashboardStub(t)
	agg := medashboard.New(medashboard.Config{
		IdentityURL:    stub.URL,
		ConsumptionURL: stub.URL,
		PerCallTimeout: time.Second,
	})
	h := httpadapter.WithMeDashboard(base, agg)
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/me/dashboard", nil)
	r.Header.Set("Authorization", "Bearer t")
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusTeapot {
		t.Error("dashboard path leaked to base — bridge must own /api/me/dashboard")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 from the bridge", w.Code)
	}
}
