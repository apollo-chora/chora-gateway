// tenancy_ownership_routes_test.go: the BFF surface for the ownership handover
// (UX Track U, E3 slice 6, S7-B5).
//
// These specs are about ROUTING and GATING, not about the handover rules, which
// live in chora-tenancy and are specified there. What matters here is that the
// right upstream path is called, that the caller's identity comes from the
// validated session rather than the request, and that a tenant-less operator is
// refused by name on the me-routes while still reaching the override.
package httpadapter

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

const (
	ownRoutesTenant  = "aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaaa"
	ownRoutesGCID    = "bbbbbbbb-bbbb-7bbb-8bbb-bbbbbbbbbbbb"
	ownRoutesNominee = "cccccccc-cccc-7ccc-8ccc-cccccccccccc"
	ownRoutesOffer   = "dddddddd-dddd-7ddd-8ddd-dddddddddddd"
	ownRoutesTarget  = "eeeeeeee-eeee-7eee-8eee-eeeeeeeeeeee"
)

// upstreamSpy stands in for chora-tenancy and records what the BFF sent.
type upstreamSpy struct {
	method, path string
	tenantHdr    string
	gcidHdr      string
	rolesHdr     string
	body         string
	status       int
	respBody     string
	calls        int
}

func newOwnershipBFF(t *testing.T, spy *upstreamSpy) (http.Handler, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spy.calls++
		spy.method, spy.path = r.Method, r.URL.Path
		spy.tenantHdr = r.Header.Get("X-Tenant-Id")
		spy.gcidHdr = r.Header.Get("gcid")
		spy.rolesHdr = r.Header.Get(servicemesh.HeaderUserRoles)
		b, _ := io.ReadAll(r.Body)
		spy.body = string(b)
		status := spy.status
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if spy.respBody != "" {
			_, _ = w.Write([]byte(spy.respBody))
		} else {
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)

	ph := NewPhyllisHandler(phyllis.New(phyllis.Config{TenancyURL: srv.URL}, nil))
	mux := http.NewServeMux()
	registerOwnershipRoutes(mux, ph)
	return mux, srv
}

func ownRoutesReq(method, path, body string, claims *servicemesh.MeshClaims) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if claims != nil {
		r = r.WithContext(withMeshClaims(r.Context(), claims))
	}
	return r
}

func ownRoutesClaims() *servicemesh.MeshClaims {
	return &servicemesh.MeshClaims{
		GCID: ownRoutesGCID, TenantID: ownRoutesTenant, Roles: []string{"owner", "admin"},
	}
}

func ownRoutesOperatorClaims() *servicemesh.MeshClaims {
	// Tenant-less on purpose: PLATFORM_OPERATOR holds no membership row.
	return &servicemesh.MeshClaims{
		GCID: ownRoutesGCID, TenantID: "", Roles: []string{"Platform_Operator"},
	}
}

func ownRoutesCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope %q: %v", rec.Body.String(), err)
	}
	return env.Error.Code
}

// --- the me-routes ----------------------------------------------------------

func TestOwnershipBFF_CreateForwardsToTenancyWithSessionIdentity(t *testing.T) {
	spy := &upstreamSpy{status: http.StatusCreated, respBody: `{"offer_id":"` + ownRoutesOffer + `"}`}
	mux, _ := newOwnershipBFF(t, spy)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, ownRoutesReq(http.MethodPost, "/api/v1/tenants/me/ownership/offers",
		`{"to_gcid":"`+ownRoutesNominee+`"}`, ownRoutesClaims()))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if spy.path != "/api/v1/tenants/me/ownership/offers" || spy.method != http.MethodPost {
		t.Errorf("upstream %s %s", spy.method, spy.path)
	}
	// The identity chora-tenancy gates on comes from the validated session.
	if spy.tenantHdr != ownRoutesTenant {
		t.Errorf("X-Tenant-Id = %q, want %q", spy.tenantHdr, ownRoutesTenant)
	}
	if spy.gcidHdr != ownRoutesGCID {
		t.Errorf("gcid = %q, want %q", spy.gcidHdr, ownRoutesGCID)
	}
	if !strings.Contains(spy.body, ownRoutesNominee) {
		t.Errorf("body = %q, want the nominee forwarded verbatim", spy.body)
	}
}

