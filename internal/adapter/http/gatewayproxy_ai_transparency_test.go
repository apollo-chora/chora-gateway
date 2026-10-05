// gatewayproxy_ai_transparency_test.go — ADR-225: HTTP route-binding tests for
// the AI Transparency Notice learner-self leaves. The bridge must OWN
// /api/v1/me/ai-transparency (GET) + /api/v1/me/ai-transparency/acknowledge
// (POST) and DefaultJWTGatedPrefixes must cover them so mesh claims are stamped
// before the downstream chora-governance handler runs.
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

func TestWithGatewayProxy_AITransparencyPaths_HandledByBridge(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	stub := newGwProxyStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	agg := gatewayproxy.New(gatewayproxy.Config{
		GovernanceURL:  stub.URL,
		PerCallTimeout: time.Second,
	})
	if agg == nil {
		t.Fatal("aggregator nil — GovernanceURL should enable the AI-transparency routes")
	}
	h := httpadapter.WithGatewayProxy(base, agg)
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/me/ai-transparency"},
		{http.MethodPost, "/api/v1/me/ai-transparency/acknowledge"},
	} {
		r := httptest.NewRequestWithContext(context.Background(), tc.method, tc.path, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer t")
		r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusTeapot {
			t.Errorf("%s %s: leaked to base — bridge must own the ai-transparency route", tc.method, tc.path)
		}
	}
}

func TestDefaultJWTGatedPrefixes_CoversAITransparency(t *testing.T) {
	for _, p := range []string{
		"/api/v1/me/ai-transparency",
		"/api/v1/me/ai-transparency/acknowledge",
	} {
		covered := false
		for _, prefix := range httpadapter.DefaultJWTGatedPrefixes {
			if strings.HasPrefix(p, prefix) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("path %q is NOT covered by DefaultJWTGatedPrefixes — JWT gate would skip, mesh claims empty, chora-governance would 400", p)
		}
	}
}
