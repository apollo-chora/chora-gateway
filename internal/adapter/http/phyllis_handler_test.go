// Phyllis MVP route layer tests — verify the 11 BFF routes per
// docs/m13/phyllis-mvp-2026-05-08.md Step 1-8 happy-path.
//
// These tests stand up the Phyllis aggregator with httptest.NewServer mocks
// for each downstream service, then issue requests against the BFF mux to
// verify URL paths, status codes, and trace + auth header propagation.
package httpadapter_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/adapter/inmem"
	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

// downstreamStub is a minimal recordable httptest server that returns a fixed
// JSON body + status. Tests can inspect the LastAuth + LastTraceparent fields
// to assert the BFF propagated headers.
type downstreamStub struct {
	*httptest.Server
	calls           atomic.Int64
	lastAuth        string
	lastTraceparent string
	lastPath        string
	lastQuery       string
	lastMethod      string
	lastBody        string
}

func newDownstream(t *testing.T, status int, body string) *downstreamStub {
	t.Helper()
	d := &downstreamStub{}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.calls.Add(1)
		d.lastAuth = r.Header.Get("Authorization")
		d.lastTraceparent = r.Header.Get("traceparent")
		d.lastPath = r.URL.Path
		d.lastQuery = r.URL.RawQuery
		d.lastMethod = r.Method
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			d.lastBody = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(d.Server.Close)
	return d
}

// newPhyllisServer wires a BFF mux with a real Phyllis aggregator pointed at
// the supplied downstream stubs.
func newPhyllisServer(t *testing.T, cfg phyllis.Config, emit phyllis.EventEmitter) *httptest.Server {
	t.Helper()
	cfg.PerCallTimeout = 1 * time.Second
	cfg.AggregationBudget = 5 * time.Second

	routesRepo := inmem.NewRouteRepository()
	sessionsRepo := inmem.NewSessionRepository()
	up := upstream.NewFakeUpstream()
	agg := phyllis.New(cfg, emit)
	router := httpadapter.NewRouterWithPhyllis(routesRepo, sessionsRepo, up, agg)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

// -----------------------------------------------------------------------------
// /api/me — happy path + 401 + 5xx
// -----------------------------------------------------------------------------

func TestPhyllis_GetMe_HappyPath(t *testing.T) {
	identity := newDownstream(t, http.StatusOK, `{"gcid":"phyllis-gcid","email":"p@mtm.sg","display_name":"Phyllis","linked_idps":["google"]}`)
	srv := newPhyllisServer(t, phyllis.Config{IdentityURL: identity.URL}, nil)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/me", nil)
	req.Header.Set("Authorization", "Bearer phyllis-gcid")
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	if identity.lastAuth != "Bearer phyllis-gcid" {
		t.Errorf("Authorization = %q; want pass-through", identity.lastAuth)
	}
	if identity.lastTraceparent == "" {
		t.Errorf("traceparent not propagated to identity")
	}
	if identity.lastPath != "/me" {
		t.Errorf("identity path = %s", identity.lastPath)
	}
}

func TestPhyllis_GetMe_401PassThrough(t *testing.T) {
	identity := newDownstream(t, http.StatusUnauthorized, `{"error":{"code":"INVALID_JWT"}}`)
	srv := newPhyllisServer(t, phyllis.Config{IdentityURL: identity.URL}, nil)

	resp, err := http.Get(srv.URL + "/api/me")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401 pass-through", resp.StatusCode)
	}
}

// -----------------------------------------------------------------------------
// /api/me/roles?course_id=X
// -----------------------------------------------------------------------------

func TestPhyllis_GetMyRoles_PerCourse(t *testing.T) {
	identity := newDownstream(t, http.StatusOK, `{"role":"instructor"}`)
	srv := newPhyllisServer(t, phyllis.Config{IdentityURL: identity.URL}, nil)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/me/roles?course_id=cspo", nil)
	req.Header.Set("Authorization", "Bearer x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(identity.lastQuery, "course_id=cspo") {
		t.Errorf("identity query = %q; want course_id=cspo", identity.lastQuery)
	}
}

// -----------------------------------------------------------------------------
// /api/tenants/me — uses X-Tenant-Id header to resolve
// -----------------------------------------------------------------------------

