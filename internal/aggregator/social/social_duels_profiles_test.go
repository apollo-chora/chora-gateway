// Package social_test — duels / profile / connections / bookmarks fan-out
// coverage for the C+ social aggregator (CHO-1457 atom-sharing redesign,
// ADR-196 D4).
//
// These route groups were wired after the original Phyllis MVP stub set, so
// they live in their own file rather than being retrofitted into social_test.go:
// each method has a stub-mode branch (SharingURL empty) and a fan-out branch
// (SharingURL set). Both branches are exercised here.
package social_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/social"
)

func TestSharingBaseURL_ReturnsConfiguredURL(t *testing.T) {
	t.Parallel()
	agg := social.New(social.Config{SharingURL: "http://sharing.internal"})
	if got := agg.SharingBaseURL(); got != "http://sharing.internal" {
		t.Errorf("SharingBaseURL() = %q; want http://sharing.internal", got)
	}
}

func TestStampDownstreamHeaders_StampsCanonicalMeshHeaders(t *testing.T) {
	t.Parallel()
	agg := social.New(social.Config{})
	req, err := http.NewRequest(http.MethodGet, "http://downstream.internal/v1/x", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	agg.StampDownstreamHeaders(req, phyllis.AuthCtx{
		Bearer:         "tok-1",
		Traceparent:    "00-abc-def",
		TenantID:       "tenant-a",
		GCID:           "gcid-1",
		IdempotencyKey: "idem-1",
		Roles:          []string{"learner", "author"},
	})
	checks := map[string]string{
		"Authorization":               "Bearer tok-1",
		"traceparent":                 "00-abc-def",
		"X-Tenant-Id":                 "tenant-a",
		"gcid":                        "gcid-1",
		"Idempotency-Key":             "idem-1",
		servicemesh.HeaderGCID:        "gcid-1",
		servicemesh.HeaderTenantID:    "tenant-a",
		servicemesh.HeaderUserRoles:   "learner,author",
		servicemesh.HeaderRoleSummary: "",
	}
	for hdr, want := range checks {
		if got := req.Header.Get(hdr); got != want {
			t.Errorf("header %s = %q; want %q", hdr, got, want)
		}
	}
}

func TestStampDownstreamHeaders_EmptyAuthSkipsOptionalHeaders(t *testing.T) {
	t.Parallel()
	agg := social.New(social.Config{})
	req, err := http.NewRequest(http.MethodGet, "http://downstream.internal/v1/x", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	agg.StampDownstreamHeaders(req, phyllis.AuthCtx{})
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q; want empty", got)
	}
	if got := req.Header.Get("X-Tenant-Id"); got != "" {
		t.Errorf("X-Tenant-Id = %q; want empty", got)
	}
	if got := req.Header.Get(servicemesh.HeaderUserRoles); got != "" {
		t.Errorf("x-mesh-user-roles = %q; want empty", got)
	}
}

func TestRevokeShareFromFeed_Stub502WhenUnconfigured(t *testing.T) {
	t.Parallel()
	agg := social.New(social.Config{})
	resp, err := agg.RevokeShareFromFeed(context.Background(), social.ShareAtomParams{
		Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"}, AtomID: "atom-1",
	})
	if err != nil {
		t.Fatalf("RevokeShareFromFeed: %v", err)
	}
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", resp.Status)
	}
	var body map[string]any
	_ = json.Unmarshal(resp.Body, &body)
	if body["error"] != "GATEWAY_NOT_CONFIGURED" {
		t.Errorf("error = %v; want GATEWAY_NOT_CONFIGURED", body["error"])
	}
}

func TestRevokeShareFromFeed_FansOutDelete(t *testing.T) {
	t.Parallel()
	var capturedMethod, capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	agg := social.New(social.Config{SharingURL: srv.URL})
	resp, err := agg.RevokeShareFromFeed(context.Background(), social.ShareAtomParams{
		Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"}, AtomID: "atom-1",
	})
	if err != nil {
		t.Fatalf("RevokeShareFromFeed: %v", err)
	}
	if resp.Status != http.StatusNoContent {
		t.Errorf("status = %d; want 204", resp.Status)
	}
	if capturedMethod != http.MethodDelete {
		t.Errorf("method = %q; want DELETE", capturedMethod)
	}
	if want := "/v1/atoms/atom-1/share"; capturedPath != want {
		t.Errorf("path = %q; want %q", capturedPath, want)
	}
}

