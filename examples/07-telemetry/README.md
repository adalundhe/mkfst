# 07 — Telemetry

Wires up the OpenTelemetry SDK using mkfst's `telemetry.Default()`,
which prints traces and metrics to stdout. The handler creates a parent
span and the helper creates a child span so the relationship is clearly
visible in the exporter output.

```bash
go run ./examples/07-telemetry
```

| Method | Path    | What it does                                                    |
| ------ | ------- | --------------------------------------------------------------- |
| GET    | `/work` | Sleeps 40 ms inside two nested spans, returns the trace ID.     |

## Try it

```bash
curl -s http://localhost:8087/work | jq
# {
#   "result": "done",
#   "trace_id": "..."
# }
```

The server log shows pretty-printed spans like:

```json
{
  "Name": "doWork",
  "SpanContext": { ... "TraceID": "...", "SpanID": "..." },
  "Parent": { "TraceID": "...", "SpanID": "..." },
  "Attributes": [{"Key": "work.units", "Value": {"Type": "INT64", "Value": 3}}]
}
{
  "Name": "handler.work",
  "Parent": { ... },
  ...
}
```

## What this example shows

- `service.ConfigureTracing(telemetry.Default())` installs the global
  tracer and meter providers using stdout exporters.
- `otel.Tracer(...)` is available everywhere afterwards.
- A child span is created by passing the request context (already
  decorated by the parent span) to `tracer.Start`.

To use a real backend, swap `telemetry.Default()` for a `TracingConfig`
that wraps an OTLP exporter (`otlptracegrpc`, `otlpmetricgrpc`). See
[docs/telemetry.md](../../docs/telemetry.md) for the complete recipe.
