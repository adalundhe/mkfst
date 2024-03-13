package telemetry

import (
	"context"
	"errors"
	"os"
	"os/signal"
)

type Context struct {
	ctx             context.Context
	stop            context.CancelFunc
	shutdownCtxCall func(context.Context) error
	errors          []error
}

func (otelctx *Context) Create() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)

	otelctx.ctx = ctx
	otelctx.stop = stop

	otelShutdown, err := SetupOpenTelemetrySDK(ctx)
	if err != nil {
		otelctx.errors = append(otelctx.errors, err)
		return
	}

	otelctx.shutdownCtxCall = otelShutdown
}

func (otelctx *Context) Shutdown() error {

	otelctx.stop()

	shutdownErr := otelctx.shutdownCtxCall(context.Background())
	otelctx.errors = append(otelctx.errors, shutdownErr)

	return errors.Join(otelctx.errors...)
}

func CreateContext() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Set up OpenTelemetry.
	otelShutdown, err := SetupOpenTelemetrySDK(ctx)
	if err != nil {
		return
	}

	defer func() {
		err = errors.Join(err, otelShutdown(context.Background()))
	}()
}