func TestGetSharedAtomsFeed_StubEmptyPageWhenUnconfigured(t *testing.T) {
	t.Parallel()
	agg := social.New(social.Config{})
	resp, err := agg.GetSharedAtomsFeed(context.Background(), social.GetSharedAtomsParams{
		Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
	})
	if err != nil {
		t.Fatalf("GetSharedAtomsFeed: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	var body map[string]any
	_ = json.Unmarshal(resp.Body, &body)
	if _, ok := body["data"].([]any); !ok {
		t.Errorf("body.data not an array: %v", body)
	}
}

func TestGetSharedAtomsFeed_FansOutWithQueryParams(t *testing.T) {
	t.Parallel()
	var capturedPath, capturedQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[],"next_cursor":""}`))
	}))
	defer srv.Close()

	agg := social.New(social.Config{SharingURL: srv.URL})
	_, err := agg.GetSharedAtomsFeed(context.Background(), social.GetSharedAtomsParams{
		Auth:               phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
		Cursor:             "c1",
		Limit:              25,
		TopicFilter:        "physics",
		QuestionTypeFilter: "mcq",
		Scope:              "following",
	})
	if err != nil {
		t.Fatalf("GetSharedAtomsFeed: %v", err)
	}
	if capturedPath != "/v1/feed/shared-atoms" {
		t.Errorf("path = %q; want /v1/feed/shared-atoms", capturedPath)
	}
	for _, kv := range []string{"cursor=c1", "limit=25", "topic_filter=physics", "question_type_filter=mcq", "scope=following"} {
		if !strings.Contains(capturedQuery, kv) {
			t.Errorf("query %q missing %q", capturedQuery, kv)
		}
	}
}

func TestGetLeaderboard_StubEmptyPageWhenUnconfigured(t *testing.T) {
	t.Parallel()
	agg := social.New(social.Config{})
	resp, err := agg.GetLeaderboard(context.Background(), social.GetLeaderboardParams{
		Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
	})
	if err != nil {
		t.Fatalf("GetLeaderboard: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
}

func TestGetLeaderboard_FansOutWithQueryParams(t *testing.T) {
	t.Parallel()
	var capturedPath, capturedQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[],"next_cursor":""}`))
	}))
	defer srv.Close()

	agg := social.New(social.Config{SharingURL: srv.URL})
	_, err := agg.GetLeaderboard(context.Background(), social.GetLeaderboardParams{
		Auth:          phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
		Scope:         "class",
		ScopeTargetID: "class-9",
		Metric:        "duel_elo",
		Period:        "weekly",
		Limit:         10,
		Cursor:        "cur",
	})
	if err != nil {
		t.Fatalf("GetLeaderboard: %v", err)
	}
	if capturedPath != "/v1/leaderboard" {
		t.Errorf("path = %q; want /v1/leaderboard", capturedPath)
	}
	for _, kv := range []string{"scope=class", "scope_target_id=class-9", "metric=duel_elo", "period=weekly", "limit=10", "cursor=cur"} {
		if !strings.Contains(capturedQuery, kv) {
			t.Errorf("query %q missing %q", capturedQuery, kv)
		}
	}
}

func TestGetDuel_Stub501WhenUnconfigured(t *testing.T) {
	t.Parallel()
	agg := social.New(social.Config{})
	resp, err := agg.GetDuel(context.Background(), social.GetDuelParams{
		Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"}, DuelID: "duel-1",
	})
	if err != nil {
		t.Fatalf("GetDuel: %v", err)
	}
	if resp.Status != http.StatusNotImplemented {
		t.Errorf("status = %d; want 501", resp.Status)
	}
}

