// 11-workflows — API server submitting DAG workflows via providers/workflows.
//
// Demonstrates:
//   - Construction of a DAG with parent-output flow
//   - Engine + scheduler + worker wiring
//   - Submit via HTTP, inspect via HTTP, cancel via HTTP
//   - Per-node failure policies (this example uses HaltWorkflow)
//
// Run from the repo root:
//
//	go run ./examples/11-workflows
//
// Then exercise:
//
//	curl -X POST http://localhost:8081/workflows/etl/run
//	curl http://localhost:8081/workflows/instances/<id>
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"mkfst/config"
	"mkfst/fizz"
	"mkfst/fizz/openapi"
	"mkfst/providers/cache"
	"mkfst/providers/tasks"
	"mkfst/providers/workflows"
	"mkfst/service"
)

func main() {
	store := tasks.NewMemoryStore(tasks.MemoryOpts{})
	defer store.Close()
	worker, _ := tasks.NewWorker(tasks.WorkerOpts{
		Store: store, Concurrency: 4,
		PollInterval: 5 * time.Millisecond, MaintenanceInterval: 10 * time.Millisecond,
	})

	wfEng, err := workflows.NewEngine(workflows.EngineOpts{
		Scheduler: tasks.NewScheduler(store),
		Worker:    worker,
		Outputs:   cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 16 << 20}),
	})
	if err != nil {
		log.Fatal(err)
	}

	// ETL pipeline: extract → transform → load.
	def := workflows.New("etl")
	extract := def.MustAdd("extract", workflows.OfType("etl.extract"))
	transform := def.MustAdd("transform",
		workflows.OfType("etl.transform"),
		workflows.DependsOn(extract),
	)
	def.MustAdd("load",
		workflows.OfType("etl.load"),
		workflows.DependsOn(transform),
	)
	if err := wfEng.Register(def); err != nil {
		log.Fatal(err)
	}

	// Handlers — receive parent outputs, return this node's bytes.
	_ = wfEng.RegisterHandler("etl.extract", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		// Pretend to fetch some rows.
		rows := []map[string]any{{"id": 1, "name": "alice"}, {"id": 2, "name": "bob"}}
		return json.Marshal(rows)
	})
	_ = wfEng.RegisterHandler("etl.transform", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		var rows []map[string]any
		if err := json.Unmarshal(parents["extract"], &rows); err != nil {
			return nil, err
		}
		for _, r := range rows {
			if name, ok := r["name"].(string); ok {
				r["name"] = strings.ToUpper(name)
			}
		}
		return json.Marshal(rows)
	})
	_ = wfEng.RegisterHandler("etl.load", func(ctx context.Context, parents map[string][]byte) ([]byte, error) {
		// Pretend to write somewhere.
		return []byte(fmt.Sprintf("loaded %d bytes", len(parents["transform"]))), nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(ctx) }()

	svc := service.Create(config.Config{
		Host: "localhost", Port: 8081, SkipDB: true,
		Spec: openapi.Info{Title: "Workflows Demo", Version: "v1.0.0"},
	})

	// Trigger an instance.
	svc.Route("POST", "/workflows/:name/run", 202,
		[]fizz.OperationOption{fizz.Summary("Submit a workflow instance")},
		func(g *gin.Context, _ *sql.DB, in *struct {
			Name string `path:"name"`
		}) (struct{ Instance string }, error) {
			id, err := wfEng.Submit(g.Request.Context(), in.Name, nil)
			return struct{ Instance string }{Instance: id}, err
		},
	)

	// Inspect.
	svc.Route("GET", "/workflows/instances/:id", 200, nil,
		func(g *gin.Context, _ *sql.DB, in *struct {
			ID string `path:"id"`
		}) (workflows.InstanceInfo, error) {
			return wfEng.Inspect(g.Request.Context(), in.ID)
		},
	)

	// Cancel.
	svc.Route("DELETE", "/workflows/instances/:id", 200, nil,
		func(g *gin.Context, _ *sql.DB, in *struct {
			ID string `path:"id"`
		}) (struct{ OK bool }, error) {
			err := wfEng.Cancel(g.Request.Context(), in.ID)
			return struct{ OK bool }{OK: err == nil}, err
		},
	)

	svc.Run()
	cancel()
	if err := <-workerDone; err != nil {
		log.Printf("worker exit: %v", err)
	}
}
