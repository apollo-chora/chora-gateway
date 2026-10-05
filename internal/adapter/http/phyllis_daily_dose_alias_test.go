// phyllis_daily_dose_alias_test.go — Debt #41 close (2026-05-16).
//
// The CR v4 rehearsal (row 12) probed `GET /companion/daily-dose` at the
// edge — i.e. without the conventional `/api/` BFF prefix — and got 404
// because the gateway only claims `/api/companion/daily-dose`. The
// chora-consumption handler itself IS mounted at the bare path
// `/companion/daily-dose` (see services/chora-consumption/internal/adapter/
// http/router.go:254), so the 404 is purely a gateway-claim mismatch.
//
// Arch-clean fix: mount an alias claim at `/companion/daily-dose` that
// delegates to the same Phyllis aggregator. Both paths land on the
// identical downstream call.
package httpadapter_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

// TestPhyllis_GetDailyDose_BareAlias — the bare `/companion/daily-dose`
// path MUST resolve identically to `/api/companion/daily-dose` (same
// aggregator, same downstream call, same response).
func TestPhyllis_GetDailyDose_BareAlias(t *testing.T) {
	consumption := newDownstream(t, http.StatusOK, `{"atoms":["a1","a2","a3","a4","a5"]}`)
	srv := newPhyllisServer(t, phyllis.Config{ConsumptionURL: consumption.URL}, nil)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/companion/daily-dose", nil)
	req.Header.Set("Authorization", "Bearer x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200 (bare /companion/daily-dose must be claimed alongside /api/companion/daily-dose)", resp.StatusCode)
	}
	if consumption.lastPath != "/companion/daily-dose" {
		t.Errorf("downstream path = %q; want /companion/daily-dose", consumption.lastPath)
	}
}

// TestPhyllis_GetDailyDose_BareAliasMethodGated — only GET. POST etc must
// 405 the same way /api/companion/daily-dose does.
func TestPhyllis_GetDailyDose_BareAliasMethodGated(t *testing.T) {
	consumption := newDownstream(t, http.StatusOK, `{"atoms":[]}`)
	srv := newPhyllisServer(t, phyllis.Config{ConsumptionURL: consumption.URL}, nil)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/companion/daily-dose", nil)
	req.Header.Set("Authorization", "Bearer x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 (POST not allowed on daily-dose)", resp.StatusCode)
	}
}
