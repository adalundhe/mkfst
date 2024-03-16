package telemetry

import (
	"context"
	"log"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type Context struct {
	provider     *sdktrace.TracerProvider
	UseTelemetry bool
}

type TracingConfig struct {
	traceExporter  sdktrace.SpanExporter
	metricExporter sdkmetric.Exporter
	options        []sdktrace.BatchSpanProcessorOption
}

func (otelctx *Context) Init(config *TracingConfig) {
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithBatcher(
			config.traceExporter,
			config.options...,
		),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	otelctx.provider = tp
}

func (otelctx *Context) Close() {
	if err := otelctx.provider.Shutdown(context.Background()); err != nil {
		log.Printf("Error shutting down tracer provider: %v", err)
	}
}
