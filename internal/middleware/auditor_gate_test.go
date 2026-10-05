// auditor_gate_test.go — table-driven coverage for the auditor role gate.
//
// Verifies:
//   - role-bearing requests under /bff/oplus/* are admitted
//   - role-less or wrong-role requests get 403 with the canonical error body
//   - non-/bff/oplus paths pass through untouched even without auditor
//   - the role resolver canonicalises both []string and `+`-joined strings
//   - nil resolver fails closed on /bff/oplus/* (defensive)
package middleware_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apollo-chora/chora-common/auth/chorasession"
	"github.com/apollo-chora/chora-common/auth/servicemesh"
	"github.com/apollo-chora/chora-gateway/internal/middleware"
)

// rolesCtxKey is a private context key used by the test fixture's
// inline role resolver. Keeps tests independent of the real chora-session
// claim parser.
type rolesCtxKey struct{}

// fixtureResolver pulls a []string roles slice out of the request context.
func fixtureResolver(r *http.Request) []string {
	v, _ := r.Context().Value(rolesCtxKey{}).([]string)
	return v
}

// fixtureHandler is the inner handler we wrap with AuditorGate — records
// that it ran so the test can assert pass-through.
type fixtureHandler struct{ called bool }

func (f *fixtureHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	f.called = true
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func TestHasAuditorRole(t *testing.T) {
	cases := []struct {
		name  string
		roles []string
		want  bool
	}{
		{"empty", []string{}, false},
		{"nil", nil, false},
		{"single auditor", []string{"auditor"}, true},
		{"mixed includes auditor", []string{"learner", "auditor"}, true},
		{"case-insensitive", []string{"AUDITOR"}, true},
		{"whitespace-tolerant", []string{"  auditor  "}, true},
		{"only learner", []string{"learner"}, false},
		{"admin not enough", []string{"admin"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := middleware.HasAuditorRole(c.roles); got != c.want {
				t.Errorf("HasAuditorRole(%v) = %v; want %v", c.roles, got, c.want)
			}
		})
	}
}

// TestHasOPlusAccess covers the widened O+ audience: auditor OR admin OR
// owner grants access; learner + instructor do not.
func TestHasOPlusAccess(t *testing.T) {
	cases := []struct {
		name  string
		roles []string
		want  bool
	}{
		{"empty", []string{}, false},
		{"nil", nil, false},
		{"auditor", []string{"auditor"}, true},
		{"admin", []string{"admin"}, true},
		{"owner", []string{"owner"}, true},
		{"admin case-insensitive", []string{"ADMIN"}, true},
		{"admin whitespace-tolerant", []string{"  admin  "}, true},
		{"mixed includes admin", []string{"learner", "admin"}, true},
		{"learner only", []string{"learner"}, false},
		{"instructor only", []string{"instructor"}, false},
		{"learner+instructor", []string{"learner", "instructor"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := middleware.HasOPlusAccess(c.roles); got != c.want {
				t.Errorf("HasOPlusAccess(%v) = %v; want %v", c.roles, got, c.want)
			}
		})
	}
}

func TestRolesFromAny(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want []string
	}{
		{"nil", nil, nil},
		{"empty slice", []string{}, []string{}},
		{"slice", []string{"learner", "auditor"}, []string{"learner", "auditor"}},
		{"slice trims blanks", []string{" ", "auditor", ""}, []string{"auditor"}},
		{"any slice from JSON", []any{"learner", "auditor"}, []string{"learner", "auditor"}},
		{"plus-joined string", "learner+auditor", []string{"learner", "auditor"}},
		{"comma-joined string", "learner,auditor", []string{"learner", "auditor"}},
		{"single string", "auditor", []string{"auditor"}},
		{"empty string", "", nil},
		{"unrelated type", 42, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := middleware.RolesFromAny(c.in)
			if !equalStrings(got, c.want) {
				t.Errorf("RolesFromAny(%v) = %v; want %v", c.in, got, c.want)
			}
		})
	}
}

// TestAuditorGate_AdmitsAuditorClaim verifies the happy path — auditor in
// roles → 200 + inner handler called.
func TestAuditorGate_AdmitsAuditorClaim(t *testing.T) {
	inner := &fixtureHandler{}
	gate := middleware.AuditorGate(fixtureResolver, inner)

	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/dashboard", nil)
	ctx := context.WithValue(req.Context(), rolesCtxKey{}, []string{"learner", "auditor"})
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	gate.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rr.Code)
	}
	if !inner.called {
		t.Error("inner handler not called")
	}
}

