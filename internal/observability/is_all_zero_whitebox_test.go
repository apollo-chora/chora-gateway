// is_all_zero_whitebox_test.go — branch coverage for the unexported
// isAllZero helper in traceparent.go.
package observability

import "testing"

func TestIsAllZero_Branches(t *testing.T) {
	if !isAllZero([]byte{0, 0, 0}) {
		t.Error("isAllZero(all zeros) = false; want true")
	}
	if isAllZero([]byte{0, 1, 0}) {
		t.Error("isAllZero(mixed) = true; want false")
	}
	// Empty slice: the loop never trips, so it reports true (defensive
	// caller treats "no bytes" as "nothing non-zero").
	if !isAllZero(nil) {
		t.Error("isAllZero(nil) = false; want true (empty loop)")
	}
}