func TestPhyllis_GetMyTenant_FromHeader(t *testing.T) {
	tenancy := newDownstream(t, http.StatusOK, `{"tenant_id":"mtm-singapore"}`)
	srv := newPhyllisServer(t, phyllis.Config{TenancyURL: tenancy.URL}, nil)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/tenants/me", nil)
	req.Header.Set("Authorization", "Bearer x")
	req.Header.Set("X-Tenant-Id", "mtm-singapore")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	// A6 follow-up (CHO-1545): chora-tenancy serves tenant detail under
	// /api/tenants/{id}, NOT /tenants/{id} — the legacy path 404'd.
	if tenancy.lastPath != "/api/tenants/mtm-singapore" {
		t.Errorf("tenancy path = %s; want /api/tenants/mtm-singapore", tenancy.lastPath)
	}
}

func TestPhyllis_GetMyTenant_NoHeader_400(t *testing.T) {
	srv := newPhyllisServer(t, phyllis.Config{TenancyURL: "http://unused"}, nil)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/tenants/me", nil)
	req.Header.Set("Authorization", "Bearer x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 when X-Tenant-Id missing", resp.StatusCode)
	}
}

// -----------------------------------------------------------------------------
// POST /api/courses — composite
// -----------------------------------------------------------------------------

func TestPhyllis_CreateCourse_Composite(t *testing.T) {
	creation := newDownstream(t, http.StatusOK, `{"atom_id":"atom-root-001"}`)
	delivery := newDownstream(t, http.StatusOK, `{"course_id":"course-001","title":"CSPO"}`)
	srv := newPhyllisServer(t, phyllis.Config{CreationURL: creation.URL, DeliveryURL: delivery.URL}, nil)

	body := strings.NewReader(`{"title":"CSPO"}`)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/api/courses", body)
	req.Header.Set("Authorization", "Bearer x")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d; want 201", resp.StatusCode)
	}
	var got map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got["course_id"] != "course-001" || got["root_atom_id"] != "atom-root-001" {
		t.Errorf("body = %v", got)
	}
}

// -----------------------------------------------------------------------------
// GET /api/catalog — Phyllis Step 5 PublicDiscovery
//
// The catalog route fans out to chora-delivery /v1/courses with a `visibility`
// query param (NOT the legacy /courses?public=true path — that route is
// tenantRequired-gated and 400s an unauthed request).
// -----------------------------------------------------------------------------

