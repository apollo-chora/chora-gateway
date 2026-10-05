// auth_ctx_propagation_test.go — RED-phase TDD specs for upstream.AuthCtx
// propagation from GraphQL resolvers to HTTPUpstream call sites.
//
// Same gap as the surface BFF handlers — resolvers were calling
// r.Upstream.Get*(ctx, tenantID, gcid) without first stamping
// upstream.WithAuthCtx(ctx, AuthCtx{TenantID, GCID, Traceparent}). The
// downstream HTTPUpstream adapter then sees zero AuthCtx and outbound
// calls carry no traceparent + partial mesh metadata.
package resolvers_test

import (
	"context"
	"sync"
	"testing"

	"github.com/apollo-chora/chora-common/tracing"
	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
	"github.com/apollo-chora/chora-gateway/internal/graphql/resolvers"
)

// authCapturingUpstream is a fake upstream.Client that records the AuthCtx
// visible via upstream.AuthCtxFromContext on every method call.
type authCapturingUpstream struct {
	upstream.Client
	mu   sync.Mutex
	seen map[string]upstream.AuthCtx
}

func newAuthCapturingUpstream() *authCapturingUpstream {
	return &authCapturingUpstream{
		Client: upstream.NewFakeUpstream(),
		seen:   map[string]upstream.AuthCtx{},
	}
}

func (c *authCapturingUpstream) record(method string, ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen[method] = upstream.AuthCtxFromContext(ctx)
}

func (c *authCapturingUpstream) Get(method string) (upstream.AuthCtx, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.seen[method]
	return a, ok
}

func (c *authCapturingUpstream) GetLearningPath(ctx context.Context, tenantID, gcid string) (any, error) {
	c.record("GetLearningPath", ctx)
	return c.Client.GetLearningPath(ctx, tenantID, gcid)
}

func (c *authCapturingUpstream) GetRecentAtoms(ctx context.Context, tenantID, gcid string) (any, error) {
	c.record("GetRecentAtoms", ctx)
	return c.Client.GetRecentAtoms(ctx, tenantID, gcid)
}

func (c *authCapturingUpstream) GetCompanion(ctx context.Context, tenantID, gcid string) (any, error) {
	c.record("GetCompanion", ctx)
	return c.Client.GetCompanion(ctx, tenantID, gcid)
}

func (c *authCapturingUpstream) GetFeed(ctx context.Context, tenantID, gcid string) (any, error) {
	c.record("GetFeed", ctx)
	return c.Client.GetFeed(ctx, tenantID, gcid)
}

// inCtxWithTraceparent attaches a traceparent to ctx via the canonical
// tracing.WithTraceparent helper so resolvers (and the handler upstream)
// can pull it out and stamp downstream.
func inCtxWithTraceparent(tp string) context.Context {
	return tracing.WithTraceparent(context.Background(), tp)
}

const testTraceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

func TestEngagementResolver_MyPaths_StampsAuthCtxOnUpstreamCall(t *testing.T) {
	cap := newAuthCapturingUpstream()
	r := resolvers.NewEngagementResolver(cap, staticIdentity("tenant-1", "gcid-1"))
	if _, err := r.MyPaths(inCtxWithTraceparent(testTraceparent)); err != nil {
		t.Fatalf("err: %v", err)
	}
	auth, ok := cap.Get("GetLearningPath")
	if !ok {
		t.Fatalf("GetLearningPath not invoked")
	}
	if auth.TenantID != "tenant-1" {
		t.Errorf("TenantID = %q; want tenant-1", auth.TenantID)
	}
	if auth.GCID != "gcid-1" {
		t.Errorf("GCID = %q; want gcid-1", auth.GCID)
	}
	if auth.Traceparent != testTraceparent {
		t.Errorf("Traceparent = %q; want %q", auth.Traceparent, testTraceparent)
	}
}

func TestEngagementResolver_Path_StampsAuthCtxOnUpstreamCall(t *testing.T) {
	cap := newAuthCapturingUpstream()
	r := resolvers.NewEngagementResolver(cap, staticIdentity("tenant-2", "gcid-2"))
	if _, err := r.Path(inCtxWithTraceparent(testTraceparent), "path-1"); err != nil {
		t.Fatalf("err: %v", err)
	}
	auth, ok := cap.Get("GetLearningPath")
	if !ok {
		t.Fatalf("GetLearningPath not invoked")
	}
	if auth.TenantID != "tenant-2" || auth.GCID != "gcid-2" {
		t.Errorf("identity = (%q, %q); want (tenant-2, gcid-2)", auth.TenantID, auth.GCID)
	}
	if auth.Traceparent != testTraceparent {
		t.Errorf("Traceparent not propagated")
	}
}

func TestEngagementResolver_DailyDose_StampsAuthCtxOnUpstreamCall(t *testing.T) {
	cap := newAuthCapturingUpstream()
	r := resolvers.NewEngagementResolver(cap, staticIdentity("tenant-3", "gcid-3"))
	if _, err := r.DailyDose(inCtxWithTraceparent(testTraceparent)); err != nil {
		t.Fatalf("err: %v", err)
	}
	auth, ok := cap.Get("GetRecentAtoms")
	if !ok {
		t.Fatalf("GetRecentAtoms not invoked")
	}
	if auth.TenantID != "tenant-3" || auth.GCID != "gcid-3" {
		t.Errorf("identity = (%q, %q)", auth.TenantID, auth.GCID)
	}
	if auth.Traceparent != testTraceparent {
		t.Errorf("Traceparent not propagated")
	}
}

func TestCompanionResolver_MyCompanion_StampsAuthCtxOnUpstreamCall(t *testing.T) {
	cap := newAuthCapturingUpstream()
	r := resolvers.NewCompanionResolver(cap, staticIdentity("tenant-4", "gcid-4"))
	if _, err := r.MyCompanion(inCtxWithTraceparent(testTraceparent)); err != nil {
		t.Fatalf("err: %v", err)
	}
	auth, ok := cap.Get("GetCompanion")
	if !ok {
		t.Fatalf("GetCompanion not invoked")
	}
	if auth.TenantID != "tenant-4" || auth.GCID != "gcid-4" {
		t.Errorf("identity = (%q, %q)", auth.TenantID, auth.GCID)
	}
	if auth.Traceparent != testTraceparent {
		t.Errorf("Traceparent not propagated")
	}
}

func TestCircleResolver_Feed_StampsAuthCtxOnUpstreamCall(t *testing.T) {
	cap := newAuthCapturingUpstream()
	r := resolvers.NewCircleResolver(cap, staticIdentity("tenant-5", "gcid-5"))
	if _, err := r.Feed(inCtxWithTraceparent(testTraceparent), "HOME", ""); err != nil {
		t.Fatalf("err: %v", err)
	}
	auth, ok := cap.Get("GetFeed")
	if !ok {
		t.Fatalf("GetFeed not invoked")
	}
	if auth.TenantID != "tenant-5" || auth.GCID != "gcid-5" {
		t.Errorf("identity = (%q, %q)", auth.TenantID, auth.GCID)
	}
	if auth.Traceparent != testTraceparent {
		t.Errorf("Traceparent not propagated")
	}
}
