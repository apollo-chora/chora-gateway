// social_handler_v2_test.go — Atom Sharing Redesign (T016) route contract
// tests for the new RMM L2 sharing routes wired in NewRouterWithSocial.
// Verifies path matching + method allow-listing + stub-fallback shapes when
// the BFF is unconfigured (SharingURL empty). Real fan-out is exercised in
// the user-story phases (US1/US3/US4) against a live chora-sharing.
package httpadapter_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// TestSocialV2_SharedAtomsFeed_Returns200WithEmptyData verifies the US1
// shared-atom feed route renders its empty-state when the BFF is
// unconfigured (the same contract GetFeed satisfies).
func TestSocialV2_SharedAtomsFeed_Returns200WithEmptyData(t *testing.T) {
	srv := newSocialFixture(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/feed/shared-atoms?limit=20")
	if err != nil {
		t.Fatalf("GET /v1/feed/shared-atoms: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if _, ok := body["data"].([]any); !ok {
		t.Errorf("body.data not array: %v", body)
	}
	if body["next_cursor"] != "" {
		t.Errorf("next_cursor = %v; want empty", body["next_cursor"])
	}
}

// TestSocialV2_Leaderboard_Returns200WithEmptyData verifies the US4
// leaderboard route renders its empty-state when unconfigured.
func TestSocialV2_Leaderboard_Returns200WithEmptyData(t *testing.T) {
	srv := newSocialFixture(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/leaderboard?scope=tenant&limit=20")
	if err != nil {
		t.Fatalf("GET /v1/leaderboard: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if _, ok := body["data"].([]any); !ok {
		t.Errorf("body.data not array: %v", body)
	}
}

// TestSocialV2_DuelsGet_Returns501Stub verifies the US3 duel-list route
// returns the 501 stub when unconfigured (duels is a write-led surface —
// read falls back to not-implemented, not an empty 200, matching the
// GetDuel stub contract).
func TestSocialV2_DuelsGet_Returns501Stub(t *testing.T) {
	srv := newSocialFixture(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/duels?tenant_id=t-1")
	if err != nil {
		t.Fatalf("GET /v1/duels: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501 (unconfigured duel stub)", resp.StatusCode)
	}
}

// TestSocialV2_DuelsPost_Returns405 verifies that POST /v1/duels is no longer
// accepted — the challenge/accept flow was removed in the pool-based
// matchmaking rewrite. POST /v1/duels/queue is the new entry point.
func TestSocialV2_DuelsPost_Returns405(t *testing.T) {
	srv := newSocialFixture(t)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/duels", "application/json", bytes.NewReader([]byte(`{"mode":"POOL"}`)))
	if err != nil {
		t.Fatalf("POST /v1/duels: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d; want 405 (POST /v1/duels removed; use /v1/duels/queue)", resp.StatusCode)
	}
}

// TestSocialV2_DuelScoped_Get verifies GET /v1/duels/{duel_id} routes to the
// GetDuel aggregator (returns the 501 stub when unconfigured).
func TestSocialV2_DuelScoped_Get(t *testing.T) {
	srv := newSocialFixture(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/duels/duel-123")
	if err != nil {
		t.Fatalf("GET /v1/duels/duel-123: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501 (unconfigured duel read stub)", resp.StatusCode)
	}
}

// TestSocialV2_SharedAtomsFeed_RejectsNonGet verifies method allow-listing.
func TestSocialV2_SharedAtomsFeed_RejectsNonGet(t *testing.T) {
	srv := newSocialFixture(t)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/feed/shared-atoms", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /v1/feed/shared-atoms: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d; want 405", resp.StatusCode)
	}
}
