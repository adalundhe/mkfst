# AWS Providers

`providers/aws/*` is mkfst's set of AWS service wrappers, sharing
one credential-resolution layer:

| Package | What it does |
|---|---|
| `providers/aws` | Shared credential + region resolver. Every other AWS provider builds on this. |
| `providers/aws/dynamodb` | DynamoDB CRUD + Query/Scan + typed marshaling. |
| `providers/aws/secrets` | Secrets Manager fetch + caching + container-injection helpers. |
| `providers/aws/sqs` | SQS Send / Receive / Delete / long-poll Subscribe. |

## Authorization model

Single resolution chain, used by every AWS provider:

```
Opts.RoleARN set?
   │
   ├─ yes ─→ resolve base credentials (env / shared / IMDS / IRSA)
   │         use them to AssumeRole on Opts.RoleARN via STS
   │         driver: temporary creds from STS
   │
   └─ no  ─→ resolve base credentials directly
             driver: whatever the host already has
```

The base chain follows the standard `aws-sdk-go-v2` order:

1. `AWS_ACCESS_KEY_ID` + `AWS_SECRET_ACCESS_KEY` env vars (if both set)
2. `~/.aws/credentials` shared file (profile honored via `AWS_PROFILE`)
3. EC2 instance profile / EKS service-account (IRSA) / ECS task role / Lambda execution role — whichever the host actually has

```go
import mkaws "mkfst/providers/aws"

// Use whatever auth the host has.
cfg, err := mkaws.FromEnv(ctx)

// Or assume a role on top.
cfg, err = mkaws.FromARN(ctx, "arn:aws:iam::123456789012:role/myapp-runtime", /*externalID*/ "")

// Or full control.
cfg, err = mkaws.Resolve(ctx, mkaws.Opts{
    Region:           "us-west-2",
    RoleARN:          "arn:aws:iam::...:role/...",
    ExternalID:       "trust-token-123",
    SessionName:      "mkfst-prod",
    SessionDuration:  time.Hour,
    Endpoint:         "https://sts.us-west-2.amazonaws.com", // optional, for VPC endpoints
    MaxRetries:       3,
    HTTPTimeout:      30 * time.Second,
})
```

Region resolution is the same chain: `Opts.Region` → `AWS_REGION` →
`AWS_DEFAULT_REGION` → shared config.

The resolved `aws.Config` is reusable across providers — share one
config across DynamoDB + Secrets + SQS calls in the same auth scope.

## DynamoDB

```go
import "mkfst/providers/aws/dynamodb"

ddb, err := dynamodb.New(ctx, dynamodb.Opts{AWS: mkaws.Opts{Region: "us-east-1"}})
// or share a config:
ddb := dynamodb.FromConfig(cfg)

// Low-level (DDB-native types).
ddb.Put(ctx, "Users", dynamodb.Item{
    "id":   &types.AttributeValueMemberS{Value: "u-1"},
    "name": &types.AttributeValueMemberS{Value: "alice"},
})
item, _ := ddb.Get(ctx, "Users", dynamodb.Item{
    "id": &types.AttributeValueMemberS{Value: "u-1"},
})

// Typed (struct-based via `dynamodbav` tags).
type User struct {
    ID   string `dynamodbav:"id"`
    Name string `dynamodbav:"name"`
    Age  int    `dynamodbav:"age"`
}
_ = ddb.PutTyped(ctx, "Users", User{ID: "u-1", Name: "alice", Age: 30})

var u User
ok, err := ddb.GetTyped(ctx, "Users", dynamodb.Item{
    "id": &types.AttributeValueMemberS{Value: "u-1"},
}, &u)
```

`Update` builds the UpdateExpression / ExpressionAttributeNames /
Values triple from a `Updates Item` map plus optional `Removes []string`,
keeping reserved-word and special-character escaping out of the
caller's lap. ConditionExpression is exposed for compare-and-set.

`Query` and `Scan` accept the SDK's `QueryInput` / `ScanInput`
directly (minus TableName) so you have the full DDB query
vocabulary — no re-defining every field.

## Secrets Manager

```go
import "mkfst/providers/aws/secrets"

sm, err := secrets.New(ctx, secrets.Opts{
    AWS:      mkaws.Opts{Region: "us-east-1"},
    CacheTTL: 5 * time.Minute,
})

// Raw bytes.
b, _ := sm.Get(ctx, "prod/db/password")

// Structured.
type DB struct{ Username, Password string }
var db DB
_ = sm.GetJSON(ctx, "prod/db/credentials", &db)

m, _ := sm.GetStringMap(ctx, "prod/api/keys")

// Create or rotate.
arn, _ := sm.Put(ctx, "prod/db/password", []byte("new-password"))

// Bust cache after upstream rotation.
sm.InvalidateCache("prod/db/password")
```

