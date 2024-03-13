package service

import (
	config "mkfst/config"
	router "mkfst/router"
	telemetry "mkfst/telemetry"

	"github.com/gin-gonic/gin"
)

type Service struct {
	config config.Config
	router *router.Router
}

func Create(opts config.Config) Service {

	service := Service{}

	router := router.Create(
		config.Create(opts),
	)
	service.router = &router

	return service
}

func (service *Service) Middleware(middleware ...gin.HandlerFunc) *router.Router {
	return service.router.Middleware(middleware...)
}

func (service *Service) AddGroup(group router.Group) *router.Router {
	return service.router.AddGroup(group)
}

func (service *Service) Route(
	method string,
	path string,
	handler gin.HandlerFunc,
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

func (service *Service) Run() (err error) {

	otel := telemetry.Context{}

	if service.config.UseTelemetry {
		otel.Create()
		defer otel.Shutdown()
	}

	ginRouter := service.router.Build()
	ginRouter.Run(service.config.ToAddress())

	return

}
