package opentel

import (
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"
)

type config struct {
	TracerProvider    oteltrace.TracerProvider
	Propagators       propagation.TextMapPropagator
	Filters           []Filter
	SpanNameFormatter SpanNameFormatter
}

// Filter is a predicate used to determine whether a given http.request should
// be traced. A Filter must return true if the request should be traced.
type Filter func(*http.Request) bool

// SpanNameFormatter is used to set span name by http.request.
type SpanNameFormatter func(r *http.Request) string

// Option specifies instrumentation configuration options.
type Option interface {
	apply(*config)
}

type optionFunc func(*config)

func (o optionFunc) apply(c *config) {
	o(c)
}

// WithPropagators specifies propagators to use for extracting
// information from the HTTP requests. If none are specified, global
// ones will be used.
func WithPropagators(propagators propagation.TextMapPropagator) Option {
	return optionFunc(func(cfg *config) {
		if propagators != nil {
			cfg.Propagators = propagators
		}
	})
}

// WithTracerProvider specifies a tracer provider to use for creating a tracer.
// If none is specified, the global provider is used.
func WithTracerProvider(provider oteltrace.TracerProvider) Option {
	return optionFunc(func(cfg *config) {
		if provider != nil {
			cfg.TracerProvider = provider
		}
	})
}

// WithFilter adds a filter to the list of filters used by the handler.
// If any filter indicates to exclude a request then the request will not be
// traced. All filters must allow a request to be traced for a Span to be created.
// If no filters are provided then all requests are traced.
// Filters will be invoked for each processed request, it is advised to make them
// simple and fast.
func WithFilter(f ...Filter) Option {
	return optionFunc(func(c *config) {
		c.Filters = append(c.Filters, f...)
	})
}

// WithSpanNameFormatter takes a function that will be called on every
// request and the returned string will become the Span Name.
func WithSpanNameFormatter(f func(r *http.Request) string) Option {
	return optionFunc(func(c *config) {
		c.SpanNameFormatter = f
	})
}

/////// Metrics -------

// Option applies a configuration to the given config
type MetricOption interface {
	applyMetric(cfg *MetricConfig)
}

type MetricOptionFunc func(cfg *MetricConfig)

func (fn MetricOptionFunc) applyMetric(cfg *MetricConfig) {
	fn(cfg)
}

// WithAttributes sets a func using which what attributes to be recorded can be specified.
// By default the DefaultAttributes is used
func WithAttributes(attributes func(serverName, route string, request *http.Request) []attribute.KeyValue) MetricOption {
	return MetricOptionFunc(func(cfg *MetricConfig) {
		cfg.attributes = attributes
	})
}

// WithRecordInFlight determines whether to record In Flight Requests or not
// By default the recordInFlight is true
func WithRecordInFlightDisabled() MetricOption {
	return MetricOptionFunc(func(cfg *MetricConfig) {
		cfg.recordInFlight = false
	})
}

// WithRecordDuration determines whether to record Duration of Requests or not
// By default the recordDuration is true
func WithRecordDurationDisabled() MetricOption {
	return MetricOptionFunc(func(cfg *MetricConfig) {
		cfg.recordDuration = false
	})
}

// WithRecordSize determines whether to record Size of Requests and Responses or not
// By default the recordSize is true
func WithRecordSizeDisabled() MetricOption {
	return MetricOptionFunc(func(cfg *MetricConfig) {
		cfg.recordSize = false
	})
}

// WithGroupedStatus determines whether to group the response status codes or not. If true 2xx, 3xx will be stored
// By default the groupedStatus is true
func WithGroupedStatusDisabled() MetricOption {
	return MetricOptionFunc(func(cfg *MetricConfig) {
		cfg.groupedStatus = false
	})
}

// WithRecorder sets a recorder for recording requests
// By default the open telemetry recorder is used
func WithRecorder(recorder MetricRecorder) MetricOption {
	return MetricOptionFunc(func(cfg *MetricConfig) {
		cfg.recorder = recorder
	})
}

// WithShouldRecordFunc sets a func using which whether a record should be recorded
// By default the all api calls are recorded
func WithShouldRecordFunc(shouldRecord func(serverName, route string, request *http.Request) bool) MetricOption {
	return MetricOptionFunc(func(cfg *MetricConfig) {
		cfg.shouldRecord = shouldRecord
	})
}
