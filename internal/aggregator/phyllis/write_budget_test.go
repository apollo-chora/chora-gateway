// write_budget_test.go — TDD RED for CHO-1826 durable fix: admin mutation
// routes (tenant-member role writes + operator grant) need a longer per-call
// + aggregation budget than the 5s/15s read budget. Their synchronous
// identity->tenancy->pg chain exceeds 5s on a cold start (Cloud SQL resume /
// gRPC subchannel re-dial), surfacing a 504 facade even though the write
// commits. Reads MUST keep the tight fast-fail budget.
package phyllis_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

// ApplyDefaults must fill the write-budget fields with sane defaults that stay
// under the 45s server WriteTimeout, and keep budget >= per-call.
func TestApplyDefaults_writeBudgetDefaults(t *testing.T) {
	c := phyllis.Config{}
	c.ApplyDefaults()

	if c.WriteCallTimeout != phyllis.DefaultWriteCallTimeout {
		t.Errorf("WriteCallTimeout = %s; want %s", c.WriteCallTimeout, phyllis.DefaultWriteCallTimeout)
	}
	if c.WriteAggregationBudget != phyllis.DefaultWriteAggregationBudget {
		t.Errorf("WriteAggregationBudget = %s; want %s", c.WriteAggregationBudget, phyllis.DefaultWriteAggregationBudget)
	}
	if c.WriteAggregationBudget < c.WriteCallTimeout {
		t.Errorf("WriteAggregationBudget (%s) must be >= WriteCallTimeout (%s)", c.WriteAggregationBudget, c.WriteCallTimeout)
	}
	// Stay under the 45s server WriteTimeout (cmd/server/main.go:591).
	if c.WriteAggregationBudget >= 45*time.Second {
		t.Errorf("WriteAggregationBudget (%s) must stay under the 45s server WriteTimeout", c.WriteAggregationBudget)
	}
}

// LoadConfigFromEnv must read the write-budget overrides per no-inline-config.
func TestLoadConfigFromEnv_writeBudgetOverrides(t *testing.T) {
	t.Setenv("CHORA_PHYLLIS_WRITE_PERCALL_TIMEOUT_SECONDS", "12")
	t.Setenv("CHORA_PHYLLIS_WRITE_BUDGET_SECONDS", "20")

	c := phyllis.LoadConfigFromEnv()

	if c.WriteCallTimeout != 12*time.Second {
		t.Errorf("WriteCallTimeout = %s; want 12s", c.WriteCallTimeout)
	}
	if c.WriteAggregationBudget != 20*time.Second {
		t.Errorf("WriteAggregationBudget = %s; want 20s", c.WriteAggregationBudget)
	}
}

// The CORE regression: a SetAdminTenantMemberRoles whose downstream is slower
// than the read per-call budget but within the write budget must SUCCEED (no
// 504 facade), while a READ route against the SAME slow downstream must still
// 504 (reads keep tight fast-fail).
func TestSetAdminTenantMemberRoles_usesWriteBudget_readsStayTight(t *testing.T) {
	// Downstream sleeps 120ms: > the 20ms read per-call, < the 500ms write
	// per-call.
	slow := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"roles":["ADMIN","INSTRUCTOR","LEARNER"]}`))
	})

	cfg := phyllis.Config{
		IdentityURL:            slow.URL,
		PerCallTimeout:         20 * time.Millisecond,
		AggregationBudget:      40 * time.Millisecond,
		WriteCallTimeout:       500 * time.Millisecond,
		WriteAggregationBudget: 600 * time.Millisecond,
	}
	a := phyllis.New(cfg, nil)

	// WRITE route uses the longer write budget -> 200 (not 504).
	res, _ := a.SetAdminTenantMemberRoles(context.Background(), l1Auth(), l1Gcid,
		[]byte(`{"roles":["ADMIN","INSTRUCTOR","LEARNER"]}`))
	if res.Status != http.StatusOK {
		t.Errorf("SetAdminTenantMemberRoles status = %d; want 200 — write route must use the write budget (body=%s)", res.Status, res.Body)
	}

	// READ route against the SAME slow downstream stays on the tight budget -> 504.
	readRes, _ := a.SearchAdminTenantMembers(context.Background(), l1Auth(), "q=x")
	if readRes.Status != http.StatusGatewayTimeout {
		t.Errorf("SearchAdminTenantMembers status = %d; want 504 — reads must keep the tight fast-fail budget", readRes.Status)
	}
}
