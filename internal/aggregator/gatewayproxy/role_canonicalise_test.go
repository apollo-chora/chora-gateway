// role_canonicalise_test.go — specs for canonicaliseAdminRole, the
// X-Chora-Role stamping boundary that maps lowercase chora_tenancy
// DB-enum roles to the uppercase tokens chora-payments ParseAdminRole
// matches case-sensitively. Regression guard for the H+ tx-history
// 403 ("admin" JWT → ErrUnknownRole) fixed 2026-05-29.
package gatewayproxy

import "testing"

func TestCanonicaliseAdminRole(t *testing.T) {
	cases := map[string]string{
		// lowercase DB-enum → canonical uppercase
		"admin":   "TENANT_ADMIN",
		"owner":   "OWNER",
		"auditor": "AUDITOR",
		// already-canonical pass through unchanged
		"TENANT_ADMIN":      "TENANT_ADMIN",
		"OWNER":             "OWNER",
		"AUDITOR":           "AUDITOR",
		"PLATFORM_OPERATOR": "PLATFORM_OPERATOR",
		// platform operator variants
		"platform_operator": "PLATFORM_OPERATOR",
		"platform-operator": "PLATFORM_OPERATOR",
		// unknown roles pass through verbatim (payments ignores them)
		"instructor": "instructor",
		"learner":    "learner",
	}
	for in, want := range cases {
		if got := canonicaliseAdminRole(in); got != want {
			t.Errorf("canonicaliseAdminRole(%q) = %q; want %q", in, got, want)
		}
	}
}