func TestGetDuel_FansOutWithAndWithoutDuelID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		duelID   string
		wantPath string
	}{
		{name: "list", duelID: "", wantPath: "/v1/duels"},
		{name: "single", duelID: "duel-1", wantPath: "/v1/duels/duel-1"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var capturedPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capturedPath = r.URL.Path
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"data":[]}`))
			}))
			defer srv.Close()

			agg := social.New(social.Config{SharingURL: srv.URL})
			_, err := agg.GetDuel(context.Background(), social.GetDuelParams{
				Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"}, DuelID: tc.duelID,
			})
			if err != nil {
				t.Fatalf("GetDuel: %v", err)
			}
			if capturedPath != tc.wantPath {
				t.Errorf("path = %q; want %q", capturedPath, tc.wantPath)
			}
		})
	}
}

func TestGetMyRating_Stub501AndFanOut(t *testing.T) {
	t.Parallel()
	agg := social.New(social.Config{})
	resp, err := agg.GetMyRating(context.Background(), social.GetMyRatingParams{
		Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
	})
	if err != nil {
		t.Fatalf("GetMyRating: %v", err)
	}
	if resp.Status != http.StatusNotImplemented {
		t.Errorf("stub status = %d; want 501", resp.Status)
	}

	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"rating":1200}`))
	}))
	defer srv.Close()

	agg2 := social.New(social.Config{SharingURL: srv.URL})
	resp2, err := agg2.GetMyRating(context.Background(), social.GetMyRatingParams{
		Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
	})
	if err != nil {
		t.Fatalf("GetMyRating (fan-out): %v", err)
	}
	if resp2.Status != http.StatusOK {
		t.Errorf("fan-out status = %d; want 200", resp2.Status)
	}
	if want := "/v1/duels/my-rating"; capturedPath != want {
		t.Errorf("path = %q; want %q", capturedPath, want)
	}
}

func TestGetDuelLeaderboard_Stub501AndFanOut(t *testing.T) {
	t.Parallel()
	agg := social.New(social.Config{})
	resp, err := agg.GetDuelLeaderboard(context.Background(), social.GetDuelLeaderboardParams{
		Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
	})
	if err != nil {
		t.Fatalf("GetDuelLeaderboard: %v", err)
	}
	if resp.Status != http.StatusNotImplemented {
		t.Errorf("stub status = %d; want 501", resp.Status)
	}

	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	agg2 := social.New(social.Config{SharingURL: srv.URL})
	if _, err := agg2.GetDuelLeaderboard(context.Background(), social.GetDuelLeaderboardParams{
		Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
	}); err != nil {
		t.Fatalf("GetDuelLeaderboard (fan-out): %v", err)
	}
	if want := "/v1/duels/leaderboard"; capturedPath != want {
		t.Errorf("path = %q; want %q", capturedPath, want)
	}
}

func TestQueueDuelOps_Stub502AndFanOut(t *testing.T) {
	t.Parallel()
	auth := phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a", IdempotencyKey: "idem-q"}
	cases := []struct {
		name     string
		method   string
		wantPath string
		call     func(*social.Aggregator) (social.Response, error)
	}{
		{
			name: "QueueDuel", method: http.MethodPost, wantPath: "/v1/duels/queue",
			call: func(a *social.Aggregator) (social.Response, error) {
				return a.QueueDuel(context.Background(), social.QueueDuelParams{Auth: auth, Body: []byte(`{"tier":"a"}`)})
			},
		},
		{
			name: "CancelQueueDuel", method: http.MethodDelete, wantPath: "/v1/duels/queue",
			call: func(a *social.Aggregator) (social.Response, error) {
				return a.CancelQueueDuel(context.Background(), social.QueueDuelParams{Auth: auth})
			},
		},
		{
			name: "HeartbeatQueue", method: http.MethodPost, wantPath: "/v1/duels/queue/heartbeat",
			call: func(a *social.Aggregator) (social.Response, error) {
				return a.HeartbeatQueue(context.Background(), social.QueueDuelParams{Auth: auth})
			},
		},
		{
			name: "QueueStatus", method: http.MethodGet, wantPath: "/v1/duels/queue/status",
			call: func(a *social.Aggregator) (social.Response, error) {
				return a.QueueStatus(context.Background(), social.QueueDuelParams{Auth: auth})
			},
		},
	}
	// Stub branch: unconfigured → 502 GATEWAY_NOT_CONFIGURED.
	agg := social.New(social.Config{})
	for _, tc := range cases {
		resp, err := tc.call(agg)
		if err != nil {
			t.Fatalf("%s stub: %v", tc.name, err)
		}
		if resp.Status != http.StatusBadGateway {
			t.Errorf("%s stub status = %d; want 502", tc.name, resp.Status)
		}
	}
	// Fan-out branch: method + path + idempotency forwarding.
	var lastMethod, lastPath, lastIdem string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastMethod = r.Method
		lastPath = r.URL.Path
		lastIdem = r.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	agg2 := social.New(social.Config{SharingURL: srv.URL})
	for _, tc := range cases {
		resp, err := tc.call(agg2)
		if err != nil {
			t.Fatalf("%s fan-out: %v", tc.name, err)
		}
		if resp.Status != http.StatusOK {
			t.Errorf("%s fan-out status = %d; want 200", tc.name, resp.Status)
		}
		if lastMethod != tc.method {
			t.Errorf("%s method = %q; want %q", tc.name, lastMethod, tc.method)
		}
		if lastPath != tc.wantPath {
			t.Errorf("%s path = %q; want %q", tc.name, lastPath, tc.wantPath)
		}
		if tc.name == "QueueDuel" && lastIdem != "idem-q" {
			t.Errorf("QueueDuel Idempotency-Key = %q; want idem-q", lastIdem)
		}
	}
}

