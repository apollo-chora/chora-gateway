// json_whitebox_test.go — white-box branch coverage for the unexported
// json helpers in json.go (rawJSONOrNull).
package phyllis

import "testing"

func TestRawJSONOrNull_Branches(t *testing.T) {
	// Empty → nil.
	if got := rawJSONOrNull(nil); got != nil {
		t.Errorf("rawJSONOrNull(nil) = %v; want nil", got)
	}
	// Malformed → nil.
	if got := rawJSONOrNull([]byte(`nope`)); got != nil {
		t.Errorf("rawJSONOrNull(bad) = %v; want nil", got)
	}
	// Valid → decoded value.
	if got := rawJSONOrNull([]byte(`{"a":1}`)); got == nil {
		t.Error("rawJSONOrNull(valid) = nil; want value")
	}
}