func TestOwnershipBFF_GetForwardsToTheSingularOfferPath(t *testing.T) {
	spy := &upstreamSpy{respBody: `{"offer_id":"` + ownRoutesOffer + `","is_live":true}`}
	mux, _ := newOwnershipBFF(t, spy)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, ownRoutesReq(http.MethodGet,
		"/api/v1/tenants/me/ownership/offer", "", ownRoutesClaims()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if spy.path != "/api/v1/tenants/me/ownership/offer" || spy.method != http.MethodGet {
		t.Errorf("upstream %s %s", spy.method, spy.path)
	}
}

// The three answer verbs each carry the offer id through to chora-tenancy. A
// verb that dropped it would act on "whatever is pending", which is the race
// the id exists to close.
func TestOwnershipBFF_SettleVerbsCarryTheOfferID(t *testing.T) {
	for _, verb := range []string{"accept", "decline", "revoke"} {
		spy := &upstreamSpy{}
		mux, _ := newOwnershipBFF(t, spy)

		rec := httptest.NewRecorder()
		path := "/api/v1/tenants/me/ownership/offers/" + ownRoutesOffer + "/" + verb
		mux.ServeHTTP(rec, ownRoutesReq(http.MethodPost, path, "", ownRoutesClaims()))

		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 (body %s)", verb, rec.Code, rec.Body.String())
			continue
		}
		if spy.path != path {
			t.Errorf("%s: upstream path = %q, want %q", verb, spy.path, path)
		}
		if spy.method != http.MethodPost {
			t.Errorf("%s: upstream method = %q", verb, spy.method)
		}
	}
}

// An offer id from the path must not be usable to reach outside the subtree.
func TestOwnershipBFF_SettleRejectsAnOfferIDThatIsNotAUUID(t *testing.T) {
	spy := &upstreamSpy{}
	mux, _ := newOwnershipBFF(t, spy)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, ownRoutesReq(http.MethodPost,
		"/api/v1/tenants/me/ownership/offers/not-a-uuid/accept", "", ownRoutesClaims()))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if spy.calls != 0 {
		t.Error("the upstream was called with an unvalidated offer id")
	}
}

// The verb set is closed. An open one would let a caller name any sub-path on
// chora-tenancy's offers subtree, which is a different route than the three
// this BFF means to expose.
func TestOwnershipBFF_SettleRejectsAnUnknownVerb(t *testing.T) {
	spy := &upstreamSpy{}
	mux, _ := newOwnershipBFF(t, spy)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, ownRoutesReq(http.MethodPost,
		"/api/v1/tenants/me/ownership/offers/"+ownRoutesOffer+"/frobnicate", "", ownRoutesClaims()))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if spy.calls != 0 {
		t.Error("the upstream was called with an unknown verb")
	}
}

// A validated session with no active tenant is refused by name, not proxied
// into a downstream 401. Same rule as the wizard routes (E1).
func TestOwnershipBFF_MeRoutesRefuseATenantlessSession(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/tenants/me/ownership/offer"},
		{http.MethodPost, "/api/v1/tenants/me/ownership/offers"},
		{http.MethodPost, "/api/v1/tenants/me/ownership/offers/" + ownRoutesOffer + "/accept"},
		{http.MethodPost, "/api/v1/tenants/me/ownership/offers/" + ownRoutesOffer + "/decline"},
		{http.MethodPost, "/api/v1/tenants/me/ownership/offers/" + ownRoutesOffer + "/revoke"},
	} {
		spy := &upstreamSpy{}
		mux, _ := newOwnershipBFF(t, spy)

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, ownRoutesReq(tc.method, tc.path, `{"to_gcid":"x"}`,
			&servicemesh.MeshClaims{GCID: ownRoutesGCID, TenantID: ""}))

		if rec.Code != http.StatusConflict {
			t.Errorf("%s %s: status = %d, want 409 (body %s)", tc.method, tc.path, rec.Code, rec.Body.String())
			continue
		}
		if got := ownRoutesCode(t, rec); got != "GATEWAY_NO_ACTIVE_TENANT" {
			t.Errorf("%s %s: code = %q, want GATEWAY_NO_ACTIVE_TENANT", tc.method, tc.path, got)
		}
		if spy.calls != 0 {
			t.Errorf("%s %s: the upstream was called for a tenant-less session", tc.method, tc.path)
		}
	}
}