func TestSubmitDuelAnswer_Stub502AndFanOut(t *testing.T) {
	t.Parallel()
	agg := social.New(social.Config{})
	resp, err := agg.SubmitDuelAnswer(context.Background(), social.SubmitDuelAnswerParams{
		Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"}, DuelID: "duel-1",
	})
	if err != nil {
		t.Fatalf("SubmitDuelAnswer: %v", err)
	}
	if resp.Status != http.StatusBadGateway {
		t.Errorf("stub status = %d; want 502", resp.Status)
	}

	var capturedMethod, capturedPath, capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	agg2 := social.New(social.Config{SharingURL: srv.URL})
	_, err = agg2.SubmitDuelAnswer(context.Background(), social.SubmitDuelAnswerParams{
		Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"}, DuelID: "duel-1",
		Body: []byte(`{"answer_id":"ans-1"}`),
	})
	if err != nil {
		t.Fatalf("SubmitDuelAnswer (fan-out): %v", err)
	}
	if capturedMethod != http.MethodPost || capturedPath != "/v1/duels/duel-1/answer" {
		t.Errorf("got %s %s; want POST /v1/duels/duel-1/answer", capturedMethod, capturedPath)
	}
	if !strings.Contains(capturedBody, `"answer_id":"ans-1"`) {
		t.Errorf("body not forwarded verbatim: %s", capturedBody)
	}
}

func TestDuelWS_Stub502AndFanOut(t *testing.T) {
	t.Parallel()
	agg := social.New(social.Config{})
	resp, err := agg.DuelWS(context.Background(), social.DuelWSParams{
		Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"}, DuelID: "duel-1",
	})
	if err != nil {
		t.Fatalf("DuelWS: %v", err)
	}
	if resp.Status != http.StatusBadGateway {
		t.Errorf("stub status = %d; want 502", resp.Status)
	}

	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	agg2 := social.New(social.Config{SharingURL: srv.URL})
	if _, err := agg2.DuelWS(context.Background(), social.DuelWSParams{
		Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"}, DuelID: "duel-1",
	}); err != nil {
		t.Fatalf("DuelWS (fan-out): %v", err)
	}
	if want := "/v1/duels/duel-1/ws"; capturedPath != want {
		t.Errorf("path = %q; want %q", capturedPath, want)
	}
}

