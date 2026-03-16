// -------------------------------------------------------------------------------
// Tracing Tests
//
// Author: Alex Freidah
//
// Tests for span creation helpers. Verifies that StartSpan creates INTERNAL
// spans and StartClientSpan creates CLIENT spans for service graph visibility
// in Tempo.
// -------------------------------------------------------------------------------

package tracing

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// setupTestTracer installs an in-memory span exporter and returns it for
// inspection. Caller should defer the returned cleanup function.
func setupTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(tp)
	tracer = tp.Tracer(tracerName)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return exporter
}

// -------------------------------------------------------------------------
// START SPAN
// -------------------------------------------------------------------------

func TestStartSpan_CreatesInternalSpan(t *testing.T) {
	exporter := setupTestTracer(t)

	_, span := StartSpan(context.Background(), "test.internal",
		attribute.String("key", "value"),
	)
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}

	if spans[0].Name != "test.internal" {
		t.Errorf("span name = %q, want %q", spans[0].Name, "test.internal")
	}
	if spans[0].SpanKind != trace.SpanKindInternal {
		t.Errorf("span kind = %v, want %v", spans[0].SpanKind, trace.SpanKindInternal)
	}
}

// -------------------------------------------------------------------------
// START CLIENT SPAN
// -------------------------------------------------------------------------

func TestStartClientSpan_CreatesClientSpan(t *testing.T) {
	exporter := setupTestTracer(t)

	_, span := StartClientSpan(context.Background(), "test.client",
		PeerServiceAttr("downstream"),
		ServerAddressAttr("example.com"),
	)
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}

	if spans[0].Name != "test.client" {
		t.Errorf("span name = %q, want %q", spans[0].Name, "test.client")
	}
	if spans[0].SpanKind != trace.SpanKindClient {
		t.Errorf("span kind = %v, want %v (CLIENT)", spans[0].SpanKind, trace.SpanKindClient)
	}

	// Verify peer.service attribute is present
	found := false
	for _, attr := range spans[0].Attributes {
		if string(attr.Key) == "peer.service" && attr.Value.AsString() == "downstream" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected peer.service attribute on client span")
	}
}

func TestStartClientSpan_IsChildOfParent(t *testing.T) {
	exporter := setupTestTracer(t)

	ctx, parent := StartSpan(context.Background(), "parent.check_nodes")
	_, child := StartClientSpan(ctx, "consul.kv.get",
		PeerServiceAttr("consul"),
	)
	child.End()
	parent.End()

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}

	childSpan := spans[0]
	parentSpan := spans[1]

	if childSpan.Parent.SpanID() != parentSpan.SpanContext.SpanID() {
		t.Error("client span should be a child of the parent span")
	}
}

// -------------------------------------------------------------------------
// ATTRIBUTE HELPERS
// -------------------------------------------------------------------------

func TestPeerServiceAttr(t *testing.T) {
	attr := PeerServiceAttr("consul")
	if string(attr.Key) != "peer.service" {
		t.Errorf("key = %q, want %q", attr.Key, "peer.service")
	}
	if attr.Value.AsString() != "consul" {
		t.Errorf("value = %q, want %q", attr.Value.AsString(), "consul")
	}
}

func TestServerAddressAttr(t *testing.T) {
	attr := ServerAddressAttr("consul.service.consul:8500")
	if string(attr.Key) != "server.address" {
		t.Errorf("key = %q, want %q", attr.Key, "server.address")
	}
	if attr.Value.AsString() != "consul.service.consul:8500" {
		t.Errorf("value = %q, want %q", attr.Value.AsString(), "consul.service.consul:8500")
	}
}
