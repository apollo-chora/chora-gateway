// error_envelope_correlation_internal_test.go
//
// Internal (package httpadapter) test: writeError is unexported and is the
// single writer behind ~25 GATEWAY_* error sites, so testing it here covers
// every one of them at once.
//
// Specification: tests/security/headers/headers_test.go
// TestErrorResponse_StructuredFormat, which asserts the envelope carries code,
// message AND correlation_id. Against the deployed gateway on 2026-08-07 the
// first two passed and correlation_id was absent, because nothing generated a
// correlation id at all.
package httpadapter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The correlation id in the body must be the SAME value as the header, not a
// second id minted here. gateway.yaml documents the field as "Correlation ID
// matching the X-Correlation-ID response header"; two different ids would be
// worse than none, because an operator would grep for the wrong one.
func TestWriteError_CarriesTheStampedCorrelationID(t *testing.T) {
	const cid = "01960000-0000-7000-8000-000000000099"
	w := httptest.NewRecorder()
	w.Header().Set("X-Correlation-ID", cid)

	writeError(w, http.StatusNotFound, "GATEWAY_ROUTE_NOT_FOUND", "route not found")

	var body struct {
		Error struct {
			Code          string `json:"code"`
			Message       string `json:"message"`
			CorrelationID string `json:"correlation_id"`
		} `json:"error"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body.Error.Code != "GATEWAY_ROUTE_NOT_FOUND" {
		t.Errorf("code = %q, want GATEWAY_ROUTE_NOT_FOUND", body.Error.Code)
	}
	if body.Error.Message != "route not found" {
		t.Errorf("message = %q, want route not found", body.Error.Message)
	}
	if body.Error.CorrelationID != cid {
		t.Errorf("correlation_id = %q, want the stamped header value %q", body.Error.CorrelationID, cid)
	}
}

// When no middleware stamped a header, the field is omitted rather than
// emitted empty. An empty string would look like a real id that failed to
// resolve; absence is the honest signal.
func TestWriteError_OmitsCorrelationIDWhenUnstamped(t *testing.T) {
	w := httptest.NewRecorder()

	writeError(w, http.StatusNotFound, "GATEWAY_ROUTE_NOT_FOUND", "route not found")

	var raw map[string]map[string]any
	if err := json.NewDecoder(w.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := raw["error"]["correlation_id"]; present {
		t.Errorf("correlation_id must be omitted when unstamped, got: %v", raw["error"])
	}
	if raw["error"]["code"] != "GATEWAY_ROUTE_NOT_FOUND" {
		t.Errorf("code = %v, want GATEWAY_ROUTE_NOT_FOUND", raw["error"]["code"])
	}
}

// The error path must not leak internals, which the same spec file asserts in
// TestErrorResponse_NoStackTrace.
func TestWriteError_NoInternalLeak(t *testing.T) {
	w := httptest.NewRecorder()
	writeError(w, http.StatusNotFound, "GATEWAY_ROUTE_NOT_FOUND", "route not found")

	body := w.Body.String()
	for _, forbidden := range []string{"goroutine", "/Users/", "/app/internal/", "panic"} {
		if len(body) > 0 && contains(body, forbidden) {
			t.Errorf("error body leaks %q: %s", forbidden, body)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}
