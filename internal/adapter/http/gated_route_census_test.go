// gated_route_census_test.go: every BFF route is JWT-gated unless it is
// explicitly and justifiably public.
//
// This turns a one-off audit into a standing check, and it exists partly
// because the hand-rolled version of that audit was wrong twice: its route
// regex required a simple handler identifier and so under-counted the
// registrations, and its prefix parse truncated the DefaultJWTGatedPrefixes
// literal and so under-counted the gate. Reading the real variable and every
// registration is the point.
//
// Two different consequences follow from a route being ungated, and only the
// first is about tenancy. A handler that reaches authCtxFromRequest sees no
// validated claims, so the caller's own X-Tenant-Id becomes the tenant and is
// stamped on the upstream call. Every other ungated handler is simply
// unauthenticated, which for some of them is correct.
//
// There are TWO gates, not one, and an earlier version of this file knew only
// about the first. DefaultJWTGatedPrefixes covers the JWT middleware, and the
// legacy route repository carries an AuthMode per path (Public, Authenticated,
// Admin, Auditor) that the surface middleware enforces independently. Three of
// the four /bff/* routes this census first flagged are gated by the second
// mechanism and were never open. A census that knows one convention cannot see
// the routes that adopted the other, so this one reads both.
//
// A third list exists and gates nothing: GatewayProxyPathPrefixes says which
// paths the proxy bridge serves. A route in it but not in either gate is still
// reachable unauthenticated.
//
// The source is parsed rather than a hand-kept list, so this cannot drift from
// the registrations it is checking.
//
// It is parsed with go/ast rather than a regex, and over EVERY non-test file in
// the package rather than two named ones. Both were blind spots in the first
// version and both flattered it. The regex saw only mux.HandleFunc with a
// string literal, so it missed every mux.Handle and every route whose path is a
// package constant, which is how closure, payments, webauthn and kg-explore
// registered theirs. The two-file list saw 73 of 247 registrations, and it
// could not see the wizard routes added in wizard_active_tenant.go by the same
// author who wrote the census. A census keyed on the convention cannot see the
// files that did not adopt it, so this one reads the package.
//
// Go 1.22 patterns may carry a METHOD prefix ("POST /api/v1/..."). That prefix
// is stripped before matching: leaving it on made one genuinely gated route
// report as ungated, which is the same class of parser error in the opposite
// direction.
package httpadapter_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
)

// publicByDesign is the allowlist. A path belongs here only with a reason that
// says why an unauthenticated caller is correct, not merely tolerable.
var publicByDesign = map[string]string{
	"/healthz":  "liveness probe; no caller identity exists or is wanted",
	"/healthz/": "same, trailing-slash form",
	"/health":   "same, legacy spelling",
	"/readyz":   "readiness probe, same reason",
	"/version":  "build identifier; carries no caller data",
	"/":         "service index",
	// You cannot hold a session before you are given one.
	"/api/auth/session": "mints the session; gating it would make sign-in impossible",
	// The same rule, for the three sign-in routes the two-file census could
	// not see because their paths are package constants. Each authenticates
	// the caller by something OTHER than a Chora session, which is what makes
	// public correct rather than merely tolerable: the mint verifies a
	// identity token, and the two passkey routes complete a WebAuthn
	// challenge. The asymmetry is deliberate and holds: the REGISTER pair is
	// gated by /api/v1/auth/webauthn/register/ in DefaultJWTGatedPrefixes,
	// because adding a passkey to an account does require already being that
	// account, and the handler enforces body-gcid == session-gcid.
	"/api/v1/auth/session/mint":          "exchanges a username/password pair for a Chora session; there is no session to gate on yet",
	"/api/v1/auth/webauthn/login/begin":  "passkey sign-in challenge; pre-session by definition, and register/ is gated",
	"/api/v1/auth/webauthn/login/finish": "passkey sign-in assertion; pre-session by definition, and register/ is gated",
	// The A+ course catalogue is genuinely pre-login: its route carries no
	// guard and sits beside the sign-in screen. This is the one allowlisted
	// route that reaches authCtxFromRequest, and it is safe only because an
	// anonymous caller carries an EMPTY tenant: GetCatalog pins such a caller
	// to visibility=public, and its own comment explains that tenant_or_public
	// with an empty tenant scope would return every tenant's non-public rows.
	"/api/catalog": "public course catalogue, pre-login by design; anonymous callers carry no tenant, which pins visibility=public",
	// The route repository states the intent: "defaults to AuthMode=Public so
	// guest preview works". It fans out to chora-consumption with whatever
	// tenant and gcid the SESSION carries (withUpstreamAuthCtx reads the
	// context, never the inbound header), so an anonymous caller gets an empty
	// pair and the upstream parts come back as errors rather than as another
	// tenant's data. It has no consumer in chora-web today; the four other
	// /bff/* routes are gated by AuthMode and only /bff/oplus/governance is
	// actually called (five references).
	"/bff/aplus/home": "guest preview, AuthMode=Public on purpose; reads its tenant from the session context, never from the inbound header",
}

