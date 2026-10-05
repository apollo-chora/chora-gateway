package observability_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/apollo-chora/chora-gateway/internal/observability"
)

func TestInit_DevFallback_StdoutExporter(t *testing.T) {
	// Ensure we're in dev mode (no OTLP endpoint, no ADC project).
	// The canonical lib's dev detection requires BOTH env vars empty.
	t.Setenv("OTEL_EXPORTER", "stdout")
	old := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	_ = os.Unsetenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	defer func() {
		if old != "" {
			_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", old)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	shutdown, err := observability.Init(ctx)
	if err != nil {
		t.Fatalf("Init(dev): %v", err)
	}
	if shutdown == nil {
		t.Fatal("expected non-nil shutdown")
	}
	if err := shutdown(ctx); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

func TestTracer_NotNil(t *testing.T) {
	if observability.Tracer() == nil {
		t.Error("Tracer returned nil")
	}
}

func TestServiceName_Constant(t *testing.T) {
	if observability.ServiceName != "chora-gateway" {
		t.Errorf("expected service.name=chora-gateway, got %s", observability.ServiceName)
	}
}

// TestHTTPMiddleware_EmitsSpanForGatewayRequest regression-guards the
// 2026-05-17 fix (commit aa148c56) that wires
// observability.HTTPMiddleware()(corsHandler) into chora-gateway main.go.
// The middleware MUST start a real OTel server-side span per request —
// without it, chora-gateway emits zero spans to Cloud Trace and the
// FE-side traceparent is broken at the gateway BFF (the entry point of
// every browser-originated request chain). Closes Debt 8 from FE-coord
// E2E-BE-AI-ASSIST-TRACE-EXPORT-PERM audit.
func TestHTTPMiddleware_EmitsSpanForGatewayRequest(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	mw := observability.HTTPMiddleware()
	if mw == nil {
		t.Fatal("observability.HTTPMiddleware() returned nil")
	}

	called := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/mana", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if !called {
		t.Fatal("inner handler not invoked")
	}
	spans := rec.Ended()
	if len(spans) == 0 {
		t.Fatal("expected a server-side span; HTTPMiddleware likely degraded to no-op")
	}
	if got := spans[0].Name(); got != "GET /api/v1/me/mana" {
		t.Errorf("span name=%q want \"GET /api/v1/me/mana\"", got)
	}
}
