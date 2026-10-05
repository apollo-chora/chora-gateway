// Package social_test exercises the C+ Phyllis MVP social aggregator
// (CHO-1457).
//
// Per AP-01 the OpenAPI contract `chora-contracts/openapi/bff-gateway.yaml`
// declares 7 new social paths; this aggregator owns the 200/201/501
// stub responses for each. Full implementation lands when chora-sharing
// learner-facing API ships (M12+); for the Phyllis MVP we return
// deterministic placeholders so the C+ Angular surface compiles.
//
// Out-of-scope for stubs (still parked per delta plan):
// Duels, Leaderboards, Three-Currency Economy, Reward Vault,
// Territory Conquest, Companion Missions, Refer-a-Friend.
package social_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/social"
)

func newTestAggregator(_ *testing.T) *social.Aggregator {
	return social.New(social.Config{})
}

func TestGetFeed_ReturnsEmptyDataInStubMode(t *testing.T) {
	t.Parallel()
	agg := newTestAggregator(t)
	resp, err := agg.GetFeed(context.Background(), social.GetFeedParams{
		Auth:     phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
		TenantID: "tenant-a",
		Limit:    20,
	})
	if err != nil {
		t.Fatalf("GetFeed: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	var body map[string]any
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("body unmarshal: %v", err)
	}
	data, ok := body["data"].([]any)
	if !ok {
		t.Fatalf("body.data not an array: %v", body)
	}
	if len(data) != 0 {
		t.Errorf("stub feed should be empty; got len=%d", len(data))
	}
}

func TestGetMySocial_ReturnsStubProfileKeyedOffGcid(t *testing.T) {
	t.Parallel()
	agg := newTestAggregator(t)
	resp, err := agg.GetMySocial(context.Background(), phyllis.AuthCtx{
		GCID: "gcid-phyllis", TenantID: "tenant-a",
	})
	if err != nil {
		t.Fatalf("GetMySocial: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	var body map[string]any
	_ = json.Unmarshal(resp.Body, &body)
	if body["gcid"] != "gcid-phyllis" {
		t.Errorf("gcid not echoed; got %v", body["gcid"])
	}
}

// TestShareAtomToFeed_FansOutToSharingService — ADR-196 P0 reconciled
// the share fan-out to the greenfield chora-sharing contract:
// POST /v1/atoms/{atom_id}/share (NOT the dead /api/posts). The BFF
// forwards the raw shareAtomRequest body {atom_revision_id, caption,
// license_terms, royalty_rate} (opaque passthrough — it does not
// reshape the body) and MUST forward the Idempotency-Key header
// (chora-sharing 400s without it per §7.1 step 5). author_gcid +
// tenant_id come from the identity headers, not the body.
func TestShareAtomToFeed_FansOutToSharingService(t *testing.T) {
	t.Parallel()
	var capturedPath, capturedBody, capturedAuth, capturedIdem string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		capturedAuth = r.Header.Get("X-Tenant-Id") + "|" + r.Header.Get("gcid")
		capturedIdem = r.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"share_entry_id":"se-1","author_display_name":"Phyllis","created_at":"2026-06-29T00:00:00.000000Z"}`))
	}))
	defer srv.Close()

	agg := social.New(social.Config{SharingURL: srv.URL})
	resp, err := agg.ShareAtomToFeed(context.Background(), social.ShareAtomParams{
		Auth:   phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a", IdempotencyKey: "idem-xyz"},
		AtomID: "01970000-0000-7000-c000-000000000001",
		Body:   []byte(`{"atom_revision_id":"rev-1","caption":"Just earned my CSPO","license_terms":"free"}`),
	})
	if err != nil {
		t.Fatalf("ShareAtomToFeed: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", resp.Status)
	}
	// ADR-196 D4: route is /v1/atoms/{atom_id}/share, NOT /api/posts.
	if want := "/v1/atoms/01970000-0000-7000-c000-000000000001/share"; capturedPath != want {
		t.Errorf("downstream path = %q; want %q", capturedPath, want)
	}
	// Opaque passthrough: the share DTO fields reach the downstream intact.
	if !strings.Contains(capturedBody, `"caption":"Just earned my CSPO"`) {
		t.Errorf("downstream body missing caption: %s", capturedBody)
	}
	if !strings.Contains(capturedBody, `"license_terms":"free"`) {
		t.Errorf("downstream body missing license_terms: %s", capturedBody)
	}
	if capturedAuth != "tenant-a|gcid-1" {
		t.Errorf("downstream auth headers = %q; want tenant-a|gcid-1", capturedAuth)
	}
	// ADR-196 D4: Idempotency-Key MUST be forwarded (chora-sharing 400s without it).
	if capturedIdem != "idem-xyz" {
		t.Errorf("downstream Idempotency-Key = %q; want idem-xyz", capturedIdem)
	}
}

func TestShareAtomToFeed_Returns502WhenSharingURLEmpty(t *testing.T) {
	t.Parallel()
	agg := social.New(social.Config{}) // no SharingURL
	resp, err := agg.ShareAtomToFeed(context.Background(), social.ShareAtomParams{
		Auth:   phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
		AtomID: "01970000-0000-7000-c000-000000000001",
	})
	if err != nil {
		t.Fatalf("ShareAtomToFeed: %v", err)
	}
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", resp.Status)
	}
}

func TestCreateReaction_Returns501Stub(t *testing.T) {
	t.Parallel()
	agg := newTestAggregator(t)
	resp, err := agg.CreateReaction(context.Background(), social.CreateReactionParams{
		Auth:   phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
		PostID: "01970000-0000-7000-c000-000000000002",
		Type:   "like",
	})
	if err != nil {
		t.Fatalf("CreateReaction: %v", err)
	}
	if resp.Status != 501 {
		t.Errorf("status = %d; want 501 (stub)", resp.Status)
	}
}

func TestDeleteReaction_Returns204Idempotent(t *testing.T) {
	t.Parallel()
	agg := newTestAggregator(t)
	resp, err := agg.DeleteReaction(context.Background(), social.DeleteReactionParams{
		Auth:       phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
		PostID:     "01970000-0000-7000-c000-000000000002",
		ReactionID: "01970000-0000-7000-c000-000000000003",
	})
	if err != nil {
		t.Fatalf("DeleteReaction: %v", err)
	}
	if resp.Status != 204 {
		t.Errorf("status = %d; want 204 (idempotent stub)", resp.Status)
	}
}

func TestGetComments_ReturnsEmptyDataInStubMode(t *testing.T) {
	t.Parallel()
	agg := newTestAggregator(t)
	resp, err := agg.GetComments(context.Background(), social.GetCommentsParams{
		Auth:   phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
		PostID: "01970000-0000-7000-c000-000000000004",
	})
	if err != nil {
		t.Fatalf("GetComments: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	var body map[string]any
	_ = json.Unmarshal(resp.Body, &body)
	data, ok := body["data"].([]any)
	if !ok || len(data) != 0 {
		t.Errorf("stub comments should be empty; got %v", body)
	}
}

func TestAddComment_Returns501Stub(t *testing.T) {
	t.Parallel()
	agg := newTestAggregator(t)
	resp, err := agg.AddComment(context.Background(), social.AddCommentParams{
		Auth:   phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
		PostID: "01970000-0000-7000-c000-000000000004",
		Body:   "This is a comment",
	})
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if resp.Status != 501 {
		t.Errorf("status = %d; want 501 (stub)", resp.Status)
	}
}

// TestReactionsAndComments_FanOutToV1PrefixedPaths — ADR-196 D4: the
// reaction/comment fan-out must hit the greenfield chora-sharing
// /v1/posts/{id}/... routes, NOT the prefix-less /posts/{id}/... paths
// that 404 against the new service. Covers CreateReaction (POST),
// DeleteReaction (DELETE), GetComments (GET), AddComment (POST).
func TestReactionsAndComments_FanOutToV1PrefixedPaths(t *testing.T) {
	t.Parallel()
	const postID = "01970000-0000-7000-c000-000000000004"
	cases := []struct {
		name     string
		method   string
		wantPath string
		call     func(*social.Aggregator) (social.Response, error)
	}{
		{
			name:     "CreateReaction POST /v1/posts/{id}/reactions",
			method:   http.MethodPost,
			wantPath: "/v1/posts/" + postID + "/reactions",
			call: func(a *social.Aggregator) (social.Response, error) {
				return a.CreateReaction(context.Background(), social.CreateReactionParams{
					Auth:   phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
					PostID: postID, Type: "like",
				})
			},
		},
		{
			name:     "DeleteReaction DELETE /v1/posts/{id}/reactions/{rid}",
			method:   http.MethodDelete,
			wantPath: "/v1/posts/" + postID + "/reactions/01970000-0000-7000-c000-000000000005",
			call: func(a *social.Aggregator) (social.Response, error) {
				return a.DeleteReaction(context.Background(), social.DeleteReactionParams{
					Auth:       phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
					PostID:     postID,
					ReactionID: "01970000-0000-7000-c000-000000000005",
				})
			},
		},
		{
			name:     "GetComments GET /v1/posts/{id}/comments",
			method:   http.MethodGet,
			wantPath: "/v1/posts/" + postID + "/comments",
			call: func(a *social.Aggregator) (social.Response, error) {
				return a.GetComments(context.Background(), social.GetCommentsParams{
					Auth:   phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
					PostID: postID,
				})
			},
		},
		{
			name:     "AddComment POST /v1/posts/{id}/comments",
			method:   http.MethodPost,
			wantPath: "/v1/posts/" + postID + "/comments",
			call: func(a *social.Aggregator) (social.Response, error) {
				return a.AddComment(context.Background(), social.AddCommentParams{
					Auth:   phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
					PostID: postID, Body: "great atom",
				})
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var capturedMethod, capturedPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capturedMethod = r.Method
				capturedPath = r.URL.Path
				if tc.method == http.MethodPost {
					w.WriteHeader(http.StatusCreated)
				} else if tc.method == http.MethodDelete {
					w.WriteHeader(http.StatusNoContent)
				} else {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"data":[],"next_cursor":""}`))
				}
			}))
			defer srv.Close()

			agg := social.New(social.Config{SharingURL: srv.URL})
			resp, err := tc.call(agg)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if resp.Status >= 500 {
				t.Fatalf("%s: upstream returned %d", tc.name, resp.Status)
			}
			if capturedMethod != tc.method {
				t.Errorf("%s: method = %q; want %q", tc.name, capturedMethod, tc.method)
			}
			if capturedPath != tc.wantPath {
				t.Errorf("%s: downstream path = %q; want %q", tc.name, capturedPath, tc.wantPath)
			}
		})
	}
}

func TestGetPublicCourses_ReturnsEmptyDataWhenDeliveryURLUnset(t *testing.T) {
	t.Parallel()
	agg := newTestAggregator(t) // Config{} = no DeliveryURL
	resp, err := agg.GetPublicCourses(context.Background(), social.GetPublicCoursesParams{
		Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
	})
	if err != nil {
		t.Fatalf("GetPublicCourses: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	var body map[string]any
	_ = json.Unmarshal(resp.Body, &body)
	data, ok := body["data"].([]any)
	if !ok {
		t.Fatalf("body.data not an array: %v", body)
	}
	if len(data) != 0 {
		t.Errorf("with no DeliveryURL, public courses must be empty; got %d", len(data))
	}
}
