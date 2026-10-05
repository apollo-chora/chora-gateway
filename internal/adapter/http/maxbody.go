// maxbody.go — defensive request-body size cap middleware.
//
// The BFF lives behind Cloud Armor + Cloud Service Mesh; those provide rate-
// limit + retry/timeout/circuit-break but neither caps an individual request
// body's byte size at the application layer. Cloud Run itself has a 32 MiB
// hard cap which is fine as a backstop, but per-route limits (mint a session,
// post a comment, etc.) deserve to fail at the gateway BEFORE bytes stream to
// upstream domain services.
//
// MaxRequestBodySize emits the canonical 413 envelope when Content-Length
// exceeds the cap, and additionally wraps the body in http.MaxBytesReader so
// clients that lie about Content-Length still hit a read error inside the
// downstream handler instead of being silently truncated.
//
// Origin: ported from the legacy top-level chora-gateway during the M12.2.G
// consolidation. Most other middleware from that codebase (Valkey rate
// limiter, circuit breaker, DLP regex inspection) was RETIRED in favour of
// platform-managed services — see services/chora-gateway/README.md.
package httpadapter

import (
	"fmt"
	"net/http"
)

// MaxRequestBodySize returns a middleware that rejects requests whose
// Content-Length exceeds maxBytes and wraps r.Body in http.MaxBytesReader so
// streaming reads above maxBytes also fail. A value of 0 or negative is
// treated as "no cap" and the middleware becomes a pass-through.
func MaxRequestBodySize(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if maxBytes <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > maxBytes {
				writeError(w, http.StatusRequestEntityTooLarge,
					"GATEWAY_PAYLOAD_TOO_LARGE",
					fmt.Sprintf("request body exceeds maximum size of %d bytes", maxBytes))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			next.ServeHTTP(w, r)
		})
	}
}
