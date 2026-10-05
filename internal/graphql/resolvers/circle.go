package resolvers

import (
	"context"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

// CircleResolver wires the circle subschema — social graph reads
// (feed, posts, reactions, profile, followers, following).
type CircleResolver struct {
	Upstream    upstream.Client
	CtxIdentity func(ctx context.Context) (string, string)
}

// NewCircleResolver constructs the resolver.
func NewCircleResolver(up upstream.Client, ctxID func(ctx context.Context) (string, string)) *CircleResolver {
	if ctxID == nil {
		ctxID = func(ctx context.Context) (string, string) { return "", "" }
	}
	return &CircleResolver{Upstream: up, CtxIdentity: ctxID}
}

// Feed resolves the `feed(scope, groupId)` query. Backed by
// chora-sharing.GetFeed with light projection into the SDL shape.
func (r *CircleResolver) Feed(ctx context.Context, scope, groupID string) (any, error) {
	if scope == "" {
		scope = "HOME"
	}
	tenantID, gcid := r.CtxIdentity(ctx)
	authCtx := withResolverAuthCtx(ctx, tenantID, gcid)
	v, err := r.Upstream.GetFeed(authCtx, tenantID, gcid)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return emptyFeed(), nil
	}
	postsRaw, _ := m["posts"].([]map[string]any)
	edges := make([]map[string]any, 0, len(postsRaw))
	for i, p := range postsRaw {
		edges = append(edges, map[string]any{
			"node": map[string]any{
				"id":         p["post_id"],
				"authorGcid": p["author_gcid"],
				"body":       p["text"],
				"mediaIds":   []any{},
				"visibility": "TENANT_ONLY",
				"reactionSummary": map[string]any{
					"like":       p["reactions"],
					"insightful": 0,
					"curious":    0,
					"cheer":      0,
					"celebrate":  0,
					"total":      p["reactions"],
					"myReaction": nil,
				},
				"replyCount": 0,
				"createdAt":  "2026-05-08T00:00:00Z",
			},
			"cursor": cursorAt(i),
		})
	}
	return map[string]any{
		"edges": edges,
		"pageInfo": map[string]any{
			"hasNextPage":     false,
			"hasPreviousPage": false,
			"startCursor":     cursorAt(0),
			"endCursor":       cursorAt(len(edges) - 1),
		},
		"totalCount": len(edges),
		"_scope":     scope,
		"_groupId":   groupID,
	}, nil
}

// MyProfile resolves `myCircleProfile`.
func (r *CircleResolver) MyProfile(ctx context.Context) (map[string]any, error) {
	_, gcid := r.CtxIdentity(ctx)
	if gcid == "" {
		gcid = "gcid-skel"
	}
	return map[string]any{
		"gcid":               gcid,
		"displayName":        "You",
		"avatarUri":          "/skins/avatars/default.svg",
		"bio":                "",
		"followerCount":      3,
		"followingCount":     5,
		"isFollowedByViewer": false,
		"isFollowingViewer":  false,
	}, nil
}

// Following resolves `following`.
func (r *CircleResolver) Following(_ context.Context) ([]any, error) {
	return []any{
		map[string]any{
			"gcid":               "gcid-other-1",
			"displayName":        "Mei",
			"followerCount":      12,
			"followingCount":     8,
			"isFollowedByViewer": true,
			"isFollowingViewer":  false,
		},
	}, nil
}

// Followers resolves `followers`.
func (r *CircleResolver) Followers(_ context.Context) ([]any, error) {
	return []any{
		map[string]any{
			"gcid":               "gcid-other-2",
			"displayName":        "Aria",
			"followerCount":      6,
			"followingCount":     11,
			"isFollowedByViewer": false,
			"isFollowingViewer":  true,
		},
	}, nil
}

func emptyFeed() map[string]any {
	return map[string]any{
		"edges":      []any{},
		"pageInfo":   map[string]any{"hasNextPage": false, "hasPreviousPage": false},
		"totalCount": 0,
	}
}

func cursorAt(i int) string {
	if i < 0 {
		return ""
	}
	return "cursor-" + intToStr(i)
}

func intToStr(i int) string {
	switch i {
	case 0:
		return "0"
	case 1:
		return "1"
	case 2:
		return "2"
	case 3:
		return "3"
	case 4:
		return "4"
	}
	return "n"
}
