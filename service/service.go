package service

import (
	"database/sql"
	config "mkfst/config"
	router "mkfst/router"
	telemetry "mkfst/telemetry"
)

type Service struct {
	config config.Config
	router *router.Router
}

func Create(opts config.Config) Service {

	service := Service{
		config: config.Create(opts),
	}

	router := router.Create(
		service.config,
	)

	service.router = &router

	return service
}

func (service *Service) Middleware(middleware ...router.MkfstHandler) *router.Router {
	return service.router.Middleware(middleware...)
}

func (service *Service) AddGroup(group router.Group) *router.Router {
	return service.router.AddGroup(group)
}

func (service *Service) Route(
	method string,
	path string,
	handler router.MkfstHandler,
) *router.Router {
	return service.router.Route(
		method,
		path,
		handler,
	)
}

func (service *Service) Group(path string) *router.Group {
	return service.router.Group(
		path,
	)
}

func (service *Service) GetDB() *sql.DB {
	return service.router.Db.Conn
}

func (service *Service) Run() (err error) {

	otel := telemetry.Context{}

	if service.config.UseTelemetry {
		otel.Init()
		defer otel.Close()
	}

	ginRouter := service.router.Build()

	defer service.router.Db.Conn.Close()
	ginRouter.Run(service.config.ToAddress())

	return

}
