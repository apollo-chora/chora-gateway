// trace_helpers_whitebox_test.go — white-box (package upstream) residual
// coverage for the unexported Cloud Trace deep-link helpers in
// observability_client.go (cloudTraceTraceURL / traceIDFromTraceparent /
// spanIDFromTraceparent) and urlHost in http_upstream.go.
package upstream

import "testing"

func TestCloudTraceTraceURL_Branches(t *testing.T) {
	// No trace UI base configured → "" (cloud Cloud Trace deep-links removed).
	t.Setenv("CHORA_TRACE_UI_URL", "")
	if got := cloudTraceTraceURL("trace", ""); got != "" {
		t.Errorf("no base = %q; want empty", got)
	}
	t.Setenv("CHORA_TRACE_UI_URL", "https://trace.chora.local/")
	// Empty trace id → "".
	if got := cloudTraceTraceURL("", ""); got != "" {
		t.Errorf("empty trace = %q", got)
	}
	// No span.
	got := cloudTraceTraceURL("trace-1", "")
	want := "https://trace.chora.local/trace/trace-1"
	if got != want {
		t.Errorf("no-span = %q; want %q", got, want)
	}
	// With span.
	got = cloudTraceTraceURL("trace-1", "span-9")
	want = "https://trace.chora.local/trace/trace-1?span=span-9"
	if got != want {
		t.Errorf("with-span = %q; want %q", got, want)
	}
}

func TestTraceIDFromTraceparent_Branches(t *testing.T) {
	traceID := "0af7651916cd43dd8448eb211c80319c"
	spanID := "b7ad6b7169203331"
	valid := "00-" + traceID + "-" + spanID + "-01"

	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{"00-a-b-c-01", ""}, // too few parts
		{"00-" + "short" + "-" + spanID + "-01", ""},                            // trace id not 32-hex
		{"00-" + "0af7651916cd43dd8448eb211c80319Z" + "-" + spanID + "-01", ""}, // non-hex
		{"00-" + "00000000000000000000000000000000" + "-" + spanID + "-01", ""}, // all-zero
		{valid, traceID},
	}
	for _, c := range cases {
		if got := traceIDFromTraceparent(c.in); got != c.want {
			t.Errorf("traceIDFromTraceparent(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestSpanIDFromTraceparent_Branches(t *testing.T) {
	traceID := "0af7651916cd43dd8448eb211c80319c"
	spanID := "b7ad6b7169203331"
	valid := "00-" + traceID + "-" + spanID + "-01"

	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"00-a-b-c", ""},                    // too few parts
		{"00-" + traceID + "-short-01", ""}, // span not 16-hex
		{"00-" + traceID + "-b7ad6b716920333Z-01", ""}, // non-hex
		{"00-" + traceID + "-0000000000000000-01", ""}, // all-zero
		{valid, spanID},
	}
	for _, c := range cases {
		if got := spanIDFromTraceparent(c.in); got != c.want {
			t.Errorf("spanIDFromTraceparent(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestURLHost_Branches(t *testing.T) {
	if got := urlHost(""); got != "" {
		t.Errorf("urlHost('') = %q; want empty", got)
	}
	if got := urlHost("http://svc.internal:8080/path"); got != "svc.internal:8080" {
		t.Errorf("urlHost(valid) = %q; want svc.internal:8080", got)
	}
	if got := urlHost("://bad"); got != "" {
		t.Errorf("urlHost(bad) = %q; want empty", got)
	}
}
func TestGovernanceReadError_Stringifier(t *testing.T) {
	e := &GovernanceReadError{StatusCode: 403, Body: "denied"}
	if got := e.Error(); got != "governance read: upstream 403" {
		t.Errorf("GovernanceReadError.Error() = %q", got)
	}
}
