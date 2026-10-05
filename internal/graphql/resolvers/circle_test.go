package resolvers_test

import (
	"context"
	"errors"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
	"github.com/apollo-chora/chora-gateway/internal/graphql/resolvers"
)

func TestCircleResolver_Feed_ProjectsPostsToConnection(t *testing.T) {
	r := resolvers.NewCircleResolver(upstream.NewFakeUpstream(), staticIdentity("tenant-9", "gcid-9"))
	v, err := r.Feed(context.Background(), "HOME", "")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	m := v.(map[string]any)
	edges := m["edges"].([]map[string]any)
	if len(edges) != 2 {
		t.Errorf("expected 2 edges, got %d", len(edges))
	}
	if m["totalCount"].(int) != 2 {
		t.Errorf("expected totalCount=2, got %v", m["totalCount"])
	}
}

func TestCircleResolver_Feed_DefaultScope(t *testing.T) {
	r := resolvers.NewCircleResolver(upstream.NewFakeUpstream(), staticIdentity("t", "g"))
	v, _ := r.Feed(context.Background(), "", "")
	m := v.(map[string]any)
	if m["_scope"] != "HOME" {
		t.Errorf("expected default scope HOME, got %v", m["_scope"])
	}
}

func TestCircleResolver_Feed_PropagatesError(t *testing.T) {
	fake := upstream.NewFakeUpstream()
	fake.FailMethods["GetFeed"] = true
	r := resolvers.NewCircleResolver(fake, staticIdentity("t", "g"))
	if _, err := r.Feed(context.Background(), "HOME", ""); !errors.Is(err, upstream.ErrUpstream) {
		t.Fatalf("expected ErrUpstream, got %v", err)
	}
}

func TestCircleResolver_MyProfile_ReturnsGcidOrDefault(t *testing.T) {
	r := resolvers.NewCircleResolver(upstream.NewFakeUpstream(), staticIdentity("", ""))
	p, err := r.MyProfile(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if p["gcid"] != "gcid-skel" {
		t.Errorf("expected default gcid-skel, got %v", p["gcid"])
	}
}

func TestCircleResolver_FollowingAndFollowers(t *testing.T) {
	r := resolvers.NewCircleResolver(upstream.NewFakeUpstream(), staticIdentity("t", "g"))
	f1, err := r.Following(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	f2, err := r.Followers(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(f1) != 1 || len(f2) != 1 {
		t.Errorf("expected 1 following + 1 follower, got %d/%d", len(f1), len(f2))
	}
}

func TestCircleResolver_NewWithNilCtxIdentity(t *testing.T) {
	r := resolvers.NewCircleResolver(upstream.NewFakeUpstream(), nil)
	if _, err := r.Feed(context.Background(), "HOME", ""); err != nil {
		t.Fatalf("expected no error with nil ctxID, got %v", err)
	}
}

// nonMapFeed is an upstream stub returning a non-map payload — exercises
// the emptyFeed fallback in CircleResolver.Feed.
type nonMapFeed struct {
	upstream.Client
}

func (n *nonMapFeed) GetFeed(_ context.Context, _, _ string) (any, error) {
	return "not-a-map", nil
}

func TestCircleResolver_Feed_NonMapPayloadReturnsEmpty(t *testing.T) {
	r := resolvers.NewCircleResolver(&nonMapFeed{Client: upstream.NewFakeUpstream()}, staticIdentity("t", "g"))
	v, err := r.Feed(context.Background(), "HOME", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := v.(map[string]any)
	if m["totalCount"].(int) != 0 {
		t.Errorf("expected empty feed, got totalCount=%v", m["totalCount"])
	}
}
