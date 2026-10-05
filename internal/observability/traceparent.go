package observability

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// EnsureTraceparent returns a W3C traceparent header for the request. If the
// inbound value is missing or malformed, a fresh root traceparent is
// generated (the BFF is the trace ROOT for inbound client requests per Tier 3
// D13). The returned value is safe to forward to upstream calls.
func EnsureTraceparent(inbound string) string {
	t := strings.TrimSpace(inbound)
	if isValidTraceparent(t) {
		return t
	}
	return newTraceparent()
}

// isValidTraceparent does a structural W3C check: 4 dash-separated hex chunks
// of expected lengths.
func isValidTraceparent(s string) bool {
	parts := strings.Split(s, "-")
	if len(parts) != 4 {
		return false
	}
	expectedLens := []int{2, 32, 16, 2}
	for i, want := range expectedLens {
		if len(parts[i]) != want {
			return false
		}
		if _, err := hex.DecodeString(parts[i]); err != nil {
			return false
		}
	}
	// Reject all-zero trace_id / span_id (per W3C invalid).
	if strings.Trim(parts[1], "0") == "" {
		return false
	}
	if strings.Trim(parts[2], "0") == "" {
		return false
	}
	return true
}

// newTraceparent mints a fresh W3C traceparent using crypto/rand.
func newTraceparent() string {
	traceID := make([]byte, 16)
	spanID := make([]byte, 8)
	_, _ = rand.Read(traceID)
	_, _ = rand.Read(spanID)
	// Ensure non-zero trace_id and span_id (W3C invalidates all-zero).
	if isAllZero(traceID) {
		traceID[0] = 1
	}
	if isAllZero(spanID) {
		spanID[0] = 1
	}
	return fmt.Sprintf("00-%s-%s-01", hex.EncodeToString(traceID), hex.EncodeToString(spanID))
}

func isAllZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}
