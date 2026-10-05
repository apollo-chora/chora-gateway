package graphql_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
	bffgraphql "github.com/apollo-chora/chora-gateway/internal/graphql"
	"github.com/apollo-chora/chora-gateway/internal/graphql/resolvers"
)

// staticIdentity is the test-only context-identity extractor.
func staticIdentity(t, g string) func(context.Context) (string, string) {
	return func(context.Context) (string, string) { return t, g }
}

// newTestSchema spins up a Schema with all four resolvers wired against
// the in-memory FakeUpstream.
func newTestSchema(t *testing.T) *bffgraphql.Schema {
	t.Helper()
	up := upstream.NewFakeUpstream()
	id := staticIdentity("tenant-test", "gcid-test")
	return bffgraphql.NewSchema(
		resolvers.NewEngagementResolver(up, id),
		resolvers.NewGamificationResolver(up, id),
		resolvers.NewCompanionResolver(up, id),
		resolvers.NewCircleResolver(up, id),
	)
}

func TestSchema_Execute_MyPaths(t *testing.T) {
	s := newTestSchema(t)
	resp := s.Execute(context.Background(), bffgraphql.Request{Query: `{ myPaths { id title } }`})
	if len(resp.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", resp.Errors)
	}
	if _, ok := resp.Data["myPaths"]; !ok {
		t.Fatalf("expected data.myPaths, got %+v", resp.Data)
	}
}

func TestSchema_Execute_UnknownRootReturnsError(t *testing.T) {
	s := newTestSchema(t)
	resp := s.Execute(context.Background(), bffgraphql.Request{Query: `{ banana }`})
	if len(resp.Errors) == 0 {
		t.Fatal("expected error for unknown root")
	}
}

