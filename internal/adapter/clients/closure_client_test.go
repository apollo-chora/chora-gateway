// closure_client_test.go — behaviour specs for the chora-closure-orchestrator
// HTTP client (CHO-1719, ADR-181 D5).
//
// Contract under test: chora-contracts/openapi/closure-orchestrator.yaml v2 —
// POST /v1/closure/request, POST /v1/closure/{closure_id}/cancel,
// GET /v1/closure/{closure_id}/status.
package clients_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
)

func newClosureServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *clients.ClosureClient) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := clients.NewClosureClient(clients.ClosureClientConfig{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewClosureClient: %v", err)
	}
	return srv, c
}

func TestNewClosureClient_RequiresBaseURL(t *testing.T) {
	if _, err := clients.NewClosureClient(clients.ClosureClientConfig{BaseURL: "  "}); err == nil {
		t.Fatal("want error on empty BaseURL, got nil")
	}
}

func TestNewClosureClient_TrimsTrailingSlash(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(clients.ClosureStatusResponse{SagaID: "s", GCID: "g", State: "closing"})
	}))
	t.Cleanup(srv.Close)
	c, err := clients.NewClosureClient(clients.ClosureClientConfig{BaseURL: srv.URL + "/"})
	if err != nil {
		t.Fatalf("NewClosureClient: %v", err)
	}
	if _, err := c.GetClosureStatus(context.Background(), "abc"); err != nil {
		t.Fatalf("GetClosureStatus: %v", err)
	}
	if gotPath != "/v1/closure/abc/status" {
		t.Fatalf("path = %q, want /v1/closure/abc/status", gotPath)
	}
}

// --- RequestClosure -----------------------------------------------------------

func TestRequestClosure_HappyPath(t *testing.T) {
	var gotBody map[string]any
	var gotRole string
	var gotMethod, gotPath string
	_, c := newClosureServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotRole = r.Header.Get("X-Chora-Role")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(clients.ClosureCloseResponse{
			SagaID:      "0197aaaa-0000-7000-8000-000000000001",
			State:       "closing",
			GraceEndsAt: "2026-07-12T00:00:00Z",
			RequestedAt: "2026-06-12T00:00:00Z",
		})
	})

	out, err := c.RequestClosure(context.Background(), clients.ClosureRequest{
		GCID:            "gcid-1",
		TenantID:        "tenant-1",
		GracePeriodDays: 30,
		Reason:          "leaving",
		RequestedByGCID: "gcid-1",
	}, "")
	if err != nil {
		t.Fatalf("RequestClosure: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/closure/request" {
		t.Fatalf("got %s %s, want POST /v1/closure/request", gotMethod, gotPath)
	}
	if gotRole != "" {
		t.Fatalf("X-Chora-Role should be ABSENT for non-operator calls, got %q", gotRole)
	}
	if gotBody["gcid"] != "gcid-1" || gotBody["tenant_id"] != "tenant-1" ||
		gotBody["requested_by_gcid"] != "gcid-1" || gotBody["grace_period_days"] != float64(30) {
		t.Fatalf("unexpected body: %#v", gotBody)
	}
	if _, present := gotBody["fast_close"]; present {
		t.Fatalf("fast_close must be omitted when false, body: %#v", gotBody)
	}
	if out.SagaID == "" || out.State != "closing" || out.GraceEndsAt == "" {
		t.Fatalf("unexpected response: %#v", out)
	}
}

func TestRequestClosure_FastCloseForwardsRoleHeader(t *testing.T) {
	var gotRole string
	var gotBody map[string]any
	_, c := newClosureServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotRole = r.Header.Get("X-Chora-Role")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(clients.ClosureCloseResponse{SagaID: "s", State: "closing"})
	})
	_, err := c.RequestClosure(context.Background(), clients.ClosureRequest{
		GCID: "g", TenantID: "t", GracePeriodDays: 30, RequestedByGCID: "op", FastClose: true,
	}, "PLATFORM_OPERATOR")
	if err != nil {
		t.Fatalf("RequestClosure: %v", err)
	}
	if gotRole != "PLATFORM_OPERATOR" {
		t.Fatalf("X-Chora-Role = %q, want PLATFORM_OPERATOR", gotRole)
	}
	if gotBody["fast_close"] != true {
		t.Fatalf("fast_close not forwarded, body: %#v", gotBody)
	}
}

func TestRequestClosure_4xxMapsToUpstreamError(t *testing.T) {
	_, c := newClosureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"code":    "CLOSURE_FAST_CLOSE_OPERATOR_REQUIRED",
			"message": "fast_close is restricted to PLATFORM_OPERATOR sessions",
		})
	})
	_, err := c.RequestClosure(context.Background(), clients.ClosureRequest{
		GCID: "g", TenantID: "t", GracePeriodDays: 30, RequestedByGCID: "g", FastClose: true,
	}, "")
	var ue *clients.ClosureUpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("want *ClosureUpstreamError, got %v", err)
	}
	if ue.StatusCode != http.StatusForbidden || ue.Code != "CLOSURE_FAST_CLOSE_OPERATOR_REQUIRED" {
		t.Fatalf("unexpected upstream error: %#v", ue)
	}
}

