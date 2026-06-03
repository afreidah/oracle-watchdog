// -------------------------------------------------------------------------------
// Oracle Watchdog - Tracing
//
// Author: Alex Freidah
//
// OpenTelemetry tracing infrastructure for shipping traces to Tempo. Provides
// initialization and span helpers for the restart cycle and node monitoring.
// -------------------------------------------------------------------------------

package tracing

import (
	"context"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	serviceName = "oracle-watchdog"
	tracerName  = "github.com/afreidah/oracle-watchdog"

	// defaultOTLPEndpoint is a bare host:port with no scheme: otlptracehttp's
	// WithEndpoint rejects a scheme (WithEndpointURL is the URL form), and
	// WithInsecure already selects plain HTTP. 4318 is the OTLP/HTTP port (4317
	// is gRPC and would silently drop every export). Operators point this at a
	// real collector via the tracing.endpoint config or OTEL_EXPORTER_OTLP_ENDPOINT.
	defaultOTLPEndpoint = "localhost:4318"
)

// Version of the service for trace metadata. Set at build time via
// -ldflags "-X github.com/afreidah/oracle-watchdog/internal/tracing.Version=..."
var Version = "dev"

var tracer trace.Tracer

// Init initializes the OpenTelemetry tracer with the OTLP/HTTP exporter and
// returns a shutdown function that should be deferred. The endpoint resolves
// in precedence order: the endpoint argument (from config), then the
// OTEL_EXPORTER_OTLP_ENDPOINT env var, then defaultOTLPEndpoint. All forms are
// a bare host:port with no scheme.
func Init(ctx context.Context, mode, endpoint string) (func(context.Context) error, error) {
	endpoint = resolveEndpoint(endpoint)

	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	// The custom resource is schemaless: resource.Default() carries the SDK's
	// own schema URL, and Merge errors if a second non-empty schema URL differs
	// from it. Pinning semconv.SchemaURL here would conflict whenever the SDK
	// outpaces the imported semconv version, so omit it and let Merge adopt the
	// default's schema.
	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(Version),
			attribute.String("mode", mode),
		),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	tracer = tp.Tracer(tracerName)

	slog.Info("tracing initialized", "endpoint", endpoint, "mode", mode)

	return tp.Shutdown, nil
}

// resolveEndpoint applies the endpoint precedence: the explicit argument (from
// config) wins, then OTEL_EXPORTER_OTLP_ENDPOINT, then defaultOTLPEndpoint. The
// result is always a bare host:port with no scheme, as otlptracehttp expects.
func resolveEndpoint(endpoint string) string {
	if endpoint == "" {
		endpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if endpoint == "" {
		endpoint = defaultOTLPEndpoint
	}
	return endpoint
}

// Tracer returns the global tracer instance.
func Tracer() trace.Tracer {
	if tracer == nil {
		return otel.Tracer(tracerName)
	}
	return tracer
}

// -------------------------------------------------------------------------
// SPAN HELPERS
// -------------------------------------------------------------------------

// StartSpan starts a new span with the given name and returns context + span.
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return Tracer().Start(ctx, name, trace.WithAttributes(attrs...))
}

// StartClientSpan creates a span with SpanKindClient for outbound service calls.
// Client spans are required for Tempo's service graph to detect service-to-service edges.
func StartClientSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return Tracer().Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)
}

// PeerServiceAttr creates a peer.service attribute for service graph edges.
func PeerServiceAttr(name string) attribute.KeyValue {
	return attribute.String("peer.service", name)
}

// ServerAddressAttr creates a server.address attribute.
func ServerAddressAttr(addr string) attribute.KeyValue {
	return attribute.String("server.address", addr)
}

// NodeAttr creates a node name attribute.
func NodeAttr(name string) attribute.KeyValue {
	return attribute.String("node.name", name)
}

// InstanceAttr creates an instance ID attribute.
func InstanceAttr(id string) attribute.KeyValue {
	return attribute.String("oci.instance_id", id)
}

// StateAttr creates a lifecycle state attribute.
func StateAttr(state string) attribute.KeyValue {
	return attribute.String("oci.lifecycle_state", state)
}

// StatusAttr creates a status attribute (alive, missing, restarting).
func StatusAttr(status string) attribute.KeyValue {
	return attribute.String("node.status", status)
}

// ErrorAttr creates an error attribute.
func ErrorAttr(err error) attribute.KeyValue {
	return attribute.String("error", err.Error())
}

// DurationAttr creates a duration attribute in seconds.
func DurationAttr(name string, seconds float64) attribute.KeyValue {
	return attribute.Float64(name, seconds)
}
