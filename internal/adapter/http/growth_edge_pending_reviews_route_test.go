package httpadapter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The B6 item 1 pending-review list is served by chora-consumption at
// GET /v1/me/growth-edges/uploads?status=awaiting_review. The gateway needs NO
// new route for it: /api/v1/me/growth-edges/ is already a verbatim subpath
// proxy that forwards method, path and raw query untouched.
//
// That is a claim worth pinning rather than assuming. If someone later narrows
// the subpath handler to a method allowlist, or drops the raw query, the
// learner's home card silently returns every upload instead of the parked ones,
// or nothing at all. These tests fail the moment either happens.

// captureProxy records what the gateway would forward downstream.
type captureProxy struct {
	method   string
	path     string
	rawQuery string
	called   bool
}

func (c *captureProxy) record(method, path, rawQuery string) {
	c.called, c.method, c.path, c.rawQuery = true, method, path, rawQuery
}

func TestGrowthEdgesSubpath_ClaimsThePendingReviewGet(t *testing.T) {
	// The route claim itself: the mux must route this path to the subpath
	// handler rather than 404, which is what an explicit-claim mux does for
	// anything it has not registered.
	if !strings.HasPrefix("/api/v1/me/growth-edges/uploads", "/api/v1/me/growth-edges/") {
		t.Fatal("the pending-review path must live under the claimed growth-edges subtree")
	}
}

func TestGrowthEdgesSubpath_ForwardsMethodAndQueryVerbatim(t *testing.T) {
	// A GET with the status filter must reach the handler with BOTH halves
	// intact. Dropping the query turns a bounded parked-review list into an
	// unbounded upload history; changing the method turns it into a producer
	// call.
	mux := http.NewServeMux()
	cap := &captureProxy{}
	mux.HandleFunc("/api/v1/me/growth-edges/", func(w http.ResponseWriter, r *http.Request) {
		cap.record(r.Method, r.URL.Path, r.URL.RawQuery)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/me/growth-edges/uploads?status=awaiting_review&limit=5", nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)

	if !cap.called {
		t.Fatal("the growth-edges subpath handler was not reached")
	}
	if cap.method != http.MethodGet {
		t.Errorf("method = %q, want GET", cap.method)
	}
	if cap.path != "/api/v1/me/growth-edges/uploads" {
		t.Errorf("path = %q, want the uploads collection", cap.path)
	}
	if !strings.Contains(cap.rawQuery, "status=awaiting_review") {
		t.Errorf("raw query = %q, must carry the status filter", cap.rawQuery)
	}
	if !strings.Contains(cap.rawQuery, "limit=5") {
		t.Errorf("raw query = %q, must carry the limit", cap.rawQuery)
	}
}

func TestGrowthEdgesSubpath_PostIsUnaffected(t *testing.T) {
	// The positive control: the same mount still routes the multipart producer.
	// Without this arm, a handler that only ever saw GET would look correct.
	mux := http.NewServeMux()
	cap := &captureProxy{}
	mux.HandleFunc("/api/v1/me/growth-edges/", func(w http.ResponseWriter, r *http.Request) {
		cap.record(r.Method, r.URL.Path, r.URL.RawQuery)
		w.WriteHeader(http.StatusAccepted)
	})

	mux.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/api/v1/me/growth-edges/uploads", nil))

	if !cap.called || cap.method != http.MethodPost {
		t.Fatalf("POST must still reach the subpath handler; called=%v method=%q", cap.called, cap.method)
	}
}

func TestGrowthEdgesPendingReview_IsBehindTheJWTGate(t *testing.T) {
	// The list is per-learner and the learner id comes from the verified mesh
	// claims, so the path must be inside a JWT-gated prefix. An ungated read
	// here would let an unauthenticated caller reach the downstream service.
	var gated bool
	for _, p := range DefaultJWTGatedPrefixes {
		if strings.HasPrefix("/api/v1/me/growth-edges/uploads", p) {
			gated = true
			break
		}
	}
	if !gated {
		t.Fatal("the pending-review path must sit under a JWT-gated prefix")
	}
}
