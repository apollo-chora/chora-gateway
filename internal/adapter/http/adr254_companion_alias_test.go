// adr254_companion_alias_test.go: ADR-254 D9 (Familiar -> Companion rename),
// gateway deploy-window aliases.
//
// Every pre-rename PUBLIC path the SPA still calls is served as an ALIAS of its
// companion-named route: same handler, same upstream path, same method gate.
// The aliases are dropped after the SPA cut (WP-X go). These tests pin each
// alias pair end-to-end: the old path and the new path MUST reach the SAME
// upstream path, through the same composition the production router uses.
//
// Upstreams that rename in a LATER window keep their pre-rename path on both
// spellings: chora-tenancy (/api/familiar-eggs/*, W5) and chora-sharing
// (/v1/me/preferences/familiar-milestone-share, its own window).
package httpadapter_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/adapter/inmem"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/companionbridge"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/social"
)

// aliasRecorder is an upstream stub that records the last path it served.
type aliasRecorder struct {
	*httptest.Server
	lastPath   string
	lastMethod string
}

func newAliasRecorder(t *testing.T, body string) *aliasRecorder {
	t.Helper()
	rec := &aliasRecorder{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.lastPath = r.URL.Path
		rec.lastMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(rec.Server.Close)
	return rec
}

// aliasBase404 is the base handler the bridges compose over; reaching it means
// the bridge did NOT claim the path.
var aliasBase404 = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "fell through to base", http.StatusNotFound)
})

