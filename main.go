package main

import (
	sql "database/sql"
	"fmt"
	"mkfst/config"
	"mkfst/middleware/opentel"
	"mkfst/router"
	"mkfst/service"
	"mkfst/telemetry"

	"mkfst/fizz"
	"mkfst/fizz/openapi"

	"github.com/gin-gonic/gin"
	_ "github.com/mattn/go-sqlite3"
)

type User struct {
	Id   int    `json:"id"`
	Name string `json:"name"`
}

func AppV1Group() router.Group {
	apiv1 := router.CreateGroup(
		"/api/v1",
		"v1 API",
		"v1 API for Users.",
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
	).Route(
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

	apiv1.Middleware(
		opentel.RequestMetrics("v1 Users"),
	).Middleware(
		opentel.RequestTracing("v1 Users"),
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

	service.ConfigureTracing(
		telemetry.Default(),
	)

	service.AddGroup(
		AppV1Group(),
	)

	db := service.GetDB()
	db.Exec("CREATE TABLE `test` (`id` INTEGER PRIMARY KEY AUTOINCREMENT, `name` TEXT NOT NULL)")

	service.Run()

}
