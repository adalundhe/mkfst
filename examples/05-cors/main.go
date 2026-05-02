// 05-cors — Apply the bundled CORS middleware.
//
// Run from the repo root:
//
//	go run ./examples/05-cors
//
// Then issue a preflight from a different origin and watch the headers:
//
//	curl -is -XOPTIONS http://localhost:8085/api \
//	    -H 'Origin: https://app.example.com' \
//	    -H 'Access-Control-Request-Method: POST' \
//	    -H 'Access-Control-Request-Headers: content-type'
package main

import (
	"database/sql"
	"time"

	"github.com/gin-gonic/gin"

	"mkfst/config"
	"mkfst/fizz"
	"mkfst/fizz/openapi"
	"mkfst/middleware/cors"
	"mkfst/service"
)

func main() {
	svc := service.Create(config.Config{
		Host:   "localhost",
		Port:   8085,
		SkipDB: true,
		Spec: openapi.Info{
			Title:       "CORS demo",
			Version:     "v1.0.0",
			Description: "Service mounted with mkfst's CORS middleware.",
		},
	})

	svc.Middleware(cors.CORS(cors.Config{
		AllowOrigins: []string{
			"https://app.example.com",
			"https://*.example.com", // requires AllowWildcard
		},
		AllowMethods:     []string{"GET", "POST", "PATCH", "DELETE"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization"},
		ExposeHeaders:    []string{"X-Request-Id"},
		AllowCredentials: true,
		AllowWildcard:    true,
		MaxAge:           12 * time.Hour,
	}))

	svc.Route("GET", "/api", 200,
		[]fizz.OperationOption{fizz.Summary("CORS-protected endpoint")},
		func(ctx *gin.Context, _ *sql.DB) (gin.H, error) {
			return gin.H{
				"origin": ctx.GetHeader("Origin"),
				"ok":     true,
			}, nil
		},
	)

	svc.Run()
}