func aliasReq(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequestWithContext(context.Background(), method, path, nil)
	}
	r.Header.Set("Authorization", "Bearer testtoken")
	r.Header.Set("X-Tenant-Id", "tenant-001")
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// TestADR254Alias_CompanionBridge_OldAndNewPathsReachSameUpstream pins the
// /api/v1/me/familiars* and /api/v1/familiar-eggs/* aliases against the
// companion-named routes through the WithCompanionBridge composition.
func TestADR254Alias_CompanionBridge_OldAndNewPathsReachSameUpstream(t *testing.T) {
	cons := newAliasRecorder(t, `{"items":[]}`)
	ten := newAliasRecorder(t, `{"items":[]}`)
	b := companionbridge.New(companionbridge.Config{
		ConsumptionURL: cons.URL,
		TenancyURL:     ten.URL,
		PerCallTimeout: time.Second,
		HatchTimeout:   time.Second,
	})
	h := httpadapter.WithCompanionBridge(aliasBase404, b)

	cases := []struct {
		name         string
		method       string
		oldPath      string
		newPath      string
		rec          *aliasRecorder
		wantUpstream string
		body         string
	}{
		{"list", http.MethodGet, "/api/v1/me/familiars", "/api/v1/me/companions", cons, "/v1/me/companions", ""},
		{"growth", http.MethodGet, "/api/v1/me/familiars/fam-1/growth", "/api/v1/me/companions/fam-1/growth", cons, "/v1/me/companions/fam-1/growth", ""},
		{"bindings", http.MethodGet, "/api/v1/me/familiars/bindings", "/api/v1/me/companions/bindings", cons, "/v1/me/companions/bindings", ""},
		// tenancy upstream renames at W5: both public spellings land on the
		// pre-rename tenancy path.
		{"egg catalog", http.MethodGet, "/api/v1/familiar-eggs/catalog", "/api/v1/companion-eggs/catalog", ten, "/api/familiar-eggs/catalog", ""},
		{"egg odds", http.MethodGet, "/api/v1/familiar-eggs/golden-001/odds", "/api/v1/companion-eggs/golden-001/odds", ten, "/api/familiar-eggs/golden-001/odds", ""},
		{"egg checkout", http.MethodPost, "/api/v1/familiar-eggs/checkout", "/api/v1/companion-eggs/checkout", ten, "/api/familiar-eggs/checkout", `{"eggSku":"golden-001"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, p := range []string{tc.newPath, tc.oldPath} {
				tc.rec.lastPath = ""
				w := aliasReq(t, h, tc.method, p, tc.body)
				if w.Code != http.StatusOK {
					t.Fatalf("%s %s -> %d (body=%s); want 200", tc.method, p, w.Code, w.Body.String())
				}
				if tc.rec.lastPath != tc.wantUpstream {
					t.Fatalf("%s %s -> upstream %q; want %q", tc.method, p, tc.rec.lastPath, tc.wantUpstream)
				}
			}
		})
	}
}

// TestADR254Alias_CompanionBridge_AliasKeepsMethodGate: the alias is the same
// handler, so it refuses the same methods the companion route refuses.
func TestADR254Alias_CompanionBridge_AliasKeepsMethodGate(t *testing.T) {
	cons := newAliasRecorder(t, `{"items":[]}`)
	ten := newAliasRecorder(t, `{"items":[]}`)
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, TenancyURL: ten.URL, PerCallTimeout: time.Second})
	h := httpadapter.WithCompanionBridge(aliasBase404, b)
	for _, p := range []string{"/api/v1/me/companions", "/api/v1/me/familiars", "/api/v1/companion-eggs/catalog", "/api/v1/familiar-eggs/catalog"} {
		w := aliasReq(t, h, http.MethodDelete, p, "")
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("DELETE %s -> %d; want 405", p, w.Code)
		}
	}
}

// TestADR254Alias_GatewayProxy_OldAndNewPathsReachSameUpstream pins the
// /api/me/familiars* and /api/familiar/daily-dose/ai aliases through the
// WithGatewayProxy composition (ownership + routing).
func TestADR254Alias_GatewayProxy_OldAndNewPathsReachSameUpstream(t *testing.T) {
	rec := newAliasRecorder(t, `{"items":[]}`)
	agg := gatewayproxy.New(gatewayproxy.Config{
		TenancyURL:       rec.URL,
		DeliveryURL:      rec.URL,
		ConsumptionURL:   rec.URL,
		NotificationsURL: rec.URL,
		CreationURL:      rec.URL,
		IdentityURL:      rec.URL,
		PerCallTimeout:   time.Second,
	})
	if agg == nil {
		t.Fatal("aggregator nil")
	}
	h := httpadapter.WithGatewayProxy(aliasBase404, agg)

	cases := []struct {
		name         string
		oldPath      string
		newPath      string
		wantUpstream string
	}{
		{"list", "/api/me/familiars", "/api/me/companions", "/v1/me/companions"},
		{"growth", "/api/me/familiars/fam-1/growth", "/api/me/companions/fam-1/growth", "/v1/me/companions/fam-1/growth"},
		{"daily dose ai", "/api/familiar/daily-dose/ai", "/api/companion/daily-dose/ai", "/companion/daily-dose/ai"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, p := range []string{tc.newPath, tc.oldPath} {
				rec.lastPath = ""
				w := aliasReq(t, h, http.MethodGet, p, "")
				if w.Code != http.StatusOK {
					t.Fatalf("GET %s -> %d (body=%s); want 200", p, w.Code, w.Body.String())
				}
				if rec.lastPath != tc.wantUpstream {
					t.Fatalf("GET %s -> upstream %q; want %q", p, rec.lastPath, tc.wantUpstream)
				}
			}
		})
	}
}

// TestADR254Alias_Phyllis_OldAndNewPathsReachSameUpstream pins the Phyllis
// /api/familiar/me, /api/familiar/daily-dose and bare /familiar/daily-dose
// aliases (route table + mux) against the companion-named routes.
func TestADR254Alias_Phyllis_OldAndNewPathsReachSameUpstream(t *testing.T) {
	consumption := newDownstream(t, http.StatusOK, `{"companion_id":"fam-001","atoms":["a1","a2","a3","a4","a5"]}`)
	srv := newPhyllisServer(t, phyllis.Config{ConsumptionURL: consumption.URL}, nil)

	cases := []struct{ path, wantUpstream string }{
		{"/api/companion/me", "/companion/me"},
		{"/api/familiar/me", "/companion/me"},
		{"/api/companion/daily-dose", "/companion/daily-dose"},
		{"/companion/daily-dose", "/companion/daily-dose"},
		{"/api/familiar/daily-dose", "/companion/daily-dose"},
		{"/familiar/daily-dose", "/companion/daily-dose"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			consumption.lastPath = ""
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+tc.path, nil)
			req.Header.Set("Authorization", "Bearer x")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s -> %d; want 200", tc.path, resp.StatusCode)
			}
			if consumption.lastPath != tc.wantUpstream {
				t.Fatalf("GET %s -> upstream %q; want %q", tc.path, consumption.lastPath, tc.wantUpstream)
			}
		})
	}
}

// TestADR254Alias_RouteTable_CarriesOldAndNewPhyllisPaths: the inmem route
// table is matched BEFORE the mux runs, so a missing entry 404s
// GATEWAY_ROUTE_NOT_FOUND no matter what the mux claims.
func TestADR254Alias_RouteTable_CarriesOldAndNewPhyllisPaths(t *testing.T) {
	repo := inmem.NewRouteRepository()
	for _, p := range []string{
		"/api/companion/me", "/api/familiar/me",
		"/api/companion/daily-dose", "/api/familiar/daily-dose",
		"/companion/daily-dose", "/familiar/daily-dose",
	} {
		rt, err := repo.Match(context.Background(), p)
		if err != nil || rt == nil {
			t.Errorf("route table has no entry for %s: %v", p, err)
			continue
		}
		if rt.BackendService != "chora-consumption" {
			t.Errorf("%s -> backend %q; want chora-consumption", p, rt.BackendService)
		}
	}
}

// TestADR254Alias_PaymentsCheckout_OldPathReachesSameRPC pins
// /api/v1/checkout/familiar-egg as an alias of /api/v1/checkout/companion-egg.
func TestADR254Alias_PaymentsCheckout_OldPathReachesSameRPC(t *testing.T) {
	for _, p := range []string{"/api/v1/checkout/companion-egg", "/api/v1/checkout/familiar-egg"} {
		t.Run(p, func(t *testing.T) {
			fake := &fakeCheckoutClient{companionEggResp: clients.CheckoutResponse{PurchaseID: "p-egg-1", State: "checkout_started"}}
			mux := newCheckoutHandlerWithFake(t, fake)
			r := authedReq(t, http.MethodPost, p, `{"egg_sku":"egg-standard","amount_cents":1999,"currency":"SGD"}`)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("POST %s -> %d (body=%s); want 200", p, w.Code, w.Body.String())
			}
			if fake.companionEggReq.EggSKU != "egg-standard" {
				t.Fatalf("POST %s did not reach CreateCompanionEggCheckoutSession (EggSKU=%q)", p, fake.companionEggReq.EggSKU)
			}
		})
	}
}

// TestADR254Alias_SocialMilestoneSharePref_OldAndNewPublicPathsReachSameUpstream:
// the public path gains the companion name plus the pre-rename alias; both land
// on the sharing upstream's pre-rename path (sharing renames at its own window
// and its Istio allowlist is byte-exact).
func TestADR254Alias_SocialMilestoneSharePref_OldAndNewPublicPathsReachSameUpstream(t *testing.T) {
	rec := newAliasRecorder(t, `{"policy":"auto"}`)
	srv := newSocialFixtureWithAgg(t, social.New(social.Config{SharingURL: rec.URL}))
	defer srv.Close()
	for _, p := range []string{"/v1/me/preferences/companion-milestone-share", "/v1/me/preferences/familiar-milestone-share"} {
		rec.lastPath = ""
		resp := milestoneDo(t, srv.URL, http.MethodGet, p, "")
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s -> %d; want 200", p, resp.StatusCode)
		}
		if rec.lastPath != "/v1/me/preferences/familiar-milestone-share" {
			t.Fatalf("GET %s -> upstream %q; want /v1/me/preferences/familiar-milestone-share (sharing upstream renames at its window)", p, rec.lastPath)
		}
	}
}

// TestADR254Alias_JWTGatedPrefixes_CoverOldAndNewPublicPaths: an alias that
// escaped the JWT gate would forward without mesh claims (and, worse, without
// session validation). Both spellings MUST be gated.
func TestADR254Alias_JWTGatedPrefixes_CoverOldAndNewPublicPaths(t *testing.T) {
	for _, p := range []string{
		"/api/v1/me/companions", "/api/v1/me/familiars",
		"/api/v1/me/companions/fam-1/growth", "/api/v1/me/familiars/fam-1/growth",
		"/api/v1/companion-eggs/catalog", "/api/v1/familiar-eggs/catalog",
		"/api/companion/me", "/api/familiar/me",
		"/api/companion/daily-dose", "/api/familiar/daily-dose",
		"/companion/daily-dose", "/familiar/daily-dose",
		"/api/me/companions", "/api/me/familiars",
		"/api/companion/daily-dose/ai", "/api/familiar/daily-dose/ai",
		"/api/v1/checkout/companion-egg", "/api/v1/checkout/familiar-egg",
		"/v1/me/preferences/companion-milestone-share", "/v1/me/preferences/familiar-milestone-share",
	} {
		covered := false
		for _, prefix := range httpadapter.DefaultJWTGatedPrefixes {
			if strings.HasPrefix(p, prefix) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("path %q is NOT covered by DefaultJWTGatedPrefixes", p)
		}
	}
}

// TestADR254Alias_CompanionBridgePathPrefixes_ListBothSpellings: the exported
// prefix list feeds DefaultJWTGatedPrefixes and the bridge dispatcher; it must
// carry the companion names AND the deploy-window aliases.
func TestADR254Alias_CompanionBridgePathPrefixes_ListBothSpellings(t *testing.T) {
	want := map[string]bool{
		"/api/v1/me/companions":  false,
		"/api/v1/companion-eggs": false,
		"/api/v1/me/familiars":   false,
		"/api/v1/familiar-eggs":  false,
	}
	for _, p := range httpadapter.CompanionBridgePathPrefixes {
		if _, ok := want[p]; ok {
			want[p] = true
		}
	}
	for p, seen := range want {
		if !seen {
			t.Errorf("CompanionBridgePathPrefixes is missing %s", p)
		}
	}
}