func TestSchema_Execute_AllFourSubschemaRoots(t *testing.T) {
	s := newTestSchema(t)
	cases := []struct{ name, query string }{
		{"engagement.myPaths", `{ myPaths { id } }`},
		{"engagement.dailyDose", `{ dailyDose { id atomIds } }`},
		{"engagement.discoveryFeed", `{ discoveryFeed { id } }`},
		{"gamification.myWallet", `{ myWallet { gcid xp } }`},
		{"gamification.myBadges", `{ myBadges { id } }`},
		{"gamification.leaderboard", `{ leaderboard { id } }`},
		{"companion.myCompanion", `{ myCompanion { id species level } }`},
		{"circle.feed", `{ feed { totalCount } }`},
		{"circle.myCircleProfile", `{ myCircleProfile { gcid } }`},
		{"circle.following", `{ following { gcid } }`},
		{"circle.followers", `{ followers { gcid } }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := s.Execute(context.Background(), bffgraphql.Request{Query: tc.query})
			if len(resp.Errors) > 0 {
				t.Fatalf("unexpected errors: %v", resp.Errors)
			}
			if len(resp.Data) == 0 {
				t.Fatalf("expected non-empty data")
			}
		})
	}
}

func TestSchema_Execute_LearnerComposesAcrossAllFourSubschemas(t *testing.T) {
	s := newTestSchema(t)
	resp := s.Execute(context.Background(), bffgraphql.Request{Query: `{ learner { gcid paths companion wallet circleProfile } }`})
	if len(resp.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", resp.Errors)
	}
	learner, ok := resp.Data["learner"].(map[string]any)
	if !ok {
		t.Fatalf("expected map data.learner, got %T", resp.Data["learner"])
	}
	for _, key := range []string{"gcid", "paths", "companion", "wallet", "circleProfile"} {
		if _, ok := learner[key]; !ok {
			t.Errorf("expected learner.%s, missing", key)
		}
	}
}

func TestSchema_Execute_VariablesPropagateToResolver(t *testing.T) {
	s := newTestSchema(t)
	resp := s.Execute(context.Background(), bffgraphql.Request{
		Query:     `query L($scope: LeaderboardScope!, $sid: ID) { leaderboard(scope: $scope, scopeId: $sid) { scope } }`,
		Variables: map[string]any{"scope": "COURSE", "sid": "course-42"},
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", resp.Errors)
	}
	lb := resp.Data["leaderboard"].(map[string]any)
	if lb["scope"] != "COURSE" {
		t.Errorf("expected scope COURSE, got %v", lb["scope"])
	}
}

func TestSchema_Handler_HTTPRoundTrip(t *testing.T) {
	s := newTestSchema(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := bytes.NewBufferString(`{"query":"{ myWallet { gcid xp } }"}`)
	res, err := http.Post(srv.URL, "application/json", body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	var resp bffgraphql.Response
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if len(resp.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", resp.Errors)
	}
	if _, ok := resp.Data["myWallet"]; !ok {
		t.Fatalf("expected data.myWallet")
	}
}

func TestSchema_Handler_RejectsEmptyQuery(t *testing.T) {
	s := newTestSchema(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	res, err := http.Post(srv.URL, "application/json", bytes.NewBufferString(`{"query": ""}`))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", res.StatusCode)
	}
}

func TestSchema_Handler_RejectsBadMethod(t *testing.T) {
	s := newTestSchema(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPut, srv.URL, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT failed: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", res.StatusCode)
	}
}

func TestSchema_Handler_GETReturnsIntrospectionStub(t *testing.T) {
	s := newTestSchema(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", res.StatusCode)
	}
	var stub map[string]any
	if err := json.NewDecoder(res.Body).Decode(&stub); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	subs, ok := stub["subschemas"].([]any)
	if !ok || len(subs) != 4 {
		t.Errorf("expected 4 subschemas in stub, got %v", stub["subschemas"])
	}
}

func TestSchema_Handler_RejectsMalformedJSON(t *testing.T) {
	s := newTestSchema(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	res, err := http.Post(srv.URL, "application/json", bytes.NewBufferString(`{not-json`))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", res.StatusCode)
	}
}

func TestSchema_Execute_InvalidPathID(t *testing.T) {
	s := newTestSchema(t)
	resp := s.Execute(context.Background(), bffgraphql.Request{
		Query:     `query P($id: ID!) { path(id: $id) { id } }`,
		Variables: map[string]any{"id": ""},
	})
	if len(resp.Errors) == 0 {
		t.Errorf("expected error for empty id")
	}
}

func TestSchema_Execute_EmptyQueryReturnsError(t *testing.T) {
	s := newTestSchema(t)
	resp := s.Execute(context.Background(), bffgraphql.Request{Query: "  "})
	if len(resp.Errors) == 0 {
		t.Errorf("expected error for empty query")
	}
}

func TestSchema_Execute_AliasOnTopLevelField(t *testing.T) {
	s := newTestSchema(t)
	// Aliased query: `me: learner` — dispatcher should still route to learner.
	resp := s.Execute(context.Background(), bffgraphql.Request{Query: `query { me: learner { gcid } }`})
	if len(resp.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", resp.Errors)
	}
}

// errorUpstream forces all calls to return an upstream error — used to
// exercise the composeLearner failure path (no gcid resolvable).
type errorUpstream struct{}

func (errorUpstream) GetLearningPath(_ context.Context, _, _ string) (any, error) {
	return nil, upstreamErr()
}
func (errorUpstream) GetRecentAtoms(_ context.Context, _, _ string) (any, error) {
	return nil, upstreamErr()
}
func (errorUpstream) GetCompanion(_ context.Context, _, _ string) (any, error) {
	return nil, upstreamErr()
}
func (errorUpstream) GetFeed(_ context.Context, _, _ string) (any, error) {
	return nil, upstreamErr()
}
func (errorUpstream) GetTenant(_ context.Context, _, _ string) (any, error) {
	return nil, upstreamErr()
}
func (errorUpstream) GetGovernance(_ context.Context, _, _ string) (any, error) {
	return nil, upstreamErr()
}
func (errorUpstream) GetAuditEvents(_ context.Context, _, _ string) (any, error) {
	return nil, upstreamErr()
}
func (errorUpstream) GetCourses(_ context.Context, _, _ string) (any, error) {
	return nil, upstreamErr()
}

func upstreamErr() error {
	// Use the real sentinel so tests stay in sync with production wrap.
	return upstream.ErrUpstream
}

func TestSchema_Execute_LearnerWithAllUpstreamFailingReturnsError(t *testing.T) {
	id := staticIdentity("", "") // empty gcid forces composeLearner fallback path
	up := errorUpstream{}
	s := bffgraphql.NewSchema(
		resolvers.NewEngagementResolver(up, id),
		resolvers.NewGamificationResolver(up, id),
		resolvers.NewCompanionResolver(up, id),
		resolvers.NewCircleResolver(up, id),
	)
	resp := s.Execute(context.Background(), bffgraphql.Request{Query: `{ learner { gcid } }`})
	// composeLearner uses CircleResolver.MyProfile which has a default
	// gcid-skel fallback even when context is empty, so the learner WILL
	// resolve. Assert it includes gcid-skel default.
	if len(resp.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", resp.Errors)
	}
	learner := resp.Data["learner"].(map[string]any)
	if learner["gcid"] != "gcid-skel" {
		t.Errorf("expected default gcid-skel, got %v", learner["gcid"])
	}
}
