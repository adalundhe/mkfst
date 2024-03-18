package main

import (
	sql "database/sql"
	"fmt"
	"mkfst/auth/avatar"
	"mkfst/auth/token"
	"mkfst/config"
	"mkfst/router"
	"mkfst/service"
	"strings"
	"time"

	"mkfst/fizz"
	"mkfst/fizz/openapi"
	"mkfst/middleware/auth"

	"github.com/gin-gonic/gin"
	_ "github.com/mattn/go-sqlite3"
)

type User struct {
	Id   int    `json:"id"`
	Name string `json:"name"`
}

func AppV1Group() router.Group {

	options := auth.Opts{
		SecretReader: token.SecretFunc(func(id string) (string, error) { // secret key for JWT
			return "secret", nil
		}),
		TokenDuration:  time.Minute * 5, // token expires in 5 minutes
		CookieDuration: time.Hour * 24,  // cookie expires in 1 day and will enforce re-login
		Issuer:         "my-test-app",
		URL:            "http://127.0.0.1:8080",
		AvatarStore:    avatar.NewLocalFS("/tmp"),
		Validator: token.ValidatorFunc(func(_ string, claims token.Claims) bool {
			// allow only dev_* names
			return claims.User != nil && strings.HasPrefix(claims.User.Name, "dev_")
		}),
	}

	// create auth service with providers
	authService := auth.NewService(options)
	authService.AddProvider("github", "<Client ID>", "<Client Secret>")   // add github provider
	authService.AddProvider("facebook", "<Client ID>", "<Client Secret>") // add facebook provider

	// retrieve auth middleware
	authMiddleware := authService.Middleware()
	authRoute, avatarRoute := authService.Handlers()

	apiv1 := router.CreateGroup(
		"/api/v1",
		"v1 API",
		"v1 API for Users.",
	).Route(
		"GET",
		"/auth",
		200,
		nil,
		authRoute,
	).Route(
		"GET",
		"/avatar",
		200,
		nil,
		avatarRoute,
	).Route(
		"GET",
		"/",
		200,
		[]fizz.OperationOption{
			fizz.Summary("Return the status of the v1 API"),
		},
		func(ctx *gin.Context, db *sql.DB) (string, error) {
			return "OK", nil
		},
	)

	apiv1.Group(
		"/users",
		"Users",
		"Users API for fetching, creating, and updating users.",
	).Middleware(authMiddleware.Auth).Route(
		"POST",
		"/create",
		201,
		[]fizz.OperationOption{
			fizz.Summary("Create a new user."),
		},
		func(ctx *gin.Context, db *sql.DB, user *User) (any, error) {
			_, err := db.Exec(
				fmt.Sprintf("INSERT INTO `test` (`name`) VALUES ('%s')", user.Name),
			)
			if err != nil {
				return nil, err
			}

			return "OK", nil
		},
	).Route(
		"GET",
		"/get",
		200,
		[]fizz.OperationOption{
			fizz.Summary("Get all users"),
		},
		func(ctx *gin.Context, db *sql.DB) ([]User, error) {

			rows, err := db.Query("SELECT `id`, `name` FROM test")
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var users []User

			for rows.Next() {
				var user User
				if err := rows.Scan(&user.Id, &user.Name); err != nil {
					return nil, err
				}

				users = append(users, user)
			}

			if rows.Err() != nil {
				return nil, rows.Err()
			}

			return users, nil

		},
	)

	apiv1.Group(
		"/avatars",
		"Avatars",
		"Avatars API for fetching, creating, and updating user avatrs.",
	).Middleware(authMiddleware.Auth).Route(
		"GET",
		"/auth",
		200,
		nil,
		authRoute,
	).Route(
		"GET",
		"/get",
		200,
		[]fizz.OperationOption{
			fizz.Summary("Get all avatars"),
		},
		func(ctx *gin.Context, db *sql.DB) (any, error) {
			return nil, nil
		},
	)

	return *apiv1
}

func main() {

	service := service.Create(config.Config{
		Host:   "localhost",
		Port:   9001,
		SkipDB: false,
		Spec: openapi.Info{
			Title:       "UsersApi",
			Version:     "v1.0.0",
			Description: "API for fetching, creating, and updating users.",
		},
	})

	service.AddGroup(
		AppV1Group(),
	)

	db := service.GetDB()
	db.Exec("CREATE TABLE `test` (`id` INTEGER PRIMARY KEY AUTOINCREMENT, `name` TEXT NOT NULL)")

	service.Run()

}