// authModeRe reads the SECOND gate: the legacy route repository's per-path
// AuthMode. Anything but Public is gated by the surface middleware.
var authModeRe = regexp.MustCompile(`PathPattern:\s*"([^"]+)"[^}]*?AuthMode:\s*route\.AuthMode(\w+)`)

func routeAuthModes(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(adapterSourceDir(t), "..", "inmem", "route_repository.go"))
	if err != nil {
		t.Fatalf("read route_repository.go: %v", err)
	}
	modes := map[string]string{}
	for _, m := range authModeRe.FindAllStringSubmatch(string(b), -1) {
		modes[m[1]] = m[2]
	}
	if len(modes) == 0 {
		t.Fatal("parsed no AuthModes; the census would silently fall back to one gate")
	}
	return modes
}

func adapterSourceDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	return filepath.Dir(thisFile)
}

// methodPrefixRe strips a Go 1.22 pattern's leading verb. "POST /api/x" and
// "/api/x" are the same route to both gates, which match on path.
var methodPrefixRe = regexp.MustCompile(`^[A-Z]+ `)

// allRegisteredPaths returns every path registered on a mux anywhere in the
// package, keyed by the file that registers it.
//
// It resolves three shapes the regex version could not: mux.Handle as well as
// mux.HandleFunc, a path given as a package-level string constant as well as a
// literal, and a Go 1.22 method-prefixed pattern.
//
// A path built by concatenation or computed at runtime is still invisible, and
// that is a known limit rather than a solved problem: nothing in the package
// does it today, and the total below is asserted so a drop is loud.
func allRegisteredPaths(t *testing.T) map[string][]string {
	t.Helper()
	dir := adapterSourceDir(t)
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}

	// Package-level string constants, so a path named by identifier resolves.
	consts := map[string]string{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						if bl, ok := vs.Values[i].(*ast.BasicLit); ok && bl.Kind == token.STRING {
							if v, err := strconv.Unquote(bl.Value); err == nil {
								consts[name.Name] = v
							}
						}
					}
				}
			}
		}
	}

	out := map[string][]string{}
	for _, pkg := range pkgs {
		for name, f := range pkg.Files {
			base := filepath.Base(name)
			ast.Inspect(f, func(n ast.Node) bool {
				ce, ok := n.(*ast.CallExpr)
				if !ok || len(ce.Args) == 0 {
					return true
				}
				sel, ok := ce.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
					return true
				}
				var path string
				switch a := ce.Args[0].(type) {
				case *ast.BasicLit:
					if a.Kind == token.STRING {
						path, _ = strconv.Unquote(a.Value)
					}
				case *ast.Ident:
					path = consts[a.Name]
				}
				if path != "" {
					out[base] = append(out[base], methodPrefixRe.ReplaceAllString(path, ""))
				}
				return true
			})
		}
	}
	if len(out) == 0 {
		t.Fatal("the package registered no routes; the census is measuring nothing")
	}
	return out
}

// censusFloor guards against the enumeration silently shrinking. It is a floor,
// not an equality, so adding routes does not break it, and it is the number
// that would have caught the two-file version: that one saw 73.
const censusFloor = 240

