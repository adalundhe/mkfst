// 02-routing — Groups, nested groups and per-group middleware.
//
// Run from the repo root:
//
//	go run ./examples/02-routing
//
// Demonstrates:
//   - Building a group outside main with router.CreateGroup.
//   - Nesting groups so paths concatenate.
//   - Attaching middleware at the router, group and nested-group levels
//     and observing the order they fire in.
package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"mkfst/config"
	"mkfst/fizz"
	"mkfst/fizz/openapi"
	"mkfst/router"
	"mkfst/service"
)

// requestLogger is a tonic-shaped middleware that logs every request that
// reaches it. We register it three times, with different prefixes, to
// make the call order observable.
func requestLogger(prefix string) func(*gin.Context, *sql.DB) (any, error) {
	return func(ctx *gin.Context, _ *sql.DB) (any, error) {
		log.Printf("[%s] %s %s", prefix, ctx.Request.Method, ctx.Request.URL.Path)
		return nil, nil
	}
}

// requireAdmin shows how to short-circuit a request from a middleware.
// It rejects every request that does not carry an `X-Admin: true` header.
func requireAdmin(ctx *gin.Context, _ *sql.DB) (any, error) {
	if ctx.GetHeader("X-Admin") != "true" {
		ctx.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error": "admin only",
		})
		return nil, nil
	}
	return nil, nil
}

func usersGroup() router.Group {
	g := router.CreateGroup(
		"/users", "Users",
		"User management endpoints.",
	)

	// Group-level middleware runs after the router-level middleware.
	g.Middleware(requestLogger("users"))

	g.Route("GET", "/", 200,
		[]fizz.OperationOption{fizz.Summary("List users")},
		func(ctx *gin.Context, _ *sql.DB) ([]string, error) {
			return []string{"alice", "bob"}, nil
		},
	)

	g.Route("GET", "/:name", 200,
		[]fizz.OperationOption{fizz.Summary("Get a user by name")},
		func(ctx *gin.Context, _ *sql.DB, in *struct {
			Name string `path:"name" validate:"required,min=1"`
		}) (string, error) {
			return fmt.Sprintf("user=%s", in.Name), nil
		},
	)

	// A nested group inherits the parent's middleware AND its path prefix.
	// Any request to /users/admin/* runs:
	//   service mw → users mw → users/admin mw → handler
	admin := g.Group(
		"/admin", "User admin",
		"Admin-only operations on users.",
	)
	admin.Middleware(requestLogger("users/admin"))
	admin.Middleware(requireAdmin)

	admin.Route("DELETE", "/:name", 204,
		[]fizz.OperationOption{
			fizz.Summary("Hard-delete a user (admin only)"),
		},
		func(ctx *gin.Context, _ *sql.DB, in *struct {
			Name string `path:"name" validate:"required"`
		}) (any, error) {
			log.Printf("hard-deleting %s", in.Name)
			return nil, nil
		},
	)

	return *g
}

func main() {
	svc := service.Create(config.Config{
		Host:   "localhost",
		Port:   8082,
		SkipDB: true,
		Spec: openapi.Info{
			Title:       "Routing demo",
			Version:     "v1.0.0",
			Description: "Demonstrates groups, nested groups and middleware order.",
		},
	})

	// Service-level middleware runs first for every route.
	svc.Middleware(requestLogger("service"))

	svc.AddGroup(usersGroup())

	svc.Run()
}
