# Telemetry

`mkfst/telemetry` bootstraps the OpenTelemetry SDK with a tracer provider,
a meter provider and a composite text-map propagator
(`TraceContext` + `Baggage`). Telemetry is **off by default**.

## Quickstart with the stdout exporters

`telemetry.Default()` returns a `TracingConfig` that prints traces and
metrics to stdout. Useful for local development:

```go
import "mkfst/telemetry"

svc := service.Create(...)
svc.ConfigureTracing(telemetry.Default())
// ...
svc.Run()
```

When the service shuts down, the deferred close in `service.Run` flushes
any in-flight spans.

## Custom exporters (OTLP / Jaeger / Zipkin)

`TracingConfig` accepts any `sdktrace.SpanExporter` and
`sdkmetric.Exporter`, so plugging in OTLP is a matter of importing the
exporter packages:

```go
import (
    "context"

    "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
    "go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
    sdkmetric "go.opentelemetry.io/otel/sdk/metric"
    sdktrace "go.opentelemetry.io/otel/sdk/trace"

    "mkfst/telemetry"
)

func tracing() *telemetry.TracingConfig {
    ctx := context.Background()

    traceExp, err := otlptracegrpc.New(ctx,
        otlptracegrpc.WithEndpoint("otel-collector:4317"),
        otlptracegrpc.WithInsecure())
    if err != nil { log.Fatal(err) }

    metricExp, err := otlpmetricgrpc.New(ctx,
        otlpmetricgrpc.WithEndpoint("otel-collector:4317"),
        otlpmetricgrpc.WithInsecure())
    if err != nil { log.Fatal(err) }

    sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.1))

    return &telemetry.TracingConfig{
        TraceExporter:  traceExp,
        MetricExporter: metricExp,
        Sampler:        sampler,
        TraceOptions: []sdktrace.TracerProviderOption{
            sdktrace.WithSampler(sampler),
            sdktrace.WithBatcher(traceExp),
        },
        MetricOptions: []sdkmetric.PeriodicReaderOption{
            sdkmetric.WithInterval(15 * time.Second),
        },
    }
}
```

```go
svc.ConfigureTracing(tracing())
```

## What gets traced

`telemetry.Context.Init` only installs the providers — it does not auto-
instrument anything. Two things you typically want to add:

### 1. The Gin middleware

mkfst ships an OpenTelemetry middleware in
[`middleware/opentel`](../middleware/opentel/) that creates a span for
each incoming request and propagates the trace context.

```go
import otelmw "mkfst/middleware/opentel"

engine := svc.Router.Base.Engine()
engine.Use(otelmw.Middleware("users-api"))
```

### 2. Database spans

Wrap your `*sql.DB` with [`otelsql`](https://github.com/XSAM/otelsql) (or
similar) **before** assigning it to the router:

```go
import (
    "database/sql"
    "github.com/XSAM/otelsql"
)

raw, _ := otelsql.Open("pgx", dsn,
    otelsql.WithAttributes(semconv.DBSystemPostgreSQL))

svc := service.Create(config.Config{Port: 8080, SkipDB: true})
svc.Router.Db.Conn = raw
```

(Use `SkipDB: true` so mkfst doesn't try to open its own connection, and
plug in the instrumented one yourself.)

## Custom spans inside a handler

Once `ConfigureTracing` has run, the global `otel.Tracer` is wired up:

```go
import "go.opentelemetry.io/otel"

func loadUser(ctx *gin.Context, db *sql.DB) (User, error) {
    spanCtx, span := otel.Tracer("users").Start(ctx.Request.Context(), "loadUser")
    defer span.End()

    var u User
    err := db.QueryRowContext(spanCtx, "SELECT id, name FROM users WHERE id=$1",
        ctx.Param("id")).Scan(&u.ID, &u.Name)
    if err != nil {
        span.RecordError(err)
        return User{}, err
    }
    return u, nil
}
```

## Resource attributes

`telemetry.Context.Init` builds an empty resource by default. To attach
service name / version / environment attributes, call into the SDK
yourself before `ConfigureTracing`:

```go
import (
    "go.opentelemetry.io/otel/sdk/resource"
    semconv "go.opentelemetry.io/otel/semconv/v1.20.0"
)

res, _ := resource.New(ctx,
    resource.WithSchemaURL(semconv.SchemaURL),
    resource.WithAttributes(
        semconv.ServiceName("users-api"),
        semconv.ServiceVersion("v1.2.3"),
        semconv.DeploymentEnvironment(os.Getenv("ENV")),
    ),
)
// then pass `sdktrace.WithResource(res)` in TracingConfig.TraceOptions
```

## Shutting down cleanly

`service.Run()` defers `telemetry.Context.Close()` when telemetry was
enabled. If you bypass `Run()` and run the engine directly, call
`Close()` yourself before exit so the batch exporter flushes.

## Working example

See [`../examples/07-telemetry`](../examples/07-telemetry) for a server
that runs with the stdout exporters and demonstrates a manual span inside
a handler.
