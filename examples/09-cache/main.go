// 09-cache — API server with a response cache backed by providers/cache.
//
// Demonstrates:
//   - cache.NewMemoryCache for ephemeral key-value storage
//   - A tiny middleware that caches GET responses for 30 seconds
//   - A handler that performs an "expensive" computation and benefits
//     from the cache on repeated requests
//
// Run from the repo root:
//
//	go run ./examples/09-cache
//
// Then exercise:
//
//	curl -i http://localhost:8081/expensive/42
//	curl -i http://localhost:8081/expensive/42      # served from cache
//	curl -i http://localhost:8081/expensive/42 -H "Cache-Bypass: 1"
//	curl -X POST http://localhost:8081/cache/clear
package main

import (
	"bytes"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"mkfst/config"
	"mkfst/fizz"
	"mkfst/fizz/openapi"
	"mkfst/providers/cache"
	"mkfst/service"
)

// computed lets us prove the cache is working: the counter only
// increments when the handler actually runs (cache miss).
var computed atomic.Uint64

type Result struct {
	N        int    `json:"n"`
	Square   int    `json:"square"`
	ComputedAt string `json:"computed_at"`
	Calls    uint64 `json:"calls_so_far"`
}

func main() {
	c := cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 32 << 20})
	defer c.Close()

	svc := service.Create(config.Config{
		Host: "localhost", Port: 8081, SkipDB: true,
		Spec: openapi.Info{
			Title:       "Cache Demo",
			Version:     "v1.0.0",
			Description: "Response-cache middleware over providers/cache.",
		},
	})

	// Response-cache middleware. Caches 200 OK responses to GETs
	// for 30s, keyed by URI. Honors Cache-Bypass: 1 to skip.
	// Middleware uses the tonic-style signature
	// func(*gin.Context, *sql.DB) (any, error).
	svc.Middleware(func(g *gin.Context, _ *sql.DB) (any, error) {
		if g.Request.Method != http.MethodGet || g.GetHeader("Cache-Bypass") != "" {
			return nil, nil
		}
		key := "resp:" + g.Request.URL.RequestURI()
		if body, ok, _ := c.Get(g.Request.Context(), key); ok {
			g.Header("X-Cache", "HIT")
			g.Data(http.StatusOK, "application/json", body)
			g.Abort()
			return nil, nil
		}
		rec := &recordingWriter{ResponseWriter: g.Writer, body: &bytes.Buffer{}}
		g.Writer = rec
		g.Next()
		if rec.code == http.StatusOK || rec.code == 0 {
			_ = c.Set(g.Request.Context(), key, rec.body.Bytes(), 30*time.Second)
			g.Header("X-Cache", "MISS")
		}
		return nil, nil
	})

	// Expensive handler: simulates 200ms of work; cache hides it on
	// subsequent calls.
	svc.Route("GET", "/expensive/:n", 200,
		[]fizz.OperationOption{fizz.Summary("Compute n^2 (slowly)")},
		func(g *gin.Context, _ *sql.DB, in *struct {
			N int `path:"n" validate:"min=0,max=10000"`
		}) (Result, error) {
			time.Sleep(200 * time.Millisecond) // simulated work
			computed.Add(1)
			return Result{
				N:          in.N,
				Square:     in.N * in.N,
				ComputedAt: time.Now().Format(time.RFC3339Nano),
				Calls:      computed.Load(),
			}, nil
		},
	)

	// Operator-flush endpoint.
	svc.Route("POST", "/cache/clear", 200, nil,
		func(g *gin.Context, _ *sql.DB) (struct{ Cleared int }, error) {
			n, err := c.DeletePrefix(g.Request.Context(), "resp:")
			return struct{ Cleared int }{Cleared: n}, err
		},
	)

	svc.Run()
}

// recordingWriter captures the response body for caching.
type recordingWriter struct {
	gin.ResponseWriter
	body *bytes.Buffer
	code int
}

func (w *recordingWriter) Write(b []byte) (int, error) {
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
}
func (w *recordingWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}
func (w *recordingWriter) WriteString(s string) (int, error) {
	w.body.WriteString(s)
	return w.ResponseWriter.WriteString(s)
}

// keep imports tidy (strconv/strings are useful for richer caching keys)
var _ = strconv.Itoa
var _ = strings.Contains
