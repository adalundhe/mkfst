// 14-aws — API server using all three AWS providers (DynamoDB,
// Secrets Manager, SQS), sharing one credential scope.
//
// Demonstrates:
//   - Single mkaws.Resolve call, shared across all three clients.
//   - Auth via host env vars OR AWS_ROLE_ARN env var (assume-role).
//   - HTTP routes that:
//        POST /orders           → DynamoDB put + SQS send
//        GET  /orders/:id       → DynamoDB get
//        GET  /config/:name     → Secrets Manager get
//   - Background SQS subscriber that processes incoming messages.
//
// Run from the repo root:
//
//	# Use whatever auth your shell already has:
//	AWS_REGION=us-east-1 \
//	  ORDERS_TABLE=Orders \
//	  ORDERS_QUEUE_URL=https://sqs.us-east-1.amazonaws.com/123/orders \
//	  go run ./examples/14-aws
//
//	# Or assume a role on top:
//	AWS_REGION=us-east-1 \
//	  AWS_ROLE_ARN=arn:aws:iam::123:role/myapp-runtime \
//	  go run ./examples/14-aws
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"mkfst/config"
	"mkfst/fizz"
	"mkfst/fizz/openapi"
	mkaws "mkfst/providers/aws"
	"mkfst/providers/aws/dynamodb"
	"mkfst/providers/aws/secrets"
	"mkfst/providers/aws/sqs"
	"mkfst/service"

	dynamotypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Order is the typed view we store in DynamoDB.
type Order struct {
	ID  string `dynamodbav:"id"  json:"id"`
	SKU string `dynamodbav:"sku" json:"sku"`
	Qty int    `dynamodbav:"qty" json:"qty"`
}

func envOrDie(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("env var %s required", key)
	}
	return v
}

func main() {
	tableName := envOrDie("ORDERS_TABLE")
	queueURL := envOrDie("ORDERS_QUEUE_URL")
	roleARN := os.Getenv("AWS_ROLE_ARN") // optional

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Resolve credentials ONCE — shared across all three clients.
	cfg, err := mkaws.Resolve(ctx, mkaws.Opts{
		RoleARN:     roleARN, // empty → use whatever the host has
		SessionName: "mkfst-14-aws",
	})
	if err != nil {
		log.Fatalf("aws resolve: %v", err)
	}
	log.Printf("auth resolved: region=%s assumed=%v", cfg.Region, roleARN != "")

	ddb := dynamodb.FromConfig(cfg)
	sm := secrets.FromConfig(cfg, 5*time.Minute)
	q := sqs.FromConfig(cfg, queueURL)

	// 2. Start a background subscriber that drains the queue.
	subDone, err := q.Subscribe(ctx, sqs.SubscribeOpts{
		AutoAck:     true,
		Concurrency: 4,
		MaxMessages: 10,
		WaitSeconds: 20,
		Handler: func(ctx context.Context, msg sqs.Message) error {
			var ord Order
			if err := json.Unmarshal(msg.Body, &ord); err != nil {
				return err
			}
			log.Printf("processed order %s sku=%s qty=%d (queued %s)",
				ord.ID, ord.SKU, ord.Qty, msg.SentAt.Format(time.RFC3339))
			return nil
		},
		OnError: func(err error) { log.Printf("sqs subscribe: %v", err) },
	})
	if err != nil {
		log.Fatalf("subscribe: %v", err)
	}

	// 3. HTTP server.
	svc := service.Create(config.Config{
		Host: "localhost", Port: 8081, SkipDB: true,
		Spec: openapi.Info{Title: "AWS Demo", Version: "v1.0.0"},
	})

	// POST /orders — write to DynamoDB, fan out via SQS.
	svc.Route("POST", "/orders", 202,
		[]fizz.OperationOption{fizz.Summary("Place an order")},
		func(g *gin.Context, _ *sql.DB, in *struct {
			ID  string `json:"id"  validate:"required"`
			SKU string `json:"sku" validate:"required"`
			Qty int    `json:"qty" validate:"min=1"`
		}) (struct{ ID string }, error) {
			ord := Order{ID: in.ID, SKU: in.SKU, Qty: in.Qty}
			if err := ddb.PutTyped(g.Request.Context(), tableName, ord); err != nil {
				return struct{ ID string }{}, err
			}
			body, _ := json.Marshal(ord)
			if _, err := q.Send(g.Request.Context(), body, sqs.SendOpts{
				Attributes: map[string]string{"sku": ord.SKU},
			}); err != nil {
				return struct{ ID string }{}, err
			}
			return struct{ ID string }{ID: ord.ID}, nil
		},
	)

	// GET /orders/:id — read from DynamoDB.
	svc.Route("GET", "/orders/:id", 200, nil,
		func(g *gin.Context, _ *sql.DB, in *struct {
			ID string `path:"id"`
		}) (Order, error) {
			var ord Order
			ok, err := ddb.GetTyped(g.Request.Context(), tableName, dynamodb.Item{
				"id": &dynamotypes.AttributeValueMemberS{Value: in.ID},
			}, &ord)
			if err != nil {
				return Order{}, err
			}
			if !ok {
				return Order{}, errors.New("not found")
			}
			return ord, nil
		},
	)

	// GET /config/:name — fetch a Secrets Manager secret.
	svc.Route("GET", "/config/:name", 200, nil,
		func(g *gin.Context, _ *sql.DB, in *struct {
			Name string `path:"name" validate:"required"`
		}) (struct{ Value string }, error) {
			b, err := sm.Get(g.Request.Context(), in.Name)
			if err != nil {
				return struct{ Value string }{}, err
			}
			return struct{ Value string }{Value: string(b)}, nil
		},
	)

	// Clean shutdown on signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("shutdown — draining sqs subscriber")
		cancel()
		<-subDone
		os.Exit(0)
	}()

	svc.Run()
}
