// mesh_roles_guard_test.go — EVERY servicemesh.MarshalToHeaders site in
// chora-gateway must carry Roles. This is a build-time guard, not a convention.
//
// Why a test and not a comment: `MeshClaims.Roles` is what becomes the
// `x-mesh-user-roles` header (servicemesh.HeaderUserRoles). Every downstream role
// gate in Chora reads that header and FAILS CLOSED — deliberately, because
// `rolesAllowed(X-Role)` was deleted in CHO-2072 for fail-OPENing. So a
// MarshalToHeaders literal that omits `Roles:` does not degrade gracefully: it
// sends NO roles header, and the downstream denies **100%** of those calls.
//
// The failure mode is invisible until the downstream grows its first role gate,
// at which point a working route dies with no code change on either side. That is
// exactly the latent bug CHO-2148 flagged on ObservabilityClient — and the audit
// it prompted found the same omission at SEVEN more sites. A prose warning had
// already been written for this class once (phyllis carries one about CHO-1708)
// and it did not stop the recurrence. Hence: a test.
//
// If this fails: add `Roles: auth.Roles` (never a client-supplied header — Roles
// must come from the VALIDATED session/mesh claims) to the named literal.
package upstream_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// rolesFieldRE matches a `Roles:` field inside a composite literal body.
// `RoleSummary:` deliberately does NOT match.
var rolesFieldRE = regexp.MustCompile(`(^|[\s,{])Roles:\s`)

const meshMarshalMarker = "servicemesh.MarshalToHeaders(servicemesh.MeshClaims{"

func TestMeshClaims_EveryMarshalSiteCarriesRoles(t *testing.T) {
	root := serviceRoot(t)

	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "vendor" || name == "node_modules" || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, lit := range meshClaimLiterals(string(src)) {
			if !rolesFieldRE.MatchString(lit.body) {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, rel+":"+itoa(lit.line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	if len(offenders) > 0 {
		t.Fatalf("servicemesh.MeshClaims literal(s) omit Roles — these calls send NO "+
			"x-mesh-user-roles header, so every fail-closed downstream role gate denies "+
			"100%% of them (silently, until that gate exists):\n  %s\n\n"+
			"Fix: add `Roles: auth.Roles` (from the VALIDATED session/mesh claims, never a "+
			"client-supplied header).", strings.Join(offenders, "\n  "))
	}
}

type meshLit struct {
	body string
	line int
}

// meshClaimLiterals returns the body of every MeshClaims composite literal passed
// to MarshalToHeaders, with its 1-indexed line.
func meshClaimLiterals(src string) []meshLit {
	var out []meshLit
	for idx := 0; ; {
		rel := strings.Index(src[idx:], meshMarshalMarker)
		if rel < 0 {
			return out
		}
		start := idx + rel
		open := start + len(meshMarshalMarker) // just past the `{`
		depth := 1
		i := open
		for ; i < len(src) && depth > 0; i++ {
			switch src[i] {
			case '{':
				depth++
			case '}':
				depth--
			}
		}
		out = append(out, meshLit{
			body: src[open:max(open, i-1)],
			line: 1 + strings.Count(src[:start], "\n"),
		})
		idx = i
	}
}

// serviceRoot walks up from this package to the chora-gateway service root.
func serviceRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd() // .../services/chora-gateway/internal/adapter/upstream
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("service root %q has no go.mod: %v", root, err)
	}
	return root
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
