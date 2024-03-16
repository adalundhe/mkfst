package config

import (
	"fmt"
	"log"
	db "mkfst/db"
	"os"
	"strconv"

	"mkfst/fizz/openapi"
)

type Config struct {
	Host     string
	Port     int
	SkipDB   bool
	UseHTTPS bool
	Database db.ConnectionInfo
	Spec     openapi.Info
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

func (config *Config) getConfigUseHTTPS(opts Config) *Config {
	if value, ok := os.LookupEnv("APP_SKIP_HTTPS"); ok {
		useHTTPS, err := strconv.ParseBool(value)
		if err != nil {
			log.Fatal(err)
		}
		config.UseHTTPS = useHTTPS
	}

	if opts.UseHTTPS {
		config.UseHTTPS = opts.UseHTTPS
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

func (config *Config) ToAddress() string {
	return fmt.Sprintf("%s:%d", config.Host, config.Port)
}

func Create(opts Config) Config {

	config := Config{
		Database: db.ConnectionInfo{},
		Spec:     opts.Spec,
	}

	config.getConfigHost(
		opts,
	).getConfigPort(
		opts,
	).getConfigUseHTTPS(
		opts,
	).getConfigSkipDB(
		opts,
	)

	return config
}