func TestProfileOps_StubAndFanOut(t *testing.T) {
	t.Parallel()
	auth := phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"}
	// GetProfile stub → 501.
	agg := social.New(social.Config{})
	resp, err := agg.GetProfile(context.Background(), social.GetProfileParams{Auth: auth})
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if resp.Status != http.StatusNotImplemented {
		t.Errorf("GetProfile stub status = %d; want 501", resp.Status)
	}
	// GenerateProfile / UpdateProfileTags stub → 502.
	resp, err = agg.GenerateProfile(context.Background(), social.GenerateProfileParams{Auth: auth, Body: []byte(`{}`)})
	if err != nil {
		t.Fatalf("GenerateProfile: %v", err)
	}
	if resp.Status != http.StatusBadGateway {
		t.Errorf("GenerateProfile stub status = %d; want 502", resp.Status)
	}
	resp, err = agg.UpdateProfileTags(context.Background(), social.UpdateProfileTagsParams{Auth: auth, Body: []byte(`{}`)})
	if err != nil {
		t.Fatalf("UpdateProfileTags: %v", err)
	}
	if resp.Status != http.StatusBadGateway {
		t.Errorf("UpdateProfileTags stub status = %d; want 502", resp.Status)
	}

	// Fan-out: paths + methods.
	cases := []struct {
		name     string
		method   string
		wantPath string
		call     func(*social.Aggregator) error
	}{
		{
			name: "GetProfile", method: http.MethodGet, wantPath: "/v1/me/profile",
			call: func(a *social.Aggregator) error {
				_, err := a.GetProfile(context.Background(), social.GetProfileParams{Auth: auth})
				return err
			},
		},
		{
			name: "GenerateProfile", method: http.MethodPost, wantPath: "/v1/me/profile/generate",
			call: func(a *social.Aggregator) error {
				_, err := a.GenerateProfile(context.Background(), social.GenerateProfileParams{Auth: auth, Body: []byte(`{}`)})
				return err
			},
		},
		{
			name: "UpdateProfileTags", method: http.MethodPut, wantPath: "/v1/me/profile/tags",
			call: func(a *social.Aggregator) error {
				_, err := a.UpdateProfileTags(context.Background(), social.UpdateProfileTagsParams{Auth: auth, Body: []byte(`{}`)})
				return err
			},
		},
	}
	var lastMethod, lastPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastMethod = r.Method
		lastPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	agg2 := social.New(social.Config{SharingURL: srv.URL})
	for _, tc := range cases {
		if err := tc.call(agg2); err != nil {
			t.Fatalf("%s fan-out: %v", tc.name, err)
		}
		if lastMethod != tc.method || lastPath != tc.wantPath {
			t.Errorf("%s got %s %s; want %s %s", tc.name, lastMethod, lastPath, tc.method, tc.wantPath)
		}
	}
}

