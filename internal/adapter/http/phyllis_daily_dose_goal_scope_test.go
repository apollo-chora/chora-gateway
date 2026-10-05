// phyllis_daily_dose_goal_scope_test.go - WS-3 edge lane, handler half.
//
// The goal scope crosses TWO drop points on its way to chora-consumption: this
// handler must READ ?goal_id off the inbound request, and the aggregator must
// NAME it when it rebuilds the upstream URL. Either one missing loses the param
// silently, so this test drives the whole edge - a real inbound query string on
// the real router, asserted at a real upstream - rather than either half alone.
package httpadapter_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

const doseGoalScopeID = "019e0000-0000-7000-d000-000000000042"

// getDoseUpstreamQuery drives GET {target} through the phyllis router and
// returns the query params the upstream chora-consumption call actually carried.
func getDoseUpstreamQuery(t *testing.T, target string) map[string]string {
	t.Helper()
	got := map[string]string{}
	consumption := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range r.URL.Query() {
			if len(v) > 0 {
				got[k] = v[0]
			}
		}
		_, _ = w.Write([]byte(`{"atoms":[]}`))
	}))
	t.Cleanup(consumption.Close)

	srv := newPhyllisServer(t, phyllis.Config{ConsumptionURL: consumption.URL}, nil)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+target, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	return got
}

// TestPhyllis_DailyDose_ForwardsGoalIDToUpstream - the load-bearing edge proof.
func TestPhyllis_DailyDose_ForwardsGoalIDToUpstream(t *testing.T) {
	got := getDoseUpstreamQuery(t, "/api/companion/daily-dose?goal_id="+doseGoalScopeID)
	if got["goal_id"] != doseGoalScopeID {
		t.Errorf("upstream goal_id = %q, want %q (the edge dropped the goal scope)", got["goal_id"], doseGoalScopeID)
	}
}

// TestPhyllis_DailyDose_ForwardsCamelCaseGoalID - `goalId` is accepted too, so a
// spelling mismatch with the caller cannot ship as a silently inert feature.
func TestPhyllis_DailyDose_ForwardsCamelCaseGoalID(t *testing.T) {
	got := getDoseUpstreamQuery(t, "/api/companion/daily-dose?goalId="+doseGoalScopeID)
	if got["goal_id"] != doseGoalScopeID {
		t.Errorf("upstream goal_id = %q, want %q (goalId alias not normalised)", got["goal_id"], doseGoalScopeID)
	}
}

// TestPhyllis_DailyDose_NoGoalIDWhenAbsent - the plain call must carry no
// goal_id at all (byte-identical to the pre-WS-3 upstream request).
func TestPhyllis_DailyDose_NoGoalIDWhenAbsent(t *testing.T) {
	got := getDoseUpstreamQuery(t, "/api/companion/daily-dose")
	if _, present := got["goal_id"]; present {
		t.Errorf("upstream carried goal_id = %q on an unscoped call, want it absent", got["goal_id"])
	}
}