### Container injection

The killer integration: pull secrets from Secrets Manager and
inject them into a `network.Stack`'s containers as tmpfs-mounted
files (the secure path — never visible via `docker inspect`).

```go
import (
    "mkfst/providers/aws/secrets"
    "mkfst/providers/docker/network"
)

stack, _ := netEng.NewStack("api")
stack.MustAddService("web",
    network.Image("myapp:v1"),
    network.UseSecret("db_password", "/run/secrets/db_password"),
    network.UseSecret("api_key", "/run/secrets/api_key"),
)

// Pull each secret from AWS and register on the stack under the
// stack-side name. The container will see them as files at the
// declared mount paths.
_ = sm.Inject(ctx, stack, map[string]string{
    "db_password": "prod/db/password",         // stackKey → SM secret
    "api_key":     "prod/api/master-key",
})

// Or explode a single JSON-shaped secret into multiple stack secrets:
_ = sm.InjectJSON(ctx, stack, "prod/all-creds", "")
// Each top-level key in the JSON becomes a separate Stack secret.

_ = stack.Up(ctx)
```

Secrets are mlock'd on Linux (best-effort, requires CAP_IPC_LOCK)
and zero-filled on `Stack.Down`. They never appear in container
env vars or `docker inspect` output.

## SQS

```go
import "mkfst/providers/aws/sqs"

q, err := sqs.New(ctx, sqs.Opts{
    AWS:      mkaws.Opts{Region: "us-east-1"},
    QueueURL: "https://sqs.us-east-1.amazonaws.com/123/orders",
})

// Send.
id, _ := q.Send(ctx, []byte(`{"order_id":"o-1"}`), sqs.SendOpts{
    Attributes: map[string]string{"trace_id": "abc-123"},
})

// One-shot Receive.
msgs, _ := q.Receive(ctx, sqs.ReceiveOpts{MaxMessages: 10, WaitSeconds: 20})
for _, m := range msgs {
    handle(m.Body)
    _ = q.Delete(ctx, "", m.Receipt) // ack
}

// Long-poll Subscribe — runs in a tracked goroutine, exits on
// ctx cancel, joins all in-flight handlers before signaling done.
done, err := q.Subscribe(ctx, sqs.SubscribeOpts{
    AutoAck:     true,
    Concurrency: 8,
    Handler: func(ctx context.Context, msg sqs.Message) error {
        return processOrder(msg.Body)
    },
    OnError: func(err error) { log.Printf("sqs: %v", err) },
})
// On shutdown:
//   cancelCtx()
//   <-done   // waits for handlers
```

## Composing with the HTTP server

Standard wiring — providers are Go values, share them across handlers:

```go
sm, _ := secrets.New(ctx, secrets.Opts{AWS: mkaws.Opts{Region: "us-east-1"}})
ddb, _ := dynamodb.New(ctx, dynamodb.Opts{AWS: mkaws.Opts{Region: "us-east-1"}})
q, _   := sqs.New(ctx, sqs.Opts{AWS: mkaws.Opts{Region: "us-east-1"}, QueueURL: orderQueue})

svc.Route("POST", "/orders", 202, nil,
    func(g *gin.Context, _ *sql.DB, in *struct {
        SKU string `json:"sku" validate:"required"`
        Qty int    `json:"qty" validate:"min=1"`
    }) (struct{ ID string }, error) {
        id := uuid.NewString()
        // Persist in DynamoDB.
        _ = ddb.PutTyped(g.Request.Context(), "Orders", Order{ID: id, SKU: in.SKU, Qty: in.Qty})
        // Fan-out via SQS.
        _, _ = q.Send(g.Request.Context(), mustJSON(Order{ID: id, ...}), sqs.SendOpts{})
        return struct{ ID string }{ID: id}, nil
    },
)
```

The whole stack — auth resolution, region resolution, retry policy,
endpoint override, role assumption — is the same one-line builder
across all three providers.

## See also

- [providers.md](providers.md) — the provider map
- [stacks.md](stacks.md) — for the secrets-injection target
- [policy.md](policy.md) — RBAC gating (cap calls to AWS providers
  by role if needed)
