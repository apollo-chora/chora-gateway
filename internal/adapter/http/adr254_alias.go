// adr254_alias.go: deploy-window path aliases for the ADR-254 D9 rename
// (Familiar -> Companion).
//
// The SPA keeps calling the pre-rename public paths until its own cut
// (WP-X go), so for one deploy window every old public path is served as an
// ALIAS of its companion-named route. An alias is implemented by re-pathing the
// request onto the canonical prefix BEFORE the owning mux dispatches, so it is
// by construction the same handler and the same upstream path: there is no
// second route table to drift. ADR-254 D9 alias, drop after the SPA cut (WP-X go).
package httpadapter

import (
	"net/http"
	"strings"
)

// rewriteAliasPrefix returns the request re-pathed onto the canonical prefix
// when it arrived on one of the alias prefixes (exact match or subtree); the
// request is returned unchanged otherwise. The clone keeps the context, the
// headers and the body; only URL.Path is rewritten (RawPath is cleared so the
// mux matches the decoded path).
func rewriteAliasPrefix(r *http.Request, aliases map[string]string) *http.Request {
	for alias, canonical := range aliases {
		if r.URL.Path != alias && !strings.HasPrefix(r.URL.Path, alias+"/") {
			continue
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = canonical + strings.TrimPrefix(r.URL.Path, alias)
		r2.URL.RawPath = ""
		return r2
	}
	return r
}

// withAliasPrefixes wraps a mux so requests on an alias prefix are re-pathed
// onto the canonical prefix before dispatch.
func withAliasPrefixes(mux http.Handler, aliases map[string]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, rewriteAliasPrefix(r, aliases))
	})
}
