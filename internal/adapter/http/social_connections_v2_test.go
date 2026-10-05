// social_connections_v2_test.go — route contract tests for the B-lite.2
// /v1/connections/* relationship subtree (ADR-230, CHO-2121).
//
// Verifies path matching + method allow-listing on NewRouterWithSocial and
// the fail-loud unconfigured contract: with SharingURL empty every
// relationship route returns the aggregator's 502 GATEWAY_NOT_CONFIGURED —
// a relationship write never fakes success and the pending lists never fake
// an empty state (contrast the read-led feed stubs). Real fan-out is proven
// at the aggregator layer (social_relationship_test.go) and live in the C+
// walk.
package httpadapter_test

import (
	"bytes"
	"net/http"
	"testing"
)

func relationshipDo(t *testing.T, base, method, path, body string) *http.Response {
	t.Helper()
	var req *http.Request
	var err error
	if body == "" {
		req, err = http.NewRequest(method, base+path, nil)
	} else {
		req, err = http.NewRequest(method, base+path, bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
	}
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func TestConnectionsSubtree_AllRoutesReach502WhenUnconfigured(t *testing.T) {
	t.Parallel()
	srv := newSocialFixture(t)
	defer srv.Close()

	cases := []struct{ method, path, body string }{
		{http.MethodPost, "/v1/connections/follows", `{"gcid":"g-2"}`},
		{http.MethodDelete, "/v1/connections/follows/g-2", ""},
		{http.MethodPost, "/v1/connections/blocks", `{"gcid":"g-2"}`},
		{http.MethodDelete, "/v1/connections/blocks/g-2", ""},
		{http.MethodPost, "/v1/connections/friend-requests", `{"gcid":"g-2"}`},
		{http.MethodGet, "/v1/connections/friend-requests", ""},
		{http.MethodGet, "/v1/connections/suggestions", ""}, // B-lite.3
		{http.MethodPost, "/v1/connections/friend-requests/g-2/accept", ""},
		{http.MethodDelete, "/v1/connections/friend-requests/incoming/g-2", ""},
		{http.MethodDelete, "/v1/connections/friend-requests/outgoing/g-2", ""},
		{http.MethodDelete, "/v1/connections/friends/g-2", ""},
	}
	for _, c := range cases {
		resp := relationshipDo(t, srv.URL, c.method, c.path, c.body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("%s %s: status = %d; want 502 GATEWAY_NOT_CONFIGURED (route must exist and fail loud)",
				c.method, c.path, resp.StatusCode)
		}
	}
}

func TestConnectionsSubtree_UnknownPath404(t *testing.T) {
	t.Parallel()
	srv := newSocialFixture(t)
	defer srv.Close()

	resp := relationshipDo(t, srv.URL, http.MethodPost, "/v1/connections/nonsense", `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown subtree path: status = %d; want 404", resp.StatusCode)
	}
}

func TestConnectionsSubtree_WrongMethod405(t *testing.T) {
	t.Parallel()
	srv := newSocialFixture(t)
	defer srv.Close()

	// follows collection is POST-only; friends/{gcid} is DELETE-only.
	resp := relationshipDo(t, srv.URL, http.MethodGet, "/v1/connections/follows", "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/connections/follows: status = %d; want 405", resp.StatusCode)
	}
	resp = relationshipDo(t, srv.URL, http.MethodPost, "/v1/connections/friends/g-2", "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/connections/friends/g-2: status = %d; want 405", resp.StatusCode)
	}
	// suggestions is GET-only (B-lite.3).
	resp = relationshipDo(t, srv.URL, http.MethodPost, "/v1/connections/suggestions", "{}")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/connections/suggestions: status = %d; want 405", resp.StatusCode)
	}
}

// TestConnectionsList_StillServesBareRoute guards the exact /v1/connections
// GET list against regression from the new subtree handler (shadowing bug:
// a "/v1/connections/" mux entry must not eat the bare path).
func TestConnectionsList_StillServesBareRoute(t *testing.T) {
	t.Parallel()
	srv := newSocialFixture(t)
	defer srv.Close()

	resp := relationshipDo(t, srv.URL, http.MethodGet, "/v1/connections?type=following", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/connections: status = %d; want 200 (unconfigured empty stub)", resp.StatusCode)
	}
}
