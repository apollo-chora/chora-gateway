// cursor_helpers_whitebox_test.go — white-box branch coverage for the
// unexported cursor helpers in circle.go (cursorAt / intToStr).
package resolvers

import "testing"

func TestCursorAt_Branches(t *testing.T) {
	if got := cursorAt(-1); got != "" {
		t.Errorf("cursorAt(-1) = %q; want empty", got)
	}
	if got := cursorAt(3); got != "cursor-3" {
		t.Errorf("cursorAt(3) = %q; want cursor-3", got)
	}
	if got := cursorAt(0); got != "cursor-0" {
		t.Errorf("cursorAt(0) = %q; want cursor-0", got)
	}
}

func TestIntToStr_Branches(t *testing.T) {
	for i, want := range map[int]string{0: "0", 1: "1", 2: "2", 3: "3", 4: "4", 5: "n", -1: "n"} {
		if got := intToStr(i); got != want {
			t.Errorf("intToStr(%d) = %q; want %q", i, got, want)
		}
	}
}
