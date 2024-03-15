package main

import (
	sql "database/sql"
	"fmt"
	"log"
	"mkfst/config"
	"mkfst/router"
	"mkfst/service"
	"net/http"

	"mkfst/fizz"
	"mkfst/fizz/openapi"

	"github.com/gin-gonic/gin"
	_ "github.com/mattn/go-sqlite3"
)

type User struct {
	id   int
	name string
}

func AppV1Group() router.Group {
	return *router.CreateGroup(
		"/app/v1",
		"v1App",
		"v1 App API",
	).Route(
		"GET",
		"/create",
		200,
		nil,
		func(ctx *gin.Context, db *sql.DB) (*[]User, error) {

			_, err := db.Exec("INSERT INTO `test` (`name`) VALUES ('Ada')")
			if err != nil {
				fmt.Print(err)
			}

			rows, err := db.Query("SELECT `id`, `name` FROM test")
			if err != nil {
				fmt.Print(err)
			}

			for rows.Next() {
				fmt.Print("HERE!")
				row := User{}
				if err := rows.Scan(&row.id, &row.name); err != nil {
					fmt.Print(err)
				}
				log.Print(row.id, row.name)
			}

			return &[]User{}, nil

		},
	)
}

func main() {

	service := service.Create(config.Config{
		Host:         "localhost",
		Port:         9001,
		SkipDB:       false,
		UseTelemetry: true,
		Spec: openapi.Info{
			Title: "MyApi",
		},
	})

	service.Route(
		"GET",
		"/status",
		200,
		[]fizz.OperationOption{
			fizz.Summary("This is the status endpoint"),
		},
		func(ctx *gin.Context, db *sql.DB) (any, error) {
			ctx.JSON(
				http.StatusOK,
				gin.H{
					"message": "pong",
				},
			)

			fmt.Print(db)

			return nil, nil
		},
	).AddGroup(
		AppV1Group(),
	)

	db := service.GetDB()
	db.Exec("CREATE TABLE `test` (`id` INTEGER PRIMARY KEY AUTOINCREMENT, `name` TEXT NOT NULL)")

	service.Run()

}
