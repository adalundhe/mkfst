package router

import (
	"fmt"
	"log"
	config "mkfst/config"
	db "mkfst/db"

	gin "github.com/gin-gonic/gin"
)

type Router struct {
	base       *gin.Engine
	Db         *db.Connection
	groups     []Group
	routes     []Route
	middleware []MkfstHandler
}

type Group struct {
	router     *Router
	base       *gin.RouterGroup
	path       string
	routes     []Route
	middleware []MkfstHandler
}

type Route struct {
	method, path string
	handlers     []MkfstHandler
}

func (router *Router) Group(path string) *Group {

	fmt.Print(router.base.Group(path), "GROUP!")
	group := &Group{
		router:     router,
		base:       router.base.Group(path),
		path:       path,
		routes:     []Route{},
		middleware: []MkfstHandler{},
	}

	router.groups = append(router.groups, *group)
	return group
}

func (router *Router) AddGroup(group Group) *Router {
	group.base = router.base.Group(group.path)
	router.groups = append(router.groups, group)
	return router
}

func (router *Router) Middleware(handlers ...MkfstHandler) *Router {
	router.middleware = append(router.middleware, handlers...)
	return router
}

func (router *Router) Route(method string, path string, handlers ...MkfstHandler) *Router {
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
		base.Use(func(ctx *gin.Context) {
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
			group.base.Use(func(ctx *gin.Context) {
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

	return router.base
}

func (router *Router) addRouteToRouter(route Route) {
	switch route.method {
	case "DELETE":
		router.base.DELETE(route.path, MapHandlers(
			route.handlers,
			func(handler MkfstHandler) gin.HandlerFunc {
				return func(ctx *gin.Context) {
					handler(
						ctx,
						router.Db.Conn,
					)
				}
			},
		)...)
	case "HEAD":
		router.base.HEAD(route.path, MapHandlers(
			route.handlers,
			func(handler MkfstHandler) gin.HandlerFunc {
				return func(ctx *gin.Context) {
					handler(
						ctx,
						router.Db.Conn,
					)
				}
			},
		)...)
	case "OPTIONS":
		router.base.OPTIONS(route.path, MapHandlers(
			route.handlers,
			func(handler MkfstHandler) gin.HandlerFunc {
				return func(ctx *gin.Context) {
					handler(
						ctx,
						router.Db.Conn,
					)
				}
			},
		)...)
	case "GET":
		router.base.GET(route.path, MapHandlers(
			route.handlers,
			func(handler MkfstHandler) gin.HandlerFunc {
				return func(ctx *gin.Context) {
					handler(
						ctx,
						router.Db.Conn,
					)
				}
			},
		)...)
	case "PATCH":
		router.base.PATCH(route.path, MapHandlers(
			route.handlers,
			func(handler MkfstHandler) gin.HandlerFunc {
				return func(ctx *gin.Context) {
					handler(
						ctx,
						router.Db.Conn,
					)
				}
			},
		)...)
	case "POST":
		router.base.POST(route.path, MapHandlers(
			route.handlers,
			func(handler MkfstHandler) gin.HandlerFunc {
				return func(ctx *gin.Context) {
					handler(
						ctx,
						router.Db.Conn,
					)
				}
			},
		)...)
	case "PUT":
		router.base.PUT(route.path, MapHandlers(
			route.handlers,
			func(handler MkfstHandler) gin.HandlerFunc {
				return func(ctx *gin.Context) {
					handler(
						ctx,
						router.Db.Conn,
					)
				}
			},
		)...)
	default:
		log.Fatalf("Err. - unknown HTTP method %s", route.method)
	}
}

func (group *Group) Middleware(handlers ...MkfstHandler) *Group {
	group.middleware = append(group.middleware, handlers...)
	return group
}

func (group *Group) Route(method string, path string, handlers ...MkfstHandler) *Group {
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
		group.base.DELETE(route.path, MapHandlers(
			route.handlers,
			func(handler MkfstHandler) gin.HandlerFunc {
				return func(ctx *gin.Context) {
					handler(
						ctx,
						group.router.Db.Conn,
					)
				}
			},
		)...)
	case "HEAD":
		group.base.HEAD(route.path, MapHandlers(
			route.handlers,
			func(handler MkfstHandler) gin.HandlerFunc {
				return func(ctx *gin.Context) {
					handler(
						ctx,
						group.router.Db.Conn,
					)
				}
			},
		)...)
	case "OPTIONS":
		group.base.OPTIONS(route.path, MapHandlers(
			route.handlers,
			func(handler MkfstHandler) gin.HandlerFunc {
				return func(ctx *gin.Context) {
					handler(
						ctx,
						group.router.Db.Conn,
					)
				}
			},
		)...)
	case "GET":
		group.base.GET(route.path, MapHandlers(
			route.handlers,
			func(handler MkfstHandler) gin.HandlerFunc {
				return func(ctx *gin.Context) {
					handler(
						ctx,
						group.router.Db.Conn,
					)
				}
			},
		)...)
	case "PATCH":
		group.base.PATCH(route.path, MapHandlers(
			route.handlers,
			func(handler MkfstHandler) gin.HandlerFunc {
				return func(ctx *gin.Context) {
					handler(
						ctx,
						group.router.Db.Conn,
					)
				}
			},
		)...)
	case "POST":
		group.base.POST(route.path, MapHandlers(
			route.handlers,
			func(handler MkfstHandler) gin.HandlerFunc {
				return func(ctx *gin.Context) {
					handler(
						ctx,
						group.router.Db.Conn,
					)
				}
			},
		)...)
	case "PUT":
		group.base.PUT(route.path, MapHandlers(
			route.handlers,
			func(handler MkfstHandler) gin.HandlerFunc {
				return func(ctx *gin.Context) {
					handler(
						ctx,
						group.router.Db.Conn,
					)
				}
			},
		)...)
	default:
		log.Fatalf("Err. - unknown HTTP method %s", route.method)
	}
}

func CreateGroup(path string) *Group {
	return &Group{
		path: path,
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
			base:       gin.New(),
			groups:     []Group{},
			routes:     []Route{},
			middleware: []MkfstHandler{},
		}
	}

	connection := db.Create(
		db.Configure(config.Database),
	)

	return Router{
		base:       gin.New(),
		Db:         &connection,
		groups:     []Group{},
		routes:     []Route{},
		middleware: []MkfstHandler{},
	}
}
