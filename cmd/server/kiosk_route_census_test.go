// kiosk_route_census_test.go: the kiosk allowlist's census (TRACKER T1.7; the orchestrator's
// ruling A). Every pattern in kioskProductRoutes must reach a route the gateway really
// registers: the test builds the real chain (buildEdgeHandler, as main() does) with recording
// fake upstreams and sends one sample request per pattern. A sample counts as reaching a
// route when an upstream received a request, or the gateway answered it itself with anything
// but its not-found answers. A planted pattern that matches no route must be caught.
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// kioskRouteSamples holds one request per allowlisted pattern.
var kioskRouteSamples = map[string][2]string{
	"/api/v1/me/goals":                   {http.MethodGet, "/api/v1/me/goals"},
	"/api/v1/me/goals/":                  {http.MethodGet, "/api/v1/me/goals/g1/campaign/questions"},
	"/api/v1/me/maps":                    {http.MethodGet, "/api/v1/me/maps"},
	"/api/v1/me/maps/":                   {http.MethodGet, "/api/v1/me/maps/m1/graph"},
	"/api/v1/me/concept-graph":           {http.MethodGet, "/api/v1/me/concept-graph"},
	"/api/v1/me/concept-graph/":          {http.MethodGet, "/api/v1/me/concept-graph/suggestions"},
	"GET /api/atoms/{atomID}":            {http.MethodGet, "/api/atoms/a1"},
	"/api/atoms/{atomID}/question-jobs":  {http.MethodPost, "/api/atoms/a1/question-jobs"},
	"/api/atoms/{atomID}/question-jobs/": {http.MethodGet, "/api/atoms/a1/question-jobs/j1"},
	"/api/atoms/{atomID}/session":        {http.MethodPost, "/api/atoms/a1/session"},
	"/api/atoms/{atomID}/session/submit": {http.MethodPost, "/api/atoms/a1/session/submit"},
	"/api/companion/daily-dose":          {http.MethodGet, "/api/companion/daily-dose"},
	"/api/companion/daily-dose/ai":       {http.MethodGet, "/api/companion/daily-dose/ai"},
	"POST /api/companion/greeting":       {http.MethodPost, "/api/companion/greeting"},
	"/api/v1/me/companions/":             {http.MethodGet, "/api/v1/me/companions/c1"},
	"/api/v1/question-banks":             {http.MethodGet, "/api/v1/question-banks"},
	"/api/v1/question-banks/":            {http.MethodGet, "/api/v1/question-banks/b1/questions"},
	"/api/v1/me/question-banks":          {http.MethodGet, "/api/v1/me/question-banks"},
	"/api/v1/test-sets":                  {http.MethodGet, "/api/v1/test-sets?source_job_id=j1"},
	"/api/v1/test-sets/":                 {http.MethodGet, "/api/v1/test-sets/t1"},
	"/api/v1/assessments":                {http.MethodPost, "/api/v1/assessments"},
	"/api/v1/assessments/":               {http.MethodGet, "/api/v1/assessments/x1/submissions/s1/grading"},
	"/api/v1/me/assessments":             {http.MethodGet, "/api/v1/me/assessments"},
	"/api/v1/me/assessments/":            {http.MethodGet, "/api/v1/me/assessments/x1"},
	"/bff/oplus/governance":              {http.MethodGet, "/bff/oplus/governance"},
	"GET /api/me/dashboard":              {http.MethodGet, "/api/me/dashboard"},
	"GET /api/v1/me/mana":                {http.MethodGet, "/api/v1/me/mana"},
	"/api/v1/me/growth-edges":            {http.MethodGet, "/api/v1/me/growth-edges"},
	"/api/v1/me/growth-edges/":           {http.MethodPost, "/api/v1/me/growth-edges/uploads/u1/resume"},
	"/healthz":                           {http.MethodGet, "/healthz"},
	"/readyz":                            {http.MethodGet, "/readyz"},
	"/health":                            {http.MethodGet, "/health"},
}

type censusRig struct {
	handler http.Handler
	mu      sync.Mutex
	hits    int
	token   string
}

