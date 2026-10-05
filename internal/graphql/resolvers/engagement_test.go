package resolvers_test

import (
	"context"
	"errors"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
	"github.com/apollo-chora/chora-gateway/internal/graphql/resolvers"
)

func staticIdentity(tenant, gcid string) func(context.Context) (string, string) {
	return func(context.Context) (string, string) { return tenant, gcid }
}

func TestEngagementResolver_MyPaths_ReturnsSkeletonPath(t *testing.T) {
	r := resolvers.NewEngagementResolver(upstream.NewFakeUpstream(), staticIdentity("tenant-1", "gcid-1"))
	paths, err := r.MyPaths(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("expected 1 path, got %d", len(paths))
	}
	m, ok := paths[0].(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", paths[0])
	}
	if m["path_id"] != "path-skeleton-001" {
		t.Errorf("unexpected path_id: %v", m["path_id"])
	}
}

func TestEngagementResolver_MyPaths_PropagatesUpstreamError(t *testing.T) {
	fake := upstream.NewFakeUpstream()
	fake.FailMethods["GetLearningPath"] = true
	r := resolvers.NewEngagementResolver(fake, staticIdentity("t", "g"))
	_, err := r.MyPaths(context.Background())
	if err == nil {
		t.Fatal("expected error when upstream fails, got nil")
	}
	if !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("expected ErrUpstream, got %v", err)
	}
}

func TestEngagementResolver_Path_RequiresID(t *testing.T) {
	r := resolvers.NewEngagementResolver(upstream.NewFakeUpstream(), staticIdentity("t", "g"))
	if _, err := r.Path(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty id, got nil")
	}
}

func TestEngagementResolver_Path_ReturnsPath(t *testing.T) {
	r := resolvers.NewEngagementResolver(upstream.NewFakeUpstream(), staticIdentity("t", "g"))
	p, err := r.Path(context.Background(), "path-skeleton-001")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil path")
	}
}

func TestEngagementResolver_DailyDose_ProjectsAtomIDs(t *testing.T) {
	r := resolvers.NewEngagementResolver(upstream.NewFakeUpstream(), staticIdentity("t", "g"))
	v, err := r.DailyDose(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	m := v.(map[string]any)
	ids, ok := m["atomIds"].([]string)
	if !ok {
		t.Fatalf("expected []string atomIds, got %T", m["atomIds"])
	}
	if len(ids) != 2 {
		t.Errorf("expected 2 atom ids, got %d", len(ids))
	}
}

func TestEngagementResolver_DailyDose_PropagatesError(t *testing.T) {
	fake := upstream.NewFakeUpstream()
	fake.FailMethods["GetRecentAtoms"] = true
	r := resolvers.NewEngagementResolver(fake, staticIdentity("t", "g"))
	if _, err := r.DailyDose(context.Background()); err == nil {
		t.Fatal("expected propagation of upstream error")
	}
}

func TestEngagementResolver_DiscoveryFeed_DefaultsDepth(t *testing.T) {
	r := resolvers.NewEngagementResolver(upstream.NewFakeUpstream(), staticIdentity("t", "g"))
	edges, err := r.DiscoveryFeed(context.Background(), "atom-skel-001", 0)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(edges) != 2 {
		t.Errorf("expected 2 edges, got %d", len(edges))
	}
}

func TestEngagementResolver_DiscoveryFeed_ClampsDepthAtFive(t *testing.T) {
	r := resolvers.NewEngagementResolver(upstream.NewFakeUpstream(), staticIdentity("t", "g"))
	edges, err := r.DiscoveryFeed(context.Background(), "", 99)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	// Skeleton mode always returns 2 edges, regardless of depth.
	if len(edges) == 0 {
		t.Errorf("expected non-empty edges")
	}
}

func TestEngagementResolver_NewWithNilCtxIdentity_DoesNotPanic(t *testing.T) {
	r := resolvers.NewEngagementResolver(upstream.NewFakeUpstream(), nil)
	if _, err := r.MyPaths(context.Background()); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}
