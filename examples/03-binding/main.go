// 03-binding — Path / query / header / body binding and validation.
//
// Run from the repo root:
//
//	go run ./examples/03-binding
//
// Demonstrates every supported source for an input field, including
// defaults, enums and validate-tag enforcement.
package main

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"

	"mkfst/config"
	"mkfst/fizz"
	"mkfst/fizz/openapi"
	"mkfst/service"
)

// SearchInput pulls fields from every binding source mkfst supports.
type SearchInput struct {
	// path
	OrgID int `path:"org_id" validate:"required,min=1"`

	// query
	Query string   `query:"q"      validate:"required,min=1,max=64"`
	Tags  []string `query:"tag"    explode:"false"` // ?tag=a,b,c
	Order string   `query:"order"  default:"asc" enum:"asc,desc"`
	Limit int      `query:"limit"  default:"20" validate:"min=1,max=200"`

	// header
	TraceID string `header:"X-Trace-Id"`

	// extra: bind a duration so we exercise encoding.TextUnmarshaler.
	Since time.Duration `query:"since" default:"24h"`
}

// CreateUserBody pulls fields from the request body.
type CreateUserBody struct {
	Name  string `json:"name"  validate:"required,min=1,max=80"`
	Email string `json:"email" validate:"required,email"`
	Age   int    `json:"age"   validate:"required,gte=13,lte=130"`
}

func main() {
	svc := service.Create(config.Config{
		Host:   "localhost",
		Port:   8083,
		SkipDB: true,
		Spec: openapi.Info{
			Title:       "Binding demo",
			Version:     "v1.0.0",
			Description: "Demonstrates every input-binding source.",
		},
	})

	orgs := svc.Group("/orgs", "Orgs", "Organisation-scoped routes")

	orgs.Route("GET", "/:org_id/search", 200,
		[]fizz.OperationOption{
			fizz.Summary("Search inside an org"),
			fizz.Description("Path, query and header binding all in one call."),
		},
		func(ctx *gin.Context, _ *sql.DB, in *SearchInput) (gin.H, error) {
			return gin.H{
				"org_id":  in.OrgID,
				"query":   in.Query,
				"tags":    in.Tags,
				"order":   in.Order,
				"limit":   in.Limit,
				"since":   in.Since.String(),
				"traceID": in.TraceID,
			}, nil
		},
	)

	orgs.Route("POST", "/:org_id/users", 201,
		[]fizz.OperationOption{
			fizz.Summary("Create a user inside an org"),
			fizz.Description("Body binding with validate tags. Try sending invalid JSON to see the 400 response."),
		},
		func(ctx *gin.Context, _ *sql.DB, in *struct {
			OrgID int `path:"org_id" validate:"required"`
			CreateUserBody
		}) (gin.H, error) {
			return gin.H{
				"org":     in.OrgID,
				"user":    in.Name,
				"created": time.Now().UTC().Format(time.RFC3339),
				"summary": fmt.Sprintf("created %s <%s>", in.Name, in.Email),
			}, nil
		},
	)

	svc.Run()
}