func TestPhyllis_GetCatalog_PublicExplicit(t *testing.T) {
	delivery := newDownstream(t, http.StatusOK, `{"items":[{"id":"csm-prep"}],"total":1}`)
	srv := newPhyllisServer(t, phyllis.Config{DeliveryURL: delivery.URL}, nil)

	resp, err := http.Get(srv.URL + "/api/catalog?public=true")

	if err != nil {

		t.Fatal(err)

	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if delivery.lastPath != "/v1/courses" {
		t.Errorf("delivery path = %q; want /v1/courses", delivery.lastPath)
	}
	if !strings.Contains(delivery.lastQuery, "visibility=public") {
		t.Errorf("delivery query = %q; want visibility=public", delivery.lastQuery)
	}
}

// CR2-C2: the catalog route must forward the learner-facing search (`q`) +
// cursor pagination (`first`, `after`) params to chora-delivery /v1/courses
// (which already honours them). Previously the aggregator hand-built the
// downstream URL with only `visibility`, silently dropping these — so the
// /a/catalog page's search box + "Load more" were no-ops end-to-end.
func TestPhyllis_GetCatalog_ForwardsSearchAndPagination(t *testing.T) {
	delivery := newDownstream(t, http.StatusOK, `{"items":[],"page_info":{"has_next_page":false,"end_cursor":""},"total":0}`)
	srv := newPhyllisServer(t, phyllis.Config{DeliveryURL: delivery.URL}, nil)

	resp, err := http.Get(srv.URL + "/api/catalog?public=true&q=agile&first=10&after=cur123")

	if err != nil {

		t.Fatal(err)

	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	if !strings.Contains(delivery.lastQuery, "visibility=public") {
		t.Errorf("delivery query = %q; want visibility=public preserved", delivery.lastQuery)
	}
	for _, want := range []string{"q=agile", "first=10", "after=cur123"} {
		if !strings.Contains(delivery.lastQuery, want) {
			t.Errorf("delivery query = %q; missing forwarded %q", delivery.lastQuery, want)
		}
	}
}

// The demo's actual UNAUTHED public-discovery shape: plain GET /api/catalog
// with no query string and no Authorization header. Must still resolve to
// /v1/courses?visibility=public (never tenant_or_public — an anonymous caller
// must not receive other tenants' tenant_only rows).
func TestPhyllis_GetCatalog_AnonymousPlain(t *testing.T) {
	delivery := newDownstream(t, http.StatusOK, `{"items":[],"total":0}`)
	srv := newPhyllisServer(t, phyllis.Config{DeliveryURL: delivery.URL}, nil)

	resp, err := http.Get(srv.URL + "/api/catalog")

	if err != nil {

		t.Fatal(err)

	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	if delivery.lastPath != "/v1/courses" {
		t.Errorf("delivery path = %q; want /v1/courses", delivery.lastPath)
	}
	if !strings.Contains(delivery.lastQuery, "visibility=public") {
		t.Errorf("anonymous delivery query = %q; want visibility=public", delivery.lastQuery)
	}
}

// -----------------------------------------------------------------------------
// POST /api/enrollments — emits event on success
// -----------------------------------------------------------------------------

func TestPhyllis_CreateEnrollment_EmitsEvent(t *testing.T) {
	delivery := newDownstream(t, http.StatusCreated, `{"enrollment_id":"enr-001"}`)
	var topics []string
	emit := func(_ context.Context, topic string, _ []byte) error {
		topics = append(topics, topic)
		return nil
	}
	srv := newPhyllisServer(t, phyllis.Config{DeliveryURL: delivery.URL}, emit)

	body := strings.NewReader(`{"course_id":"csm-prep","gcid":"phyllis"}`)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/api/enrollments", body)
	req.Header.Set("Authorization", "Bearer x")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if len(topics) != 1 || topics[0] != "chora.delivery.enrollment.created.v1" {
		t.Errorf("emitted = %v", topics)
	}
}

// -----------------------------------------------------------------------------
// GET /api/atoms/{id} — a SINGLE synchronous proxy to chora-creation
// -----------------------------------------------------------------------------

// TestPhyllis_GetAtom_SingleProxyNoSessionSideLoad pins the CHO-1968 contract.
//
// This test used to be TestPhyllis_GetAtom_Parallel and had been RED on main
// ever since `23af9370c` ("fix(gateway): drop dead atom session side-load")
// deliberately removed the consumption fan-out: chora-consumption never had a
// GET /atoms/{id}/session route, so the side-load was a dead call that could
// only ever degrade. The implementation was fixed; the test was not, so it kept
// asserting a `session` key the BFF no longer returns and a downstream call it
// no longer makes. It was carried as "pre-existing, don't chase it" — which is
// how a permanently-red suite stops being read at all.
//
// Rewritten to assert what GetAtom actually contracts today, so it now GUARDS
// the fix instead of contradicting it: one call to chora-creation, no session
// side-load, and a body carrying `atom` only.
func TestPhyllis_GetAtom_SingleProxyNoSessionSideLoad(t *testing.T) {
	creation := newDownstream(t, http.StatusOK, `{"atom_id":"atom-001","title":"x"}`)
	consumption := newDownstream(t, http.StatusOK, `{"session_id":"sess-001"}`)
	srv := newPhyllisServer(t, phyllis.Config{CreationURL: creation.URL, ConsumptionURL: consumption.URL}, nil)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/atoms/atom-001", nil)
	req.Header.Set("Authorization", "Bearer x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	atom, _ := body["atom"].(map[string]any)
	if atom["atom_id"] != "atom-001" {
		t.Errorf("atom = %v, want atom_id atom-001", atom)
	}

	// chora-creation serves the atom under /api/atoms/{id} (its legacy AtomHandler
	// mux), NOT /atoms/{id} — the earlier /atoms/{id} forward 404'd there.
	if creation.lastPath != "/api/atoms/atom-001" {
		t.Errorf("creation path = %q, want /api/atoms/atom-001", creation.lastPath)
	}

	// The heart of CHO-1968: NO session side-load. chora-consumption has no such
	// route, so any call here is dead weight on the atom-open hot path.
	if consumption.lastPath != "" {
		t.Errorf("consumption was called at %q — the atom session side-load is dead "+
			"(chora-consumption has no GET /atoms/{id}/session) and was removed in 23af9370c; "+
			"re-adding it puts a guaranteed-failing call back on the atom-open path",
			consumption.lastPath)
	}
	if _, present := body["session"]; present {
		t.Errorf("body carries a `session` key (%v) — GetAtom is a single proxy to "+
			"chora-creation and returns `atom` only", body["session"])
	}
}

// -----------------------------------------------------------------------------
// POST /api/atoms/{id}/feedback
// -----------------------------------------------------------------------------

func TestPhyllis_SubmitFeedback(t *testing.T) {
	consumption := newDownstream(t, http.StatusAccepted, `{"accepted":true}`)
	srv := newPhyllisServer(t, phyllis.Config{ConsumptionURL: consumption.URL}, nil)

	body := strings.NewReader(`{"quality":4}`)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/api/atoms/atom-001/feedback", body)
	req.Header.Set("Authorization", "Bearer x")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d; want 202", resp.StatusCode)
	}
	if consumption.lastPath != "/atoms/atom-001/feedback" {
		t.Errorf("consumption path = %s", consumption.lastPath)
	}
}

// -----------------------------------------------------------------------------
// POST /api/atoms/{id}/media — E2E-BE-ATOM-GW / E2E-INFRA-ATOM-3
// ADR-156 Phase 1 atom-media signed-URL mint. Verbatim passthrough preserving
// the /api/ prefix (same pattern as PatchAtom + DeleteAtom). chora-creation
// handler is ATOM-1 scope; gateway proxy ships independently.
// -----------------------------------------------------------------------------

func TestPhyllis_MintAtomMediaSignedUrl(t *testing.T) {
	creation := newDownstream(t, http.StatusOK, `{"upload_url":"https://storage.googleapis.com/chora-atom-media-dev/...","expires_at":"2026-05-17T01:00:00Z"}`)
	srv := newPhyllisServer(t, phyllis.Config{CreationURL: creation.URL}, nil)

	body := strings.NewReader(`{"mime":"image/png","size_bytes":12345}`)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/api/atoms/019e2f76-0d67-73b2-ac76-cc6d16f67a62/media", body)
	req.Header.Set("Authorization", "Bearer x")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	// Verbatim passthrough must preserve the /api/ prefix on the downstream
	// call (not strip-prefix); mirrors PatchAtom / DeleteAtom convention.
	if creation.lastPath != "/api/atoms/019e2f76-0d67-73b2-ac76-cc6d16f67a62/media" {
		t.Errorf("creation path = %q; want /api/atoms/{id}/media", creation.lastPath)
	}
	var got map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got["upload_url"] == nil || got["upload_url"] == "" {
		t.Errorf("response missing upload_url; got %v", got)
	}
}

func TestPhyllis_MintAtomMediaSignedUrl_EmptyAtomID_Returns404(t *testing.T) {
	srv := newPhyllisServer(t, phyllis.Config{}, nil)

	// Path with empty atom id segment: /api/atoms//media — phyllis handler
	// requires a non-empty atom id segment.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/api/atoms//media", nil)
	req.Header.Set("Authorization", "Bearer x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("status = %d; want non-200 on empty atom id", resp.StatusCode)
	}
}

// -----------------------------------------------------------------------------
// POST /api/ai/generate
// -----------------------------------------------------------------------------

func TestPhyllis_GenerateAI(t *testing.T) {
	router := newDownstream(t, http.StatusOK, `{"verdict":"approved","content":"..."}`)
	srv := newPhyllisServer(t, phyllis.Config{ModelBrokerRouterURL: router.URL}, nil)

	body := strings.NewReader(`{"prompt":"Generate 10 MCQs"}`)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/api/ai/generate", body)
	req.Header.Set("Authorization", "Bearer x")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if router.lastPath != "/generate" {
		t.Errorf("router path = %s", router.lastPath)
	}
}

// -----------------------------------------------------------------------------
// GET /api/companion/me + GET /api/companion/daily-dose
// -----------------------------------------------------------------------------

func TestPhyllis_GetCompanionMe(t *testing.T) {
	consumption := newDownstream(t, http.StatusOK, `{"companion_id":"fam-001"}`)
	srv := newPhyllisServer(t, phyllis.Config{ConsumptionURL: consumption.URL}, nil)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/companion/me", nil)
	req.Header.Set("Authorization", "Bearer x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if consumption.lastPath != "/companion/me" {
		t.Errorf("path = %s", consumption.lastPath)
	}
}

func TestPhyllis_GetDailyDose(t *testing.T) {
	consumption := newDownstream(t, http.StatusOK, `{"atoms":["a1","a2","a3","a4","a5"]}`)
	srv := newPhyllisServer(t, phyllis.Config{ConsumptionURL: consumption.URL}, nil)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/companion/daily-dose", nil)
	req.Header.Set("Authorization", "Bearer x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if consumption.lastPath != "/companion/daily-dose" {
		t.Errorf("path = %s", consumption.lastPath)
	}
}

// TestPhyllis_GetDailyDose_StampsGcidFromMeshClaims pins the D1.5 fix:
// /api/companion/daily-dose is per-user — chora-consumption's requireContext
// reads the lowercase `gcid` header (returns "gcid required" without it,
// the Phyllis step 8 blocker). When the JWT middleware ran upstream and
// stamped MeshClaims, authCtxFromRequest MUST propagate the GCID so the
// outbound call carries the gcid + X-Tenant-Id headers chora-consumption
// requires.
func TestPhyllis_GetDailyDose_StampsGcidFromMeshClaims(t *testing.T) {
	var gotGcid, gotTenant string
	consumption := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotGcid = r.Header.Get("gcid")
		gotTenant = r.Header.Get("X-Tenant-Id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"atoms":[]}`))
	}))
	t.Cleanup(consumption.Close)

	agg := phyllis.New(phyllis.Config{ConsumptionURL: consumption.URL, PerCallTimeout: time.Second}, nil)
	router := httpadapter.NewRouterWithPhyllis(
		inmem.NewRouteRepository(), inmem.NewSessionRepository(),
		upstream.NewFakeUpstream(), agg,
	)

	r := httptest.NewRequest(http.MethodGet, "/api/companion/daily-dose", nil)
	r.Header.Set("Authorization", "Bearer x")
	// Simulate RequireChoraSessionJWT having run upstream — it stamps MeshClaims.
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(
		r.Context(), "00000000-0000-7000-8000-000000001999", "tenant-phyllis"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if gotGcid != "00000000-0000-7000-8000-000000001999" {
		t.Errorf("lowercase gcid header reaching chora-consumption = %q; want the GCID — without it handleDailyDose 4xxs 'gcid required'", gotGcid)
	}
	if gotTenant != "tenant-phyllis" {
		t.Errorf("X-Tenant-Id header = %q; want tenant-phyllis", gotTenant)
	}
}

// TestPhyllis_GetDailyDose_StampsGcidFromChoraSessionClaims pins the D1.5
// defensive fallback: when MeshClaims was NOT stamped (e.g. a route ordering
// where only the raw ChoraSession claims landed on the context),
// authCtxFromRequest must still recover GCID + TenantID from the validated
// ChoraSession claims so daily-dose stays per-user-correct.
func TestPhyllis_GetDailyDose_StampsGcidFromChoraSessionClaims(t *testing.T) {
	var gotGcid, gotTenant string
	consumption := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotGcid = r.Header.Get("gcid")
		gotTenant = r.Header.Get("X-Tenant-Id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"atoms":[]}`))
	}))
	t.Cleanup(consumption.Close)

	agg := phyllis.New(phyllis.Config{ConsumptionURL: consumption.URL, PerCallTimeout: time.Second}, nil)
	router := httpadapter.NewRouterWithPhyllis(
		inmem.NewRouteRepository(), inmem.NewSessionRepository(),
		upstream.NewFakeUpstream(), agg,
	)

	r := httptest.NewRequest(http.MethodGet, "/api/companion/daily-dose", nil)
	r.Header.Set("Authorization", "Bearer x")
	// ONLY the raw ChoraSession claims on the context — no MeshClaims.
	r = r.WithContext(httpadapter.InjectChoraSessionClaimsForTest(
		r.Context(), "00000000-0000-7000-8000-000000001999", "tenant-phyllis", "phyllis@mightymind.sg"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if gotGcid != "00000000-0000-7000-8000-000000001999" {
		t.Errorf("lowercase gcid header = %q; want GCID recovered from ChoraSession claims", gotGcid)
	}
	if gotTenant != "tenant-phyllis" {
		t.Errorf("X-Tenant-Id header = %q; want tenant recovered from ChoraSession claims", gotTenant)
	}
}

// -----------------------------------------------------------------------------
// Method enforcement
// -----------------------------------------------------------------------------

func TestPhyllis_MethodNotAllowed(t *testing.T) {
	srv := newPhyllisServer(t, phyllis.Config{IdentityURL: "http://unused"}, nil)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/api/me", nil)
	req.Header.Set("Authorization", "Bearer x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405", resp.StatusCode)
	}
}

// -----------------------------------------------------------------------------
// 5xx upstream is normalised to 502
// -----------------------------------------------------------------------------

func TestPhyllis_5xxNormalisedTo502(t *testing.T) {
	identity := newDownstream(t, http.StatusInternalServerError, `{}`)
	srv := newPhyllisServer(t, phyllis.Config{IdentityURL: identity.URL}, nil)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/me", nil)
	req.Header.Set("Authorization", "Bearer x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", resp.StatusCode)
	}
}

