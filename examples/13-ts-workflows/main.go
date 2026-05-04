// 13-ts-workflows — API server accepting TypeScript workflow submissions
// via providers/ts. Workflows are bundled by esbuild, validated against
// an allowlist, executed in a sandboxed QuickJS runtime, and run as
// DAGs against the existing workflows engine — bound to a docker stack.
//
// Demonstrates:
//   - One stack ("demo") brought up on docker.
//   - A TS engine wired with an allowlist + bridge dispatcher.
//   - HTTP endpoints to submit / run / inspect TS workflows.
//   - Workflow→stack scoping: every submitted workflow is bound to
//     "demo"; cross-stack reach is denied at the bridge.
//
// Requires a reachable docker daemon (rootful or rootless).
//
// Run from the repo root:
//
//	DOCKER_HOST=unix:///run/user/$(id -u)/docker.sock \
//	  go run ./examples/13-ts-workflows
//
// Then submit a TS workflow:
//
//	curl -X POST --data-binary @example.workflow.ts \
//	  -H 'Content-Type: application/typescript' \
//	  http://localhost:8081/workflows
//	curl -X POST http://localhost:8081/workflows/<name>/run
//	curl http://localhost:8081/workflows/instances/<id>
package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log"
	"path/filepath"
	"runtime"
	"time"

	"github.com/gin-gonic/gin"

	"mkfst/config"
	"mkfst/fizz"
	"mkfst/fizz/openapi"
	"mkfst/providers/cache"
	dockerprov "mkfst/providers/docker"
	"mkfst/providers/docker/network"
	"mkfst/providers/tasks"
	"mkfst/providers/ts"
	"mkfst/providers/ts/bundle"
	tsruntime "mkfst/providers/ts/runtime"
	"mkfst/providers/workflows"
	"mkfst/service"
)

// sdkPath returns the absolute path to providers/ts/sdk relative to
// this source file.
func sdkPath() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "providers", "ts", "sdk")
}

func main() {
	// 1. Docker + the target stack.
	cli, err := dockerprov.New(dockerprov.Opts{Timeout: 5 * time.Second})
	if err != nil {
		if errors.Is(err, dockerprov.ErrUnreachable) {
			log.Fatalf("docker daemon unreachable; set DOCKER_HOST: %v", err)
		}
		log.Fatal(err)
	}
	defer cli.Close()

	netEng, err := network.NewEngine(cli.SDK(), network.EngineOpts{})
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stack, _ := netEng.NewStack("demo")
	stack.MustAddService("svc",
		network.Image("alpine:3.19"),
		network.Cmd("sleep", "3600"),
	)
	if err := stack.Up(ctx); err != nil {
		log.Fatalf("stack up: %v", err)
	}
	defer func() {
		dc, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		_ = stack.Down(dc)
		_ = netEng.Close(dc)
	}()

	// 2. Workflow engine + worker.
	store := tasks.NewMemoryStore(tasks.MemoryOpts{})
	defer store.Close()
	worker, _ := tasks.NewWorker(tasks.WorkerOpts{
		Store: store, Concurrency: 8,
	})
	wfEng, _ := workflows.NewEngine(workflows.EngineOpts{
		Scheduler: tasks.NewScheduler(store),
		Worker:    worker,
		Outputs:   cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 32 << 20}),
	})
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(ctx) }()

	// 3. Bridge + stack resolver.
	resolver := ts.NewMapStackResolver()
	resolver.Set("demo", stack)
	bridge := tsruntime.NewBridge(tsruntime.AllowAll{})
	if err := ts.RegisterStackHandlers(bridge, resolver.Lookup); err != nil {
		log.Fatal(err)
	}

	// 4. TS engine — allowlist with the SDK + the blessed mkfst-stack
	//    module so workflows can call exec/runOneShot.
	al := bundle.NewAllowlist(sdkPath())
	_ = al.Add(bundle.ModuleEntry{Name: "@mkfst/sdk", Path: filepath.Join(sdkPath(), "mkfst-sdk")})
	_ = al.Add(bundle.ModuleEntry{Name: "mkfst-stack", Path: filepath.Join(sdkPath(), "mkfst-stack")})
	tsEng, err := ts.NewEngine(ts.EngineOpts{
		WorkflowEngine: wfEng,
		Allowlist:      al,
		Bridge:         bridge,
		EmitSourceMaps: true,
	})
	if err != nil {
		log.Fatal(err)
	}

	// 5. HTTP API.
	svc := service.Create(config.Config{
		Host: "localhost", Port: 8081, SkipDB: true,
		Spec: openapi.Info{Title: "TS Workflows Demo", Version: "v1.0.0"},
	})

	// Submit a TS workflow file. Body is the .ts source. Bound to
	// the "demo" stack — workflows can only reach this stack.
	svc.Route("POST", "/workflows", 200,
		[]fizz.OperationOption{fizz.Summary("Submit a TS workflow")},
		func(g *gin.Context, _ *sql.DB) (struct {
			Name   string `json:"name"`
			SHA    string `json:"sha256"`
			SizeKB int    `json:"size_kb"`
			Tasks  int    `json:"tasks"`
			Nodes  int    `json:"nodes"`
		}, error) {
			body, err := io.ReadAll(io.LimitReader(g.Request.Body, 256*1024))
			if err != nil {
				return struct {
					Name   string `json:"name"`
					SHA    string `json:"sha256"`
					SizeKB int    `json:"size_kb"`
					Tasks  int    `json:"tasks"`
					Nodes  int    `json:"nodes"`
				}{}, err
			}
			wf, err := tsEng.SubmitWith(g.Request.Context(), ts.SubmitOpts{
				Source:   body,
				Filename: g.Query("name"),
				Stack:    "demo",
			})
			if err != nil {
				return struct {
					Name   string `json:"name"`
					SHA    string `json:"sha256"`
					SizeKB int    `json:"size_kb"`
					Tasks  int    `json:"tasks"`
					Nodes  int    `json:"nodes"`
				}{}, err
			}
			return struct {
				Name   string `json:"name"`
				SHA    string `json:"sha256"`
				SizeKB int    `json:"size_kb"`
				Tasks  int    `json:"tasks"`
				Nodes  int    `json:"nodes"`
			}{wf.Name, wf.Bundle.SHA256, wf.Bundle.SizeKB, len(wf.DAG.Tasks), len(wf.DAG.Nodes)}, nil
		},
	)

	svc.Route("POST", "/workflows/:name/run", 202, nil,
		func(g *gin.Context, _ *sql.DB, in *struct {
			Name string `path:"name"`
		}) (struct{ Instance string }, error) {
			id, err := tsEng.Run(g.Request.Context(), in.Name, nil)
			return struct{ Instance string }{Instance: id}, err
		},
	)

	svc.Route("GET", "/workflows/instances/:id", 200, nil,
		func(g *gin.Context, _ *sql.DB, in *struct {
			ID string `path:"id"`
		}) (workflows.InstanceInfo, error) {
			return tsEng.Inspect(g.Request.Context(), in.ID)
		},
	)

	svc.Run()
	cancel()
	if err := <-workerDone; err != nil {
		log.Printf("worker exit: %v", err)
	}
}
