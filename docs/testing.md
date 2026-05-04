# Testing pattern: interfaces + generated mocks

mkfst uses [vektra/mockery](https://github.com/vektra/mockery) to
generate testify-style mocks from narrow Go interfaces, so providers
can be tested against their full call paths without touching real
external services (AWS, docker, etc.).

## Why

A provider that holds a concrete SDK client (`*dynamodb.Client`,
`*sqs.Client`, etc.) can only be tested two ways:

1. By going to the real service (slow, requires credentials, can't run in CI without secrets).
2. By only exercising input validation and pure-Go helper functions (shallow coverage).

A provider that holds a narrow **interface** of the SDK calls it
makes can be tested a third way: substitute a mock that returns
canned responses, asserting both the input we send AND the result
we surface. We get full call-path coverage with zero external
dependencies.

## The pattern

Each provider:

1. Defines a small unexported interface (e.g. `ddbAPI`, `sqsAPI`,
   `secretsAPI`) covering ONLY the SDK methods the provider
   actually calls.
2. Stores the interface, not the concrete client, on its main
   struct.
3. Provides a `New(...)` / `FromConfig(...)` constructor that wires
   the real client (which satisfies the interface implicitly).
4. Provides a test-only `fromAPI(api)` constructor that builds the
   provider around any implementation of the interface.
5. Lists the interface in `.mockery.yaml`. Mockery regenerates the
   mock into `mock_<interface>_test.go` alongside the source.

```go
// providers/aws/sqs/sqs.go

type sqsAPI interface {
    SendMessage(ctx context.Context, in *sqs.SendMessageInput, opts ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
    ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
    DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, opts ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

type Client struct {
    cli sqsAPI         // ← interface, not *sqs.Client
    cfg awssdk.Config
    defaultURL string
}

func FromConfig(cfg awssdk.Config, url string) *Client {
    return &Client{cli: sqs.NewFromConfig(cfg), ...}  // real client
}

func fromAPI(api sqsAPI, url string) *Client {  // test-only
    return &Client{cli: api, ...}
}
```

## Writing a mock-driven test

```go
// providers/aws/sqs/sqs_test.go

func TestClient_Send_BuildsCorrectInput(t *testing.T) {
    api := newMocksqsAPI(t)        // ← generated mock constructor
    c := fromAPI(api, "https://sqs/q")

    api.EXPECT().SendMessage(mock.Anything,
        mock.MatchedBy(func(in *awssqs.SendMessageInput) bool {
            return awssdk.ToString(in.QueueUrl) == "https://sqs/q" &&
                   awssdk.ToString(in.MessageBody) == `{"x":1}`
        }),
    ).Return(&awssqs.SendMessageOutput{
        MessageId: awssdk.String("mid-123"),
    }, nil).Once()

    id, err := c.Send(ctx, []byte(`{"x":1}`), SendOpts{})
    if err != nil { t.Fatal(err) }
    if id != "mid-123" { t.Fatalf("got %q", id) }
}
```

`newMocksqsAPI(t)` registers itself on the testing.T — assertions
fire automatically at test cleanup, so missing-call expectations
fail the test.

For runtime-captured behavior:

```go
api.EXPECT().UpdateItem(mock.Anything, mock.Anything).
    Run(func(ctx context.Context, in *dynamodb.UpdateItemInput, opts ...func(*dynamodb.Options)) {
        captured = in
    }).
    Return(&dynamodb.UpdateItemOutput{}, nil).Once()
```

For loops / multi-call surfaces (e.g. `Subscribe`):

```go
api.EXPECT().ReceiveMessage(mock.Anything, mock.Anything).RunAndReturn(
    func(ctx context.Context, in *awssqs.ReceiveMessageInput, opts ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
        if firstCall { return burstResponse, nil }
        return emptyResponse, nil
    },
)
```

## Regenerating mocks

```sh
# install mockery once:
go install github.com/vektra/mockery/v3@latest

# regenerate from the project root:
~/go/bin/mockery
# or just `mockery` if your $GOPATH/bin is on PATH
```

`.mockery.yaml` at the repo root drives generation. To add a new
mockable interface, list it under `packages.<import-path>.interfaces`
and re-run.

## Cross-package mocking

Generated mocks are written to `mock_*_test.go`, which is
package-internal test code. They can't be imported from other
packages. Two ways to share:

- **Hand-roll a small stub** in the consuming package's tests.
  See `providers/docker/network/policy_enforcement_test.go`'s
  `fakeChecker` — a 12-line stub that records calls and returns
  configurable verdicts.
- **Generate into a `/mocks` subpackage** by adding a separate
  mockery config block. Useful when the same mock is consumed
  from many packages.

For most provider tests, the hand-rolled stub is faster to read
and write than a generated mock with EXPECT() chains.

## What we mock vs. what we don't

- ✅ External SDKs (AWS, docker, … — the boundary between mkfst and
  the world).
- ✅ Cross-provider abstractions (`policy.Checker`, `cache.Cache`,
  `tasks.Store`) when the test wants to verify the consumer's
  behavior, not the provider's correctness.
- ❌ Pure-Go logic (expression builders, marshaling, retry math,
  validation) — test those directly with regular Go tests.
- ❌ The HTTP server / fizz / tonic surface — exercised end-to-end
  in examples + the e2e test suite.

## Test scoreboard

After the mockery integration:

| Suite | Tests |
|---|---|
| `providers/aws/dynamodb` | 17 (was 5) |
| `providers/aws/sqs` | 13 (was 6) |
| `providers/aws/secrets` | 12 (was 4) |
| `providers/docker/network` (incl. policy enforcement) | 4 new policy tests + existing |
| All providers, total | 226 passing tests |
| Race-detector under `go test -race` | clean across all 17 packages |
