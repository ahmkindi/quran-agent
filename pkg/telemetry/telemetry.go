// Package telemetry sets up OpenTelemetry tracing. It is a no-op unless
// OTEL_EXPORTER_OTLP_ENDPOINT is set (e.g. "localhost:4317" for a local Jaeger),
// so the app runs with zero tracing infra by default. When enabled, spans are
// exported over OTLP/gRPC and trace context propagates across services.
package telemetry

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Init configures the global tracer provider for service. It returns a shutdown
// func (always non-nil) and whether tracing is enabled. When
// OTEL_EXPORTER_OTLP_ENDPOINT is unset, tracing is disabled and Tracer() returns
// a no-op tracer.
func Init(ctx context.Context, service string) (shutdown func(context.Context) error, enabled bool, err error) {
	noop := func(context.Context) error { return nil }

	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return noop, false, nil
	}

	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(), // local Jaeger/collector, plaintext
	)
	if err != nil {
		return noop, false, err
	}

	res, err := resource.New(ctx, resource.WithAttributes(
		attribute.String("service.name", service),
	))
	if err != nil {
		res = resource.Default()
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	return tp.Shutdown, true, nil
}

// Tracer returns a named tracer. Safe to call whether or not Init enabled
// tracing (returns a no-op tracer when disabled).
func Tracer(name string) trace.Tracer { return otel.Tracer(name) }
