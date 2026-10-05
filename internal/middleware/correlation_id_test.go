// correlation_id_test.go: RED-first spec for X-Correlation-ID generation and
// propagation at the gateway edge.
//
// Specification: tests/security/headers/headers_test.go
// (TestCorrelationID_Generated + TestCorrelationID_Propagated) and the
// documented intent in chora-contracts/openapi/gateway.yaml, which declares
// the header "Always present in responses".
package middleware_test

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/apollo-chora/chora-gateway/internal/middleware"
)

func serveCorrelated(t *testing.T, handler http.Handler, path string, reqHeaders map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range reqHeaders {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	middleware.CorrelationID(handler).ServeHTTP(w, r)
	return w
}

func TestCorrelationID_GeneratedWhenAbsent(t *testing.T) {
	w := serveCorrelated(t, okHandler(), "/healthz", nil)

	cid := w.Header().Get(middleware.HeaderCorrelationID)
	if cid == "" {
		t.Fatal("missing X-Correlation-ID, the gateway must generate one")
	}
	parsed, err := uuid.Parse(cid)
	if err != nil {
		t.Fatalf("generated correlation id %q is not a UUID: %v", cid, err)
	}
	// New identifiers in this repo are UUIDv7.
	if parsed.Version() != 7 {
		t.Errorf("generated correlation id is UUIDv%d, want UUIDv7", parsed.Version())
	}
}

func TestCorrelationID_GeneratesDistinctIDs(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		w := serveCorrelated(t, okHandler(), "/healthz", nil)
		cid := w.Header().Get(middleware.HeaderCorrelationID)
		if seen[cid] {
			t.Fatalf("correlation id %q reused across requests", cid)
		}
		seen[cid] = true
	}
}

func TestCorrelationID_PropagatedWhenPresent(t *testing.T) {
	const inbound = "01960000-0000-7000-8000-000000000099"
	w := serveCorrelated(t, okHandler(), "/healthz", map[string]string{
		middleware.HeaderCorrelationID: inbound,
	})

	if got := w.Header().Get(middleware.HeaderCorrelationID); got != inbound {
		t.Errorf("X-Correlation-ID = %q, want the inbound %q", got, inbound)
	}
}

// The header is attacker-controlled. Go's net/http will not let a bare CRLF
// reach the wire, but an unbounded or control-character value still reaches
// the logs, so an unsafe inbound value is replaced rather than echoed.
func TestCorrelationID_RejectsUnsafeInbound(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"control character", "abc\x00def"},
		{"tab", "abc\tdef"},
		{"newline escape text", "abc\\ndef GET /admin"},
		{"space", "abc def"},
		{"over length", strings.Repeat("a", 200)},
		{"non ascii", "abcédef"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := serveCorrelated(t, okHandler(), "/healthz", map[string]string{
				middleware.HeaderCorrelationID: tc.value,
			})
			got := w.Header().Get(middleware.HeaderCorrelationID)
			if got == tc.value {
				t.Errorf("unsafe inbound correlation id %q was echoed verbatim", tc.value)
			}
			if got == "" {
				t.Error("a rejected inbound value must still yield a generated correlation id")
			}
			if _, err := uuid.Parse(got); err != nil {
				t.Errorf("replacement correlation id %q is not a UUID", got)
			}
		})
	}
}

// A caller-supplied id that is safe but not a UUID (another system's trace key)
// is honoured, because propagation is the point.
func TestCorrelationID_AcceptsSafeNonUUID(t *testing.T) {
	const inbound = "req-2026-08-07-abc123"
	w := serveCorrelated(t, okHandler(), "/healthz", map[string]string{
		middleware.HeaderCorrelationID: inbound,
	})
	if got := w.Header().Get(middleware.HeaderCorrelationID); got != inbound {
		t.Errorf("X-Correlation-ID = %q, want the inbound %q", got, inbound)
	}
}

func TestCorrelationID_ReachesTheHandlerContext(t *testing.T) {
	var fromCtx string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fromCtx = middleware.CorrelationIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	w := serveCorrelated(t, h, "/healthz", nil)

	if fromCtx == "" {
		t.Fatal("correlation id absent from the request context")
	}
	if got := w.Header().Get(middleware.HeaderCorrelationID); got != fromCtx {
		t.Errorf("context correlation id %q does not match response header %q", fromCtx, got)
	}
}

func TestCorrelationIDFromContext_EmptyWhenUnset(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	if got := middleware.CorrelationIDFromContext(r.Context()); got != "" {
		t.Errorf("CorrelationIDFromContext on a bare context = %q, want empty", got)
	}
}

// The id is worthless for incident triage if it never lands in Cloud Logging.
func TestCorrelationID_ReachesTheLogs(t *testing.T) {
	var buf bytes.Buffer
	origOut := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	})

	const inbound = "01960000-0000-7000-8000-000000000099"
	_ = serveCorrelated(t, okHandler(), "/api/v1/atoms", map[string]string{
		middleware.HeaderCorrelationID: inbound,
	})

	logged := buf.String()
	if !strings.Contains(logged, inbound) {
		t.Errorf("correlation id %q never reached the logs; got: %s", inbound, logged)
	}
	for _, want := range []string{"correlation_id=", "method=GET", "path=/api/v1/atoms"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log line missing %q; got: %s", want, logged)
		}
	}
}

// Kubernetes probes poll the health endpoints continuously. Logging every one
// buys nothing and costs log volume on a platform that is routinely cost-paused.
func TestCorrelationID_DoesNotLogHealthProbes(t *testing.T) {
	var buf bytes.Buffer
	origOut := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(origOut) })

	for _, p := range []string{"/healthz", "/healthz/", "/health", "/readyz"} {
		w := serveCorrelated(t, okHandler(), p, nil)
		// The header guarantee still applies to probes.
		if w.Header().Get(middleware.HeaderCorrelationID) == "" {
			t.Errorf("%s response missing X-Correlation-ID", p)
		}
	}
	if logged := buf.String(); strings.TrimSpace(logged) != "" {
		t.Errorf("health probes should not be logged; got: %s", logged)
	}
}

// The response header must be stamped before the downstream writes its status,
// otherwise a 401 carries no correlation id and the DAST finding stands.
func TestCorrelationID_PresentOn401(t *testing.T) {
	unauthorized := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	w := serveCorrelated(t, unauthorized, "/api/me", nil)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if w.Header().Get(middleware.HeaderCorrelationID) == "" {
		t.Error("401 response missing X-Correlation-ID")
	}
}

// The gateway forwards the id upstream so a single request is traceable across
// the fan-out, not just at the edge.
func TestCorrelationID_StampedOnTheInboundRequestHeader(t *testing.T) {
	var upstreamSaw string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamSaw = r.Header.Get(middleware.HeaderCorrelationID)
		w.WriteHeader(http.StatusOK)
	})
	w := serveCorrelated(t, h, "/api/v1/atoms", nil)

	if upstreamSaw == "" {
		t.Fatal("request header not stamped, and proxies forward r.Header, so upstream loses the id")
	}
	if got := w.Header().Get(middleware.HeaderCorrelationID); got != upstreamSaw {
		t.Errorf("request-side id %q differs from response-side %q", upstreamSaw, got)
	}
}
