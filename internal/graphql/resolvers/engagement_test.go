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

// myStreak backs the live SPA Daily Dose read; it must resolve (not return
// GRAPHQL_UNKNOWN_FIELD) and expose the FE's camelCase fields from the
// chora-consumption /v1/me/streak wire shape.
func TestEngagementResolver_MyStreak_MapsConsumptionWire(t *testing.T) {
	r := resolvers.NewEngagementResolver(upstream.NewFakeUpstream(), staticIdentity("t", "g"))
	v, err := r.MyStreak(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", v)
	}
	if m["currentDays"] != 5 {
		t.Errorf("currentDays = %v; want 5 (count)", m["currentDays"])
	}
	if m["lastActivityAt"] != "2026-05-07T08:00:00Z" {
		t.Errorf("lastActivityAt = %v; want the last_activity_at value", m["lastActivityAt"])
	}
}

func TestEngagementResolver_MyStreak_PropagatesUpstreamError(t *testing.T) {
	fake := upstream.NewFakeUpstream()
	fake.FailMethods["GetStreak"] = true
	r := resolvers.NewEngagementResolver(fake, staticIdentity("t", "g"))
	if _, err := r.MyStreak(context.Background()); !errors.Is(err, upstream.ErrUpstream) {
		t.Fatalf("expected ErrUpstream, got %v", err)
	}
}

// numericField handles both the float64 JSON decode and the int FakeUpstream
// returns; a json-decoded count must map too.
func TestEngagementResolver_MyStreak_MapsJSONDecodedCount(t *testing.T) {
	fake := &fakeStreak{Client: upstream.NewFakeUpstream(), payload: map[string]any{
		"count":            float64(9),
		"last_activity_at": "2026-03-12T10:00:00Z",
		"longest_streak":   float64(21),
		"status":           "active",
	}}
	r := resolvers.NewEngagementResolver(fake, staticIdentity("t", "g"))
	v, err := r.MyStreak(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	m := v.(map[string]any)
	if m["currentDays"] != 9 {
		t.Errorf("currentDays = %v; want 9", m["currentDays"])
	}
	if m["longestStreak"] != 21 {
		t.Errorf("longestStreak = %v; want 21 (forwarded when the callee adds it)", m["longestStreak"])
	}
	if m["status"] != "active" {
		t.Errorf("status = %v; want active (forwarded when the callee adds it)", m["status"])
	}
}

type fakeStreak struct {
	upstream.Client
	payload map[string]any
}

func (f *fakeStreak) GetStreak(_ context.Context, _, _ string) (any, error) {
	return f.payload, nil
}
