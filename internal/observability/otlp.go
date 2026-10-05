// Package observability is a thin compatibility shim that delegates to the
// canonical libs/chora-go-common/otel package per Wave B (2026-05-14, tracker
// #146). The previous local implementation duplicated the
// otlptracegrpc + WithInsecure() / TLS+ADC branching pattern, which silently
// failed against telemetry.googleapis.com:443 (without the ADC-creds branch
// the gateway carried, but the broader fleet did) and otherwise duplicated
// wiring already encapsulated in the canonical lib.
//
// The canonical lib wires a real Cloud Trace exporter (ADC auth + TLS); this
// shim preserves the local Init(ctx), Tracer(), and ServiceName public API
// so call sites in cmd/server/main.go and internal/adapter/http/middleware.go
// keep compiling without edits. Spans now reach Cloud Trace correctly per
// Tier 3 D13.
//
// The traceparent helper (EnsureTraceparent) lives in traceparent.go and is
// unchanged — it's a pure W3C-traceparent string helper and was not part of
// Wave B's duplicate-exporter pattern.
package observability

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	commonotel "github.com/apollo-chora/chora-common/otel"
	commontracing "github.com/apollo-chora/chora-common/tracing"
)

// ServiceName is the canonical OTLP service.name attribute. Exported because
// the BFF stamps it on health-check JSON responses + log lines (see
// cmd/server/main.go).
const ServiceName = "chora-gateway"

// version is bumped per-release; emitted as the service.version resource attr.
const version = "0.1.0"

// Init wires the OTel exporter via the canonical lib and registers a global
// TracerProvider. Returns a shutdown func the caller MUST defer to flush
// spans on exit. Public signature preserved for backward compatibility.
func Init(ctx context.Context) (shutdown func(context.Context) error, err error) {
	return commonotel.Init(ctx, ServiceName, version)
}

// Tracer returns a Tracer scoped to this service — sugar for
// otel.Tracer(ServiceName). Preserved for callers that pulled the helper.
func Tracer() trace.Tracer { return otel.Tracer(ServiceName) }

// HTTPMiddleware starts an OTel server-side span for every incoming request
// + propagates W3C TraceContext from inbound headers. Delegates to the
// shared chora-go-common tracing.Middleware() (upgraded 2026-05-17 per
// FE-coord E2E-BE-AI-ASSIST-TRACE-EXPORT-PERM — gateway was previously
// silent on Cloud Trace because main.go never wrapped the handler with
// any OTel-emitting middleware).
//
// Mount as the OUTERMOST middleware in main.go so CORS preflights, auth
// gates, and routes all sit inside the request span.
func HTTPMiddleware() func(next http.Handler) http.Handler {
	return commontracing.Middleware()
}
