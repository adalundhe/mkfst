// 10-tasks — API server enqueuing background jobs via providers/tasks.
//
// Demonstrates:
//   - In-memory task store + worker pool inside the API process
//   - Handler enqueues a job and returns immediately (202 Accepted)
//   - Status lookup endpoint
//   - Recurring "heartbeat" job
//
// Run from the repo root:
//
//	go run ./examples/10-tasks
//
// Then exercise:
//
//	curl -X POST -H "Content-Type: application/json" \
//	  -d '{"to":"alice@example.com","subject":"hi"}' \
//	  http://localhost:8081/jobs/email
//	curl http://localhost:8081/jobs/<id>
//	curl http://localhost:8081/stats
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"mkfst/config"
	"mkfst/fizz"
	"mkfst/fizz/openapi"
	"mkfst/providers/tasks"
	"mkfst/service"
)

var (
	emailsSent atomic.Uint64
	beats      atomic.Uint64
)

type EnqueueRequest struct {
	To      string `json:"to" validate:"required,email"`
	Subject string `json:"subject" validate:"required"`
}

type EnqueueResponse struct {
	JobID string `json:"job_id"`
}

type StatusResponse struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error,omitempty"`
}

type StatsResponse struct {
	EmailsSent  uint64        `json:"emails_sent"`
	HeartBeats  uint64        `json:"heart_beats"`
	WorkerStats tasks.Stats   `json:"worker"`
}

func main() {
	store := tasks.NewMemoryStore(tasks.MemoryOpts{DedupWindow: time.Minute})
	defer store.Close()

	worker, err := tasks.NewWorker(tasks.WorkerOpts{
		Store:       store,
		Concurrency: 4,
		OnError: func(workerID, op string, err error) {
			log.Printf("worker[%s] %s: %v", workerID, op, err)
		},
	})
	if err != nil {
		log.Fatalf("worker: %v", err)
	}

	// Register handlers.
	if err := worker.Register("email", func(ctx context.Context, t tasks.Task) error {
		// Simulate sending an email.
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
		emailsSent.Add(1)
		log.Printf("email sent: %s", t.Payload)
		return nil
	}); err != nil {
		log.Fatalf("register email: %v", err)
	}
	if err := worker.Register("heartbeat", func(ctx context.Context, t tasks.Task) error {
		beats.Add(1)
		return nil
	}); err != nil {
		log.Fatalf("register heartbeat: %v", err)
	}

	// Worker run loop.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(ctx) }()

	// Recurring scheduler — heartbeat every 5 seconds.
	rs, err := tasks.NewRecurringScheduler(tasks.RecurringOpts{
		Store: store, Tick: 500 * time.Millisecond,
	})
	if err != nil {
		log.Fatalf("recurring: %v", err)
	}
	if err := rs.Every("hb", 5*time.Second, tasks.Task{Type: "heartbeat"}); err != nil {
		log.Fatalf("schedule heartbeat: %v", err)
	}
	rsDone := make(chan error, 1)
	go func() { rsDone <- rs.Run(ctx) }()

	scheduler := tasks.NewScheduler(store)

	svc := service.Create(config.Config{
		Host: "localhost", Port: 8081, SkipDB: true,
		Spec: openapi.Info{
			Title:   "Tasks Demo",
			Version: "v1.0.0",
		},
	})

	svc.Route("POST", "/jobs/email", 202,
		[]fizz.OperationOption{fizz.Summary("Enqueue an email send")},
		func(g *gin.Context, _ *sql.DB, in *EnqueueRequest) (EnqueueResponse, error) {
			payload := []byte(fmt.Sprintf("%s|%s", in.To, in.Subject))
			rec, err := scheduler.Enqueue(g.Request.Context(), tasks.Task{
				Type:       "email",
				Payload:    payload,
				UniqueKey:  "email:" + in.To + ":" + in.Subject,
				MaxRetries: tasks.Retries(3),
				Timeout:    10 * time.Second,
			})
			if err != nil {
				return EnqueueResponse{}, err
			}
			return EnqueueResponse{JobID: rec.Task.ID}, nil
		},
	)

	svc.Route("GET", "/jobs/:id", 200,
		[]fizz.OperationOption{fizz.Summary("Fetch a job's status")},
		func(g *gin.Context, _ *sql.DB, in *struct {
			ID string `path:"id" validate:"required"`
		}) (StatusResponse, error) {
			rec, err := scheduler.Inspect(g.Request.Context(), in.ID)
			if err != nil {
				return StatusResponse{}, err
			}
			return StatusResponse{
				ID:       rec.Task.ID,
				State:    string(rec.State),
				Attempts: rec.Attempts,
				Error:    rec.LastError,
			}, nil
		},
	)

	svc.Route("DELETE", "/jobs/:id", 200, nil,
		func(g *gin.Context, _ *sql.DB, in *struct {
			ID string `path:"id" validate:"required"`
		}) (struct{ Cancelled bool }, error) {
			err := scheduler.Cancel(g.Request.Context(), in.ID)
			return struct{ Cancelled bool }{Cancelled: err == nil}, err
		},
	)

	svc.Route("GET", "/stats", 200, nil,
		func(g *gin.Context, _ *sql.DB) (StatsResponse, error) {
			return StatsResponse{
				EmailsSent:  emailsSent.Load(),
				HeartBeats:  beats.Load(),
				WorkerStats: worker.Stats(),
			}, nil
		},
	)

	svc.Run()
	// On shutdown the parent ctx is canceled; the worker + recurring
	// scheduler goroutines exit and their errors land in the channels.
	cancel()
	if err := <-workerDone; err != nil {
		log.Printf("worker exit: %v", err)
	}
	if err := <-rsDone; err != nil {
		log.Printf("recurring exit: %v", err)
	}
}
