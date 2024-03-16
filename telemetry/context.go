package telemetry

import (
	"context"
	"log"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.20.0"
)

type Context struct {
	provider     *sdktrace.TracerProvider
	UseTelemetry bool
}

type TracingConfig struct {
	TraceExporter  sdktrace.SpanExporter
	MetricExporter sdkmetric.Exporter
	Sampler        sdktrace.Sampler
	TraceOptions   []sdktrace.TracerProviderOption
	MetricOptions  []sdkmetric.PeriodicReaderOption
}

func Default() *TracingConfig {

	tracer, err := stdouttrace.New(
		stdouttrace.WithPrettyPrint(),
	)

	if err != nil {
		log.Fatal(err)
	}

	reporter, err := stdoutmetric.New()
	if err != nil {
		log.Fatal(err)
	}

	sampler := sdktrace.AlwaysSample()

	return &TracingConfig{
		TraceExporter:  tracer,
		MetricExporter: reporter,
		Sampler:        sampler,
		TraceOptions: []sdktrace.TracerProviderOption{
			sdktrace.WithSampler(sampler),
			sdktrace.WithBatcher(
				tracer,
			),
		},
		MetricOptions: []sdkmetric.PeriodicReaderOption{
			sdkmetric.WithInterval(1 * time.Second),
		},
	}
}

func (otelctx *Context) Init(config *TracingConfig) {
	tp := sdktrace.NewTracerProvider(
		config.TraceOptions...,
	)

	reader := sdkmetric.NewPeriodicReader(
		config.MetricExporter,
		config.MetricOptions...,
	)

	res, err := resource.New(context.Background(),
		resource.WithSchemaURL(semconv.SchemaURL),
	)
	if err != nil {
		panic(err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
	)

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	otelctx.provider = tp
}

func (otelctx *Context) Close() {
	if err := otelctx.provider.Shutdown(context.Background()); err != nil {
		log.Printf("Error shutting down tracer provider: %v", err)
	}
}
