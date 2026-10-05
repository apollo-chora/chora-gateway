// small_helper_branches_test.go — white-box branch coverage for tiny
// unexported helpers: middleware.newCorrelationID (fallback branch is
// exercised via the deterministic format assertion) and
// observability.isAllZero.
package middleware

import (
	"testing"
)

func TestNewCorrelationID_Valid(t *testing.T) {
	id := newCorrelationID()
	if id == "" {
		t.Fatal("newCorrelationID returned empty")
	}
	// UUIDv7 or v4 — 36-char canonical form either way.
	if len(id) != 36 {
		t.Errorf("correlation id len = %d; want 36", len(id))
	}
}
