// egress_killswitch_handler_test.go — specs for the O+ platform egress
// kill-switch BFF route (CHO-2148).
//
// Two things are load-bearing and neither is the happy path:
//
//  1. The gate FAILS CLOSED on the VALIDATED session roles. This switch disables
//     external web egress for every tenant on the platform.
//
//  2. The proxy STAMPS x-mesh-user-roles. chora-observability re-gates on that
//     header fail-closed, so an unstamped call would be denied upstream and the
//     kill-switch would be permanently unreachable — the CHO-1708 bug class,
//     which phyllis already carries a scar from.
package httpadapter

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
)

const (
	ksGCID   = "00000000-0000-7000-8000-000000001999"
	ksTenant = "00000000-0000-7000-8000-000000000001"
)

// ksRequest builds a request carrying VALIDATED mesh claims (as the JWT gate
// would have attached them) — never a client-supplied role header.
func ksRequest(method, body string, roles ...string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/api/v1/admin/egress/kill-switch", nil)
	} else {
		r = httptest.NewRequest(method, "/api/v1/admin/egress/kill-switch", bytes.NewBufferString(body))
	}
	claims := &servicemesh.MeshClaims{GCID: ksGCID, TenantID: ksTenant, Roles: roles}
	return r.WithContext(withMeshClaims(r.Context(), claims))
}

func newObsStub(t *testing.T, fn http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(fn)
	t.Cleanup(s.Close)
	return s
}

// --- gate: fail CLOSED -----------------------------------------------------

func TestEgressKillSwitch_NoRoles_403_NoUpstreamCall(t *testing.T) {
	called := false
	obs := newObsStub(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := NewEgressKillSwitchHandler(obs.URL, nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, ksRequest(http.MethodGet, ""))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — an empty role set MUST deny", w.Code)
	}
	if called {
		t.Fatal("a refused caller must never reach the upstream")
	}
}

func TestEgressKillSwitch_TenantAdmin_403_OperatorOnly(t *testing.T) {
	called := false
	obs := newObsStub(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := NewEgressKillSwitchHandler(obs.URL, nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, ksRequest(http.MethodPatch, `{"engaged":true}`, "TENANT_ADMIN", "OWNER"))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — a tenant admin must not disable egress "+
			"for the whole platform", w.Code)
	}
	if called {
		t.Fatal("a refused caller must never reach the upstream")
	}
}

// --- the mesh-roles stamping (the CHO-1708 bug class) ----------------------

func TestEgressKillSwitch_Operator_StampsMeshRolesUpstream(t *testing.T) {
	var seenRoles, seenGCID, seenMethod, seenPath string
	obs := newObsStub(t, func(w http.ResponseWriter, r *http.Request) {
		seenRoles = r.Header.Get(servicemesh.HeaderUserRoles)
		seenGCID = r.Header.Get(servicemesh.HeaderGCID)
		seenMethod = r.Method
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"engaged":false}`))
	})
	h := NewEgressKillSwitchHandler(obs.URL, nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, ksRequest(http.MethodGet, "", "learner", "platform_operator"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if seenPath != "/api/v1/admin/egress/kill-switch" {
		t.Errorf("upstream path = %q, want /api/v1/admin/egress/kill-switch — it is "+
			"enumerated verbatim in obs's Istio policy (no wildcards)", seenPath)
	}
	if seenMethod != http.MethodGet {
		t.Errorf("method = %s, want GET", seenMethod)
	}
	if !strings.Contains(strings.ToLower(seenRoles), "platform_operator") {
		t.Fatalf("x-mesh-user-roles = %q — it MUST carry platform_operator. "+
			"chora-observability re-gates on this header fail-closed, so an unstamped "+
			"call would 403 upstream and the kill-switch would be unreachable "+
			"(ObservabilityClient does exactly this — its AuthCtx has no Roles field)",
			seenRoles)
	}
	if seenGCID != ksGCID {
		t.Errorf("chora-gcid = %q, want %q", seenGCID, ksGCID)
	}
}

func TestEgressKillSwitch_Operator_PatchForwardsBody(t *testing.T) {
	var seenBody []byte
	var seenMethod string
	obs := newObsStub(t, func(w http.ResponseWriter, r *http.Request) {
		seenMethod = r.Method
		seenBody, _ = readAllLimited(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"engaged":true,"reason":"incident-42"}`))
	})
	h := NewEgressKillSwitchHandler(obs.URL, nil)
	w := httptest.NewRecorder()

	body := `{"engaged":true,"reason":"incident-42"}`
	h.ServeHTTP(w, ksRequest(http.MethodPatch, body, "platform_operator"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if seenMethod != http.MethodPatch {
		t.Errorf("method = %s, want PATCH", seenMethod)
	}
	if string(seenBody) != body {
		t.Errorf("upstream body = %s, want %s (forwarded verbatim)", seenBody, body)
	}
}

// The upstream's own fail-closed 403 must pass through, not be masked as a 500.
func TestEgressKillSwitch_UpstreamForbidden_PassesThrough(t *testing.T) {
	obs := newObsStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"OBS_FORBIDDEN","message":"operator only"}`))
	})
	h := NewEgressKillSwitchHandler(obs.URL, nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, ksRequest(http.MethodGet, "", "platform_operator"))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want the upstream's 403 passed through verbatim", w.Code)
	}
}

// --- misc ------------------------------------------------------------------

func TestEgressKillSwitch_MethodNotAllowed_405(t *testing.T) {
	obs := newObsStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := NewEgressKillSwitchHandler(obs.URL, nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, ksRequest(http.MethodDelete, "", "platform_operator"))

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

// An unconfigured upstream must 503, never a silent "not engaged" — that answer
// is indistinguishable from a real one and would hide that the platform has no
// working override.
func TestEgressKillSwitch_UnwiredUpstream_503(t *testing.T) {
	h := NewEgressKillSwitchHandler("", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, ksRequest(http.MethodGet, "", "platform_operator"))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the upstream is unconfigured", w.Code)
	}
}

// WithEgressKillSwitch must not swallow unrelated paths.
func TestWithEgressKillSwitch_FallsThroughOnOtherPaths(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	obs := newObsStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	composed := WithEgressKillSwitch(base, NewEgressKillSwitchHandler(obs.URL, nil))

	w := httptest.NewRecorder()
	composed.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/tenants/me/addons", nil))

	if w.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want the base handler (418) — the composer must only "+
			"claim its own path", w.Code)
	}
}

// The route must be covered by the JWT gate. /api/v1/admin/ is a blanket prefix
// in DefaultJWTGatedPrefixes; assert it actually matches, so a future edit to
// that list cannot silently expose the kill-switch unauthenticated.
func TestEgressKillSwitch_IsJWTGated(t *testing.T) {
	const path = "/api/v1/admin/egress/kill-switch"
	covered := false
	for _, p := range DefaultJWTGatedPrefixes {
		if strings.HasPrefix(path, p) {
			covered = true
			break
		}
	}
	if !covered {
		t.Fatalf("%s is NOT covered by DefaultJWTGatedPrefixes — the platform egress "+
			"kill-switch would be reachable unauthenticated", path)
	}
}

// readAllLimited reads a bounded request body in the test stub.
func readAllLimited(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(io.LimitReader(r.Body, 1<<20))
	return buf.Bytes(), err
}
