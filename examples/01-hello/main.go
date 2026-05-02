// 01-hello — the smallest possible mkfst server.
//
// Run from the repo root:
//
//	go run ./examples/01-hello
//
// Then open:
//
//	http://localhost:8081/hello
//	http://localhost:8081/api/docs
package main

import (
	"database/sql"

	"github.com/gin-gonic/gin"

	"mkfst/config"
	"mkfst/fizz"
	"mkfst/fizz/openapi"
	"mkfst/service"
)

func main() {
	svc := service.Create(config.Config{
		Host:   "localhost",
		Port:   8081,
		SkipDB: true,
		Spec: openapi.Info{
			Title:       "Hello API",
			Version:     "v1.0.0",
			Description: "Smallest possible mkfst server.",
		},
	})

	svc.Route(
		"GET", "/hello", 200,
		[]fizz.OperationOption{
			fizz.Summary("Greet the world"),
		},
		func(ctx *gin.Context, _ *sql.DB) (string, error) {
			return "hello, world", nil
		},
	)

	svc.Run()
}
