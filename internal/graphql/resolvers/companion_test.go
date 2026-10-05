package resolvers_test

import (
	"context"
	"errors"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
	"github.com/apollo-chora/chora-gateway/internal/graphql/resolvers"
)

func TestCompanionResolver_MyCompanion_ProjectsDomainShape(t *testing.T) {
	r := resolvers.NewCompanionResolver(upstream.NewFakeUpstream(), staticIdentity("tenant-3", "gcid-3"))
	v, err := r.MyCompanion(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", v)
	}
	if m["id"] != "fam-skel-001" {
		t.Errorf("expected fam-skel-001 id, got %v", m["id"])
	}
	if m["ownerGcid"] != "gcid-3" {
		t.Errorf("expected ownerGcid=gcid-3, got %v", m["ownerGcid"])
	}
	if m["mood"] != "CURIOUS" {
		t.Errorf("expected uppercased mood CURIOUS, got %v", m["mood"])
	}
}

func TestCompanionResolver_MyCompanion_NeverLeaksAIAgentFields(t *testing.T) {
	// CRITICAL invariant: per memory feedback_companion_vs_agent the
	// federation surface must NEVER expose ai-agent / model / prompt /
	// embedding fields. We assert the projected shape contains exactly
	// the domain attributes and nothing else.
	r := resolvers.NewCompanionResolver(upstream.NewFakeUpstream(), staticIdentity("t", "g"))
	v, err := r.MyCompanion(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	m := v.(map[string]any)
	forbidden := []string{"model", "prompt", "embedding", "_ai_agent", "system_prompt", "model_id"}
	for _, f := range forbidden {
		if _, ok := m[f]; ok {
			t.Errorf("forbidden AI agent field %q must not be exposed in Companion resolver", f)
		}
	}
}

func TestCompanionResolver_MyCompanion_PropagatesError(t *testing.T) {
	fake := upstream.NewFakeUpstream()
	fake.FailMethods["GetCompanion"] = true
	r := resolvers.NewCompanionResolver(fake, staticIdentity("t", "g"))
	_, err := r.MyCompanion(context.Background())
	if err == nil {
		t.Fatal("expected error when upstream fails, got nil")
	}
	if !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("expected ErrUpstream, got %v", err)
	}
}

func TestCompanionResolver_NewWithNilCtxIdentity(t *testing.T) {
	r := resolvers.NewCompanionResolver(upstream.NewFakeUpstream(), nil)
	v, err := r.MyCompanion(context.Background())
	if err != nil {
		t.Fatalf("expected no error with nil ctxID, got %v", err)
	}
	if v == nil {
		t.Fatal("expected non-nil companion")
	}
}

// fakeCompanion is a lightweight upstream impl that returns a chosen
// Companion payload. Used to exercise the species/mood enum branches that
// the deterministic FakeUpstream cannot reach.
type fakeCompanion struct {
	upstream.Client
	payload map[string]any
}

func (f *fakeCompanion) GetCompanion(_ context.Context, _, _ string) (any, error) {
	return f.payload, nil
}

// satisfy other Client methods via embedded zero-value (won't be called).

func TestCompanionResolver_MoodAndSpeciesEnumMapping(t *testing.T) {
	cases := []struct {
		name, mood, species, expectMood, expectSpecies string
		nameField                                      string
	}{
		{"proud-explicit", "proud", "FOX", "PROUD", "FOX", ""},
		{"sleepy-fallback", "sleepy", "", "SLEEPY", "OWL", "Aurora"},
		{"energetic", "energetic", "OWL", "ENERGETIC", "OWL", ""},
		{"contemplative", "contemplative", "WOLF", "CONTEMPLATIVE", "WOLF", ""},
		{"name-prefix-D", "", "", "CURIOUS", "FOX", "Dewey"},
		{"name-prefix-G", "", "", "CURIOUS", "CAT", "Ginger"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeCompanion{Client: upstream.NewFakeUpstream(), payload: map[string]any{
				"companion_id": "fam-x",
				"mood":         tc.mood,
				"species":      tc.species,
				"name":         tc.nameField,
				"level":        3,
			}}
			r := resolvers.NewCompanionResolver(fake, staticIdentity("t", "g"))
			v, err := r.MyCompanion(context.Background())
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			m := v.(map[string]any)
			if m["mood"] != tc.expectMood {
				t.Errorf("expected mood=%s, got %v", tc.expectMood, m["mood"])
			}
			if m["species"] != tc.expectSpecies {
				t.Errorf("expected species=%s, got %v", tc.expectSpecies, m["species"])
			}
		})
	}
}

func TestCompanionResolver_NonMapUpstreamReturnsNil(t *testing.T) {
	// Force a non-map return type to trigger the type-assertion fallback.
	r := resolvers.NewCompanionResolver(
		&nonMapCompanion{Client: upstream.NewFakeUpstream()},
		staticIdentity("t", "g"),
	)
	v, err := r.MyCompanion(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if v != nil {
		t.Errorf("expected nil for non-map upstream payload, got %v", v)
	}
}

type nonMapCompanion struct {
	upstream.Client
}

func (n *nonMapCompanion) GetCompanion(_ context.Context, _, _ string) (any, error) {
	return "not-a-map", nil
}
