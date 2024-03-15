package router

import (
	config "mkfst/config"
	db "mkfst/db"

	tonic "mkfst/tonic"

	gin "github.com/gin-gonic/gin"
	fizz "github.com/wI2L/fizz"
)

type Router struct {
	Base       *fizz.Fizz
	Db         *db.Connection
	groups     []Group
	routes     []Route
	middleware []MkfstHandler
}

type Group struct {
	router                  *Router
	Base                    *fizz.RouterGroup
	path, name, description string
	routes                  []Route
	middleware              []MkfstHandler
}

type Route struct {
	method, path string
	docs         []fizz.OperationOption
	handlers     []interface{}
	status       int
}

func (router *Router) Group(
	path string,
	name string,
	description string,
) *Group {

	group := &Group{
		router: router,
		Base: router.Base.Group(
			path,
			name,
			description,
		),
		path:        path,
		name:        name,
		description: description,
		routes:      []Route{},
		middleware:  []MkfstHandler{},
	}

	router.groups = append(router.groups, *group)
	return group
}

func (router *Router) AddGroup(group Group) *Router {
	group.Base = router.Base.Group(
		group.path,
		group.name,
		group.description,
	)

	router.groups = append(router.groups, group)
	return router
}

func (router *Router) Middleware(handlers ...MkfstHandler) *Router {
	router.middleware = append(router.middleware, handlers...)
	return router
}

func (router *Router) Route(
	method string,
	path string,
	status int,
	docs []fizz.OperationOption,
	handlers ...interface{},
) *Router {
	router.routes = append(
		router.routes,
		Route{
			method:   method,
			path:     path,
			docs:     docs,
			handlers: handlers,
			status:   status,
		},
	)

	return router
}

func (router *Router) Build() *fizz.Fizz {
	Base := router.Base

	for _, middleware := range router.middleware {
		Base.Use(func(ctx *gin.Context) {
			middleware(
				ctx,
				router.Db.Conn,
			)
		})
	}

	for _, route := range router.routes {
		router.addRouteToRouter(route)
	}

	for _, group := range router.groups {

		group.router = router

		for _, middleware := range group.middleware {
			group.Base.Use(func(ctx *gin.Context) {
				middleware(
					ctx,
					router.Db.Conn,
				)
			})
		}

		for _, route := range group.routes {
			group.addRouteToGroup(route)
		}
	}

	return router.Base
}

func (router *Router) addRouteToRouter(route Route) {

	mappedHandlers := MapHandlers(
		route.handlers,
		func(handler interface{}) gin.HandlerFunc {
			return tonic.Handler(handler, router.Db.Conn, route.status)
		},
	)

	router.Base.Handle(
		route.path,
		route.method,
		route.docs,
		mappedHandlers...,
	)
}

func (group *Group) Middleware(handlers ...MkfstHandler) *Group {
	group.middleware = append(group.middleware, handlers...)
	return group
}

func (group *Group) Route(
	method string,
	path string,
	status int,
	docs []fizz.OperationOption,
	handlers ...interface{},
) *Group {
	group.routes = append(
		group.routes,
		Route{
			method:   method,
			path:     path,
			docs:     docs,
			handlers: handlers,
			status:   status,
		},
	)

	return group
}

func (group *Group) Build() *fizz.Fizz {
	return group.router.Build()
}

func (group *Group) addRouteToGroup(route Route) {

	mappedHandlers := MapHandlers(
		route.handlers,
		func(handler interface{}) gin.HandlerFunc {
			return tonic.Handler(handler, group.router.Db.Conn, route.status)
		},
	)

	group.Base.Handle(
		route.path,
		route.method,
		route.docs,
		mappedHandlers...,
	)
}

func CreateGroup(
	path string,
	name string,
	description string,
) *Group {
	return &Group{
		path:        path,
		name:        name,
		description: description,
	}
}

func MapHandlers[T, U any](ts []T, f func(T) U) []U {
	var res []U
	for _, t := range ts {
		res = append(res, f(t))
	}
	return res
}

func Create(config config.Config) Router {

	if config.SkipDB {
		return Router{
			Base:       fizz.NewFromEngine(gin.New()),
			groups:     []Group{},
			routes:     []Route{},
			middleware: []MkfstHandler{},
		}
	}

	connection := db.Create(
		db.Configure(config.Database),
	)

	return Router{
		Base:       fizz.New(),
		Db:         &connection,
		groups:     []Group{},
		routes:     []Route{},
		middleware: []MkfstHandler{},
	}
}
