// 06-auth — Local social login with the bundled "dev" OAuth provider.
//
// Run from the repo root:
//
//	go run ./examples/06-auth
//
// Then sign in by opening:
//
//	http://localhost:8086/api/v1/auth?action=login&using=dev
//
// The dev OAuth server (an in-process stand-in for github / google / …)
// listens on http://localhost:8084. Pick any name on the form to be
// signed in as that user. The browser is redirected back with a JWT
// cookie; calls to /api/v1/me will then succeed.
package main

import (
	"context"
	"database/sql"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"mkfst/auth/avatar"
	"mkfst/auth/token"
	"mkfst/config"
	"mkfst/fizz"
	"mkfst/fizz/openapi"
	authmw "mkfst/middleware/auth"
	"mkfst/router"
	"mkfst/service"
)

func main() {
	svc := service.Create(config.Config{
		Host:   "localhost",
		Port:   8086,
		SkipDB: true,
		Spec: openapi.Info{
			Title:       "Auth demo",
			Version:     "v1.0.0",
			Description: "Dev OAuth provider + JWT-protected /me endpoint.",
		},
	})

	authSvc := authmw.NewService(authmw.Opts{
		SecretReader: token.SecretFunc(func(string) (string, error) {
			return "do-not-use-this-secret-in-production", nil
		}),
		TokenDuration:  5 * time.Minute,
		CookieDuration: 24 * time.Hour,
		Issuer:         "mkfst-auth-demo",
		URL:            "http://localhost:8086",
		AvatarStore:    avatar.NewNoOp(),
		DisableXSRF:    true, // simplifies curl-based testing
	})

	// In-process dev OAuth server on :8084. NEVER enable in production.
	authSvc.AddDevProvider("localhost", 8084)

	// The dev server runs in its own goroutine, but we tie it to a
	// cancellable context and a SIGINT/SIGTERM handler so it shuts
	// down cleanly when you Ctrl+C this process — not "leaked" until
	// the OS reaps the binary.
	devCtx, cancelDev := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancelDev()

	var devWG sync.WaitGroup
	devWG.Add(1)
	go func() {
		defer devWG.Done()
		dev, err := authSvc.DevAuth()
		if err != nil {
			log.Fatal(err)
		}
		dev.Run(devCtx) // returns when devCtx is cancelled (Shutdown is called for us)
	}()

	authRoute, avatarRoute := authSvc.Handlers()
	mw := authSvc.Middleware()

	api := router.CreateGroup("/api/v1", "v1", "v1 API root")

	// Public auth endpoints. /auth multiplexes login/callback/user/status/logout
	// based on ?action=...
	api.Route("GET", "/auth", 200,
		[]fizz.OperationOption{
			fizz.Summary("Auth dispatch (login/logout/callback/user/status)"),
		},
		authRoute,
	)
	api.Route("GET", "/avatar", 200,
		[]fizz.OperationOption{fizz.Summary("Cached avatar passthrough")},
		avatarRoute,
	)

	// Protected sub-tree. mw.Auth rejects requests without a valid JWT.
	me := api.Group("/me", "Me", "Authenticated user routes")
	me.Middleware(mw.Auth)

	me.Route("GET", "/", 200,
		[]fizz.OperationOption{
			fizz.Summary("Return the authenticated user"),
		},
		func(ctx *gin.Context, _ *sql.DB) (token.User, error) {
			return token.GetUserInfo(ctx.Request)
		},
	)

	svc.AddGroup(*api)

	// service.Run blocks until ListenAndServe returns. When you Ctrl+C
	// the OS sends SIGINT to the whole process: signal.NotifyContext
	// fires, the dev server unwinds, this goroutine returns, and we
	// wait for it before exiting main.
	svc.Run()

	cancelDev()
	devWG.Wait()
}
