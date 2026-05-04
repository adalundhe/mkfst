# 14-aws

API server using all three AWS providers (DynamoDB, Secrets
Manager, SQS) with shared credential resolution.

## Prerequisites

- AWS account access (IAM user keys, instance profile, IRSA, or
  any other source the SDK's default chain understands)
- A DynamoDB table with `id` (String) as the primary key
- An SQS queue
- (Optional) Secrets Manager secrets to look up via `/config/:name`

## Run

Use the host's existing AWS auth:

```sh
AWS_REGION=us-east-1 \
  ORDERS_TABLE=Orders \
  ORDERS_QUEUE_URL=https://sqs.us-east-1.amazonaws.com/123/orders \
  go run ./examples/14-aws
```

Or assume a role on top:

```sh
AWS_REGION=us-east-1 \
  AWS_ROLE_ARN=arn:aws:iam::123:role/myapp-runtime \
  ORDERS_TABLE=Orders \
  ORDERS_QUEUE_URL=https://sqs.us-east-1.amazonaws.com/123/orders \
  go run ./examples/14-aws
```

## Exercise

```sh
# Place an order.
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"id":"o-1","sku":"WIDGET","qty":3}' \
  http://localhost:8081/orders
# {"ID":"o-1"}

# The background SQS subscriber will log:
# processed order o-1 sku=WIDGET qty=3 (queued 2026-...)

# Read it back.
curl -s http://localhost:8081/orders/o-1 | jq

# Fetch a Secrets Manager secret.
curl -s http://localhost:8081/config/prod%2Fdb%2Fpassword
```

## What this demonstrates

- **One credential resolution** (`mkaws.Resolve`) shared across
  DynamoDB, Secrets, and SQS clients. Auth flows from env / shared
  config / IMDS / IRSA, optionally layered with `AWS_ROLE_ARN`
  assume-role.
- **DynamoDB typed CRUD** via `dynamodbav` struct tags — no
  AttributeValue wrangling at the application layer.
- **SQS Subscribe** runs in a tracked goroutine, exits cleanly
  on ctx cancel, joins all in-flight handlers before signaling
  done.
- **Order-of-events**: HTTP POST writes to DynamoDB, then
  publishes to SQS, then returns 202; the background subscriber
  picks up the message asynchronously.

## Composing with the rest of mkfst

Drop in `providers/policy` to gate `/orders` behind a
`PermDynamoDBWrite`-style permission. Drop in `providers/cache`
in front of `/config/:name` to memoize secret lookups beyond the
in-package TTL. Drop in `providers/docker/network` and use
`secrets.Inject` to push the secrets into stack containers as
tmpfs-mounted files instead of returning them over HTTP.
