// Package social_test — Phase 6 fan-out tests.
//
// Validates that when SharingURL / DeliveryURL are configured, each social
// aggregator method fans out to the correct downstream service path with
// auth headers + mesh metadata stamped. When the URLs are empty the
// methods retain the pre-existing stub responses (covered by social_test.go).
package social_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/social"
)

func TestPhase6_GetFeed_FansOutWhenSharingURLConfigured(t *testing.T) {
	t.Parallel()
	var capturedPath, capturedTenant, capturedGCID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedTenant = r.Header.Get(servicemesh.HeaderTenantID)
		capturedGCID = r.Header.Get(servicemesh.HeaderGCID)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"post_id":"p-1"}],"next_cursor":""}`))
	}))
	defer srv.Close()

	agg := social.New(social.Config{SharingURL: srv.URL})
	resp, err := agg.GetFeed(context.Background(), social.GetFeedParams{
		Auth:     phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
		TenantID: "tenant-a",
	})
	if err != nil {
		t.Fatalf("GetFeed: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200; body=%s", resp.Status, string(resp.Body))
	}
	if capturedPath != "/v1/feed/shared-atoms" {
		t.Errorf("path = %q; want /v1/feed/shared-atoms", capturedPath)
	}
	if capturedTenant != "tenant-a" || capturedGCID != "gcid-1" {
		t.Errorf("mesh hdrs: tenant=%q gcid=%q", capturedTenant, capturedGCID)
	}
	var body map[string]any
	_ = json.Unmarshal(resp.Body, &body)
	data, _ := body["data"].([]any)
	if len(data) != 1 {
		t.Errorf("body.data len = %d; want 1", len(data))
	}
}

func TestPhase6_GetMySocial_FansOutWhenSharingURLConfigured(t *testing.T) {
	t.Parallel()
	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"gcid":"gcid-1","atom_count":5}`))
	}))
	defer srv.Close()

	agg := social.New(social.Config{SharingURL: srv.URL})
	resp, err := agg.GetMySocial(context.Background(), phyllis.AuthCtx{
		GCID: "gcid-1", TenantID: "tenant-a",
	})
	if err != nil {
		t.Fatalf("GetMySocial: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if capturedPath != "/v1/connections" {
		t.Errorf("path = %q; want /v1/connections", capturedPath)
	}
}

func TestPhase6_CreateReaction_FansOutWhenSharingURLConfigured(t *testing.T) {
	t.Parallel()
	var capturedPath, capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	agg := social.New(social.Config{SharingURL: srv.URL})
	resp, err := agg.CreateReaction(context.Background(), social.CreateReactionParams{
		Auth:   phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
		PostID: "post-1",
		Type:   "like",
	})
	if err != nil {
		t.Fatalf("CreateReaction: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if capturedPath != "/v1/posts/post-1/reactions" {
		t.Errorf("path = %q; want /v1/posts/post-1/reactions (ADR-196 D4 /v1 prefix)", capturedPath)
	}
	if !strings.Contains(capturedBody, `"kind":"like"`) {
		t.Errorf("body missing kind field: %s", capturedBody)
	}
}

func TestPhase6_DeleteReaction_FansOutWhenSharingURLConfigured(t *testing.T) {
	t.Parallel()
	var capturedPath, capturedMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedMethod = r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	agg := social.New(social.Config{SharingURL: srv.URL})
	resp, err := agg.DeleteReaction(context.Background(), social.DeleteReactionParams{
		Auth:       phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
		PostID:     "post-1",
		ReactionID: "r-1",
	})
	if err != nil {
		t.Fatalf("DeleteReaction: %v", err)
	}
	if resp.Status != http.StatusNoContent {
		t.Errorf("status = %d; want 204", resp.Status)
	}
	if capturedPath != "/v1/posts/post-1/reactions/r-1" {
		t.Errorf("path = %q; want /v1/posts/post-1/reactions/r-1 (ADR-196 D4 /v1 prefix)", capturedPath)
	}
	if capturedMethod != http.MethodDelete {
		t.Errorf("method = %q; want DELETE", capturedMethod)
	}
}

func TestPhase6_GetComments_FansOutWhenSharingURLConfigured(t *testing.T) {
	t.Parallel()
	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"comment_id":"c-1"}],"next_cursor":""}`))
	}))
	defer srv.Close()

	agg := social.New(social.Config{SharingURL: srv.URL})
	resp, err := agg.GetComments(context.Background(), social.GetCommentsParams{
		Auth:   phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
		PostID: "post-1",
	})
	if err != nil {
		t.Fatalf("GetComments: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if capturedPath != "/v1/posts/post-1/comments" {
		t.Errorf("path = %q; want /v1/posts/post-1/comments (ADR-196 D4 /v1 prefix)", capturedPath)
	}
}

func TestPhase6_AddComment_FansOutWhenSharingURLConfigured(t *testing.T) {
	t.Parallel()
	var capturedPath, capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"comment_id":"c-1"}`))
	}))
	defer srv.Close()

	agg := social.New(social.Config{SharingURL: srv.URL})
	resp, err := agg.AddComment(context.Background(), social.AddCommentParams{
		Auth:   phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
		PostID: "post-1",
		Body:   "Nice atom",
	})
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", resp.Status)
	}
	if capturedPath != "/v1/posts/post-1/comments" {
		t.Errorf("path = %q; want /v1/posts/post-1/comments (ADR-196 D4 /v1 prefix)", capturedPath)
	}
	if !strings.Contains(capturedBody, `"body":"Nice atom"`) {
		t.Errorf("body missing body field: %s", capturedBody)
	}
}

func TestPhase6_GetPublicCourses_FansOutWhenDeliveryURLConfigured(t *testing.T) {
	t.Parallel()
	var capturedPath, capturedQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"course_id":"c-1","public":true}]}`))
	}))
	defer srv.Close()

	agg := social.New(social.Config{DeliveryURL: srv.URL})
	resp, err := agg.GetPublicCourses(context.Background(), social.GetPublicCoursesParams{
		Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
	})
	if err != nil {
		t.Fatalf("GetPublicCourses: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if capturedPath != "/courses" {
		t.Errorf("path = %q; want /courses", capturedPath)
	}
	if !strings.Contains(capturedQuery, "public=true") {
		t.Errorf("query = %q; want public=true", capturedQuery)
	}
}

// Ensure stub fallback path is preserved when URL is empty (back-compat
// with social_test.go expectations).
func TestPhase6_CreateReaction_StubFallbackWhenSharingURLEmpty(t *testing.T) {
	t.Parallel()
	agg := social.New(social.Config{}) // no SharingURL
	resp, err := agg.CreateReaction(context.Background(), social.CreateReactionParams{
		Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"}, PostID: "p-1", Type: "like",
	})
	if err != nil {
		t.Fatalf("CreateReaction: %v", err)
	}
	if resp.Status != http.StatusNotImplemented {
		t.Errorf("stub fallback: status = %d; want 501", resp.Status)
	}
}

func TestPhase6_AddComment_StubFallbackWhenSharingURLEmpty(t *testing.T) {
	t.Parallel()
	agg := social.New(social.Config{})
	resp, err := agg.AddComment(context.Background(), social.AddCommentParams{
		Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"}, PostID: "p-1", Body: "x",
	})
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if resp.Status != http.StatusNotImplemented {
		t.Errorf("stub fallback: status = %d; want 501", resp.Status)
	}
}