// --- the operator override --------------------------------------------------

func TestOwnershipBFF_OverrideForwardsThePathTenantAndTheOperatorRoles(t *testing.T) {
	spy := &upstreamSpy{status: http.StatusCreated}
	mux, _ := newOwnershipBFF(t, spy)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, ownRoutesReq(http.MethodPost,
		"/api/v1/admin/tenants/"+ownRoutesTarget+"/ownership/offers",
		`{"to_gcid":"`+ownRoutesNominee+`","reason":"the owner left"}`, ownRoutesOperatorClaims()))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	want := "/api/v1/admin/tenants/" + ownRoutesTarget + "/ownership/offers"
	if spy.path != want {
		t.Errorf("upstream path = %q, want %q", spy.path, want)
	}
	// chora-tenancy gates on the roles header, so it has to arrive.
	if !strings.Contains(strings.ToLower(spy.rolesHdr), "platform_operator") {
		t.Errorf("x-mesh-user-roles = %q, want the operator role forwarded", spy.rolesHdr)
	}
	if !strings.Contains(spy.body, "the owner left") {
		t.Errorf("body = %q, want the reason forwarded", spy.body)
	}
}

// The override is NOT behind requireActiveTenant: the operator is tenant-less
// by design, so gating it on an active tenant would make the route unreachable
// by the only role allowed to use it.
func TestOwnershipBFF_OverrideReachableByATenantlessOperator(t *testing.T) {
	spy := &upstreamSpy{status: http.StatusCreated}
	mux, _ := newOwnershipBFF(t, spy)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, ownRoutesReq(http.MethodPost,
		"/api/v1/admin/tenants/"+ownRoutesTarget+"/ownership/offers",
		`{"to_gcid":"`+ownRoutesNominee+`","reason":"r"}`, ownRoutesOperatorClaims()))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if spy.calls != 1 {
		t.Errorf("upstream calls = %d, want 1", spy.calls)
	}
}

// Defence in depth: the gateway refuses a non-operator before proxying, even
// though chora-tenancy gates independently. A 403 that never leaves the BFF is
// also one fewer cross-service call on a hostile request.
func TestOwnershipBFF_OverrideRefusesANonOperator(t *testing.T) {
	spy := &upstreamSpy{}
	mux, _ := newOwnershipBFF(t, spy)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, ownRoutesReq(http.MethodPost,
		"/api/v1/admin/tenants/"+ownRoutesTarget+"/ownership/offers",
		`{"to_gcid":"`+ownRoutesNominee+`","reason":"r"}`, ownRoutesClaims()))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if got := ownRoutesCode(t, rec); got != "GATEWAY_OPERATOR_REQUIRED" {
		t.Errorf("code = %q, want GATEWAY_OPERATOR_REQUIRED", got)
	}
	if spy.calls != 0 {
		t.Error("the upstream was called for a non-operator")
	}
}

func TestOwnershipBFF_OverrideRejectsATenantIDThatIsNotAUUID(t *testing.T) {
	spy := &upstreamSpy{}
	mux, _ := newOwnershipBFF(t, spy)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, ownRoutesReq(http.MethodPost,
		"/api/v1/admin/tenants/not-a-uuid/ownership/offers",
		`{"to_gcid":"`+ownRoutesNominee+`","reason":"r"}`, ownRoutesOperatorClaims()))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if spy.calls != 0 {
		t.Error("the upstream was called with an unvalidated tenant id")
	}
}

// --- method discipline ------------------------------------------------------

