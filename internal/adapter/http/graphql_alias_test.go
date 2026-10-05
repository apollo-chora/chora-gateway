// graphql_alias_test.go — W0 (2026-06-09).
//
// The FE posts learner-facing GraphQL reads to POST /api/v1/graphql
// (chora-web/src/app/core/services/graphql.service.ts:36), but the gateway
// historically mounts the BFF GraphQL federation handler only at /graphql
// (phyllis_handler.go + handler.go), so /api/v1/graphql 404s shell-wide.
// Arch-clean fix: alias /api/v1/graphql -> the same federation handler.
// REST + GraphQL coexist; existing /graphql is NEVER removed.
package httpadapter_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/adapter/inmem"
	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

// TestGraphQL_ApiV1Alias — /api/v1/graphql MUST resolve to the same federation
// handler as /graphql (both reach the handler; neither 404s).
func TestGraphQL_ApiV1Alias(t *testing.T) {
	var hits int
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{}}`))
	})

	routesRepo := inmem.NewRouteRepository()
	sessionsRepo := inmem.NewSessionRepository()
	up := upstream.NewFakeUpstream()
	agg := phyllis.New(phyllis.Config{PerCallTimeout: time.Second, AggregationBudget: 5 * time.Second}, nil)
	router := httpadapter.NewRouterWithPhyllisAndGraphQL(routesRepo, sessionsRepo, up, agg, stub)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	for _, path := range []string{"/graphql", "/api/v1/graphql"} {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer x")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s err: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("POST %s status = %d; want 200 (must alias to the /graphql federation handler)", path, resp.StatusCode)
		}
	}
	if hits != 2 {
		t.Errorf("graphql handler hits = %d; want 2 (both /graphql and /api/v1/graphql must reach it)", hits)
	}
}