func newCensusRig(t *testing.T) *censusRig {
	t.Helper()
	setKioskEnv(t)
	rig := &censusRig{}
	upstream := func() string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rig.mu.Lock()
			rig.hits++
			rig.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	for _, k := range []string{"SVC_IDENTITY_URL", "SVC_CREATION_URL", "SVC_CONSUMPTION_URL", "SVC_DELIVERY_URL", "SVC_OBSERVABILITY_URL"} {
		t.Setenv(k, upstream())
	}
	t.Setenv(EnvObservabilityHTTPAddr, upstream())
	t.Setenv(EnvKioskConductorURL, upstream())
	t.Setenv(EnvAIKernelOrchestratorURL, upstream())
	t.Setenv("BFF_HTTPUPSTREAM_ENABLED", "true")
	k, err := loadKioskEdition()
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := NewChoraSessionValidatorFromEnvWith(context.Background(), k.secretReader())
	if err != nil {
		t.Fatal(err)
	}
	h, cleanups, err := buildEdgeHandler(context.Background(), k, v)
	if err != nil {
		t.Fatalf("buildEdgeHandler with the kiosk environment: %v", err)
	}
	t.Cleanup(func() {
		for _, c := range cleanups {
			c()
		}
	})
	rig.handler = h
	rig.token = signKioskToken(t, []byte(kioskTestSigner), nil)
	return rig
}

// reaches reports whether one request reached a registered route, and what came back.
func (rig *censusRig) reaches(method, target string) (bool, int, string) {
	rig.mu.Lock()
	before := rig.hits
	rig.mu.Unlock()
	req := httptest.NewRequest(method, target, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+rig.token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	rig.handler.ServeHTTP(rec, req)
	rig.mu.Lock()
	hit := rig.hits > before
	rig.mu.Unlock()
	body := rec.Body.String()
	notFound := rec.Code == http.StatusNotFound &&
		(strings.Contains(body, "GATEWAY_ROUTE_NOT_FOUND") || strings.Contains(body, "404 page not found"))
	refused := strings.Contains(body, "NOT_PART_OF_KIOSK_EDITION")
	return hit || (!notFound && !refused && rec.Code < 500), rec.Code, body
}

func TestKioskRouteCensus_EveryAllowedPatternHasASample(t *testing.T) {
	for _, p := range kioskProductRoutes {
		if _, ok := kioskRouteSamples[p]; !ok {
			t.Errorf("allowlisted pattern %q has no census sample", p)
		}
	}
	for p := range kioskRouteSamples {
		found := false
		for _, q := range kioskProductRoutes {
			found = found || p == q
		}
		if !found {
			t.Errorf("census sample %q names no allowlisted pattern", p)
		}
	}
}

func TestKioskRouteCensus_EveryAllowedPatternReachesARegisteredRoute(t *testing.T) {
	rig := newCensusRig(t)
	for _, p := range kioskProductRoutes {
		s := kioskRouteSamples[p]
		if ok, code, body := rig.reaches(s[0], s[1]); !ok {
			t.Errorf("allowlisted %q: %s %s reached no registered route (%d %s)", p, s[0], s[1], code, strings.TrimSpace(body))
		}
	}
}

// Negative control: an allowlisted pattern with no route behind it is caught.
func TestKioskRouteCensus_CatchesAPatternWithNoRoute(t *testing.T) {
	saved := kioskProductRoutes
	kioskProductRoutes = append(append([]string{}, saved...), "/api/v1/me/kiosk-census-probe")
	t.Cleanup(func() { kioskProductRoutes = saved })
	rig := newCensusRig(t)
	if ok, code, body := rig.reaches(http.MethodGet, "/api/v1/me/kiosk-census-probe"); ok {
		t.Fatalf("a pattern with no route behind it must be caught, got %d %s", code, body)
	}
}

// W20 (K158): the review resume reaches the orchestrator's own route with the session's tenant,
// and no Google ID token rides it (the kiosk refuses the audience setting).
func TestKioskRouteCensus_TheWeaknessResumeReachesTheOrchestrator(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotTenant, gotAuth string
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath, gotTenant, gotAuth = r.URL.Path, r.Header.Get("X-Tenant-Id"), r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"completed"}`))
	}))
	t.Cleanup(orch.Close)
	rig := newCensusRig(t)
	t.Setenv(EnvAIKernelOrchestratorURL, orch.URL)
	k, err := loadKioskEdition()
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := NewChoraSessionValidatorFromEnvWith(context.Background(), k.secretReader())
	if err != nil {
		t.Fatal(err)
	}
	h, cleanups, err := buildEdgeHandler(context.Background(), k, v)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, c := range cleanups {
			c()
		}
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/growth-edges/uploads/u1/resume", strings.NewReader(`{"action":"confirm"}`))
	req.Header.Set("Authorization", "Bearer "+rig.token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	mu.Lock()
	defer mu.Unlock()
	if rec.Code != http.StatusOK || gotPath != "/v1/orchestrator/weakness/u1/resume" {
		t.Fatalf("want 200 from the orchestrator's resume route, got %d at %q (%s)", rec.Code, gotPath, rec.Body.String())
	}
	if gotTenant == "" {
		t.Fatal("the resume must carry the session's tenant as X-Tenant-Id")
	}
	if gotAuth != "" {
		t.Fatalf("no token may ride the kiosk's resume, got Authorization %q", gotAuth)
	}
}