// The me-routes carry a tenanted session here on purpose: requireActiveTenant
// wraps the handler, so a tenant-less session on a wrong-method request is a
// 409 about the session rather than a 405 about the verb. That ordering is the
// guard doing its job, and testing the verb needs a session that clears it.
func TestOwnershipBFF_405OnTheWrongVerb(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		claims       *servicemesh.MeshClaims
	}{
		{http.MethodGet, "/api/v1/tenants/me/ownership/offers", ownRoutesClaims()},
		{http.MethodPost, "/api/v1/tenants/me/ownership/offer", ownRoutesClaims()},
		{http.MethodGet, "/api/v1/tenants/me/ownership/offers/" + ownRoutesOffer + "/accept", ownRoutesClaims()},
		{http.MethodGet, "/api/v1/admin/tenants/" + ownRoutesTarget + "/ownership/offers", ownRoutesOperatorClaims()},
	} {
		spy := &upstreamSpy{}
		mux, _ := newOwnershipBFF(t, spy)

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, ownRoutesReq(tc.method, tc.path, "", tc.claims))

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: status = %d, want 405", tc.method, tc.path, rec.Code)
		}
		if spy.calls != 0 {
			t.Errorf("%s %s: the upstream was called on a rejected verb", tc.method, tc.path)
		}
	}
}

// --- the third matcher ------------------------------------------------------

// Three things match differently in this service and all three have to agree:
// DefaultJWTGatedPrefixes by HasPrefix, matchesGatewayProxyPath by exact path
// plus a trailing-slash subtree walk, and the legacy route repository by exact
// path. The census covers the first and third. This covers the second, and it
// calls the real function rather than reading the prefix list, because that
// function carries carve-outs and subtree loops a list read cannot see.
//
// If the bridge ever claimed one of these paths it would shadow the
// registration above, and the symptom would be a 404 or a proxy to the wrong
// service rather than a test failure anywhere else.
func TestOwnershipBFF_TheProxyBridgeDoesNotClaimTheseRoutes(t *testing.T) {
	for _, p := range []string{
		PathMeOwnershipOfferBFF,
		PathMeOwnershipOffersBFF,
		"/api/v1/tenants/me/ownership/offers/" + ownRoutesOffer + "/accept",
		"/api/v1/tenants/me/ownership/offers/" + ownRoutesOffer + "/decline",
		"/api/v1/tenants/me/ownership/offers/" + ownRoutesOffer + "/revoke",
		"/api/v1/admin/tenants/" + ownRoutesTarget + "/ownership/offers",
	} {
		if matchesGatewayProxyPath(p) {
			t.Errorf("the gatewayproxy bridge claims %q, which shadows the ownership registration", p)
		}
	}
}

// The mirror of the above: the paths this BFF sends UPSTREAM must be the paths
// chora-tenancy actually mounts. A rename on either side is otherwise a
// production 404 that no test in either service would catch, because each one
// only knows its own half.
func TestOwnershipBFF_UpstreamPathsMatchTheTenancyMounts(t *testing.T) {
	spy := &upstreamSpy{status: http.StatusCreated}
	mux, _ := newOwnershipBFF(t, spy)

	for _, tc := range []struct {
		method, path, wantUpstream string
		claims                     *servicemesh.MeshClaims
	}{
		{http.MethodGet, PathMeOwnershipOfferBFF,
			"/api/v1/tenants/me/ownership/offer", ownRoutesClaims()},
		{http.MethodPost, PathMeOwnershipOffersBFF,
			"/api/v1/tenants/me/ownership/offers", ownRoutesClaims()},
		{http.MethodPost, "/api/v1/tenants/me/ownership/offers/" + ownRoutesOffer + "/accept",
			"/api/v1/tenants/me/ownership/offers/" + ownRoutesOffer + "/accept", ownRoutesClaims()},
		{http.MethodPost, "/api/v1/admin/tenants/" + ownRoutesTarget + "/ownership/offers",
			"/api/v1/admin/tenants/" + ownRoutesTarget + "/ownership/offers", ownRoutesOperatorClaims()},
	} {
		spy.path = ""
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, ownRoutesReq(tc.method, tc.path, `{"to_gcid":"x","reason":"r"}`, tc.claims))
		if spy.path != tc.wantUpstream {
			t.Errorf("%s %s proxied to %q, want %q", tc.method, tc.path, spy.path, tc.wantUpstream)
		}
	}
}