// -----------------------------------------------------------------------------
// Existing routes still work — non-regression
// -----------------------------------------------------------------------------

func TestPhyllis_DoesNotBreakExistingRoutes(t *testing.T) {
	srv := newPhyllisServer(t, phyllis.Config{IdentityURL: "http://unused"}, nil)
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz status = %d; want 200", resp.StatusCode)
	}
	resp2, err := http.Get(srv.URL + "/bff/aplus/home")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("/bff/aplus/home status = %d; want 200", resp2.StatusCode)
	}
}

// -----------------------------------------------------------------------------
// Per-route method enforcement matrix
// -----------------------------------------------------------------------------

func TestPhyllis_MethodMatrix_405(t *testing.T) {
	cases := []struct {
		path   string
		method string
	}{
		{"/api/me/roles", http.MethodPost},
		{"/api/tenants/me", http.MethodPost},
		{"/api/courses", http.MethodGet},
		{"/api/catalog", http.MethodPost},
		{"/api/enrollments", http.MethodGet},
		{"/api/ai/generate", http.MethodGet},
		{"/api/companion/me", http.MethodPost},
		{"/api/companion/daily-dose", http.MethodPost},
	}
	srv := newPhyllisServer(t, phyllis.Config{}, nil)
	for _, tc := range cases {
		req, _ := http.NewRequestWithContext(context.Background(), tc.method, srv.URL+tc.path, nil)
		req.Header.Set("Authorization", "Bearer x")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s err: %v", tc.method, tc.path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s status = %d; want 405", tc.method, tc.path, resp.StatusCode)
		}
	}
}

// /api/atoms/{id} — empty segment + PUT not supported. PATCH + DELETE
// are supported per commit 8e57a70b (a508f184 close).
func TestPhyllis_Atoms_BadPathOrMethod(t *testing.T) {
	srv := newPhyllisServer(t, phyllis.Config{}, nil)

	// Empty atom id.
	resp, err := http.Get(srv.URL + "/api/atoms/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty atom id", resp.StatusCode)
	}

	// PUT is genuinely unsupported on /api/atoms/{id} — full-replace is
	// forbidden by ddd-enforcement #4 (AtomRevision append-only). DELETE
	// + PATCH ARE supported now (a508f184 close at commit 8e57a70b).
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, srv.URL+"/api/atoms/atom-001", nil)
	req.Header.Set("Authorization", "Bearer x")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT status = %d; want 405 (full-replace forbidden by ddd-enforcement #4)", resp2.StatusCode)
	}

	// Unsupported sub-path on /api/atoms/{id}/foo.
	req2, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/api/atoms/atom-001/foo", nil)
	req2.Header.Set("Authorization", "Bearer x")
	resp3, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /foo status = %d; want 405", resp3.StatusCode)
	}
}
