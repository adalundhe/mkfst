# 12-stacks

API server orchestrating a Compose-like docker stack via
`providers/docker/network`.

## Prerequisites

A reachable docker daemon. Common setups:

```sh
# rootless docker on Linux
export DOCKER_HOST=unix:///run/user/$(id -u)/docker.sock
```

## Run

```sh
go run ./examples/12-stacks
```

The server brings up a 2-service stack (`web=nginx`, `cache=redis`)
on its own bridge network, gates `web` on an HTTP readiness probe,
and exposes a host-side gateway for `web` with a `127.0.0.0/8` allow rule.

## Exercise

```sh
# Status of the stack + each service.
curl -s http://localhost:8081/stack/status | jq

# Ingress addresses (host:port reachable from your machine).
curl -s http://localhost:8081/stack/ingress | jq
# {"web":"127.0.0.1:54213"}

# Hit nginx through the gateway.
WEB=$(curl -s http://localhost:8081/stack/ingress | jq -r .web)
curl -s http://$WEB/

# Spawn a one-shot alpine container inside the stack network.
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"msg":"hello-from-oneshot"}' \
  http://localhost:8081/stack/oneshot/echo

# Exec into the existing redis service.
curl -s -X POST http://localhost:8081/stack/exec/redis-ping
# {"Reply":"PONG\n"}
```

Press Ctrl+C to bring the stack down cleanly.

## What this demonstrates

- `network.Engine` + `network.Stack` + multiple `Service` definitions.
- `WithProbe(...)` gating: `Up()` blocks until both services pass
  their readiness probes.
- `Stack.Ingress(...)` with a source-IP allow rule and an in-process
  Go gateway.
- `Stack.Monitor()` event stream — every connection accepted/denied
  prints a line.
- `Stack.RunOneShot(...)` — workflow-driven test container on the
  stack's network.
- `Stack.Exec(...)` — run a command inside an existing service replica.

For a cross-stack-isolation demo, run two of these on different
`Port`s; their `127.0.0.1:...` ingresses are distinct, and their
internal service names cannot resolve across stacks.
