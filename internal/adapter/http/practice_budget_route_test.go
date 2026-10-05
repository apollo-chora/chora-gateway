package httpadapter

import (
	"strings"
	"testing"
)

// The B6 item 3 read is served by chora-consumption at
// GET /v1/me/practice-budget. Unlike the item 1 pending-review list, this one
// is NOT under an existing claimed subtree, so the gateway needs its own route.
// These tests pin the two properties that make it safe.

func TestPracticeBudgetRoute_IsClaimed(t *testing.T) {
	// The gateway is an explicit-claim mux with no catch-all under /api/v1/,
	// so an unclaimed path is a hard 404 and the card would never load.
	var claimed bool
	for _, p := range GatewayProxyPathPrefixes {
		if p == "/api/v1/me/practice-budget" {
			claimed = true
			break
		}
	}
	if !claimed {
		t.Fatal("/api/v1/me/practice-budget must be claimed by the gateway proxy mux")
	}
}

func TestPracticeBudgetRoute_IsBehindTheJWTGate(t *testing.T) {
	// The budget is per-learner and the learner comes from the verified mesh
	// claims. An ungated read would let an unauthenticated caller reach the
	// downstream service.
	var gated bool
	for _, p := range DefaultJWTGatedPrefixes {
		if strings.HasPrefix("/api/v1/me/practice-budget", p) {
			gated = true
			break
		}
	}
	if !gated {
		t.Fatal("the practice-budget path must sit under a JWT-gated prefix")
	}
}