func TestListConnections_StubEmptyAndFanOut(t *testing.T) {
	t.Parallel()
	agg := social.New(social.Config{})
	resp, err := agg.ListConnections(context.Background(), social.ListConnectionsParams{
		Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
	})
	if err != nil {
		t.Fatalf("ListConnections: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("stub status = %d; want 200", resp.Status)
	}

	var capturedQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"connections":[],"next_cursor":""}`))
	}))
	defer srv.Close()

	agg2 := social.New(social.Config{SharingURL: srv.URL})
	_, err = agg2.ListConnections(context.Background(), social.ListConnectionsParams{
		Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
		Type: "following", Cursor: "c1", Limit: 20,
	})
	if err != nil {
		t.Fatalf("ListConnections (fan-out): %v", err)
	}
	for _, kv := range []string{"type=following", "cursor=c1", "limit=20"} {
		if !strings.Contains(capturedQuery, kv) {
			t.Errorf("query %q missing %q", capturedQuery, kv)
		}
	}
}

func TestConnectionWrites_Stub502AndFanOut(t *testing.T) {
	t.Parallel()
	auth := phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"}
	cases := []struct {
		name     string
		method   string
		wantPath string
		call     func(*social.Aggregator) (social.Response, error)
	}{
		{
			name: "FollowUser", method: http.MethodPost, wantPath: "/v1/connections/gcid-2/follow",
			call: func(a *social.Aggregator) (social.Response, error) {
				return a.FollowUser(context.Background(), auth, "gcid-2")
			},
		},
		{
			name: "UnfollowUser", method: http.MethodDelete, wantPath: "/v1/connections/gcid-2/follow",
			call: func(a *social.Aggregator) (social.Response, error) {
				return a.UnfollowUser(context.Background(), auth, "gcid-2")
			},
		},
		{
			name: "BlockUser", method: http.MethodPost, wantPath: "/v1/connections/gcid-2/block",
			call: func(a *social.Aggregator) (social.Response, error) {
				return a.BlockUser(context.Background(), auth, "gcid-2")
			},
		},
		{
			name: "UnblockUser", method: http.MethodDelete, wantPath: "/v1/connections/gcid-2/block",
			call: func(a *social.Aggregator) (social.Response, error) {
				return a.UnblockUser(context.Background(), auth, "gcid-2")
			},
		},
	}
	agg := social.New(social.Config{})
	for _, tc := range cases {
		resp, err := tc.call(agg)
		if err != nil {
			t.Fatalf("%s stub: %v", tc.name, err)
		}
		if resp.Status != http.StatusBadGateway {
			t.Errorf("%s stub status = %d; want 502", tc.name, resp.Status)
		}
	}
	var lastMethod, lastPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastMethod = r.Method
		lastPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	agg2 := social.New(social.Config{SharingURL: srv.URL})
	for _, tc := range cases {
		resp, err := tc.call(agg2)
		if err != nil {
			t.Fatalf("%s fan-out: %v", tc.name, err)
		}
		if resp.Status != http.StatusOK {
			t.Errorf("%s fan-out status = %d; want 200", tc.name, resp.Status)
		}
		if lastMethod != tc.method || lastPath != tc.wantPath {
			t.Errorf("%s got %s %s; want %s %s", tc.name, lastMethod, lastPath, tc.method, tc.wantPath)
		}
	}
}

func TestBookmarks_StubAndFanOut(t *testing.T) {
	t.Parallel()
	auth := phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"}
	agg := social.New(social.Config{})
	// BookmarkAtom stub → 502.
	resp, err := agg.BookmarkAtom(context.Background(), social.BookmarkParams{Auth: auth, AtomID: "atom-1"})
	if err != nil {
		t.Fatalf("BookmarkAtom: %v", err)
	}
	if resp.Status != http.StatusBadGateway {
		t.Errorf("BookmarkAtom stub status = %d; want 502", resp.Status)
	}
	// UnbookmarkAtom stub → 502.
	resp, err = agg.UnbookmarkAtom(context.Background(), social.BookmarkParams{Auth: auth, AtomID: "atom-1"})
	if err != nil {
		t.Fatalf("UnbookmarkAtom: %v", err)
	}
	if resp.Status != http.StatusBadGateway {
		t.Errorf("UnbookmarkAtom stub status = %d; want 502", resp.Status)
	}
	// ListBookmarks stub → 200 empty.
	resp, err = agg.ListBookmarks(context.Background(), social.ListBookmarksParams{Auth: auth})
	if err != nil {
		t.Fatalf("ListBookmarks: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("ListBookmarks stub status = %d; want 200", resp.Status)
	}

	var lastMethod, lastPath, lastQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastMethod = r.Method
		lastPath = r.URL.Path
		lastQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"bookmarks":[],"next_cursor":""}`))
	}))
	defer srv.Close()

	agg2 := social.New(social.Config{SharingURL: srv.URL})
	cases := []struct {
		name     string
		method   string
		wantPath string
		call     func(*social.Aggregator) error
	}{
		{
			name: "BookmarkAtom", method: http.MethodPost, wantPath: "/v1/atoms/atom-1/bookmark",
			call: func(a *social.Aggregator) error {
				_, err := a.BookmarkAtom(context.Background(), social.BookmarkParams{Auth: auth, AtomID: "atom-1"})
				return err
			},
		},
		{
			name: "UnbookmarkAtom", method: http.MethodDelete, wantPath: "/v1/atoms/atom-1/bookmark",
			call: func(a *social.Aggregator) error {
				_, err := a.UnbookmarkAtom(context.Background(), social.BookmarkParams{Auth: auth, AtomID: "atom-1"})
				return err
			},
		},
		{
			name: "ListBookmarks", method: http.MethodGet, wantPath: "/v1/me/bookmarks",
			call: func(a *social.Aggregator) error {
				_, err := a.ListBookmarks(context.Background(), social.ListBookmarksParams{Auth: auth, Cursor: "c1", Limit: 15})
				return err
			},
		},
	}
	for _, tc := range cases {
		if err := tc.call(agg2); err != nil {
			t.Fatalf("%s fan-out: %v", tc.name, err)
		}
		if lastMethod != tc.method || lastPath != tc.wantPath {
			t.Errorf("%s got %s %s; want %s %s", tc.name, lastMethod, lastPath, tc.method, tc.wantPath)
		}
		if tc.name == "ListBookmarks" {
			for _, kv := range []string{"cursor=c1", "limit=15"} {
				if !strings.Contains(lastQuery, kv) {
					t.Errorf("ListBookmarks query %q missing %q", lastQuery, kv)
				}
			}
		}
	}
}

