// 07-telemetry — OpenTelemetry traces + metrics with the stdout exporters.
//
// Run from the repo root:
//
//	go run ./examples/07-telemetry
//
// Every request to /work creates a parent span "handler.work" and a
// child span "doWork" so the parent/child relationship is visible in
// the stdout exporter output.
package main

import (
	"context"
	"database/sql"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"mkfst/config"
	"mkfst/fizz"
	"mkfst/fizz/openapi"
	"mkfst/service"
	"mkfst/telemetry"
)

func doWork(parentCtx context.Context) string {
	tracer := otel.Tracer("07-telemetry")
	_, span := tracer.Start(parentCtx, "doWork")
	defer span.End()

	span.SetAttributes(attribute.Int("work.units", 3))
	time.Sleep(40 * time.Millisecond) // pretend to do something
	return "done"
}

func main() {
	svc := service.Create(config.Config{
		Host:   "localhost",
		Port:   8087,
		SkipDB: true,
		Spec: openapi.Info{
			Title:       "Telemetry demo",
			Version:     "v1.0.0",
			Description: "OTel SDK with stdout exporters.",
		},
	})

	// Default() uses the stdouttrace + stdoutmetric exporters.
	svc.ConfigureTracing(telemetry.Default())

	svc.Route("GET", "/work", 200,
		[]fizz.OperationOption{
			fizz.Summary("Do some traced work"),
		},
		func(ctx *gin.Context, _ *sql.DB) (gin.H, error) {
			tracer := otel.Tracer("07-telemetry")
			reqCtx, span := tracer.Start(ctx.Request.Context(), "handler.work")
			defer span.End()

			result := doWork(reqCtx)
			span.SetAttributes(attribute.String("work.result", result))

			return gin.H{
				"result":   result,
				"trace_id": span.SpanContext().TraceID().String(),
			}, nil
		},
	)

	svc.Run()
}
