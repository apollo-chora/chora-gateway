// kiosk_d8_route_census_test.go: T2.16, the D8 and conductor routes through the real kiosk
// edge chain. For each concrete route: it is allowlisted, it is refused without the kiosk
// session, and with one it reaches the right upstream service at the right path (and query).
// Builds on T1.7's census (kiosk_route_census_test.go), whose patterns it leaves to T1.
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type d8Hit struct{ service, method, path, query string }

type d8Rig struct {
	handler http.Handler
	mu      sync.Mutex
	hits    []d8Hit
	token   string
}

func newD8Rig(t *testing.T) *d8Rig { return newD8RigWith(t, nil) }

// newD8RigWith is newD8Rig with extra settings over the kiosk environment.
func newD8RigWith(t *testing.T, extra map[string]string) *d8Rig {
	t.Helper()
	setKioskEnv(t)
	for k, v := range extra {
		t.Setenv(k, v)
	}
	rig := &d8Rig{}
	upstream := func(service string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rig.mu.Lock()
			rig.hits = append(rig.hits, d8Hit{service, r.Method, r.URL.Path, r.URL.RawQuery})
			rig.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	for env, service := range map[string]string{
		"SVC_IDENTITY_URL": "identity", "SVC_CREATION_URL": "creation", "SVC_CONSUMPTION_URL": "consumption",
		"SVC_DELIVERY_URL": "delivery", "SVC_OBSERVABILITY_URL": "observability",
		EnvObservabilityHTTPAddr: "observability-http", EnvKioskConductorURL: "conductor",
	} {
		t.Setenv(env, upstream(service))
	}
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

// do sends one request (with the session when token is not empty) and returns the answer
// and the upstream requests it caused.
func (rig *d8Rig) do(method, target, body, token string) (*httptest.ResponseRecorder, []d8Hit) {
	rig.mu.Lock()
	before := len(rig.hits)
	rig.mu.Unlock()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	rig.handler.ServeHTTP(rec, req)
	rig.mu.Lock()
	defer rig.mu.Unlock()
	return rec, append([]d8Hit(nil), rig.hits[before:]...)
}

// d8Route is one concrete kiosk route and the upstream request it must become.
type d8Route struct {
	name, method, target, body string
	want                       d8Hit
}

// d8Routes: the D8 changes' routes (T2.8 to T2.18) and the conductor's, each as kiosk-web
// calls it. The downstream paths were checked against each service's own router.
var d8Routes = []d8Route{
	{"K49 greeting", "POST", "/api/companion/greeting", `{}`,
		d8Hit{"consumption", "POST", "/companion/greeting", ""}},
	{"K57 request grading", "POST", "/api/v1/me/assessments/x1/submissions/s1/request-grading", `{}`,
		d8Hit{"delivery", "POST", "/api/v1/me/assessments/x1/submissions/s1/request-grading", ""}},
	{"D8-b grading read", "GET", "/api/v1/assessments/x1/submissions/s1/grading", "",
		d8Hit{"delivery", "GET", "/api/v1/assessments/x1/submissions/s1/grading", ""}},
	{"K51 suggestion request", "GET", "/api/v1/me/concept-graph/suggestions/requests/r1", "",
		d8Hit{"consumption", "GET", "/v1/me/concept-graph/suggestions/requests/r1", ""}},
	{"Q-fresh press", "POST", "/api/v1/me/goals/g1/campaign/questions/fresh", `{}`,
		d8Hit{"consumption", "POST", "/v1/me/goals/g1/campaign/questions/fresh", ""}},
	{"Q-fresh read", "GET", "/api/v1/me/goals/g1/campaign/questions/fresh/f1", "",
		d8Hit{"consumption", "GET", "/v1/me/goals/g1/campaign/questions/fresh/f1", ""}},
	{"V1 and V5 serve door", "GET", "/api/v1/me/goals/g1/campaign/questions?concept_id=c1&rung=2", "",
		d8Hit{"consumption", "GET", "/v1/me/goals/g1/campaign/questions", "concept_id=c1&rung=2"}},
	{"V5 governance by trace", "GET", "/bff/oplus/governance?trace_id=0af7651916cd43dd8448eb211c80319c", "",
		d8Hit{"observability-http", "GET", "/api/agent-decisions", "trace_id=0af7651916cd43dd8448eb211c80319c"}},
	{"V2 regenerate", "POST", "/api/atoms/a1/question-jobs/j1/regenerate-image", `{}`,
		d8Hit{"creation", "POST", "/api/atoms/a1/question-jobs/j1/regenerate-image", ""}},
	{"D8-c job read", "GET", "/api/atoms/a1/question-jobs/j1", "",
		d8Hit{"creation", "GET", "/api/atoms/a1/question-jobs/j1", ""}},
	{"F17 dose card body", "GET", "/api/atoms/a1", "",
		d8Hit{"creation", "GET", "/api/atoms/a1", ""}},
	{"K127 goal dose", "GET", "/api/companion/daily-dose?goal_id=g1", "",
		d8Hit{"consumption", "GET", "/companion/daily-dose", "goal_id=g1"}},
	{"dose answer", "POST", "/api/atoms/a1/session/submit", `{"session_id":"s1","selected_option_id":"o1"}`,
		d8Hit{"consumption", "POST", "/v1/me/atom-sessions/s1/answers", ""}},
	{"conductor manifest", "GET", "/api/v1/kiosk/prewarm/manifest", "",
		d8Hit{"conductor", "GET", "/api/v1/kiosk/prewarm/manifest", ""}},
	{"conductor reset", "POST", "/api/v1/kiosk/reset", "",
		d8Hit{"conductor", "POST", "/api/v1/kiosk/reset", ""}},
	{"conductor reset status", "GET", "/api/v1/kiosk/reset", "",
		d8Hit{"conductor", "GET", "/api/v1/kiosk/reset", ""}},
	{"conductor readiness", "GET", "/api/v1/kiosk/readiness", "",
		d8Hit{"conductor", "GET", "/api/v1/kiosk/readiness", ""}},
	{"conductor visits", "POST", "/api/v1/kiosk/visits", `{}`,
		d8Hit{"conductor", "POST", "/api/v1/kiosk/visits", ""}},
	{"conductor xray", "GET", "/api/v1/kiosk/xray/r1", "",
		d8Hit{"conductor", "GET", "/api/v1/kiosk/xray/r1", ""}},
}

// reached reports whether exactly one upstream request arrived and it is want: the same
// service, method and path, and every parameter of want's query with the same value.
func reached(hits []d8Hit, want d8Hit) bool {
	if len(hits) != 1 {
		return false
	}
	h := hits[0]
	if h.service != want.service || h.method != want.method || h.path != want.path {
		return false
	}
	got, err := url.ParseQuery(h.query)
	if err != nil {
		return false
	}
	need, err := url.ParseQuery(want.query)
	if err != nil {
		return false
	}
	for k, vs := range need {
		if strings.Join(got[k], ",") != strings.Join(vs, ",") {
			return false
		}
	}
	return true
}

// T2.16: every D8 and conductor route is allowlisted, gated by the kiosk session, and
// reaches its upstream at the right path.
func TestKioskD8RouteCensus_EveryRouteIsGatedAndReachesItsUpstream(t *testing.T) {
	rig := newD8Rig(t)
	for _, r := range d8Routes {
		rec, hits := rig.do(r.method, r.target, r.body, "")
		if rec.Code != http.StatusUnauthorized || len(hits) != 0 {
			t.Errorf("%s: without a session %s %s answered %d with %d upstream call(s), want 401 and none",
				r.name, r.method, r.target, rec.Code, len(hits))
		}
		rec, hits = rig.do(r.method, r.target, r.body, rig.token)
		if strings.Contains(rec.Body.String(), "NOT_PART_OF_KIOSK_EDITION") {
			t.Errorf("%s: %s %s is not allowlisted", r.name, r.method, r.target)
			continue
		}
		if !reached(hits, r.want) {
			t.Errorf("%s: %s %s reached %v (status %d), want %v", r.name, r.method, r.target, hits, rec.Code, r.want)
		}
	}
}

// Negative controls: a route outside the allowlist is refused with no upstream call, the
// pre-warm is refused at the edge, and the matcher rejects a wrong expectation.
func TestKioskD8RouteCensus_TheCensusCanFail(t *testing.T) {
	rig := newD8Rig(t)
	rec, hits := rig.do("GET", "/api/v1/me/kiosk-census-not-listed", "", rig.token)
	if !strings.Contains(rec.Body.String(), "NOT_PART_OF_KIOSK_EDITION") || len(hits) != 0 {
		t.Errorf("an unlisted route answered %d %s with %d upstream call(s)", rec.Code, rec.Body.String(), len(hits))
	}
	for _, m := range []string{"GET", "POST"} {
		rec, hits = rig.do(m, "/api/v1/kiosk/prewarm", `{}`, rig.token)
		if rec.Code != http.StatusForbidden || len(hits) != 0 {
			t.Errorf("%s /api/v1/kiosk/prewarm answered %d with %d upstream call(s), want 403 and none", m, rec.Code, len(hits))
		}
	}
	_, hits = rig.do("POST", "/api/companion/greeting", `{}`, rig.token)
	for name, wrong := range map[string]d8Hit{
		"service": {"delivery", "POST", "/companion/greeting", ""},
		"method":  {"consumption", "GET", "/companion/greeting", ""},
		"path":    {"consumption", "POST", "/v1/companion/greeting", ""},
		"query":   {"consumption", "POST", "/companion/greeting", "goal_id=g1"},
	} {
		if reached(hits, wrong) {
			t.Errorf("the matcher accepted a wrong %s: %v for %v", name, hits, wrong)
		}
	}
	if reached(nil, d8Routes[0].want) {
		t.Error("the matcher accepted no upstream call")
	}
}
