// companion_suspension_handler_test.go: specs for the O+ Learning Companion
// containment BFF route (ADR-252 via ADR-254 D7).
//
// Load-bearing, neither the happy path: (1) the gate FAILS CLOSED on the
// VALIDATED session roles (operator / auditor / admin / owner; anything else
// denies before any upstream call); (2) the proxy STAMPS x-mesh-user-roles and
// X-Tenant-Id, because chora-observability re-gates on the roles fail-closed
// and enforces the scope matrix itself (platform = operator only; tenant =
// admin / owner of THAT tenant or the operator).
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

func csRequest(method, body string, roles ...string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/api/v1/admin/companion/suspension", nil)
	} else {
		r = httptest.NewRequest(method, "/api/v1/admin/companion/suspension", bytes.NewBufferString(body))
	}
	claims := &servicemesh.MeshClaims{GCID: ksGCID, TenantID: ksTenant, Roles: roles}
	return r.WithContext(withMeshClaims(r.Context(), claims))
}

func TestCompanionSuspension_NoRoles_403_NoUpstreamCall(t *testing.T) {
	called := false
	obs := newObsStub(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := NewCompanionSuspensionHandler(obs.URL, nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, csRequest(http.MethodGet, ""))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: an empty role set MUST deny", w.Code)
	}
	if called {
		t.Fatal("a refused caller must never reach the upstream")
	}
}

func TestCompanionSuspension_Learner_403_NoUpstreamCall(t *testing.T) {
	called := false
	obs := newObsStub(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := NewCompanionSuspensionHandler(obs.URL, nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, csRequest(http.MethodPatch, `{"scope":"tenant","engaged":true,"reason":"x"}`, "learner", "instructor"))

	if w.Code != http.StatusForbidden || called {
		t.Fatalf("status = %d called=%v, want 403 with no upstream call", w.Code, called)
	}
}

func TestCompanionSuspension_AllowedRoles_StampMeshRolesAndTenantUpstream(t *testing.T) {
	for _, role := range []string{"platform_operator", "auditor", "admin", "owner", "OWNER"} {
		t.Run(role, func(t *testing.T) {
			var seenRoles, seenGCID, seenTenant, seenPath, seenMethod string
			obs := newObsStub(t, func(w http.ResponseWriter, r *http.Request) {
				seenRoles = r.Header.Get(servicemesh.HeaderUserRoles)
				seenGCID = r.Header.Get(servicemesh.HeaderGCID)
				seenTenant = r.Header.Get("X-Tenant-Id")
				seenPath = r.URL.Path
				seenMethod = r.Method
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"platform":[],"tenant":[]}`))
			})
			h := NewCompanionSuspensionHandler(obs.URL, nil)
			w := httptest.NewRecorder()

			h.ServeHTTP(w, csRequest(http.MethodGet, "", "learner", role))

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(strings.ToLower(seenRoles), strings.ToLower(role)) {
				t.Fatalf("upstream must receive the validated roles (got %q): the observability gate fails closed without them", seenRoles)
			}
			if seenGCID != ksGCID || seenTenant != ksTenant {
				t.Fatalf("upstream identity headers wrong: gcid=%q tenant=%q", seenGCID, seenTenant)
			}
			if seenPath != "/api/v1/admin/companion/suspension" || seenMethod != http.MethodGet {
				t.Fatalf("upstream path/method wrong: %s %s", seenMethod, seenPath)
			}
			if !strings.Contains(w.Body.String(), `"platform"`) {
				t.Fatalf("upstream body must be passed through verbatim: %s", w.Body.String())
			}
		})
	}
}

func TestCompanionSuspension_Patch_ForwardsBodyAndUpstreamStatus(t *testing.T) {
	var seenBody string
	obs := newObsStub(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden) // observability enforces the scope matrix itself
		_, _ = w.Write([]byte(`{"error":{"code":"OBS_FORBIDDEN"}}`))
	})
	h := NewCompanionSuspensionHandler(obs.URL, nil)
	w := httptest.NewRecorder()

	body := `{"scope":"platform","engaged":true,"reason":"incident 42"}`
	h.ServeHTTP(w, csRequest(http.MethodPatch, body, "admin"))

	if seenBody != body {
		t.Fatalf("PATCH body must be forwarded verbatim, got %q", seenBody)
	}
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "OBS_FORBIDDEN") {
		t.Fatalf("upstream status + body must pass through (scope matrix lives upstream): %d %s", w.Code, w.Body.String())
	}
}

func TestCompanionSuspension_Unwired_503(t *testing.T) {
	h := NewCompanionSuspensionHandler("", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, csRequest(http.MethodGet, "", "platform_operator"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: unwired must never answer a plausible default", w.Code)
	}
}

func TestCompanionSuspension_MethodNotAllowed(t *testing.T) {
	obs := newObsStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := NewCompanionSuspensionHandler(obs.URL, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, csRequest(http.MethodDelete, "", "platform_operator"))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func TestCompanionSuspension_NoGCID_401(t *testing.T) {
	obs := newObsStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := NewCompanionSuspensionHandler(obs.URL, nil)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/companion/suspension", nil)
	r = r.WithContext(withMeshClaims(r.Context(), &servicemesh.MeshClaims{TenantID: ksTenant, Roles: []string{"platform_operator"}}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestWithCompanionSuspension_RoutesOnlyItsPath(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	obs := newObsStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := WithCompanionSuspension(base, NewCompanionSuspensionHandler(obs.URL, nil))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, csRequest(http.MethodGet, "", "platform_operator"))
	if w.Code != http.StatusOK {
		t.Fatalf("companion path must be served by the proxy, got %d", w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/egress/kill-switch", nil))
	if w.Code != http.StatusTeapot {
		t.Fatalf("other paths must fall through to base, got %d", w.Code)
	}
	if WithCompanionSuspension(base, nil) == nil {
		t.Fatal("nil handler must pass through base (env-driven dev opt-out)")
	}
}
