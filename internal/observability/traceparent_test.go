package observability_test

import (
	"strings"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/observability"
)

func TestEnsureTraceparent_GeneratesWhenMissing(t *testing.T) {
	got := observability.EnsureTraceparent("")
	if got == "" {
		t.Fatal("expected generated traceparent")
	}
	parts := strings.Split(got, "-")
	if len(parts) != 4 {
		t.Errorf("traceparent shape wrong: %q", got)
	}
	if len(parts[1]) != 32 || len(parts[2]) != 16 {
		t.Errorf("traceparent component lens wrong: %q", got)
	}
}

func TestEnsureTraceparent_PreservesValidInbound(t *testing.T) {
	in := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	got := observability.EnsureTraceparent(in)
	if got != in {
		t.Errorf("traceparent mutated: in=%q out=%q", in, got)
	}
}

func TestEnsureTraceparent_RegeneratesMalformed(t *testing.T) {
	cases := []string{
		"not-a-traceparent",
		"00-tooshort-shortspan-01",
		"00-00000000000000000000000000000000-b7ad6b7169203331-01", // all-zero trace_id
		"00-0af7651916cd43dd8448eb211c80319c-0000000000000000-01", // all-zero span_id
		"  ",
	}
	for _, c := range cases {
		got := observability.EnsureTraceparent(c)
		if got == c {
			t.Errorf("expected regeneration for malformed input %q", c)
		}
		// New value must be valid.
		if again := observability.EnsureTraceparent(got); again != got {
			t.Errorf("regenerated value not stable: %q -> %q", got, again)
		}
	}
}

func TestEnsureTraceparent_FreshValuesDiffer(t *testing.T) {
	a := observability.EnsureTraceparent("")
	b := observability.EnsureTraceparent("")
	if a == b {
		t.Errorf("expected distinct fresh traceparents, got %q twice", a)
	}
}
