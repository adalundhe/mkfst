package service

import (
	"database/sql"
	"fmt"
	config "mkfst/config"
	router "mkfst/router"
	telemetry "mkfst/telemetry"
	http "net/http"

	"github.com/gin-gonic/gin"
	"github.com/wI2L/fizz"
	"github.com/wI2L/fizz/openapi"
)

type Service struct {
	config config.Config
	router *router.Router
	spec   *openapi.Info
}

func Create(opts config.Config) Service {

	service := Service{
		config: config.Create(opts),
		spec:   &opts.Spec,
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
	status int,
	docs []fizz.OperationOption,
	handler interface{},
) *router.Router {
	return service.router.Route(
		method,
		path,
		status,
		docs,
		handler,
	)
}

func (service *Service) Group(
	path string,
	name string,
	description string,
) *router.Group {
	return service.router.Group(
		path,
		name,
		description,
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

	var docsPrefix = "http"
	if service.config.UseHTTPS {
		docsPrefix = "https"
	}

	serviceAddress := service.config.ToAddress()

	service.router.Base.Engine().LoadHTMLGlob("docs/*")
	service.router.Base.GET("/docs/api", nil, func(ctx *gin.Context) {
		ctx.HTML(200, "index.tmpl", gin.H{
			"url": fmt.Sprintf(
				"%s://%s/%s",
				docsPrefix,
				serviceAddress,
				"openapi.json",
			),
		})
	})
	service.router.Base.GET("/openapi.json", nil, service.router.Base.OpenAPI(service.spec, "json"))
	service.router.Base.GET("/openapi.yaml", nil, service.router.Base.OpenAPI(service.spec, "yaml"))

	fizzRouter := service.router.Build()

	defer service.router.Db.Conn.Close()

	srv := &http.Server{
		Addr:    serviceAddress,
		Handler: fizzRouter,
	}
	srv.ListenAndServe()

	return

}
