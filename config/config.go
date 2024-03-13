package config

import (
	"fmt"
	"log"
	db "mkfst/db"
	"os"
	"strconv"
)

type Config struct {
	Host         string
	Port         int
	UseTelemetry bool
	SkipDB       bool
	Database     db.ConnectionInfo
}

func (config *Config) getConfigHost(opts Config) *Config {
	if value, ok := os.LookupEnv("APP_HOST"); ok {
		config.Host = value
	}

	if opts.Host != "" {
		config.Host = opts.Host
	} else {
		config.Host = "0.0.0.0"
	}

	return config
}

func (config *Config) getConfigPort(opts Config) *Config {
	if value, ok := os.LookupEnv("APP_PORT"); ok {
		port, err := strconv.Atoi(value)
		if err != nil {
			log.Fatal(err)
		}
		config.Port = port
	}

	if opts.Port != 0 {
		config.Port = opts.Port
	} else {
		config.Port = 8000
	}

	return config

}

func (config *Config) getConfigSkipDB(opts Config) *Config {
	if value, ok := os.LookupEnv("APP_SKIP_DB"); ok {
		skipDB, err := strconv.ParseBool(value)
		if err != nil {
			log.Fatal(err)
		}
		config.SkipDB = skipDB
	}

	if opts.SkipDB {
		config.SkipDB = opts.SkipDB
	}

	return config
}

func (config *Config) getConfigUseTelemetry(opts Config) *Config {
	if value, ok := os.LookupEnv("APP_USE_TELEMETRY"); ok {
		useTelemetry, err := strconv.ParseBool(value)
		if err != nil {
			log.Fatal(err)
		}
		config.UseTelemetry = useTelemetry
	}

	if opts.UseTelemetry {
		config.UseTelemetry = opts.UseTelemetry
	}

	return config
}

func (config *Config) ToAddress() string {
	return fmt.Sprintf("%s:%d", config.Host, config.Port)
}

func Create(opts Config) Config {

	config := Config{
		Database: db.ConnectionInfo{},
	}

	config.getConfigHost(
		opts,
	).getConfigPort(
		opts,
	).getConfigSkipDB(
		opts,
	).getConfigUseTelemetry(
		opts,
	)

	return config
}
