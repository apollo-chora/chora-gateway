package httpadapter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

// newTranscriptSeenTestHandler builds the bridge handler against a stub
// chora-consumption.
func newTranscriptSeenTestHandler(t *testing.T, consumptionURL string) *GatewayProxyHandler {
	t.Helper()
	agg := gatewayproxy.New(gatewayproxy.Config{
		ConsumptionURL: consumptionURL,
		PerCallTimeout: time.Second,
	})
	if agg == nil {
		t.Fatal("aggregator nil: ConsumptionURL should enable the transcript routes")
	}
	return NewGatewayProxyHandler(agg)
}

// B6 item 2 shipped the mark-seen write on chora-consumption
// (POST /v1/me/transcript/{entryID}:seen) but NOT a gateway route for it.
// The gateway claimed /api/v1/me/transcript as an exact GET-only leaf with no
// subtree, so through api.chora.site the A+ card could READ the unseen flag
// and could never CLEAR it: the learner is told about a grade forever.
//
// That is the worst shape of a half-wired feature, because the read works.
// Nothing 500s and no log line fires; the flag simply never goes down, and it
// looks like a consumption bug rather than a missing route.
//
// These tests pin both halves of the claim (trap: TWO prefix lists, one gates
// and one routes, and a path in only one is either unauthenticated or
// unreachable) and pin that the two GET leaves are untouched by the change.

const (
	transcriptLeaf         = "/api/v1/me/transcript"
	transcriptSubtree      = "/api/v1/me/transcript/"
	transcriptByAssessLeaf = "/api/v1/transcript/by-assessments"
	markSeenPath           = "/api/v1/me/transcript/01923f8e-1c2d-7e3a-9b4c-5d6e7f801234:seen"
)

func TestTranscriptSeen_SubtreeIsClaimed(t *testing.T) {
	// The gateway is an explicit-claim mux with no catch-all under /api/v1/.
	// Without the trailing-slash subtree claim the write is a hard 404.
	var claimed bool
	for _, p := range GatewayProxyPathPrefixes {
		if p == transcriptSubtree {
			claimed = true
			break
		}
	}
	if !claimed {
		t.Fatalf("%s must be claimed by the gateway proxy mux", transcriptSubtree)
	}
}

func TestTranscriptSeen_SubtreeIsBehindTheJWTGate(t *testing.T) {
	// The mark is a WRITE scoped by the verified gcid. Ungated, an anonymous
	// caller reaches the downstream, and the downstream trusts the mesh
	// headers this gate is the only thing that stamps.
	var gated bool
	for _, p := range DefaultJWTGatedPrefixes {
		if strings.HasPrefix(markSeenPath, p) {
			gated = true
			break
		}
	}
	if !gated {
		t.Fatalf("%s must sit under a JWT-gated prefix", markSeenPath)
	}
}

func TestTranscriptSeen_PathIsOwnedByTheProxy(t *testing.T) {
	// matchesGatewayProxyPath is what decides the request goes to the bridge
	// rather than falling through to the Phyllis base router.
	if !matchesGatewayProxyPath(markSeenPath) {
		t.Fatalf("matchesGatewayProxyPath(%q) = false, want true", markSeenPath)
	}
}

func TestTranscriptSeen_GetLeavesAreUnchanged(t *testing.T) {
	// Adding a subtree must not disturb the two exact GET leaves that W6
	// shipped. Both must still be claimed in their own right: relying on the
	// subtree to cover the bare leaf would be wrong anyway, since
	// "/api/v1/me/transcript" does not have the trailing-slash prefix.
	for _, want := range []string{transcriptLeaf, transcriptByAssessLeaf} {
		var found bool
		for _, p := range GatewayProxyPathPrefixes {
			if p == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("exact leaf %q must remain claimed", want)
		}
		if !matchesGatewayProxyPath(want) {
			t.Errorf("matchesGatewayProxyPath(%q) = false, want true", want)
		}
	}
}

func TestTranscriptSeen_SubtreeDoesNotSwallowTheExactLeaf(t *testing.T) {
	// Go's ServeMux prefers the exact pattern over the subtree pattern, so
	// the read leaf keeps its own GET-only handler. If this ever regresses,
	// a GET of the transcript list would be dispatched as an entry action and
	// 404 on the missing ":seen" suffix.
	mux := http.NewServeMux()
	var hitLeaf, hitSubtree bool
	mux.HandleFunc(transcriptLeaf, func(w http.ResponseWriter, r *http.Request) {
		hitLeaf = true
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc(transcriptSubtree, func(w http.ResponseWriter, r *http.Request) {
		hitSubtree = true
		w.WriteHeader(http.StatusNoContent)
	})

	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, transcriptLeaf, nil))
	if !hitLeaf || hitSubtree {
		t.Fatalf("GET %s: leaf=%v subtree=%v, want leaf only", transcriptLeaf, hitLeaf, hitSubtree)
	}

	hitLeaf, hitSubtree = false, false
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, markSeenPath, nil))
	if !hitSubtree || hitLeaf {
		t.Fatalf("POST %s: leaf=%v subtree=%v, want subtree only", markSeenPath, hitLeaf, hitSubtree)
	}
}

func TestTranscriptSeen_ForwardsMethodAndPathVerbatim(t *testing.T) {
	// The downstream does the leaf dispatch: it 404s an unknown action and
	// 405s a non-POST. So the bridge must forward the method and the path
	// untouched. Rewriting either here would move that decision to the
	// gateway, where the ":seen" suffix rule is not written down.
	var gotMethod, gotPath string
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer down.Close()

	h := newTranscriptSeenTestHandler(t, down.URL)
	rec := httptest.NewRecorder()
	h.handleMeTranscriptEntryAction(rec, httptest.NewRequest(http.MethodPost, markSeenPath, nil))

	if gotMethod != http.MethodPost {
		t.Errorf("downstream method = %q, want POST", gotMethod)
	}
	if want := "/v1/me/transcript/01923f8e-1c2d-7e3a-9b4c-5d6e7f801234:seen"; gotPath != want {
		t.Errorf("downstream path = %q, want %q", gotPath, want)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
}

func TestTranscriptSeen_PassesAnUnknownActionToTheDownstream(t *testing.T) {
	// An unknown action must reach chora-consumption so the learner sees its
	// real 404, not a gateway guess. The gateway owning the WHOLE subtree is
	// the point: a bridge that filtered actions here would have to be kept in
	// step with the downstream's suffix rule forever.
	var gotPath string
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNotFound)
	}))
	defer down.Close()

	h := newTranscriptSeenTestHandler(t, down.URL)
	rec := httptest.NewRecorder()
	h.handleMeTranscriptEntryAction(rec,
		httptest.NewRequest(http.MethodPost, "/api/v1/me/transcript/abc:teardown", nil))

	if want := "/v1/me/transcript/abc:teardown"; gotPath != want {
		t.Errorf("downstream path = %q, want %q", gotPath, want)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want the downstream 404 passed through", rec.Code)
	}
}

func TestTranscriptSeen_ReachesTheBridgeNotTheBaseRouter(t *testing.T) {
	// The end-to-end claim. Before this route existed the POST fell through
	// to the Phyllis base router, which is what made the write unreachable
	// while the read kept working.
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer down.Close()

	agg := gatewayproxy.New(gatewayproxy.Config{
		ConsumptionURL: down.URL,
		PerCallTimeout: time.Second,
	})
	h := WithGatewayProxy(base, agg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, markSeenPath, nil))

	if rec.Code == http.StatusTeapot {
		t.Fatalf("POST %s fell through to the base router", markSeenPath)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 from the downstream", rec.Code)
	}
}
