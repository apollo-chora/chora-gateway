package resolvers

import (
	"context"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

// GamificationResolver wires the gamification subschema — Wallet (XP /
// Coins / Reputation), Badges, Leaderboards.
type GamificationResolver struct {
	Upstream    upstream.Client
	CtxIdentity func(ctx context.Context) (string, string)
}

// NewGamificationResolver constructs the resolver.
func NewGamificationResolver(up upstream.Client, ctxID func(ctx context.Context) (string, string)) *GamificationResolver {
	if ctxID == nil {
		ctxID = func(ctx context.Context) (string, string) { return "", "" }
	}
	return &GamificationResolver{Upstream: up, CtxIdentity: ctxID}
}

// MyWallet resolves the `myWallet` query — Three-Currency Economy balance.
// In skeleton mode we synthesise a deterministic wallet keyed by the gcid;
// real implementation will read chora-sharing wallet.
func (r *GamificationResolver) MyWallet(ctx context.Context) (any, error) {
	_, gcid := r.CtxIdentity(ctx)
	if gcid == "" {
		gcid = "gcid-skel"
	}
	return map[string]any{
		"gcid":          gcid,
		"xp":            1240,
		"coins":         85,
		"reputation":    47,
		"lastUpdatedAt": "2026-05-08T00:00:00Z",
		"_stub":         true,
	}, nil
}

// MyBadges resolves the `myBadges` query.
func (r *GamificationResolver) MyBadges(ctx context.Context) ([]any, error) {
	_, gcid := r.CtxIdentity(ctx)
	if gcid == "" {
		gcid = "gcid-skel"
	}
	return []any{
		map[string]any{
			"id":          "badge-skel-001",
			"code":        "first-atom",
			"name":        "First Atom",
			"description": "Completed your first LearningAtom.",
			"rarity":      "COMMON",
			"earnedAt":    "2026-05-01T12:00:00Z",
			"iconUri":     "/skins/badges/first-atom.svg",
		},
		map[string]any{
			"id":          "badge-skel-002",
			"code":        "5-day-streak",
			"name":        "5-Day Streak",
			"description": "Studied for 5 consecutive days.",
			"rarity":      "RARE",
			"earnedAt":    "2026-05-06T08:00:00Z",
			"iconUri":     "/skins/badges/streak-5.svg",
		},
	}, nil
}

// Leaderboard resolves the `leaderboard(scope, scopeId, seasonId)` query.
func (r *GamificationResolver) Leaderboard(_ context.Context, scope, scopeID, seasonID string) (any, error) {
	if scope == "" {
		scope = "TENANT"
	}
	if seasonID == "" {
		seasonID = "season-current"
	}
	return map[string]any{
		"id":       "lb-skel-001",
		"scope":    scope,
		"scopeId":  scopeID,
		"seasonId": seasonID,
		"topEntries": []map[string]any{
			{"gcid": "gcid-other-1", "displayName": "Mei", "score": 4200, "rank": 1},
			{"gcid": "gcid-other-2", "displayName": "Aria", "score": 3850, "rank": 2},
			{"gcid": "gcid-skel", "displayName": "You", "score": 1240, "rank": 14},
		},
		"myEntry": map[string]any{
			"gcid":        "gcid-skel",
			"displayName": "You",
			"score":       1240,
			"rank":        14,
		},
		"updatedAt": "2026-05-08T00:00:00Z",
		"_stub":     true,
	}, nil
}
