// daily_dose_goal_scope_test.go - WS-3 edge lane.
//
// GetDailyDose REBUILDS the upstream URL from scratch rather than proxying the
// inbound query string, so a query param the aggregator does not name explicitly
// is DROPPED here and never reaches chora-consumption. That makes this file
// load-bearing: without it the whole goal-scoped-dose lane is green on both
// sides and inert in production, because the backend simply never sees goal_id.
package phyllis_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

const goalScopeTestID = "019e0000-0000-7000-d000-000000000042"

// TestGetDailyDose_ForwardsGoalID - a non-empty goalID must arrive upstream as
// ?goal_id so chora-consumption can bias the dose toward that goal's material.
func TestGetDailyDose_ForwardsGoalID(t *testing.T) {
	var gotGoalID string
	consumption := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotGoalID = r.URL.Query().Get("goal_id")
		_, _ = w.Write([]byte(`{"atoms":[]}`))
	})
	a := phyllis.New(phyllis.Config{ConsumptionURL: consumption.URL, PerCallTimeout: 1 * time.Second}, nil)
	_, _ = a.GetDailyDose(context.Background(), phyllis.AuthCtx{Bearer: "x"}, "", goalScopeTestID)
	if gotGoalID != goalScopeTestID {
		t.Errorf("upstream goal_id = %q, want %q (the aggregator rebuilds the URL, so an unnamed param is dropped)",
			gotGoalID, goalScopeTestID)
	}
}

// TestGetDailyDose_OmitsGoalIDWhenEmpty - the unscoped call must stay
// byte-identical: no goal_id param at all, not an empty one.
func TestGetDailyDose_OmitsGoalIDWhenEmpty(t *testing.T) {
	var gotRawQuery string
	consumption := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"atoms":[]}`))
	})
	a := phyllis.New(phyllis.Config{ConsumptionURL: consumption.URL, PerCallTimeout: 1 * time.Second}, nil)
	_, _ = a.GetDailyDose(context.Background(), phyllis.AuthCtx{Bearer: "x"}, "", "")
	if gotRawQuery != "" {
		t.Errorf("upstream raw query = %q, want empty (no goal_id on the unscoped call)", gotRawQuery)
	}
}

// TestGetDailyDose_ForwardsBothScopes - the focused-practice edge id and the
// goal scope are independent params and must not clobber each other.
func TestGetDailyDose_ForwardsBothScopes(t *testing.T) {
	var gotEdge, gotGoal string
	consumption := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotEdge = r.URL.Query().Get("growth_edge_id")
		gotGoal = r.URL.Query().Get("goal_id")
		_, _ = w.Write([]byte(`{"atoms":[]}`))
	})
	a := phyllis.New(phyllis.Config{ConsumptionURL: consumption.URL, PerCallTimeout: 1 * time.Second}, nil)
	_, _ = a.GetDailyDose(context.Background(), phyllis.AuthCtx{Bearer: "x"}, "edge-99", goalScopeTestID)
	if gotEdge != "edge-99" {
		t.Errorf("upstream growth_edge_id = %q, want %q", gotEdge, "edge-99")
	}
	if gotGoal != goalScopeTestID {
		t.Errorf("upstream goal_id = %q, want %q", gotGoal, goalScopeTestID)
	}
}
