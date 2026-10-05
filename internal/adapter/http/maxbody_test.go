// maxbody_test.go — TDD coverage for the MaxRequestBodySize middleware.
//
// Ported from the legacy top-level chora-gateway as the one piece of
// genuinely useful pre-proxy defense it carried that platform-managed services do
// NOT cover end-to-end:
//
//   - Cloud Armor edge rate-limit is request-rate-bound, not body-size-bound.
//   - Cloud Service Mesh retry/timeout/circuit-break is response-bound.
//   - Cloud Run has a 32 MiB request body cap, but per-route caps (e.g. 1 MiB
//     for /api/auth/session vs. 16 MiB for /api/media/uploads) are an
//     application-layer concern. Failing fast at the BFF avoids streaming
//     bogus bodies to upstream domain services.
//
// RED → GREEN → REFACTOR.
package httpadapter

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMaxRequestBodySize_PassThrough_BelowLimit(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		// Drain body so the downstream handler matches real flow.
		_, _ = io.Copy(io.Discard, r.Body)
	})
	h := MaxRequestBodySize(1 << 20)(next) // 1 MiB cap.

	body := strings.NewReader("small")
	req := httptest.NewRequest(http.MethodPost, "/x", body)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if !called {
		t.Fatalf("next handler should have been invoked when body is below cap")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
}

func TestMaxRequestBodySize_413_WhenContentLengthExceeds(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { called = true })
	h := MaxRequestBodySize(8)(next) // 8 bytes cap.

	body := bytes.NewReader([]byte("0123456789abcdef")) // 16 bytes.
	req := httptest.NewRequest(http.MethodPost, "/big", body)
	req.ContentLength = 16
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if called {
		t.Fatalf("next handler must NOT be invoked when Content-Length > cap")
	}
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: got %d, want 413", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "GATEWAY_PAYLOAD_TOO_LARGE") {
		t.Fatalf("expected canonical error code in body, got: %s", rr.Body.String())
	}
}

func TestMaxRequestBodySize_413_OnStreamingExceedance(t *testing.T) {
	// Simulate a client that lies about Content-Length (sends 0 but streams
	// bytes). The middleware MUST also enforce the cap during read via
	// http.MaxBytesReader so the downstream handler can't be tricked.
	innerErr := error(nil)
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, innerErr = io.ReadAll(r.Body)
	})
	h := MaxRequestBodySize(4)(next) // 4 bytes cap.

	body := strings.NewReader("0123456789") // 10 bytes.
	req := httptest.NewRequest(http.MethodPost, "/streaming", body)
	req.ContentLength = -1 // unknown length to bypass the fast-path 413.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if innerErr == nil {
		t.Fatalf("inner handler should see a read error once cap is exceeded")
	}
}
