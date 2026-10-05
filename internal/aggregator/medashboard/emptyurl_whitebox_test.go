// emptyurl_whitebox_test.go — white-box branch coverage for the unexported
// emptyURLError stringifier in medashboard.go.
package medashboard

import "testing"

func TestEmptyURLError_Stringifier(t *testing.T) {
	if got := errEmptyURL.Error(); got != "medashboard: downstream url not configured" {
		t.Errorf("errEmptyURL.Error() = %q", got)
	}
}