// TestAuditorGate_AdmitsAdminClaim verifies the audience widening: an admin
// (and owner) is a valid O+ audience per the arch doc, not just an auditor.
func TestAuditorGate_AdmitsAdminClaim(t *testing.T) {
	for _, role := range []string{"admin", "owner"} {
		t.Run(role, func(t *testing.T) {
			inner := &fixtureHandler{}
			gate := middleware.AuditorGate(fixtureResolver, inner)

			req := httptest.NewRequest(http.MethodGet, "/bff/oplus/dashboard", nil)
			ctx := context.WithValue(req.Context(), rolesCtxKey{}, []string{"learner", role})
			req = req.WithContext(ctx)
			rr := httptest.NewRecorder()
			gate.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Errorf("status = %d; want 200 (O+ admits %s)", rr.Code, role)
			}
			if !inner.called {
				t.Errorf("inner handler not called for %s", role)
			}
		})
	}
}

// TestAuditorGate_RejectsMissingAuditorClaim verifies that a request
// without auditor returns 403 + the canonical error envelope.
func TestAuditorGate_RejectsMissingAuditorClaim(t *testing.T) {
	inner := &fixtureHandler{}
	gate := middleware.AuditorGate(fixtureResolver, inner)

	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/governance", nil)
	ctx := context.WithValue(req.Context(), rolesCtxKey{}, []string{"learner"})
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	gate.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403", rr.Code)
	}
	if inner.called {
		t.Error("inner handler should not be called when gate rejects")
	}
	body, _ := io.ReadAll(rr.Body)
	var parsed map[string]string
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("body unmarshal: %v; body=%s", err, string(body))
	}
	if parsed["error"] != "auditor_role_required" {
		t.Errorf("error = %q; want auditor_role_required", parsed["error"])
	}
}

// TestAuditorGate_NoRolesReturns403 verifies that a request without any
// resolved roles is treated the same as missing auditor.
func TestAuditorGate_NoRolesReturns403(t *testing.T) {
	inner := &fixtureHandler{}
	gate := middleware.AuditorGate(fixtureResolver, inner)

	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/agents", nil)
	rr := httptest.NewRecorder()
	gate.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403", rr.Code)
	}
	if inner.called {
		t.Error("inner handler should not be called when gate rejects")
	}
}

// TestAuditorGate_GatesAgentPromptsRoute pins the CHO-2364 /bff/oplus/prompts
// route under the prefix gate: role-less requests are rejected 403 and an
// auditor is admitted. The gate is prefix middleware, so the new route
// inherits it with no per-route wiring; this test keeps that inheritance
// contractual.
func TestAuditorGate_GatesAgentPromptsRoute(t *testing.T) {
	t.Run("no roles rejected", func(t *testing.T) {
		inner := &fixtureHandler{}
		gate := middleware.AuditorGate(fixtureResolver, inner)
		req := httptest.NewRequest(http.MethodGet, "/bff/oplus/prompts", nil)
		rr := httptest.NewRecorder()
		gate.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("status = %d; want 403", rr.Code)
		}
		if inner.called {
			t.Error("inner handler should not be called when gate rejects")
		}
	})
	t.Run("auditor admitted", func(t *testing.T) {
		inner := &fixtureHandler{}
		gate := middleware.AuditorGate(fixtureResolver, inner)
		req := httptest.NewRequest(http.MethodGet, "/bff/oplus/prompts", nil)
		ctx := context.WithValue(req.Context(), rolesCtxKey{}, []string{"auditor"})
		req = req.WithContext(ctx)
		rr := httptest.NewRecorder()
		gate.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("status = %d; want 200", rr.Code)
		}
		if !inner.called {
			t.Error("inner handler not called for auditor")
		}
	})
}

// TestAuditorGate_PassThroughNonOPlusPaths verifies that paths outside
// /bff/oplus/* skip the gate entirely.
func TestAuditorGate_PassThroughNonOPlusPaths(t *testing.T) {
	cases := []string{
		"/healthz",
		"/api/me",
		"/bff/aplus/home",
		"/bff/cplus/feed",
		"/bff/hplus/tenant",
		"/bff/rplus/courses",
		"/graphql",
		"/bff/oplus", // intentionally NOT matched (no trailing slash)
	}
	for _, path := range cases {
		t.Run(strings.ReplaceAll(path, "/", "_"), func(t *testing.T) {
			inner := &fixtureHandler{}
			gate := middleware.AuditorGate(fixtureResolver, inner)

			req := httptest.NewRequest(http.MethodGet, path, nil)
			rr := httptest.NewRecorder()
			gate.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Errorf("path %s: status = %d; want 200 (gate should pass through)", path, rr.Code)
			}
			if !inner.called {
				t.Errorf("path %s: inner handler was not called", path)
			}
		})
	}
}

