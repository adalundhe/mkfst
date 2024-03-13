package db

import (
	"database/sql"
	"errors"
	"log"
	"os"
	"path/filepath"
)

func checkFileExists(filePath string) bool {
	_, error := os.Stat(filePath)
	return !errors.Is(error, os.ErrNotExist)
}

func createSqliteDB(connection Connection, config ConnectionInfo) Connection {

	if !checkFileExists(config.Host) {
		cwd, err := os.Getwd()
		if err != nil {
			log.Fatal(err)
		}

		file, err := os.Create(
			filepath.Join(cwd, connection.Config.Host),
		)

		if err != nil {
			log.Fatal(err)
		}

		file.Close()
	}

	db, err := sql.Open(
		"sqlite3",
		config.Host,
	)

	if err != nil {
		log.Fatal(err)
	}
	connection.Conn = db

	return connection

}