func TestCensusSeesTheWholePackage(t *testing.T) {
	t.Parallel()
	byFile := allRegisteredPaths(t)
	total := 0
	for _, paths := range byFile {
		total += len(paths)
	}
	if total < censusFloor {
		t.Fatalf("census resolved %d registrations across %d files, want at least %d. "+
			"Either routes were deleted, or a new registration shape is invisible to the parser "+
			"(a computed path, or a mux method this walker does not know).", total, len(byFile), censusFloor)
	}
	// The files that carry the wizard and ownership routes must be among them:
	// they are the ones the two-file version could not see.
	for _, want := range []string{"wizard_active_tenant.go", "closure_handler.go", "payments_handler.go"} {
		if len(byFile[want]) == 0 {
			t.Errorf("%s registers no route the census can see", want)
		}
	}
}

// authModeFor matches a registered path against the route repository's
// patterns, which may end in a "*" glob (/api/proxy/* covers /api/proxy/).
func authModeFor(modes map[string]string, path string) string {
	if m, ok := modes[path]; ok {
		return m
	}
	for pattern, m := range modes {
		if strings.HasSuffix(pattern, "*") &&
			strings.HasPrefix(path, strings.TrimSuffix(pattern, "*")) {
			return m
		}
	}
	return ""
}

func TestEveryBFFRouteIsGatedOrExplicitlyPublic(t *testing.T) {
	t.Parallel()
	modes := routeAuthModes(t)
	for file, paths := range allRegisteredPaths(t) {
		t.Run(file, func(t *testing.T) {
			for _, path := range paths {
				if _, public := publicByDesign[path]; public {
					continue
				}
				// Gate one: the JWT middleware.
				covered := false
				for _, p := range httpadapter.DefaultJWTGatedPrefixes {
					if strings.HasPrefix(path, p) {
						covered = true
						break
					}
				}
				// Gate two: the route repository's AuthMode. Anything but
				// Public is enforced by the surface middleware.
				if mode := authModeFor(modes, path); mode != "" && mode != "Public" {
					covered = true
				}
				if !covered {
					t.Errorf("%s registers %q and NEITHER gate covers it: no DefaultJWTGatedPrefixes entry "+
						"matches, and the route repository gives it AuthMode %q. The handler sees no validated "+
						"claims, so if it reaches authCtxFromRequest the caller's own X-Tenant-Id becomes the "+
						"tenant and is stamped on the upstream call. Add the prefix to jwt_auth.go, give the "+
						"route a non-Public AuthMode, or add it to publicByDesign with a reason an "+
						"unauthenticated caller is CORRECT.", file, path, authModeFor(modes, path))
				}
			}
		})
	}
}

// /bff/aplus/home is Public on purpose, so the census passes it, and that
// decision deserves a test of its own rather than a silent skip: the route
// repository's own comment says "defaults to AuthMode=Public so guest preview
// works". If somebody makes it Authenticated, guest preview breaks; if
// somebody makes its siblings Public, three admin and member surfaces open.
func TestBFFSurfaceAuthModesAreDeliberate(t *testing.T) {
	t.Parallel()
	modes := routeAuthModes(t)
	for path, want := range map[string]string{
		"/bff/aplus/home":       "Public",
		"/bff/cplus/feed":       "Authenticated",
		"/bff/hplus/tenant":     "Admin",
		"/bff/oplus/governance": "Admin",
		"/bff/rplus/courses":    "Authenticated",
		"/api/proxy/*":          "Authenticated",
	} {
		if got := modes[path]; got != want {
			t.Errorf("%s AuthMode = %q, want %q", path, got, want)
		}
	}
}

// The allowlist must not outlive the routes it excuses. An entry for a path
// nobody registers any more is a licence sitting around waiting to be
// inherited by a future route that happens to reuse the string.
func TestPublicAllowlistHasNoStaleEntries(t *testing.T) {
	t.Parallel()
	registered := map[string]bool{}
	for _, paths := range allRegisteredPaths(t) {
		for _, p := range paths {
			registered[p] = true
		}
	}
	for path := range publicByDesign {
		if !registered[path] {
			t.Errorf("publicByDesign excuses %q, which no handler registers any more; remove the entry", path)
		}
	}
}
