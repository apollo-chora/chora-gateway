// social_milestone_drafts_handler_test.go — gateway route contract for the
// owner-facing C+ Companion-milestone routes (CHO-2258).
//
// This is "list 1 of 3" of the gateway-route trap: a /v1/* social route is
// registered on the NewRouterWithSocial mux, NOT the phyllis route repository.
// Miss it and the request falls through to `base` and 404s while the aggregator,
// the mesh policy and the downstream service are all perfectly correct.
//
// Lists 2 and 3 need NO new entry for these routes:
//   - list 2 — DefaultJWTGatedPrefixes already carries "/v1/me/"; pinned below
//     so a future prefix edit cannot silently ungate them.
//   - list 3 — the ns/sharing Istio policy already allowlists all three paths
//     (live-verified 2026-07-17). Unassertable from here, which is exactly why
//     the exact paths are pinned at the aggregator (social_milestone_drafts_test.go).
//
// Route matching + method allow-listing are proven here against the fail-loud
// unconfigured aggregator (SharingURL empty -> 502): reaching 502 proves the mux
// routed the request to the social handler, since an unrouted path 404s instead.
// Real fan-out is proven at the aggregator layer.
package httpadapter_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
)

func milestoneDo(t *testing.T, base, method, path, body string) *http.Response {
	t.Helper()
	var (
		req *http.Request
		err error
	)
	if body == "" {
		req, err = http.NewRequest(method, base+path, nil)
	} else {
		req, err = http.NewRequest(method, base+path, bytes.NewReader([]byte(body)))
	}
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

const testDraftID = "01970000-0000-7000-a000-0000000000d1"

// A 404 on any of these means the social mux never claimed the path and the
// request fell through to the base router — gateway list 1 of 3.
func TestMilestoneRoutes_AreRoutedByTheSocialMux(t *testing.T) {
	t.Parallel()
	srv := newSocialFixture(t)
	defer srv.Close()

	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/me/post-drafts", ""},
		{http.MethodPost, "/v1/me/post-drafts/" + testDraftID + "/publish", ""},
		{http.MethodPost, "/v1/me/post-drafts/" + testDraftID + "/discard", ""},
		{http.MethodGet, "/v1/me/preferences/companion-milestone-share", ""},
		{http.MethodPost, "/v1/me/preferences/companion-milestone-share", `{"policy":"auto"}`},
	} {
		resp := milestoneDo(t, srv.URL, c.method, c.path, c.body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			t.Errorf("%s %s -> 404: the social mux does not route it (gateway list 1 of 3)", c.method, c.path)
			continue
		}
		// The fixture leaves SharingURL empty, so a routed request reaches the
		// aggregator's fail-loud 502 rather than a stubbed success.
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("%s %s = %d; want 502 GATEWAY_NOT_CONFIGURED", c.method, c.path, resp.StatusCode)
		}
	}
}

// Methods outside the contract must be refused at the gateway. PUT/PATCH/DELETE
// in particular never reach the gateway in production — Cloud Armor rule 1005
// denies them at the edge — so accepting them would build a surface that can
// only ever work in tests.
func TestMilestoneRoutes_RefuseMethodsOutsideTheContract(t *testing.T) {
	t.Parallel()
	srv := newSocialFixture(t)
	defer srv.Close()

	for _, c := range []struct{ method, path string }{
		{http.MethodDelete, "/v1/me/post-drafts/" + testDraftID},
		{http.MethodPut, "/v1/me/post-drafts/" + testDraftID},
		{http.MethodPut, "/v1/me/preferences/companion-milestone-share"},
		{http.MethodPatch, "/v1/me/preferences/companion-milestone-share"},
		{http.MethodPost, "/v1/me/post-drafts"},
		{http.MethodGet, "/v1/me/post-drafts/" + testDraftID + "/publish"},
		{http.MethodPost, "/v1/me/post-drafts/" + testDraftID + "/bogus"},
	} {
		resp := milestoneDo(t, srv.URL, c.method, c.path, "")
		_ = resp.Body.Close()
		// 502 would mean it was forwarded to the aggregator.
		if resp.StatusCode == http.StatusBadGateway {
			t.Errorf("%s %s was forwarded downstream; want refused at the gateway", c.method, c.path)
		}
		if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d; want 405/404", c.method, c.path, resp.StatusCode)
		}
	}
}

// List 2 of 3 — the JWT gate. These routes inherit session validation from the
// existing "/v1/me/" prefix and need no new entry; this pins that inheritance.
func TestMilestoneRoutes_AreJWTGatedByTheExistingMePrefix(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/v1/me/post-drafts", "/v1/me/preferences/companion-milestone-share"} {
		var covered bool
		for _, p := range httpadapter.DefaultJWTGatedPrefixes {
			if strings.HasPrefix(path, p) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("%s is covered by no DefaultJWTGatedPrefixes entry (gateway list 2 of 3): %v",
				path, httpadapter.DefaultJWTGatedPrefixes)
		}
	}
}
