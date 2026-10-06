// PE-14 — A+ GDPR consent center HTTP handler tests.
//
// Verifies the three /api/me/{consents,consents/grant,data-export} routes wire
// correctly through to the Phyllis aggregator with Bearer + traceparent
// pass-through. Account closure (Art. 17) moved to the canonical
// /api/v1/me/account/close saga me-route (CHO-1719); the legacy
// /api/me/account-closure BFF route was dead and is removed (CHO-1790 D12).
package httpadapter_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/adapter/inmem"
	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

// buildPhyllisConfig builds a phyllis.Config with the supplied service URLs
// keyed by short name (identity, tenancy, creation, delivery, consumption,
// model_broker). Unrelated routes use an empty URL.
func buildPhyllisConfig(routes map[string]string) phyllis.Config {
	return phyllis.Config{
		IdentityURL:          routes["identity"],
		TenancyURL:           routes["tenancy"],
		CreationURL:          routes["creation"],
		DeliveryURL:          routes["delivery"],
		ConsumptionURL:       routes["consumption"],
		ModelBrokerRouterURL: routes["model_broker"],
	}
}

func TestGdpr_GetConsents_HappyPath(t *testing.T) {
	identity := newDownstream(t, http.StatusOK, `{"gcid":"phyllis","consents":[{"consent_type":"marketing_consent","granted":false}]}`)
	cfg := buildPhyllisConfig(map[string]string{"identity": identity.URL})
	srv := newPhyllisServer(t, cfg, nil)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/me/consents", nil)
	req.Header.Set("Authorization", "Bearer phyllis")
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "marketing_consent") {
		t.Errorf("body = %q; want consent payload pass-through", string(body))
	}
	if identity.lastAuth != "Bearer phyllis" {
		t.Errorf("downstream did not receive Bearer; got %q", identity.lastAuth)
	}
	if identity.lastTraceparent == "" {
		t.Errorf("downstream did not receive traceparent (empty)")
	}
	if identity.lastPath != "/me/consents" {
		t.Errorf("downstream path = %s; want /me/consents", identity.lastPath)
	}
}

func TestGdpr_GetConsents_RejectsNonGet(t *testing.T) {
	identity := newDownstream(t, http.StatusOK, `{}`)
	cfg := buildPhyllisConfig(map[string]string{"identity": identity.URL})
	srv := newPhyllisServer(t, cfg, nil)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, srv.URL+"/api/me/consents", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405", resp.StatusCode)
	}
}

func TestGdpr_GrantConsent_HappyPath(t *testing.T) {
	identity := newDownstream(t, http.StatusOK, `{"consent_type":"marketing_consent","granted":true}`)
	cfg := buildPhyllisConfig(map[string]string{"identity": identity.URL})
	srv := newPhyllisServer(t, cfg, nil)

	body := `{"consent_type":"marketing_consent","granted":true,"version":"2026-05-07"}`
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/api/me/consents/grant", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer phyllis")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	if identity.lastPath != "/me/consents" {
		t.Errorf("downstream path = %s; want /me/consents", identity.lastPath)
	}
	if identity.lastMethod != http.MethodPost {
		t.Errorf("downstream method = %s; want POST", identity.lastMethod)
	}
	if !strings.Contains(identity.lastBody, "marketing_consent") {
		t.Errorf("downstream body = %q; want forwarded payload", identity.lastBody)
	}
}

func TestGdpr_GrantConsent_RejectsNonPost(t *testing.T) {
	identity := newDownstream(t, http.StatusOK, `{}`)
	cfg := buildPhyllisConfig(map[string]string{"identity": identity.URL})
	srv := newPhyllisServer(t, cfg, nil)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/me/consents/grant", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405", resp.StatusCode)
	}
}

func TestGdpr_DataExport_HappyPath(t *testing.T) {
	identity := newDownstream(t, http.StatusAccepted, `{"export_id":"01900-export","status":"queued","sla_days":30}`)
	cfg := buildPhyllisConfig(map[string]string{"identity": identity.URL})
	cfg.PerCallTimeout = 1 * time.Second
	cfg.AggregationBudget = 5 * time.Second
	router := httpadapter.NewRouterWithPhyllis(
		inmem.NewRouteRepository(), inmem.NewSessionRepository(),
		upstream.NewFakeUpstream(), phyllis.New(cfg, nil),
	)

	// chora-identity resolves the GCID from the path, so the export route now
	// needs a resolved identity. In production RequireChoraSessionJWT stamps
	// it; here we inject the validated mesh claims directly.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/me/data-export", strings.NewReader(`{"format":"portable_json"}`))
	req.Header.Set("Authorization", "Bearer phyllis")
	req = req.WithContext(httpadapter.InjectMeshClaimsForTest(req.Context(), "gcid-export", "tenant-1"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d; want 202", w.Code)
	}
	if identity.lastPath != "/api/users/gcid-export/portability/export" {
		t.Errorf("downstream path = %s; want /api/users/gcid-export/portability/export", identity.lastPath)
	}
}