// TestClassify_UpstreamErrorPaths covers the error/5xx classification branches:
// connection failure → 502 GATEWAY_UPSTREAM_ERROR, deadline → 504, upstream 5xx
// → 502 GATEWAY_UPSTREAM_5XX, clean 2xx → passthrough with headers.
func TestClassify_UpstreamErrorPaths(t *testing.T) {
	t.Parallel()
	// Connection refused: point SharingURL at a closed listener.
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := closed.URL
	closed.Close()

	agg := social.New(social.Config{SharingURL: closedURL, PerCallTimeout: 2 * time.Second})
	resp, err := agg.GetFeed(context.Background(), social.GetFeedParams{
		Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
	})
	if err != nil {
		t.Fatalf("GetFeed: %v", err)
	}
	if resp.Status != http.StatusBadGateway {
		t.Errorf("connection-failure status = %d; want 502", resp.Status)
	}
	var body map[string]any
	_ = json.Unmarshal(resp.Body, &body)
	if body["error"] != "GATEWAY_UPSTREAM_ERROR" {
		t.Errorf("error = %v; want GATEWAY_UPSTREAM_ERROR", body["error"])
	}

	// Upstream 5xx → classified as 502 GATEWAY_UPSTREAM_5XX.
	srv5xx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv5xx.Close()

	agg5xx := social.New(social.Config{SharingURL: srv5xx.URL, PerCallTimeout: 2 * time.Second})
	resp, err = agg5xx.GetFeed(context.Background(), social.GetFeedParams{
		Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
	})
	if err != nil {
		t.Fatalf("GetFeed (5xx): %v", err)
	}
	if resp.Status != http.StatusBadGateway {
		t.Errorf("5xx status = %d; want 502", resp.Status)
	}
	_ = json.Unmarshal(resp.Body, &body)
	if body["error"] != "GATEWAY_UPSTREAM_5XX" {
		t.Errorf("error = %v; want GATEWAY_UPSTREAM_5XX", body["error"])
	}

	// Clean 2xx passthrough including headers.
	srvOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Downstream", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srvOK.Close()

	aggOK := social.New(social.Config{SharingURL: srvOK.URL, PerCallTimeout: 2 * time.Second})
	resp, err = aggOK.GetFeed(context.Background(), social.GetFeedParams{
		Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
	})
	if err != nil {
		t.Fatalf("GetFeed (ok): %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Errorf("ok status = %d; want 201", resp.Status)
	}
	if got := resp.Headers.Get("X-Downstream"); got != "yes" {
		t.Errorf("passthrough header = %q; want yes", got)
	}
	if !strings.Contains(string(resp.Body), `"ok":true`) {
		t.Errorf("body not passed through: %s", resp.Body)
	}
}

func TestClassify_DeadlineExceededBecomes504(t *testing.T) {
	t.Parallel()
	// Server that sleeps far longer than the per-call timeout so
	// context.DeadlineExceeded wins and classify maps it to 504.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(500 * time.Millisecond):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	agg := social.New(social.Config{SharingURL: srv.URL, PerCallTimeout: 10 * time.Millisecond})
	resp, err := agg.GetFeed(context.Background(), social.GetFeedParams{
		Auth: phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
	})
	if err != nil {
		t.Fatalf("GetFeed: %v", err)
	}
	if resp.Status != http.StatusGatewayTimeout {
		t.Errorf("timeout status = %d; want 504", resp.Status)
	}
	var body map[string]any
	_ = json.Unmarshal(resp.Body, &body)
	if body["error"] != "GATEWAY_UPSTREAM_TIMEOUT" {
		t.Errorf("error = %v; want GATEWAY_UPSTREAM_TIMEOUT", body["error"])
	}
}
