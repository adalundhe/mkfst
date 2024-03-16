package opentel

import (
	"database/sql"
	"fmt"
	"mkfst/middleware/opentel/semconvutil"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const (
	tracerKey = "otel-go-contrib-tracer"
	// ScopeName is the instrumentation scope name.
	ScopeName = "go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
)

// Middleware returns middleware that will trace incoming requests.
// The service parameter should describe the name of the (virtual)
// server handling the request.
func RequestTracing(service string, opts ...Option) interface{} {
	cfg := config{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	if cfg.TracerProvider == nil {
		cfg.TracerProvider = otel.GetTracerProvider()
	}
	tracer := cfg.TracerProvider.Tracer(
		ScopeName,
		oteltrace.WithInstrumentationVersion(Version()),
	)
	if cfg.Propagators == nil {
		cfg.Propagators = otel.GetTextMapPropagator()
	}
	return func(c *gin.Context, db *sql.DB) (any, error) {
		for _, f := range cfg.Filters {
			if !f(c.Request) {
				// Serve the request to the next middleware
				// if a filter rejects the request.
				c.Next()
				return nil, nil
			}
		}

		c.Set(tracerKey, tracer)
		savedCtx := c.Request.Context()
		defer func() {
			c.Request = c.Request.WithContext(savedCtx)
		}()

		ctx := cfg.Propagators.Extract(savedCtx, propagation.HeaderCarrier(c.Request.Header))
		opts := []oteltrace.SpanStartOption{
			oteltrace.WithAttributes(semconvutil.HTTPServerRequest(service, c.Request)...),
			oteltrace.WithSpanKind(oteltrace.SpanKindServer),
		}
		var spanName string
		if cfg.SpanNameFormatter == nil {
			spanName = c.FullPath()
		} else {
			spanName = cfg.SpanNameFormatter(c.Request)
		}
		if spanName == "" {
			spanName = fmt.Sprintf("HTTP %s route not found", c.Request.Method)
		} else {
			rAttr := semconv.HTTPRoute(spanName)
			opts = append(opts, oteltrace.WithAttributes(rAttr))
		}

		ctx, span := tracer.Start(ctx, spanName, opts...)
		defer span.End()

		// pass the span through the request context
		c.Request = c.Request.WithContext(ctx)

		status := c.Writer.Status()
		span.SetStatus(semconvutil.HTTPServerStatus(status))
		if status > 0 {
			span.SetAttributes(semconv.HTTPStatusCode(status))
		}
		if len(c.Errors) > 0 {
			span.SetAttributes(attribute.String("gin.errors", c.Errors.String()))
		}

		// serve the request to the next middleware
		c.Next()

		return nil, nil
	}
}

// HTML will trace the rendering of the template as a child of the
// span in the given context. This is a replacement for
// gin.Context.HTML function - it invokes the original function after
// setting up the span.
func HTML(c *gin.Context, code int, name string, obj interface{}) {
	var tracer oteltrace.Tracer
	tracerInterface, ok := c.Get(tracerKey)
	if ok {
		tracer, ok = tracerInterface.(oteltrace.Tracer)
	}
	if !ok {
		tracer = otel.GetTracerProvider().Tracer(
			ScopeName,
			oteltrace.WithInstrumentationVersion(Version()),
		)
	}
	savedContext := c.Request.Context()
	defer func() {
		c.Request = c.Request.WithContext(savedContext)
	}()
	opt := oteltrace.WithAttributes(attribute.String("go.template", name))
	_, span := tracer.Start(savedContext, "gin.renderer.html", opt)
	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("error rendering template:%s: %s", name, r)
			span.RecordError(err)
			span.SetStatus(codes.Error, "template failure")
			span.End()
			panic(r)
		}
		span.End()
	}()
	c.HTML(code, name, obj)
}

func RequestMetrics(service string, options ...MetricOption) interface{} {
	cfg := defaultConfig()
	for _, option := range options {
		option.applyMetric(cfg)
	}
	recorder := cfg.recorder
	if recorder == nil {
		recorder = GetRecorder("")
	}
	return func(c *gin.Context, db *sql.DB) (any, error) {

		ctx := c.Request.Context()

		route := c.FullPath()
		if len(route) <= 0 {
			route = "nonconfigured"
		}
		if !cfg.shouldRecord(service, route, c.Request) {
			c.Next()
			return nil, nil
		}

		start := time.Now()
		reqAttributes := cfg.attributes(service, route, c.Request)

		if cfg.recordInFlight {
			recorder.AddInflightRequests(ctx, 1, reqAttributes)
			defer recorder.AddInflightRequests(ctx, -1, reqAttributes)
		}

		defer func() {

			resAttributes := append(reqAttributes[0:0], reqAttributes...)

			if cfg.groupedStatus {
				code := c.Writer.Status()
				resAttributes = append(resAttributes, semconv.HTTPStatusCodeKey.Int(code))
			} else {
				resAttributes = append(resAttributes, semconvutil.HTTPAttributesFromHTTPStatusCode(c.Writer.Status())...)

			}

			recorder.AddRequests(ctx, 1, resAttributes)

			if cfg.recordSize {
				requestSize := computeApproximateRequestSize(c.Request)
				responseSize := int64(c.Writer.Size())
				recorder.ObserveHTTPRequestSize(ctx, requestSize, resAttributes)
				recorder.ObserveHTTPResponseSize(ctx, responseSize, resAttributes)
			}

			if cfg.recordDuration {
				recorder.ObserveHTTPRequestDuration(ctx, time.Since(start), resAttributes)
			}
		}()

		c.Next()

		return nil, nil
	}
}

func computeApproximateRequestSize(r *http.Request) int64 {
	s := 0
	if r.URL != nil {
		s = len(r.URL.Path)
	}

	s += len(r.Method)
	s += len(r.Proto)
	for name, values := range r.Header {
		s += len(name)
		for _, value := range values {
			s += len(value)
		}
	}
	s += len(r.Host)

	// N.B. r.Form and r.MultipartForm are assumed to be included in r.URL.

	if r.ContentLength != -1 {
		s += int(r.ContentLength)
	}
	return int64(s)
}