// TestAuditorGate_NilResolverFailsClosed verifies the defensive
// nil-resolver path returns 403 on /bff/oplus/* (never silent-allow).
func TestAuditorGate_NilResolverFailsClosed(t *testing.T) {
	inner := &fixtureHandler{}
	gate := middleware.AuditorGate(nil, inner)

	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/dashboard", nil)
	rr := httptest.NewRecorder()
	gate.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403", rr.Code)
	}
	if inner.called {
		t.Error("inner handler should not be called with nil resolver")
	}
}

// TestAuditorGate_NilResolverDoesNotAffectNonOPlus verifies non-/bff/oplus
// paths with nil resolver still pass through.
func TestAuditorGate_NilResolverDoesNotAffectNonOPlus(t *testing.T) {
	inner := &fixtureHandler{}
	gate := middleware.AuditorGate(nil, inner)

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	rr := httptest.NewRecorder()
	gate.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rr.Code)
	}
}

// TestChoraSessionRoleResolver_ReadsClaimsRoles verifies the canonical
// chora-session claims path: Roles []string lands first.
func TestChoraSessionRoleResolver_ReadsClaimsRoles(t *testing.T) {
	type chsKey struct{}
	type meshKey struct{}
	choraExtract := func(r *http.Request) (*chorasession.Claims, bool) {
		v, ok := r.Context().Value(chsKey{}).(*chorasession.Claims)
		return v, ok
	}
	meshExtract := func(r *http.Request) (*servicemesh.MeshClaims, bool) {
		v, ok := r.Context().Value(meshKey{}).(*servicemesh.MeshClaims)
		return v, ok
	}
	resolver := middleware.NewChoraSessionRoleResolver(choraExtract, meshExtract)

	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/dashboard", nil)
	claims := &chorasession.Claims{Roles: []string{"learner", "auditor"}}
	req = req.WithContext(context.WithValue(req.Context(), chsKey{}, claims))

	got := resolver(req)
	if !middleware.HasAuditorRole(got) {
		t.Errorf("resolver returned %v; want roles including auditor", got)
	}
}

// TestChoraSessionRoleResolver_FallsBackToMeshClaims verifies that when
// chora-session claims are absent, the mesh-claims path is used.
func TestChoraSessionRoleResolver_FallsBackToMeshClaims(t *testing.T) {
	type chsKey struct{}
	type meshKey struct{}
	choraExtract := func(r *http.Request) (*chorasession.Claims, bool) {
		v, ok := r.Context().Value(chsKey{}).(*chorasession.Claims)
		return v, ok
	}
	meshExtract := func(r *http.Request) (*servicemesh.MeshClaims, bool) {
		v, ok := r.Context().Value(meshKey{}).(*servicemesh.MeshClaims)
		return v, ok
	}
	resolver := middleware.NewChoraSessionRoleResolver(choraExtract, meshExtract)

	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/dashboard", nil)
	mc := &servicemesh.MeshClaims{Roles: []string{"auditor"}}
	req = req.WithContext(context.WithValue(req.Context(), meshKey{}, mc))

	got := resolver(req)
	if !middleware.HasAuditorRole(got) {
		t.Errorf("resolver returned %v; want roles including auditor", got)
	}
}

// TestChoraSessionRoleResolver_ReadsRoleSummaryString verifies the legacy
// `+`-joined RoleSummary string is parsed when Roles is empty.
func TestChoraSessionRoleResolver_ReadsRoleSummaryString(t *testing.T) {
	type chsKey struct{}
	choraExtract := func(r *http.Request) (*chorasession.Claims, bool) {
		v, ok := r.Context().Value(chsKey{}).(*chorasession.Claims)
		return v, ok
	}
	resolver := middleware.NewChoraSessionRoleResolver(choraExtract, nil)

	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/dashboard", nil)
	claims := &chorasession.Claims{RoleSummary: "learner+auditor"}
	req = req.WithContext(context.WithValue(req.Context(), chsKey{}, claims))

	got := resolver(req)
	if !middleware.HasAuditorRole(got) {
		t.Errorf("resolver returned %v; want roles including auditor (from RoleSummary)", got)
	}
}

// equalStrings is a small helper since slices.Equal lives in 1.21+ and
// this package already targets 1.26+; using a hand-rolled version keeps
// the test free of stdlib churn.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
