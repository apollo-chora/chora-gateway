package resolvers_test

import (
	"context"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
	"github.com/apollo-chora/chora-gateway/internal/graphql/resolvers"
)

func TestGamificationResolver_MyWallet_ReturnsThreeCurrencies(t *testing.T) {
	r := resolvers.NewGamificationResolver(upstream.NewFakeUpstream(), staticIdentity("t", "gcid-7"))
	w, err := r.MyWallet(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	m := w.(map[string]any)
	for _, key := range []string{"xp", "coins", "reputation"} {
		if _, ok := m[key]; !ok {
			t.Errorf("expected %s in wallet, missing", key)
		}
	}
	if m["gcid"] != "gcid-7" {
		t.Errorf("expected gcid-7, got %v", m["gcid"])
	}
}

func TestGamificationResolver_MyWallet_DefaultGcidWhenAbsent(t *testing.T) {
	r := resolvers.NewGamificationResolver(upstream.NewFakeUpstream(), staticIdentity("", ""))
	w, _ := r.MyWallet(context.Background())
	m := w.(map[string]any)
	if m["gcid"] != "gcid-skel" {
		t.Errorf("expected default gcid-skel, got %v", m["gcid"])
	}
}

func TestGamificationResolver_MyBadges_ReturnsBadgeList(t *testing.T) {
	r := resolvers.NewGamificationResolver(upstream.NewFakeUpstream(), staticIdentity("t", "g"))
	bs, err := r.MyBadges(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(bs) != 2 {
		t.Errorf("expected 2 badges, got %d", len(bs))
	}
	first := bs[0].(map[string]any)
	if first["rarity"] != "COMMON" {
		t.Errorf("expected COMMON rarity, got %v", first["rarity"])
	}
}

func TestGamificationResolver_Leaderboard_DefaultsScopeAndSeason(t *testing.T) {
	r := resolvers.NewGamificationResolver(upstream.NewFakeUpstream(), staticIdentity("t", "g"))
	lb, err := r.Leaderboard(context.Background(), "", "", "")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	m := lb.(map[string]any)
	if m["scope"] != "TENANT" {
		t.Errorf("expected default TENANT, got %v", m["scope"])
	}
	if m["seasonId"] != "season-current" {
		t.Errorf("expected default season-current, got %v", m["seasonId"])
	}
	entries := m["topEntries"].([]map[string]any)
	if len(entries) != 3 {
		t.Errorf("expected 3 top entries, got %d", len(entries))
	}
}

func TestGamificationResolver_Leaderboard_PinnedScopeId(t *testing.T) {
	r := resolvers.NewGamificationResolver(upstream.NewFakeUpstream(), staticIdentity("t", "g"))
	lb, err := r.Leaderboard(context.Background(), "COURSE", "course-42", "season-2")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	m := lb.(map[string]any)
	if m["scope"] != "COURSE" {
		t.Errorf("expected COURSE, got %v", m["scope"])
	}
	if m["scopeId"] != "course-42" {
		t.Errorf("expected course-42, got %v", m["scopeId"])
	}
}

func TestGamificationResolver_NewWithNilCtxIdentity(t *testing.T) {
	r := resolvers.NewGamificationResolver(upstream.NewFakeUpstream(), nil)
	if _, err := r.MyWallet(context.Background()); err != nil {
		t.Fatalf("nil ctxID should not error, got %v", err)
	}
}

func TestGamificationResolver_MyBadges_DefaultGcid(t *testing.T) {
	r := resolvers.NewGamificationResolver(upstream.NewFakeUpstream(), staticIdentity("", ""))
	bs, err := r.MyBadges(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(bs) != 2 {
		t.Errorf("expected 2 badges with default gcid, got %d", len(bs))
	}
}
