// social_handler_test.go — RED-phase TDD specs for the C+ social BFF
// handlers (CHO-1457). Verifies path matching, method allow-listing,
// and contract-shape compliance against the OpenAPI definitions in
// `chora-contracts/openapi/bff-gateway.yaml`.
//
// All routes are stubs in this MVP — read paths return 200 with empty
// `data` arrays; writes return 501 except idempotent DELETE which is
// 204.
package httpadapter_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/adapter/inmem"
	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/social"
)

func newSocialFixture(t *testing.T) *httptest.Server {
	t.Helper()
	routes := inmem.NewRouteRepository()
	sessions := inmem.NewSessionRepository()
	up := upstream.NewFakeUpstream()
	phyllisCfg := phyllis.LoadConfigFromEnv()
	phyllisAgg := phyllis.New(phyllisCfg, nil)
	socialAgg := social.New(social.Config{})

	mux := httpadapter.NewRouterWithSocial(routes, sessions, up, phyllisAgg, socialAgg, nil)
	return httptest.NewServer(mux)
}

func TestSocial_GetFeed_Returns200WithEmptyData(t *testing.T) {
	t.Parallel()
	srv := newSocialFixture(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/feed?limit=20")
	if err != nil {
		t.Fatalf("GET /v1/feed: %v", err)
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

func TestSocial_GetMySocial_Returns200(t *testing.T) {
	t.Parallel()
	srv := newSocialFixture(t)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/me/social", nil)
	req.Header.Set("Authorization", "Bearer gcid-phyllis")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/me/social: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
}

// TestSocial_ShareAtomToFeed_Returns502WhenSharingURLEmpty pins the
// M14.iter5.B fail-loud behaviour at the handler layer. The aggregator
// is constructed with no SharingURL, so the request 502s. The unit
// test `TestShareAtomToFeed_FansOutToSharingService` covers the happy
// path with a httptest.Server-backed sharing URL.
func TestSocial_ShareAtomToFeed_Returns502WhenSharingURLEmpty(t *testing.T) {
	t.Parallel()
	srv := newSocialFixture(t)
	defer srv.Close()

	body := bytes.NewReader([]byte(`{"body":"check out this atom","visibility":"network"}`))
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/atoms/01970000-0000-7000-c000-000000000001/share", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST share: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d; want 502 (no SVC_SHARING_URL)", resp.StatusCode)
	}
}

// TestSocial_ShareAtomToFeed_ForwardsIdempotencyKeyAndShareDTO — ADR-196 D4:
// the BFF POST /v1/atoms/{atom_id}/share handler must forward the inbound
// Idempotency-Key header to chora-sharing (which 400s without it per §7.1
// step 5) AND pass the share body through to the correct downstream route
// /v1/atoms/{atom_id}/share. This wires a capturing test server as the
// sharing backend and asserts both reach it intact.
func TestSocial_ShareAtomToFeed_ForwardsIdempotencyKeyAndShareDTO(t *testing.T) {
	t.Parallel()

	var capturedPath, capturedIdem, capturedBody string
	sharing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedIdem = r.Header.Get("Idempotency-Key")
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"share_entry_id":"se-1","author_display_name":"Phyllis","created_at":"2026-06-29T00:00:00.000000Z"}`))
	}))
	defer sharing.Close()

	routes := inmem.NewRouteRepository()
	sessions := inmem.NewSessionRepository()
	up := upstream.NewFakeUpstream()
	phyllisAgg := phyllis.New(phyllis.LoadConfigFromEnv(), nil)
	socialAgg := social.New(social.Config{SharingURL: sharing.URL})
	mux := httpadapter.NewRouterWithSocial(routes, sessions, up, phyllisAgg, socialAgg, nil)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	shareBody := `{"atom_revision_id":"rev-1","caption":"earned my CSPO","license_terms":"free"}`
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/v1/atoms/01970000-0000-7000-c000-000000000001/share",
		bytes.NewReader([]byte(shareBody)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "idem-handler-xyz")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST share: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d; want 201 (sharing returned 201)", resp.StatusCode)
	}
	if want := "/v1/atoms/01970000-0000-7000-c000-000000000001/share"; capturedPath != want {
		t.Errorf("downstream path = %q; want %q", capturedPath, want)
	}
	if capturedIdem != "idem-handler-xyz" {
		t.Errorf("downstream Idempotency-Key = %q; want idem-handler-xyz (chora-sharing 400s without it)", capturedIdem)
	}
	if !strings.Contains(capturedBody, `"caption":"earned my CSPO"`) {
		t.Errorf("downstream body missing caption passthrough: %s", capturedBody)
	}
	if !strings.Contains(capturedBody, `"license_terms":"free"`) {
		t.Errorf("downstream body missing license_terms passthrough: %s", capturedBody)
	}
}

func TestSocial_CreateReaction_Returns501Stub(t *testing.T) {
	t.Parallel()
	srv := newSocialFixture(t)
	defer srv.Close()

	body := bytes.NewReader([]byte(`{"type":"like"}`))
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/posts/01970000-0000-7000-c000-000000000002/reactions", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST reaction: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501", resp.StatusCode)
	}
}

func TestSocial_DeleteReaction_Returns204Idempotent(t *testing.T) {
	t.Parallel()
	srv := newSocialFixture(t)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete,
		srv.URL+"/v1/posts/01970000-0000-7000-c000-000000000002/reactions/01970000-0000-7000-c000-000000000003", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE reaction: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d; want 204", resp.StatusCode)
	}
}

func TestSocial_GetComments_Returns200WithEmptyData(t *testing.T) {
	t.Parallel()
	srv := newSocialFixture(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/posts/01970000-0000-7000-c000-000000000004/comments")
	if err != nil {
		t.Fatalf("GET comments: %v", err)
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

func TestSocial_AddComment_Returns501Stub(t *testing.T) {
	t.Parallel()
	srv := newSocialFixture(t)
	defer srv.Close()

	body := bytes.NewReader([]byte(`{"body":"great atom"}`))
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/v1/posts/01970000-0000-7000-c000-000000000004/comments", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST comment: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501", resp.StatusCode)
	}
}

func TestSocial_GetPublicCourses_Returns200WithEmptyDataInStubMode(t *testing.T) {
	t.Parallel()
	srv := newSocialFixture(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/discovery/courses/public")
	if err != nil {
		t.Fatalf("GET public courses: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if data, ok := body["data"].([]any); !ok || len(data) != 0 {
		t.Errorf("expected empty data array; got %v", body)
	}
}

func TestSocial_RejectsWrongMethod(t *testing.T) {
	t.Parallel()
	srv := newSocialFixture(t)
	defer srv.Close()

	// PUT on /v1/feed is unsupported.
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/v1/feed", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /v1/feed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405", resp.StatusCode)
	}
}
