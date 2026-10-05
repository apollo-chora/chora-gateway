// Package resolvers contains the four federated subschema resolver
// implementations for the chora-gateway GraphQL gateway. Each resolver
// is a thin adapter that calls into the existing REST upstream client (or
// the Phyllis aggregator), translating its `any` payload into a
// federation-friendly map[string]any.
//
// Per CLAUDE.md §4 GraphQL is for learner-facing reads — these resolvers
// MUST never carry mutations. Mutations stay on REST per the protocol
// strategy.
package resolvers

import (
	"context"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

// EngagementResolver wires the engagement subschema (LearningPath /
// AtomAttempt / DailyDose / KnowledgeGraphEdge) onto the existing
// upstream HTTP client.
type EngagementResolver struct {
	Upstream upstream.Client
	// CtxIdentity is a pluggable function that extracts (tenantID, gcid)
	// from the request context. Tests override with a deterministic value.
	CtxIdentity func(ctx context.Context) (string, string)
}

// NewEngagementResolver constructs the resolver with the canonical context
// identity extractor that reads from the BFF middleware's auth context.
func NewEngagementResolver(up upstream.Client, ctxID func(ctx context.Context) (string, string)) *EngagementResolver {
	if ctxID == nil {
		ctxID = func(ctx context.Context) (string, string) { return "", "" }
	}
	return &EngagementResolver{Upstream: up, CtxIdentity: ctxID}
}

// MyPaths resolves the `myPaths` query — the current learner's enrolled
// LearningPaths. Backed by chora-consumption.GetLearningPath; the upstream
// returns a single canonical path in skeleton mode, which we wrap in a
// slice for federation compatibility.
func (r *EngagementResolver) MyPaths(ctx context.Context) ([]any, error) {
	tenantID, gcid := r.CtxIdentity(ctx)
	authCtx := withResolverAuthCtx(ctx, tenantID, gcid)
	v, err := r.Upstream.GetLearningPath(authCtx, tenantID, gcid)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return []any{}, nil
	}
	return []any{v}, nil
}

// Path resolves the `path(id)` query.
func (r *EngagementResolver) Path(ctx context.Context, id string) (any, error) {
	if id == "" {
		return nil, errInvalid("id is required")
	}
	tenantID, gcid := r.CtxIdentity(ctx)
	authCtx := withResolverAuthCtx(ctx, tenantID, gcid)
	v, err := r.Upstream.GetLearningPath(authCtx, tenantID, gcid)
	if err != nil {
		return nil, err
	}
	// Skeleton-mode fake returns one canonical LearningPath; we just hand
	// it back. Real implementation will query by id.
	return v, nil
}

// DailyDose resolves the `dailyDose` query.
func (r *EngagementResolver) DailyDose(ctx context.Context) (any, error) {
	tenantID, gcid := r.CtxIdentity(ctx)
	authCtx := withResolverAuthCtx(ctx, tenantID, gcid)
	atoms, err := r.Upstream.GetRecentAtoms(authCtx, tenantID, gcid)
	if err != nil {
		return nil, err
	}
	atomList, _ := atoms.([]map[string]any)
	atomIDs := make([]string, 0, len(atomList))
	for _, a := range atomList {
		if id, ok := a["atom_id"].(string); ok {
			atomIDs = append(atomIDs, id)
		}
	}
	return map[string]any{
		"id":               "dose-skel-" + gcid,
		"atomIds":          atomIDs,
		"estimatedMinutes": 15,
		"_stub":            true,
	}, nil
}

// DiscoveryFeed resolves the `discoveryFeed(seedAtomId, depth)` query —
// KnowledgeGraph traversal for Discovery / curiosity-driven mode.
func (r *EngagementResolver) DiscoveryFeed(_ context.Context, seedAtomID string, depth int) ([]any, error) {
	if depth < 1 {
		depth = 2
	}
	if depth > 5 {
		depth = 5
	}
	if seedAtomID == "" {
		seedAtomID = "atom-skel-001"
	}
	// Skeleton: return a deterministic 2-edge fan-out from the seed. Real
	// implementation calls chora-consumption knowledge-graph traversal.
	return []any{
		map[string]any{
			"id":             "edge-001",
			"sourceNodeId":   seedAtomID,
			"targetNodeId":   "atom-skel-002",
			"edgeType":       "EXTENDS",
			"weight":         0.85,
			"traversalDepth": 1,
		},
		map[string]any{
			"id":             "edge-002",
			"sourceNodeId":   seedAtomID,
			"targetNodeId":   "atom-skel-003",
			"edgeType":       "ANALOGY_OF",
			"weight":         0.62,
			"traversalDepth": 1,
		},
	}, nil
}
