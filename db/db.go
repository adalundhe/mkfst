package db

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"
)

type ConnectionInfo struct {
	Type, Host, Port, Username, Password, Database string
	UseSSL                                         bool
}

type Connection struct {
	db     *sql.DB
	config ConnectionInfo
}

func (config *ConnectionInfo) getDBType(opts ConnectionInfo) *ConnectionInfo {

	if value, ok := os.LookupEnv("DB_TYPE"); ok {
		config.Type = value
	}

	if opts.Type != "" {
		config.Type = opts.Type

	} else {
		config.Type = "SQLITE"
	}

	return config

}

func (config *ConnectionInfo) getDBHost(opts ConnectionInfo) *ConnectionInfo {

	if value, ok := os.LookupEnv("DB_HOST"); ok {
		config.Host = value
	}

	if opts.Host != "" {
		config.Host = opts.Host

	} else {
		switch config.Type {
		case "MYSQL":
			config.Host = "localhost"
		case "POSTGRESQL":
			config.Host = "localhost"
		default:
			config.Host = "app.db"
		}
	}

	return config
}

func (config *ConnectionInfo) getDBPort(opts ConnectionInfo) *ConnectionInfo {

	if value, ok := os.LookupEnv("DB_PORT"); ok {
		config.Port = value
	}

	if opts.Port != "" {
		config.Port = opts.Port

	} else {
		switch config.Type {
		case "MYSQL":
			config.Port = "3306"
		case "POSTGRESQL":
			config.Port = "5432"
		default:
			config.Port = ""
		}
	}

	return config
}

func (config *ConnectionInfo) getDBUsername(opts ConnectionInfo) *ConnectionInfo {

	if value, ok := os.LookupEnv("DB_USERNAME"); ok {
		config.Username = value
	}

	if opts.Username != "" {
		config.Username = opts.Username

	} else {
		switch config.Type {
		case "MYSQL":
			log.Fatal("Err. - A username is required to use MYSQL databases")
		case "POSTGRESQL":
			log.Fatal("Err. - A username is required to use MYSQL databases")
		default:
			config.Username = ""
		}
	}

	return config
}

func (config *ConnectionInfo) getDBPassword(opts ConnectionInfo) *ConnectionInfo {

	if value, ok := os.LookupEnv("DB_PASSWORD"); ok {
		config.Password = value
	}

	if opts.Password != "" {
		config.Password = opts.Password

	} else {
		switch config.Type {
		case "MYSQL":
			log.Fatal("Err. - A password is required to use MYSQL databases")
		case "POSTGRESQL":
			log.Fatal("Err. - A password is required to use MYSQL databases")
		default:
			config.Password = ""
		}
	}

	return config
}

func (config *ConnectionInfo) getDBName(opts ConnectionInfo) *ConnectionInfo {

	if value, ok := os.LookupEnv("DB_NAME"); ok {
		config.Database = value
	}

	if opts.Database != "" {
		config.Database = opts.Database

	} else {
		config.Database = "app"
	}

	return config

}

func (config *ConnectionInfo) getDBSSL(opts ConnectionInfo) *ConnectionInfo {
	if value, ok := os.LookupEnv("DB_USE_SSL"); ok {
		useSSL, err := strconv.ParseBool(value)
		if err != nil {
			log.Fatal(err)
		}

		config.UseSSL = useSSL
	} else {
		config.UseSSL = opts.UseSSL
	}

	return config
}

func Configure(opts ConnectionInfo) ConnectionInfo {

	config := ConnectionInfo{}

	config.getDBType(
		opts,
	).getDBHost(
		opts,
	).getDBPort(
		opts,
	).getDBUsername(
		opts,
	).getDBPassword(
		opts,
	).getDBName(
		opts,
	).getDBSSL(
		opts,
	)

	return config

}

func Create(config ConnectionInfo) Connection {

	connection := Connection{
		config: config,
	}

	switch config.Type {
	case "MYSQL":
		db, err := sql.Open(
			"mysql",
			fmt.Sprintf(
				"%s:%s@tcp(%s:%s)/%s",
				config.Username,
				config.Password,
				config.Host,
				config.Port,
				config.Database,
			),
		)

		if err != nil {
			log.Fatal(err)
		}
		connection.db = db

	case "POSTGRESQL":
		db, err := sql.Open(
			"pgx",
			fmt.Sprintf(
				"postgres://%s:%s@%s:%s/%s?sslmode=disable",
				config.Username,
				config.Password,
				config.Host,
				config.Port,
				config.Database,
			),
		)

		if err != nil {
			log.Fatal(err)
		}
		connection.db = db
	default:
		db, err := sql.Open(
			"sqlite3",
			config.Host,
		)

		if err != nil {
			log.Fatal(err)
		}
		connection.db = db
	}

	return connection

}
