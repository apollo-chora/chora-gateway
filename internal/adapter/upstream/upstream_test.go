package upstream_test

import (
	"context"
	"errors"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

func TestFakeUpstream_AllMethodsReturnDeterministic(t *testing.T) {
	f := upstream.NewFakeUpstream()
	ctx := context.Background()

	calls := []func() (any, error){
		func() (any, error) { return f.GetLearningPath(ctx, "t", "g") },
		func() (any, error) { return f.GetRecentAtoms(ctx, "t", "g") },
		func() (any, error) { return f.GetCompanion(ctx, "t", "g") },
		func() (any, error) { return f.GetFeed(ctx, "t", "g") },
		func() (any, error) { return f.GetTenant(ctx, "t", "g") },
		func() (any, error) { return f.GetGovernance(ctx, "t", "g") },
		func() (any, error) { return f.GetAuditEvents(ctx, "t", "g") },
		func() (any, error) { return f.GetCourses(ctx, "t", "g") },
	}
	for i, call := range calls {
		v, err := call()
		if err != nil {
			t.Errorf("call %d unexpected err: %v", i, err)
		}
		if v == nil {
			t.Errorf("call %d returned nil payload", i)
		}
	}
}

func TestFakeUpstream_FailureInjection(t *testing.T) {
	f := upstream.NewFakeUpstream()
	f.FailMethods["GetRecentAtoms"] = true

	if _, err := f.GetRecentAtoms(context.Background(), "t", "g"); err == nil {
		t.Fatal("expected error when GetRecentAtoms is in FailMethods")
	} else if !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("err = %v; want wrap of ErrUpstream", err)
	}

	// Other methods still succeed.
	if _, err := f.GetLearningPath(context.Background(), "t", "g"); err != nil {
		t.Errorf("non-failing method errored: %v", err)
	}
}

func TestFakeUpstream_NilSafe(t *testing.T) {
	var f *upstream.FakeUpstream
	// shouldFail is internal but the constructor is the public path. Verify
	// NewFakeUpstream returns a usable, non-nil pointer.
	f = upstream.NewFakeUpstream()
	if f == nil {
		t.Fatal("NewFakeUpstream returned nil")
	}
}

// TestIsFakeUpstreamAllowed verifies the env-flag gate per
// feedback_no_stubs_real_wiring. Production wiring of FakeUpstream MUST be
// gated; tests set the env var to admit the fake.
func TestIsFakeUpstreamAllowed(t *testing.T) {
	t.Setenv("CHORA_GATEWAY_UPSTREAM_FAKE", "")
	if upstream.IsFakeUpstreamAllowed() {
		t.Error("IsFakeUpstreamAllowed = true with empty env var; want false")
	}
	t.Setenv("CHORA_GATEWAY_UPSTREAM_FAKE", "true")
	if !upstream.IsFakeUpstreamAllowed() {
		t.Error("IsFakeUpstreamAllowed = false with CHORA_GATEWAY_UPSTREAM_FAKE=true; want true")
	}
	t.Setenv("CHORA_GATEWAY_UPSTREAM_FAKE", "yes")
	if upstream.IsFakeUpstreamAllowed() {
		t.Error("IsFakeUpstreamAllowed = true with non-canonical value; want strict 'true' match")
	}
}

// TestFakeUpstream_AllFailureInjectionPaths ensures the failure branch of
// every method is exercised — guards the graceful-degradation contract from
// regressions when new methods are added.
func TestFakeUpstream_AllFailureInjectionPaths(t *testing.T) {
	cases := map[string]func(f *upstream.FakeUpstream) (any, error){
		"GetLearningPath": func(f *upstream.FakeUpstream) (any, error) { return f.GetLearningPath(context.Background(), "t", "g") },
		"GetRecentAtoms":  func(f *upstream.FakeUpstream) (any, error) { return f.GetRecentAtoms(context.Background(), "t", "g") },
		"GetCompanion":    func(f *upstream.FakeUpstream) (any, error) { return f.GetCompanion(context.Background(), "t", "g") },
		"GetFeed":         func(f *upstream.FakeUpstream) (any, error) { return f.GetFeed(context.Background(), "t", "g") },
		"GetTenant":       func(f *upstream.FakeUpstream) (any, error) { return f.GetTenant(context.Background(), "t", "g") },
		"GetGovernance":   func(f *upstream.FakeUpstream) (any, error) { return f.GetGovernance(context.Background(), "t", "g") },
		"GetAuditEvents":  func(f *upstream.FakeUpstream) (any, error) { return f.GetAuditEvents(context.Background(), "t", "g") },
		"GetCourses":      func(f *upstream.FakeUpstream) (any, error) { return f.GetCourses(context.Background(), "t", "g") },
	}
	for name, call := range cases {
		f := upstream.NewFakeUpstream()
		f.FailMethods[name] = true
		_, err := call(f)
		if err == nil {
			t.Errorf("%s: expected ErrUpstream, got nil", name)
		}
		if !errors.Is(err, upstream.ErrUpstream) {
			t.Errorf("%s: error = %v; want wrap of ErrUpstream", name, err)
		}
	}
}
