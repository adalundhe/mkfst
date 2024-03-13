package service

import (
	config "mkfst/config"
	router "mkfst/router"
	telemetry "mkfst/telemetry"
)

type Service struct {
	config config.Config
	router *router.Router
}

func (service *Service) Create(opts config.Config) *Service {
	router := router.Create(
		config.Create(opts),
	)
	service.router = &router

	return service
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
