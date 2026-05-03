package tasks

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Telemetry bundles the OTEL handles the worker uses to record
// per-task spans and metrics. All fields are optional — the worker
// no-ops cleanly when any are nil.
//
// Construct via NewTelemetry(meterProvider, tracerProvider) — the
// constructor wires up every metric we emit. Pass directly to
// WorkerOpts.Telemetry, or use NewTelemetryFromGlobals() to pick up
// otel.GetTracerProvider() / otel.GetMeterProvider() (mkfst's
// telemetry package sets globals at startup).
type Telemetry struct {
	tracer trace.Tracer

	enqueuedCounter   metric.Int64Counter
	claimedCounter    metric.Int64Counter
	completedCounter  metric.Int64Counter
	failedCounter     metric.Int64Counter
	retriedCounter    metric.Int64Counter
	cancelledCounter  metric.Int64Counter
	executeDuration   metric.Float64Histogram
	queueDepthGauge   metric.Int64ObservableGauge

	// queueDepthFn supplies live queue depths to the gauge callback.
	// Set internally by the worker via SetQueueDepthSource.
	queueDepthFn func() map[string]int
}

// NewTelemetry returns a Telemetry bound to the given OTEL providers.
// Either may be nil to skip that subsystem (tracing-only,
// metrics-only, or both nil = full no-op).
func NewTelemetry(mp metric.MeterProvider, tp trace.TracerProvider) (*Telemetry, error) {
	t := &Telemetry{}
	if tp != nil {
		t.tracer = tp.Tracer("mkfst/providers/tasks")
	}
	if mp != nil {
		meter := mp.Meter("mkfst/providers/tasks")
		var err error
		if t.enqueuedCounter, err = meter.Int64Counter("tasks.enqueued",
			metric.WithDescription("Tasks enqueued (any state).")); err != nil {
			return nil, fmt.Errorf("enqueued counter: %w", err)
		}
		if t.claimedCounter, err = meter.Int64Counter("tasks.claimed",
			metric.WithDescription("Tasks successfully claimed by a worker.")); err != nil {
			return nil, fmt.Errorf("claimed counter: %w", err)
		}
		if t.completedCounter, err = meter.Int64Counter("tasks.completed",
			metric.WithDescription("Tasks that finished successfully.")); err != nil {
			return nil, fmt.Errorf("completed counter: %w", err)
		}
		if t.failedCounter, err = meter.Int64Counter("tasks.failed",
			metric.WithDescription("Tasks that exhausted retries.")); err != nil {
			return nil, fmt.Errorf("failed counter: %w", err)
		}
		if t.retriedCounter, err = meter.Int64Counter("tasks.retried",
			metric.WithDescription("Tasks that were re-scheduled after a failure.")); err != nil {
			return nil, fmt.Errorf("retried counter: %w", err)
		}
		if t.cancelledCounter, err = meter.Int64Counter("tasks.cancelled",
			metric.WithDescription("Tasks cancelled before execution.")); err != nil {
			return nil, fmt.Errorf("cancelled counter: %w", err)
		}
		if t.executeDuration, err = meter.Float64Histogram("tasks.execute.duration",
			metric.WithDescription("Per-attempt handler runtime."),
			metric.WithUnit("s")); err != nil {
			return nil, fmt.Errorf("execute duration: %w", err)
		}
		if t.queueDepthGauge, err = meter.Int64ObservableGauge("tasks.queue.depth",
			metric.WithDescription("Pending tasks per queue."),
			metric.WithInt64Callback(func(_ context.Context, observer metric.Int64Observer) error {
				if t.queueDepthFn == nil {
					return nil
				}
				for queue, depth := range t.queueDepthFn() {
					observer.Observe(int64(depth), metric.WithAttributes(attribute.String("queue", queue)))
				}
				return nil
			})); err != nil {
			return nil, fmt.Errorf("queue depth gauge: %w", err)
		}
	}
	return t, nil
}

