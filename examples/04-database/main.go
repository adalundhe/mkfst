// 04-database — Full CRUD on SQLite using mkfst's bundled connection.
//
// Run from the repo root:
//
//	go run ./examples/04-database
//
// On the first run a file `04-database.sqlite` is created in the current
// working directory and the `users` table is provisioned.
package main

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	_ "github.com/mattn/go-sqlite3"

	"mkfst/config"
	"mkfst/db"
	"mkfst/fizz"
	"mkfst/fizz/openapi"
	"mkfst/router"
	"mkfst/service"
)

type User struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

const schema = `
CREATE TABLE IF NOT EXISTS users (
    id    INTEGER PRIMARY KEY AUTOINCREMENT,
    name  TEXT NOT NULL,
    email TEXT NOT NULL UNIQUE
);`

func usersGroup() router.Group {
	g := router.CreateGroup("/users", "Users", "User CRUD on SQLite.")

	g.Route("GET", "/", 200,
		[]fizz.OperationOption{fizz.Summary("List users")},
		func(ctx *gin.Context, db *sql.DB) ([]User, error) {
			rows, err := db.QueryContext(ctx.Request.Context(),
				`SELECT id, name, email FROM users ORDER BY id`)
			if err != nil {
				return nil, err
			}
			defer rows.Close()

			users := []User{}
			for rows.Next() {
				var u User
				if err := rows.Scan(&u.ID, &u.Name, &u.Email); err != nil {
					return nil, err
				}
				users = append(users, u)
			}
			return users, rows.Err()
		},
	)

	g.Route("POST", "/", 201,
		[]fizz.OperationOption{
			fizz.Summary("Create a user"),
			fizz.Response("409", "Email already exists", gin.H{}, nil, nil),
		},
		func(ctx *gin.Context, db *sql.DB, in *struct {
			Name  string `json:"name"  validate:"required,min=1,max=80"`
			Email string `json:"email" validate:"required,email"`
		}) (User, error) {
			res, err := db.ExecContext(ctx.Request.Context(),
				`INSERT INTO users(name, email) VALUES(?, ?)`,
				in.Name, in.Email)
			if err != nil {
				return User{}, err
			}
			id, _ := res.LastInsertId()
			return User{ID: id, Name: in.Name, Email: in.Email}, nil
		},
	)

	g.Route("GET", "/:id", 200,
		[]fizz.OperationOption{
			fizz.Summary("Get a user by ID"),
			fizz.Response("404", "User not found", gin.H{}, nil, nil),
		},
		func(ctx *gin.Context, db *sql.DB, in *struct {
			ID int64 `path:"id" validate:"required"`
		}) (User, error) {
			var u User
			err := db.QueryRowContext(ctx.Request.Context(),
				`SELECT id, name, email FROM users WHERE id = ?`, in.ID).
				Scan(&u.ID, &u.Name, &u.Email)
			if errors.Is(err, sql.ErrNoRows) {
				ctx.AbortWithStatus(http.StatusNotFound)
				return User{}, errors.New("user not found")
			}
			return u, err
		},
	)

	g.Route("DELETE", "/:id", 204,
		[]fizz.OperationOption{fizz.Summary("Delete a user")},
		func(ctx *gin.Context, db *sql.DB, in *struct {
			ID int64 `path:"id" validate:"required"`
		}) (any, error) {
			_, err := db.ExecContext(ctx.Request.Context(),
				`DELETE FROM users WHERE id = ?`, in.ID)
			return nil, err
		},
	)

	return *g
}

func main() {
	svc := service.Create(config.Config{
		Host: "localhost",
		Port: 8084,
		Database: db.ConnectionInfo{
			Type: "SQLITE",
			Host: "04-database.sqlite", // file path relative to cwd
		},
		Spec: openapi.Info{
			Title:       "Users API",
			Version:     "v1.0.0",
			Description: "CRUD on a SQLite-backed users table.",
		},
	})

	if _, err := svc.GetDB().Exec(schema); err != nil {
		panic(err)
	}

	svc.AddGroup(usersGroup())
	svc.Run()
}
