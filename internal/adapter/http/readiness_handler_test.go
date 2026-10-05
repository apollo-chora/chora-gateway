package httpadapter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/readiness"
)

// The readiness endpoint is an OPERATOR and TENANT-ADMIN surface. These tests
// pin the two things that make it safe to mount: the audience gate, and the
// fact that a route must be in BOTH prefix lists or it is reachable
// unauthenticated (the trap caught in B6 item 3).

func TestReadinessRoute_IsClaimed(t *testing.T) {
	var claimed bool
	for _, p := range GatewayProxyPathPrefixes {
		if p == "/api/v1/admin/readiness" {
			claimed = true
			break
		}
	}
	if !claimed {
		t.Fatal("/api/v1/admin/readiness must be claimed by the gateway proxy mux")
	}
}

func TestReadinessRoute_IsBehindTheJWTGate(t *testing.T) {
	// Claiming a path does NOT gate it: GatewayProxyPathPrefixes and
	// DefaultJWTGatedPrefixes are two separate lists, and a route in one but
	// not the other is reachable with no session.
	var gated bool
	for _, p := range DefaultJWTGatedPrefixes {
		if strings.HasPrefix("/api/v1/admin/readiness", p) {
			gated = true
			break
		}
	}
	if !gated {
		t.Fatal("the readiness path must sit under a JWT-gated prefix")
	}
}

// readinessReq seeds the roles as VALIDATED MESH CLAIMS on the context, which
// is where the gates read them. Setting an x-mesh-user-roles header instead
// would test nothing: a caller-supplied header is exactly what the JWT
// middleware exists to replace, and a gate that trusted one would be the bug.
func readinessReq(roles string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/readiness", nil)
	r.Header.Set("X-Tenant-Id", "11111111-1111-7111-8111-111111111111")
	r.Header.Set("gcid", "22222222-2222-7222-8222-222222222222")
	claims := &servicemesh.MeshClaims{
		GCID:     "22222222-2222-7222-8222-222222222222",
		TenantID: "11111111-1111-7111-8111-111111111111",
	}
	if roles != "" {
		claims.Roles = strings.Split(roles, ",")
	}
	return r.WithContext(withMeshClaims(r.Context(), claims))
}

func TestReadinessHandler_RefusesALearner(t *testing.T) {
	// Fail-closed on the audience. The rows count administrators and read
	// tenant configuration; a learner has no business seeing either, and a
	// missing role header must be a refusal rather than a silent narrow.
	h := &ReadinessHandler{}
	for _, roles := range []string{"", "learner", "author,learner"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, readinessReq(roles))
		if w.Code != http.StatusForbidden {
			t.Errorf("roles %q: status = %d, want 403", roles, w.Code)
		}
	}
}

func TestReadinessHandler_AdmitsOperatorAndTenantAdmin(t *testing.T) {
	// The aggregator is nil here, so an admitted caller must fall through to
	// the unwired 503. A 403 would mean the gate rejected an audience it
	// should admit, and this arm is what makes the refusal test above evidence
	// rather than a gate that refuses everyone.
	h := &ReadinessHandler{}
	for _, roles := range []string{"platform_operator", "tenant_admin", "admin", "owner", "PLATFORM_OPERATOR"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, readinessReq(roles))
		if w.Code == http.StatusForbidden {
			t.Errorf("roles %q was refused; operator and tenant-admin are the audience", roles)
		}
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("roles %q: status = %d, want 503 from the unwired aggregator", roles, w.Code)
		}
	}
}

func TestReadinessHandler_WrongMethodIs405(t *testing.T) {
	h := &ReadinessHandler{}
	r := readinessReq("platform_operator")
	r.Method = http.MethodPost
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func TestReadinessHandler_MissingTenantIs400(t *testing.T) {
	// Every row is scoped to one organisation. Without a tenant there is
	// nothing to report on, and answering with an empty set would look like a
	// healthy instance with no data.
	h := &ReadinessHandler{}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/readiness", nil)
	r.Header.Set("gcid", "22222222-2222-7222-8222-222222222222")
	r = r.WithContext(withMeshClaims(r.Context(), &servicemesh.MeshClaims{Roles: []string{"platform_operator"}}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// stubReadinessAgg returns a canned aggregate so the SUCCESS path is covered.
// Without this the handler tests only ever exercise refusals, and a handler
// that refused everything would pass them all.
type stubReadinessAgg struct {
	body   string
	status int
	err    error
}

func (s stubReadinessAgg) GetReadiness(_ context.Context, _ readiness.AuthCtx) (readiness.Response, error) {
	if s.err != nil {
		return readiness.Response{}, s.err
	}
	return readiness.Response{Status: s.status, Body: []byte(s.body)}, nil
}

func TestReadinessHandler_RelaysTheAggregate(t *testing.T) {
	h := &ReadinessHandler{Agg: stubReadinessAgg{status: http.StatusOK, body: `{"rows":[],"partial":false}`}}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, readinessReq("platform_operator"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	if w.Body.String() != `{"rows":[],"partial":false}` {
		t.Errorf("body = %s, want the aggregate relayed verbatim", w.Body.String())
	}
}

func TestReadinessHandler_AggregatorErrorIs500(t *testing.T) {
	h := &ReadinessHandler{Agg: stubReadinessAgg{err: errors.New("marshal exploded")}}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, readinessReq("platform_operator"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func TestWithReadiness_MountsOnlyItsOwnPath(t *testing.T) {
	// The bridge must not swallow neighbouring paths, or an unrelated route
	// would start answering readiness.
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := WithReadiness(base, stubReadinessAgg{status: http.StatusOK, body: `{}`})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, readinessReq("platform_operator"))
	if w.Code != http.StatusOK {
		t.Errorf("own path: status = %d, want 200", w.Code)
	}

	other := httptest.NewRequest(http.MethodGet, "/api/v1/admin/readiness-other", nil)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, other)
	if w2.Code != http.StatusTeapot {
		t.Errorf("neighbouring path: status = %d, want the base handler's 418", w2.Code)
	}
}

func TestWithReadiness_NilAggregatorLeavesTheRouteUnmounted(t *testing.T) {
	// An unconfigured deployment must not mount a route that can only answer
	// unknown for everything: the base handler stays in charge.
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	w := httptest.NewRecorder()
	WithReadiness(base, nil).ServeHTTP(w, readinessReq("platform_operator"))
	if w.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want the base handler's 418 (route unmounted)", w.Code)
	}
}
