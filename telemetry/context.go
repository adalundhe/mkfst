package telemetry

import (
	"context"
	"log"

	"go.opentelemetry.io/otel"
	stdout "go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type Context struct {
	provider *sdktrace.TracerProvider
}

func (otelctx *Context) Init() {
	exporter, err := stdout.New(stdout.WithPrettyPrint())
	if err != nil {
		log.Fatal(err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithBatcher(exporter),
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