// NewTelemetryFromGlobals picks up the global OTEL providers
// (otel.GetMeterProvider / otel.GetTracerProvider). Convenient when
// mkfst's `telemetry` package has already configured the SDK at
// process startup.
func NewTelemetryFromGlobals() (*Telemetry, error) {
	return NewTelemetry(otel.GetMeterProvider(), otel.GetTracerProvider())
}

// SetQueueDepthSource registers a callback the metrics SDK invokes
// when scraping. The callback should be cheap (it runs on every
// scrape; default OTLP exporters scrape every 60s).
func (t *Telemetry) SetQueueDepthSource(fn func() map[string]int) {
	if t == nil {
		return
	}
	t.queueDepthFn = fn
}

// startSpan opens a span over the handler's execution. Span context
// is derived from the task's Tags["traceparent"] if present, so the
// span chains into whatever produced the enqueue (e.g. an HTTP
// request). The returned context has the span attached; callers pass
// it to the handler so any downstream spans hang off it.
func (t *Telemetry) startSpan(ctx context.Context, op string, task Task) (context.Context, trace.Span) {
	if t == nil || t.tracer == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	// Reconstruct upstream context from task tags so the trace
	// chains through enqueue → claim → execute even when those
	// happen in different processes.
	if len(task.Tags) > 0 {
		carrier := propagation.MapCarrier(task.Tags)
		ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)
	}
	ctx, span := t.tracer.Start(ctx, op,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("task.id", task.ID),
			attribute.String("task.type", task.Type),
			attribute.String("task.queue", task.Queue),
		),
	)
	return ctx, span
}

// InjectTraceContext embeds the current span's context into a Task's
// Tags so the trace propagates across the enqueue→claim handoff.
// Callers writing custom enqueue paths (instead of going through
// Scheduler) can use this to keep distributed traces intact.
func InjectTraceContext(ctx context.Context, task *Task) {
	if task == nil {
		return
	}
	if task.Tags == nil {
		task.Tags = make(map[string]string)
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(task.Tags))
}

// recordEnqueue/recordClaim/etc. wrap the per-event metric updates.
// All no-op when Telemetry or the specific counter is nil.

func (t *Telemetry) recordEnqueue(ctx context.Context, task Task) {
	if t == nil || t.enqueuedCounter == nil {
		return
	}
	t.enqueuedCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("queue", task.Queue),
		attribute.String("type", task.Type),
	))
}

func (t *Telemetry) recordClaim(ctx context.Context, task Task) {
	if t == nil || t.claimedCounter == nil {
		return
	}
	t.claimedCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("queue", task.Queue),
		attribute.String("type", task.Type),
	))
}

func (t *Telemetry) recordComplete(ctx context.Context, task Task, dur time.Duration) {
	if t == nil {
		return
	}
	if t.completedCounter != nil {
		t.completedCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("queue", task.Queue),
			attribute.String("type", task.Type),
		))
	}
	if t.executeDuration != nil {
		t.executeDuration.Record(ctx, dur.Seconds(), metric.WithAttributes(
			attribute.String("queue", task.Queue),
			attribute.String("type", task.Type),
			attribute.String("outcome", "success"),
		))
	}
}

func (t *Telemetry) recordFail(ctx context.Context, task Task, dur time.Duration, retried bool) {
	if t == nil {
		return
	}
	outcome := "failed"
	if retried {
		outcome = "retry"
		if t.retriedCounter != nil {
			t.retriedCounter.Add(ctx, 1, metric.WithAttributes(
				attribute.String("queue", task.Queue),
				attribute.String("type", task.Type),
			))
		}
	} else if t.failedCounter != nil {
		t.failedCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("queue", task.Queue),
			attribute.String("type", task.Type),
		))
	}
	if t.executeDuration != nil {
		t.executeDuration.Record(ctx, dur.Seconds(), metric.WithAttributes(
			attribute.String("queue", task.Queue),
			attribute.String("type", task.Type),
			attribute.String("outcome", outcome),
		))
	}
}

func (t *Telemetry) recordCancel(ctx context.Context, queue string) {
	if t == nil || t.cancelledCounter == nil {
		return
	}
	t.cancelledCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("queue", queue),
	))
}
