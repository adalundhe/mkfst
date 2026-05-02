// 08-openapi — Rich OpenAPI metadata.
//
// Run from the repo root:
//
//	go run ./examples/08-openapi
//
// Then open http://localhost:8088/api/docs to see the rendered Swagger
// UI, or fetch /openapi.json directly.
package main

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"mkfst/config"
	"mkfst/fizz"
	"mkfst/fizz/openapi"
	"mkfst/service"
	"mkfst/tonic"
)

// User is the response model for our endpoints.
type User struct {
	ID    int64  `json:"id"    example:"42"      description:"Auto-incrementing user ID."`
	Name  string `json:"name"  example:"alice"   description:"User's display name."`
	Email string `json:"email" example:"a@b.c"   description:"Primary email address."`
}

// APIError is a structured error type rendered for non-2xx responses.
type APIError struct {
	Code    string `json:"code"    example:"USER_NOT_FOUND" description:"Stable machine-readable error code."`
	Message string `json:"message" example:"user 42 does not exist" description:"Human-readable description."`
}

func (e APIError) Error() string { return e.Message }

func main() {
	svc := service.Create(config.Config{
		Host:   "localhost",
		Port:   8088,
		SkipDB: true,
		Spec: openapi.Info{
			Title:          "Users API",
			Version:        "v1.2.3",
			Description:    "Demonstrates rich OpenAPI metadata generation.",
			TermsOfService: "https://example.com/tos",
			Contact: &openapi.Contact{
				Name:  "API team",
				Email: "api@example.com",
				URL:   "https://example.com/contact",
			},
			License: &openapi.License{
				Name: "Apache-2.0",
				URL:  "https://www.apache.org/licenses/LICENSE-2.0",
			},
		},
	})

	// Map our APIError to the right HTTP status. With this hook the
	// schema we declared with fizz.Response("404", ..., APIError{}, ...)
	// matches what clients actually receive.
	tonic.SetErrorHook(func(c *gin.Context, err error) (int, interface{}) {
		var apiErr APIError
		if errors.As(err, &apiErr) {
			switch apiErr.Code {
			case "USER_NOT_FOUND":
				return http.StatusNotFound, apiErr
			case "EMAIL_TAKEN":
				return http.StatusConflict, apiErr
			}
		}
		var be tonic.BindError
		if errors.As(err, &be) {
			return http.StatusUnprocessableEntity, gin.H{
				"error":  be.Error(),
				"fields": be.ValidationErrors(),
			}
		}
		return http.StatusInternalServerError, gin.H{"error": err.Error()}
	})

	docs := []fizz.OperationOption{
		fizz.Summary("Get a user by ID"),
		fizz.Description("Returns the requested user, or a structured 404 if it does not exist."),
		fizz.ID("getUser"),
		fizz.Header("X-Request-Id", "Server-side trace ID", fizz.String),
		fizz.Response("404", "User not found", APIError{}, nil,
			APIError{Code: "USER_NOT_FOUND", Message: "user 42 does not exist"}),
		fizz.XCodeSample(&openapi.XCodeSample{
			Lang:   "curl",
			Label:  "curl",
			Source: "curl -s http://localhost:8088/users/42 | jq",
		}),
	}

	svc.Group("/users", "Users", "User read endpoints").Route(
		"GET", "/:id", 200, docs,
		func(ctx *gin.Context, _ *sql.DB, in *struct {
			ID int64 `path:"id" validate:"required,min=1"`
		}) (User, error) {
			if in.ID == 42 {
				return User{ID: 42, Name: "alice", Email: "a@b.c"}, nil
			}
			return User{}, APIError{
				Code:    "USER_NOT_FOUND",
				Message: "user does not exist",
			}
		},
	)

	// A second route, marked deprecated and tagged differently, to show
	// that overlap and tagging propagate into the spec.
	svc.Group("/users", "Users", "User read endpoints").Route(
		"GET", "/by-name/:name", 200,
		[]fizz.OperationOption{
			fizz.Summary("Get a user by name (deprecated)"),
			fizz.Deprecated(true),
			fizz.Description("Use GET /users/:id instead."),
		},
		func(ctx *gin.Context, _ *sql.DB, in *struct {
			Name string `path:"name" validate:"required"`
		}) (User, error) {
			return User{ID: 42, Name: in.Name, Email: in.Name + "@example.com"}, nil
		},
	)

	svc.Run()
}