func TestRequestClosure_5xxMapsToUnavailable(t *testing.T) {
	_, c := newClosureServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	_, err := c.RequestClosure(context.Background(), clients.ClosureRequest{
		GCID: "g", TenantID: "t", GracePeriodDays: 30, RequestedByGCID: "g",
	}, "")
	if !errors.Is(err, clients.ErrClosureUpstreamUnavailable) {
		t.Fatalf("want ErrClosureUpstreamUnavailable, got %v", err)
	}
}

func TestRequestClosure_TransportErrorMapsToUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // dead socket
	c, err := clients.NewClosureClient(clients.ClosureClientConfig{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewClosureClient: %v", err)
	}
	_, err = c.RequestClosure(context.Background(), clients.ClosureRequest{
		GCID: "g", TenantID: "t", GracePeriodDays: 30, RequestedByGCID: "g",
	}, "")
	if !errors.Is(err, clients.ErrClosureUpstreamUnavailable) {
		t.Fatalf("want ErrClosureUpstreamUnavailable, got %v", err)
	}
}

// --- CancelClosure ------------------------------------------------------------

func TestCancelClosure_HappyPath(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	_, c := newClosureServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(clients.ClosureCancelResponse{
			SagaID: "saga-1", State: "active", CancelledAt: "2026-06-12T01:00:00Z",
		})
	})
	out, err := c.CancelClosure(context.Background(), "saga-1", clients.ClosureCancelRequest{
		ActorGCID: "gcid-1", Reason: "changed my mind",
	})
	if err != nil {
		t.Fatalf("CancelClosure: %v", err)
	}
	if gotPath != "/v1/closure/saga-1/cancel" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody["actor_gcid"] != "gcid-1" || gotBody["reason"] != "changed my mind" {
		t.Fatalf("unexpected body: %#v", gotBody)
	}
	if out.State != "active" || out.CancelledAt == "" {
		t.Fatalf("unexpected response: %#v", out)
	}
}

func TestCancelClosure_409TooLate(t *testing.T) {
	_, c := newClosureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"code": "CLOSURE_CANCEL_TOO_LATE", "message": "saga past grace",
		})
	})
	_, err := c.CancelClosure(context.Background(), "saga-1", clients.ClosureCancelRequest{ActorGCID: "g"})
	var ue *clients.ClosureUpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != http.StatusConflict || ue.Code != "CLOSURE_CANCEL_TOO_LATE" {
		t.Fatalf("want 409 CLOSURE_CANCEL_TOO_LATE upstream error, got %v", err)
	}
}

// FastAPI's HTTPException envelope is {"detail": "..."} — no code/message
// keys. The client must still surface a structured 4xx with the default
// code and the detail as message.
func TestCancelClosure_404FastAPIDetailEnvelope(t *testing.T) {
	_, c := newClosureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"saga not found"}`))
	})
	_, err := c.CancelClosure(context.Background(), "nope", clients.ClosureCancelRequest{ActorGCID: "g"})
	var ue *clients.ClosureUpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("want *ClosureUpstreamError, got %v", err)
	}
	if ue.StatusCode != http.StatusNotFound || ue.Code != "CLOSURE_UPSTREAM_REJECTED" {
		t.Fatalf("unexpected upstream error: %#v", ue)
	}
	if !strings.Contains(ue.Message, "saga not found") {
		t.Fatalf("detail not surfaced as message: %#v", ue)
	}
}

func TestCancelClosure_EscapesClosureID(t *testing.T) {
	var gotEscaped string
	_, c := newClosureServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotEscaped = r.URL.EscapedPath()
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(clients.ClosureCancelResponse{SagaID: "x", State: "active"})
	})
	if _, err := c.CancelClosure(context.Background(), "a/b", clients.ClosureCancelRequest{ActorGCID: "g"}); err != nil {
		t.Fatalf("CancelClosure: %v", err)
	}
	if gotEscaped != "/v1/closure/a%2Fb/cancel" {
		t.Fatalf("escaped path = %q, want /v1/closure/a%%2Fb/cancel", gotEscaped)
	}
}

// --- GetClosureStatus -----------------------------------------------------------

func TestGetClosureStatus_HappyPath(t *testing.T) {
	var gotMethod, gotPath string
	_, c := newClosureServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(clients.ClosureStatusResponse{
			SagaID:      "saga-1",
			GCID:        "gcid-1",
			TenantID:    "tenant-1",
			State:       "closing",
			GraceEndsAt: "2026-07-12T00:00:00Z",
			RequestedAt: "2026-06-12T00:00:00Z",
			History: []clients.ClosureHistoryEntry{
				{PriorState: "active", NewState: "closing", ActorGCID: "gcid-1", TransitionedAt: "2026-06-12T00:00:00Z"},
			},
			DomainAcks: []clients.ClosureDomainAck{{Domain: "creation", AckedAt: "2026-06-12T02:00:00Z"}},
		})
	})
	out, err := c.GetClosureStatus(context.Background(), "saga-1")
	if err != nil {
		t.Fatalf("GetClosureStatus: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/v1/closure/saga-1/status" {
		t.Fatalf("got %s %s", gotMethod, gotPath)
	}
	if out.GCID != "gcid-1" || out.State != "closing" || len(out.History) != 1 || len(out.DomainAcks) != 1 {
		t.Fatalf("unexpected response: %#v", out)
	}
}

func TestGetClosureStatus_404(t *testing.T) {
	_, c := newClosureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"saga not found"}`))
	})
	_, err := c.GetClosureStatus(context.Background(), "nope")
	var ue *clients.ClosureUpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 upstream error, got %v", err)
	}
}
