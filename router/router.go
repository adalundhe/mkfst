package router

import (
	"log"
	config "mkfst/config"
	db "mkfst/db"

	gin "github.com/gin-gonic/gin"
)

type Router struct {
	base       *gin.Engine
	connection *db.Connection
	groups     []Group
	routes     []Route
	middleware []gin.HandlerFunc
}

type Group struct {
	router     *Router
	base       *gin.RouterGroup
	path       string
	routes     []Route
	middleware []gin.HandlerFunc
}

type Route struct {
	method, path string
	handlers     []gin.HandlerFunc
}

func (router *Router) Group(path string) *Group {
	group := &Group{
		router:     router,
		base:       router.base.Group(path),
		path:       path,
		routes:     []Route{},
		middleware: []gin.HandlerFunc{},
	}

	router.groups = append(router.groups, *group)
	return group
}

func (router *Router) AddGroup(group Group) *Router {
	router.groups = append(router.groups, group)
	return router
}

func (router *Router) Middleware(handlers ...gin.HandlerFunc) *Router {
	router.middleware = append(router.middleware, handlers...)
	return router
}

func (router *Router) Route(method string, path string, handlers ...gin.HandlerFunc) *Router {
	router.routes = append(
		router.routes,
		Route{
			method:   method,
			path:     path,
			handlers: handlers,
		},
	)

	return router
}

func (router *Router) Build() *gin.Engine {
	base := router.base

	for _, middleware := range router.middleware {
		base.Use(middleware)
	}

	for _, route := range router.routes {
		router.addRouteToRouter(route)
	}

	for _, group := range router.groups {

		for _, middleware := range group.middleware {
			group.base.Use(middleware)
		}

		for _, route := range group.routes {
			group.addRouteToGroup(route)
		}
	}

	return router.base
}

func (router *Router) addRouteToRouter(route Route) {
	switch route.method {
	case "DELETE":
		router.base.DELETE(route.path, route.handlers...)
	case "HEAD":
		router.base.HEAD(route.path, route.handlers...)
	case "OPTIONS":
		router.base.OPTIONS(route.path, route.handlers...)
	case "GET":
		router.base.GET(route.path, route.handlers...)
	case "PATCH":
		router.base.PATCH(route.path, route.handlers...)
	case "POST":
		router.base.POST(route.path, route.handlers...)
	case "PUT":
		router.base.PUT(route.path, route.handlers...)
	default:
		log.Fatalf("Err. - unknown HTTP method %s", route.method)
	}
}

func (group *Group) Middleware(handlers ...gin.HandlerFunc) *Group {
	group.middleware = append(group.middleware, handlers...)
	return group
}

func (group *Group) Route(method string, path string, handlers ...gin.HandlerFunc) *Group {
	group.routes = append(
		group.routes,
		Route{
			method:   method,
			path:     path,
			handlers: handlers,
		},
	)

	return group
}

func (group *Group) Build() *gin.Engine {
	return group.router.Build()
}

func (group *Group) addRouteToGroup(route Route) {
	switch route.method {
	case "DELETE":
		group.base.DELETE(route.path, route.handlers...)
	case "HEAD":
		group.base.HEAD(route.path, route.handlers...)
	case "OPTIONS":
		group.base.OPTIONS(route.path, route.handlers...)
	case "GET":
		group.base.GET(route.path, route.handlers...)
	case "PATCH":
		group.base.PATCH(route.path, route.handlers...)
	case "POST":
		group.base.POST(route.path, route.handlers...)
	case "PUT":
		group.base.PUT(route.path, route.handlers...)
	default:
		log.Fatalf("Err. - unknown HTTP method %s", route.method)
	}
}

func CreateGroup(path string) *Group {
	return &Group{
		path: path,
	}
}

func Create(config config.Config) Router {

	if config.SkipDB {
		return Router{
			base:       gin.Default(),
			groups:     []Group{},
			routes:     []Route{},
			middleware: []gin.HandlerFunc{},
		}
	}

	connection := db.Create(
		db.Configure(config.Database),
	)

	return Router{
		base:       gin.Default(),
		connection: &connection,
		groups:     []Group{},
		routes:     []Route{},
		middleware: []gin.HandlerFunc{},
	}
}
