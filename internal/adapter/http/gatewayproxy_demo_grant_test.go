// gatewayproxy_demo_grant_test.go — composition-level proof that the
// demo-grant leaf is owned by the gatewayproxy bridge (matchesGatewayProxyPath)
// and dispatched to chora-identity, NOT leaked to the base router. Also pins
// that /api/v1/me/mana/demo-grant sits under DefaultJWTGatedPrefixes so the
// JWT gate runs and mesh claims reach chora-identity.
package httpadapter_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

func TestWithGatewayProxy_ManaDemoGrant_HandledByBridge(t *testing.T) {
	// base 414 proves the request fell through to the base router, which would
	// mean matchesGatewayProxyPath failed to claim the path.
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnavailableForLegalReasons)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"granted_units":1,"balance_units":1,"replayed":false,"reason":"demo_grant"}`))
	})
	agg := gatewayproxy.New(gatewayproxy.Config{
		IdentityURL:    stub.URL,
		PerCallTimeout: time.Second,
	})
	if agg == nil {
		t.Fatal("aggregator nil — IdentityURL should enable the mana routes")
	}
	h := httpadapter.WithGatewayProxy(base, agg)

	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/v1/me/mana/demo-grant", strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer t")
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code == http.StatusUnavailableForLegalReasons {
		t.Fatal("POST /api/v1/me/mana/demo-grant leaked to base — bridge must own the demo-grant route")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/me/mana/demo-grant → %d; want 200 via chora-identity", w.Code)
	}
}

// The demo grant credits mana to the JWT-identified caller, so the JWT gate
// MUST run: without it RequireChoraSessionJWT never stamps MeshClaims and
// chora-identity cannot scope the ledger write. Pinned explicitly because the
// path reaches the gate only via the "/api/v1/me/mana" prefix.
func TestDefaultJWTGatedPrefixes_CoversManaDemoGrant(t *testing.T) {
	const p = "/api/v1/me/mana/demo-grant"
	covered := false
	for _, prefix := range httpadapter.DefaultJWTGatedPrefixes {
		if strings.HasPrefix(p, prefix) {
			covered = true
			break
		}
	}
	if !covered {
		t.Errorf("path %q is NOT covered by DefaultJWTGatedPrefixes — JWT gate would skip, mesh claims empty, chora-identity cannot scope the grant", p)
	}
}
