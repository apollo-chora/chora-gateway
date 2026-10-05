// correlation_id.go: X-Correlation-ID generation, propagation and logging.
//
// chora-contracts/openapi/gateway.yaml declares X-Correlation-ID on every
// response and documents CorrelationID as the first middleware in the chain.
// The gateway generated no such header: an authenticated OWASP ZAP scan and a
// manual probe of /healthz and a 401 path on 2026-08-07 both found it absent,
// and the error envelope's correlation_id field had nothing to carry.
//
// This is the trace-correlation sibling of the W3C traceparent the gateway
// already mints (internal/observability/traceparent.go). traceparent is for
// Cloud Trace; the correlation id is the human-facing handle a learner or an
// operator can quote from an error envelope, and the key that ties a support
// ticket to a log line.
package middleware

import (
	"context"
	"log"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// HeaderCorrelationID is the canonical header name, matching gateway.yaml.
const HeaderCorrelationID = "X-Correlation-ID"

// maxCorrelationIDLen bounds an inbound value. A correlation id is an
// identifier, not a payload; anything longer is either a mistake or an attempt
// to flood the logs.
const maxCorrelationIDLen = 128

// correlationIDCtxKey is the unexported context key. Unexported so no other
// package can write the value behind the middleware's back.
type correlationIDCtxKey struct{}

// healthProbePaths are polled continuously by the Kubernetes liveness and
// readiness probes. They still get a correlation id; they are not logged,
// because a per-probe access line buys nothing and costs log volume on a
// platform that is routinely cost-paused.
var healthProbePaths = map[string]struct{}{
	"/healthz":  {},
	"/healthz/": {},
	"/health":   {},
	"/readyz":   {},
	"/version":  {},
}

// CorrelationID wraps next so every request carries an X-Correlation-ID:
// propagated when the caller supplied a safe one, freshly minted as a UUIDv7
// otherwise.
//
// The id is stamped in three places, each load-bearing:
//
//   - the RESPONSE header, before next runs, so a 401 written by the auth gate
//     carries it as surely as a 200
//   - the REQUEST header, so the BFF's fan-out to chora-consumption and the
//     rest forwards it and one browser action is traceable across services
//   - the request CONTEXT, so a handler can put it in an error envelope via
//     CorrelationIDFromContext
//
// Apply it outside SecurityHeaders in cmd/server/main.go so the id exists
// before anything else can fail.
func CorrelationID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cid := resolveCorrelationID(r.Header.Get(HeaderCorrelationID))

		w.Header().Set(HeaderCorrelationID, cid)
		r.Header.Set(HeaderCorrelationID, cid)

		if _, isProbe := healthProbePaths[r.URL.Path]; !isProbe {
			// Same shape as the existing logging middleware in
			// internal/adapter/http/middleware.go, which only covers the
			// unbridged inner mux; this line covers every route.
			log.Printf("correlation_id=%s method=%s path=%s", cid, r.Method, r.URL.Path)
		}

		next.ServeHTTP(w, r.WithContext(
			context.WithValue(r.Context(), correlationIDCtxKey{}, cid),
		))
	})
}

// CorrelationIDFromContext returns the correlation id the middleware stamped,
// or empty when the context did not pass through it.
func CorrelationIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	cid, _ := ctx.Value(correlationIDCtxKey{}).(string)
	return cid
}

// resolveCorrelationID honours a safe inbound value and mints a fresh UUIDv7
// otherwise. Propagation is the point, so a caller's own identifier is kept
// even when it is not a UUID.
func resolveCorrelationID(inbound string) string {
	if v := strings.TrimSpace(inbound); isSafeCorrelationID(v) {
		return v
	}
	return newCorrelationID()
}

// isSafeCorrelationID reports whether an inbound value may be echoed back and
// written to the logs.
//
// The header is attacker-controlled. Go's net/http will not put a bare CRLF on
// the wire, so response splitting is not the risk; log injection is. A value
// restricted to printable ASCII with no whitespace cannot forge a second
// key=value log line, and the length bound stops a log flood.
func isSafeCorrelationID(v string) bool {
	if v == "" || len(v) > maxCorrelationIDLen {
		return false
	}
	for _, c := range []byte(v) {
		// Printable ASCII excluding space (0x21 to 0x7E).
		if c < 0x21 || c > 0x7E {
			return false
		}
	}
	return true
}

// newCorrelationID mints a UUIDv7, the repo-wide identifier convention: it is
// time-ordered, so ids sort by request arrival in a log search.
//
// uuid.NewV7 only fails when the system entropy source does, which would also
// break TLS and JWT signing. There is no v4 fallback worth having: uuid.New /
// uuid.NewString panic on that same entropy failure, so the old fallback was
// dead code wearing the costume of a graceful degradation.
func newCorrelationID() string {
	return uuid.Must(uuid.NewV7()).String()
}
